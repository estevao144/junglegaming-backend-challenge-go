-- Shared uniqueness prevents REFUND + direct ROLLBACK of the same BET.
CREATE UNIQUE INDEX one_processed_reversal_per_reference
    ON wager_transactions(reference_transaction_id)
    WHERE kind IN ('REFUND','ROLLBACK') AND status='PROCESSED';

CREATE TABLE reference_retries (
    transaction_id TEXT PRIMARY KEY REFERENCES wager_transactions(id),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    claimed_by TEXT CHECK (claimed_by IS NULL OR btrim(claimed_by) <> ''),
    claim_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    CHECK ((claimed_by IS NULL) = (claim_until IS NULL)),
    CHECK (completed_at IS NULL OR claimed_by IS NULL)
);
CREATE INDEX reference_retry_due ON reference_retries(next_attempt_at,transaction_id) WHERE completed_at IS NULL;
INSERT INTO reference_retries(transaction_id)
    SELECT id FROM wager_transactions WHERE status='PENDING_REFERENCE';

CREATE FUNCTION validate_processed_rollback() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE original wager_transactions%ROWTYPE;
BEGIN
    IF NEW.kind <> 'ROLLBACK' OR NEW.status <> 'PROCESSED' THEN RETURN NEW; END IF;
    SELECT * INTO original FROM wager_transactions WHERE id=NEW.reference_transaction_id;
    IF NOT FOUND OR original.kind NOT IN ('BET','WIN','REFUND') OR original.status <> 'PROCESSED'
       OR ROW(original.provider_id,original.external_transaction_id,original.player_id,
              original.wallet_id,original.currency,original.round_id,original.amount_minor_units)
          IS DISTINCT FROM
          ROW(NEW.provider_id,NEW.reference_external_transaction_id,NEW.player_id,
              NEW.wallet_id,NEW.currency,NEW.round_id,NEW.amount_minor_units) THEN
        RAISE EXCEPTION 'processed rollback requires matching integral processed BET/WIN/REFUND'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER processed_rollback_reference BEFORE INSERT OR UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION validate_processed_rollback();

CREATE OR REPLACE FUNCTION validate_ledger_transaction() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation wager_transactions%ROWTYPE;
    original_kind TEXT;
BEGIN
    SELECT * INTO operation FROM wager_transactions WHERE id=NEW.transaction_id;
    IF operation.kind='ROLLBACK' THEN
        SELECT kind INTO original_kind FROM wager_transactions WHERE id=operation.reference_transaction_id;
    END IF;
    IF operation.status <> 'PROCESSED' OR operation.kind='LOSS'
       OR operation.amount_minor_units <> NEW.amount_minor_units
       OR operation.result_balance_minor_units <> NEW.balance_after_minor_units
       OR (operation.kind='BET' AND NEW.direction <> 'DEBIT')
       OR (operation.kind IN ('OPENING','WIN','REFUND') AND NEW.direction <> 'CREDIT')
       OR (operation.kind='ROLLBACK' AND
           (original_kind IS NULL OR original_kind NOT IN ('BET','WIN','REFUND')
            OR (original_kind='BET' AND NEW.direction <> 'CREDIT')
            OR (original_kind IN ('WIN','REFUND') AND NEW.direction <> 'DEBIT'))) THEN
        RAISE EXCEPTION 'ledger must match processed financial operation' USING ERRCODE='23514';
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
         'INVALID_REFERENCE','REFERENCE_NOT_PROCESSED','REVERSAL_CONFLICT',
         'REVERSAL_INSUFFICIENT_BALANCE','REFERENCE_NOT_FOUND'))
);

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
