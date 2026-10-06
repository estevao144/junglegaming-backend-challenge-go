//go:build integration

package application_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/workers"
	"jungle-gaming/migrations"
)

func referenceConfig(connection string) config.Config {
	return config.Config{DatabaseURL: connection, DependencyTimeout: time.Second, ReferenceBatchSize: 5,
		ReferenceLease: time.Second, ReferencePollInterval: 10 * time.Millisecond, ReferenceProcessTimeout: time.Second,
		ReferenceRetryBase: 20 * time.Millisecond, ReferenceRetryMax: 50 * time.Millisecond, ReferenceMaxAttempts: 1000}
}
func referenceWorker(node instance, c config.Config) *workers.ReferenceWorker {
	return workers.NewReferenceWorker(postgres.NewReferenceQueue(node.database), node.service, c, outboxLogger())
}
func claimReference(t *testing.T, f fixture, lease time.Duration) postgres.ReferenceClaim {
	t.Helper()
	claims, err := postgres.NewReferenceQueue(f.database).Claim(f.ctx, 1, lease)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v, count %d", err, len(claims))
	}
	return claims[0]
}
func requireStatus(t *testing.T, f fixture, external string, status domain.TransactionStatus) domain.WagerTransactionState {
	t.Helper()
	tx, err := f.store.GetExternalTransaction(f.ctx, "provider-a", external)
	if err != nil {
		t.Fatal(err)
	}
	state := tx.Snapshot()
	if state.Status != status {
		t.Fatalf("%s status %s, want %s", external, state.Status, status)
	}
	return state
}
func eventually(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition not reached")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestReferenceWorkerResolvesSameTransaction(t *testing.T) {
	for _, kind := range []domain.TransactionKind{domain.Refund, domain.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "late-reference", "100.00")
			cmd := refundCommand(t, w, "25.00", "pending", "late")
			originalKind := domain.Bet
			if kind == domain.Rollback {
				cmd.Kind = domain.Rollback
				originalKind = domain.Win
			}
			pending, err := f.service.Process(f.ctx, cmd)
			if err != nil || pending.Transaction.Status != domain.PendingReference {
				t.Fatalf("pending: %v", err)
			}
			original := processed(t, f, command(t, w, originalKind, "25.00", "late"))
			worker := referenceWorker(f.instance, referenceConfig(f.url))
			if n, err := worker.RunOnce(f.ctx); err != nil || n != 1 {
				t.Fatalf("resolution: %d %v", n, err)
			}
			state := requireStatus(t, f, "pending", domain.Processed)
			if state.Data.ID != pending.Transaction.Data.ID || state.ReferenceTransactionID != original.Transaction.Data.ID || state.Result.Balance.MinorUnits() != 10000 {
				t.Fatal("pending recreated or wrong result")
			}
			assertBalance(t, f, w.Snapshot().ID, 10000, 3)
			if count(t, f, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='WagerTransactionPendingReference'`, state.Data.ID) != 1 || count(t, f, `SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId'=$1`, state.Data.ID) != 3 {
				t.Fatal("pending event duplicated or resolution events missing")
			}
			if count(t, f, `SELECT count(*) FROM reference_retries WHERE transaction_id=$1 AND attempts=1 AND completed_at IS NOT NULL AND claimed_by IS NULL`, state.Data.ID) != 1 {
				t.Fatal("attempt audit missing")
			}
			if n, err := worker.RunOnce(f.ctx); err != nil || n != 0 {
				t.Fatal("terminal operation claimed again")
			}
		})
	}
}

func TestReferenceRetryBackoffAndExhaustion(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "missing-reference", "100.00")
	pending, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "pending", "never-arrives"))
	if err != nil {
		t.Fatal(err)
	}
	policy := application.ReferenceRetryPolicy{MaxAttempts: 3, Base: 30 * time.Millisecond, Maximum: 45 * time.Millisecond}
	queue := postgres.NewReferenceQueue(f.database)
	for attempt := 1; attempt <= 3; attempt++ {
		claim := claimReference(t, f, time.Second)
		if err := f.service.ResolveReference(f.ctx, claim, policy); err != nil {
			t.Fatal(err)
		}
		var attempts int
		var due time.Time
		var remaining time.Duration
		if err := f.database.Pool().QueryRow(f.ctx, `SELECT attempts,next_attempt_at FROM reference_retries WHERE transaction_id=$1`, pending.Transaction.Data.ID).Scan(&attempts, &due); err != nil {
			t.Fatal(err)
		}
		if attempts != attempt {
			t.Fatal("attempt not persisted")
		}
		if attempt < 3 {
			if due.Before(time.Now()) {
				t.Fatal("retry not scheduled in future")
			}
			claims, err := queue.Claim(f.ctx, 1, time.Second)
			if err != nil || len(claims) != 0 {
				t.Fatal("backoff ignored")
			}
			remaining = time.Until(due) + 5*time.Millisecond
			select {
			case <-time.After(remaining):
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
		}
	}
	state := requireStatus(t, f, "pending", domain.Rejected)
	if state.FailureCode != domain.FailureReferenceNotFound {
		t.Fatal("wrong exhaustion failure")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, state.Data.ID) != 0 || count(t, f, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, state.Data.ID) != 2 {
		t.Fatal("retry duplicated events/money")
	}
}

func TestReferenceWorkersLeaseRecoveryAndConcurrentReplay(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "lease-reference", "100.00")
	cmd := refundCommand(t, w, "80.00", "pending", "bet")
	pending, err := f.service.Process(f.ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	old := claimReference(t, f, 30*time.Millisecond) // Simulate a dead process after claim.
	processed(t, f, command(t, w, domain.Bet, "80.00", "bet"))
	policy := application.ReferenceRetryPolicy{MaxAttempts: 3, Base: time.Millisecond, Maximum: time.Second}
	select {
	case <-time.After(50 * time.Millisecond):
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	if err := f.service.ResolveReference(f.ctx, old, policy); !errors.Is(err, postgres.ErrReferenceClaimLost) {
		t.Fatalf("expired claim accepted: %v", err)
	}
	nodes := []instance{startInstance(t, f.url), startInstance(t, f.url)}
	start := make(chan struct{})
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(node instance) {
			defer wg.Done()
			<-start
			_, err := referenceWorker(node, referenceConfig(f.url)).RunOnce(f.ctx)
			errs <- err
		}(node)
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; _, err := nodes[0].service.Process(f.ctx, cmd); errs <- err }()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state := requireStatus(t, f, "pending", domain.Processed)
	if state.Data.ID != pending.Transaction.Data.ID {
		t.Fatal("worker recreated transaction")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, state.Data.ID) != 1 {
		t.Fatal("workers credited twice")
	}
	if err := nodes[1].service.ResolveReference(f.ctx, old, policy); err != nil {
		t.Fatal("terminal stale call should be harmless")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
}

func TestReferenceWorkerFinancialRejections(t *testing.T) {
	for _, scenario := range []string{"funds", "invalid", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "pending-rejection", "100.00")
			cmd := rollbackCommand(t, w, "50.00", "pending", "late")
			if _, err := f.service.Process(f.ctx, cmd); err != nil {
				t.Fatal(err)
			}
			if scenario == "invalid" {
				processed(t, f, command(t, w, domain.Loss, "0.00", "late"))
			} else {
				processed(t, f, command(t, w, domain.Win, "50.00", "late"))
			}
			if scenario == "funds" {
				processed(t, f, command(t, w, domain.Bet, "125.00", "spend"))
			}
			if scenario == "duplicate" {
				processed(t, f, rollbackCommand(t, w, "50.00", "other", "late"))
			}
			before, _ := f.store.GetWallet(f.ctx, w.Snapshot().ID)
			if _, err := referenceWorker(f.instance, referenceConfig(f.url)).RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			state := requireStatus(t, f, "pending", domain.Rejected)
			want := domain.FailureInvalidReference
			if scenario == "funds" {
				want = domain.FailureReversalInsufficientBalance
			}
			if scenario == "duplicate" {
				want = domain.FailureReversalConflict
			}
			if state.FailureCode != want || state.ReferenceTransactionID == "" {
				t.Fatal("wrong resolved rejection")
			}
			assertBalance(t, f, w.Snapshot().ID, before.Snapshot().Balance.MinorUnits(), before.Snapshot().Version)
			if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, state.Data.ID) != 0 {
				t.Fatal("rejection moved money")
			}
		})
	}
}

func TestReferenceSameClaimExecutedConcurrently(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "same-claim-reference", "100.00")
	pending, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "pending", "bet"))
	if err != nil {
		t.Fatal(err)
	}
	processed(t, f, command(t, w, domain.Bet, "25.00", "bet"))
	claim := claimReference(t, f, 5*time.Second)
	nodes := []instance{startInstance(t, f.url), startInstance(t, f.url)}
	policy := application.ReferenceRetryPolicy{MaxAttempts: 3, Base: time.Millisecond, Maximum: time.Second}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(node instance) {
			defer wg.Done()
			<-start
			errs <- node.service.ResolveReference(f.ctx, claim, policy)
		}(node)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, pending.Transaction.Data.ID) != 1 {
		t.Fatal("same claim executed financially twice")
	}
	if count(t, f, `SELECT attempts FROM reference_retries WHERE transaction_id=$1`, pending.Transaction.Data.ID) != 1 {
		t.Fatal("terminal replay consumed another attempt")
	}
}

func TestReferenceLeaseExpiresWhileWaitingForRetryLock(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "waiting-lease-reference", "100.00")
	pending, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "pending", "bet"))
	if err != nil {
		t.Fatal(err)
	}
	processed(t, f, command(t, w, domain.Bet, "25.00", "bet"))
	claim := claimReference(t, f, 500*time.Millisecond)
	locker, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(f.ctx, `SELECT transaction_id FROM reference_retries WHERE transaction_id=$1 FOR UPDATE`, pending.Transaction.Data.ID); err != nil {
		t.Fatal(err)
	}
	node := startInstance(t, f.url)
	errCh := make(chan error, 1)
	go func() {
		errCh <- node.service.ResolveReference(f.ctx, claim, application.ReferenceRetryPolicy{MaxAttempts: 3, Base: time.Millisecond, Maximum: time.Second})
	}()
	eventually(t, f.ctx, func() bool {
		return count(t, f, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%SELECT attempts FROM reference_retries%'`) > 0
	})
	select {
	case <-time.After(550 * time.Millisecond):
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	if err := locker.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; !errors.Is(err, postgres.ErrReferenceClaimLost) {
		t.Fatalf("lease expired during lock wait was accepted: %v", err)
	}
	requireStatus(t, f, "pending", domain.PendingReference)
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if _, err := referenceWorker(node, referenceConfig(f.url)).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
}

func TestReferenceCrashBeforeAndAfterCommit(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "crash-reference", "100.00")
	pending, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "pending", "late"))
	if err != nil {
		t.Fatal(err)
	}
	processed(t, f, command(t, w, domain.Bet, "25.00", "late"))
	claim := claimReference(t, f, 100*time.Millisecond)
	_, err = f.database.Pool().Exec(f.ctx, `CREATE FUNCTION fail_resolution_event() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.event_type='WalletBalanceChanged' THEN RAISE EXCEPTION 'crash before commit' USING ERRCODE='40001'; END IF; RETURN NEW; END; $$;
	CREATE TRIGGER fail_resolution BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_resolution_event();`)
	if err != nil {
		t.Fatal(err)
	}
	policy := application.ReferenceRetryPolicy{MaxAttempts: 3, Base: time.Millisecond, Maximum: time.Second}
	if err := f.service.ResolveReference(f.ctx, claim, policy); err == nil {
		t.Fatal("fault injection did not fail")
	}
	requireStatus(t, f, "pending", domain.PendingReference)
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM reference_retries WHERE transaction_id=$1 AND attempts=0 AND completed_at IS NULL`, pending.Transaction.Data.ID) != 1 || count(t, f, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, pending.Transaction.Data.ID) != 1 {
		t.Fatal("partial resolution survived")
	}
	if _, err := f.database.Pool().Exec(f.ctx, `DROP TRIGGER fail_resolution ON outbox_events; DROP FUNCTION fail_resolution_event();`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(120 * time.Millisecond):
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	node := startInstance(t, f.url)
	if _, err := referenceWorker(node, referenceConfig(f.url)).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f, "pending", domain.Processed)
	// Simulated death after commit: a fresh pool receives the old work again.
	second := startInstance(t, f.url)
	if err := second.service.ResolveReference(f.ctx, claim, policy); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, pending.Transaction.Data.ID) != 1 {
		t.Fatal("post-commit recovery duplicated money")
	}
}

func startReferenceGraph(t *testing.T, connection string) (*fx.App, instance) {
	t.Helper()
	var node instance
	c := referenceConfig(connection)
	graph := fx.New(fx.NopLogger, fx.Supply(c, outboxLogger()), postgres.Module,
		fx.Provide(postgres.NewStore, application.NewFinancialService), workers.ReferenceModule,
		fx.Populate(&node.service, &node.store, &node.database))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := graph.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := graph.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return graph, node
}

func TestReferenceFxRestartRecovery(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "restart-reference", "100.00")
	pending, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "pending", "late"))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := startReferenceGraph(t, f.url)
	eventually(t, f.ctx, func() bool {
		return count(t, f, `SELECT attempts FROM reference_retries WHERE transaction_id=$1`, pending.Transaction.Data.ID) > 0
	})
	stopCtx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	if err := first.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	processed(t, f, command(t, w, domain.Bet, "25.00", "late"))
	_, second := startReferenceGraph(t, f.url)
	eventually(t, f.ctx, func() bool {
		tx, err := second.store.GetExternalTransaction(f.ctx, "provider-a", "pending")
		return err == nil && tx.Snapshot().Status == domain.Processed
	})
	state := requireStatus(t, f, "pending", domain.Processed)
	if state.Data.ID != pending.Transaction.Data.ID {
		t.Fatal("restart created another transaction")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
}

func TestReferenceMigrationBackfillDownUp(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "migration-reference", "100.00")
	pending, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "pending", "late"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/0005_reference_retry.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(f.ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `DELETE FROM schema_migrations WHERE version=5`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(f.ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
	if count(t, f, `SELECT count(*) FROM reference_retries WHERE transaction_id=$1 AND attempts=0 AND claimed_by IS NULL`, pending.Transaction.Data.ID) != 1 {
		t.Fatal("legacy pending not scheduled")
	}
	processed(t, f, command(t, w, domain.Bet, "25.00", "late"))
	if _, err := referenceWorker(f.instance, referenceConfig(f.url)).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	if err := migrations.Apply(f.ctx, f.database.Pool(), "down"); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(f.ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackSQSAndHistoricalInboxAfterResolution(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "processed", true: "pending"}[pending], func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "sqs-rollback", "100.00")
			if !pending {
				processed(t, f, command(t, w, domain.Win, "25.00", "win"))
			}
			cmd := rollbackCommand(t, w, "25.00", "rb", "win")
			body := operationBody(t, cmd, "original-message")
			sendOperation(t, f, q, body, "wallet", "delivery-1")
			consumer := newConsumer(f, q)
			if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
				t.Fatal(err)
			}
			assertQueueDeleted(t, f, q)
			var inboxBefore string
			if err := f.database.Pool().QueryRow(f.ctx, `SELECT to_jsonb(i)::text FROM inbox_messages i`).Scan(&inboxBefore); err != nil {
				t.Fatal(err)
			}
			if pending {
				requireStatus(t, f, "rb", domain.PendingReference)
				processed(t, f, command(t, w, domain.Win, "25.00", "win"))
				if _, err := referenceWorker(f.instance, referenceConfig(f.url)).RunOnce(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			state := requireStatus(t, f, "rb", domain.Processed)
			assertBalance(t, f, w.Snapshot().ID, 10000, 3)
			var inboxAfter string
			if err := f.database.Pool().QueryRow(f.ctx, `SELECT to_jsonb(i)::text FROM inbox_messages i`).Scan(&inboxAfter); err != nil {
				t.Fatal(err)
			}
			if inboxBefore != inboxAfter {
				t.Fatal("worker changed historical inbox")
			}
			// Re-send the exact original envelope; SQS dedup ID differs, inbox ID stays.
			sendOperation(t, f, q, body, "wallet", "delivery-2")
			if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
				t.Fatal(err)
			}
			assertQueueDeleted(t, f, q)
			replay, err := f.service.Process(f.ctx, cmd)
			if err != nil || !replay.IdempotentReplay {
				t.Fatal("cross-transport replay failed")
			}
			if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, state.Data.ID) != 1 {
				t.Fatal("SQS replay duplicated rollback")
			}
			events := newEventQueue(t, f)
			outbox := workers.NewOutboxWorker(postgres.NewOutboxDelivery(f.database), events.publisher, events.config, outboxLogger())
			drainOutbox(t, f, outbox)
			messages, err := events.client.ReceiveMessage(f.ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(events.url), MaxNumberOfMessages: 10, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameMessageGroupId}})
			if err != nil || len(messages.Messages) == 0 {
				t.Fatalf("rollback outbox not publishable: %v", err)
			}
		})
	}
}
