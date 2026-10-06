package workers

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetricsSamplerShutdownJoinsAfterDeadline(t *testing.T) {
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	done := make(chan struct{})
	finish := make(chan struct{})
	sampler := &MetricsSampler{cancel: cancelWork, done: done}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- sampler.Stop(ctx) }()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("sampler work was not canceled")
	}
	go func() { <-finish; close(done) }()
	select {
	case <-stopped:
		close(finish)
		t.Fatal("shutdown returned before sampler goroutine finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-stopped; !errors.Is(err, context.Canceled) {
		t.Fatalf("deadline not reported: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("sampler goroutine leaked")
	}
}
