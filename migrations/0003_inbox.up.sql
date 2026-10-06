CREATE TABLE inbox_messages (
    consumer_name TEXT NOT NULL CHECK (btrim(consumer_name) <> ''),
    source TEXT NOT NULL CHECK (btrim(source) <> ''),
    message_id TEXT NOT NULL CHECK (btrim(message_id) <> ''),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    correlation_id TEXT NOT NULL CHECK (btrim(correlation_id) <> ''),
    status TEXT NOT NULL CHECK (status IN ('PENDING','PROCESSED','REJECTED')),
    transaction_id TEXT REFERENCES wager_transactions(id),
    failure_code TEXT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    processed_at TIMESTAMPTZ,
    PRIMARY KEY (consumer_name, source, message_id),
    CHECK (processed_at IS NULL OR processed_at >= received_at),
    CHECK ((status='PENDING' AND processed_at IS NULL AND transaction_id IS NULL AND failure_code IS NULL)
        OR (status='PROCESSED' AND processed_at IS NOT NULL AND transaction_id IS NOT NULL AND failure_code IS NULL)
        OR (status='REJECTED' AND processed_at IS NOT NULL AND failure_code IS NOT NULL AND failure_code IN
            ('INSUFFICIENT_BALANCE','INVALID_INPUT','INVALID_MONEY','UNSUPPORTED_OPERATION',
             'IDEMPOTENCY_CONFLICT','WALLET_NOT_FOUND','WALLET_IDENTITY','ARITHMETIC_OVERFLOW')))
);

CREATE FUNCTION protect_inbox_resolution() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'inbox resolution cannot be deleted' USING ERRCODE='55000';
    END IF;
    IF OLD.status <> 'PENDING' OR NEW.status NOT IN ('PROCESSED','REJECTED')
       OR ROW(NEW.consumer_name,NEW.source,NEW.message_id,NEW.payload_hash,NEW.correlation_id,NEW.received_at)
          IS DISTINCT FROM ROW(OLD.consumer_name,OLD.source,OLD.message_id,OLD.payload_hash,OLD.correlation_id,OLD.received_at) THEN
        RAISE EXCEPTION 'inbox identity and terminal resolution are immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER inbox_resolution_immutable BEFORE UPDATE OR DELETE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION protect_inbox_resolution();

CREATE FUNCTION validate_inbox_resolution() RETURNS TRIGGER LANGUAGE plpgsql AS $$
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
        IF operation.status <> resolution.status
           OR (resolution.status='REJECTED' AND operation.failure_code IS DISTINCT FROM resolution.failure_code) THEN
            RAISE EXCEPTION 'inbox must match terminal financial result' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER inbox_resolution_at_commit AFTER INSERT OR UPDATE ON inbox_messages
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_inbox_resolution();
