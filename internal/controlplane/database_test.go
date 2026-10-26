package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
)

// Migration 005 is a frozen copy of the vault package schema, so a later edit to
// vault/schema.sql must be a new migration, never a silent divergence.
func TestMigration005MatchesVaultSchema(t *testing.T) {
	if migration005 != vault.SchemaSQL {
		t.Fatal("005_vault.sql differs from vault/schema.sql; add a new migration instead of editing an applied one")
	}
	for index, m := range migrations {
		if m.version != index+1 {
			t.Fatalf("migration %d is out of order at index %d", m.version, index)
		}
	}
	if requiredVersion() != len(migrations) {
		t.Fatalf("required schema version = %d, want %d", requiredVersion(), len(migrations))
	}
}

func TestDatabaseNameAndBackupKey(t *testing.T) {
	if got := databaseName("postgres://user:secret@127.0.0.1:5432/eregion?sslmode=disable"); got != "eregion" {
		t.Fatalf("database name = %q", got)
	}
	if got := databaseName("::not a url"); got != "" {
		t.Fatalf("invalid DSN produced %q", got)
	}
	slot := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("x", 3600))
	key := scheduledBackupKey("appdb", slot)
	if key != scheduledBackupKey("appdb", slot.UTC()) || key != "scheduled-backup:appdb:1792461600" {
		t.Fatalf("key = %s", key)
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		t.Fatal(err)
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
