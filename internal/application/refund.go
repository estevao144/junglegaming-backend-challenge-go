package application

import (
	"context"
	"errors"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"time"
)

// prepareRefundReference runs under the wallet lock. It either leaves a valid
// REFUND ready to credit or durably resolves it as pending/rejected, without money.
func prepareRefundReference(ctx context.Context, r *postgres.Repositories, transaction *domain.WagerTransaction, wallet domain.WalletState, at time.Time) (string, error) {
	data := transaction.Snapshot().Data
	reference, err := r.Transactions.Reference(ctx, data.ProviderID, data.ReferenceExternalTransactionID)
	if errors.Is(err, postgres.ErrNotFound) {
		if err := transaction.MarkPendingReference(at); err != nil {
			return "", err
		}
		if err := r.Transactions.Complete(ctx, transaction); err != nil {
			return "", err
		}
		id, err := newID()
		if err != nil {
			return "", err
		}
		event, err := domain.NewWagerTransactionPendingReferenceEvent(id, data.CorrelationID, data.CausationID, transaction)
		if err != nil {
			return "", err
		}
		return "", insertEvent(ctx, r, event.EventHeader, event)
	}
	if err != nil {
		return "", err
	}
	state := reference.Snapshot()
	failure := transaction.RefundReferenceFailure(state)
	if failure == "" {
		refunded, err := r.Transactions.HasProcessedRefund(ctx, state.Data.ID)
		if err != nil {
			return "", err
		}
		if refunded {
			failure = domain.FailureReversalConflict
		}
	}
	if failure != "" {
		result := domain.FinancialResult{Balance: wallet.Balance, WalletVersion: wallet.Version}
		if err := transaction.RejectReference(failure, result, state.Data.ID, at); err != nil {
			return "", err
		}
		if err := r.Transactions.Complete(ctx, transaction); err != nil {
			return "", err
		}
		return state.Data.ID, recordRejected(ctx, r, transaction, data.CorrelationID, data.CausationID)
	}
	return state.Data.ID, nil
}
