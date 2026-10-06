package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockedMetricsWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func TestOutboxMetricOwnsTimestampSnapshot(t *testing.T) {
	m := NewMetrics()
	oldest := time.Now().Add(-time.Hour)
	m.Outbox(&oldest, true)
	oldest = time.Now().Add(time.Hour)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "jungle_outbox_lag_seconds 0.000") {
		t.Fatal("caller changed the sampled outbox timestamp")
	}
}

func (w *blockedMetricsWriter) Header() http.Header { return w.header }
func (w *blockedMetricsWriter) WriteHeader(int)     {}
func (w *blockedMetricsWriter) Write(body []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(body), nil
}

func TestSlowMetricsClientDoesNotBlockOperations(t *testing.T) {
	m := NewMetrics()
	w := &blockedMetricsWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	scraped := make(chan struct{})
	var updated chan struct{}
	go func() { m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil)); close(scraped) }()
	t.Cleanup(func() {
		close(w.release)
		<-scraped
		if updated != nil {
			<-updated
		}
	})
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("scrape did not start writing")
	}
	updated = make(chan struct{})
	go func() { m.Operation("http", "BET", "PROCESSED", false); close(updated) }()
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("slow metrics response blocked completion of independent operations")
	}
}
