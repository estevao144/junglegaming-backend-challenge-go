// Package observability keeps operational measurements separate from Money.
package observability

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics uses integer counters and microsecond timings. Labels are selected
// from bounded vocabularies, never from resource IDs or arbitrary input.
type Metrics struct {
	mu              sync.Mutex
	counters        map[string]uint64
	latency         [6]uint64
	latencyCount    uint64
	latencyMicros   uint64
	oldest          *time.Time
	dlq             int64
	outboxAvailable bool
	dlqAvailable    bool
}

func NewMetrics() *Metrics { return &Metrics{counters: make(map[string]uint64)} }

func (m *Metrics) add(sample string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[sample]++
}

func (m *Metrics) Operation(transport, kind, status string, replay bool) {
	if transport != "http" && transport != "sqs" && transport != "reference" {
		transport = "internal"
	}
	switch kind {
	case "OPENING", "BET", "WIN", "LOSS", "REFUND", "ROLLBACK":
	default:
		kind = "UNKNOWN"
	}
	switch status {
	case "PROCESSED", "REJECTED", "FAILED", "PENDING_REFERENCE", "ERROR":
	default:
		status = "ERROR"
	}
	m.add(fmt.Sprintf(`jungle_operations_total{transport=%q,kind=%q,result=%q}`, transport, kind, status))
	if replay {
		m.add(fmt.Sprintf(`jungle_replays_total{transport=%q}`, transport))
	}
}

func (m *Metrics) Worker(worker, result string) {
	switch worker {
	case "consumer", "outbox", "reference":
	default:
		return
	}
	switch result {
	case "processed", "retry", "poison", "published", "claim_lost", "error", "attempt_complete", "ack_failed":
	default:
		return
	}
	m.add(fmt.Sprintf(`jungle_worker_results_total{worker=%q,result=%q}`, worker, result))
	if result == "retry" {
		m.add(fmt.Sprintf(`jungle_retries_total{worker=%q}`, worker))
	}
}
func (m *Metrics) Conflict()   { m.add("jungle_concurrency_conflicts_total") }
func (m *Metrics) Divergence() { m.add("jungle_reconciliation_divergences_total") }

var boundsMicros = [...]uint64{1000, 10000, 100000, 500000, 1000000, 5000000}

func (m *Metrics) HTTP(status int, elapsed time.Duration) {
	if m == nil {
		return
	}
	class := status / 100
	if class < 1 || class > 5 {
		class = 5
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[fmt.Sprintf(`jungle_http_requests_total{result="%dxx"}`, class)]++
	micros := uint64(max(elapsed.Microseconds(), 0))
	m.latencyCount++
	m.latencyMicros += micros
	for i, bound := range boundsMicros {
		if micros <= bound {
			m.latency[i]++
		}
	}
}

func (m *Metrics) Outbox(oldest *time.Time, available bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if available {
		m.oldest = nil
		if oldest != nil {
			value := *oldest
			m.oldest = &value
		}
	}
	m.outboxAvailable = available
}
func (m *Metrics) DLQ(count int64, available bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if available {
		m.dlq = count
	}
	m.dlqAvailable = available
}

// ServeHTTP performs no infrastructure I/O. Gauge availability and staleness
// remain visible when a dependency fails instead of presenting a false zero.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Render the bounded snapshot in memory. A slow HTTP client must never
	// hold the mutex used by operations and acknowledgements.
	var snapshot strings.Builder
	m.mu.Lock()
	m.writeSnapshot(&snapshot)
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, snapshot.String())
}

// writeSnapshot is called only while mu is held, with an in-memory writer.
func (m *Metrics) writeSnapshot(w io.Writer) {
	families := []string{"jungle_operations_total", "jungle_replays_total", "jungle_worker_results_total", "jungle_retries_total", "jungle_concurrency_conflicts_total", "jungle_reconciliation_divergences_total", "jungle_http_requests_total"}
	keys := make([]string, 0, len(m.counters))
	for key := range m.counters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, family := range families {
		fmt.Fprintf(w, "# HELP %s Observed process-local events.\n# TYPE %s counter\n", family, family)
		found := false
		for _, key := range keys {
			if strings.SplitN(key, "{", 2)[0] == family {
				fmt.Fprintf(w, "%s %d\n", key, m.counters[key])
				found = true
			}
		}
		if !found && (family == "jungle_concurrency_conflicts_total" || family == "jungle_reconciliation_divergences_total") {
			fmt.Fprintf(w, "%s 0\n", family)
		}
	}
	fmt.Fprint(w, "# HELP jungle_http_duration_seconds Financial and private HTTP latency.\n# TYPE jungle_http_duration_seconds histogram\n")
	for i, bound := range boundsMicros {
		fmt.Fprintf(w, "jungle_http_duration_seconds_bucket{le=\"%d.%06d\"} %d\n", bound/1000000, bound%1000000, m.latency[i])
	}
	fmt.Fprintf(w, "jungle_http_duration_seconds_bucket{le=\"+Inf\"} %d\njungle_http_duration_seconds_sum %d.%06d\njungle_http_duration_seconds_count %d\n", m.latencyCount, m.latencyMicros/1000000, m.latencyMicros%1000000, m.latencyCount)
	lag := int64(0)
	if m.oldest != nil {
		lag = max(time.Since(*m.oldest).Milliseconds(), 0)
	}
	fmt.Fprint(w, "# HELP jungle_outbox_lag_seconds Age of oldest unpublished event at last successful sample.\n# TYPE jungle_outbox_lag_seconds gauge\n")
	fmt.Fprintf(w, "jungle_outbox_lag_seconds %d.%03d\n", lag/1000, lag%1000)
	fmt.Fprint(w, "# HELP jungle_dlq_visible_messages Approximate visible messages in the input DLQ.\n# TYPE jungle_dlq_visible_messages gauge\n")
	fmt.Fprintf(w, "jungle_dlq_visible_messages %d\n", m.dlq)
	fmt.Fprint(w, "# HELP jungle_metrics_dependency_available Last collection success.\n# TYPE jungle_metrics_dependency_available gauge\n")
	for _, sample := range []struct {
		name string
		ok   bool
	}{{"postgres", m.outboxAvailable}, {"sqs", m.dlqAvailable}} {
		available := 0
		if sample.ok {
			available = 1
		}
		fmt.Fprintf(w, "jungle_metrics_dependency_available{dependency=%q} %d\n", sample.name, available)
	}
}
