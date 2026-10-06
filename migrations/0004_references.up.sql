ALTER TABLE wager_transactions ADD COLUMN correlation_id TEXT
    CHECK (correlation_id IS NULL OR btrim(correlation_id) <> '');
ALTER TABLE wager_transactions ADD COLUMN causation_id TEXT
    CHECK (causation_id IS NULL OR btrim(causation_id) <> '');

CREATE UNIQUE INDEX one_processed_refund_per_reference
    ON wager_transactions(reference_transaction_id)
    WHERE kind='REFUND' AND status='PROCESSED';
CREATE INDEX pending_reference_order ON wager_transactions(created_at,id)
    WHERE status='PENDING_REFERENCE';

CREATE FUNCTION validate_processed_refund() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE original wager_transactions%ROWTYPE;
BEGIN
    IF NEW.kind <> 'REFUND' OR NEW.status <> 'PROCESSED' THEN RETURN NEW; END IF;
    SELECT * INTO original FROM wager_transactions WHERE id=NEW.reference_transaction_id;
    IF NOT FOUND OR original.kind <> 'BET' OR original.status <> 'PROCESSED'
       OR ROW(original.provider_id,original.external_transaction_id,original.player_id,
              original.wallet_id,original.currency,original.round_id,original.amount_minor_units)
          IS DISTINCT FROM
          ROW(NEW.provider_id,NEW.reference_external_transaction_id,NEW.player_id,
              NEW.wallet_id,NEW.currency,NEW.round_id,NEW.amount_minor_units) THEN
        RAISE EXCEPTION 'processed refund requires matching integral processed BET'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER processed_refund_reference BEFORE INSERT OR UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION validate_processed_refund();

ALTER TABLE inbox_messages DROP CONSTRAINT inbox_messages_status_check;
ALTER TABLE inbox_messages DROP CONSTRAINT inbox_messages_check1;
ALTER TABLE inbox_messages ADD CONSTRAINT inbox_messages_status_check
    CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED'));
ALTER TABLE inbox_messages ADD CONSTRAINT inbox_messages_check1 CHECK (
    (status='PENDING' AND processed_at IS NULL AND transaction_id IS NULL AND failure_code IS NULL)
    OR (status IN ('PROCESSED','PENDING_REFERENCE') AND processed_at IS NOT NULL
        AND transaction_id IS NOT NULL AND failure_code IS NULL)
    OR (status='REJECTED' AND processed_at IS NOT NULL AND failure_code IS NOT NULL AND failure_code IN
        ('INSUFFICIENT_BALANCE','INVALID_INPUT','INVALID_MONEY','UNSUPPORTED_OPERATION',
         'IDEMPOTENCY_CONFLICT','WALLET_NOT_FOUND','WALLET_IDENTITY','ARITHMETIC_OVERFLOW',
         'INVALID_REFERENCE','REFERENCE_NOT_PROCESSED','REVERSAL_CONFLICT'))
);

CREATE OR REPLACE FUNCTION protect_inbox_resolution() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'inbox resolution cannot be deleted' USING ERRCODE='55000';
    END IF;
    IF OLD.status <> 'PENDING' OR NEW.status NOT IN ('PROCESSED','REJECTED','PENDING_REFERENCE')
       OR ROW(NEW.consumer_name,NEW.source,NEW.message_id,NEW.payload_hash,NEW.correlation_id,NEW.received_at)
          IS DISTINCT FROM ROW(OLD.consumer_name,OLD.source,OLD.message_id,OLD.payload_hash,OLD.correlation_id,OLD.received_at) THEN
        RAISE EXCEPTION 'inbox identity and terminal resolution are immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END;
$$;

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
