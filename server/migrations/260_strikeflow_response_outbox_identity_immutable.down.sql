DO $$ BEGIN RAISE EXCEPTION 'evidence-preserving rollback required: strikeflow response outbox identity protection must not be removed'; END $$;
