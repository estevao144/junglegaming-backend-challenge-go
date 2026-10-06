package domain

import (
	"fmt"
	"math"
	"time"
)

// WalletState is a value snapshot for persistence, never a mutable view of Wallet.
type WalletState struct {
	ID        string
	PlayerID  string
	Currency  string
	Balance   Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Wallet struct{ state WalletState }

func NewWallet(id, playerID string, balance Money, at time.Time) (*Wallet, error) {
	return RehydrateWallet(WalletState{
		ID: id, PlayerID: playerID, Currency: balance.Currency(), Balance: balance,
		Version: 1, CreatedAt: at, UpdatedAt: at,
	})
}

// RehydrateWallet preserves the supplied balance and version; it performs no movement.
func RehydrateWallet(state WalletState) (*Wallet, error) {
	if err := validateWalletState(state); err != nil {
		return nil, err
	}
	state.CreatedAt, state.UpdatedAt = state.CreatedAt.UTC(), state.UpdatedAt.UTC()
	return &Wallet{state: state}, nil
}

func validateWalletState(state WalletState) error {
	if !validID(state.ID) || !validID(state.PlayerID) || state.Version < 1 || !validTimes(state.CreatedAt, state.UpdatedAt) {
		return fmt.Errorf("%w: identity, version or timestamps", ErrInvalidWallet)
	}
	if err := state.Balance.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWallet, err)
	}
	if state.Balance.MinorUnits() < 0 {
		return fmt.Errorf("%w: negative balance", ErrInvalidWallet)
	}
	if state.Currency != state.Balance.Currency() {
		return fmt.Errorf("%w: %w", ErrInvalidWallet, ErrCurrencyMismatch)
	}
	return nil
}

func (w Wallet) Snapshot() WalletState { return w.state }

func (w *Wallet) Credit(money Money, at time.Time) error { return w.move(money, at, false) }
func (w *Wallet) Debit(money Money, at time.Time) error  { return w.move(money, at, true) }

func (w *Wallet) move(money Money, at time.Time, debit bool) error {
	if w == nil {
		return ErrInvalidWallet
	}
	if err := validateWalletState(w.state); err != nil {
		return err
	}
	if err := w.state.Balance.compatible(money); err != nil {
		return err
	}
	if money.MinorUnits() < 0 {
		return fmt.Errorf("%w: negative movement", ErrInvalidMoney)
	}
	if at.IsZero() || at.Before(w.state.UpdatedAt) {
		return fmt.Errorf("%w: movement timestamp precedes current state", ErrInvalidWallet)
	}
	if money.MinorUnits() == 0 {
		return nil
	}
	if w.state.Version == math.MaxInt64 {
		return ErrOverflow
	}
	var balance Money
	var err error
	if debit {
		if money.MinorUnits() > w.state.Balance.MinorUnits() {
			return ErrInsufficientBalance
		}
		balance, err = w.state.Balance.Subtract(money)
	} else {
		balance, err = w.state.Balance.Add(money)
	}
	if err != nil {
		return err
	}
	// All validations succeeded: commit the in-memory transition together.
	w.state.Balance = balance
	w.state.Version++
	w.state.UpdatedAt = at.UTC()
	return nil
}
