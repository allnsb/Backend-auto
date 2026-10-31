-- +goose Up
CREATE TABLE odysseia.forum_member_removals (
    operation_id uuid PRIMARY KEY,
    forum_id uuid NOT NULL REFERENCES odysseia.forums(id),
    user_id uuid NOT NULL REFERENCES odysseia.users(id),
    operator_label text NOT NULL CHECK (length(btrim(operator_label)) BETWEEN 1 AND 200),
    reason text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
    removed boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE odysseia.forum_member_removals;
