package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"jungle-gaming/internal/domain"
)

var (
	ErrNotFound         = errors.New("record not found")
	ErrConflict         = errors.New("persistent identity conflict")
	ErrConcurrentChange = errors.New("wallet version changed")
)

type Store struct{ db *Database }

func NewStore(db *Database) *Store { return &Store{db: db} }

type WalletRepository struct{ tx pgx.Tx }
type TransactionRepository struct{ tx pgx.Tx }
type LedgerRepository struct{ tx pgx.Tx }
type OutboxRepository struct{ tx pgx.Tx }

type Repositories struct {
	tx               pgx.Tx
	Inbox            InboxRepository
	Wallets          WalletRepository
	Transactions     TransactionRepository
	Ledger           LedgerRepository
	Outbox           OutboxRepository
	ReferenceRetries ReferenceRetryRepository
}

// WithTx owns BEGIN/COMMIT/ROLLBACK. All repositories use this exact pgx.Tx.
func (s *Store) WithTx(ctx context.Context, work func(*Repositories) error) error {
	tx, err := s.db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	repos := repositoriesFor(tx)
	if err := work(repos); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("financial commit: %w", err)
	}
	return nil
}

func repositoriesFor(tx pgx.Tx) *Repositories {
	return &Repositories{tx: tx, Inbox: InboxRepository{tx}, Wallets: WalletRepository{tx}, Transactions: TransactionRepository{tx}, Ledger: LedgerRepository{tx}, Outbox: OutboxRepository{tx}, ReferenceRetries: ReferenceRetryRepository{tx}}
}

// WithSavepoint rolls back financial writes on a terminal input error, while
// allowing the surrounding inbox transaction to durably record that rejection.
func (r *Repositories) WithSavepoint(ctx context.Context, work func(*Repositories) error) error {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if err := work(repositoriesFor(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx) // RELEASE SAVEPOINT, not the outer financial COMMIT.
}

func classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrConflict, pgError.ConstraintName)
	}
	return err
}

const walletColumns = `id, player_id, currency, balance_minor_units, version, created_at, updated_at`

func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var state domain.WalletState
	var units int64
	if err := row.Scan(&state.ID, &state.PlayerID, &state.Currency, &units, &state.Version, &state.CreatedAt, &state.UpdatedAt); err != nil {
		return nil, classify(err)
	}
	money, err := domain.MoneyFromMinorUnits(units, state.Currency)
	if err != nil {
		return nil, err
	}
	state.Balance = money
	return domain.RehydrateWallet(state)
}

func (s *Store) GetWallet(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(s.db.pool.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id=$1`, id))
}

func (r WalletRepository) Lock(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(r.tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id=$1 FOR UPDATE`, id))
}

func (r WalletRepository) Insert(ctx context.Context, wallet *domain.Wallet) error {
	s := wallet.Snapshot()
	_, err := r.tx.Exec(ctx, `INSERT INTO wallets (`+walletColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7)`, s.ID, s.PlayerID, s.Currency, s.Balance.MinorUnits(), s.Version, s.CreatedAt, s.UpdatedAt)
	return classify(err)
}

func (r WalletRepository) Update(ctx context.Context, wallet *domain.Wallet, previousVersion int64) error {
	s := wallet.Snapshot()
	result, err := r.tx.Exec(ctx, `UPDATE wallets SET balance_minor_units=$2,version=$3,updated_at=$4 WHERE id=$1 AND version=$5`, s.ID, s.Balance.MinorUnits(), s.Version, s.UpdatedAt, previousVersion)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConcurrentChange
	}
	return nil
}

const transactionColumns = `id, external_transaction_id, provider_id, idempotency_key, payload_hash,
wallet_id, player_id, round_id, game_id, kind, amount_minor_units, currency,
reference_external_transaction_id, reference_transaction_id, status, failure_code,
result_balance_minor_units, result_wallet_version, created_at, updated_at, correlation_id, causation_id`

func optional(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanTransaction(row pgx.Row) (*domain.WagerTransaction, error) {
	var s domain.WagerTransactionState
	var external, provider, key, hash, round, game, reference, internal, failure *string
	var amount int64
	var currency string
	var balance, version *int64
	var correlation, causation *string
	d := &s.Data
	err := row.Scan(&d.ID, &external, &provider, &key, &hash, &d.WalletID, &d.PlayerID, &round, &game, &d.Kind, &amount, &currency, &reference, &internal, &s.Status, &failure, &balance, &version, &s.CreatedAt, &s.UpdatedAt, &correlation, &causation)
	if err != nil {
		return nil, classify(err)
	}
	for _, pair := range []struct {
		source *string
		target *string
	}{
		{external, &d.ExternalTransactionID}, {provider, &d.ProviderID}, {key, &d.IdempotencyKey}, {hash, &d.PayloadHash},
		{round, &d.RoundID}, {game, &d.GameID}, {reference, &d.ReferenceExternalTransactionID}, {internal, &s.ReferenceTransactionID},
		{correlation, &d.CorrelationID}, {causation, &d.CausationID},
	} {
		if pair.source != nil {
			*pair.target = *pair.source
		}
	}
	if failure != nil {
		s.FailureCode = domain.FailureCode(*failure)
	}
	d.Money, err = domain.MoneyFromMinorUnits(amount, currency)
	if err != nil {
		return nil, err
	}
	if balance != nil && version != nil {
		money, err := domain.MoneyFromMinorUnits(*balance, currency)
		if err != nil {
			return nil, err
		}
		s.Result = &domain.FinancialResult{Balance: money, WalletVersion: *version}
	}
	return domain.RehydrateWagerTransaction(s)
}

// Identity reads both unique identities in a new statement snapshot.
func (r TransactionRepository) Identity(ctx context.Context, provider, key, external string) (*domain.WagerTransaction, error) {
	return scanTransaction(r.tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions
		WHERE provider_id=$1 AND (idempotency_key=$2 OR external_transaction_id=$3) ORDER BY id LIMIT 1`, provider, key, external))
}

func (r TransactionRepository) Insert(ctx context.Context, transaction *domain.WagerTransaction) (bool, error) {
	s := transaction.Snapshot()
	d := s.Data
	var balance, version any
	if s.Result != nil {
		balance, version = s.Result.Balance.MinorUnits(), s.Result.WalletVersion
	}
	result, err := r.tx.Exec(ctx, `INSERT INTO wager_transactions (`+transactionColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT DO NOTHING`, d.ID, optional(d.ExternalTransactionID), optional(d.ProviderID), optional(d.IdempotencyKey), optional(d.PayloadHash),
		d.WalletID, d.PlayerID, optional(d.RoundID), optional(d.GameID), d.Kind, d.Money.MinorUnits(), d.Money.Currency(),
		optional(d.ReferenceExternalTransactionID), optional(s.ReferenceTransactionID), s.Status, optional(string(s.FailureCode)), balance, version, s.CreatedAt, s.UpdatedAt, optional(d.CorrelationID), optional(d.CausationID))
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func (r TransactionRepository) Complete(ctx context.Context, transaction *domain.WagerTransaction) error {
	s := transaction.Snapshot()
	var balance, version any
	if s.Result != nil {
		balance, version = s.Result.Balance.MinorUnits(), s.Result.WalletVersion
	}
	result, err := r.tx.Exec(ctx, `UPDATE wager_transactions SET status=$2,failure_code=$3,
		result_balance_minor_units=$4,result_wallet_version=$5,reference_transaction_id=$6,updated_at=$7
		WHERE id=$1 AND status IN ('PENDING','PENDING_REFERENCE')`, s.Data.ID, s.Status, optional(string(s.FailureCode)), balance, version, optional(s.ReferenceTransactionID), s.UpdatedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrInvalidTransition
	}
	return nil
}

func (s *Store) GetTransaction(ctx context.Context, provider, id string) (*domain.WagerTransaction, error) {
	return scanTransaction(s.db.pool.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE provider_id=$1 AND id=$2`, provider, id))
}

func (s *Store) GetExternalTransaction(ctx context.Context, provider, external string) (*domain.WagerTransaction, error) {
	return scanTransaction(s.db.pool.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE provider_id=$1 AND external_transaction_id=$2`, provider, external))
}

func (r LedgerRepository) Insert(ctx context.Context, entry domain.WalletLedgerEntry) error {
	s := entry.Snapshot()
	_, err := r.tx.Exec(ctx, `INSERT INTO wallet_ledger_entries
		(id,wallet_id,transaction_id,direction,amount_minor_units,currency,balance_before_minor_units,balance_after_minor_units,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, s.ID, s.WalletID, s.TransactionID, s.Direction, s.Money.MinorUnits(), s.Money.Currency(), s.BalanceBefore.MinorUnits(), s.BalanceAfter.MinorUnits(), s.CreatedAt)
	return classify(err)
}

func (s *Store) GetLedger(ctx context.Context, walletID string) ([]domain.WalletLedgerEntry, error) {
	rows, err := s.db.pool.Query(ctx, `SELECT id,wallet_id,transaction_id,direction,amount_minor_units,currency,
		balance_before_minor_units,balance_after_minor_units,created_at FROM wallet_ledger_entries WHERE wallet_id=$1 ORDER BY created_at,id`, walletID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []domain.WalletLedgerEntry{}
	for rows.Next() {
		var state domain.LedgerEntryState
		var amount, before, after int64
		var currency string
		if err := rows.Scan(&state.ID, &state.WalletID, &state.TransactionID, &state.Direction, &amount, &currency, &before, &after, &state.CreatedAt); err != nil {
			return nil, err
		}
		state.Money, err = domain.MoneyFromMinorUnits(amount, currency)
		if err != nil {
			return nil, err
		}
		state.BalanceBefore, err = domain.MoneyFromMinorUnits(before, currency)
		if err != nil {
			return nil, err
		}
		state.BalanceAfter, err = domain.MoneyFromMinorUnits(after, currency)
		if err != nil {
			return nil, err
		}
		entry, err := domain.RehydrateWalletLedgerEntry(state)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

type OutboxRecord struct {
	EventID     string
	AggregateID string
	EventType   string
	Payload     []byte
	OccurredAt  time.Time
}

func (r OutboxRepository) Insert(ctx context.Context, event OutboxRecord) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO outbox_events(event_id,aggregate_id,event_type,payload,occurred_at,next_attempt_at)
		VALUES ($1,$2,$3,$4::jsonb,$5,$5)`, event.EventID, event.AggregateID, event.EventType, string(event.Payload), event.OccurredAt)
	return classify(err)
}
