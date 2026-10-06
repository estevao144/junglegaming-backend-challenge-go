package domain

import (
	"fmt"
	"time"
)

type TransactionKind string

const (
	Opening  TransactionKind = "OPENING"
	Bet      TransactionKind = "BET"
	Win      TransactionKind = "WIN"
	Loss     TransactionKind = "LOSS"
	Refund   TransactionKind = "REFUND"
	Rollback TransactionKind = "ROLLBACK"
)

type TransactionStatus string

const (
	Pending          TransactionStatus = "PENDING"
	PendingReference TransactionStatus = "PENDING_REFERENCE"
	Processed        TransactionStatus = "PROCESSED"
	Rejected         TransactionStatus = "REJECTED"
	Failed           TransactionStatus = "FAILED"
)

func (s TransactionStatus) Terminal() bool { return s == Processed || s == Rejected || s == Failed }

// canTransitionTo defines the complete state machine. Unlisted transitions,
// including self-transitions and transitions from terminal states, are rejected.
func (s TransactionStatus) canTransitionTo(next TransactionStatus) bool {
	switch s {
	case Pending:
		return next == PendingReference || next == Processed || next == Rejected || next == Failed
	case PendingReference:
		return next == Processed || next == Rejected || next == Failed
	default:
		return false
	}
}

type FailureCode string

const (
	FailureInsufficientBalance         FailureCode = "INSUFFICIENT_BALANCE"
	FailureReversalInsufficientBalance FailureCode = "REVERSAL_INSUFFICIENT_BALANCE"
	FailureReferenceNotFound           FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceNotProcessed       FailureCode = "REFERENCE_NOT_PROCESSED"
	FailureInvalidReference            FailureCode = "INVALID_REFERENCE"
	FailureReversalConflict            FailureCode = "REVERSAL_CONFLICT"
	FailureInfrastructurePermanent     FailureCode = "INFRASTRUCTURE_PERMANENT"
)

type Direction string

const (
	DebitDirection  Direction = "DEBIT"
	CreditDirection Direction = "CREDIT"
	NoMovement      Direction = "NONE"
)

// TransactionData contains immutable identity and business input.
type TransactionData struct {
	ID                             string
	ExternalTransactionID          string
	ProviderID                     string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           TransactionKind
	Money                          Money
	ReferenceExternalTransactionID string
}

// FinancialResult captures the original balance, not the current wallet balance.
type FinancialResult struct {
	Balance       Money
	WalletVersion int64
}

type WagerTransactionState struct {
	Data                   TransactionData
	ReferenceTransactionID string
	Status                 TransactionStatus
	FailureCode            FailureCode
	Result                 *FinancialResult
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type WagerTransaction struct{ state WagerTransactionState }

func NewWagerTransaction(data TransactionData, at time.Time) (*WagerTransaction, error) {
	if data.Kind == Opening {
		return nil, fmt.Errorf("%w: OPENING is internal only", ErrInvalidTransaction)
	}
	return RehydrateWagerTransaction(WagerTransactionState{Data: data, Status: Pending, CreatedAt: at, UpdatedAt: at})
}

// NewOpeningTransaction creates the internal transaction as PENDING. The future
// opening use case must mark it PROCESSED before committing the wallet and ledger.
func NewOpeningTransaction(id, walletID, playerID string, money Money, at time.Time) (*WagerTransaction, error) {
	return RehydrateWagerTransaction(WagerTransactionState{
		Data:   TransactionData{ID: id, WalletID: walletID, PlayerID: playerID, Kind: Opening, Money: money},
		Status: Pending, CreatedAt: at, UpdatedAt: at,
	})
}

// RehydrateWagerTransaction restores a validated snapshot without transitions or effects.
func RehydrateWagerTransaction(state WagerTransactionState) (*WagerTransaction, error) {
	if err := validateTransactionState(state); err != nil {
		return nil, err
	}
	state.CreatedAt, state.UpdatedAt = state.CreatedAt.UTC(), state.UpdatedAt.UTC()
	state.Result = copyResult(state.Result)
	return &WagerTransaction{state: state}, nil
}

func copyResult(result *FinancialResult) *FinancialResult {
	if result == nil {
		return nil
	}
	copy := *result
	return &copy
}

func (t WagerTransaction) Snapshot() WagerTransactionState {
	state := t.state
	state.Result = copyResult(state.Result)
	return state
}

func validateTransactionData(data TransactionData) error {
	if !validID(data.ID) || !validID(data.WalletID) || !validID(data.PlayerID) {
		return fmt.Errorf("%w: missing identity", ErrInvalidTransaction)
	}
	if err := data.Money.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTransaction, err)
	}
	if data.Kind == Loss {
		if data.Money.MinorUnits() != 0 {
			return fmt.Errorf("%w: LOSS requires zero money", ErrInvalidTransaction)
		}
	} else if data.Money.MinorUnits() <= 0 {
		return fmt.Errorf("%w: positive money required", ErrInvalidTransaction)
	}
	switch data.Kind {
	case Opening:
		if data.ExternalTransactionID != "" || data.ProviderID != "" || data.IdempotencyKey != "" || data.PayloadHash != "" || data.RoundID != "" || data.GameID != "" || data.ReferenceExternalTransactionID != "" {
			return fmt.Errorf("%w: OPENING cannot contain external metadata", ErrInvalidTransaction)
		}
		return nil
	case Bet, Win, Loss, Refund, Rollback:
	default:
		return fmt.Errorf("%w: unknown kind", ErrInvalidTransaction)
	}
	for _, value := range []string{data.ExternalTransactionID, data.ProviderID, data.IdempotencyKey, data.PayloadHash, data.RoundID, data.GameID} {
		if !validID(value) {
			return fmt.Errorf("%w: missing external metadata", ErrInvalidTransaction)
		}
	}
	if data.ReferenceExternalTransactionID != "" {
		if !validID(data.ReferenceExternalTransactionID) || data.ReferenceExternalTransactionID == data.ExternalTransactionID {
			return fmt.Errorf("%w: invalid external reference", ErrInvalidTransaction)
		}
		if data.Kind != Win && data.Kind != Refund && data.Kind != Rollback {
			return fmt.Errorf("%w: kind does not accept a reference", ErrInvalidTransaction)
		}
	}
	if (data.Kind == Refund || data.Kind == Rollback) && data.ReferenceExternalTransactionID == "" {
		return fmt.Errorf("%w: reversal requires external reference", ErrInvalidTransaction)
	}
	return nil
}

func validateTransactionState(state WagerTransactionState) error {
	if err := validateTransactionData(state.Data); err != nil {
		return err
	}
	if !validTimes(state.CreatedAt, state.UpdatedAt) {
		return fmt.Errorf("%w: invalid timestamps", ErrInvalidTransaction)
	}
	if state.ReferenceTransactionID != "" {
		if !validID(state.ReferenceTransactionID) || state.ReferenceTransactionID == state.Data.ID || state.Data.ReferenceExternalTransactionID == "" {
			return fmt.Errorf("%w: invalid internal reference", ErrInvalidTransaction)
		}
	}
	if state.Result != nil {
		if err := state.Result.Balance.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidTransaction, err)
		}
		if state.Result.Balance.Currency() != state.Data.Money.Currency() {
			return fmt.Errorf("%w: %w", ErrInvalidTransaction, ErrCurrencyMismatch)
		}
		if state.Result.Balance.MinorUnits() < 0 || state.Result.WalletVersion < 1 {
			return fmt.Errorf("%w: invalid financial result", ErrInvalidTransaction)
		}
	}
	switch state.Status {
	case Pending:
		if state.ReferenceTransactionID != "" || state.FailureCode != "" || state.Result != nil {
			return fmt.Errorf("%w: PENDING cannot contain an outcome", ErrInvalidTransaction)
		}
	case PendingReference:
		if state.Data.ReferenceExternalTransactionID == "" || state.ReferenceTransactionID != "" || state.FailureCode != "" || state.Result != nil {
			return fmt.Errorf("%w: invalid reference wait state", ErrInvalidTransaction)
		}
	case Processed:
		if state.FailureCode != "" || state.Result == nil {
			return fmt.Errorf("%w: processed outcome required", ErrInvalidTransaction)
		}
		if state.Data.ReferenceExternalTransactionID != "" && state.ReferenceTransactionID == "" {
			return fmt.Errorf("%w: processed reference must be resolved", ErrInvalidTransaction)
		}
		if state.Data.Kind == Opening && (state.Result.Balance != state.Data.Money || state.Result.WalletVersion != 1) {
			return fmt.Errorf("%w: opening result must match initial balance at version 1", ErrInvalidTransaction)
		}
	case Rejected:
		if !businessFailure(state.FailureCode) {
			return fmt.Errorf("%w: business failure code required", ErrInvalidTransaction)
		}
	case Failed:
		if state.FailureCode != FailureInfrastructurePermanent {
			return fmt.Errorf("%w: permanent infrastructure code required", ErrInvalidTransaction)
		}
	default:
		return fmt.Errorf("%w: unknown status", ErrInvalidTransaction)
	}
	return nil
}

func businessFailure(code FailureCode) bool {
	switch code {
	case FailureInsufficientBalance, FailureReversalInsufficientBalance, FailureReferenceNotFound, FailureReferenceNotProcessed, FailureInvalidReference, FailureReversalConflict:
		return true
	default:
		return false
	}
}

func (t *WagerTransaction) transition(state WagerTransactionState, at time.Time) error {
	if t == nil {
		return ErrInvalidTransaction
	}
	if err := validateTransactionState(t.state); err != nil {
		return err
	}
	if !t.state.Status.canTransitionTo(state.Status) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.state.Status, state.Status)
	}
	if at.IsZero() || at.Before(t.state.UpdatedAt) {
		return ErrInvalidTransition
	}
	state.UpdatedAt = at.UTC()
	if err := validateTransactionState(state); err != nil {
		return err
	}
	state.Result = copyResult(state.Result)
	t.state = state
	return nil
}

func (t *WagerTransaction) MarkPendingReference(at time.Time) error {
	if t == nil {
		return ErrInvalidTransaction
	}
	state := t.state
	state.Status = PendingReference
	return t.transition(state, at)
}

func (t *WagerTransaction) MarkProcessed(result FinancialResult, referenceTransactionID string, at time.Time) error {
	if t == nil {
		return ErrInvalidTransaction
	}
	state := t.state
	state.Status, state.Result, state.ReferenceTransactionID = Processed, &result, referenceTransactionID
	return t.transition(state, at)
}

func (t *WagerTransaction) Reject(code FailureCode, result *FinancialResult, at time.Time) error {
	if t == nil {
		return ErrInvalidTransaction
	}
	state := t.state
	state.Status, state.FailureCode, state.Result = Rejected, code, result
	return t.transition(state, at)
}

// Fail is for permanent failures only. Transient I/O errors leave state unchanged
// and must be retried by the future application layer.
func (t *WagerTransaction) Fail(at time.Time) error {
	if t == nil {
		return ErrInvalidTransaction
	}
	state := t.state
	state.Status, state.FailureCode = Failed, FailureInfrastructurePermanent
	return t.transition(state, at)
}

func (t WagerTransaction) Movement() (Direction, error) {
	if err := validateTransactionState(t.state); err != nil {
		return "", err
	}
	switch t.state.Data.Kind {
	case Opening, Win, Refund:
		return CreditDirection, nil
	case Bet:
		return DebitDirection, nil
	case Loss:
		return NoMovement, nil
	case Rollback:
		return "", ErrUnresolvedReference
	default:
		return "", ErrInvalidTransaction
	}
}
