-- +goose Up
CREATE TABLE odysseia.users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer text NOT NULL CHECK (issuer <> ''),
    subject text NOT NULL CHECK (subject <> ''),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (issuer, subject)
);

-- +goose Down
DROP TABLE odysseia.users;
