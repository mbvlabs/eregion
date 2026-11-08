package controlplane

import (
	"context"
	"strings"
	"testing"
)

// TestIntegrationMigration026FailsClosedOnInflightDeployment proves the
// cutover refuses to infer ownership for an in-flight legacy deployment.
func TestIntegrationMigration026FailsClosedOnInflightDeployment(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := context.Background()
	for _, m := range migrations[:25] {
		if _, err := pool.Exec(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO eregion_platform_nodes(name,roles,capabilities) VALUES ('local',ARRAY['application'],ARRAY['docker']);
INSERT INTO eregion_platform_apps(id,name) VALUES ('app_00000000000000000000000000000001','legacy');
INSERT INTO eregion_platform_environments(id,app_id,name,node_name,host_port,candidate_port,container_port) VALUES
 ('env_00000000000000000000000000000001','app_00000000000000000000000000000001','production','local',10001,10002,80);
INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt) VALUES
 ('op_00000000000000000000000000000011','platform.release.create','env/legacy/production','ci','test','r11',repeat('a',64),'{}','succeeded',1),
 ('op_00000000000000000000000000000021','platform.deploy','env/legacy/production','ci','test','d21',repeat('a',64),'{}','running',1);
INSERT INTO eregion_platform_releases(id,operation_id,app_id,environment_id,image,source,status) VALUES
 ('rel_00000000000000000000000000000011','op_00000000000000000000000000000011','app_00000000000000000000000000000001','env_00000000000000000000000000000001','img/base','{}','ready');
INSERT INTO eregion_platform_deployments(id,operation_id,app_id,environment_id,action,release_id,state) VALUES
 ('dep_00000000000000000000000000000021','op_00000000000000000000000000000021','app_00000000000000000000000000000001','env_00000000000000000000000000000001','deploy','rel_00000000000000000000000000000011','running');
`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, migration026); err == nil {
		t.Fatal("migration 026 accepted an in-flight legacy deployment")
	} else if !strings.Contains(err.Error(), "in-flight deployment lifecycle operation") {
		t.Fatalf("migration 026 error = %v, want fail-closed in-flight refusal", err)
	}
}

// TestIntegrationMigration026LeavesNoLegacyOwners proves a safe cutover does
// not infer or mutate ownership from terminal legacy deployments.
func TestIntegrationMigration026LeavesNoLegacyOwners(t *testing.T) {
	pool, _ := testPool(t, false)
	ctx := context.Background()
	for _, m := range migrations[:25] {
		if _, err := pool.Exec(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO eregion_platform_nodes(name,roles,capabilities) VALUES ('local',ARRAY['application'],ARRAY['docker']);
INSERT INTO eregion_platform_apps(id,name) VALUES ('app_00000000000000000000000000000001','legacy');
INSERT INTO eregion_platform_environments(id,app_id,name,node_name,host_port,candidate_port,container_port) VALUES
 ('env_00000000000000000000000000000001','app_00000000000000000000000000000001','production','local',10001,10002,80);
INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt) VALUES
 ('op_00000000000000000000000000000011','platform.release.create','env/legacy/production','ci','test','r11',repeat('a',64),'{}','succeeded',1),
 ('op_00000000000000000000000000000021','platform.deploy','env/legacy/production','ci','test','d21',repeat('a',64),'{}','succeeded',1);
INSERT INTO eregion_platform_releases(id,operation_id,app_id,environment_id,image,source,status) VALUES
 ('rel_00000000000000000000000000000011','op_00000000000000000000000000000011','app_00000000000000000000000000000001','env_00000000000000000000000000000001','img/base','{}','ready');
INSERT INTO eregion_platform_deployments(id,operation_id,app_id,environment_id,action,release_id,state) VALUES
 ('dep_00000000000000000000000000000021','op_00000000000000000000000000000021','app_00000000000000000000000000000001','env_00000000000000000000000000000001','deploy','rel_00000000000000000000000000000011','succeeded');
UPDATE eregion_platform_environments SET serving_release_id='rel_00000000000000000000000000000011',serving_deployment_id='dep_00000000000000000000000000000021'
 WHERE id='env_00000000000000000000000000000001';
`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, migration026); err != nil {
		t.Fatalf("migration 026 on a safe database: %v", err)
	}
	var owners int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_deployment_owners`).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 0 {
		t.Fatalf("migration 026 backfilled %d legacy owners, want 0", owners)
	}
	var release, deployment string
	if err := pool.QueryRow(ctx, `SELECT coalesce(serving_release_id,''),coalesce(serving_deployment_id,'')
        FROM eregion_platform_environments WHERE id='env_00000000000000000000000000000001'`).Scan(&release, &deployment); err != nil {
		t.Fatal(err)
	}
	if release != "rel_00000000000000000000000000000011" || deployment != "dep_00000000000000000000000000000021" {
		t.Fatalf("migration 026 changed legacy serving to %s/%s", release, deployment)
	}
}
