package strikeflowresponse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEveryResponseLineageProducerUsesSharedAdvisoryLock(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve source contract test path")
	}
	serverRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	read := func(relative string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(serverRoot, relative))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	comment := read(filepath.Join("internal", "handler", "comment.go"))
	connector := read(filepath.Join("internal", "handler", "strikeflow_connector.go"))
	task := read(filepath.Join("internal", "service", "task.go"))
	lock := read(filepath.Join("internal", "util", "response_producer_lock.go"))

	contracts := []struct {
		name string
		body string
		want []string
	}{
		{"direct agent comment", comment, []string{`authorType == "agent" && h.TxStarter != nil`, "util.LockResponseProducer(r.Context(), tx)"}},
		{"connector receipt and member comment", connector, []string{"func (h *Handler) ReplyStrikeFlowInbox", "util.LockResponseProducer(r.Context(), tx)"}},
		{"task completion and generated comment", task, []string{"func (s *TaskService) CompleteTask", "s.runResponseProducerInTx(ctx", "err = s.runResponseProducerInTx(ctx", "util.LockResponseProducer(ctx, tx)"}},
		{"shared lock identity", lock, []string{"multica.strikeflow.response.producer.freeze", "pg_advisory_xact_lock"}},
	}
	for _, contract := range contracts {
		for _, want := range contract.want {
			if !strings.Contains(contract.body, want) {
				t.Errorf("%s is missing source contract %q", contract.name, want)
			}
		}
	}
}
