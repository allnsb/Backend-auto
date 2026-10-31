-- +goose Up
-- NOTIFY serializes notifying transactions on PostgreSQL's global database-0
-- object lock until commit. Independent message streams then wait behind each
-- other's WAL flushes. Keep the durable outbox and its existing five partitions;
-- native consumers use the already configured 20ms polling interval instead.
-- Routing and pending commands are unchanged, so this migration needs no drain.
-- Confirm all consumers poll before upgrading. A busy table fails safely rather
-- than waiting indefinitely for the short DDL lock.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER notify_realtime_partition ON odysseia.realtime_outbox;
DROP FUNCTION odysseia.notify_realtime_partition();

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Rolling back restores the former commit contention. Consumers must also have
-- partition_notification_channel configured to use these optional wake-ups.
-- +goose StatementBegin
CREATE FUNCTION odysseia.notify_realtime_partition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('odysseia_realtime_partition',NEW.partition::text);
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER notify_realtime_partition AFTER INSERT ON odysseia.realtime_outbox
    FOR EACH ROW EXECUTE FUNCTION odysseia.notify_realtime_partition();
