-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Conversation messages share odysseia.messages with forums and channels.
-- The old body check required at least one byte for every stream. Empty text
-- is now legal only for a conversation row that has a message_attachments
-- link, and only at commit, so the attachment insert can follow the message
-- insert in the same transaction. Topic and channel rows still cannot commit
-- an empty body. Existing rows already satisfy the stricter check, so this
-- migration does not rewrite them and does not touch the outbox.
-- +goose StatementBegin
DO $$
DECLARE
    body_check text;
BEGIN
    SELECT con.conname INTO body_check
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
    WHERE nsp.nspname = 'odysseia'
      AND rel.relname = 'messages'
      AND con.contype = 'c'
      AND pg_get_constraintdef(con.oid) LIKE '%octet_length(body)%';
    IF body_check IS NULL THEN
        RAISE EXCEPTION 'message body length check not found';
    END IF;
    EXECUTE format('ALTER TABLE odysseia.messages DROP CONSTRAINT %I', body_check);
END $$;
-- +goose StatementEnd

ALTER TABLE odysseia.messages
    ADD CONSTRAINT messages_body_length_check CHECK (octet_length(body) BETWEEN 0 AND 32768);

-- Uploaded objects are private. The object key is server-generated. status is
-- ready because the API writes R2 before inserting the row. One attachment can
-- be linked from only one message. The byte, edge, pixel, and duration ceilings
-- match the process configuration maximums; a deployment may only be stricter.
CREATE TABLE odysseia.attachments (
    id uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES odysseia.conversations(id),
    uploader_id uuid NOT NULL REFERENCES odysseia.users(id),
    kind text NOT NULL CHECK (kind IN ('image', 'voice')),
    content_type text NOT NULL CHECK (content_type IN (
        'image/jpeg', 'image/png', 'image/webp', 'image/gif',
        'audio/webm;codecs=opus', 'audio/mp4', 'audio/ogg'
    )),
    byte_size bigint NOT NULL CHECK (byte_size BETWEEN 1 AND 10485760),
    width integer,
    height integer,
    duration_ms integer,
    object_key text NOT NULL UNIQUE CHECK (
        object_key ~ '^conversations/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    ),
    sha256 bytea NOT NULL CHECK (octet_length(sha256) = 32),
    status text NOT NULL CHECK (status = 'ready'),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (
        (kind = 'image' AND duration_ms IS NULL
            AND width BETWEEN 1 AND 32768 AND height BETWEEN 1 AND 32768
            AND width::bigint * height::bigint <= 67108864)
        OR
        (kind = 'voice' AND width IS NULL AND height IS NULL
            AND duration_ms BETWEEN 1 AND 300000)
    )
);

CREATE INDEX attachments_uploader_created
    ON odysseia.attachments (uploader_id, created_at DESC);

CREATE TABLE odysseia.message_attachments (
    message_id uuid NOT NULL REFERENCES odysseia.messages(id),
    attachment_id uuid NOT NULL UNIQUE REFERENCES odysseia.attachments(id),
    position smallint NOT NULL CHECK (position >= 0 AND position < 4),
    PRIMARY KEY (message_id, position)
);

-- Empty body is rejected unless this conversation message owns an attachment.
-- A non-empty body returns immediately, so forum and channel inserts do not
-- look up the stream on the common path.
-- +goose StatementBegin
CREATE FUNCTION odysseia.message_text_or_attachment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    stream_kind text;
    attached integer;
BEGIN
    IF octet_length(NEW.body) >= 1 THEN
        RETURN NULL;
    END IF;
    SELECT kind INTO stream_kind FROM odysseia.message_streams WHERE id = NEW.stream_id;
    IF stream_kind IS DISTINCT FROM 'conversation' THEN
        RAISE EXCEPTION 'forum and channel messages require text' USING ERRCODE = '23514';
    END IF;
    SELECT count(*) INTO attached FROM odysseia.message_attachments WHERE message_id = NEW.id;
    IF attached < 1 THEN
        RAISE EXCEPTION 'conversation message requires text or an attachment' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER messages_text_or_attachment
    AFTER INSERT OR UPDATE OF body ON odysseia.messages
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION odysseia.message_text_or_attachment();

-- Unlinking the last attachment from an empty conversation message is refused.
-- +goose StatementBegin
CREATE FUNCTION odysseia.message_keeps_text_or_attachment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    body_length integer;
    remaining integer;
BEGIN
    SELECT octet_length(body) INTO body_length FROM odysseia.messages WHERE id = OLD.message_id;
    IF body_length IS NULL OR body_length >= 1 THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO remaining FROM odysseia.message_attachments WHERE message_id = OLD.message_id;
    IF remaining < 1 THEN
        RAISE EXCEPTION 'conversation message requires text or an attachment' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER message_attachments_keep_body
    AFTER DELETE ON odysseia.message_attachments
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION odysseia.message_keeps_text_or_attachment();

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.attachments)
        OR EXISTS (SELECT 1 FROM odysseia.messages WHERE octet_length(body) = 0) THEN
        RAISE EXCEPTION 'attachment rows or empty message bodies exist; migrate them before reverting conversation attachments';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER message_attachments_keep_body ON odysseia.message_attachments;
DROP FUNCTION odysseia.message_keeps_text_or_attachment();
DROP TRIGGER messages_text_or_attachment ON odysseia.messages;
DROP FUNCTION odysseia.message_text_or_attachment();
DROP TABLE odysseia.message_attachments;
DROP TABLE odysseia.attachments;

ALTER TABLE odysseia.messages DROP CONSTRAINT messages_body_length_check;
ALTER TABLE odysseia.messages
    ADD CONSTRAINT messages_body_length_check CHECK (octet_length(body) BETWEEN 1 AND 32768);
