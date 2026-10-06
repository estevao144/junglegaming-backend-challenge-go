DROP TRIGGER outbox_delivery_order_immutable ON outbox_events;
DROP FUNCTION protect_outbox_delivery_order();
DROP INDEX outbox_wallet_pending_order;
ALTER TABLE outbox_events DROP CONSTRAINT outbox_wallet_required;
ALTER TABLE outbox_events DROP COLUMN wallet_id;
ALTER TABLE outbox_events DROP COLUMN delivery_order;
