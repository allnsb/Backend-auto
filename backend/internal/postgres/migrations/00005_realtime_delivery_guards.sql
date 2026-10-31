-- +goose Up
-- Reject commands the native consumer would permanently discard, before the
-- business transaction can commit. These are provider-envelope constraints.
ALTER TABLE odysseia.realtime_outbox
    ADD CONSTRAINT realtime_method CHECK (method IN ('publish', 'disconnect')),
    ADD CONSTRAINT realtime_partition CHECK (partition = 0),
    ADD CONSTRAINT realtime_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    ADD CONSTRAINT realtime_publish CHECK (method <> 'publish' OR (
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
    ) IS TRUE),
    ADD CONSTRAINT realtime_disconnect CHECK (method <> 'disconnect' OR (
        payload ? 'user'
        AND jsonb_typeof(payload->'user') = 'string'
        AND payload->>'user' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
        AND payload->'disconnect' = '{"code":4001,"reason":"permissions changed"}'::jsonb
    ) IS TRUE);

-- A dedicated observer may read aggregates without gaining access to payloads,
-- user IDs or business tables. The view intentionally runs as its schema owner.
CREATE VIEW odysseia.realtime_backlog WITH (security_barrier = true) AS
SELECT count(*) AS pending,
       count(*) FILTER (WHERE method = 'publish') AS publications,
       count(*) FILTER (WHERE method = 'disconnect') AS disconnects,
       min(created_at) AS oldest_created_at,
       COALESCE(GREATEST(extract(epoch FROM (statement_timestamp() - min(created_at))), 0), 0)::double precision AS oldest_age_seconds
FROM odysseia.realtime_outbox;
REVOKE ALL ON odysseia.realtime_backlog FROM PUBLIC;

-- +goose Down
DROP VIEW odysseia.realtime_backlog;
ALTER TABLE odysseia.realtime_outbox
    DROP CONSTRAINT realtime_disconnect,
    DROP CONSTRAINT realtime_publish,
    DROP CONSTRAINT realtime_payload_object,
    DROP CONSTRAINT realtime_partition,
    DROP CONSTRAINT realtime_method;
