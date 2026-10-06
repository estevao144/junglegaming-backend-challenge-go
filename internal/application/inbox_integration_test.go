//go:build integration

package application_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/postgres"
	sqstransport "jungle-gaming/internal/transport/sqs"
	"jungle-gaming/internal/workers"
)

type operationQueue struct {
	client *sqs.Client
	queue  *messaging.Queue
	url    string
	dlq    string
	config config.Config
}

func newOperationQueue(t *testing.T, f fixture) operationQueue {
	t.Helper()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_SQS_ENDPOINT required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(f.ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint); o.RetryMaxAttempts = 1 })
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "part4b-" + hex.EncodeToString(random[:])
	dlq, err := client.CreateQueue(f.ctx, &sqs.CreateQueueInput{QueueName: aws.String(name + "-dlq.fifo"), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := client.GetQueueAttributes(f.ctx, &sqs.GetQueueAttributesInput{QueueUrl: dlq.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	redrive, err := json.Marshal(map[string]string{"deadLetterTargetArn": attrs.Attributes["QueueArn"], "maxReceiveCount": "2"})
	if err != nil {
		t.Fatal(err)
	}
	main, err := client.CreateQueue(f.ctx, &sqs.CreateQueueInput{QueueName: aws.String(name + ".fifo"), Attributes: map[string]string{
		"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": "1", "RedrivePolicy": string(redrive)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, url := range []*string{main.QueueUrl, dlq.QueueUrl} {
			if _, err := client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: url}); err != nil {
				t.Error(err)
			}
		}
	})
	c := config.Config{DatabaseURL: f.url, AWSRegion: "us-east-1", SQSEndpoint: endpoint, SQSQueueName: name + ".fifo", DependencyTimeout: 3 * time.Second,
		ConsumerName: "financial-operations-v1", ConsumerConcurrency: 2, ConsumerBatchSize: 1, ConsumerWaitSeconds: 1, ConsumerVisibilitySeconds: 1, ConsumerProcessTimeout: 3 * time.Second}
	var queue *messaging.Queue
	app := fx.New(fx.NopLogger, fx.Supply(c), messaging.Module, fx.Populate(&queue))
	if err := app.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return operationQueue{client: client, queue: queue, url: aws.ToString(main.QueueUrl), dlq: aws.ToString(dlq.QueueUrl), config: c}
}

func operationBody(t *testing.T, c application.ProcessCommand, messageID string) string {
	t.Helper()
	envelope := sqstransport.OperationEnvelope{MessageID: messageID, Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC(), CorrelationID: "sqs-correlation"}
	envelope.Data = sqstransport.OperationData{ProviderID: c.ProviderID, ExternalTransactionID: c.ExternalTransactionID, IdempotencyKey: c.IdempotencyKey, PlayerID: c.PlayerID, WalletID: c.WalletID, RoundID: c.RoundID, GameID: c.GameID, Kind: c.Kind, ReferenceExternalTransactionID: c.ReferenceExternalTransactionID}
	amount, err := c.Money.Decimal()
	if err != nil {
		t.Fatal(err)
	}
	envelope.Data.Money.Amount = amount
	envelope.Data.Money.Currency = c.Money.Currency()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func sendOperation(t *testing.T, f fixture, q operationQueue, body, group, dedup string) {
	t.Helper()
	if _, err := q.client.SendMessage(f.ctx, &sqs.SendMessageInput{QueueUrl: aws.String(q.url), MessageBody: aws.String(body), MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(dedup)}); err != nil {
		t.Fatal(err)
	}
}
func receiveOperation(t *testing.T, f fixture, q operationQueue) types.Message {
	t.Helper()
	messages, err := q.queue.Receive(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected a real SQS delivery, received %d", len(messages))
	}
	return messages[0]
}
func assertQueueDeleted(t *testing.T, f fixture, q operationQueue) {
	t.Helper()
	attrs, err := q.client.GetQueueAttributes(f.ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(q.url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Attributes["ApproximateNumberOfMessages"] != "0" || attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] != "0" {
		t.Fatal("message was not deleted after durable resolution")
	}
}
func newConsumer(f fixture, q operationQueue) *workers.OperationConsumer {
	return workers.NewOperationConsumer(q.queue, f.service, q.config, outboxLogger())
}

func TestInboxBetWinLossViaRealSQS(t *testing.T) {
	for _, test := range []struct {
		kind             domain.TransactionKind
		amount           string
		balance, version int64
		ledger, events   int
	}{
		{domain.Bet, "25.00", 7500, 2, 2, 4}, {domain.Win, "25.00", 12500, 2, 2, 4}, {domain.Loss, "0.00", 10000, 1, 1, 3},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "sqs-player", "100.00")
			cmd := command(t, w, test.kind, test.amount, "sqs-operation")
			sendOperation(t, f, q, operationBody(t, cmd, "logical-message"), w.Snapshot().ID, "delivery-1")
			if err := newConsumer(f, q).Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
				t.Fatal(err)
			}
			assertBalance(t, f, w.Snapshot().ID, test.balance, test.version)
			if count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != test.ledger || count(t, f, "SELECT count(*) FROM outbox_events") != test.events {
				t.Fatal("financial effects differ from direct service")
			}
			if count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='PROCESSED' AND transaction_id IS NOT NULL AND processed_at IS NOT NULL") != 1 {
				t.Fatal("inbox not resolved")
			}
			assertQueueDeleted(t, f, q)
		})
	}
}

func TestInboxCrashAfterCommitBeforeDelete(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	w := open(t, f, "crash-inbox-player", "100.00")
	cmd := command(t, w, domain.Bet, "25.00", "crash-bet")
	sendOperation(t, f, q, operationBody(t, cmd, "stable-envelope"), w.Snapshot().ID, "crash-delivery")
	first := receiveOperation(t, f, q)
	consumer := newConsumer(f, q)
	result, err := consumer.Resolve(f.ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if result.InboxReplay {
		t.Fatal("first delivery replayed")
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='PROCESSED'") != 1 {
		t.Fatal("commit omitted inbox")
	}
	<-time.After(1100 * time.Millisecond) // Real visibility expiry, not duplicate SendMessage.
	second := receiveOperation(t, f, q)
	if aws.ToString(first.MessageId) != aws.ToString(second.MessageId) || second.Attributes["ApproximateReceiveCount"] != "2" {
		t.Fatal("not a real broker redelivery")
	}
	other := startInstance(t, f.url)
	consumer = workers.NewOperationConsumer(q.queue, other.service, q.config, outboxLogger())
	replayed, err := consumer.Resolve(f.ctx, second)
	if err != nil || !replayed.InboxReplay || replayed.TransactionID != result.TransactionID {
		t.Fatalf("durable inbox replay failed: %v", err)
	}
	if err := consumer.Handle(f.ctx, second); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, "SELECT count(*) FROM wager_transactions") != 2 || count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != 2 || count(t, f, "SELECT count(*) FROM outbox_events") != 4 || count(t, f, "SELECT count(*) FROM inbox_messages") != 1 {
		t.Fatal("redelivery duplicated financial effects")
	}
	assertQueueDeleted(t, f, q)
}

func TestInboxTransientRollbackThenRedelivery(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	w := open(t, f, "transient-player", "100.00")
	_, err := f.database.Pool().Exec(f.ctx, `CREATE FUNCTION fail_inbox_commit() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled infrastructure failure' USING ERRCODE='40001'; END; $$;
		CREATE CONSTRAINT TRIGGER fail_inbox_commit AFTER UPDATE ON inbox_messages DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_inbox_commit()`)
	if err != nil {
		t.Fatal(err)
	}
	sendOperation(t, f, q, operationBody(t, command(t, w, domain.Bet, "25.00", "retry-bet"), "retry-envelope"), w.Snapshot().ID, "retry-delivery")
	consumer := newConsumer(f, q)
	first := receiveOperation(t, f, q)
	if err := consumer.Handle(f.ctx, first); err == nil {
		t.Fatal("expected real commit failure")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, "SELECT count(*) FROM inbox_messages") != 0 || count(t, f, "SELECT count(*) FROM wager_transactions") != 1 || count(t, f, "SELECT count(*) FROM outbox_events") != 2 {
		t.Fatal("commit failure retained partial work")
	}
	if _, err := f.database.Pool().Exec(f.ctx, "DROP TRIGGER fail_inbox_commit ON inbox_messages; DROP FUNCTION fail_inbox_commit()"); err != nil {
		t.Fatal(err)
	}
	<-time.After(1100 * time.Millisecond)
	second := receiveOperation(t, f, q)
	if aws.ToString(second.MessageId) != aws.ToString(first.MessageId) {
		t.Fatal("message was deleted on transient failure")
	}
	if err := consumer.Handle(f.ctx, second); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	assertQueueDeleted(t, f, q)
}

func TestInboxPoisonMessageRedrivesToRealDLQ(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	w := open(t, f, "poison-player", "100.00")
	sendOperation(t, f, q, "{", w.Snapshot().ID, "poison")
	consumer := newConsumer(f, q)
	for i := 0; i < 2; i++ {
		message := receiveOperation(t, f, q)
		if err := consumer.Handle(f.ctx, message); !errors.Is(err, sqstransport.ErrPoisonMessage) {
			t.Fatal("expected poison classification")
		}
		<-time.After(1100 * time.Millisecond)
	}
	// The next receive lets SQS's RedrivePolicy move the exhausted delivery.
	if _, err := q.queue.Receive(f.ctx); err != nil {
		t.Fatal(err)
	}
	dead, err := q.client.ReceiveMessage(f.ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(q.dlq), MaxNumberOfMessages: 1, WaitTimeSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(dead.Messages) != 1 || aws.ToString(dead.Messages[0].Body) != "{" {
		t.Fatal("poison did not reach broker-managed DLQ")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, "SELECT count(*) FROM inbox_messages") != 0 || count(t, f, "SELECT count(*) FROM wager_transactions") != 1 {
		t.Fatal("poison produced financial effects")
	}
}

func TestInboxDifferentMessagesAndCrossTransportReplay(t *testing.T) {
	for _, directFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct-first-%t", directFirst), func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "idempotent-player", "100.00")
			cmd := command(t, w, domain.Bet, "25.00", "shared-bet")
			if directFirst {
				if _, err := f.service.Process(f.ctx, cmd); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				id := fmt.Sprintf("envelope-%d", i)
				sendOperation(t, f, q, operationBody(t, cmd, id), w.Snapshot().ID, id)
			}
			consumer := newConsumer(f, q)
			for i := 0; i < 2; i++ {
				if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
					t.Fatal(err)
				}
			}
			assertBalance(t, f, w.Snapshot().ID, 7500, 2)
			if count(t, f, "SELECT count(*) FROM inbox_messages") != 2 || count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != 2 || count(t, f, "SELECT count(*) FROM outbox_events") != 4 {
				t.Fatal("distinct deliveries duplicated financial operation")
			}
			assertQueueDeleted(t, f, q)
		})
	}
}

func TestInboxTerminalRejectionsAndSavepoint(t *testing.T) {
	for _, test := range []struct {
		name                  string
		kind                  domain.TransactionKind
		amount, initial, code string
		financial             bool
	}{
		{"insufficient", domain.Bet, "200.00", "100.00", "INSUFFICIENT_BALANCE", true},
		{"unsupported", domain.TransactionKind("UNKNOWN"), "25.00", "100.00", "UNSUPPORTED_OPERATION", false},
		{"overflow", domain.Win, "1.00", "92233720368547758.07", "ARITHMETIC_OVERFLOW", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "terminal-player", test.initial)
			sendOperation(t, f, q, operationBody(t, command(t, w, test.kind, test.amount, "terminal-operation"), "terminal-envelope"), w.Snapshot().ID, "terminal")
			if err := newConsumer(f, q).Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
				t.Fatal(err)
			}
			if count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='REJECTED' AND failure_code=$1", test.code) != 1 {
				t.Fatal("terminal rejection not durable")
			}
			expectedTx, expectedEvents := 1, 2
			if test.financial {
				expectedTx++
				expectedEvents++
			}
			if count(t, f, "SELECT count(*) FROM wager_transactions") != expectedTx || count(t, f, "SELECT count(*) FROM outbox_events") != expectedEvents || count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != 1 {
				t.Fatal("rejection retained partial financial effects")
			}
			assertQueueDeleted(t, f, q)
		})
	}
}

func TestInboxIdentityHashConflict(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	w := open(t, f, "hash-player", "100.00")
	cmd := command(t, w, domain.Bet, "25.00", "hash-bet")
	body := operationBody(t, cmd, "same-envelope")
	incoming, err := sqstransport.ParseOperation(body, q.config.ConsumerName, q.queue.Source())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ProcessIncoming(f.ctx, incoming); err != nil {
		t.Fatal(err)
	}
	incoming.PayloadHash = fmt.Sprintf("%064d", 1)
	if _, err := f.service.ProcessIncoming(f.ctx, incoming); !errors.Is(err, postgres.ErrInboxIdentity) {
		t.Fatal("changed envelope accepted as inbox replay")
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
}

func TestInboxFxConsumersIndependentWallets(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	a := open(t, f, "blocked-player", "100.00")
	b := open(t, f, "free-player", "100.00")
	tx, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", a.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	for i, w := range []*domain.Wallet{a, b} {
		id := fmt.Sprintf("parallel-%d", i)
		sendOperation(t, f, q, operationBody(t, command(t, w, domain.Bet, "25.00", id), id), w.Snapshot().ID, id)
	}
	var db *postgres.Database
	app := fx.New(fx.NopLogger, fx.Supply(q.config, q.queue, outboxLogger()), postgres.Module, fx.Provide(postgres.NewStore, application.NewFinancialService), workers.ConsumerModule, fx.Populate(&db))
	if err := app.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='PROCESSED'") < 1 {
		if time.Now().After(deadline) {
			t.Fatal("independent wallet blocked behind other consumer")
		}
		<-time.After(10 * time.Millisecond)
	}
	assertBalance(t, f, b.Snapshot().ID, 7500, 2)
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	for count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='PROCESSED'") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("blocked wallet did not recover")
		}
		<-time.After(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Check(ctx); err == nil {
		t.Fatal("pool remained open after consumer shutdown")
	}
}

func TestInboxSemanticInputAndFinancialConflicts(t *testing.T) {
	for _, scenario := range []string{"invalid currency", "key reused", "external ID reused"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "conflict-inbox-player", "100.00")
			cmd := command(t, w, domain.Bet, "25.00", "original")
			code := "INVALID_MONEY"
			if scenario != "invalid currency" {
				if _, err := f.service.Process(f.ctx, cmd); err != nil {
					t.Fatal(err)
				}
				code = "IDEMPOTENCY_CONFLICT"
				if scenario == "key reused" {
					cmd.Money = money(t, "26.00")
				} else {
					cmd.IdempotencyKey = "another-key"
				}
			}
			body := operationBody(t, cmd, "conflicting-envelope")
			if scenario == "invalid currency" {
				body = strings.Replace(body, "BRL", "USD", 1)
			}
			sendOperation(t, f, q, body, w.Snapshot().ID, "conflict-delivery")
			if err := newConsumer(f, q).Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
				t.Fatal(err)
			}
			if count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='REJECTED' AND failure_code=$1", code) != 1 {
				t.Fatal("permanent conflict not durably rejected")
			}
			balance, version := int64(10000), int64(1)
			if scenario != "invalid currency" {
				balance = 7500
				version = 2
			}
			assertBalance(t, f, w.Snapshot().ID, balance, version)
			assertQueueDeleted(t, f, q)
		})
	}
}

func TestInboxDatabaseConstraints(t *testing.T) {
	f := newFixture(t)
	if _, err := f.database.Pool().Exec(f.ctx, `INSERT INTO inbox_messages(consumer_name,source,message_id,payload_hash,correlation_id,status)
		VALUES ('consumer','source','pending',repeat('a',64),'correlation','PENDING')`); err == nil {
		t.Fatal("PENDING inbox committed without resolution")
	}
	w := open(t, f, "inbox-constraint-player", "100.00")
	q := newOperationQueue(t, f)
	body := operationBody(t, command(t, w, domain.Loss, "0.00", "constraint-loss"), "constraint-envelope")
	incoming, err := sqstransport.ParseOperation(body, q.config.ConsumerName, q.queue.Source())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ProcessIncoming(f.ctx, incoming); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"UPDATE inbox_messages SET payload_hash=repeat('b',64)", "UPDATE inbox_messages SET status='REJECTED',failure_code='INVALID_INPUT'", "DELETE FROM inbox_messages"} {
		if _, err := f.database.Pool().Exec(f.ctx, query); err == nil {
			t.Fatalf("inbox integrity bypass: %s", query)
		}
	}
}
