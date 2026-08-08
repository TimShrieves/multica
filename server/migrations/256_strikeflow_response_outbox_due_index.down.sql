DO $$ BEGIN RAISE EXCEPTION 'evidence-preserving rollback required: disable publisher runtime without deleting durable evidence'; END $$;
