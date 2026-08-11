ALTER TABLE strikeflow_connector_token
    ADD COLUMN IF NOT EXISTS agent_id UUID;

ALTER TABLE strikeflow_connector_token
    DROP CONSTRAINT IF EXISTS strikeflow_connector_token_scopes_check;

ALTER TABLE strikeflow_connector_token
    DROP CONSTRAINT IF EXISTS strikeflow_connector_token_content_reply_scope_check;

ALTER TABLE strikeflow_connector_token
    ADD CONSTRAINT strikeflow_connector_token_scopes_check CHECK (
        cardinality(scopes) BETWEEN 1 AND 4
        AND scopes <@ ARRAY[
            'inbox:read','inbox:read_receipt','inbox:archive','inbox:reply',
            'content:reply'
        ]::text[]
    );

ALTER TABLE strikeflow_connector_token
    ADD CONSTRAINT strikeflow_connector_token_content_reply_scope_check CHECK (
        ('content:reply' = ANY(scopes)
            AND cardinality(scopes) = 1
            AND cardinality(project_ids) = 1
            AND agent_id IS NOT NULL)
        OR (NOT ('content:reply' = ANY(scopes)) AND agent_id IS NULL)
    );

ALTER TABLE strikeflow_connector_audit
    ADD COLUMN IF NOT EXISTS root_comment_id UUID;

CREATE TABLE IF NOT EXISTS strikeflow_connector_content_reply_receipt (
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
    continuation_task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    committed_at TIMESTAMPTZ,
    CHECK (length(reply_root_hash) = 64),
    CHECK (length(package_payload_hash) = 64),
    CHECK (length(payload_hash) = 64),
    CHECK (
        (comment_id IS NULL AND committed_at IS NULL)
        OR (comment_id IS NOT NULL AND committed_at IS NOT NULL)
    )
);
