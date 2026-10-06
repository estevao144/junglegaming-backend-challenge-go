package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func testTime() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }

func mustWallet(t *testing.T, amount string) *Wallet {
	t.Helper()
	w, err := NewWallet("wallet-1", "player-1", mustMoney(t, amount, "BRL"), testTime())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWalletCreationAndRehydration(t *testing.T) {
	for _, amount := range []string{"0.00", "100.00"} {
		w := mustWallet(t, amount)
		state := w.Snapshot()
		if state.Version != 1 || state.Currency != "BRL" || state.CreatedAt != testTime() || state.UpdatedAt != testTime() {
			t.Fatalf("unexpected state: %+v", state)
		}
		state.Version = 7
		state.UpdatedAt = testTime().Add(time.Hour)
		rehydrated, err := RehydrateWallet(state)
		if err != nil {
			t.Fatal(err)
		}
		if rehydrated.Snapshot() != state {
			t.Fatal("rehydration changed persisted state")
		}
		state.Balance = mustMoney(t, "1.00", "BRL")
		if w.Snapshot().Balance != mustMoney(t, amount, "BRL") {
			t.Fatal("snapshot mutation changed wallet")
		}
	}
	w, err := NewWallet("wallet", "player", mustMoney(t, "0.00", "BRL"), testTime().In(time.FixedZone("local", -3*3600)))
	if err != nil || w.Snapshot().CreatedAt.Location() != time.UTC {
		t.Fatalf("UTC conversion failed: %v", err)
	}
}

func TestWalletMovements(t *testing.T) {
	w := mustWallet(t, "100.00")
	if err := w.Debit(mustMoney(t, "80.00", "BRL"), testTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if state := w.Snapshot(); state.Balance.MinorUnits() != 2000 || state.Version != 2 {
		t.Fatalf("debit: %+v", state)
	}
	if err := w.Credit(mustMoney(t, "5.00", "BRL"), testTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if state := w.Snapshot(); state.Balance.MinorUnits() != 2500 || state.Version != 3 || state.UpdatedAt != testTime().Add(2*time.Second) {
		t.Fatalf("credit: %+v", state)
	}
	if err := w.Debit(mustMoney(t, "25.00", "BRL"), testTime().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if w.Snapshot().Balance.MinorUnits() != 0 {
		t.Fatal("exact balance debit must be accepted")
	}
}

func TestWalletRejectedMovementsDoNotMutate(t *testing.T) {
	for _, test := range []struct {
		name  string
		money Money
		debit bool
		at    time.Time
		want  error
	}{
		{"insufficient", mustMoney(t, "100.01", "BRL"), true, testTime(), ErrInsufficientBalance},
		{"currency debit", foreignMoneyForTest(100), true, testTime(), ErrCurrencyMismatch},
		{"currency credit", foreignMoneyForTest(100), false, testTime(), ErrCurrencyMismatch},
		{"uninitialized", Money{}, false, testTime(), ErrInvalidCurrency},
		{"zero credit", mustUnits(t, 0), false, testTime().Add(time.Hour), ErrInvalidMoney},
		{"zero debit", mustUnits(t, 0), true, testTime().Add(time.Hour), ErrInvalidMoney},
		{"negative credit", mustUnits(t, -1), false, testTime(), ErrInvalidMoney},
		{"negative debit", mustUnits(t, -1), true, testTime(), ErrInvalidMoney},
		{"missing time", mustUnits(t, 1), false, time.Time{}, ErrInvalidWallet},
		{"past time", mustUnits(t, 1), false, testTime().Add(-time.Second), ErrInvalidWallet},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := mustWallet(t, "100.00")
			before := w.Snapshot()
			var err error
			if test.debit {
				err = w.Debit(test.money, test.at)
			} else {
				err = w.Credit(test.money, test.at)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if w.Snapshot() != before {
				t.Fatal("rejected movement mutated wallet")
			}
		})
	}
	for _, version := range []int64{1, math.MaxInt64} {
		state := mustWallet(t, "0.00").Snapshot()
		state.Version = version
		if version == 1 {
			state.Balance = mustUnits(t, math.MaxInt64)
		}
		w, err := RehydrateWallet(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Credit(mustUnits(t, 1), testTime()); !errors.Is(err, ErrOverflow) {
			t.Fatalf("overflow: %v", err)
		}
		if w.Snapshot() != state {
			t.Fatal("overflow mutated wallet")
		}
	}
}

func TestWalletRejectsInvalidState(t *testing.T) {
	for _, change := range []func(*WalletState){
		func(s *WalletState) { s.ID = "" },
		func(s *WalletState) { s.PlayerID = " " },
		func(s *WalletState) { s.Version = 0 },
		func(s *WalletState) { s.Balance = Money{} },
		func(s *WalletState) { s.Balance = mustUnits(t, -1) },
		func(s *WalletState) { s.Currency = "USD" },
		func(s *WalletState) { s.CreatedAt = time.Time{} },
		func(s *WalletState) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) },
	} {
		state := mustWallet(t, "0.00").Snapshot()
		change(&state)
		if _, err := RehydrateWallet(state); !errors.Is(err, ErrInvalidWallet) {
			t.Fatalf("accepted invalid state: %v", err)
		}
	}
	if _, err := NewWallet("wallet", "player", mustUnits(t, -1), testTime()); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal(err)
	}
	var w Wallet
	if err := w.Credit(mustUnits(t, 1), testTime()); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal("zero wallet accepted credit")
	}
	var nilWallet *Wallet
	if err := nilWallet.Debit(mustUnits(t, 1), testTime()); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal("nil wallet accepted debit")
	}
}
