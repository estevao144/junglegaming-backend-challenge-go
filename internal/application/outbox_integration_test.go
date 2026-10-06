//go:build integration

package application_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/workers"
	"jungle-gaming/migrations"
)

type eventQueue struct {
	client    *sqs.Client
	url       string
	publisher *messaging.EventPublisher
	config    config.Config
}

func newEventQueue(t *testing.T, f fixture) eventQueue {
	t.Helper()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_SQS_ENDPOINT is required; start real LocalStack")
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
	name := "part4a-" + hex.EncodeToString(random[:]) + ".fifo"
	created, err := client.CreateQueue(f.ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"}})
	if err != nil {
		t.Fatalf("real LocalStack unavailable: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: created.QueueUrl}); err != nil {
			t.Error(err)
		}
	})
	c := config.Config{DatabaseURL: f.url, AWSRegion: "us-east-1", SQSEndpoint: endpoint, SQSEventsQueueName: name, DependencyTimeout: 3 * time.Second,
		OutboxBatchSize: 4, OutboxPollInterval: 10 * time.Millisecond, OutboxLease: 10 * time.Second, OutboxRetryBase: 200 * time.Millisecond, OutboxRetryMax: time.Second}
	var publisher *messaging.EventPublisher
	app := fx.New(fx.NopLogger, fx.Supply(c), fx.Provide(messaging.NewEventPublisher), fx.Populate(&publisher))
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
	return eventQueue{client: client, url: aws.ToString(created.QueueUrl), publisher: publisher, config: c}
}

func outboxLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func snapshots(t *testing.T, f fixture) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	rows, err := f.database.Pool().Query(f.ctx, "SELECT event_id,payload FROM outbox_events ORDER BY delivery_order")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			t.Fatal(err)
		}
		result[id] = payload
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func drainOutbox(t *testing.T, f fixture, worker *workers.OutboxWorker) {
	t.Helper()
	for count(t, f, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") > 0 {
		n, err := worker.RunOnce(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("pending events are unexpectedly unavailable")
		}
	}
}

func assertReceivedSnapshots(t *testing.T, f fixture, q eventQueue, expected map[string][]byte) {
	t.Helper()
	received := map[string]bool{}
	positions := map[string]int64{}
	rows, err := f.database.Pool().Query(f.ctx, "SELECT event_id, delivery_order FROM outbox_events")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var position int64
		if err := rows.Scan(&id, &position); err != nil {
			t.Fatal(err)
		}
		positions[id] = position
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	lastPosition := map[string]int64{}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	for len(received) < len(expected) {
		messages, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(q.url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll}})
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages.Messages {
			body := []byte(aws.ToString(message.Body))
			matched := ""
			for id, payload := range expected {
				if bytes.Equal(body, payload) {
					matched = id
					break
				}
			}
			if matched == "" {
				t.Fatal("SQS body differs from persisted outbox snapshot")
			}
			if received[matched] {
				t.Fatal("unexpected normal-path duplicate")
			}
			received[matched] = true
			var routing struct {
				Data struct {
					WalletID string `json:"walletId"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &routing); err != nil {
				t.Fatal(err)
			}
			walletID := routing.Data.WalletID
			if positions[matched] <= lastPosition[walletID] {
				t.Fatal("SQS reordered wallet events")
			}
			lastPosition[walletID] = positions[matched]
			input, err := messaging.BuildEventMessage(q.url, postgres.ClaimedEvent{OutboxRecord: postgres.OutboxRecord{EventID: matched, Payload: body}, WalletID: walletID})
			if err != nil {
				t.Fatal(err)
			}
			if message.Attributes["MessageGroupId"] != aws.ToString(input.MessageGroupId) || message.Attributes["MessageDeduplicationId"] != aws.ToString(input.MessageDeduplicationId) {
				t.Fatal("SQS FIFO routing mismatch")
			}
			if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(q.url), ReceiptHandle: message.ReceiptHandle}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestOutboxNormalPublication(t *testing.T) {
	f := newFixture(t)
	q := newEventQueue(t, f)
	w := open(t, f, "snapshot-player", "100.00")
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "snapshot-bet")); err != nil {
		t.Fatal(err)
	}
	expected := snapshots(t, f)
	if len(expected) != 4 || count(t, f, "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL") != 0 {
		t.Fatal("financial commit must leave pending snapshots")
	}
	repo := postgres.NewOutboxDelivery(f.database)
	worker := workers.NewOutboxWorker(repo, q.publisher, q.config, outboxLogger())
	drainOutbox(t, f, worker)
	assertReceivedSnapshots(t, f, q, expected)
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL") != 4 {
		t.Fatal("publication missing")
	}
	if events, err := repo.Claim(f.ctx, 10, time.Second); err != nil || len(events) != 0 {
		t.Fatalf("published event claimed again: %v", err)
	}
	for id, payload := range snapshots(t, f) {
		if !bytes.Equal(payload, expected[id]) {
			t.Fatal("published record snapshot changed")
		}
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
}

type observedPublisher struct {
	publisher *messaging.EventPublisher
	f         fixture
	mu        sync.Mutex
	calls     map[string]int
}

func (p *observedPublisher) Publish(ctx context.Context, event postgres.ClaimedEvent) error {
	p.mu.Lock()
	p.calls[event.EventID]++
	p.mu.Unlock()
	// A separate PG transaction can immediately lock the same row while Send
	// runs: the claim transaction has already committed, no network row lock.
	tx, err := p.f.database.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "SELECT event_id FROM outbox_events WHERE event_id=$1 FOR UPDATE NOWAIT", event.EventID)
	rollbackErr := tx.Rollback(ctx)
	if err != nil {
		return err
	}
	if rollbackErr != nil {
		return rollbackErr
	}
	return p.publisher.Publish(ctx, event)
}

func TestOutboxConcurrentIndependentPublishers(t *testing.T) {
	f := newFixture(t)
	q := newEventQueue(t, f)
	for i := 0; i < 8; i++ {
		w := open(t, f, fmt.Sprintf("concurrent-player-%d", i), "100.00")
		if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "10.00", fmt.Sprintf("outbox-bet-%d", i))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.Process(f.ctx, command(t, w, domain.Loss, "0.00", fmt.Sprintf("outbox-loss-%d", i))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "100.00", fmt.Sprintf("outbox-reject-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	expected := snapshots(t, f)
	second := startInstance(t, f.url)
	observed := &observedPublisher{publisher: q.publisher, f: f, calls: map[string]int{}}
	repos := []*postgres.OutboxDelivery{postgres.NewOutboxDelivery(f.database), postgres.NewOutboxDelivery(second.database)}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, repo := range repos {
		wg.Add(1)
		go func(repository *postgres.OutboxDelivery) {
			defer wg.Done()
			worker := workers.NewOutboxWorker(repository, observed, q.config, outboxLogger())
			for {
				var pending int
				if err := f.database.Pool().QueryRow(f.ctx, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL").Scan(&pending); err != nil {
					failures <- err
					return
				}
				if pending == 0 {
					return
				}
				if _, err := worker.RunOnce(f.ctx); err != nil {
					failures <- err
					return
				}
				select {
				case <-f.ctx.Done():
					failures <- f.ctx.Err()
					return
				case <-time.After(time.Millisecond):
				}
			}
		}(repo)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if len(observed.calls) != len(expected) {
		t.Fatal("lost events")
	}
	for id, calls := range observed.calls {
		if calls != 1 {
			t.Fatalf("event %s sent %d times; correctness cannot depend on FIFO dedup", id, calls)
		}
	}
	assertReceivedSnapshots(t, f, q, expected)
}

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, postgres.ClaimedEvent) error {
	return errors.New("controlled temporary send failure")
}

func TestOutboxTemporaryFailureDurableRetry(t *testing.T) {
	f := newFixture(t)
	q := newEventQueue(t, f)
	open(t, f, "retry-player", "1.00")
	expected := snapshots(t, f)
	repo := postgres.NewOutboxDelivery(f.database)
	worker := workers.NewOutboxWorker(repo, failingPublisher{}, q.config, outboxLogger())
	if _, err := worker.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	var id string
	var attempts int
	var next, now time.Time
	var token *string
	if err := f.database.Pool().QueryRow(f.ctx, "SELECT event_id,attempts,next_attempt_at,clock_timestamp(),locked_by FROM outbox_events WHERE attempts=1").Scan(&id, &attempts, &next, &now, &token); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !next.After(now) || token != nil {
		t.Fatal("retry not persisted or claim not released")
	}
	if events, err := repo.Claim(f.ctx, 4, time.Second); err != nil || len(events) != 0 {
		t.Fatal("backoff or wallet order bypassed")
	}
	if _, err := f.database.Pool().Exec(f.ctx, "UPDATE outbox_events SET next_attempt_at=clock_timestamp() WHERE event_id=$1", id); err != nil {
		t.Fatal(err)
	}
	worker = workers.NewOutboxWorker(repo, q.publisher, q.config, outboxLogger())
	drainOutbox(t, f, worker)
	assertReceivedSnapshots(t, f, q, expected)
	if !bytes.Equal(snapshots(t, f)[id], expected[id]) {
		t.Fatal("retry changed identity/snapshot")
	}
}

func TestOutboxCrashAfterRealSendAndStaleOwner(t *testing.T) {
	f := newFixture(t)
	q := newEventQueue(t, f)
	open(t, f, "crash-player", "1.00")
	repo := postgres.NewOutboxDelivery(f.database)
	first, err := repo.Claim(f.ctx, 1, time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v", err)
	}
	if err := q.publisher.Publish(f.ctx, first[0]); err != nil {
		t.Fatal(err)
	} // Intentionally no MarkPublished.
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL") != 0 {
		t.Fatal("send acknowledged prematurely")
	}
	if _, err := f.database.Pool().Exec(f.ctx, "UPDATE outbox_events SET locked_until=clock_timestamp()-interval '1 second' WHERE event_id=$1", first[0].EventID); err != nil {
		t.Fatal(err)
	}
	secondInstance := startInstance(t, f.url)
	secondRepo := postgres.NewOutboxDelivery(secondInstance.database)
	second, err := secondRepo.Claim(f.ctx, 1, time.Second)
	if err != nil || len(second) != 1 {
		t.Fatalf("recovery claim: %v", err)
	}
	if first[0].EventID != second[0].EventID || first[0].ClaimToken == second[0].ClaimToken || !bytes.Equal(first[0].Payload, second[0].Payload) {
		t.Fatal("recovery must retain identity and change owner")
	}
	a, err := messaging.BuildEventMessage(q.url, first[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := messaging.BuildEventMessage(q.url, second[0])
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(a.MessageDeduplicationId) != aws.ToString(b.MessageDeduplicationId) {
		t.Fatal("crash retry changed FIFO dedup ID")
	}
	if !errors.Is(repo.MarkPublished(f.ctx, first[0]), postgres.ErrClaimLost) {
		t.Fatal("stale owner acknowledged new claim")
	}
	if !errors.Is(repo.Retry(f.ctx, first[0], time.Second), postgres.ErrClaimLost) {
		t.Fatal("stale owner changed retry")
	}
	if !errors.Is(repo.Renew(f.ctx, first[0], time.Second), postgres.ErrClaimLost) {
		t.Fatal("stale owner renewed claim")
	}
	if err := q.publisher.Publish(f.ctx, second[0]); err != nil {
		t.Fatal(err)
	}
	if err := secondRepo.MarkPublished(f.ctx, second[0]); err != nil {
		t.Fatal(err)
	}
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE event_id=$1 AND published_at IS NOT NULL", second[0].EventID) != 1 {
		t.Fatal("crashed event lost")
	}
	// FIFO may suppress the second physical message; recovery never depends on it.
	assertReceivedSnapshots(t, f, q, map[string][]byte{first[0].EventID: first[0].Payload})
}

func TestOutboxLeaseExpiresAndSkipsLockedWallet(t *testing.T) {
	f := newFixture(t)
	open(t, f, "lease-player-a", "1.00")
	open(t, f, "lease-player-b", "1.00")
	repo := postgres.NewOutboxDelivery(f.database)
	first, err := repo.Claim(f.ctx, 1, 40*time.Millisecond)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v", err)
	}
	<-time.After(60 * time.Millisecond)
	recovered, err := repo.Claim(f.ctx, 1, time.Second)
	if err != nil || len(recovered) != 1 || recovered[0].EventID != first[0].EventID {
		t.Fatalf("expired lease not recovered: %v", err)
	}
	if _, err := f.database.Pool().Exec(f.ctx, "UPDATE outbox_events SET locked_until=clock_timestamp()-interval '1 second' WHERE event_id=$1", first[0].EventID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, "SELECT event_id FROM outbox_events WHERE event_id=$1 FOR UPDATE", first[0].EventID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	other, err := repo.Claim(ctx, 4, time.Second)
	if err != nil || len(other) != 1 || other[0].WalletID == first[0].WalletID {
		t.Fatalf("SKIP LOCKED must let independent wallet progress: %v", err)
	}
}

func TestOutboxFxLifecycle(t *testing.T) {
	f := newFixture(t)
	q := newEventQueue(t, f)
	open(t, f, "fx-player", "1.00")
	var db *postgres.Database
	app := fx.New(fx.NopLogger, fx.Supply(q.config, outboxLogger()), postgres.Module, workers.Module, fx.Populate(&db))
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
	deadline := time.Now().Add(5 * time.Second)
	for count(t, f, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") > 0 {
		if time.Now().After(deadline) {
			t.Fatal("Fx did not start publisher")
		}
		<-time.After(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Check(ctx); err == nil {
		t.Fatal("pool stayed open after worker shutdown")
	}
}

func TestOutboxMigrationUpgradePreservesPart3(t *testing.T) {
	f := newFixture(t)
	down, err := os.ReadFile("../../migrations/0002_outbox_delivery.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.Pool().Exec(f.ctx, string(down)+" DELETE FROM schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	open(t, f, "upgrade-player", "1.00")
	var checksum string
	if err := f.database.Pool().QueryRow(f.ctx, "SELECT checksum FROM schema_migrations WHERE version=1").Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(f.ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(f.ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
	if count(t, f, "SELECT count(*) FROM schema_migrations WHERE version=1 AND checksum=$1", checksum) != 1 {
		t.Fatal("0001 checksum modified")
	}
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE wallet_id IS NOT NULL AND delivery_order>0") != 2 {
		t.Fatal("existing events lost during upgrade")
	}
	if _, err := f.database.Pool().Exec(f.ctx, "UPDATE outbox_events SET delivery_order=delivery_order+100"); err == nil {
		t.Fatal("delivery order can be changed")
	}
}

func TestOutboxDoesNotPublishUncommittedEvent(t *testing.T) {
	f := newFixture(t)
	q := newEventQueue(t, f)
	open(t, f, "uncommitted-player", "1.00")
	repo := postgres.NewOutboxDelivery(f.database)
	worker := workers.NewOutboxWorker(repo, q.publisher, q.config, outboxLogger())
	drainOutbox(t, f, worker)
	tx, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	_, err = tx.Exec(f.ctx, `INSERT INTO outbox_events(event_id,aggregate_id,event_type,payload,occurred_at,next_attempt_at)
		SELECT 'uncommitted-event',aggregate_id,event_type,
		jsonb_set(payload,'{eventId}','"uncommitted-event"'),occurred_at,clock_timestamp()
		FROM outbox_events ORDER BY delivery_order LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	if events, err := repo.Claim(f.ctx, 10, time.Second); err != nil || len(events) != 0 {
		t.Fatalf("uncommitted event became visible: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE event_id='uncommitted-event' AND published_at IS NOT NULL") != 1 {
		t.Fatal("committed event did not publish")
	}
}
