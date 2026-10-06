package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
)

var (
	ErrUnsupportedOperation = errors.New("operation requires a later processing stage")
	ErrWalletIdentity       = errors.New("wallet player or currency does not match operation")
)

type ProcessCommand struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           domain.TransactionKind
	Money                          domain.Money
	ReferenceExternalTransactionID string
	CorrelationID                  string
	CausationID                    string
}

type ProcessResult struct {
	Transaction      domain.WagerTransactionState
	IdempotentReplay bool
}

type FinancialService struct{ store *postgres.Store }

func NewFinancialService(store *postgres.Store) *FinancialService {
	return &FinancialService{store: store}
}

func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

// PayloadHash uses a fixed schema whose fields (including nested Money) are
// declared in lexical key order. It excludes all transport fields and the key.
func PayloadHash(c ProcessCommand) (string, error) {
	if err := c.Money.Validate(); err != nil {
		return "", err
	}
	canonical := struct {
		ExternalTransactionID          string                 `json:"externalTransactionId"`
		GameID                         string                 `json:"gameId"`
		Kind                           domain.TransactionKind `json:"kind"`
		Money                          domain.Money           `json:"money"`
		PlayerID                       string                 `json:"playerId"`
		ProviderID                     string                 `json:"providerId"`
		ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId"`
		RoundID                        string                 `json:"roundId"`
		WalletID                       string                 `json:"walletId"`
	}{c.ExternalTransactionID, c.GameID, c.Kind, c.Money, c.PlayerID, c.ProviderID, c.ReferenceExternalTransactionID, c.RoundID, c.WalletID}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

func eventIdentity(correlation, causation string) bool {
	return correlation != "" && strings.TrimSpace(correlation) == correlation && (causation == "" || strings.TrimSpace(causation) == causation)
}

// OpenWallet is an internal use case. A future transport must authorize callers
// before invoking it. A zero opening produces no financial transaction or event.
func (s *FinancialService) OpenWallet(ctx context.Context, playerID string, balance domain.Money, correlation string) (*domain.Wallet, error) {
	if !eventIdentity(correlation, "") {
		return nil, fmt.Errorf("correlationId is required")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	wallet, err := domain.NewWallet(id, playerID, balance, at)
	if err != nil {
		return nil, err
	}
	err = s.store.WithTx(ctx, func(r *postgres.Repositories) error {
		if err := r.Wallets.Insert(ctx, wallet); err != nil {
			return err
		}
		if balance.MinorUnits() == 0 {
			return nil
		}
		txID, err := newID()
		if err != nil {
			return err
		}
		transaction, err := domain.NewOpeningTransaction(txID, id, playerID, balance, at)
		if err != nil {
			return err
		}
		if err := transaction.MarkProcessed(domain.FinancialResult{Balance: balance, WalletVersion: 1}, "", at); err != nil {
			return err
		}
		inserted, err := r.Transactions.Insert(ctx, transaction)
		if err != nil {
			return err
		}
		if !inserted {
			return postgres.ErrConflict
		}
		zero, err := domain.ZeroMoney(balance.Currency())
		if err != nil {
			return err
		}
		entry, err := newEntry(transaction, domain.CreditDirection, zero, balance, at)
		if err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, entry); err != nil {
			return err
		}
		return recordProcessed(ctx, r, transaction, &entry, correlation, "")
	})
	if err != nil {
		return nil, err
	}
	return wallet, nil
}

// Process shares one implementation for future HTTP and SQS adapters. Provider
// authentication is deliberately outside this use case and must precede it.
func (s *FinancialService) Process(ctx context.Context, c ProcessCommand) (ProcessResult, error) {
	transaction, hash, err := prepareOperation(c)
	if err != nil {
		return ProcessResult{}, err
	}
	var outcome ProcessResult
	err = s.store.WithTx(ctx, func(r *postgres.Repositories) error {
		return processOperation(ctx, r, c, transaction, hash, &outcome)
	})
	if err != nil {
		return ProcessResult{}, err
	}
	return outcome, nil
}

func prepareOperation(c ProcessCommand) (*domain.WagerTransaction, string, error) {
	if c.Kind != domain.Bet && c.Kind != domain.Win && c.Kind != domain.Loss && c.Kind != domain.Refund {
		return nil, "", ErrUnsupportedOperation
	}
	if c.ReferenceExternalTransactionID != "" && c.Kind != domain.Refund {
		return nil, "", ErrUnsupportedOperation
	}
	if !eventIdentity(c.CorrelationID, c.CausationID) {
		return nil, "", fmt.Errorf("invalid correlation or causation identity")
	}
	hash, err := PayloadHash(c)
	if err != nil {
		return nil, "", err
	}
	id, err := newID()
	if err != nil {
		return nil, "", err
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	transaction, err := domain.NewWagerTransaction(domain.TransactionData{
		ID: id, ProviderID: c.ProviderID, ExternalTransactionID: c.ExternalTransactionID, IdempotencyKey: c.IdempotencyKey, PayloadHash: hash,
		WalletID: c.WalletID, PlayerID: c.PlayerID, RoundID: c.RoundID, GameID: c.GameID, Kind: c.Kind, Money: c.Money,
		ReferenceExternalTransactionID: c.ReferenceExternalTransactionID, CorrelationID: c.CorrelationID, CausationID: c.CausationID,
	}, at)
	if err != nil {
		return nil, "", err
	}
	return transaction, hash, nil
}

// processOperation contains the existing financial rules. Both entry paths use
// these exact repositories and never open a second top-level transaction here.
func processOperation(ctx context.Context, r *postgres.Repositories, c ProcessCommand, transaction *domain.WagerTransaction, hash string, outcome *ProcessResult) error {
	at := transaction.Snapshot().CreatedAt
	existing, err := r.Transactions.Identity(ctx, c.ProviderID, c.IdempotencyKey, c.ExternalTransactionID)
	if err == nil {
		return replay(existing, c, hash, outcome)
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return err
	}
	// Lock first: an inserted transaction's FK takes KEY SHARE on the wallet.
	// Acquiring FOR UPDATE afterwards could deadlock competing writers.
	wallet, err := r.Wallets.Lock(ctx, c.WalletID)
	if err != nil {
		return err
	}
	before := wallet.Snapshot()
	if before.PlayerID != c.PlayerID || before.Currency != c.Money.Currency() {
		return ErrWalletIdentity
	}
	inserted, err := r.Transactions.Insert(ctx, transaction)
	if err != nil {
		return err
	}
	if !inserted {
		// Separate statement: READ COMMITTED now observes the committed winner.
		existing, err := r.Transactions.Identity(ctx, c.ProviderID, c.IdempotencyKey, c.ExternalTransactionID)
		if err != nil {
			return err
		}
		return replay(existing, c, hash, outcome)
	}
	// Capture time after acquiring the lock, avoiding a stale timestamp after waiting.
	at = time.Now().UTC().Truncate(time.Microsecond)
	if at.Before(before.UpdatedAt) {
		at = before.UpdatedAt
	}
	var referenceID string
	if c.Kind == domain.Refund {
		referenceID, err = prepareRefundReference(ctx, r, transaction, before, at)
		if err != nil {
			return err
		}
		if transaction.Snapshot().Status != domain.Pending {
			outcome.Transaction = transaction.Snapshot()
			return nil
		}
	}
	switch c.Kind {
	case domain.Bet:
		err = wallet.Debit(c.Money, at)
	case domain.Win, domain.Refund:
		err = wallet.Credit(c.Money, at)
	case domain.Loss: // Never call financial wallet methods for LOSS.
	}
	if errors.Is(err, domain.ErrInsufficientBalance) {
		result := domain.FinancialResult{Balance: before.Balance, WalletVersion: before.Version}
		if err := transaction.Reject(domain.FailureInsufficientBalance, &result, at); err != nil {
			return err
		}
		if err := r.Transactions.Complete(ctx, transaction); err != nil {
			return err
		}
		if err := recordRejected(ctx, r, transaction, c.CorrelationID, c.CausationID); err != nil {
			return err
		}
		outcome.Transaction = transaction.Snapshot()
		return nil
	}
	if err != nil {
		return err
	}
	after := wallet.Snapshot()
	if err := transaction.MarkProcessed(domain.FinancialResult{Balance: after.Balance, WalletVersion: after.Version}, referenceID, at); err != nil {
		return err
	}
	if err := r.Transactions.Complete(ctx, transaction); err != nil {
		return err
	}
	var entry *domain.WalletLedgerEntry
	if c.Kind != domain.Loss {
		if err := r.Wallets.Update(ctx, wallet, before.Version); err != nil {
			return err
		}
		direction, err := transaction.Movement()
		if err != nil {
			return err
		}
		created, err := newEntry(transaction, direction, before.Balance, after.Balance, at)
		if err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, created); err != nil {
			return err
		}
		entry = &created
	}
	if err := recordProcessed(ctx, r, transaction, entry, c.CorrelationID, c.CausationID); err != nil {
		return err
	}
	outcome.Transaction = transaction.Snapshot()
	return nil
}

func replay(existing *domain.WagerTransaction, c ProcessCommand, hash string, outcome *ProcessResult) error {
	state := existing.Snapshot()
	if state.Data.IdempotencyKey != c.IdempotencyKey || state.Data.ExternalTransactionID != c.ExternalTransactionID || state.Data.PayloadHash != hash {
		return postgres.ErrConflict
	}
	if !state.Status.Terminal() && state.Status != domain.PendingReference {
		return fmt.Errorf("persisted operation is pending; recovery is not implemented")
	}
	*outcome = ProcessResult{Transaction: state, IdempotentReplay: true}
	return nil
}

func newEntry(tx *domain.WagerTransaction, direction domain.Direction, before, after domain.Money, at time.Time) (domain.WalletLedgerEntry, error) {
	id, err := newID()
	if err != nil {
		return domain.WalletLedgerEntry{}, err
	}
	s := tx.Snapshot()
	return domain.NewWalletLedgerEntry(domain.LedgerEntryState{ID: id, WalletID: s.Data.WalletID, TransactionID: s.Data.ID,
		Direction: direction, Money: s.Data.Money, BalanceBefore: before, BalanceAfter: after, CreatedAt: at})
}

func recordProcessed(ctx context.Context, r *postgres.Repositories, tx *domain.WagerTransaction, entry *domain.WalletLedgerEntry, correlation, causation string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	event, err := domain.NewWagerTransactionProcessedEvent(id, correlation, causation, tx)
	if err != nil {
		return err
	}
	if err := insertEvent(ctx, r, event.EventHeader, event); err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	id, err = newID()
	if err != nil {
		return err
	}
	changed, err := domain.NewWalletBalanceChangedEvent(id, correlation, causation, *entry, tx.Snapshot().Result.WalletVersion)
	if err != nil {
		return err
	}
	return insertEvent(ctx, r, changed.EventHeader, changed)
}

func recordRejected(ctx context.Context, r *postgres.Repositories, tx *domain.WagerTransaction, correlation, causation string) error {
	id, err := newID()
	if err != nil {
		return err
	}
	event, err := domain.NewWagerTransactionRejectedEvent(id, correlation, causation, tx)
	if err != nil {
		return err
	}
	return insertEvent(ctx, r, event.EventHeader, event)
}

func insertEvent(ctx context.Context, r *postgres.Repositories, header domain.EventHeader, event any) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return r.Outbox.Insert(ctx, postgres.OutboxRecord{EventID: header.EventID, AggregateID: header.AggregateID, EventType: header.EventType, Payload: payload, OccurredAt: header.OccurredAt})
}
