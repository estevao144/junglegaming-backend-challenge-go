package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
)

// IncomingOperation carries transport identity separately from business input.
type IncomingOperation struct {
	ConsumerName  string
	Source        string
	MessageID     string
	PayloadHash   string
	CorrelationID string
	Command       ProcessCommand
	RejectionCode string // Semantic parsing failures, e.g. an invalid Money string.
}

type IncomingResult struct {
	Status          string
	TransactionID   string
	FailureCode     string
	InboxReplay     bool
	FinancialReplay bool
}

// TerminalFailure classifies only known permanent input/business errors.
// Unknown errors, database failures and timeouts always remain retryable.
func TerminalFailure(err error) string {
	switch {
	case errors.Is(err, ErrUnsupportedOperation):
		return "UNSUPPORTED_OPERATION"
	case errors.Is(err, postgres.ErrConflict):
		return "IDEMPOTENCY_CONFLICT"
	case errors.Is(err, postgres.ErrNotFound):
		return "WALLET_NOT_FOUND"
	case errors.Is(err, ErrWalletIdentity):
		return "WALLET_IDENTITY"
	case errors.Is(err, domain.ErrOverflow):
		return "ARITHMETIC_OVERFLOW"
	case errors.Is(err, domain.ErrInvalidMoney), errors.Is(err, domain.ErrInvalidCurrency), errors.Is(err, domain.ErrCurrencyMismatch):
		return "INVALID_MONEY"
	case errors.Is(err, domain.ErrInvalidTransaction):
		return "INVALID_INPUT"
	default:
		return ""
	}
}

func (s *FinancialService) CheckInbox(ctx context.Context) error { return s.store.CheckInbox(ctx) }

// ProcessIncoming commits inbox and new financial effects together. A savepoint
// permits a terminal rejection without retaining partial financial writes.
func (s *FinancialService) ProcessIncoming(ctx context.Context, incoming IncomingOperation) (result IncomingResult, processErr error) {
	defer func() {
		s.observeOperation("sqs", string(incoming.Command.Kind), result.Status, result.InboxReplay || result.FinancialReplay, processErr)
	}()
	for _, identity := range []string{incoming.ConsumerName, incoming.Source, incoming.MessageID, incoming.CorrelationID} {
		if identity == "" || strings.TrimSpace(identity) != identity {
			return IncomingResult{}, fmt.Errorf("invalid incoming transport identity")
		}
	}
	record := postgres.InboxRecord{ConsumerName: incoming.ConsumerName, Source: incoming.Source, MessageID: incoming.MessageID, PayloadHash: incoming.PayloadHash, CorrelationID: incoming.CorrelationID}
	var outcome IncomingResult
	err := s.store.WithTx(ctx, func(r *postgres.Repositories) error {
		inserted, err := r.Inbox.Insert(ctx, record)
		if err != nil {
			return err
		}
		if !inserted {
			existing, err := r.Inbox.Get(ctx, record.ConsumerName, record.Source, record.MessageID)
			if err != nil {
				return err
			}
			if existing.PayloadHash != record.PayloadHash {
				return postgres.ErrInboxIdentity
			}
			outcome = IncomingResult{Status: existing.Status, TransactionID: existing.TransactionID, FailureCode: existing.FailureCode, InboxReplay: true}
			return nil
		}
		failure := incoming.RejectionCode
		var financial ProcessResult
		if failure == "" {
			err = r.WithSavepoint(ctx, func(financialRepos *postgres.Repositories) error {
				transaction, hash, err := prepareOperation(incoming.Command)
				if err != nil {
					return err
				}
				return processOperation(ctx, financialRepos, incoming.Command, transaction, hash, &financial)
			})
			if err != nil {
				failure = TerminalFailure(err)
				if failure == "" {
					return err
				}
			}
		}
		if failure != "" {
			record.Status = "REJECTED"
			record.FailureCode = failure
		} else {
			record.Status = string(financial.Transaction.Status)
			record.TransactionID = financial.Transaction.Data.ID
			record.FailureCode = string(financial.Transaction.FailureCode)
		}
		if err := r.Inbox.Complete(ctx, record); err != nil {
			return err
		}
		outcome = IncomingResult{Status: record.Status, TransactionID: record.TransactionID, FailureCode: record.FailureCode, FinancialReplay: financial.IdempotentReplay}
		return nil
	})
	if err != nil {
		return IncomingResult{}, err
	}
	return outcome, nil
}
