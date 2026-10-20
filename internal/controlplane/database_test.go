package controlplane

import (
	"strings"
	"testing"
)

func TestDatabaseTransportPolicy(t *testing.T) {
	for _, dsn := range []string{
		"", "host=localhost dbname=test", "http://localhost/db", "postgres://127.0.0.1/db",
		"postgres://localhost/db?sslmode=disable", "postgres://remote.example/db?sslmode=require",
		"postgres://remote.example/db?sslmode=disable", "postgres://remote.example/db?sslmode=verify-full&host=127.0.0.1",
		"postgres://remote.example/db?sslmode=verify-full&sslmode=disable",
	} {
		if _, err := ParseDatabaseConfig(dsn); err == nil {
			t.Fatalf("unsafe DSN accepted: %s", dsn)
		}
	}
	for _, dsn := range []string{"postgres://127.0.0.1/test?sslmode=disable", "postgres://[::1]/test?sslmode=disable", "postgres://database.example/test?sslmode=verify-full"} {
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
