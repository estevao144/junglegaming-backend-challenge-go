package httptransport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/security"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type traceKey struct{}
type requestTrace struct{ correlation, provider, wallet, transaction, external, result string }

func traceResource(r *http.Request, provider, wallet, transaction, external, result string) {
	if trace, ok := r.Context().Value(traceKey{}).(*requestTrace); ok {
		trace.provider, trace.wallet, trace.transaction, trace.external, trace.result = provider, wallet, transaction, external, result
	}
}
func correlationID(r *http.Request) string {
	if trace, ok := r.Context().Value(traceKey{}).(*requestTrace); ok {
		return trace.correlation
	}
	// Direct handler tests do not run the request middleware.
	return r.Header.Get("X-Correlation-ID")
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(body)
}

func NewObservedOperationHandler(financial *application.AuthorizedFinancialService, metrics *observability.Metrics, log *slog.Logger) *OperationHandler {
	return &OperationHandler{financial: financial, metrics: metrics, log: log}
}

func (h *OperationHandler) Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlation := r.Header.Get("X-Correlation-ID")
		if len(correlation) > 128 || strings.TrimSpace(correlation) != correlation {
			// Keep invalid input out of logs; authentication still runs first.
			correlation = ""
		}
		if correlation == "" {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				respond(w, 500, map[string]string{"error": "internal_error"})
				return
			}
			correlation = hex.EncodeToString(id[:])
		}
		trace := &requestTrace{correlation: correlation}
		r = r.WithContext(context.WithValue(r.Context(), traceKey{}, trace))
		w.Header().Set("X-Correlation-ID", correlation)
		writer := &statusWriter{ResponseWriter: w}
		started := time.Now()
		next.ServeHTTP(writer, r)
		elapsed := time.Since(started)
		if writer.status == 0 {
			writer.status = 200
		}
		h.metrics.HTTP(writer.status, elapsed)
		h.log.InfoContext(r.Context(), "HTTP operation completed", "correlationId", correlation, "providerId", trace.provider, "walletId", trace.wallet, "transactionId", trace.transaction, "externalTransactionId", trace.external, "result", trace.result, "status", writer.status, "latencyMicros", elapsed.Microseconds())
	})
}

// Auth runs before this helper; it enriches the trace without recording JWTs.
func (h *OperationHandler) authenticatedTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlation := r.Header.Get("X-Correlation-ID")
		if len(correlation) > 128 || strings.TrimSpace(correlation) != correlation {
			respond(w, 400, map[string]string{"error": "invalid_correlation_id"})
			return
		}
		if p, err := security.PrincipalFromContext(r.Context()); err == nil {
			if trace, ok := r.Context().Value(traceKey{}).(*requestTrace); ok {
				trace.provider = p.ProviderID
			}
		}
		next.ServeHTTP(w, r)
	})
}

func defaultHTTPLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
