DO $$
BEGIN
    RAISE EXCEPTION '259_strikeflow_content_reply_receipt_unique is evidence-preserving and cannot be rolled back';
END $$;
