package strikeflowresponse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResponseEventIDMigrationIsConcurrentAndEvidencePreserving(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve migration contract test path")
	}
	migrations := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "migrations"))
	up, err := os.ReadFile(filepath.Join(migrations, "257_strikeflow_response_outbox_event_id_unique.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE UNIQUE INDEX CONCURRENTLY idx_strikeflow_response_outbox_event_id_unique ON strikeflow_response_outbox (event_id);"
	if strings.TrimSpace(string(up)) != want {
		t.Fatalf("257 up migration = %q, want one exact concurrent statement", strings.TrimSpace(string(up)))
	}
	down, err := os.ReadFile(filepath.Join(migrations, "257_strikeflow_response_outbox_event_id_unique.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(down), "RAISE EXCEPTION") || strings.Contains(strings.ToUpper(string(down)), "DROP INDEX") {
		t.Fatal("257 down migration must abort without deleting evidence")
	}
}

func TestResponseMigrationPortPreservesMainLineage(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve migration contract test path")
	}
	migrations := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "migrations"))
	for _, name := range []string{
		"253_strikeflow_response_outbox.up.sql",
		"254_strikeflow_connector_reply_command_unique.up.sql",
		"255_strikeflow_response_outbox_event_unique.up.sql",
		"256_strikeflow_response_outbox_due_index.up.sql",
		"257_strikeflow_response_outbox_event_id_unique.up.sql",
		"258_strikeflow_content_reply_connector.up.sql",
		"259_strikeflow_content_reply_receipt_unique.up.sql",
	} {
		if _, err := os.Stat(filepath.Join(migrations, name)); err != nil {
			t.Fatalf("required main-lineage migration %s is missing: %v", name, err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(migrations, "*_strikeflow_response_*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"253_strikeflow_response_outbox.up.sql":                   true,
		"253_strikeflow_response_outbox.down.sql":                 true,
		"255_strikeflow_response_outbox_event_unique.up.sql":      true,
		"255_strikeflow_response_outbox_event_unique.down.sql":    true,
		"256_strikeflow_response_outbox_due_index.up.sql":         true,
		"256_strikeflow_response_outbox_due_index.down.sql":       true,
		"257_strikeflow_response_outbox_event_id_unique.up.sql":   true,
		"257_strikeflow_response_outbox_event_id_unique.down.sql": true,
	}
	for _, match := range matches {
		if !allowed[filepath.Base(match)] {
			t.Fatalf("unexpected response migration outside current main lineage: %s", match)
		}
	}
}

func TestContentReplyMigrationsAreForwardOnlyAndConcurrent(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve migration contract test path")
	}
	migrations := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "migrations"))
	schema, err := os.ReadFile(filepath.Join(migrations, "258_strikeflow_content_reply_connector.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	schemaText := string(schema)
	for _, required := range []string{
		"ADD COLUMN IF NOT EXISTS agent_id UUID",
		"content:reply",
		"cardinality(scopes) = 1",
		"cardinality(project_ids) = 1",
		"agent_id IS NOT NULL",
		"ADD COLUMN IF NOT EXISTS root_comment_id UUID",
		"CREATE TABLE IF NOT EXISTS strikeflow_connector_content_reply_receipt",
		"continuation_task_id UUID",
	} {
		if !strings.Contains(schemaText, required) {
			t.Fatalf("258 migration missing %q", required)
		}
	}
	if strings.Contains(strings.ToUpper(schemaText), "REFERENCES ") || strings.Contains(strings.ToUpper(schemaText), "CREATE INDEX") {
		t.Fatal("258 must not add foreign keys or build an index in its multi-statement migration")
	}
	unique, err := os.ReadFile(filepath.Join(migrations, "259_strikeflow_content_reply_receipt_unique.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_strikeflow_content_reply_receipt_unique ON strikeflow_connector_content_reply_receipt (workspace_id, recipient_id, agent_id, idempotency_key);"
	if strings.TrimSpace(string(unique)) != want {
		t.Fatalf("259 up migration = %q, want one exact concurrent statement", strings.TrimSpace(string(unique)))
	}
	for _, version := range []string{"258_strikeflow_content_reply_connector", "259_strikeflow_content_reply_receipt_unique"} {
		down, err := os.ReadFile(filepath.Join(migrations, version+".down.sql"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(down), "RAISE EXCEPTION") || strings.Contains(strings.ToUpper(string(down)), "DROP ") {
			t.Fatalf("%s down migration must abort without deleting evidence", version)
		}
	}
}
