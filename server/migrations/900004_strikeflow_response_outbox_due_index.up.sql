CREATE INDEX CONCURRENTLY idx_strikeflow_response_outbox_due ON strikeflow_response_outbox (next_attempt_at, created_at) WHERE delivered_at IS NULL;
