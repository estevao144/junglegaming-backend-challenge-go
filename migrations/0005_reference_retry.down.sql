DROP TABLE reference_retries;
DROP INDEX one_processed_reversal_per_reference;
DROP TRIGGER processed_rollback_reference ON wager_transactions;
DROP FUNCTION validate_processed_rollback();
CREATE OR REPLACE FUNCTION validate_ledger_transaction() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO operation FROM wager_transactions WHERE id = NEW.transaction_id;
    IF operation.status <> 'PROCESSED' OR operation.kind = 'LOSS'
       OR operation.amount_minor_units <> NEW.amount_minor_units
       OR operation.result_balance_minor_units <> NEW.balance_after_minor_units
       OR (operation.kind = 'BET' AND NEW.direction <> 'DEBIT')
       OR (operation.kind IN ('OPENING','WIN','REFUND') AND NEW.direction <> 'CREDIT') THEN
        RAISE EXCEPTION 'ledger must match processed financial operation' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
ALTER TABLE inbox_messages DROP CONSTRAINT inbox_messages_check1;
ALTER TABLE inbox_messages ADD CONSTRAINT inbox_messages_check1 CHECK (
    (status='PENDING' AND processed_at IS NULL AND transaction_id IS NULL AND failure_code IS NULL)
    OR (status IN ('PROCESSED','PENDING_REFERENCE') AND processed_at IS NOT NULL
        AND transaction_id IS NOT NULL AND failure_code IS NULL)
    OR (status='REJECTED' AND processed_at IS NOT NULL AND failure_code IS NOT NULL AND failure_code IN
        ('INSUFFICIENT_BALANCE','INVALID_INPUT','INVALID_MONEY','UNSUPPORTED_OPERATION',
         'IDEMPOTENCY_CONFLICT','WALLET_NOT_FOUND','WALLET_IDENTITY','ARITHMETIC_OVERFLOW',
         'INVALID_REFERENCE','REFERENCE_NOT_PROCESSED','REVERSAL_CONFLICT'))
) NOT VALID;
CREATE OR REPLACE FUNCTION validate_inbox_resolution() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    resolution inbox_messages%ROWTYPE;
    operation wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO resolution FROM inbox_messages
      WHERE consumer_name=NEW.consumer_name AND source=NEW.source AND message_id=NEW.message_id;
    IF resolution.status='PENDING' THEN
        RAISE EXCEPTION 'inbox must be resolved in its transaction' USING ERRCODE='23514';
    END IF;
    IF resolution.transaction_id IS NOT NULL THEN
        SELECT * INTO operation FROM wager_transactions WHERE id=resolution.transaction_id;
        -- Inbox records the durable resolution of this delivery. Its immutable
        -- pending snapshot must not prohibit future financial resolution in 5B.
        IF resolution.status='PENDING_REFERENCE' THEN
            IF operation.kind <> 'REFUND' OR operation.status NOT IN
                ('PENDING_REFERENCE','PROCESSED','REJECTED','FAILED') THEN
                RAISE EXCEPTION 'pending inbox requires durable refund reference state' USING ERRCODE='23514';
            END IF;
        ELSIF operation.status <> resolution.status
           OR (resolution.status='REJECTED' AND operation.failure_code IS DISTINCT FROM resolution.failure_code) THEN
            RAISE EXCEPTION 'inbox must match terminal financial result' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
