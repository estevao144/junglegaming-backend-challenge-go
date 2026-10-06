package workers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
)

// Narrow boundaries support cancellation tests; integration tests use real PG.
type ReferenceClaimer interface {
	Claim(context.Context, int, time.Duration) ([]postgres.ReferenceClaim, error)
}
type ReferenceResolver interface {
	ResolveReference(context.Context, postgres.ReferenceClaim, application.ReferenceRetryPolicy) error
}

type ReferenceWorker struct {
	metrics *observability.Metrics
	queue   ReferenceClaimer
	service ReferenceResolver
	config  config.Config
	logger  *slog.Logger
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewReferenceWorker(queue ReferenceClaimer, service ReferenceResolver, c config.Config, logger *slog.Logger) *ReferenceWorker {
	return &ReferenceWorker{queue: queue, service: service, config: c, logger: logger}
}

func RegisterReference(lc fx.Lifecycle, queue *postgres.ReferenceQueue, service *application.FinancialService, c config.Config, logger *slog.Logger, metrics *observability.Metrics) *ReferenceWorker {
	w := NewReferenceWorker(queue, service, c, logger)
	w.metrics = metrics
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		check, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
		defer cancel()
		if err := queue.Check(check); err != nil {
			return err
		}
		return w.Start(ctx)
	}, OnStop: w.Stop})
	return w
}

var ReferenceModule = fx.Module("references", fx.Provide(postgres.NewReferenceQueue, RegisterReference), fx.Invoke(func(*ReferenceWorker) {}))

func (w *ReferenceWorker) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		for {
			if ctx.Err() != nil {
				return
			}
			if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.Warn("reference batch failed; leases remain recoverable", "operation", "resolve")
			}
			timer := time.NewTimer(w.config.ReferencePollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

func (w *ReferenceWorker) Stop(ctx context.Context) error {
	if w.cancel == nil {
		return nil
	}
	w.cancel() // Cancels claims and any uncommitted resolution; PG rolls it back.
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		<-w.done // Join before Fx closes the pool; operations are context-bounded.
		return ctx.Err()
	}
}

func (w *ReferenceWorker) RunOnce(ctx context.Context) (int, error) {
	claimCtx, cancel := context.WithTimeout(ctx, w.config.DependencyTimeout)
	claims, err := w.queue.Claim(claimCtx, w.config.ReferenceBatchSize, w.config.ReferenceLease)
	cancel()
	if err != nil {
		return 0, err
	}
	policy := application.ReferenceRetryPolicy{MaxAttempts: w.config.ReferenceMaxAttempts, Base: w.config.ReferenceRetryBase, Maximum: w.config.ReferenceRetryMax}
	var batchErr error
	for _, claim := range claims {
		if ctx.Err() != nil {
			break
		}
		resolveCtx, cancel := context.WithTimeout(ctx, w.config.ReferenceProcessTimeout)
		started := time.Now()
		err := w.service.ResolveReference(resolveCtx, claim, policy)
		cancel()
		if err == nil {
			w.metrics.Worker("reference", "attempt_complete")
			w.logger.Info("reference attempt completed", "transactionId", claim.TransactionID, "latencyMicros", time.Since(started).Microseconds())
		} else {
			w.metrics.Worker("reference", "error")
		}
		if err != nil && !errors.Is(err, postgres.ErrReferenceClaimLost) {
			w.logger.Warn("reference resolution failed; lease will recover", "transactionId", claim.TransactionID)
			batchErr = errors.Join(batchErr, err)
		}
	}
	return len(claims), batchErr
}
