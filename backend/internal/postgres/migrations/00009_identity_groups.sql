-- +goose Up
-- Identity groups are a catalog. Forum membership keeps its own role, and a
-- user may also belong to several catalog groups. Adding role_id with a constant
-- default does not rewrite existing member rows. Outbox routing is unchanged.
CREATE TABLE odysseia.identity_groups (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    priority integer NOT NULL UNIQUE CHECK (priority >= 0),
    can_read boolean NOT NULL,
    can_post boolean NOT NULL,
    can_delete boolean NOT NULL,
    can_manage_members boolean NOT NULL,
    can_manage_groups boolean NOT NULL
);

INSERT INTO odysseia.identity_groups
    (id, name, priority, can_read, can_post, can_delete, can_manage_members, can_manage_groups)
VALUES
    ('00000000-0000-4000-8000-000000000001', 'member', 100, true, true, false, false, false),
    ('00000000-0000-4000-8000-000000000002', 'moderator', 200, true, true, true, true, false),
    ('00000000-0000-4000-8000-000000000003', 'owner', 300, true, true, true, true, true);

CREATE TABLE odysseia.user_groups (
    user_id uuid NOT NULL REFERENCES odysseia.users(id),
    group_id uuid NOT NULL REFERENCES odysseia.identity_groups(id),
    PRIMARY KEY (user_id, group_id)
);

ALTER TABLE odysseia.forum_members
    ADD COLUMN role_id uuid NOT NULL DEFAULT '00000000-0000-4000-8000-000000000001'
    REFERENCES odysseia.identity_groups(id);

-- +goose Down
ALTER TABLE odysseia.forum_members DROP COLUMN role_id;
DROP TABLE odysseia.user_groups;
DROP TABLE odysseia.identity_groups;
