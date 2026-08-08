DO $$ BEGIN RAISE EXCEPTION 'evidence-preserving rollback required: strikeflow response outbox and command correlation must not be dropped'; END $$;
