-- Transport source remains audit metadata, not an additional identity scope.
-- Existing duplicates must be reviewed before upgrading; never erase history.
ALTER TABLE inbox_messages ADD CONSTRAINT inbox_consumer_message_unique
    UNIQUE (consumer_name,message_id);

CREATE FUNCTION validate_processed_win_reference() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE original wager_transactions%ROWTYPE;
BEGIN
    IF NEW.kind <> 'WIN' OR NEW.status <> 'PROCESSED'
       OR NEW.reference_external_transaction_id IS NULL THEN RETURN NEW; END IF;
    SELECT * INTO original FROM wager_transactions WHERE id=NEW.reference_transaction_id;
    IF NOT FOUND OR original.kind <> 'BET' OR original.status <> 'PROCESSED'
       OR ROW(original.provider_id,original.external_transaction_id,original.player_id,
              original.wallet_id,original.currency,original.round_id)
          IS DISTINCT FROM
          ROW(NEW.provider_id,NEW.reference_external_transaction_id,NEW.player_id,
              NEW.wallet_id,NEW.currency,NEW.round_id) THEN
        RAISE EXCEPTION 'processed referenced WIN requires matching processed BET context'
            USING ERRCODE='23514';
    END IF;
    -- Payout intentionally does not need to equal the BET amount.
    RETURN NEW;
END;
$$;
CREATE TRIGGER processed_win_reference BEFORE INSERT OR UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION validate_processed_win_reference();

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
            IF operation.kind NOT IN ('WIN','REFUND','ROLLBACK') OR operation.status NOT IN
                ('PENDING_REFERENCE','PROCESSED','REJECTED','FAILED') THEN
                RAISE EXCEPTION 'pending inbox requires durable financial reference state' USING ERRCODE='23514';
            END IF;
        ELSIF operation.status <> resolution.status
           OR (resolution.status='REJECTED' AND operation.failure_code IS DISTINCT FROM resolution.failure_code) THEN
            RAISE EXCEPTION 'inbox must match terminal financial result' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
