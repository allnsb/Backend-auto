-- +goose Up
CREATE TABLE odysseia.forums (
    id uuid PRIMARY KEY,
    title text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    last_topic_position bigint NOT NULL DEFAULT 0 CHECK (last_topic_position >= 0)
);

CREATE TABLE odysseia.forum_members (
    forum_id uuid NOT NULL REFERENCES odysseia.forums(id),
    user_id uuid NOT NULL REFERENCES odysseia.users(id),
    PRIMARY KEY (forum_id, user_id)
);

CREATE TABLE odysseia.message_streams (
    id uuid PRIMARY KEY,
    last_position bigint NOT NULL CHECK (last_position > 0)
);

CREATE TABLE odysseia.topics (
    id uuid PRIMARY KEY REFERENCES odysseia.message_streams(id),
    forum_id uuid NOT NULL REFERENCES odysseia.forums(id),
    position bigint NOT NULL CHECK (position > 0),
    author_id uuid NOT NULL REFERENCES odysseia.users(id),
    title text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    mode text NOT NULL CHECK (mode = 'threaded'),
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (forum_id, position),
    UNIQUE (forum_id, author_id, request_id)
);

CREATE TABLE odysseia.messages (
    id uuid PRIMARY KEY,
    stream_id uuid NOT NULL REFERENCES odysseia.message_streams(id),
    position bigint NOT NULL CHECK (position > 0),
    author_id uuid NOT NULL REFERENCES odysseia.users(id),
    body text NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 32768),
    quote_id uuid,
    request_id uuid,
    request_hash bytea,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (stream_id, id),
    UNIQUE (stream_id, position),
    UNIQUE (stream_id, author_id, request_id),
    FOREIGN KEY (stream_id, quote_id) REFERENCES odysseia.messages(stream_id, id),
    CHECK ((request_id IS NOT NULL AND request_hash IS NOT NULL AND octet_length(request_hash) = 32)
        OR (position = 1 AND request_id IS NULL AND request_hash IS NULL))
);

CREATE TABLE odysseia.forum_entries (
    stream_id uuid NOT NULL REFERENCES odysseia.topics(id),
    message_id uuid PRIMARY KEY,
    root_id uuid,
    root_key uuid GENERATED ALWAYS AS (CASE WHEN root_id IS NULL THEN message_id END) STORED,
    FOREIGN KEY (stream_id, message_id) REFERENCES odysseia.messages(stream_id, id),
    UNIQUE (stream_id, root_key),
    FOREIGN KEY (stream_id, root_id) REFERENCES odysseia.forum_entries(stream_id, root_key)
);

-- Column layout matches the documented Centrifugo PostgreSQL outbox consumer.
-- Business event data remains versioned and independent of the transport provider.
CREATE TABLE odysseia.realtime_outbox (
    id bigserial PRIMARY KEY,
    method text NOT NULL,
    payload jsonb NOT NULL,
    partition integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE odysseia.realtime_outbox;
DROP TABLE odysseia.forum_entries;
DROP TABLE odysseia.messages;
DROP TABLE odysseia.topics;
DROP TABLE odysseia.message_streams;
DROP TABLE odysseia.forum_members;
DROP TABLE odysseia.forums;
