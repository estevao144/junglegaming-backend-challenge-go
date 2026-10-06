CREATE TABLE wallets (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    player_id TEXT NOT NULL CHECK (btrim(player_id) <> ''),
    currency TEXT NOT NULL CHECK (currency = 'BRL'),
    balance_minor_units BIGINT NOT NULL CHECK (balance_minor_units >= 0),
    version BIGINT NOT NULL CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    UNIQUE (player_id, currency),
    UNIQUE (id, player_id, currency)
);

CREATE TABLE wager_transactions (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    external_transaction_id TEXT,
    provider_id TEXT,
    idempotency_key TEXT,
    payload_hash TEXT,
    wallet_id TEXT NOT NULL,
    player_id TEXT NOT NULL,
    round_id TEXT,
    game_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
    amount_minor_units BIGINT NOT NULL,
    currency TEXT NOT NULL CHECK (currency = 'BRL'),
    reference_external_transaction_id TEXT,
    reference_transaction_id TEXT REFERENCES wager_transactions(id),
    status TEXT NOT NULL CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
    failure_code TEXT,
    result_balance_minor_units BIGINT CHECK (result_balance_minor_units >= 0),
    result_wallet_version BIGINT CHECK (result_wallet_version >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    FOREIGN KEY (wallet_id, player_id, currency) REFERENCES wallets(id, player_id, currency),
    UNIQUE (provider_id, external_transaction_id),
    UNIQUE (provider_id, idempotency_key),
    UNIQUE (id, wallet_id, currency),
    CHECK ((kind = 'LOSS' AND amount_minor_units = 0) OR (kind <> 'LOSS' AND amount_minor_units > 0)),
    CHECK (
        (kind = 'OPENING' AND provider_id IS NULL AND external_transaction_id IS NULL
         AND idempotency_key IS NULL AND payload_hash IS NULL AND round_id IS NULL
         AND game_id IS NULL AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL)
        OR
        (kind <> 'OPENING' AND provider_id IS NOT NULL AND btrim(provider_id) <> ''
         AND external_transaction_id IS NOT NULL AND btrim(external_transaction_id) <> ''
         AND idempotency_key IS NOT NULL AND btrim(idempotency_key) <> ''
         AND payload_hash IS NOT NULL AND payload_hash ~ '^[0-9a-f]{64}$'
         AND round_id IS NOT NULL AND btrim(round_id) <> '' AND game_id IS NOT NULL AND btrim(game_id) <> '')
    ),
    CHECK (kind NOT IN ('REFUND','ROLLBACK') OR
        (reference_external_transaction_id IS NOT NULL AND btrim(reference_external_transaction_id) <> '')),
    CHECK (reference_external_transaction_id IS NULL OR
        (kind IN ('WIN','REFUND','ROLLBACK') AND btrim(reference_external_transaction_id) <> ''
         AND reference_external_transaction_id <> external_transaction_id)),
    CHECK (reference_transaction_id IS NULL OR
        (reference_external_transaction_id IS NOT NULL AND reference_transaction_id <> id)),
    CHECK ((result_balance_minor_units IS NULL) = (result_wallet_version IS NULL)),
    CHECK (
        (status = 'PENDING' AND failure_code IS NULL AND result_balance_minor_units IS NULL AND reference_transaction_id IS NULL)
        OR (status = 'PENDING_REFERENCE' AND failure_code IS NULL AND result_balance_minor_units IS NULL
            AND reference_external_transaction_id IS NOT NULL AND reference_transaction_id IS NULL)
        OR (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor_units IS NOT NULL
            AND (reference_external_transaction_id IS NULL OR reference_transaction_id IS NOT NULL))
        OR (status = 'REJECTED' AND failure_code IS NOT NULL AND failure_code IN
            ('INSUFFICIENT_BALANCE','REVERSAL_INSUFFICIENT_BALANCE','REFERENCE_NOT_FOUND',
             'REFERENCE_NOT_PROCESSED','INVALID_REFERENCE','REVERSAL_CONFLICT'))
        OR (status = 'FAILED' AND failure_code IS NOT NULL AND failure_code = 'INFRASTRUCTURE_PERMANENT')
    ),
    CHECK (kind <> 'OPENING' OR status <> 'PROCESSED' OR
        (result_balance_minor_units = amount_minor_units AND result_wallet_version = 1))
);
CREATE UNIQUE INDEX one_opening_per_wallet ON wager_transactions(wallet_id) WHERE kind = 'OPENING';
CREATE UNIQUE INDEX one_financial_operation_per_wallet_version
    ON wager_transactions(wallet_id, result_wallet_version)
    WHERE status = 'PROCESSED' AND kind <> 'LOSS';

CREATE TABLE wallet_ledger_entries (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    wallet_id TEXT NOT NULL REFERENCES wallets(id),
    transaction_id TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units > 0),
    currency TEXT NOT NULL CHECK (currency = 'BRL'),
    balance_before_minor_units BIGINT NOT NULL CHECK (balance_before_minor_units >= 0),
    balance_after_minor_units BIGINT NOT NULL CHECK (balance_after_minor_units >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (transaction_id, wallet_id, currency) REFERENCES wager_transactions(id, wallet_id, currency),
    UNIQUE (wallet_id, transaction_id),
    CHECK ((direction = 'CREDIT' AND balance_after_minor_units::NUMERIC = balance_before_minor_units::NUMERIC + amount_minor_units::NUMERIC)
        OR (direction = 'DEBIT' AND balance_after_minor_units::NUMERIC = balance_before_minor_units::NUMERIC - amount_minor_units::NUMERIC))
);
CREATE INDEX ledger_wallet_order ON wallet_ledger_entries(wallet_id, created_at, id);

CREATE FUNCTION validate_ledger_transaction() RETURNS TRIGGER LANGUAGE plpgsql AS $$
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
CREATE TRIGGER ledger_matches_transaction BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION validate_ledger_transaction();

-- Deferred checks see all writes at COMMIT and use indexed identities.
CREATE FUNCTION validate_wallet_movement() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.version <> 1 THEN
            RAISE EXCEPTION 'wallet starts at version 1' USING ERRCODE = '23514';
        END IF;
        IF NEW.balance_minor_units > 0 AND NOT EXISTS (
            SELECT 1 FROM wager_transactions t JOIN wallet_ledger_entries l ON l.transaction_id = t.id
            WHERE t.wallet_id = NEW.id AND t.kind = 'OPENING' AND t.status = 'PROCESSED'
              AND t.result_wallet_version = 1 AND l.balance_before_minor_units = 0
              AND l.balance_after_minor_units = NEW.balance_minor_units
        ) THEN
            RAISE EXCEPTION 'positive opening requires its ledger' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.balance_minor_units = OLD.balance_minor_units THEN
        IF NEW.version <> OLD.version THEN
            RAISE EXCEPTION 'version changes only with balance' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF NEW.version::numeric <> OLD.version::numeric + 1 OR NOT EXISTS (
            SELECT 1 FROM wager_transactions t JOIN wallet_ledger_entries l ON l.transaction_id = t.id
            WHERE t.wallet_id = NEW.id AND t.status = 'PROCESSED' AND t.kind <> 'LOSS'
              AND t.result_wallet_version = NEW.version AND l.wallet_id = NEW.id
              AND l.balance_before_minor_units = OLD.balance_minor_units
              AND l.balance_after_minor_units = NEW.balance_minor_units
        ) THEN
            RAISE EXCEPTION 'balance change requires next version and matching ledger' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER wallet_movement_consistency AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_wallet_movement();

CREATE FUNCTION validate_processed_ledger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    current_wallet wallets%ROWTYPE;
BEGIN
    IF NEW.status <> 'PROCESSED' THEN RETURN NULL; END IF;
    SELECT * INTO current_wallet FROM wallets WHERE id = NEW.wallet_id;
    IF NEW.result_wallet_version > current_wallet.version
       OR (NEW.result_wallet_version = current_wallet.version AND NEW.result_balance_minor_units <> current_wallet.balance_minor_units) THEN
        RAISE EXCEPTION 'processed result must match wallet history' USING ERRCODE = '23514';
    END IF;
    IF NEW.kind <> 'LOSS' THEN
        IF (NEW.kind <> 'OPENING' AND NEW.result_wallet_version < 2) OR NOT EXISTS (
            SELECT 1 FROM wallet_ledger_entries WHERE transaction_id = NEW.id AND wallet_id = NEW.wallet_id
        ) THEN
            RAISE EXCEPTION 'processed financial operation requires ledger' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER processed_ledger_consistency AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_processed_ledger();

CREATE FUNCTION protect_ledger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger is append-only' USING ERRCODE = '55000';
END;
$$;
CREATE TRIGGER ledger_immutable BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION protect_ledger();
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION protect_ledger();

CREATE FUNCTION protect_wager_transaction() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'transactions cannot be deleted' USING ERRCODE = '55000';
    END IF;
    IF NOT ((OLD.status = 'PENDING' AND NEW.status IN ('PENDING_REFERENCE','PROCESSED','REJECTED','FAILED'))
        OR (OLD.status = 'PENDING_REFERENCE' AND NEW.status IN ('PROCESSED','REJECTED','FAILED'))) THEN
        RAISE EXCEPTION 'invalid transaction transition' USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status','failure_code','reference_transaction_id','result_balance_minor_units','result_wallet_version','updated_at'])
        IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status','failure_code','reference_transaction_id','result_balance_minor_units','result_wallet_version','updated_at']) THEN
        RAISE EXCEPTION 'transaction input is immutable' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER transaction_protection BEFORE UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION protect_wager_transaction();

CREATE TABLE outbox_events (
    event_id TEXT PRIMARY KEY CHECK (btrim(event_id) <> ''),
    aggregate_id TEXT NOT NULL CHECK (btrim(aggregate_id) <> ''),
    event_type TEXT NOT NULL CHECK (btrim(event_type) <> ''),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    occurred_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    locked_by TEXT,
    locked_until TIMESTAMPTZ,
    CHECK ((locked_by IS NULL) = (locked_until IS NULL)),
    CHECK (payload->>'eventId' IS NOT NULL AND payload->>'eventId' = event_id),
    CHECK (payload->>'eventType' IS NOT NULL AND payload->>'eventType' = event_type),
    CHECK (payload->>'aggregateId' IS NOT NULL AND payload->>'aggregateId' = aggregate_id)
);
CREATE INDEX outbox_pending ON outbox_events(next_attempt_at, event_id) WHERE published_at IS NULL;

CREATE FUNCTION protect_outbox_snapshot() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.event_id, NEW.aggregate_id, NEW.event_type, NEW.payload, NEW.occurred_at)
       IS DISTINCT FROM ROW(OLD.event_id, OLD.aggregate_id, OLD.event_type, OLD.payload, OLD.occurred_at) THEN
        RAISE EXCEPTION 'outbox snapshot is immutable' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER outbox_snapshot_immutable BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION protect_outbox_snapshot();
