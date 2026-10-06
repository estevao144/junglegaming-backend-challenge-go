package workers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
)

const consumerBody = `{"messageId":"envelope","type":"WagerTransactionRequested","occurredAt":"2026-10-06T12:00:00Z","data":{"providerId":"provider","externalTransactionId":"external","idempotencyKey":"key","playerId":"player","walletId":"wallet","roundId":"round","gameId":"game","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`

type fakeOperationQueue struct {
	deleted        int
	receiveStarted chan struct{}
	message        *types.Message
}

func (q *fakeOperationQueue) Source() string { return "source" }
func (q *fakeOperationQueue) Receive(ctx context.Context) ([]types.Message, error) {
	if q.message != nil {
		message := *q.message
		q.message = nil
		return []types.Message{message}, nil
	}
	if q.receiveStarted != nil {
		close(q.receiveStarted)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (q *fakeOperationQueue) Delete(context.Context, types.Message) error { q.deleted++; return nil }

type incomingFunc func(context.Context, application.IncomingOperation) (application.IncomingResult, error)

func (f incomingFunc) ProcessIncoming(ctx context.Context, incoming application.IncomingOperation) (application.IncomingResult, error) {
	return f(ctx, incoming)
}
func consumerConfig() config.Config {
	return config.Config{ConsumerName: "consumer", ConsumerConcurrency: 1, ConsumerWaitSeconds: 20, ConsumerProcessTimeout: time.Second, DependencyTimeout: time.Second}
}

func TestConsumerAcknowledgementDecision(t *testing.T) {
	for _, test := range []struct {
		name, body, status string
		err                error
		wantDelete         int
	}{
		{"success", consumerBody, "PROCESSED", nil, 1}, {"rejected", consumerBody, "REJECTED", nil, 1},
		{"transient", consumerBody, "", errors.New("database unavailable"), 0}, {"poison", "{", "", nil, 0},
		{"unresolved", consumerBody, "PENDING", nil, 0},
		{"durable reference wait", consumerBody, "PENDING_REFERENCE", nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := &fakeOperationQueue{}
			service := incomingFunc(func(_ context.Context, incoming application.IncomingOperation) (application.IncomingResult, error) {
				if incoming.Command.IdempotencyKey != "key" {
					t.Fatal("key changed")
				}
				return application.IncomingResult{Status: test.status}, test.err
			})
			consumer := NewOperationConsumer(q, service, consumerConfig(), quietLogger())
			_ = consumer.Handle(context.Background(), types.Message{MessageId: aws.String("broker-id"), Body: aws.String(test.body)})
			if q.deleted != test.wantDelete {
				t.Fatal("unsafe delete/redelivery decision")
			}
		})
	}
}

func TestConsumerShutdownCancelsLongPoll(t *testing.T) {
	q := &fakeOperationQueue{receiveStarted: make(chan struct{})}
	consumer := NewOperationConsumer(q, nil, consumerConfig(), quietLogger())
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-q.receiveStarted
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := consumer.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-consumer.done:
	default:
		t.Fatal("consumer goroutine leaked")
	}
}

func TestConsumerShutdownFinishesCommittedWork(t *testing.T) {
	message := types.Message{Body: aws.String(consumerBody)}
	q := &fakeOperationQueue{message: &message}
	started := make(chan struct{})
	finish := make(chan struct{})
	service := incomingFunc(func(ctx context.Context, _ application.IncomingOperation) (application.IncomingResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			return application.IncomingResult{}, ctx.Err()
		case <-finish:
			return application.IncomingResult{Status: "PROCESSED"}, nil
		}
	})
	consumer := NewOperationConsumer(q, service, consumerConfig(), quietLogger())
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stopped <- consumer.Stop(ctx)
	}()
	close(finish)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if q.deleted != 1 {
		t.Fatal("shutdown failed to acknowledge committed work")
	}
}
