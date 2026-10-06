package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
)

type ReferenceRetryPolicy struct {
	MaxAttempts int
	Base        time.Duration
	Maximum     time.Duration
}

func ReferenceBackoff(attempt int, base, maximum time.Duration) time.Duration {
	delay := base
	for n := 1; n < attempt; n++ {
		if delay >= maximum || delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

// ResolveReference starts a fresh financial transaction. A claim never replaces
// wallet locking, state/reference validation or database uniqueness.
func (s *FinancialService) ResolveReference(ctx context.Context, claim postgres.ReferenceClaim, policy ReferenceRetryPolicy) error {
	if policy.MaxAttempts < 1 || policy.Base <= 0 || policy.Maximum < policy.Base {
		return fmt.Errorf("invalid reference retry policy")
	}
	var outcome *domain.WagerTransaction
	err := s.store.WithTx(ctx, func(r *postgres.Repositories) error {
		observed, err := r.Transactions.Get(ctx, claim.TransactionID)
		if err != nil {
			return err
		}
		wallet, err := r.Wallets.Lock(ctx, observed.Snapshot().Data.WalletID)
		if err != nil {
			return err
		}
		transaction, err := r.Transactions.Lock(ctx, claim.TransactionID)
		if err != nil {
			return err
		}
		outcome = transaction
		state := transaction.Snapshot()
		if state.Status.Terminal() {
			return nil
		}
		if state.Status != domain.PendingReference {
			return domain.ErrInvalidTransition
		}
		attempts, err := r.ReferenceRetries.Lock(ctx, claim)
		if err != nil {
			return err
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if at.Before(wallet.Snapshot().UpdatedAt) {
			at = wallet.Snapshot().UpdatedAt
		}
		if at.Before(state.UpdatedAt) {
			at = state.UpdatedAt
		}
		referenceID, direction, err := prepareReversalReference(ctx, r, transaction, wallet.Snapshot(), at, false)
		if errors.Is(err, postgres.ErrNotFound) {
			if attempts >= policy.MaxAttempts-1 {
				result := domain.FinancialResult{Balance: wallet.Snapshot().Balance, WalletVersion: wallet.Snapshot().Version}
				if err := transaction.Reject(domain.FailureReferenceNotFound, &result, at); err != nil {
					return err
				}
				if err := r.Transactions.Complete(ctx, transaction); err != nil {
					return err
				}
				if err := recordRejected(ctx, r, transaction, state.Data.CorrelationID, state.Data.CausationID); err != nil {
					return err
				}
				return r.ReferenceRetries.Finish(ctx, claim, 0, true)
			}
			delay := ReferenceBackoff(attempts+1, policy.Base, policy.Maximum)
			return r.ReferenceRetries.Finish(ctx, claim, delay, false)
		}
		if err != nil {
			return err
		}
		if transaction.Snapshot().Status == domain.PendingReference {
			if err := applyFinancialMovement(ctx, r, transaction, wallet, referenceID, direction, at); err != nil {
				return err
			}
		}
		return r.ReferenceRetries.Finish(ctx, claim, 0, true)
	})
	if outcome != nil && s.metrics != nil {
		state := outcome.Snapshot()
		s.observeOperation("reference", string(state.Data.Kind), string(state.Status), false, err)
		if err != nil || state.Status == domain.PendingReference {
			s.metrics.Worker("reference", "retry")
		}
	}
	return err
}
