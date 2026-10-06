package workers

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/postgres"
)

type blockingReferenceWork struct {
	claim    bool
	started  chan struct{}
	finished chan struct{}
}

func (b *blockingReferenceWork) Claim(ctx context.Context, _ int, _ time.Duration) ([]postgres.ReferenceClaim, error) {
	if !b.claim {
		return []postgres.ReferenceClaim{{TransactionID: "transaction", Token: "token"}}, nil
	}
	close(b.started)
	<-ctx.Done()
	close(b.finished)
	return nil, ctx.Err()
}
func (b *blockingReferenceWork) ResolveReference(ctx context.Context, _ postgres.ReferenceClaim, _ application.ReferenceRetryPolicy) error {
	close(b.started)
	<-ctx.Done()
	close(b.finished)
	return ctx.Err()
}

func TestReferenceShutdownCancelsAndJoins(t *testing.T) {
	for _, claim := range []bool{true, false} {
		t.Run(map[bool]string{true: "claim", false: "resolution"}[claim], func(t *testing.T) {
			work := &blockingReferenceWork{claim: claim, started: make(chan struct{}), finished: make(chan struct{})}
			c := config.Config{DependencyTimeout: time.Second, ReferenceBatchSize: 1, ReferenceLease: time.Second, ReferencePollInterval: time.Hour, ReferenceProcessTimeout: time.Second, ReferenceMaxAttempts: 2, ReferenceRetryBase: time.Millisecond, ReferenceRetryMax: time.Second}
			w := NewReferenceWorker(work, work, c, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := w.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-work.started:
			case <-time.After(time.Second):
				t.Fatal("worker did not start")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := w.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-work.finished:
			default:
				t.Fatal("worker returned before operation stopped")
			}
			select {
			case <-w.done:
			default:
				t.Fatal("goroutine not joined")
			}
		})
	}
}
