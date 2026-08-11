package main

import (
	"path/filepath"
	"testing"
)

func TestFilterMigrationFilesRequiresExactExistingVersions(t *testing.T) {
	files := []string{
		filepath.Join("migrations", "258_strikeflow_content_reply_connector.up.sql"),
		filepath.Join("migrations", "259_strikeflow_content_reply_receipt_unique.up.sql"),
		filepath.Join("migrations", "260_strikeflow_response_outbox_identity_immutable.up.sql"),
	}
	selected, err := filterMigrationFiles(files, "258_strikeflow_content_reply_connector,259_strikeflow_content_reply_receipt_unique,260_strikeflow_response_outbox_identity_immutable")
	if err != nil || len(selected) != 3 {
		t.Fatalf("filterMigrationFiles() = %v, %v", selected, err)
	}
	if _, err := filterMigrationFiles(files, "258_strikeflow_content_reply_connector,258_strikeflow_content_reply_connector"); err == nil {
		t.Fatal("duplicate allowlist entry must fail closed")
	}
	if _, err := filterMigrationFiles(files, "258_strikeflow_content_reply_connector,261_missing"); err == nil {
		t.Fatal("unknown allowlist entry must fail closed")
	}
}
