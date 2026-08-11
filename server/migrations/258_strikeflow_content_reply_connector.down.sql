DO $$
BEGIN
    RAISE EXCEPTION '258_strikeflow_content_reply_connector is evidence-preserving and cannot be rolled back';
END $$;
