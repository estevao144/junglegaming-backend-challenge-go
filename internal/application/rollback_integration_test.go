//go:build integration

package application_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/domain"
)

func rollbackCommand(t *testing.T, w *domain.Wallet, amount, external, reference string) application.ProcessCommand {
	c := command(t, w, domain.Rollback, amount, external)
	c.ReferenceExternalTransactionID = reference
	return c
}

func processed(t *testing.T, f fixture, c application.ProcessCommand) application.ProcessResult {
	t.Helper()
	result, err := f.service.Process(f.ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if result.Transaction.Status != domain.Processed {
		t.Fatalf("expected processed: %+v", result.Transaction)
	}
	return result
}

func TestRollbackTypesAndOriginalReplay(t *testing.T) {
	for _, kind := range []domain.TransactionKind{domain.Bet, domain.Win, domain.Refund} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "rollback-player", "100.00")
			var original application.ProcessResult
			if kind == domain.Refund {
				processed(t, f, command(t, w, domain.Bet, "25.00", "bet"))
				original = processed(t, f, refundCommand(t, w, "25.00", "original", "bet"))
			} else {
				original = processed(t, f, command(t, w, kind, "25.00", "original"))
			}
			cmd := rollbackCommand(t, w, "25.00", "rollback", "original")
			result := processed(t, f, cmd)
			wantBalance, wantVersion := int64(10000), int64(3)
			direction := "DEBIT"
			if kind == domain.Bet {
				direction = "CREDIT"
			}
			if kind == domain.Refund {
				wantBalance, wantVersion = 7500, 4
			}
			assertBalance(t, f, w.Snapshot().ID, wantBalance, wantVersion)
			if result.Transaction.ReferenceTransactionID != original.Transaction.Data.ID {
				t.Fatal("internal reference missing")
			}
			if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1 AND direction=$2 AND amount_minor_units=2500`, result.Transaction.Data.ID, direction) != 1 {
				t.Fatal("wrong rollback ledger")
			}
			if count(t, f, `SELECT count(*) FROM outbox_events WHERE payload->'data'->>'transactionId'=$1`, result.Transaction.Data.ID) != 2 {
				t.Fatal("rollback events missing")
			}
			if count(t, f, `SELECT count(*) FROM outbox_events WHERE event_type='WalletBalanceChanged' AND payload->'data'->>'transactionId'=$1 AND payload->'data'->>'direction'=$2 AND (payload->'data'->>'walletVersion')::bigint=$3`, result.Transaction.Data.ID, direction, wantVersion) != 1 {
				t.Fatal("wrong event snapshot")
			}
			processed(t, f, command(t, w, domain.Win, "10.00", "later"))
			node := startInstance(t, f.url)
			replay, err := node.service.Process(f.ctx, cmd)
			if err != nil || !replay.IdempotentReplay || replay.Transaction.Result.Balance.MinorUnits() != wantBalance || replay.Transaction.Result.WalletVersion != wantVersion {
				t.Fatalf("original replay lost: %v", err)
			}
			assertBalance(t, f, w.Snapshot().ID, wantBalance+1000, wantVersion+1)
		})
	}
}

func TestRollbackInsufficientAndInvalidReferences(t *testing.T) {
	for _, scenario := range []string{"funds", "amount", "round", "wallet", "LOSS", "ROLLBACK", "rejected"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "invalid-rollback", "100.00")
			kind, amount := domain.Win, "50.00"
			if scenario == "LOSS" {
				kind, amount = domain.Loss, "0.00"
			}
			if scenario == "rejected" {
				kind, amount = domain.Bet, "200.00"
			}
			original, err := f.service.Process(f.ctx, command(t, w, kind, amount, "original"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "ROLLBACK" {
				original = processed(t, f, rollbackCommand(t, w, "50.00", "previous-rollback", "original"))
			}
			if scenario == "funds" {
				processed(t, f, command(t, w, domain.Bet, "125.00", "spend"))
			}
			cmd := rollbackCommand(t, w, amount, "rollback", original.Transaction.Data.ExternalTransactionID)
			if scenario == "LOSS" {
				cmd.Money = money(t, "50.00")
			}
			if scenario == "amount" {
				cmd.Money = money(t, "49.99")
			}
			if scenario == "round" {
				cmd.RoundID = "other-round"
			}
			if scenario == "wallet" {
				other := open(t, f, "other-rollback-player", "100.00")
				cmd.WalletID, cmd.PlayerID = other.Snapshot().ID, other.Snapshot().PlayerID
			}
			before, _ := f.store.GetWallet(f.ctx, cmd.WalletID)
			result, err := f.service.Process(f.ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.FailureInvalidReference
			if scenario == "funds" {
				want = domain.FailureReversalInsufficientBalance
			}
			if scenario == "rejected" {
				want = domain.FailureReferenceNotProcessed
			}
			if result.Transaction.Status != domain.Rejected || result.Transaction.FailureCode != want {
				t.Fatalf("wrong rejection: %+v", result.Transaction)
			}
			assertBalance(t, f, cmd.WalletID, before.Snapshot().Balance.MinorUnits(), before.Snapshot().Version)
			if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, result.Transaction.Data.ID) != 0 {
				t.Fatal("rejection moved money")
			}
			if count(t, f, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='WagerTransactionRejected'`, result.Transaction.Data.ID) != 1 {
				t.Fatal("rejection event missing")
			}
		})
	}
}

func TestRollbackAndRefundPolicyConcurrent(t *testing.T) {
	for _, scenario := range []string{"two rollbacks", "same rollback", "mixed", "refund then rollback", "rollback then refund", "rollback refund"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			w := open(t, f, "concurrent-reversal", "100.00")
			processed(t, f, command(t, w, domain.Bet, "80.00", "bet"))
			first := rollbackCommand(t, w, "80.00", "first", "bet")
			second := rollbackCommand(t, w, "80.00", "second", "bet")
			if scenario == "mixed" || scenario == "refund then rollback" || scenario == "rollback refund" {
				first = refundCommand(t, w, "80.00", "first", "bet")
			}
			if scenario == "rollback then refund" {
				second = refundCommand(t, w, "80.00", "second", "bet")
			}
			if scenario == "rollback refund" {
				processed(t, f, first)
				processed(t, f, rollbackCommand(t, w, "80.00", "reverse-refund", "first"))
				second = refundCommand(t, w, "80.00", "second", "bet")
				r, err := f.service.Process(f.ctx, second)
				if err != nil || r.Transaction.FailureCode != domain.FailureReversalConflict {
					t.Fatalf("refunded BET became available again: %v", err)
				}
				assertBalance(t, f, w.Snapshot().ID, 2000, 4)
				return
			}
			if scenario == "refund then rollback" || scenario == "rollback then refund" {
				processed(t, f, first)
				r, err := f.service.Process(f.ctx, second)
				if err != nil || r.Transaction.FailureCode != domain.FailureReversalConflict {
					t.Fatalf("double reversal allowed: %v", err)
				}
				assertBalance(t, f, w.Snapshot().ID, 10000, 3)
				return
			}
			nodes := []instance{f.instance, startInstance(t, f.url), startInstance(t, f.url)}
			n := 2
			if scenario == "same rollback" {
				n = 50
			}
			results := make(chan application.ProcessResult, n)
			errs := make(chan error, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				cmd := first
				if i == 1 && n == 2 {
					cmd = second
				}
				wg.Add(1)
				go func(i int, c application.ProcessCommand) {
					defer wg.Done()
					<-start
					r, err := nodes[i%len(nodes)].service.Process(f.ctx, c)
					if err != nil {
						errs <- err
					} else {
						results <- r
					}
				}(i, cmd)
			}
			close(start)
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			processedCount, rejectedCount, replays := 0, 0, 0
			for r := range results {
				if r.IdempotentReplay {
					replays++
				}
				if r.Transaction.Status == domain.Processed {
					processedCount++
				} else if r.Transaction.FailureCode == domain.FailureReversalConflict {
					rejectedCount++
				} else {
					t.Fatal("unstable conflict")
				}
			}
			if n == 2 && (processedCount != 1 || rejectedCount != 1) {
				t.Fatal("two reversals did not choose exactly one winner")
			}
			if n == 50 && (processedCount != 50 || replays != 49) {
				t.Fatal("same rollback not idempotent")
			}
			assertBalance(t, f, w.Snapshot().ID, 10000, 3)
			if count(t, f, `SELECT count(*) FROM wallet_ledger_entries l JOIN wager_transactions t ON t.id=l.transaction_id WHERE t.kind IN ('REFUND','ROLLBACK')`) != 1 {
				t.Fatal("duplicated reversal ledger")
			}
		})
	}
}

func TestRollbackDatabaseProtection(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "database-rollback", "100.00")
	processed(t, f, command(t, w, domain.Win, "25.00", "original"))
	rb := processed(t, f, rollbackCommand(t, w, "25.00", "rb", "original"))
	// Bypass Go and pick a distinct result version so the reversal index is tested.
	_, err := f.database.Pool().Exec(f.ctx, `INSERT INTO wager_transactions
		(id,external_transaction_id,provider_id,idempotency_key,payload_hash,wallet_id,player_id,round_id,game_id,kind,amount_minor_units,currency,reference_external_transaction_id,reference_transaction_id,status,result_balance_minor_units,result_wallet_version,created_at,updated_at)
		SELECT 'bypass','bypass',provider_id,'bypass',payload_hash,wallet_id,player_id,round_id,game_id,kind,amount_minor_units,currency,reference_external_transaction_id,reference_transaction_id,status,result_balance_minor_units,999,created_at,updated_at FROM wager_transactions WHERE id=$1`, rb.Transaction.Data.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "one_processed_reversal_per_reference" {
		t.Fatalf("database did not prevent duplicate rollback: %v", err)
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 3)
	// A mismatching existing reference is rejected by the database before ledger.
	_, err = f.database.Pool().Exec(f.ctx, `INSERT INTO wager_transactions
		(id,external_transaction_id,provider_id,idempotency_key,payload_hash,wallet_id,player_id,round_id,game_id,kind,amount_minor_units,currency,reference_external_transaction_id,reference_transaction_id,status,result_balance_minor_units,result_wallet_version,created_at,updated_at)
		SELECT 'invalid','invalid',provider_id,'invalid',payload_hash,wallet_id,player_id,'wrong-round',game_id,kind,amount_minor_units,currency,reference_external_transaction_id,reference_transaction_id,status,result_balance_minor_units,999,created_at,updated_at FROM wager_transactions WHERE id=$1`, rb.Transaction.Data.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("database accepted invalid rollback: %v", err)
	}
}

// Lock cancellation also exercises requests competing with financial resolution.
func TestRollbackIndependentWallets(t *testing.T) {
	f := newFixture(t)
	a := open(t, f, "blocked-rollback", "100.00")
	b := open(t, f, "free-rollback", "100.00")
	for i, w := range []*domain.Wallet{a, b} {
		processed(t, f, command(t, w, domain.Bet, "25.00", fmt.Sprintf("bet-%d", i)))
	}
	lock, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(f.ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, a.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	if _, err := f.service.Process(ctx, rollbackCommand(t, b, "25.00", "rb-free", "bet-1")); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, b.Snapshot().ID, 10000, 3)
}
