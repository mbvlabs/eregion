package dbschema

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func baseline(t *testing.T) string {
	t.Helper()
	files, err := fs.Glob(GooseFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "migrations/00001_baseline.sql" {
		t.Fatalf("unexpected migration inventory: %v", files)
	}
	data, err := fs.ReadFile(GooseFS, files[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBaselineInventoryAndCreateOnly(t *testing.T) {
	sql := baseline(t)
	if !strings.HasPrefix(sql, "-- +goose Up\n") {
		t.Fatal("missing Goose Up annotation")
	}
	// Strip comments before examining statement keywords.
	comment := regexp.MustCompile(`(?m)--[^\n]*`)
	body := comment.ReplaceAllString(sql, "")
	for _, statement := range strings.Split(body, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if !regexp.MustCompile(`(?i)^CREATE\s+(TABLE|(?:UNIQUE\s+)?INDEX)\b`).MatchString(statement) {
			t.Errorf("not a CREATE TABLE/INDEX statement: %s", statement)
		}
	}
	for _, forbidden := range []string{`\bALTER\b`, `\bDEFAULT\b`, `\b(?:SMALLSERIAL|SERIAL|BIGSERIAL)\b`, `\bGENERATED\s+(?:ALWAYS|BY\s+DEFAULT)\s+AS\s+IDENTITY\b`, `\bCHECK\s*\(`, `\bFUNCTION\b`, `\bTRIGGER\b`, `\bINSERT\b`, `\bUPDATE\b`, `\bDELETE\s+FROM\b`, `\bDO\s+\$\$`} {
		if regexp.MustCompile(`(?i)` + forbidden).MatchString(body) {
			t.Errorf("baseline contains forbidden SQL: %s", forbidden)
		}
	}
	tablePattern := regexp.MustCompile(`(?m)^CREATE TABLE (\w+)`)
	var tables []string
	for _, match := range tablePattern.FindAllStringSubmatch(body, -1) {
		tables = append(tables, match[1])
	}
	slices.Sort(tables)
	want := strings.Fields(`
		eregion_access_grants eregion_definition_dns_policy eregion_definition_runs
		eregion_definition_steps eregion_integration_audit eregion_operation_events
		eregion_operation_steps eregion_operations eregion_platform_apps
		eregion_platform_builds eregion_platform_changes eregion_platform_container_cleanup
		eregion_platform_deployment_owners eregion_platform_deployments eregion_platform_domain_dns
		eregion_platform_domains eregion_platform_environment_revisions eregion_platform_environments
		eregion_platform_nodes eregion_platform_release_command_runs eregion_platform_releases
		eregion_platform_resources eregion_provider_credential_revisions eregion_provider_credentials
		eregion_retention_runs eregion_secret_revisions eregion_secrets eregion_server_settings
		eregion_webhook_deliveries eregion_workspace
	`)
	slices.Sort(want)
	if !slices.Equal(tables, want) {
		t.Errorf("tables = %v; want %v", tables, want)
	}
	indexPattern := regexp.MustCompile(`(?m)^CREATE (?:UNIQUE )?INDEX (\w+)`)
	var indexes []string
	for _, match := range indexPattern.FindAllStringSubmatch(body, -1) {
		indexes = append(indexes, match[1])
	}
	slices.Sort(indexes)
	wantIndexes := strings.Fields(`
		eregion_definition_runs_active_environment eregion_operation_steps_incomplete
		eregion_operations_unfinished eregion_platform_apps_image_repository_unique
		eregion_platform_builds_environment eregion_platform_changes_app
		eregion_platform_container_cleanup_due eregion_platform_deployment_owners_deployment
		eregion_platform_deployments_environment eregion_platform_domain_dns_state
		eregion_platform_domains_environment eregion_platform_domains_id_key
		eregion_platform_domains_primary eregion_platform_domains_route_pending
		eregion_platform_environment_revisions_environment eregion_platform_nodes_identity_key
		eregion_platform_nodes_wg_address eregion_platform_release_command_runs_operation
		eregion_platform_releases_environment eregion_platform_resources_app
		eregion_platform_resources_environment eregion_provider_credential_revisions_created
		eregion_retention_runs_started eregion_secret_revisions_created eregion_secrets_environment
		eregion_webhook_deliveries_admitted eregion_webhook_deliveries_first_seen
	`)
	slices.Sort(wantIndexes)
	if !slices.Equal(indexes, wantIndexes) {
		t.Errorf("indexes = %v; want %v", indexes, wantIndexes)
	}
}

func TestBaselineStructuralContracts(t *testing.T) {
	sql := baseline(t)
	for _, contract := range []string{
		"UNIQUE (actor_kind, actor_subject, kind, target, idempotency_key)",
		"UNIQUE (app_id, name)",
		"UNIQUE (environment_id, number)",
		"PRIMARY KEY (environment_id, delivery_id_hash)",
		"FOREIGN KEY (app, environment, name) REFERENCES eregion_secrets(app, environment, name) ON DELETE CASCADE",
		"FOREIGN KEY (provider, zone_id) REFERENCES eregion_provider_credentials(provider, zone_id) ON DELETE CASCADE",
		"domain_id text PRIMARY KEY REFERENCES eregion_platform_domains(id) ON DELETE RESTRICT",
		"environment_id text PRIMARY KEY REFERENCES eregion_platform_environments(id) ON DELETE CASCADE",
		"release_id text REFERENCES eregion_platform_releases(id) ON DELETE SET NULL",
		"child_operation_id text UNIQUE REFERENCES eregion_operations(id) DEFERRABLE INITIALLY DEFERRED",
		"CREATE UNIQUE INDEX eregion_platform_domains_primary ON eregion_platform_domains (environment_id) WHERE is_primary",
		"CREATE UNIQUE INDEX eregion_definition_runs_active_environment ON eregion_definition_runs(environment_id) WHERE finished_at IS NULL",
		"CREATE UNIQUE INDEX eregion_platform_apps_image_repository_unique ON eregion_platform_apps (image_repository) WHERE image_repository <> ''",
	} {
		if !strings.Contains(sql, contract) {
			t.Errorf("missing contract: %s", contract)
		}
	}
	// These pointers would form cycles in a CREATE-only baseline. Their
	// ownership-side foreign keys remain present.
	for _, column := range []string{"current_revision_id", "desired_release_id", "applied_release_id", "serving_release_id", "serving_deployment_id", "build_id"} {
		if !regexp.MustCompile(`(?m)^\s+` + column + ` text,\s*$`).MatchString(sql) {
			t.Errorf("missing unconstrained cyclic backpointer %s", column)
		}
	}
}
