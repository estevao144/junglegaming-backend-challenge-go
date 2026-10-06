package workers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/postgres"
)

func TestBackoff(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, 5 * time.Second}, {math.MaxInt32, 5 * time.Second}} {
		if got := Backoff(test.attempt, time.Second, 5*time.Second); got != test.want {
			t.Fatalf("attempt %d: %v", test.attempt, got)
		}
	}
	if Backoff(2, time.Duration(math.MaxInt64/2+1), time.Duration(math.MaxInt64)) != time.Duration(math.MaxInt64) {
		t.Fatal("backoff overflow")
	}
}

type fakeRepository struct {
	event    postgres.ClaimedEvent
	renewErr error
	marked   int
	retried  int
	delay    time.Duration
}

func (r *fakeRepository) Claim(context.Context, int, time.Duration) ([]postgres.ClaimedEvent, error) {
	return []postgres.ClaimedEvent{r.event}, nil
}
func (r *fakeRepository) Renew(context.Context, postgres.ClaimedEvent, time.Duration) error {
	return r.renewErr
}
func (r *fakeRepository) MarkPublished(context.Context, postgres.ClaimedEvent) error {
	r.marked++
	return nil
}
func (r *fakeRepository) Retry(_ context.Context, _ postgres.ClaimedEvent, delay time.Duration) error {
	r.retried++
	r.delay = delay
	return nil
}

type sendFunc func(context.Context, postgres.ClaimedEvent) error

func (f sendFunc) Publish(ctx context.Context, e postgres.ClaimedEvent) error { return f(ctx, e) }
func workerConfig() config.Config {
	return config.Config{DependencyTimeout: time.Second, OutboxBatchSize: 2, OutboxLease: 3 * time.Second, OutboxPollInterval: time.Hour, OutboxRetryBase: time.Second, OutboxRetryMax: 10 * time.Second}
}
func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestWorkerSuccessFailureAndLostClaim(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "lost claim"} {
		t.Run(outcome, func(t *testing.T) {
			r := &fakeRepository{event: postgres.ClaimedEvent{OutboxRecord: postgres.OutboxRecord{EventID: "stable"}, Attempts: 2}}
			if outcome == "lost claim" {
				r.renewErr = postgres.ErrClaimLost
			}
			sent := 0
			publisher := sendFunc(func(_ context.Context, event postgres.ClaimedEvent) error {
				sent++
				if event.EventID != "stable" {
					t.Fatal("worker regenerated event ID")
				}
				if outcome == "failure" {
					return errors.New("controlled failure")
				}
				return nil
			})
			w := NewOutboxWorker(r, publisher, workerConfig(), quietLogger())
			if _, err := w.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch outcome {
			case "success":
				if sent != 1 || r.marked != 1 || r.retried != 0 {
					t.Fatal("success was not acknowledged")
				}
			case "failure":
				if sent != 1 || r.marked != 0 || r.retried != 1 || r.delay != 4*time.Second {
					t.Fatal("failure must schedule durable retry")
				}
			case "lost claim":
				if sent != 0 || r.marked != 0 || r.retried != 0 {
					t.Fatal("stale owner sent an event")
				}
			}
		})
	}
}

func TestWorkerShutdownFinishesCurrentSend(t *testing.T) {
	r := &fakeRepository{}
	started := make(chan struct{})
	finish := make(chan struct{})
	p := sendFunc(func(ctx context.Context, _ postgres.ClaimedEvent) error {
		close(started)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	w := NewOutboxWorker(r, p, workerConfig(), quietLogger())
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stopped <- w.Stop(ctx)
	}()
	close(finish)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if r.marked != 1 {
		t.Fatal("shutdown dropped successful send")
	}
}

func TestWorkerShutdownCancelsAtDeadline(t *testing.T) {
	r := &fakeRepository{}
	started := make(chan struct{})
	p := sendFunc(func(ctx context.Context, _ postgres.ClaimedEvent) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	w := NewOutboxWorker(r, p, workerConfig(), quietLogger())
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if !errors.Is(w.Stop(ctx), context.DeadlineExceeded) {
		t.Fatal("expected deadline cancellation")
	}
	select {
	case <-w.done:
	default:
		t.Fatal("worker goroutine did not finish")
	}
	if r.marked != 0 {
		t.Fatal("cancelled send must not be acknowledged")
	}
}
