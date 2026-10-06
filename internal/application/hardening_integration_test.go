//go:build integration

package application_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgconn"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/postgres"
	httptransport "jungle-gaming/internal/transport/http"
	sqstransport "jungle-gaming/internal/transport/sqs"
	"jungle-gaming/internal/workers"
	"jungle-gaming/migrations"
)

func TestReferencedWinSQSCommitsPendingAndRecovers(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	_, identity := testAuthenticator(t, authConfig(q.config))
	w := open(t, f, "sqs-early-win", "100.00")
	cmd := command(t, w, domain.Win, "10.00", "early-win")
	cmd.ReferenceExternalTransactionID = "late-bet"
	body := operationBody(t, cmd, "early-win-envelope")
	consumer := workers.NewOperationConsumer(q.queue, application.NewAuthorizedIncomingService(f.service, identity), q.config, outboxLogger())
	sendOperation(t, f, q, body, "wallet", "first-delivery")
	if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
		t.Fatal(err)
	}
	assertQueueDeleted(t, f, q)
	pending := requireStatus(t, f, "early-win", domain.PendingReference)
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	var historical string
	if err := f.database.Pool().QueryRow(f.ctx, `SELECT to_jsonb(i)::text FROM inbox_messages i`).Scan(&historical); err != nil {
		t.Fatal(err)
	}
	processed(t, f, command(t, w, domain.Bet, "25.00", "late-bet"))
	restarted := startInstance(t, f.url)
	if n, err := referenceWorker(restarted, referenceConfig(f.url)).RunOnce(f.ctx); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	resolved := requireStatus(t, f, "early-win", domain.Processed)
	if resolved.Data.ID != pending.Data.ID || resolved.ReferenceTransactionID == "" {
		t.Fatal("pending transaction identity lost")
	}
	var after string
	if err := f.database.Pool().QueryRow(f.ctx, `SELECT to_jsonb(i)::text FROM inbox_messages i`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if historical != after {
		t.Fatal("reference resolution changed historical inbox")
	}
	sendOperation(t, f, q, body, "wallet", "second-delivery")
	if err := consumer.Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
		t.Fatal(err)
	}
	assertQueueDeleted(t, f, q)
	assertBalance(t, f, w.Snapshot().ID, 8500, 3)
	if count(t, f, `SELECT count(*) FROM inbox_messages`) != 1 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, resolved.Data.ID) != 1 || count(t, f, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, resolved.Data.ID) != 2 {
		t.Fatal("replay duplicated effects")
	}
}

func TestConcurrentAuthenticatedHTTPAndSQSUseOneFinancialCommit(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	c := authConfig(q.config)
	authenticator, identity := testAuthenticator(t, c)
	w := open(t, f, "mixed-concurrent", "100.00")
	cmd := command(t, w, domain.Bet, "25.00", "shared-bet")
	cmd.ProviderID = "provider-alpha"
	nodes := []instance{f.instance, startInstance(t, f.url), startInstance(t, f.url)}
	server := httptest.NewServer(httptransport.NewAuthenticatedMux(httptransport.NewHealth(c, f.database, q.queue, outboxLogger()), httptransport.NewOperationHandler(application.NewAuthorizedFinancialService(nodes[0].service)), authenticator))
	defer server.Close()
	token := realToken(t, c, "provider-alpha")
	var messages []types.Message
	for index := 0; index < 2; index++ {
		id := fmt.Sprintf("parallel-envelope-%d", index)
		// Distinct groups allow two actual in-flight deliveries of one operation.
		sendOperation(t, f, q, operationBody(t, cmd, id), id, id)
		messages = append(messages, receiveOperation(t, f, q))
	}
	start := make(chan struct{})
	errorsFound := make(chan error, 52)
	var group sync.WaitGroup
	body := authenticatedBody(t, cmd)
	for index := 0; index < 50; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			req, err := http.NewRequestWithContext(f.ctx, "POST", server.URL+"/wagering/transactions", strings.NewReader(body))
			if err != nil {
				errorsFound <- err
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", cmd.IdempotencyKey)
			response, err := server.Client().Do(req)
			if err != nil {
				errorsFound <- err
				return
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				errorsFound <- fmt.Errorf("parallel HTTP status %d", response.StatusCode)
			}
		}()
	}
	for index, message := range messages {
		consumer := workers.NewOperationConsumer(q.queue, application.NewAuthorizedIncomingService(nodes[index+1].service, identity), c, outboxLogger())
		group.Add(1)
		go func(message types.Message) {
			defer group.Done()
			<-start
			errorsFound <- consumer.Handle(f.ctx, message)
		}(message)
	}
	close(start)
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM inbox_messages`) != 2 || count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`) != 1 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE direction='DEBIT'`) != 1 || count(t, f, `SELECT count(*) FROM outbox_events`) != 4 {
		t.Fatal("concurrent HTTP/SQS duplicated effects")
	}
	assertQueueDeleted(t, f, q)
}

type heldDeliveryQueue struct {
	*messaging.Queue
	message types.Message
}

func (q *heldDeliveryQueue) Receive(ctx context.Context) ([]types.Message, error) {
	if q.message.ReceiptHandle != nil {
		message := q.message
		q.message = types.Message{}
		return []types.Message{message}, nil
	}
	return q.Queue.Receive(ctx)
}

func TestConsumerShutdownReleasesRealSQSVisibility(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	_, identity := testAuthenticator(t, authConfig(q.config))
	w := open(t, f, "shutdown-release", "100.00")
	cmd := command(t, w, domain.Bet, "25.00", "interrupted")
	sendOperation(t, f, q, operationBody(t, cmd, "shutdown-envelope"), "wallet", "shutdown-delivery")
	message := receiveOperation(t, f, q)
	if _, err := q.client.ChangeMessageVisibility(f.ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(q.url), ReceiptHandle: message.ReceiptHandle, VisibilityTimeout: 30}); err != nil {
		t.Fatal(err)
	}
	locker, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(f.ctx)
	if _, err := locker.Exec(f.ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, w.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	c := q.config
	c.ConsumerConcurrency = 1
	consumer := workers.NewOperationConsumer(&heldDeliveryQueue{Queue: q.queue, message: message}, application.NewAuthorizedIncomingService(f.service, identity), c, outboxLogger())
	if err := consumer.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = consumer.Stop(ctx)
	})
	eventually(t, f.ctx, func() bool {
		return count(t, f, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%wallets%FOR UPDATE%'`) > 0
	})
	stop, cancel := context.WithCancel(f.ctx)
	cancel()
	if !errors.Is(consumer.Stop(stop), context.Canceled) {
		t.Fatal("shutdown did not cancel blocked work")
	}
	if err := locker.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, `SELECT count(*) FROM inbox_messages`) != 0 {
		t.Fatal("canceled work committed inbox")
	}
	// No sleep: the original 30-second visibility must have been released.
	redelivery := receiveOperation(t, f, q)
	if aws.ToString(redelivery.MessageId) != aws.ToString(message.MessageId) || redelivery.Attributes["ApproximateReceiveCount"] != "2" {
		t.Fatal("shutdown did not release the actual delivery")
	}
	other := startInstance(t, f.url)
	if err := workers.NewOperationConsumer(q.queue, application.NewAuthorizedIncomingService(other.service, identity), c, outboxLogger()).Handle(f.ctx, redelivery); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	assertQueueDeleted(t, f, q)
}

func TestAuditMigrationPreservesHistoryAndRejectsLegacyDuplicateInbox(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate-%t", duplicate), func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "migration-history", "100.00")
			before := snapshots(t, f)
			down, err := os.ReadFile("../../migrations/0006_audit_hardening.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.database.Pool().Exec(f.ctx, string(down)+`DELETE FROM schema_migrations WHERE version=6;`); err != nil {
				t.Fatal(err)
			}
			if duplicate {
				if _, err := f.database.Pool().Exec(f.ctx, `INSERT INTO inbox_messages(consumer_name,source,message_id,payload_hash,correlation_id,status,failure_code,received_at,processed_at) VALUES ('consumer','a','legacy',repeat('a',64),'test','REJECTED','INVALID_INPUT',now(),now()),('consumer','b','legacy',repeat('b',64),'test','REJECTED','INVALID_INPUT',now(),now())`); err != nil {
					t.Fatal(err)
				}
			}
			err = migrations.Apply(f.ctx, f.database.Pool(), "up")
			if duplicate {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
					t.Fatalf("legacy ambiguity was silently accepted: %v", err)
				}
				if count(t, f, `SELECT count(*) FROM inbox_messages`) != 2 || count(t, f, `SELECT count(*) FROM schema_migrations WHERE version=6`) != 0 {
					t.Fatal("failed upgrade altered history/version")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			assertBalance(t, f, w.Snapshot().ID, 10000, 1)
			after := snapshots(t, f)
			if len(after) != len(before) {
				t.Fatal("upgrade lost events")
			}
			for id, payload := range after {
				if string(payload) != string(before[id]) {
					t.Fatal("upgrade changed event snapshot")
				}
			}
		})
	}
}

func TestInboxMessageIdentityCannotChangeSource(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "source-identity", "100.00")
	first, err := sqstransport.ParseOperation(operationBody(t, command(t, w, domain.Bet, "25.00", "first"), "same-message"), "consumer", "source-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ProcessIncoming(f.ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := sqstransport.ParseOperation(operationBody(t, command(t, w, domain.Bet, "10.00", "second"), "same-message"), "consumer", "source-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startInstance(t, f.url).service.ProcessIncoming(f.ctx, second); !errors.Is(err, postgres.ErrInboxIdentity) {
		t.Fatalf("source changed durable message identity: %v", err)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM inbox_messages`) != 1 || count(t, f, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id='second'`) != 0 || count(t, f, `SELECT count(*) FROM outbox_events`) != 4 {
		t.Fatal("identity conflict left financial effects")
	}
	_, err = f.database.Pool().Exec(f.ctx, `INSERT INTO inbox_messages(consumer_name,source,message_id,payload_hash,correlation_id,status,failure_code,received_at,processed_at) VALUES ('consumer','source-c','same-message',repeat('a',64),'test','REJECTED','INVALID_INPUT',now(),now())`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "inbox_consumer_message_unique" {
		t.Fatalf("database accepted repeated consumer/message: %v", err)
	}
}

func TestDatabaseRejectsInvalidReferencedWin(t *testing.T) {
	for _, scenario := range []string{"provider", "round", "wallet", "kind", "status", "external"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "win-context", "100.00")
			originalCmd := command(t, w, domain.Bet, "25.00", "original")
			switch scenario {
			case "provider":
				originalCmd.ProviderID = "provider-beta"
			case "round":
				originalCmd.RoundID = "another-round"
			case "wallet":
				originalCmd = command(t, open(t, f, "other-wallet", "100.00"), domain.Bet, "25.00", "original")
			case "kind":
				originalCmd.Kind = domain.Win
			case "status":
				originalCmd.Money = money(t, "200.00")
			}
			original, err := f.service.Process(f.ctx, originalCmd)
			if err != nil {
				t.Fatal(err)
			}
			cmd := command(t, w, domain.Win, "10.00", "bypass-win")
			cmd.ReferenceExternalTransactionID = "original"
			if scenario == "external" {
				cmd.ReferenceExternalTransactionID = "different-external"
			}
			before, err := f.store.GetWallet(f.ctx, w.Snapshot().ID)
			if err != nil {
				t.Fatal(err)
			}
			// Bypass the application reference validator while preserving all other
			// wallet/ledger invariants, so only the reference protection is tested.
			err = f.store.WithTx(f.ctx, func(r *postgres.Repositories) error {
				wallet, err := r.Wallets.Lock(f.ctx, w.Snapshot().ID)
				if err != nil {
					return err
				}
				previous := wallet.Snapshot()
				at := time.Now().UTC().Truncate(time.Microsecond)
				hash, err := application.PayloadHash(cmd)
				if err != nil {
					return err
				}
				tx, err := domain.NewWagerTransaction(domain.TransactionData{ID: "bypass-win", ExternalTransactionID: cmd.ExternalTransactionID, ProviderID: cmd.ProviderID, IdempotencyKey: cmd.IdempotencyKey, PayloadHash: hash, WalletID: cmd.WalletID, PlayerID: cmd.PlayerID, RoundID: cmd.RoundID, GameID: cmd.GameID, Kind: cmd.Kind, Money: cmd.Money, ReferenceExternalTransactionID: cmd.ReferenceExternalTransactionID, CorrelationID: cmd.CorrelationID}, at)
				if err != nil {
					return err
				}
				if inserted, err := r.Transactions.Insert(f.ctx, tx); err != nil || !inserted {
					return errors.New("cannot insert bypass transaction")
				}
				if err := wallet.Credit(cmd.Money, at); err != nil {
					return err
				}
				next := wallet.Snapshot()
				if err := tx.MarkProcessed(domain.FinancialResult{Balance: next.Balance, WalletVersion: next.Version}, original.Transaction.Data.ID, at); err != nil {
					return err
				}
				if err := r.Transactions.Complete(f.ctx, tx); err != nil {
					return err
				}
				if err := r.Wallets.Update(f.ctx, wallet, previous.Version); err != nil {
					return err
				}
				entry, err := domain.NewWalletLedgerEntry(domain.LedgerEntryState{ID: "bypass-ledger", WalletID: cmd.WalletID, TransactionID: "bypass-win", Direction: domain.CreditDirection, Money: cmd.Money, BalanceBefore: previous.Balance, BalanceAfter: next.Balance, CreatedAt: at})
				if err != nil {
					return err
				}
				return r.Ledger.Insert(f.ctx, entry)
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("database accepted invalid WIN reference: %v", err)
			}
			assertBalance(t, f, w.Snapshot().ID, before.Snapshot().Balance.MinorUnits(), before.Snapshot().Version)
			if count(t, f, `SELECT count(*) FROM wager_transactions WHERE id='bypass-win'`) != 0 {
				t.Fatal("invalid reference committed")
			}
		})
	}
}
