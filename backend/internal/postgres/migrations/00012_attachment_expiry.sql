-- +goose Up
SET LOCAL lock_timeout = '5s';

-- The sweeper marks bound attachments expired and deletes unbound rows.
-- message_attachments stays, so an empty conversation body still has its link.
-- Outbox routing is unchanged and this migration does not rewrite messages.
-- +goose StatementBegin
DO $$
DECLARE
    status_check text;
BEGIN
    SELECT con.conname INTO status_check
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
    WHERE nsp.nspname = 'odysseia'
      AND rel.relname = 'attachments'
      AND con.contype = 'c'
      AND pg_get_constraintdef(con.oid) LIKE '%status%';
    IF status_check IS NULL THEN
        RAISE EXCEPTION 'attachment status check not found';
    END IF;
    EXECUTE format('ALTER TABLE odysseia.attachments DROP CONSTRAINT %I', status_check);
END $$;
-- +goose StatementEnd

ALTER TABLE odysseia.attachments
    ADD CONSTRAINT attachments_status_check CHECK (status IN ('ready', 'expired'));

ALTER TABLE odysseia.attachments
    ADD COLUMN expired_at timestamptz;

ALTER TABLE odysseia.attachments
    ADD CONSTRAINT attachments_expired_pair_check CHECK (
        (status = 'ready' AND expired_at IS NULL)
        OR (status = 'expired' AND expired_at IS NOT NULL)
    );

-- The sweep reads ready rows in age order. Partial so expired history stays out of it.
CREATE INDEX attachments_ready_created
    ON odysseia.attachments (created_at, id)
    WHERE status = 'ready';

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.attachments WHERE status <> 'ready' OR expired_at IS NOT NULL) THEN
        RAISE EXCEPTION 'expired attachment rows exist; clear them before reverting attachment expiry';
    END IF;
END $$;
-- +goose StatementEnd

DROP INDEX odysseia.attachments_ready_created;
ALTER TABLE odysseia.attachments DROP CONSTRAINT attachments_expired_pair_check;
ALTER TABLE odysseia.attachments DROP COLUMN expired_at;
ALTER TABLE odysseia.attachments DROP CONSTRAINT attachments_status_check;
ALTER TABLE odysseia.attachments
    ADD CONSTRAINT attachments_status_check CHECK (status = 'ready');
