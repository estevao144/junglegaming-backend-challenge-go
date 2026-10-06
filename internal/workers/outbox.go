package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
)

// These two narrow interfaces allow failure and shutdown tests without replacing
// the real PostgreSQL/SQS integration tests.
type DeliveryRepository interface {
	Claim(context.Context, int, time.Duration) ([]postgres.ClaimedEvent, error)
	Renew(context.Context, postgres.ClaimedEvent, time.Duration) error
	MarkPublished(context.Context, postgres.ClaimedEvent) error
	Retry(context.Context, postgres.ClaimedEvent, time.Duration) error
}

type Publisher interface {
	Publish(context.Context, postgres.ClaimedEvent) error
}

type OutboxWorker struct {
	metrics    *observability.Metrics
	repository DeliveryRepository
	publisher  Publisher
	config     config.Config
	logger     *slog.Logger
	stopClaims context.CancelFunc
	cancelWork context.CancelFunc
	done       chan struct{}
}

func NewOutboxWorker(repository DeliveryRepository, publisher Publisher, c config.Config, logger *slog.Logger) *OutboxWorker {
	return &OutboxWorker{repository: repository, publisher: publisher, config: c, logger: logger}
}

// Concrete dependencies ensure Fx stops this hook before closing SQS and PG.
func RegisterOutbox(lc fx.Lifecycle, repository *postgres.OutboxDelivery, publisher *messaging.EventPublisher, c config.Config, logger *slog.Logger, metrics *observability.Metrics) *OutboxWorker {
	w := NewOutboxWorker(repository, publisher, c, logger)
	w.metrics = metrics
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		checkCtx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
		defer cancel()
		if err := repository.Check(checkCtx); err != nil {
			return err
		}
		return w.Start(ctx)
	}, OnStop: w.Stop})
	return w
}

var Module = fx.Module("outbox", fx.Provide(postgres.NewOutboxDelivery, messaging.NewEventPublisher, RegisterOutbox), fx.Invoke(func(*OutboxWorker) {}))

func Backoff(failedAttempts int, base, maximum time.Duration) time.Duration {
	delay := base
	for attempt := 1; attempt < failedAttempts; attempt++ {
		if delay >= maximum || delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func (w *OutboxWorker) Start(context.Context) error {
	claims, stopClaims := context.WithCancel(context.Background())
	work, cancelWork := context.WithCancel(context.Background())
	w.stopClaims = stopClaims
	w.cancelWork = cancelWork
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		defer cancelWork()
		for {
			if claims.Err() != nil {
				return
			}
			if _, err := w.runBatch(claims, work); err != nil && claims.Err() == nil {
				w.logger.Error("outbox claim failed", "operation", "claim")
			}
			timer := time.NewTimer(w.config.OutboxPollInterval)
			select {
			case <-claims.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

func (w *OutboxWorker) Stop(ctx context.Context) error {
	w.stopClaims()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		w.cancelWork()
		// All database/send operations use cancellable bounded contexts. Join
		// before Fx proceeds to resource teardown; unconfirmed leases recover.
		<-w.done
		return ctx.Err()
	}
}

// RunOnce is also useful for integration tests; it uses the production path.
func (w *OutboxWorker) RunOnce(ctx context.Context) (int, error) { return w.runBatch(ctx, ctx) }

func (w *OutboxWorker) runBatch(claims, work context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(claims, w.config.DependencyTimeout)
	events, err := w.repository.Claim(ctx, w.config.OutboxBatchSize, w.config.OutboxLease)
	cancel()
	if err != nil {
		w.metrics.Worker("outbox", "error")
		return 0, err
	}
	for _, event := range events {
		if claims.Err() != nil {
			break
		} // Remaining claims safely expire.
		w.deliver(work, event)
	}
	return len(events), nil
}

func (w *OutboxWorker) deliver(ctx context.Context, event postgres.ClaimedEvent) {
	started := time.Now()
	var metadata struct {
		CorrelationID string `json:"correlationId"`
		Data          struct {
			TransactionID         string `json:"transactionId"`
			ProviderID            string `json:"providerId"`
			ExternalTransactionID string `json:"externalTransactionId"`
		} `json:"data"`
	}
	_ = json.Unmarshal(event.Payload, &metadata)
	attributes := []any{"eventId", event.EventID, "walletId", event.WalletID, "correlationId", metadata.CorrelationID, "transactionId", metadata.Data.TransactionID, "providerId", metadata.Data.ProviderID, "externalTransactionId", metadata.Data.ExternalTransactionID, "eventType", event.EventType, "attempt", int64(event.Attempts) + 1}
	defer func() {
		w.logger.Debug("outbox delivery completed", "eventId", event.EventID, "latencyMicros", time.Since(started).Microseconds())
	}()
	w.logger.Debug("outbox claimed", attributes...)
	renewCtx, cancel := context.WithTimeout(ctx, w.config.DependencyTimeout)
	err := w.repository.Renew(renewCtx, event, w.config.OutboxLease)
	cancel()
	if err != nil {
		w.logger.Warn("outbox claim unavailable", attributes...)
		w.metrics.Worker("outbox", "claim_lost")
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.config.DependencyTimeout)
	err = w.publisher.Publish(sendCtx, event)
	cancel()
	updateCtx, cancel := context.WithTimeout(ctx, w.config.DependencyTimeout)
	defer cancel()
	if err != nil {
		delay := Backoff(event.Attempts+1, w.config.OutboxRetryBase, w.config.OutboxRetryMax)
		if retryErr := w.repository.Retry(updateCtx, event, delay); retryErr != nil {
			w.logger.Warn("outbox retry persistence failed; lease will recover", attributes...)
			return
		}
		attributes = append(attributes, "retryAfter", delay.String())
		w.logger.Warn("outbox send failed; retry scheduled", attributes...)
		w.metrics.Worker("outbox", "retry")
		return
	}
	if err := w.repository.MarkPublished(updateCtx, event); err != nil {
		if errors.Is(err, postgres.ErrClaimLost) {
			w.metrics.Worker("outbox", "claim_lost")
			w.logger.Warn("outbox sent but claim lost", attributes...)
		} else {
			w.metrics.Worker("outbox", "error")
			w.logger.Warn("outbox sent but confirmation failed", attributes...)
		}
		return
	}
	w.logger.Info("outbox published", attributes...)
	w.metrics.Worker("outbox", "published")
}
