package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrReferenceClaimLost = errors.New("reference claim expired or ownership changed")

type ReferenceRetryRepository struct{ tx pgx.Tx }
type ReferenceQueue struct{ db *Database }

func NewReferenceQueue(db *Database) *ReferenceQueue { return &ReferenceQueue{db: db} }

type ReferenceClaim struct {
	TransactionID string
	Token         string
}

func (q *ReferenceQueue) Check(ctx context.Context) error {
	_, err := q.db.pool.Exec(ctx, `SELECT transaction_id,attempts,next_attempt_at,claimed_by,claim_until,completed_at FROM reference_retries LIMIT 0`)
	if err != nil {
		return fmt.Errorf("reference retry schema unavailable; apply migrations before starting the API")
	}
	return nil
}

// A single statement commits its leases before returning. It locks retry rows
// only: financial locks belong to the separate resolution transaction.
func (q *ReferenceQueue) Claim(ctx context.Context, limit int, lease time.Duration) ([]ReferenceClaim, error) {
	if limit < 1 || lease < time.Microsecond {
		return nil, fmt.Errorf("invalid reference claim options")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(random[:])
	rows, err := q.db.pool.Query(ctx, `WITH candidates AS (
		SELECT r.transaction_id FROM reference_retries r
		JOIN wager_transactions t ON t.id=r.transaction_id
		WHERE t.status='PENDING_REFERENCE' AND r.completed_at IS NULL
		  AND r.next_attempt_at <= clock_timestamp()
		  AND (r.claim_until IS NULL OR r.claim_until <= clock_timestamp())
		ORDER BY r.next_attempt_at,r.transaction_id LIMIT $1 FOR UPDATE OF r SKIP LOCKED
	)
	UPDATE reference_retries r SET claimed_by=$2,
	  claim_until=clock_timestamp()+($3::bigint * interval '1 microsecond')
	FROM candidates WHERE r.transaction_id=candidates.transaction_id
	RETURNING r.transaction_id`, limit, token, lease.Microseconds())
	if err != nil {
		return nil, err
	}
	claims := []ReferenceClaim{}
	for rows.Next() {
		claim := ReferenceClaim{Token: token}
		if err := rows.Scan(&claim.TransactionID); err != nil {
			rows.Close()
			return nil, err
		}
		claims = append(claims, claim)
	}
	err = rows.Err()
	rows.Close() // completes the implicit transaction before exposing claims
	return claims, err
}

func (r ReferenceRetryRepository) Insert(ctx context.Context, transactionID string) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO reference_retries(transaction_id) VALUES ($1)`, transactionID)
	return err
}

// Lock validates lease against PostgreSQL time after all financial locks. Once
// locked, a competing claim cannot replace this token until the transaction ends.
func (r ReferenceRetryRepository) Lock(ctx context.Context, claim ReferenceClaim) (int, error) {
	var attempts int
	err := r.tx.QueryRow(ctx, `SELECT attempts FROM reference_retries
		WHERE transaction_id=$1 AND completed_at IS NULL FOR UPDATE`, claim.TransactionID).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrReferenceClaimLost
	}
	if err != nil {
		return 0, err
	}
	// Evaluate time AFTER obtaining the row lock: waiting for it may itself
	// exhaust the lease, even if no owner changed the row while we waited.
	var valid bool
	err = r.tx.QueryRow(ctx, `SELECT COALESCE(claimed_by=$2 AND claim_until > clock_timestamp(),false)
		FROM reference_retries WHERE transaction_id=$1`, claim.TransactionID, claim.Token).Scan(&valid)
	if err != nil {
		return 0, err
	}
	if !valid {
		return 0, ErrReferenceClaimLost
	}
	return attempts, nil
}

// Finish records one committed attempt and releases the lease atomically with
// the financial outcome. Completed rows remain available for retry audit.
func (r ReferenceRetryRepository) Finish(ctx context.Context, claim ReferenceClaim, delay time.Duration, terminal bool) error {
	result, err := r.tx.Exec(ctx, `UPDATE reference_retries SET
		attempts=LEAST(attempts::bigint+1,2147483647)::integer,
		next_attempt_at=clock_timestamp()+($3::bigint * interval '1 microsecond'),
		completed_at=CASE WHEN $4 THEN clock_timestamp() ELSE NULL END,
		claimed_by=NULL,claim_until=NULL WHERE transaction_id=$1 AND claimed_by=$2`, claim.TransactionID, claim.Token, delay.Microseconds(), terminal)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrReferenceClaimLost
	}
	return nil
}
