package handler

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStrikeFlowContentReplyProducerTransactionsAreFrozenDuringAdoption(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve content reply source path")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "strikeflow_content_reply.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	if begins, locks := strings.Count(source, "h.TxStarter.Begin(r.Context())"), strings.Count(source, "util.LockResponseProducer(r.Context(), tx)"); begins != 2 || locks != begins {
		t.Fatalf("content reply transactions must each take the producer lock immediately: begins=%d locks=%d", begins, locks)
	}
	firstBegin := strings.Index(source, "h.TxStarter.Begin(r.Context())")
	if firstBegin < 0 {
		t.Fatal("outer content reply transaction is missing")
	}
	firstLock := strings.Index(source[firstBegin:], "util.LockResponseProducer(r.Context(), tx)")
	firstTokenLock := strings.Index(source[firstBegin:], "FROM strikeflow_connector_token")
	if firstLock < 0 || firstTokenLock < 0 || firstLock > firstTokenLock {
		t.Fatal("outer content reply transaction must freeze producers before token/source/receipt mutation")
	}
	continuation := strings.Index(source, "func (h *Handler) recoverStrikeFlowContentReplyContinuation")
	if continuation < 0 {
		t.Fatal("content reply continuation recovery function is missing")
	}
	continuationSource := source[continuation:]
	continuationBegin := strings.Index(continuationSource, "h.TxStarter.Begin(r.Context())")
	continuationLock := strings.Index(continuationSource, "util.LockResponseProducer(r.Context(), tx)")
	receiptLock := strings.Index(continuationSource, "FROM strikeflow_connector_content_reply_receipt")
	if continuationBegin < 0 || continuationLock < continuationBegin || receiptLock < continuationLock {
		t.Fatal("continuation recovery must freeze producers before receipt adoption and task enqueue")
	}
}

func TestStrikeFlowContentReplyRouteAndAgentAuthorityRemainBound(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve content reply contract path")
	}
	handlerDir := filepath.Dir(sourceFile)
	content, err := os.ReadFile(filepath.Join(handlerDir, "strikeflow_content_reply.go"))
	if err != nil {
		t.Fatal(err)
	}
	router, err := os.ReadFile(filepath.Join(handlerDir, "..", "..", "cmd", "server", "router.go"))
	if err != nil {
		t.Fatal(err)
	}
	for name, contract := range map[string]struct {
		body      string
		contracts []string
	}{
		"content handler": {string(content), []string{`requireStrikeFlowScope(w, r, "content:reply")`, `scope.AgentID == ""`, "agent_id=$4"}},
		"connector route": {string(router), []string{"r.Use(middleware.StrikeFlowConnectorAuth(pool))", `r.Post("/content-replies", h.ReplyStrikeFlowContentPackage)`}},
	} {
		for _, want := range contract.contracts {
			if !strings.Contains(contract.body, want) {
				t.Errorf("%s is missing authority contract %q", name, want)
			}
		}
	}
}
