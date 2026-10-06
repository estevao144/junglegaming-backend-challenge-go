package domain

import (
	"fmt"
	"time"
)

type LedgerEntryState struct {
	ID            string
	WalletID      string
	TransactionID string
	Direction     Direction
	Money         Money
	BalanceBefore Money
	BalanceAfter  Money
	CreatedAt     time.Time
}

// WalletLedgerEntry is immutable: it exposes only a value snapshot.
type WalletLedgerEntry struct{ state LedgerEntryState }

func NewWalletLedgerEntry(state LedgerEntryState) (WalletLedgerEntry, error) {
	return RehydrateWalletLedgerEntry(state)
}

// RehydrateWalletLedgerEntry validates historical data without moving a wallet.
func RehydrateWalletLedgerEntry(state LedgerEntryState) (WalletLedgerEntry, error) {
	if !validID(state.ID) || !validID(state.WalletID) || !validID(state.TransactionID) || state.CreatedAt.IsZero() {
		return WalletLedgerEntry{}, fmt.Errorf("%w: identity or timestamp", ErrInvalidLedgerEntry)
	}
	for _, money := range []Money{state.Money, state.BalanceBefore, state.BalanceAfter} {
		if err := money.Validate(); err != nil {
			return WalletLedgerEntry{}, fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
		}
	}
	if err := state.Money.compatible(state.BalanceBefore); err != nil {
		return WalletLedgerEntry{}, fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
	}
	if err := state.Money.compatible(state.BalanceAfter); err != nil {
		return WalletLedgerEntry{}, fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
	}
	if state.Money.MinorUnits() <= 0 || state.BalanceBefore.MinorUnits() < 0 || state.BalanceAfter.MinorUnits() < 0 {
		return WalletLedgerEntry{}, fmt.Errorf("%w: positive money and nonnegative balances required", ErrInvalidLedgerEntry)
	}
	var expected Money
	var err error
	switch state.Direction {
	case CreditDirection:
		expected, err = state.BalanceBefore.Add(state.Money)
	case DebitDirection:
		expected, err = state.BalanceBefore.Subtract(state.Money)
	default:
		return WalletLedgerEntry{}, fmt.Errorf("%w: direction must be DEBIT or CREDIT", ErrInvalidLedgerEntry)
	}
	if err != nil {
		return WalletLedgerEntry{}, fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
	}
	if expected != state.BalanceAfter {
		return WalletLedgerEntry{}, fmt.Errorf("%w: inconsistent balance equation", ErrInvalidLedgerEntry)
	}
	state.CreatedAt = state.CreatedAt.UTC()
	return WalletLedgerEntry{state: state}, nil
}

func (e WalletLedgerEntry) Snapshot() LedgerEntryState { return e.state }
