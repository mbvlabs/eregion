package controlplane

import (
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/vault"
)

// Migration 005 is a frozen copy of the vault package schema, so a later edit to
// vault/schema.sql must be a new migration, never a silent divergence.
func TestMigration005MatchesVaultSchema(t *testing.T) {
	if migration005 != vault.SchemaSQL {
		t.Fatal("005_vault.sql differs from vault/schema.sql; add a new migration instead of editing an applied one")
	}
	if migration027 != vault.ProviderSchemaSQL {
		t.Fatal("027_provider_credentials.sql differs from vault/provider_schema.sql; add a new migration instead of editing an applied one")
	}
	for index, m := range migrations {
		if m.version != index+1 {
			t.Fatalf("migration %d is out of order at index %d", m.version, index)
		}
	}
	if requiredVersion() != len(migrations) {
		t.Fatalf("required schema version = %d, want %d", requiredVersion(), len(migrations))
	}
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	if len(files) != len(migrations) {
		t.Fatalf("migration files = %d, registered migrations = %d; register every SQL migration", len(files), len(migrations))
	}
	for index, name := range files {
		if migrations[index].name != name || strings.TrimSpace(migrations[index].sql) == "" {
			t.Fatalf("migration %s is missing or empty in the registry", name)
		}
	}
}

func TestDatabaseTransportPolicy(t *testing.T) {
	for _, dsn := range []string{
		"", "host=localhost dbname=test", "http://localhost/db", "postgres://127.0.0.1/db",
		"postgres://localhost/db?sslmode=disable", "postgres://remote.example/db?sslmode=require",
		"postgres://remote.example/db?sslmode=disable", "postgres://remote.example/db?sslmode=verify-full&host=127.0.0.1",
		"postgres://remote.example/db?sslmode=verify-full&sslmode=disable",
		"postgres://100.128.0.1/db?sslmode=disable", "postgres://evilts.net/db?sslmode=disable",
		"postgres://thelab.ts.net.example.com/db?sslmode=disable", "postgres://thelab.tailnet.ts.net/db?sslmode=require",
	} {
		if _, err := ParseDatabaseConfig(dsn); err == nil {
			t.Fatalf("unsafe DSN accepted: %s", dsn)
		}
	}
	for _, dsn := range []string{"postgres://127.0.0.1/test?sslmode=disable", "postgres://[::1]/test?sslmode=disable", "postgres://database.example/test?sslmode=verify-full",
		"postgres://100.83.227.89:5433/test?sslmode=disable", "postgres://[fd7a:115c:a1e0::1]/test?sslmode=disable",
		"postgres://thelab.tailbbed89.ts.net:5433/test?sslmode=disable", "postgres://THELAB.TAILBBED89.TS.NET./test?sslmode=disable"} {
		config, err := ParseDatabaseConfig(dsn)
		if err != nil || config.MaxConns != 12 {
			t.Fatalf("valid config rejected: %v", err)
		}
	}
	_, err := ParseDatabaseConfig("postgres://user:do-not-print@remote.example/test?sslmode=disable")
	if strings.Contains(err.Error(), "do-not-print") {
		t.Fatal("configuration error leaked credentials")
	}
}
