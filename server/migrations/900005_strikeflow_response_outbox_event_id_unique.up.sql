CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_strikeflow_response_outbox_event_id_unique ON strikeflow_response_outbox (event_id);
