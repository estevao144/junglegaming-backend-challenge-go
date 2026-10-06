package workers

import (
	"context"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
	"log/slog"
	"time"
)

type MetricsSampler struct {
	store   *postgres.Store
	queue   *messaging.Queue
	metrics *observability.Metrics
	timeout time.Duration
	log     *slog.Logger
	cancel  context.CancelFunc
	done    chan struct{}
}

func RegisterMetrics(lc fx.Lifecycle, c config.Config, store *postgres.Store, queue *messaging.Queue, metrics *observability.Metrics, log *slog.Logger) *MetricsSampler {
	sampler := &MetricsSampler{store: store, queue: queue, metrics: metrics, timeout: c.DependencyTimeout, log: log}
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		ctx, cancel := context.WithCancel(context.Background())
		sampler.cancel = cancel
		sampler.done = make(chan struct{})
		go func() {
			defer close(sampler.done)
			sampler.Sample(ctx)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sampler.Sample(ctx)
				}
			}
		}()
		return nil
	}, OnStop: sampler.Stop})
	return sampler
}

func (s *MetricsSampler) Stop(ctx context.Context) error {
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		// The adapters honor cancellation. Join their goroutine before Fx
		// closes PostgreSQL/SQS, even when the shutdown deadline has expired.
		<-s.done
		return ctx.Err()
	}
}

func (s *MetricsSampler) Sample(ctx context.Context) {
	check, cancel := context.WithTimeout(ctx, s.timeout)
	oldest, err := s.store.OldestPendingEvent(check)
	cancel()
	s.metrics.Outbox(oldest, err == nil)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("metric collection failed", "dependency", "postgres")
	}
	check, cancel = context.WithTimeout(ctx, s.timeout)
	count, err := s.queue.DLQVisible(check)
	cancel()
	s.metrics.DLQ(count, err == nil)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("metric collection failed", "dependency", "sqs")
	}
}
