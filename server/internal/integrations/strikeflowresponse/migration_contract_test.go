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
	up, err := os.ReadFile(filepath.Join(migrations, "900005_strikeflow_response_outbox_event_id_unique.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_strikeflow_response_outbox_event_id_unique ON strikeflow_response_outbox (event_id);"
	if strings.TrimSpace(string(up)) != want {
		t.Fatalf("900005 up migration = %q, want one exact concurrent statement", strings.TrimSpace(string(up)))
	}
	down, err := os.ReadFile(filepath.Join(migrations, "900005_strikeflow_response_outbox_event_id_unique.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(down), "RAISE EXCEPTION") || strings.Contains(strings.ToUpper(string(down)), "DROP INDEX") {
		t.Fatal("900005 down migration must abort without deleting evidence")
	}
}
