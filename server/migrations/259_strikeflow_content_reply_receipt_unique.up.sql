CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_strikeflow_content_reply_receipt_unique ON strikeflow_connector_content_reply_receipt (workspace_id, recipient_id, agent_id, idempotency_key);
