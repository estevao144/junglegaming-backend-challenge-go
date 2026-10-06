package application

import (
	"context"
	"errors"
	"time"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
)

// prepareReversalReference runs under the wallet lock. Missing references enter
// pending only on the original request, never on retries of that same transaction.
func prepareReversalReference(ctx context.Context, r *postgres.Repositories, transaction *domain.WagerTransaction, wallet domain.WalletState, at time.Time, enterPending bool) (string, domain.Direction, error) {
	data := transaction.Snapshot().Data
	reference, err := r.Transactions.Reference(ctx, data.ProviderID, data.ReferenceExternalTransactionID)
	if errors.Is(err, postgres.ErrNotFound) {
		if !enterPending {
			return "", "", postgres.ErrNotFound
		}
		if err := transaction.MarkPendingReference(at); err != nil {
			return "", "", err
		}
		if err := r.Transactions.Complete(ctx, transaction); err != nil {
			return "", "", err
		}
		if err := r.ReferenceRetries.Insert(ctx, data.ID); err != nil {
			return "", "", err
		}
		id, err := newID()
		if err != nil {
			return "", "", err
		}
		event, err := domain.NewWagerTransactionPendingReferenceEvent(id, data.CorrelationID, data.CausationID, transaction)
		if err != nil {
			return "", "", err
		}
		return "", "", insertEvent(ctx, r, event.EventHeader, event)
	}
	if err != nil {
		return "", "", err
	}
	state := reference.Snapshot()
	failure := transaction.ReversalReferenceFailure(state)
	if failure == "" {
		reversed, err := r.Transactions.HasProcessedReversal(ctx, state.Data.ID)
		if err != nil {
			return "", "", err
		}
		if reversed {
			failure = domain.FailureReversalConflict
		}
	}
	if failure != "" {
		result := domain.FinancialResult{Balance: wallet.Balance, WalletVersion: wallet.Version}
		if err := transaction.RejectReference(failure, result, state.Data.ID, at); err != nil {
			return "", "", err
		}
		if err := r.Transactions.Complete(ctx, transaction); err != nil {
			return "", "", err
		}
		return state.Data.ID, "", recordRejected(ctx, r, transaction, data.CorrelationID, data.CausationID)
	}
	direction, err := transaction.ReversalMovement(state)
	return state.Data.ID, direction, err
}
