ALTER TABLE strikeflow_connector_reply_receipt
    ADD COLUMN strikeflow_command_id UUID;

CREATE OR REPLACE FUNCTION reject_strikeflow_reply_command_binding_change()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.strikeflow_command_id IS DISTINCT FROM OLD.strikeflow_command_id THEN
        RAISE EXCEPTION 'strikeflow command binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER strikeflow_reply_command_binding_immutable
    BEFORE UPDATE ON strikeflow_connector_reply_receipt
    FOR EACH ROW EXECUTE FUNCTION reject_strikeflow_reply_command_binding_change();

-- Durable, dormant-by-default outbound events for the purpose-scoped
-- StrikeFlow connector. Relationships are enforced by the publisher's exact
-- binding query rather than foreign keys so deletion and rollback stay
-- explicit at the application layer.
CREATE TABLE strikeflow_response_outbox (
    event_id UUID NOT NULL DEFAULT gen_random_uuid(),
    event_type TEXT NOT NULL CHECK (event_type IN ('agent_comment.created', 'task.completed')),
    strikeflow_command_id UUID NOT NULL,
    workspace_key TEXT NOT NULL,
    workspace_id UUID NOT NULL,
    project_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    issue_identifier TEXT NOT NULL,
    inbox_item_id UUID NOT NULL,
    root_comment_id UUID NOT NULL,
    member_comment_id UUID NOT NULL,
    continuation_task_id UUID NOT NULL,
    recipient_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    agent_comment_id UUID,
    agent_comment_parent_id UUID,
    agent_comment_content TEXT,
    agent_comment_type TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    delivered_at TIMESTAMPTZ,
    needs_attention_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (event_type = 'agent_comment.created'
            AND agent_comment_id IS NOT NULL
            AND agent_comment_content IS NOT NULL
            AND agent_comment_type IS NOT NULL)
        OR
        (event_type = 'task.completed'
            AND agent_comment_id IS NULL
            AND agent_comment_content IS NULL
            AND agent_comment_type IS NULL)
    )
);
