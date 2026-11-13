package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/platform"
)

// insertFixtureNode records a registered local node with no enrolled remote
// identity. Empty key fields are intentional: this fixture has no SSH tunnel.
func insertFixtureNode(ctx context.Context, pool *pgxpool.Pool, name string, roles, capabilities []string, address string) error {
	_, err := pool.Exec(ctx, `INSERT INTO eregion_platform_nodes
		(name,roles,capabilities,address,status,created_at,updated_at,enrollment_state,enrollment_error,
		transport,ssh_address,ssh_user,ssh_host_key,ssh_host_key_fingerprint,expected_identity_fingerprint,
		identity_public_key,identity_fingerprint,os_id,os_version,arch,build_concurrency,build_user,
		wg_enabled,wg_address,wg_endpoint,wg_ssh_port,wg_public_key,control_wg_public_key,wg_state,last_check_state,last_check_detail)
		VALUES($1,$2,$3,$4,'registered',clock_timestamp(),clock_timestamp(),'registered','',
		'local','','','','','','','','','', '',1,'',false,'','',0,'','','none','unknown','')
		ON CONFLICT (name) DO NOTHING`, name, roles, capabilities, address)
	return err
}

func insertFixtureApp(ctx context.Context, pool *pgxpool.Pool, id, name, repository string) error {
	_, err := pool.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name,image_repository,created_at,updated_at)
		VALUES($1,$2,$3,clock_timestamp(),clock_timestamp())`, id, name, repository)
	return err
}

func insertFixtureEnvironment(ctx context.Context, pool *pgxpool.Pool, id, appID, name, node string, hostPort, candidatePort, containerPort int) error {
	_, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environments
		(id,app_id,name,node_name,host_port,candidate_port,container_port,bind_address,health_type,health_path,
		health_timeout_seconds,env,secret_refs,secret_revision,health_state,health_detail,created_at,updated_at,protected)
		VALUES($1,$2,$3,$4,$5,$6,$7,'127.0.0.1','http','/',30,'{}','{}',0,'unknown','',clock_timestamp(),clock_timestamp(),false)`,
		id, appID, name, node, hostPort, candidatePort, containerPort)
	return err
}

func insertFixtureRevision(ctx context.Context, pool *pgxpool.Pool, id, environmentID string, number int, runtime string) error {
	_, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environment_revisions
		(id,environment_id,number,actor_kind,actor_subject,operation,summary,source,auto_deploy,webhook_secret_name,
		runtime,processes,config,secret_names,deploy_intent,created_at,retention,resources)
		VALUES($1,$2,$3,'system','integration','env.changed','integration fixture','{}',false,'',$4,'[]','{}',
		ARRAY[]::text[],false,clock_timestamp(),'{}',ARRAY[]::text[])`, id, environmentID, number, runtime)
	return err
}

func insertFixtureDomain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, environmentID string, primary bool, createdAt time.Time) {
	t.Helper()
	id, err := platform.NewDomainID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO eregion_platform_domains
		(name,environment_id,created_at,is_primary,desired_revision,applied_revision,route_state,route_error,
		expected_addresses,dns_state,dns_addresses,dns_error,certificate_state,certificate_error,
		public_state,public_error,check_path,id)
		VALUES($1,$2,$3,$4,1,0,'pending','',ARRAY[]::text[],'pending',ARRAY[]::text[],'','pending','','pending','','',$5)`,
		name, environmentID, createdAt, primary, id)
	if err != nil {
		t.Fatal(err)
	}
}
