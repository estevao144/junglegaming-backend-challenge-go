package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInboxIdentity = errors.New("inbox message identity reused with different source or content")

type InboxRepository struct{ tx pgx.Tx }

type InboxRecord struct {
	ConsumerName  string
	Source        string
	MessageID     string
	PayloadHash   string
	CorrelationID string
	Status        string
	TransactionID string
	FailureCode   string
	ReceivedAt    time.Time
	ProcessedAt   *time.Time
}

func (r InboxRepository) Insert(ctx context.Context, record InboxRecord) (bool, error) {
	result, err := r.tx.Exec(ctx, `INSERT INTO inbox_messages
		(consumer_name,source,message_id,payload_hash,correlation_id,status)
		VALUES ($1,$2,$3,$4,$5,'PENDING') ON CONFLICT DO NOTHING`,
		record.ConsumerName, record.Source, record.MessageID, record.PayloadHash, record.CorrelationID)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func (r InboxRepository) Get(ctx context.Context, consumer, source, message string) (InboxRecord, error) {
	record := InboxRecord{ConsumerName: consumer, Source: source, MessageID: message}
	err := r.tx.QueryRow(ctx, `SELECT source,payload_hash,correlation_id,status,COALESCE(transaction_id,''),
		COALESCE(failure_code,''),received_at,processed_at FROM inbox_messages
		WHERE consumer_name=$1 AND message_id=$2`, consumer, message).Scan(
		&record.Source, &record.PayloadHash, &record.CorrelationID, &record.Status, &record.TransactionID, &record.FailureCode, &record.ReceivedAt, &record.ProcessedAt)
	if err != nil {
		return record, classify(err)
	}
	if record.Source != source {
		return record, ErrInboxIdentity
	}
	return record, nil
}

func (r InboxRepository) Complete(ctx context.Context, record InboxRecord) error {
	result, err := r.tx.Exec(ctx, `UPDATE inbox_messages SET status=$4,transaction_id=$5,
		failure_code=$6,processed_at=clock_timestamp()
		WHERE consumer_name=$1 AND source=$2 AND message_id=$3 AND status='PENDING'`,
		record.ConsumerName, record.Source, record.MessageID, record.Status, optional(record.TransactionID), optional(record.FailureCode))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrInboxIdentity
	}
	return nil
}

func (s *Store) CheckInbox(ctx context.Context) error {
	_, err := s.db.pool.Exec(ctx, `SELECT consumer_name,source,message_id FROM inbox_messages LIMIT 0`)
	return err
}
