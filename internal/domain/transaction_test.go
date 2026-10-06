package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func transactionData(t *testing.T, kind TransactionKind) TransactionData {
	t.Helper()
	amount := "25.00"
	if kind == Loss {
		amount = "0.00"
	}
	data := TransactionData{
		ID: "transaction-1", ExternalTransactionID: "external-1", ProviderID: "provider-a",
		IdempotencyKey: "custom-key", PayloadHash: "payload-hash", WalletID: "wallet-1",
		PlayerID: "player-1", RoundID: "round-1", GameID: "game-1", Kind: kind,
		Money: mustMoney(t, amount, "BRL"),
	}
	if kind == Refund || kind == Rollback {
		data.ReferenceExternalTransactionID = "external-original"
	}
	return data
}

func mustTransaction(t *testing.T, kind TransactionKind) *WagerTransaction {
	t.Helper()
	tx, err := NewWagerTransaction(transactionData(t, kind), testTime())
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func financialResult(t *testing.T) FinancialResult {
	return FinancialResult{Balance: mustMoney(t, "75.00", "BRL"), WalletVersion: 2}
}

func TestTransactionKindsAndAmounts(t *testing.T) {
	for _, test := range []struct {
		kind        TransactionKind
		movement    Direction
		movementErr error
	}{
		{Bet, DebitDirection, nil}, {Win, CreditDirection, nil}, {Loss, NoMovement, nil},
		{Refund, CreditDirection, nil}, {Rollback, "", ErrUnresolvedReference},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			tx := mustTransaction(t, test.kind)
			state := tx.Snapshot()
			if state.Status != Pending || state.Result != nil || state.FailureCode != "" || state.Data.IdempotencyKey != "custom-key" {
				t.Fatalf("unexpected creation: %+v", state)
			}
			movement, err := tx.Movement()
			if movement != test.movement || !errors.Is(err, test.movementErr) {
				t.Fatalf("movement = %s, %v", movement, err)
			}
			for _, units := range []int64{-1, 0, 1} {
				data := transactionData(t, test.kind)
				data.Money = mustUnits(t, units)
				_, err := NewWagerTransaction(data, testTime())
				valid := (test.kind == Loss && units == 0) || (test.kind != Loss && units > 0)
				if valid && err != nil {
					t.Fatal(err)
				}
				if !valid && !errors.Is(err, ErrInvalidTransaction) {
					t.Fatalf("kind=%s units=%d accepted: %v", test.kind, units, err)
				}
			}
		})
	}
}

func TestTransactionRejectsInvalidInput(t *testing.T) {
	for _, change := range []func(*TransactionData){
		func(d *TransactionData) { d.ID = "" },
		func(d *TransactionData) { d.ExternalTransactionID = "" },
		func(d *TransactionData) { d.ProviderID = " " },
		func(d *TransactionData) { d.IdempotencyKey = "" },
		func(d *TransactionData) { d.PayloadHash = "" },
		func(d *TransactionData) { d.WalletID = "" },
		func(d *TransactionData) { d.PlayerID = "" },
		func(d *TransactionData) { d.RoundID = "" },
		func(d *TransactionData) { d.GameID = "" },
		func(d *TransactionData) { d.Money = Money{} },
		func(d *TransactionData) { d.Kind = "OTHER" },
		func(d *TransactionData) { d.Kind = Opening },
		func(d *TransactionData) { d.ReferenceExternalTransactionID = "external-original" },
	} {
		data := transactionData(t, Bet)
		change(&data)
		if _, err := NewWagerTransaction(data, testTime()); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatalf("accepted input: %+v, %v", data, err)
		}
	}
	for _, kind := range []TransactionKind{Refund, Rollback} {
		data := transactionData(t, kind)
		data.ReferenceExternalTransactionID = ""
		if _, err := NewWagerTransaction(data, testTime()); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatal("accepted reversal without reference")
		}
		data.ReferenceExternalTransactionID = data.ExternalTransactionID
		if _, err := NewWagerTransaction(data, testTime()); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatal("accepted self reference")
		}
	}
	if _, err := NewWagerTransaction(transactionData(t, Bet), time.Time{}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatal("accepted zero time")
	}
}

func TestOpeningTransaction(t *testing.T) {
	money := mustMoney(t, "100.00", "BRL")
	tx, err := NewOpeningTransaction("opening-1", "wallet-1", "player-1", money, testTime())
	if err != nil {
		t.Fatal(err)
	}
	state := tx.Snapshot()
	if state.Status != Pending || state.Data.ProviderID != "" || state.Data.ExternalTransactionID != "" || state.Data.IdempotencyKey != "" || state.Data.PayloadHash != "" || state.Data.RoundID != "" || state.Data.GameID != "" {
		t.Fatal("opening includes external metadata")
	}
	if direction, err := tx.Movement(); err != nil || direction != CreditDirection {
		t.Fatal("opening must credit")
	}
	if err := tx.MarkProcessed(FinancialResult{Balance: money, WalletVersion: 1}, "", testTime()); err != nil {
		t.Fatal(err)
	}
	if tx.Snapshot().Status != Processed {
		t.Fatal("opening must finish processed")
	}
	for _, units := range []int64{0, -1} {
		if _, err := NewOpeningTransaction("opening", "wallet", "player", mustUnits(t, units), testTime()); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatal("zero or negative opening accepted")
		}
	}
	state.Data.ProviderID = "provider-a"
	if _, err := RehydrateWagerTransaction(state); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatal("opening accepted provider metadata")
	}
}

func TestTransactionTransitions(t *testing.T) {
	for _, initial := range []TransactionStatus{Pending, PendingReference, Processed, Rejected, Failed} {
		for _, target := range []TransactionStatus{PendingReference, Processed, Rejected, Failed} {
			t.Run(string(initial)+" to "+string(target), func(t *testing.T) {
				tx := mustTransaction(t, Refund)
				at := testTime().Add(time.Second)
				switch initial {
				case PendingReference:
					if err := tx.MarkPendingReference(at); err != nil {
						t.Fatal(err)
					}
				case Processed:
					if err := tx.MarkProcessed(financialResult(t), "original-1", at); err != nil {
						t.Fatal(err)
					}
				case Rejected:
					if err := tx.Reject(FailureReferenceNotFound, nil, at); err != nil {
						t.Fatal(err)
					}
				case Failed:
					if err := tx.Fail(at); err != nil {
						t.Fatal(err)
					}
				}
				before := tx.Snapshot()
				var err error
				at = at.Add(time.Second)
				switch target {
				case PendingReference:
					err = tx.MarkPendingReference(at)
				case Processed:
					err = tx.MarkProcessed(financialResult(t), "original-1", at)
				case Rejected:
					err = tx.Reject(FailureReferenceNotFound, nil, at)
				case Failed:
					err = tx.Fail(at)
				}
				valid := !initial.Terminal() && initial != target
				if valid {
					if err != nil {
						t.Fatal(err)
					}
					if tx.Snapshot().Status != target || tx.Snapshot().UpdatedAt != at {
						t.Fatal("transition did not update state")
					}
				} else {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("expected transition rejection, got %v", err)
					}
					if !reflect.DeepEqual(before, tx.Snapshot()) {
						t.Fatal("invalid transition mutated transaction")
					}
				}
			})
		}
	}
}

func TestTransactionOutcomesAndSnapshotsAreIndependent(t *testing.T) {
	tx := mustTransaction(t, Bet)
	result := financialResult(t)
	if err := tx.MarkProcessed(result, "", testTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	state := tx.Snapshot()
	restored, err := RehydrateWagerTransaction(state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Snapshot(), state) {
		t.Fatal("rehydration changed outcome")
	}
	state.Result.Balance = mustMoney(t, "1.00", "BRL")
	state.Data.ProviderID = "another-provider"
	result.Balance = mustMoney(t, "2.00", "BRL")
	if tx.Snapshot().Result.Balance.MinorUnits() != 7500 || restored.Snapshot().Result.Balance.MinorUnits() != 7500 || tx.Snapshot().Data.ProviderID != "provider-a" {
		t.Fatal("result or metadata aliased external state")
	}
	rejected := mustTransaction(t, Bet)
	result = financialResult(t)
	if err := rejected.Reject(FailureInsufficientBalance, &result, testTime()); err != nil {
		t.Fatal(err)
	}
	result.WalletVersion = 100
	if rejected.Snapshot().Result.WalletVersion != 2 {
		t.Fatal("rejected result aliased caller pointer")
	}
}

func TestTransactionRejectsInvalidOutcomes(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   TransactionKind
		change func(*WagerTransaction) error
	}{
		{"wait without reference", Bet, func(tx *WagerTransaction) error { return tx.MarkPendingReference(testTime()) }},
		{"unresolved refund", Refund, func(tx *WagerTransaction) error { return tx.MarkProcessed(financialResult(t), "", testTime()) }},
		{"internal reference without external", Bet, func(tx *WagerTransaction) error { return tx.MarkProcessed(financialResult(t), "original", testTime()) }},
		{"self internal reference", Refund, func(tx *WagerTransaction) error {
			return tx.MarkProcessed(financialResult(t), "transaction-1", testTime())
		}},
		{"empty result", Bet, func(tx *WagerTransaction) error { return tx.MarkProcessed(FinancialResult{}, "", testTime()) }},
		{"negative result", Bet, func(tx *WagerTransaction) error {
			return tx.MarkProcessed(FinancialResult{Balance: mustUnits(t, -1), WalletVersion: 1}, "", testTime())
		}},
		{"mismatched result currency", Bet, func(tx *WagerTransaction) error {
			return tx.MarkProcessed(FinancialResult{Balance: foreignMoneyForTest(100), WalletVersion: 1}, "", testTime())
		}},
		{"result version zero", Bet, func(tx *WagerTransaction) error {
			return tx.MarkProcessed(FinancialResult{Balance: mustUnits(t, 1)}, "", testTime())
		}},
		{"empty failure", Bet, func(tx *WagerTransaction) error { return tx.Reject("", nil, testTime()) }},
		{"unknown failure", Bet, func(tx *WagerTransaction) error { return tx.Reject("UNKNOWN", nil, testTime()) }},
		{"infra is not business rejection", Bet, func(tx *WagerTransaction) error { return tx.Reject(FailureInfrastructurePermanent, nil, testTime()) }},
		{"past time", Bet, func(tx *WagerTransaction) error { return tx.Fail(testTime().Add(-time.Second)) }},
		{"zero time", Bet, func(tx *WagerTransaction) error { return tx.Fail(time.Time{}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := mustTransaction(t, test.kind)
			before := tx.Snapshot()
			if err := test.change(tx); err == nil {
				t.Fatal("invalid transition accepted")
			}
			if !reflect.DeepEqual(before, tx.Snapshot()) {
				t.Fatal("invalid outcome mutated transaction")
			}
		})
	}
	win := transactionData(t, Win)
	win.ReferenceExternalTransactionID = "external-original"
	tx, err := NewWagerTransaction(win, testTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkPendingReference(testTime()); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkProcessed(financialResult(t), "original-1", testTime()); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionRehydrationValidation(t *testing.T) {
	for _, change := range []func(*WagerTransactionState){
		func(s *WagerTransactionState) { s.Status = "UNKNOWN" },
		func(s *WagerTransactionState) { s.Status = Processed },
		func(s *WagerTransactionState) { s.Status = Rejected },
		func(s *WagerTransactionState) { s.Status = Failed },
		func(s *WagerTransactionState) { s.Status = PendingReference },
		func(s *WagerTransactionState) { s.FailureCode = FailureInsufficientBalance },
		func(s *WagerTransactionState) { r := financialResult(t); s.Result = &r },
		func(s *WagerTransactionState) { s.CreatedAt = time.Time{} },
		func(s *WagerTransactionState) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) },
	} {
		state := mustTransaction(t, Bet).Snapshot()
		change(&state)
		if _, err := RehydrateWagerTransaction(state); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatalf("invalid persisted state accepted: %v", err)
		}
	}
	var tx WagerTransaction
	if err := tx.Fail(testTime()); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatal("zero transaction accepted transition")
	}
	var nilTx *WagerTransaction
	if err := nilTx.MarkProcessed(financialResult(t), "", testTime()); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatal("nil transaction accepted transition")
	}
}
