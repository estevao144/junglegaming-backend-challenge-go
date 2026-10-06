package observability

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMetricsConcurrencyAndBoundedLabels(t *testing.T) {
	m := NewMetrics()
	var group sync.WaitGroup
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			m.Operation("http", "BET", "PROCESSED", true)
			m.HTTP(200, time.Millisecond)
		}()
	}
	group.Wait()
	m.Operation("arbitrary-id", "wallet-id", "player-id", false)
	m.Divergence()
	m.Conflict()
	m.Worker("outbox", "retry")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	text := w.Body.String()
	for _, want := range []string{`jungle_operations_total{transport="http",kind="BET",result="PROCESSED"} 100`, `jungle_replays_total{transport="http"} 100`, `jungle_reconciliation_divergences_total 1`, `jungle_concurrency_conflicts_total 1`, `jungle_http_duration_seconds_count 100`, `jungle_http_duration_seconds_bucket{le="+Inf"} 100`} {
		if !strings.Contains(text, want) {
			t.Fatal("missing metric", want)
		}
	}
	for _, forbidden := range []string{"arbitrary-id", "wallet-id", "player-id", "walletId=", "transactionId=", "providerId="} {
		if strings.Contains(text, forbidden) {
			t.Fatal("unbounded label", forbidden)
		}
	}
}
