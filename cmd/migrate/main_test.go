package main

import (
	"os"
	"testing"
)

func TestMigrateOnlyRequiresDatabaseConfiguration(t *testing.T) {
	previous := os.Args
	os.Args = []string{"migrate", "up"}
	t.Cleanup(func() { os.Args = previous })
	t.Setenv("DATABASE_URL", "postgres://local:local@127.0.0.1:1/audit?sslmode=disable&connect_timeout=1")
	for _, key := range []string{"OIDC_ISSUER_URL", "OIDC_AUDIENCE", "MESSAGING_CLIENT_ID", "MESSAGING_CLIENT_SECRET"} {
		t.Setenv(key, "")
	}
	t.Setenv("HTTP_ADDR", "irrelevant-invalid-http-address")
	if err := run(); err == nil || err.Error() != "PostgreSQL connection failed" {
		t.Fatalf("migration configuration depended on API/auth settings: %v", err)
	}
}
