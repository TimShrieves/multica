DO $$ BEGIN RAISE EXCEPTION 'evidence-preserving rollback required: strikeflow command correlation index retained'; END $$;
