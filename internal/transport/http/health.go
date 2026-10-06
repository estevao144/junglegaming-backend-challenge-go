package httptransport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/postgres"
)

type Checker interface{ Check(context.Context) error }

type Health struct {
	database Checker
	queue    Checker
	timeout  time.Duration
	log      *slog.Logger
}

func NewHealth(c config.Config, db *postgres.Database, queue *messaging.Queue, log *slog.Logger) *Health {
	return &Health{database: db, queue: queue, timeout: c.DependencyTimeout, log: log}
}

func (h *Health) Live(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	checks := map[string]string{}
	status := http.StatusOK
	for _, check := range []struct {
		name       string
		dependency Checker
	}{{"postgres", h.database}, {"sqs", h.queue}} {
		if err := check.dependency.Check(ctx); err != nil {
			status = http.StatusServiceUnavailable
			checks[check.name] = "unavailable"
			h.log.WarnContext(ctx, "readiness check failed", "dependency", check.name)
		} else {
			checks[check.name] = "ok"
		}
	}
	respond(w, status, checks)
}

func NewMux(h *Health) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", h.Live)
	mux.HandleFunc("GET /health/ready", h.Ready)
	return mux
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

var Module = fx.Module("http", fx.Provide(NewHealth, NewAuthenticatedMux, NewObservedOperationHandler, NewServer), fx.Invoke(func(*Server) {}))
