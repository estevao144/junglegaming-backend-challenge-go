DROP TRIGGER processed_win_reference ON wager_transactions;
DROP FUNCTION validate_processed_win_reference();
ALTER TABLE inbox_messages DROP CONSTRAINT inbox_consumer_message_unique;

-- Restore exactly the resolution policy supplied by 0005. The runner removes
-- all earlier versions in this same transaction, including historical inboxes.
CREATE OR REPLACE FUNCTION validate_inbox_resolution() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE resolution inbox_messages%ROWTYPE; operation wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO resolution FROM inbox_messages
      WHERE consumer_name=NEW.consumer_name AND source=NEW.source AND message_id=NEW.message_id;
    IF resolution.status='PENDING' THEN
        RAISE EXCEPTION 'inbox must be resolved in its transaction' USING ERRCODE='23514';
    END IF;
    IF resolution.transaction_id IS NOT NULL THEN
        SELECT * INTO operation FROM wager_transactions WHERE id=resolution.transaction_id;
        IF resolution.status='PENDING_REFERENCE' THEN
            IF operation.kind NOT IN ('REFUND','ROLLBACK') OR operation.status NOT IN
                ('PENDING_REFERENCE','PROCESSED','REJECTED','FAILED') THEN
                RAISE EXCEPTION 'pending inbox requires durable reversal reference state' USING ERRCODE='23514';
            END IF;
        ELSIF operation.status <> resolution.status
           OR (resolution.status='REJECTED' AND operation.failure_code IS DISTINCT FROM resolution.failure_code) THEN
            RAISE EXCEPTION 'inbox must match terminal financial result' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
