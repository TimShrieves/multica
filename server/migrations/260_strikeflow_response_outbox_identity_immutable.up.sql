CREATE OR REPLACE FUNCTION reject_strikeflow_response_outbox_identity_change()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(
        NEW.event_id,NEW.event_type,NEW.strikeflow_command_id,NEW.workspace_key,
        NEW.workspace_id,NEW.project_id,NEW.issue_id,NEW.issue_identifier,
        NEW.inbox_item_id,NEW.root_comment_id,NEW.member_comment_id,
        NEW.continuation_task_id,NEW.recipient_id,NEW.agent_id,
        NEW.agent_comment_id,NEW.agent_comment_parent_id,
        NEW.agent_comment_content,NEW.agent_comment_type,NEW.occurred_at
    ) IS DISTINCT FROM ROW(
        OLD.event_id,OLD.event_type,OLD.strikeflow_command_id,OLD.workspace_key,
        OLD.workspace_id,OLD.project_id,OLD.issue_id,OLD.issue_identifier,
        OLD.inbox_item_id,OLD.root_comment_id,OLD.member_comment_id,
        OLD.continuation_task_id,OLD.recipient_id,OLD.agent_id,
        OLD.agent_comment_id,OLD.agent_comment_parent_id,
        OLD.agent_comment_content,OLD.agent_comment_type,OLD.occurred_at
    ) THEN
        RAISE EXCEPTION 'strikeflow response outbox identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS strikeflow_response_outbox_identity_immutable
    ON strikeflow_response_outbox;
CREATE TRIGGER strikeflow_response_outbox_identity_immutable
    BEFORE UPDATE ON strikeflow_response_outbox
    FOR EACH ROW EXECUTE FUNCTION reject_strikeflow_response_outbox_identity_change();
