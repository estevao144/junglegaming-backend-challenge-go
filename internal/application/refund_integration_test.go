//go:build integration

package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgconn"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/internal/workers"
	"jungle-gaming/migrations"
)

func refundCommand(t *testing.T, w *domain.Wallet, amount, external, reference string) application.ProcessCommand {
	t.Helper()
	cmd := command(t, w, domain.Refund, amount, external)
	cmd.ReferenceExternalTransactionID = reference
	return cmd
}

func TestRefundNormalAndOriginalReplay(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "refund-player", "100.00")
	bet, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "original-bet"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := refundCommand(t, w, "25.00", "refund", "original-bet")
	refund, err := f.service.Process(f.ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if refund.Transaction.Status != domain.Processed || refund.Transaction.ReferenceTransactionID != bet.Transaction.Data.ID {
		t.Fatal("refund reference not resolved")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	if count(t, f, "SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1 AND direction='CREDIT' AND amount_minor_units=2500 AND balance_before_minor_units=7500 AND balance_after_minor_units=10000", refund.Transaction.Data.ID) != 1 {
		t.Fatal("refund ledger differs")
	}
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId'=$1", refund.Transaction.Data.ID) != 2 {
		t.Fatal("refund outbox missing")
	}
	var payload []byte
	if err := f.database.Pool().QueryRow(f.ctx, "SELECT payload FROM outbox_events WHERE event_type='WalletBalanceChanged' AND payload->'data'->>'transactionId'=$1", refund.Transaction.Data.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	// Decode only nonmonetary routing via concrete string fields below; Money
	// has no permissive financial unmarshalling API.
	var snapshot struct {
		Data struct {
			Direction     string `json:"direction"`
			WalletVersion int64  `json:"walletVersion"`
			Money         struct {
				Amount string `json:"amount"`
			} `json:"money"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Data.Direction != "CREDIT" || snapshot.Data.WalletVersion != 3 || snapshot.Data.Money.Amount != "25.00" {
		t.Fatal("incorrect refund event snapshot")
	}
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Win, "10.00", "later-win")); err != nil {
		t.Fatal(err)
	}
	node := startInstance(t, f.url)
	replay, err := node.service.Process(f.ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.IdempotentReplay || replay.Transaction.Data.ID != refund.Transaction.Data.ID || replay.Transaction.Result.Balance.MinorUnits() != 10000 || replay.Transaction.Result.WalletVersion != 3 {
		t.Fatal("replay returned current balance")
	}
	assertBalance(t, f, w.Snapshot().ID, 11000, 4)
}

func TestRefundPendingDurableReplayAndNoAutomaticResolution(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "pending-refund-player", "100.00")
	cmd := refundCommand(t, w, "25.00", "early-refund", "late-bet")
	cmd.CausationID = "source-message"
	first, err := f.service.Process(f.ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if first.Transaction.Status != domain.PendingReference || first.Transaction.ReferenceTransactionID != "" || first.Transaction.Result != nil {
		t.Fatal("missing reference should wait without result")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != 1 || count(t, f, "SELECT count(*) FROM outbox_events WHERE event_type='WagerTransactionPendingReference'") != 1 {
		t.Fatal("pending changed financial state")
	}
	node := startInstance(t, f.url)
	replay, err := node.service.Process(f.ctx, cmd)
	if err != nil || !replay.IdempotentReplay || replay.Transaction.Data.ID != first.Transaction.Data.ID {
		t.Fatalf("pending replay failed: %v", err)
	}
	if replay.Transaction.Data.ReferenceExternalTransactionID != "late-bet" || replay.Transaction.Data.CorrelationID != cmd.CorrelationID || replay.Transaction.Data.CausationID != "source-message" {
		t.Fatal("future resolution metadata not persisted")
	}
	changed := cmd
	changed.Money = money(t, "26.00")
	if _, err := node.service.Process(f.ctx, changed); !errors.Is(err, postgres.ErrConflict) {
		t.Fatal("pending identity accepted different payload")
	}
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "late-bet")); err != nil {
		t.Fatal(err)
	}
	replay, err = node.service.Process(f.ctx, cmd)
	if err != nil || replay.Transaction.Status != domain.PendingReference {
		t.Fatal("5A must not resolve pending on replay")
	}
	if count(t, f, "SELECT count(*) FROM outbox_events WHERE event_type='WagerTransactionPendingReference'") != 1 {
		t.Fatal("pending replay duplicated event")
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
}

func TestRefundInvalidExistingReferences(t *testing.T) {
	for _, scenario := range []string{"WIN", "LOSS", "REFUND", "amount", "round", "wallet", "not processed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "invalid-reference-player", "100.00")
			kind, amount := domain.Bet, "25.00"
			if scenario == "WIN" {
				kind = domain.Win
			}
			if scenario == "LOSS" {
				kind = domain.Loss
				amount = "0.00"
			}
			original, err := f.service.Process(f.ctx, command(t, w, kind, amount, "original"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "REFUND" {
				original, err = f.service.Process(f.ctx, refundCommand(t, w, "25.00", "first-refund", "original"))
				if err != nil {
					t.Fatal(err)
				}
			}
			refundWallet := w
			if scenario == "wallet" {
				refundWallet = open(t, f, "other-reference-player", "100.00")
			}
			cmd := refundCommand(t, refundWallet, "25.00", "invalid-refund", original.Transaction.Data.ExternalTransactionID)
			if scenario == "amount" {
				cmd.Money = money(t, "24.99")
			}
			if scenario == "round" {
				cmd.RoundID = "other-round"
			}
			want := domain.FailureInvalidReference
			if scenario == "not processed" {
				// A rejected BET is an existing reference, not a missing reference.
				rejected, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "200.00", "rejected-bet"))
				if err != nil {
					t.Fatal(err)
				}
				cmd.Money = money(t, "200.00")
				cmd.ReferenceExternalTransactionID = "rejected-bet"
				original = rejected
				want = domain.FailureReferenceNotProcessed
			}
			before, err := f.store.GetWallet(f.ctx, refundWallet.Snapshot().ID)
			if err != nil {
				t.Fatal(err)
			}
			ledgerBefore := count(t, f, "SELECT count(*) FROM wallet_ledger_entries")
			refund, err := f.service.Process(f.ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			if refund.Transaction.Status != domain.Rejected || refund.Transaction.FailureCode != want || refund.Transaction.ReferenceTransactionID != original.Transaction.Data.ID {
				t.Fatal("invalid existing reference was not terminally audited")
			}
			assertBalance(t, f, refundWallet.Snapshot().ID, before.Snapshot().Balance.MinorUnits(), before.Snapshot().Version)
			if count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != ledgerBefore || count(t, f, "SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='WagerTransactionRejected'", refund.Transaction.Data.ID) != 1 {
				t.Fatal("rejected refund caused movement or lost rejection event")
			}
		})
	}
}

func TestRefundProviderScopedLookup(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "provider-reference-player", "100.00")
	cmd := command(t, w, domain.Bet, "25.00", "same-external")
	if _, err := f.service.Process(f.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	other := refundCommand(t, w, "25.00", "other-provider-refund", "same-external")
	other.ProviderID = "provider-b"
	pending, err := f.service.Process(f.ctx, other)
	if err != nil || pending.Transaction.Status != domain.PendingReference {
		t.Fatalf("reference leaked across providers: %v", err)
	}
	cmd.ProviderID = "provider-b"
	if _, err := f.service.Process(f.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	other.ExternalTransactionID = "provider-b-refund"
	other.IdempotencyKey = "provider-b-key"
	refund, err := f.service.Process(f.ctx, other)
	if err != nil || refund.Transaction.Status != domain.Processed {
		t.Fatalf("provider-scoped valid BET not resolved: %v", err)
	}
}

func TestRefundConcurrentIndependentPools(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-refund-%t", same), func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "concurrent-refund-player", "100.00")
			if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "80.00", "bet-80")); err != nil {
				t.Fatal(err)
			}
			nodes := []instance{f.instance, startInstance(t, f.url), startInstance(t, f.url)}
			total := 2
			if same {
				total = 50
			}
			commands := make([]application.ProcessCommand, total)
			for i := range commands {
				external := fmt.Sprintf("refund-%d", i)
				if same {
					external = "same-refund"
				}
				commands[i] = refundCommand(t, w, "80.00", external, "bet-80")
			}
			start := make(chan struct{})
			results := make(chan application.ProcessResult, total)
			failures := make(chan error, total)
			var wg sync.WaitGroup
			for i, cmd := range commands {
				wg.Add(1)
				go func(node instance, input application.ProcessCommand) {
					defer wg.Done()
					<-start
					result, err := node.service.Process(f.ctx, input)
					if err != nil {
						failures <- err
						return
					}
					results <- result
				}(nodes[i%len(nodes)], cmd)
			}
			close(start)
			wg.Wait()
			close(results)
			close(failures)
			for err := range failures {
				t.Fatal(err)
			}
			processed, rejected, replays := 0, 0, 0
			for result := range results {
				if result.IdempotentReplay {
					replays++
					continue
				}
				switch result.Transaction.Status {
				case domain.Processed:
					processed++
				case domain.Rejected:
					rejected++
					if result.Transaction.FailureCode != domain.FailureReversalConflict {
						t.Fatal("wrong duplicate code")
					}
				default:
					t.Fatal("unexpected refund state")
				}
			}
			if processed != 1 || (!same && rejected != 1) || (same && replays != 49) {
				t.Fatalf("processed=%d rejected=%d replays=%d", processed, rejected, replays)
			}
			assertBalance(t, f, w.Snapshot().ID, 10000, 3)
			if count(t, f, "SELECT count(*) FROM wallet_ledger_entries l JOIN wager_transactions t ON t.id=l.transaction_id WHERE t.kind='REFUND' AND l.direction='CREDIT'") != 1 || count(t, f, "SELECT count(*) FROM outbox_events WHERE event_type='WalletBalanceChanged'") != 3 {
				t.Fatal("duplicate refund effects")
			}
		})
	}
}

func TestRefundDatabaseProtection(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "database-refund-player", "100.00")
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "db-bet")); err != nil {
		t.Fatal(err)
	}
	refund, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "db-refund", "db-bet"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.database.Pool().Exec(f.ctx, `INSERT INTO wager_transactions
		(id,external_transaction_id,provider_id,idempotency_key,payload_hash,wallet_id,player_id,round_id,game_id,kind,
		amount_minor_units,currency,reference_external_transaction_id,reference_transaction_id,status,
		result_balance_minor_units,result_wallet_version,created_at,updated_at)
		SELECT 'bypass-refund','bypass-external',provider_id,'bypass-key',payload_hash,wallet_id,player_id,round_id,game_id,kind,
		amount_minor_units,currency,reference_external_transaction_id,reference_transaction_id,status,
		result_balance_minor_units+amount_minor_units,result_wallet_version+1,created_at,updated_at
		FROM wager_transactions WHERE id=$1`, refund.Transaction.Data.ID)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23505" || pgError.ConstraintName != "one_processed_refund_per_reference" {
		t.Fatalf("duplicate refund not prevented by dedicated DB index: %v", err)
	}
}

func TestRefundViaSQSAndPendingAck(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending-%t", pending), func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "sqs-refund-player", "100.00")
			if !pending {
				if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "sqs-bet")); err != nil {
					t.Fatal(err)
				}
			}
			cmd := refundCommand(t, w, "25.00", "sqs-refund", "sqs-bet")
			sendOperation(t, f, q, operationBody(t, cmd, "refund-envelope"), w.Snapshot().ID, "refund-delivery")
			first := receiveOperation(t, f, q)
			consumer := newConsumer(f, q)
			result, err := consumer.Resolve(f.ctx, first)
			if err != nil {
				t.Fatal(err)
			} // Commit without DeleteMessage: real crash window.
			status := domain.Processed
			if pending {
				status = domain.PendingReference
			}
			if result.Status != string(status) || count(t, f, "SELECT count(*) FROM inbox_messages WHERE status=$1 AND processed_at IS NOT NULL", string(status)) != 1 {
				t.Fatal("refund inbox not durably resolved")
			}
			<-time.After(1100 * time.Millisecond)
			redelivery := receiveOperation(t, f, q)
			if aws.ToString(first.MessageId) != aws.ToString(redelivery.MessageId) {
				t.Fatal("not a real redelivery")
			}
			if err := consumer.Handle(f.ctx, redelivery); err != nil {
				t.Fatal(err)
			}
			assertQueueDeleted(t, f, q)
			version := int64(3)
			if pending {
				version = 1
			}
			assertBalance(t, f, w.Snapshot().ID, 10000, version)
			ledger, events := 3, 6
			if pending {
				ledger = 1
				events = 3
			}
			if count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != ledger || count(t, f, "SELECT count(*) FROM outbox_events") != events || count(t, f, "SELECT count(*) FROM inbox_messages") != 1 {
				t.Fatal("refund redelivery duplicated effects")
			}
			// The existing publisher handles the pending event's wallet routing too.
			out := newEventQueue(t, f)
			expected := snapshots(t, f)
			worker := workers.NewOutboxWorker(postgres.NewOutboxDelivery(f.database), out.publisher, out.config, outboxLogger())
			drainOutbox(t, f, worker)
			assertReceivedSnapshots(t, f, out, expected)
		})
	}
}

func TestRefundCrossTransportReplay(t *testing.T) {
	for _, directFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct-first-%t", directFirst), func(t *testing.T) {
			f := newFixture(t)
			q := newOperationQueue(t, f)
			w := open(t, f, "cross-refund-player", "100.00")
			if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "cross-bet")); err != nil {
				t.Fatal(err)
			}
			cmd := refundCommand(t, w, "25.00", "cross-refund", "cross-bet")
			if directFirst {
				if _, err := f.service.Process(f.ctx, cmd); err != nil {
					t.Fatal(err)
				}
			}
			sendOperation(t, f, q, operationBody(t, cmd, "cross-refund-envelope"), w.Snapshot().ID, "cross-refund-delivery")
			if err := newConsumer(f, q).Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
				t.Fatal(err)
			}
			replay, err := f.service.Process(f.ctx, cmd)
			if err != nil || !replay.IdempotentReplay {
				t.Fatalf("cross-transport replay failed: %v", err)
			}
			assertBalance(t, f, w.Snapshot().ID, 10000, 3)
			if count(t, f, "SELECT count(*) FROM outbox_events") != 6 || count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != 3 {
				t.Fatal("cross-transport duplicate")
			}
		})
	}
}

func TestRefundMigrationDownUpWithPendingInbox(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	w := open(t, f, "migration-pending-player", "100.00")
	sendOperation(t, f, q, operationBody(t, refundCommand(t, w, "25.00", "pending-refund", "missing-bet"), "migration-message"), w.Snapshot().ID, "migration-delivery")
	if err := newConsumer(f, q).Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	if err := migrations.Apply(ctx, f.database.Pool(), "down"); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
}

func TestRefundRollbackAllEffectsWhenOutboxFails(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "refund-rollback-player", "100.00")
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "original")); err != nil {
		t.Fatal(err)
	}
	_, err := f.database.Pool().Exec(f.ctx, `CREATE FUNCTION fail_refund_outbox() RETURNS TRIGGER LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'controlled outbox failure' USING ERRCODE='40001'; END; $$;
		CREATE TRIGGER fail_refund_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_refund_outbox()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Process(f.ctx, refundCommand(t, w, "25.00", "failed-refund", "original")); err == nil {
		t.Fatal("outbox failure ignored")
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, "SELECT count(*) FROM wager_transactions") != 2 || count(t, f, "SELECT count(*) FROM wallet_ledger_entries") != 2 || count(t, f, "SELECT count(*) FROM outbox_events") != 4 {
		t.Fatal("partial refund survived rollback")
	}
}

func TestRefundSchemaAllowsFuturePendingResolution(t *testing.T) {
	f := newFixture(t)
	q := newOperationQueue(t, f)
	w := open(t, f, "future-pending-player", "100.00")
	cmd := refundCommand(t, w, "25.00", "future-refund", "future-bet")
	sendOperation(t, f, q, operationBody(t, cmd, "future-message"), w.Snapshot().ID, "future-delivery")
	if err := newConsumer(f, q).Handle(f.ctx, receiveOperation(t, f, q)); err != nil {
		t.Fatal(err)
	}
	bet, err := f.service.Process(f.ctx, command(t, w, domain.Bet, "25.00", "future-bet"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := f.store.GetExternalTransaction(f.ctx, cmd.ProviderID, cmd.ExternalTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	// Test-only explicit transition via repositories, not a runtime resolver.
	// Proves the existing inbox snapshot does not freeze the future financial state.
	err = f.store.WithTx(f.ctx, func(r *postgres.Repositories) error {
		wallet, err := r.Wallets.Lock(f.ctx, w.Snapshot().ID)
		if err != nil {
			return err
		}
		before := wallet.Snapshot()
		at := time.Now().UTC().Truncate(time.Microsecond)
		if err := wallet.Credit(cmd.Money, at); err != nil {
			return err
		}
		after := wallet.Snapshot()
		if err := pending.MarkProcessed(domain.FinancialResult{Balance: after.Balance, WalletVersion: after.Version}, bet.Transaction.Data.ID, at); err != nil {
			return err
		}
		if err := r.Transactions.Complete(f.ctx, pending); err != nil {
			return err
		}
		if err := r.Wallets.Update(f.ctx, wallet, before.Version); err != nil {
			return err
		}
		entry, err := domain.NewWalletLedgerEntry(domain.LedgerEntryState{ID: "future-ledger", WalletID: before.ID, TransactionID: pending.Snapshot().Data.ID,
			Direction: domain.CreditDirection, Money: cmd.Money, BalanceBefore: before.Balance, BalanceAfter: after.Balance, CreatedAt: at})
		if err != nil {
			return err
		}
		if err := r.Ledger.Insert(f.ctx, entry); err != nil {
			return err
		}
		data := pending.Snapshot().Data
		processed, err := domain.NewWagerTransactionProcessedEvent("future-processed", data.CorrelationID, data.CausationID, pending)
		if err != nil {
			return err
		}
		changed, err := domain.NewWalletBalanceChangedEvent("future-changed", data.CorrelationID, data.CausationID, entry, after.Version)
		if err != nil {
			return err
		}
		for _, event := range []struct {
			header  domain.EventHeader
			payload any
		}{{processed.EventHeader, processed}, {changed.EventHeader, changed}} {
			payload, err := json.Marshal(event.payload)
			if err != nil {
				return err
			}
			if err := r.Outbox.Insert(f.ctx, postgres.OutboxRecord{EventID: event.header.EventID, AggregateID: event.header.AggregateID, EventType: event.header.EventType, Payload: payload, OccurredAt: event.header.OccurredAt}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	if count(t, f, "SELECT count(*) FROM inbox_messages WHERE status='PENDING_REFERENCE' AND processed_at IS NOT NULL") != 1 {
		t.Fatal("delivery snapshot should remain immutable")
	}
	if count(t, f, "SELECT count(*) FROM wager_transactions WHERE kind='REFUND' AND status='PROCESSED'") != 1 {
		t.Fatal("future transition forbidden by schema")
	}
}
