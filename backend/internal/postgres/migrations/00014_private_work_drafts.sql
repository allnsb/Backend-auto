-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Works are independent of forums and conversations. Only the private draft
-- state is enabled; publishing and associations require a later explicit change.
CREATE TABLE odysseia.works (
    id uuid PRIMARY KEY,
    author_id uuid NOT NULL REFERENCES odysseia.users(id),
    status text NOT NULL DEFAULT 'draft' CHECK (status = 'draft'),
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200 AND btrim(title) <> ''),
    introduction text NOT NULL CHECK (octet_length(introduction) <= 32768),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    CONSTRAINT works_author_request UNIQUE (author_id, request_id)
);

CREATE INDEX works_author_created ON odysseia.works (author_id, created_at DESC, id DESC);

COMMENT ON TABLE odysseia.works IS 'Author-private current drafts; no public discovery, files or forum ownership implied.';

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM odysseia.works) THEN
        RAISE EXCEPTION 'work drafts exist; explicitly preserve or migrate them before reverting';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE odysseia.works;
