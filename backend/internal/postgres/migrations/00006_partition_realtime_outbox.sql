-- +goose Up
-- Upgrade requires stopped writers and an empty outbox, then all consumers must
-- switch together to five partitions. Do not redistribute an active backlog.
LOCK TABLE odysseia.realtime_outbox IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.realtime_outbox) THEN
        RAISE EXCEPTION 'drain the realtime outbox before changing partition routing';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT realtime_partition;
ALTER TABLE odysseia.realtime_outbox DROP COLUMN partition;
-- Partition 0 is control traffic. A canonical scope UUID always maps to the
-- same data partition (1..4), independently of process and event identity.
ALTER TABLE odysseia.realtime_outbox ADD COLUMN partition integer
    GENERATED ALWAYS AS (CASE WHEN method='publish'
        AND payload#>>'{data,scope_id}' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
        THEN 1 + get_byte(uuid_send((payload#>>'{data,scope_id}')::uuid),15) % 4
        ELSE 0 END) STORED NOT NULL;
ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT realtime_partition
    CHECK ((method='publish' AND partition BETWEEN 1 AND 4)
        OR (method='disconnect' AND partition=0));
CREATE INDEX realtime_partition_id ON odysseia.realtime_outbox(partition,id);

-- Transactional notifications only wake the official consumer; polling remains
-- its recovery path. Payloads contain a partition number, never message data.
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

-- +goose Down
LOCK TABLE odysseia.realtime_outbox IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.realtime_outbox) THEN
        RAISE EXCEPTION 'drain the realtime outbox before reverting partition routing';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER notify_realtime_partition ON odysseia.realtime_outbox;
DROP FUNCTION odysseia.notify_realtime_partition();
ALTER TABLE odysseia.realtime_outbox DROP COLUMN partition;
ALTER TABLE odysseia.realtime_outbox ADD COLUMN partition integer NOT NULL DEFAULT 0;
ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT realtime_partition CHECK (partition=0);
