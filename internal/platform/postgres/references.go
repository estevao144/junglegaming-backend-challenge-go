package postgres

import (
	"context"
	"jungle-gaming/internal/domain"
)

// Reference uses the provider-scoped external identity, never a global lookup.
// Call after the wallet lock so a preceding BET commit is visible.
func (r TransactionRepository) Reference(ctx context.Context, provider, external string) (*domain.WagerTransaction, error) {
	return scanTransaction(r.tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions
		WHERE provider_id=$1 AND external_transaction_id=$2`, provider, external))
}

func (r TransactionRepository) HasProcessedReversal(ctx context.Context, referenceID string) (bool, error) {
	var exists bool
	err := r.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wager_transactions
		WHERE reference_transaction_id=$1 AND kind IN ('REFUND','ROLLBACK') AND status='PROCESSED')`, referenceID).Scan(&exists)
	return exists, err
}

func (r TransactionRepository) Get(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	return scanTransaction(r.tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id=$1`, id))
}

// Lock must follow the wallet lock; the wallet ID cannot change after creation.
func (r TransactionRepository) Lock(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	return scanTransaction(r.tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id=$1 FOR UPDATE`, id))
}
