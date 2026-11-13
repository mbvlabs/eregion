package controlplane

import (
	"database/sql"
	"strings"
	"testing"
)

func TestGooseSourcesAreAvailableWithoutDatabase(t *testing.T) {
	db, err := sql.Open("pgx", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	provider, err := gooseProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	sources := provider.ListSources()
	if len(sources) != 1 || sources[0].Version != 1 {
		t.Fatalf("unexpected sources: %+v", sources)
	}
}

func TestMigrationIdentityRequiresSingleSchema(t *testing.T) {
	for _, tc := range []struct {
		count        int
		schema, role string
		ok           bool
	}{
		{1, "public", "operator", true},
		{1, "tenant", "operator", true},
		{0, "", "operator", false},
		{2, "tenant", "operator", false},
		{1, "tenant", "", false},
	} {
		got, err := checkedIdentity(tc.count, tc.schema, tc.role)
		if (err == nil) != tc.ok || (tc.ok && got != (migrationIdentity{tc.schema, tc.role})) {
			t.Fatalf("identity %+v: got %+v, %v", tc, got, err)
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
