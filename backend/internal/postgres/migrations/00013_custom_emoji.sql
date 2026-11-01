-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Custom emoji is a catalog, not a message column. Message bodies keep the
-- Discord-style token. Deletion sets deleted_at; the public object stays,
-- because the key is content-addressed and may be shared. Outbox routing is
-- unchanged, so this migration does not need a drain.
CREATE TABLE odysseia.emojis (
    id uuid PRIMARY KEY,
    scope text NOT NULL CHECK (scope IN ('site', 'forum')),
    forum_id uuid REFERENCES odysseia.forums(id),
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9_]{2,32}$'),
    animated boolean NOT NULL,
    content_type text NOT NULL CHECK (content_type IN ('image/png', 'image/gif', 'image/webp')),
    byte_size integer NOT NULL CHECK (byte_size BETWEEN 1 AND 262144),
    width integer NOT NULL CHECK (width BETWEEN 1 AND 256),
    height integer NOT NULL CHECK (height BETWEEN 1 AND 256),
    sha256 bytea NOT NULL CHECK (octet_length(sha256) = 32),
    object_key text NOT NULL CHECK (object_key ~ '^emoji/v1/[0-9a-f]{2}/[0-9a-f]{64}\.(png|gif|webp)$'),
    created_by uuid NOT NULL REFERENCES odysseia.users(id),
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at timestamptz,
    CONSTRAINT emojis_request UNIQUE (created_by, request_id),
    CHECK (
        (scope = 'site' AND forum_id IS NULL)
        OR (scope = 'forum' AND forum_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX emojis_site_name_live
    ON odysseia.emojis (name)
    WHERE scope = 'site' AND deleted_at IS NULL;

CREATE UNIQUE INDEX emojis_forum_name_live
    ON odysseia.emojis (forum_id, name)
    WHERE scope = 'forum' AND deleted_at IS NULL;

COMMENT ON TABLE odysseia.emojis IS 'Site and forum custom emoji. Live names are unique per scope. Deleted rows stay so message tokens can fall back to :name:.';

-- +goose Down
DROP TABLE odysseia.emojis;
