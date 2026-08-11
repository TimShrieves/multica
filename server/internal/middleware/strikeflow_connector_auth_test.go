package middleware

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStrikeFlowConnectorScopeIsFailClosed(t *testing.T) {
	scope := StrikeFlowConnectorScope{
		Projects: map[string]struct{}{"project-1": {}},
		Scopes:   map[string]struct{}{"inbox:read": {}},
	}
	if !scope.Allows("inbox:read") || scope.Allows("inbox:reply") {
		t.Fatal("scope permission check did not fail closed")
	}
	if !scope.AllowsProject("project-1") || scope.AllowsProject("project-2") {
		t.Fatal("project check did not fail closed")
	}
}

func TestBearerTokenExactScheme(t *testing.T) {
	if got := bearerToken("Bearer msc_example"); got != "msc_example" {
		t.Fatalf("got %q", got)
	}
	for _, value := range []string{"msc_example", "bearer msc_example", "Basic abc"} {
		if got := bearerToken(value); got != "" {
			t.Fatalf("accepted non-canonical authorization %q", value)
		}
	}
}

func TestStrikeFlowConnectorScopeCarriesOnlyDatabaseBoundAgent(t *testing.T) {
	want := "eb361a09-be12-4626-9d03-faadc99a3933"
	scope := StrikeFlowConnectorScope{AgentID: want}
	ctx := context.WithValue(context.Background(), strikeFlowConnectorScopeKey{}, scope)
	got, ok := StrikeFlowConnectorScopeFromContext(ctx)
	if !ok || got.AgentID != want {
		t.Fatalf("connector scope agent = %q, ok=%v; want authoritative bound agent %q", got.AgentID, ok, want)
	}
	if empty, ok := StrikeFlowConnectorScopeFromContext(context.Background()); ok || empty.AgentID != "" {
		t.Fatal("context without connector authentication exposed an agent binding")
	}
}

func TestStrikeFlowConnectorAuthSelectsScansAndPublishesAgentBinding(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve auth source path")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "strikeflow_connector_auth.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	for _, contract := range []string{
		"SELECT id, workspace_id, recipient_id, agent_id, project_ids, scopes, expires_at",
		"&tokenID, &workspaceID, &recipientID, &agentID, &projects, &permissions, &expiresAt",
		"AgentID:     util.UUIDToString(agentID)",
	} {
		if !strings.Contains(source, contract) {
			t.Fatalf("connector authentication is missing agent binding contract %q", contract)
		}
	}
}
