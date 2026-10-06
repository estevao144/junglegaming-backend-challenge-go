//go:build integration

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/platform/postgres"
	httptransport "jungle-gaming/internal/transport/http"
)

// TestRealInfrastructureLifecycle uses the real PostgreSQL and SQS adapters.
// Prepare Compose dependencies and environment before running this test.
func TestRealInfrastructureLifecycle(t *testing.T) {
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	var server *httptransport.Server
	var database *postgres.Database
	application := New(fx.Populate(&server, &database))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := application.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + server.Address() + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("readiness returned %d", response.StatusCode)
	}
	var checks map[string]string
	if err := json.NewDecoder(response.Body).Decode(&checks); err != nil {
		t.Fatal(err)
	}
	if checks["postgres"] != "ok" || checks["sqs"] != "ok" {
		t.Fatalf("dependencies not ready: %v", checks)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	if err := application.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := database.Check(stopCtx); err == nil {
		t.Fatal("PostgreSQL pool remained open after shutdown")
	}
	if response, err := client.Get("http://" + server.Address() + "/health/live"); err == nil {
		response.Body.Close()
		t.Fatal("HTTP server accepted a request after shutdown")
	}
}
