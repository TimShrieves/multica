-- The outbox is created empty by 900001. Transactional recreation makes a
-- post-SQL/pre-ledger crash retry deterministic and repairs invalid remnants.
DROP INDEX IF EXISTS idx_strikeflow_response_outbox_event_unique;
CREATE UNIQUE INDEX idx_strikeflow_response_outbox_event_unique ON strikeflow_response_outbox (event_type, continuation_task_id, COALESCE(agent_comment_id, '00000000-0000-0000-0000-000000000000'::uuid));
