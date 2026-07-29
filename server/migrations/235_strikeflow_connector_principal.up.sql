-- Purpose-built, non-human principal for the private StrikeFlow projection.
-- The plaintext token is returned once and is never stored.
CREATE TABLE strikeflow_connector_token (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    recipient_id UUID NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    agent_id UUID REFERENCES agent(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    token_hash TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    project_ids UUID[] NOT NULL CHECK (cardinality(project_ids) BETWEEN 1 AND 32),
    scopes TEXT[] NOT NULL CHECK (
        cardinality(scopes) BETWEEN 1 AND 4
        AND scopes <@ ARRAY[
            'inbox:read','inbox:read_receipt','inbox:archive','inbox:reply',
            'content:reply'
        ]::text[]
    ),
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    rotated_from_id UUID REFERENCES strikeflow_connector_token(id) ON DELETE SET NULL,
    created_by UUID NOT NULL REFERENCES "user"(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        ('content:reply' = ANY(scopes)
            AND cardinality(scopes) = 1
            AND cardinality(project_ids) = 1
            AND agent_id IS NOT NULL)
        OR (NOT ('content:reply' = ANY(scopes)) AND agent_id IS NULL)
    ),
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '30 days')
);

CREATE INDEX idx_strikeflow_connector_active
    ON strikeflow_connector_token (workspace_id, recipient_id, expires_at)
    WHERE revoked_at IS NULL;

-- Durable, append-only security/event trail. Bodies and token values are never
-- stored; object ids and payload hashes are enough to reconcile a connector run.
CREATE TABLE strikeflow_connector_audit (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    recipient_id UUID NOT NULL,
    request_id TEXT,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('allowed','denied','replayed','failed')),
    inbox_item_id UUID,
    issue_id UUID,
    root_comment_id UUID,
    comment_id UUID,
    idempotency_key UUID,
    payload_hash TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_strikeflow_connector_audit_token_created
    ON strikeflow_connector_audit (token_id, created_at DESC);

CREATE FUNCTION reject_strikeflow_connector_audit_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'strikeflow connector audit is append-only';
END;
$$;

CREATE TRIGGER strikeflow_connector_audit_immutable
    BEFORE UPDATE OR DELETE ON strikeflow_connector_audit
    FOR EACH ROW EXECUTE FUNCTION reject_strikeflow_connector_audit_mutation();

-- Reservation-before-side-effect makes reply retries crash-safe. A retry with
-- the same key and hash either replays the committed receipt or recovers the
-- server-generated marker; a different hash is always a conflict.
CREATE TABLE strikeflow_connector_reply_receipt (
    token_id UUID NOT NULL REFERENCES strikeflow_connector_token(id) ON DELETE CASCADE,
    idempotency_key UUID NOT NULL,
    inbox_item_id UUID NOT NULL REFERENCES inbox_item(id) ON DELETE CASCADE,
    issue_id UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    root_comment_id UUID NOT NULL REFERENCES comment(id) ON DELETE CASCADE,
    payload_hash TEXT NOT NULL,
    comment_id UUID REFERENCES comment(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    committed_at TIMESTAMPTZ,
    PRIMARY KEY (token_id, idempotency_key)
);

-- Content-package replies use the immutable package binding's issue/root pair.
-- These UUIDs intentionally have no foreign keys: the receipt is forensic and
-- must survive token rotation/revocation or later source cleanup. The handler
-- revalidates every live relationship in the same transaction before mutation.
CREATE TABLE strikeflow_connector_content_reply_receipt (
    token_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    recipient_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    idempotency_key UUID NOT NULL,
    issue_id UUID NOT NULL,
    root_comment_id UUID NOT NULL,
    reply_root_hash TEXT NOT NULL,
    package_id UUID NOT NULL,
    package_payload_hash TEXT NOT NULL,
    source_revision INTEGER NOT NULL CHECK (source_revision > 0),
    payload_hash TEXT NOT NULL,
    comment_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    committed_at TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, recipient_id, agent_id, idempotency_key),
    CHECK (length(reply_root_hash) = 64),
    CHECK (length(package_payload_hash) = 64),
    CHECK (length(payload_hash) = 64),
    CHECK (
        (comment_id IS NULL AND committed_at IS NULL)
        OR (comment_id IS NOT NULL AND committed_at IS NOT NULL)
    )
);
