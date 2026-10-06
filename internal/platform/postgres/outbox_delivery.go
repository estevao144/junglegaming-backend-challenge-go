package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var ErrClaimLost = errors.New("outbox claim expired or ownership changed")

// OutboxDelivery uses short transactions independently of the financial Store.
type OutboxDelivery struct{ db *Database }

func NewOutboxDelivery(db *Database) *OutboxDelivery { return &OutboxDelivery{db: db} }

func (r *OutboxDelivery) Check(ctx context.Context) error {
	_, err := r.db.pool.Exec(ctx, `SELECT event_id, wallet_id, delivery_order,
		locked_by, locked_until FROM outbox_events LIMIT 0`)
	if err != nil {
		return fmt.Errorf("outbox schema unavailable; apply migrations before starting the API")
	}
	return nil
}

type ClaimedEvent struct {
	OutboxRecord
	WalletID   string
	Attempts   int
	ClaimToken string
}

// Claim commits the lease before returning. Only the earliest unpublished event
// of each wallet can advance, including when its predecessor is backing off.
func (r *OutboxDelivery) Claim(ctx context.Context, limit int, lease time.Duration) ([]ClaimedEvent, error) {
	if limit < 1 || lease <= 0 {
		return nil, fmt.Errorf("invalid outbox claim options")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(random[:])
	tx, err := r.db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	rows, err := tx.Query(ctx, `WITH candidates AS (
		SELECT e.event_id FROM outbox_events e
		WHERE e.published_at IS NULL AND e.next_attempt_at <= clock_timestamp()
		  AND (e.locked_until IS NULL OR e.locked_until <= clock_timestamp())
		  AND NOT EXISTS (
		    SELECT 1 FROM outbox_events previous
		    WHERE previous.wallet_id=e.wallet_id AND previous.published_at IS NULL
		      AND previous.delivery_order < e.delivery_order)
		ORDER BY e.next_attempt_at, e.delivery_order
		LIMIT $1 FOR UPDATE OF e SKIP LOCKED
	)
	UPDATE outbox_events e SET locked_by=$2,
	    locked_until=clock_timestamp()+($3::bigint * interval '1 microsecond')
	FROM candidates WHERE e.event_id=candidates.event_id
	RETURNING e.event_id, e.aggregate_id, e.event_type, e.payload, e.occurred_at, e.wallet_id, e.attempts`, limit, token, lease.Microseconds())
	if err != nil {
		return nil, err
	}
	var events []ClaimedEvent
	for rows.Next() {
		event := ClaimedEvent{ClaimToken: token}
		if err := rows.Scan(&event.EventID, &event.AggregateID, &event.EventType, &event.Payload, &event.OccurredAt, &event.WalletID, &event.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return events, nil
}

// Renew gives each sequential send its full lease, even when a batch waited.
func (r *OutboxDelivery) Renew(ctx context.Context, event ClaimedEvent, lease time.Duration) error {
	result, err := r.db.pool.Exec(ctx, `UPDATE outbox_events
		SET locked_until=clock_timestamp()+($3::bigint * interval '1 microsecond')
		WHERE event_id=$1 AND locked_by=$2 AND published_at IS NULL
		AND locked_until > clock_timestamp()`, event.EventID, event.ClaimToken, lease.Microseconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (r *OutboxDelivery) MarkPublished(ctx context.Context, event ClaimedEvent) error {
	result, err := r.db.pool.Exec(ctx, `UPDATE outbox_events
		SET published_at=clock_timestamp(), locked_by=NULL, locked_until=NULL
		WHERE event_id=$1 AND locked_by=$2 AND published_at IS NULL
		AND locked_until > clock_timestamp()`, event.EventID, event.ClaimToken)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (r *OutboxDelivery) Retry(ctx context.Context, event ClaimedEvent, delay time.Duration) error {
	result, err := r.db.pool.Exec(ctx, `UPDATE outbox_events
		SET attempts=LEAST(attempts::bigint+1,2147483647)::integer,
		    next_attempt_at=clock_timestamp()+($3::bigint * interval '1 microsecond'),
		    locked_by=NULL, locked_until=NULL
		WHERE event_id=$1 AND locked_by=$2 AND published_at IS NULL
		AND locked_until > clock_timestamp()`, event.EventID, event.ClaimToken, delay.Microseconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}
