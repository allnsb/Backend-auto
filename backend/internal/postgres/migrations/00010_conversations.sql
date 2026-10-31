-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Conversation streams reuse messages. This check is a superset of topic/channel,
-- and the publish check below is a superset of the P3 envelope. Partition
-- generation is unchanged, so an occupied outbox does not need to be drained.
-- Deploy the inbox namespace before enabling these writers; consumers that
-- reject an unknown channel will otherwise retry the new rows.
-- +goose StatementBegin
DO $$
DECLARE
    kind_check text;
BEGIN
    SELECT con.conname INTO kind_check
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
    WHERE nsp.nspname = 'odysseia'
      AND rel.relname = 'message_streams'
      AND con.contype = 'c'
      AND pg_get_constraintdef(con.oid) LIKE '%topic%'
      AND pg_get_constraintdef(con.oid) LIKE '%channel%'
      AND pg_get_constraintdef(con.oid) NOT LIKE '%last_position%';
    IF kind_check IS NULL THEN
        RAISE EXCEPTION 'message stream kind check not found';
    END IF;
    EXECUTE format('ALTER TABLE odysseia.message_streams DROP CONSTRAINT %I', kind_check);
END $$;
-- +goose StatementEnd

ALTER TABLE odysseia.message_streams
    ADD CONSTRAINT message_streams_kind_check CHECK (kind IN ('topic', 'channel', 'conversation'));

-- One live direct pair occupies dm_low/dm_high. Leaving does not restore that
-- membership. A later open that starts a new pair clears those columns on the
-- old row; PostgreSQL unique treats the nulls as distinct, so history can stay
-- while the slot is free. Groups keep a single owner_id. List order uses the
-- latest message timestamp, so this table is not updated when a message is appended.
CREATE TABLE odysseia.conversations (
    id uuid PRIMARY KEY REFERENCES odysseia.message_streams(id),
    stream_kind text GENERATED ALWAYS AS ('conversation'::text) STORED,
    kind text NOT NULL CHECK (kind IN ('dm', 'group')),
    dm_low uuid,
    dm_high uuid,
    owner_id uuid,
    title text,
    created_by uuid NOT NULL REFERENCES odysseia.users(id),
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    dissolved_at timestamptz,
    FOREIGN KEY (id, stream_kind) REFERENCES odysseia.message_streams(id, kind),
    CONSTRAINT conversations_dm_pair UNIQUE (dm_low, dm_high),
    CONSTRAINT conversations_request UNIQUE (created_by, request_id),
    CHECK (
        (kind = 'dm' AND owner_id IS NULL AND title IS NULL AND dissolved_at IS NULL
            AND ((dm_low IS NOT NULL AND dm_high IS NOT NULL AND dm_low < dm_high)
                OR (dm_low IS NULL AND dm_high IS NULL)))
        OR
        (kind = 'group' AND dm_low IS NULL AND dm_high IS NULL AND owner_id IS NOT NULL
            AND title IS NOT NULL AND length(btrim(title)) BETWEEN 1 AND 200)
    )
);
COMMENT ON COLUMN odysseia.conversations.dm_low IS 'Live direct-pair slot, ordered below dm_high. Null after the slot is released; removed members are not restored into the old row.';

-- joined_position records where the member entered. This slice stores 0 and
-- does not filter history: a new member can read every earlier message.
-- last_read_position is private to the member and only moves forward.
CREATE TABLE odysseia.conversation_members (
    conversation_id uuid NOT NULL REFERENCES odysseia.conversations(id),
    user_id uuid NOT NULL REFERENCES odysseia.users(id),
    role text NOT NULL CHECK (role IN ('owner', 'member')),
    joined_position bigint NOT NULL DEFAULT 0 CHECK (joined_position >= 0),
    last_read_position bigint NOT NULL DEFAULT 0 CHECK (last_read_position >= 0),
    joined_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    removed_at timestamptz,
    PRIMARY KEY (conversation_id, user_id),
    CHECK (role <> 'owner' OR removed_at IS NULL)
);

ALTER TABLE odysseia.conversations
    ADD CONSTRAINT conversations_owner_member
    FOREIGN KEY (id, owner_id)
    REFERENCES odysseia.conversation_members(conversation_id, user_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX conversation_members_active_user
    ON odysseia.conversation_members (user_id)
    WHERE removed_at IS NULL;

-- 50 is the database ceiling. ODYSSEIA_CONVERSATION_MEMBER_LIMIT may only be lower.
-- +goose StatementBegin
CREATE FUNCTION odysseia.conversation_member_ceiling() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    active integer;
BEGIN
    IF NEW.removed_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO active
    FROM odysseia.conversation_members
    WHERE conversation_id = NEW.conversation_id AND removed_at IS NULL;
    IF active > 50 THEN
        RAISE EXCEPTION 'conversation member limit exceeded' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER conversation_member_ceiling
    AFTER INSERT OR UPDATE OF removed_at ON odysseia.conversation_members
    FOR EACH ROW EXECUTE FUNCTION odysseia.conversation_member_ceiling();

-- +goose StatementBegin
CREATE FUNCTION odysseia.reject_read_regression() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.last_read_position < OLD.last_read_position THEN
        RAISE EXCEPTION 'read cursor cannot move backwards' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER conversation_members_read_forward
    BEFORE UPDATE OF last_read_position ON odysseia.conversation_members
    FOR EACH ROW EXECUTE FUNCTION odysseia.reject_read_regression();

-- A live group has exactly one active owner, and that row is owner_id.
-- Dissolved groups and direct messages skip the requirement. Deferred so a
-- transfer can change the member role and owner_id in one transaction.
-- +goose StatementBegin
CREATE FUNCTION odysseia.conversation_owner_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cid uuid;
    conv odysseia.conversations%ROWTYPE;
    owners integer;
BEGIN
    IF TG_TABLE_NAME = 'conversations' THEN
        cid := NEW.id;
        conv := NEW;
    ELSIF TG_OP = 'DELETE' THEN
        cid := OLD.conversation_id;
        SELECT * INTO conv FROM odysseia.conversations WHERE id = cid;
    ELSE
        cid := NEW.conversation_id;
        SELECT * INTO conv FROM odysseia.conversations WHERE id = cid;
    END IF;
    IF conv.id IS NULL THEN
        RETURN NULL;
    END IF;
    IF conv.kind = 'dm' THEN
        IF EXISTS (
            SELECT 1 FROM odysseia.conversation_members
            WHERE conversation_id = cid AND role = 'owner'
        ) THEN
            RAISE EXCEPTION 'direct conversation has no owner' USING ERRCODE = '23514';
        END IF;
        RETURN NULL;
    END IF;
    IF conv.dissolved_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO owners
    FROM odysseia.conversation_members
    WHERE conversation_id = cid AND role = 'owner' AND removed_at IS NULL;
    IF owners <> 1 OR NOT EXISTS (
        SELECT 1 FROM odysseia.conversation_members
        WHERE conversation_id = cid AND user_id = conv.owner_id
          AND role = 'owner' AND removed_at IS NULL
    ) THEN
        RAISE EXCEPTION 'group must keep exactly one active owner' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER conversations_owner_guard
    AFTER INSERT OR UPDATE ON odysseia.conversations
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION odysseia.conversation_owner_guard();

CREATE CONSTRAINT TRIGGER conversation_members_owner_guard
    AFTER INSERT OR UPDATE OR DELETE ON odysseia.conversation_members
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION odysseia.conversation_owner_guard();

-- Personal inbox channels are inbox:#<user>. scope_id stays the conversation,
-- so one conversation's fan-out keeps the existing scope partition. The channel
-- suffix is the recipient and is intentionally not equal to scope_id.
ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT realtime_publish;
ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT realtime_publish CHECK (method <> 'publish' OR (
    payload ?& ARRAY['channel','data','idempotency_key']
    AND jsonb_typeof(payload->'channel') = 'string'
    AND payload->>'channel' ~ '^(forum|topic|channel):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$|^inbox:#[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND jsonb_typeof(payload->'data') = 'object'
    AND payload->'data' ?& ARRAY['id','version','type','scope_id','position','data']
    AND payload#>'{data,version}' = '1'::jsonb
    AND jsonb_typeof(payload#>'{data,id}') = 'string'
    AND payload#>>'{data,id}' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND payload->'idempotency_key' = payload#>'{data,id}'
    AND (
        (split_part(payload->>'channel', ':', 1) <> 'inbox'
            AND payload#>>'{data,scope_id}' = split_part(payload->>'channel', ':', 2))
        OR (split_part(payload->>'channel', ':', 1) = 'inbox'
            AND payload#>>'{data,scope_id}' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
    )
    AND jsonb_typeof(payload#>'{data,position}') = 'string'
    AND (
        payload#>>'{data,position}' ~ '^[1-9][0-9]*$'
        OR (split_part(payload->>'channel', ':', 1) = 'inbox'
            AND payload#>>'{data,position}' = '0'
            AND payload#>>'{data,type}' IN (
                'conversation.created', 'conversation.member.added', 'conversation.member.removed',
                'conversation.read', 'conversation.owner.changed', 'conversation.dissolved'))
    )
    AND jsonb_typeof(payload#>'{data,data}') = 'object'
    AND (split_part(payload->>'channel', ':', 1), payload#>>'{data,type}')
        IN (
            ('forum','forum.topic.created'), ('forum','forum.channel.created'),
            ('topic','forum.message.created'), ('channel','channel.message.created'),
            ('inbox','conversation.created'), ('inbox','conversation.message.created'),
            ('inbox','conversation.member.added'), ('inbox','conversation.member.removed'),
            ('inbox','conversation.read'), ('inbox','conversation.owner.changed'),
            ('inbox','conversation.dissolved')
        )
) IS TRUE);

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.conversations)
        OR EXISTS (SELECT 1 FROM odysseia.message_streams WHERE kind = 'conversation')
        OR EXISTS (
            SELECT 1 FROM odysseia.realtime_outbox
            WHERE method = 'publish' AND split_part(payload->>'channel', ':', 1) = 'inbox'
        ) THEN
        RAISE EXCEPTION 'P4 conversation data or inbox events exist; explicitly migrate them before reverting private and group conversations';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT realtime_publish;
ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT realtime_publish CHECK (method <> 'publish' OR (
    payload ?& ARRAY['channel','data','idempotency_key']
    AND jsonb_typeof(payload->'channel') = 'string'
    AND payload->>'channel' ~ '^(forum|topic|channel):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND jsonb_typeof(payload->'data') = 'object'
    AND payload->'data' ?& ARRAY['id','version','type','scope_id','position','data']
    AND payload#>'{data,version}' = '1'::jsonb
    AND jsonb_typeof(payload#>'{data,id}') = 'string'
    AND payload#>>'{data,id}' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND payload->'idempotency_key' = payload#>'{data,id}'
    AND payload#>>'{data,scope_id}' = split_part(payload->>'channel', ':', 2)
    AND jsonb_typeof(payload#>'{data,position}') = 'string'
    AND payload#>>'{data,position}' ~ '^[1-9][0-9]*$'
    AND jsonb_typeof(payload#>'{data,data}') = 'object'
    AND (split_part(payload->>'channel', ':', 1), payload#>>'{data,type}')
        IN (('forum','forum.topic.created'), ('forum','forum.channel.created'),
            ('topic','forum.message.created'), ('channel','channel.message.created'))
) IS TRUE);

DROP TRIGGER conversation_members_owner_guard ON odysseia.conversation_members;
DROP TRIGGER conversations_owner_guard ON odysseia.conversations;
DROP FUNCTION odysseia.conversation_owner_guard();
DROP TRIGGER conversation_members_read_forward ON odysseia.conversation_members;
DROP FUNCTION odysseia.reject_read_regression();
DROP TRIGGER conversation_member_ceiling ON odysseia.conversation_members;
DROP FUNCTION odysseia.conversation_member_ceiling();
ALTER TABLE odysseia.conversations DROP CONSTRAINT conversations_owner_member;
DROP TABLE odysseia.conversation_members;
DROP TABLE odysseia.conversations;

ALTER TABLE odysseia.message_streams DROP CONSTRAINT message_streams_kind_check;
ALTER TABLE odysseia.message_streams
    ADD CONSTRAINT message_streams_kind_check CHECK (kind IN ('topic', 'channel'));
