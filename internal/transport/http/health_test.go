package httptransport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/config"
)

type checkFunc func(context.Context) error

func (f checkFunc) Check(ctx context.Context) error { return f(ctx) }

func TestHealth(t *testing.T) {
	for _, test := range []struct {
		name            string
		database, queue error
		status          int
	}{
		{"ready", nil, nil, http.StatusOK},
		{"postgres unavailable", errors.New("database password=secret"), nil, http.StatusServiceUnavailable},
		{"sqs unavailable", nil, errors.New("AWS secret"), http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &Health{
				database: checkFunc(func(context.Context) error { return test.database }),
				queue:    checkFunc(func(context.Context) error { return test.queue }),
				timeout:  time.Second, log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
			}
			mux := NewMux(h)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
			if w.Code != test.status {
				t.Fatalf("got %d, want %d", w.Code, test.status)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("health response leaked dependency details")
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("liveness depends on infrastructure: %d", w.Code)
			}
		})
	}
}

func TestReadinessPropagatesTimeout(t *testing.T) {
	h := &Health{
		database: checkFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }),
		queue:    checkFunc(func(ctx context.Context) error { return ctx.Err() }),
		timeout:  10 * time.Millisecond, log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	w := httptest.NewRecorder()
	h.Ready(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
}

func TestServerLifecycle(t *testing.T) {
	var server *Server
	application := fx.New(
		fx.NopLogger,
		fx.Supply(config.Config{HTTPAddress: "127.0.0.1:0"}, http.NewServeMux(), slog.New(slog.NewJSONHandler(io.Discard, nil))),
		fx.Provide(NewServer), fx.Populate(&server),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := application.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + server.Address() + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := application.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.done:
	default:
		t.Fatal("server goroutine survived shutdown")
	}
	if response, err := client.Get("http://" + server.Address() + "/"); err == nil {
		response.Body.Close()
		t.Fatal("server accepted request after shutdown")
	}
}
