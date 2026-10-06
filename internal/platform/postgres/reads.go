package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"jungle-gaming/internal/domain"
)

var ErrInvalidCursor = errors.New("invalid ledger cursor")

type ledgerCursor struct {
	WalletID string    `json:"wallet"`
	At       time.Time `json:"at"`
	ID       string    `json:"id"`
}

type LedgerPage struct {
	Entries    []domain.LedgerEntryState
	NextCursor string
}

// The cursor is tied to the wallet and the same indexed ordering as the query.
func decodeLedgerCursor(value, walletID string) (ledgerCursor, error) {
	if value == "" {
		return ledgerCursor{}, nil
	}
	if len(value) > 1024 {
		return ledgerCursor{}, ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return ledgerCursor{}, ErrInvalidCursor
	}
	var cursor ledgerCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.WalletID != walletID || cursor.At.IsZero() || cursor.ID == "" {
		return ledgerCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}

func (s *Store) LedgerPage(ctx context.Context, walletID, after string, limit int) (LedgerPage, error) {
	if limit < 1 || limit > 200 {
		return LedgerPage{}, ErrInvalidCursor
	}
	cursor, err := decodeLedgerCursor(after, walletID)
	if err != nil {
		return LedgerPage{}, err
	}
	rows, err := s.db.pool.Query(ctx, `SELECT id,transaction_id,direction,amount_minor_units,currency,
 balance_before_minor_units,balance_after_minor_units,created_at FROM wallet_ledger_entries
 WHERE wallet_id=$1 AND ($2 OR (created_at,id)>($3,$4)) ORDER BY created_at,id LIMIT $5`,
		walletID, after == "", cursor.At, cursor.ID, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}
	defer rows.Close()
	page := LedgerPage{Entries: []domain.LedgerEntryState{}}
	for rows.Next() {
		entry := domain.LedgerEntryState{WalletID: walletID}
		var amount, before, after int64
		var currency string
		if err := rows.Scan(&entry.ID, &entry.TransactionID, &entry.Direction, &amount, &currency, &before, &after, &entry.CreatedAt); err != nil {
			return LedgerPage{}, err
		}
		entry.Money, err = domain.MoneyFromMinorUnits(amount, currency)
		if err != nil {
			return LedgerPage{}, err
		}
		entry.BalanceBefore, err = domain.MoneyFromMinorUnits(before, currency)
		if err != nil {
			return LedgerPage{}, err
		}
		entry.BalanceAfter, err = domain.MoneyFromMinorUnits(after, currency)
		if err != nil {
			return LedgerPage{}, err
		}
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return LedgerPage{}, err
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		data, err := json.Marshal(ledgerCursor{walletID, last.CreatedAt, last.ID})
		if err != nil {
			return LedgerPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

type Reconciliation struct {
	WalletID          string       `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
	ChainConsistent   bool         `json:"chainConsistent"`
}

// Repeatable read observes wallet and ledger at the same committed snapshot.
// Rows are streamed, ordered by financial version, without locking writers.
func (s *Store) Reconcile(ctx context.Context, id string) (Reconciliation, error) {
	tx, err := s.db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Reconciliation{}, err
	}
	defer tx.Rollback(ctx)
	wallet, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id=$1`, id))
	if err != nil {
		return Reconciliation{}, err
	}
	state := wallet.Snapshot()
	calculated, err := domain.ZeroMoney(state.Currency)
	if err != nil {
		return Reconciliation{}, err
	}
	result := Reconciliation{WalletID: id, StoredBalance: state.Balance, ChainConsistent: true}
	rows, err := tx.Query(ctx, `SELECT l.direction,l.amount_minor_units,l.balance_before_minor_units,l.balance_after_minor_units
 FROM wallet_ledger_entries l JOIN wager_transactions t ON t.id=l.transaction_id
 WHERE l.wallet_id=$1 ORDER BY t.result_wallet_version,l.id`, id)
	if err != nil {
		return Reconciliation{}, err
	}
	for rows.Next() {
		var direction domain.Direction
		var units, before, after int64
		if err := rows.Scan(&direction, &units, &before, &after); err != nil {
			rows.Close()
			return Reconciliation{}, err
		}
		if calculated.MinorUnits() != before {
			result.ChainConsistent = false
		}
		amount, err := domain.MoneyFromMinorUnits(units, state.Currency)
		if err != nil {
			rows.Close()
			return Reconciliation{}, err
		}
		switch direction {
		case domain.CreditDirection:
			calculated, err = calculated.Add(amount)
		case domain.DebitDirection:
			calculated, err = calculated.Subtract(amount)
		default:
			err = fmt.Errorf("invalid ledger direction")
		}
		if err != nil {
			rows.Close()
			return Reconciliation{}, err
		}
		if calculated.MinorUnits() != after {
			result.ChainConsistent = false
		}
		result.CheckedEntries++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Reconciliation{}, err
	}
	result.CalculatedBalance = calculated
	result.Difference, err = state.Balance.Subtract(calculated)
	if err != nil {
		return Reconciliation{}, err
	}
	result.Consistent = result.ChainConsistent && result.Difference.MinorUnits() == 0
	if err := tx.Commit(ctx); err != nil {
		return Reconciliation{}, err
	}
	return result, nil
}

// Oldest pending includes leased events and events waiting for backoff.
func (s *Store) OldestPendingEvent(ctx context.Context) (*time.Time, error) {
	var oldest *time.Time
	err := s.db.pool.QueryRow(ctx, `SELECT min(occurred_at) FROM outbox_events WHERE published_at IS NULL`).Scan(&oldest)
	return oldest, err
}
