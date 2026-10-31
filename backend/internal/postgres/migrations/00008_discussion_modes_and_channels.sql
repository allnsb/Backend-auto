-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE odysseia.topics DROP CONSTRAINT topics_mode_check;
ALTER TABLE odysseia.topics ADD CONSTRAINT topics_mode_check CHECK (mode IN ('threaded','chat'));

-- A channel starts empty. The discriminator also prevents one stream from
-- becoming both a forum topic and a separately authorized product channel.
ALTER TABLE odysseia.message_streams DROP CONSTRAINT message_streams_last_position_check;
ALTER TABLE odysseia.message_streams
    ADD COLUMN kind text NOT NULL DEFAULT 'topic' CHECK (kind IN ('topic','channel')),
    ADD CONSTRAINT message_streams_position_check CHECK (last_position >= 0 AND (kind <> 'topic' OR last_position > 0)),
    ADD CONSTRAINT message_streams_id_kind UNIQUE (id,kind);
ALTER TABLE odysseia.topics
    ADD COLUMN stream_kind text GENERATED ALWAYS AS ('topic'::text) STORED,
    ADD CONSTRAINT topics_stream_kind FOREIGN KEY (id,stream_kind) REFERENCES odysseia.message_streams(id,kind);

CREATE TABLE odysseia.channels (
    id uuid PRIMARY KEY REFERENCES odysseia.message_streams(id),
    stream_kind text GENERATED ALWAYS AS ('channel'::text) STORED,
    forum_id uuid NOT NULL REFERENCES odysseia.forums(id),
    position bigint NOT NULL CHECK (position > 0),
    author_id uuid NOT NULL REFERENCES odysseia.users(id),
    title text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (id,stream_kind) REFERENCES odysseia.message_streams(id,kind),
    UNIQUE (forum_id,position),
    UNIQUE (forum_id,author_id,request_id)
);
COMMENT ON COLUMN odysseia.forums.last_topic_position IS 'Shared topic/channel creation order within this forum; list positions may have gaps.';

-- Keep the existing durable consumer contract and stable five-partition route.
-- Roll out the channel namespace to all consumers before enabling P3 writers.
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

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Never silently convert chat posts or delete product channels on downgrade.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.channels)
        OR EXISTS (SELECT 1 FROM odysseia.topics WHERE mode='chat')
        OR EXISTS (SELECT 1 FROM odysseia.realtime_outbox WHERE method='publish'
            AND payload#>>'{data,type}' IN ('forum.channel.created','channel.message.created')) THEN
        RAISE EXCEPTION 'P3 data or events exist; explicitly migrate them before reverting discussion modes and channels';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE odysseia.realtime_outbox DROP CONSTRAINT realtime_publish;
ALTER TABLE odysseia.realtime_outbox ADD CONSTRAINT realtime_publish CHECK (method <> 'publish' OR (
    payload ?& ARRAY['channel','data','idempotency_key']
    AND jsonb_typeof(payload->'channel') = 'string'
    AND payload->>'channel' ~ '^(forum|topic):[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
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
        IN (('forum','forum.topic.created'), ('topic','forum.message.created'))
) IS TRUE);
DROP TABLE odysseia.channels;
ALTER TABLE odysseia.topics DROP CONSTRAINT topics_stream_kind, DROP COLUMN stream_kind;
ALTER TABLE odysseia.message_streams
    DROP CONSTRAINT message_streams_id_kind,
    DROP CONSTRAINT message_streams_position_check,
    DROP COLUMN kind,
    ADD CONSTRAINT message_streams_last_position_check CHECK (last_position > 0);
ALTER TABLE odysseia.topics DROP CONSTRAINT topics_mode_check;
ALTER TABLE odysseia.topics ADD CONSTRAINT topics_mode_check CHECK (mode='threaded');
COMMENT ON COLUMN odysseia.forums.last_topic_position IS NULL;
