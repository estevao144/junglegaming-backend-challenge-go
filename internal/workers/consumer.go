package workers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/observability"
	"jungle-gaming/internal/platform/postgres"
	sqstransport "jungle-gaming/internal/transport/sqs"
)

type OperationQueue interface {
	Source() string
	Receive(context.Context) ([]types.Message, error)
	Delete(context.Context, types.Message) error
	Release(context.Context, types.Message) error
}

type IncomingService interface {
	ProcessIncoming(context.Context, application.IncomingOperation) (application.IncomingResult, error)
}

type OperationConsumer struct {
	metrics    *observability.Metrics
	queue      OperationQueue
	service    IncomingService
	config     config.Config
	logger     *slog.Logger
	stopFetch  context.CancelFunc
	cancelWork context.CancelFunc
	done       chan struct{}
}

func NewOperationConsumer(queue OperationQueue, service IncomingService, c config.Config, logger *slog.Logger) *OperationConsumer {
	return &OperationConsumer{queue: queue, service: service, config: c, logger: logger}
}

func RegisterConsumer(lc fx.Lifecycle, queue *messaging.Queue, service *application.AuthorizedIncomingService, c config.Config, logger *slog.Logger, metrics *observability.Metrics) *OperationConsumer {
	consumer := NewOperationConsumer(queue, service, c, logger)
	consumer.metrics = metrics
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		check, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
		defer cancel()
		if err := service.CheckInbox(check); err != nil {
			return err
		}
		return consumer.Start(ctx)
	}, OnStop: consumer.Stop})
	return consumer
}

var ConsumerModule = fx.Module("operation-consumer", fx.Provide(RegisterConsumer), fx.Invoke(func(*OperationConsumer) {}))

func (c *OperationConsumer) Start(context.Context) error {
	fetch, stop := context.WithCancel(context.Background())
	work, cancel := context.WithCancel(context.Background())
	c.stopFetch = stop
	c.cancelWork = cancel
	c.done = make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < c.config.ConsumerConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fetch.Err() == nil {
				if err := c.runBatch(fetch, work); err != nil && fetch.Err() == nil {
					c.logger.Warn("SQS receive failed", "classification", "transient")
					// Broker failures do not become a tight polling loop.
					timer := time.NewTimer(time.Second)
					select {
					case <-fetch.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		cancel()
		close(c.done)
	}()
	return nil
}

func (c *OperationConsumer) Stop(ctx context.Context) error {
	c.stopFetch()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		c.cancelWork()
		<-c.done
		return ctx.Err()
	}
}

func (c *OperationConsumer) RunOnce(ctx context.Context) error { return c.runBatch(ctx, ctx) }

func (c *OperationConsumer) runBatch(fetch, work context.Context) error {
	ctx, cancel := context.WithTimeout(fetch, time.Duration(c.config.ConsumerWaitSeconds)*time.Second+c.config.DependencyTimeout)
	messages, err := c.queue.Receive(ctx)
	cancel()
	if err != nil {
		return err
	}
	for index, message := range messages {
		if fetch.Err() != nil {
			c.releaseMessages(messages[index:])
			break
		}
		if err := c.Handle(work, message); err != nil && fetch.Err() != nil {
			// A stopped consumer cannot retry this delivery. Commit ambiguity is
			// safe: redelivery reuses the durable inbox and financial identity.
			c.releaseMessages(messages[index:])
			break
		}
	}
	return nil
}

func (c *OperationConsumer) releaseMessages(messages []types.Message) {
	// Cleanup must outlive the canceled work context, but stays bounded while
	// SQS is still open. All messages share one cleanup budget.
	ctx, cancel := context.WithTimeout(context.Background(), c.config.DependencyTimeout)
	defer cancel()
	for _, message := range messages {
		if err := c.queue.Release(ctx, message); err != nil {
			c.logger.Warn("SQS shutdown visibility release failed", "messageId", aws.ToString(message.MessageId))
		}
	}
}

// Resolve deliberately does not acknowledge: tests use this production path to
// simulate a process dying after COMMIT and before DeleteMessage.
func (c *OperationConsumer) Resolve(ctx context.Context, message types.Message) (application.IncomingResult, error) {
	incoming, err := sqstransport.ParseOperation(aws.ToString(message.Body), c.config.ConsumerName, c.queue.Source())
	if err != nil {
		return application.IncomingResult{}, err
	}
	process, cancel := context.WithTimeout(ctx, c.config.ConsumerProcessTimeout)
	defer cancel()
	return c.service.ProcessIncoming(process, incoming)
}

func (c *OperationConsumer) Handle(ctx context.Context, message types.Message) error {
	started := time.Now()
	attributes := []any{"messageId", aws.ToString(message.MessageId), "receiveCount", message.Attributes["ApproximateReceiveCount"]}
	incoming, parseErr := sqstransport.ParseOperation(aws.ToString(message.Body), c.config.ConsumerName, c.queue.Source())
	if parseErr == nil {
		attributes = append(attributes, "envelopeMessageId", incoming.MessageID, "correlationId", incoming.CorrelationID, "walletId", incoming.Command.WalletID, "providerId", incoming.Command.ProviderID, "externalTransactionId", incoming.Command.ExternalTransactionID)
	}
	result, err := c.Resolve(ctx, message)
	attributes = append(attributes, "latencyMicros", time.Since(started).Microseconds())
	if err != nil {
		classification := "transient"
		if errors.Is(err, sqstransport.ErrPoisonMessage) || errors.Is(err, postgres.ErrInboxIdentity) {
			classification = "poison"
		}
		attributes = append(attributes, "classification", classification)
		c.logger.Warn("SQS operation left for redelivery", attributes...)
		c.metrics.Worker("consumer", "retry")
		if classification == "poison" {
			c.metrics.Worker("consumer", "poison")
		}
		return err
	}
	if result.Status != "PROCESSED" && result.Status != "REJECTED" && result.Status != "PENDING_REFERENCE" {
		return errors.New("incoming operation did not return a terminal resolution")
	}
	attributes = append(attributes, "transactionId", result.TransactionID, "result", result.Status, "failureCode", result.FailureCode, "inboxReplay", result.InboxReplay, "financialReplay", result.FinancialReplay)
	ack, cancel := context.WithTimeout(ctx, c.config.DependencyTimeout)
	defer cancel()
	if err := c.queue.Delete(ack, message); err != nil {
		c.logger.Warn("SQS durable operation acknowledgement failed", attributes...)
		c.metrics.Worker("consumer", "ack_failed")
		return err
	}
	c.logger.Info("SQS operation acknowledged after commit", attributes...)
	c.metrics.Worker("consumer", "processed")
	return nil
}
