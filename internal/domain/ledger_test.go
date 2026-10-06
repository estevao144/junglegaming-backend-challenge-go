package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func ledgerState(t *testing.T) LedgerEntryState {
	return LedgerEntryState{
		ID: "ledger-1", WalletID: "wallet-1", TransactionID: "transaction-1", Direction: CreditDirection,
		Money: mustMoney(t, "25.00", "BRL"), BalanceBefore: mustMoney(t, "100.00", "BRL"),
		BalanceAfter: mustMoney(t, "125.00", "BRL"), CreatedAt: testTime(),
	}
}

func TestLedgerCreationRehydrationAndImmutability(t *testing.T) {
	for _, test := range []struct {
		name          string
		direction     Direction
		before, after string
	}{
		{"credit", CreditDirection, "100.00", "125.00"},
		{"debit", DebitDirection, "100.00", "75.00"},
		{"opening", CreditDirection, "0.00", "25.00"},
		{"full debit", DebitDirection, "25.00", "0.00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := ledgerState(t)
			state.Direction, state.BalanceBefore, state.BalanceAfter = test.direction, mustMoney(t, test.before, "BRL"), mustMoney(t, test.after, "BRL")
			entry, err := NewWalletLedgerEntry(state)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Snapshot() != state {
				t.Fatal("creation changed entry")
			}
			restored, err := RehydrateWalletLedgerEntry(entry.Snapshot())
			if err != nil || restored.Snapshot() != state {
				t.Fatalf("rehydration changed entry: %v", err)
			}
			copy := entry.Snapshot()
			copy.ID, copy.BalanceAfter = "changed", mustMoney(t, "0.00", "BRL")
			state.Money = mustMoney(t, "1.00", "BRL")
			if entry.Snapshot().ID != "ledger-1" || entry.Snapshot().Money.MinorUnits() != 2500 {
				t.Fatal("ledger mutated through caller state")
			}
		})
	}
}

func TestLedgerRejectsInvalidState(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*LedgerEntryState)
		cause  error
	}{
		{"id", func(s *LedgerEntryState) { s.ID = "" }, nil},
		{"wallet", func(s *LedgerEntryState) { s.WalletID = " " }, nil},
		{"transaction", func(s *LedgerEntryState) { s.TransactionID = "" }, nil},
		{"timestamp", func(s *LedgerEntryState) { s.CreatedAt = time.Time{} }, nil},
		{"direction", func(s *LedgerEntryState) { s.Direction = "UNKNOWN" }, nil},
		{"loss cannot produce ledger", func(s *LedgerEntryState) { s.Direction = NoMovement }, nil},
		{"zero", func(s *LedgerEntryState) { s.Money = mustUnits(t, 0) }, nil},
		{"negative money", func(s *LedgerEntryState) { s.Money = mustUnits(t, -1) }, nil},
		{"invalid money", func(s *LedgerEntryState) { s.Money = Money{} }, ErrInvalidCurrency},
		{"invalid before", func(s *LedgerEntryState) { s.BalanceBefore = Money{} }, ErrInvalidCurrency},
		{"invalid after", func(s *LedgerEntryState) { s.BalanceAfter = Money{} }, ErrInvalidCurrency},
		{"negative before", func(s *LedgerEntryState) { s.BalanceBefore = mustUnits(t, -1) }, nil},
		{"negative after", func(s *LedgerEntryState) { s.BalanceAfter = mustUnits(t, -1) }, nil},
		{"unsupported money currency", func(s *LedgerEntryState) { s.Money = foreignMoneyForTest(2500) }, ErrInvalidCurrency},
		{"unsupported after currency", func(s *LedgerEntryState) { s.BalanceAfter = foreignMoneyForTest(12500) }, ErrInvalidCurrency},
		{"wrong credit equation", func(s *LedgerEntryState) { s.BalanceAfter = mustMoney(t, "75.00", "BRL") }, nil},
		{"wrong debit equation", func(s *LedgerEntryState) { s.Direction = DebitDirection }, nil},
		{"insufficient balance", func(s *LedgerEntryState) {
			s.Direction = DebitDirection
			s.BalanceBefore = mustUnits(t, 1)
			s.BalanceAfter = mustUnits(t, 0)
		}, nil},
		{"overflow", func(s *LedgerEntryState) {
			s.BalanceBefore = mustUnits(t, math.MaxInt64)
			s.Money = mustUnits(t, 1)
			s.BalanceAfter = mustUnits(t, 0)
		}, ErrOverflow},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := ledgerState(t)
			test.change(&state)
			if _, err := NewWalletLedgerEntry(state); !errors.Is(err, ErrInvalidLedgerEntry) || (test.cause != nil && !errors.Is(err, test.cause)) {
				t.Fatalf("error = %v", err)
			}
			if _, err := RehydrateWalletLedgerEntry(state); !errors.Is(err, ErrInvalidLedgerEntry) {
				t.Fatalf("invalid rehydration accepted: %v", err)
			}
		})
	}
}
