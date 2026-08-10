-- The live scoped receipt table is deliberately tiny. Keep this migration in
-- one transaction so a crash between SQL execution and schema_migrations
-- recording can safely replay the whole file.
DROP INDEX IF EXISTS idx_strikeflow_connector_reply_command_unique;
CREATE UNIQUE INDEX idx_strikeflow_connector_reply_command_unique ON strikeflow_connector_reply_receipt (strikeflow_command_id) WHERE strikeflow_command_id IS NOT NULL;
