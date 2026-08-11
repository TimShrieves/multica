package handler

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidStrikeFlowReplyFailsClosed(t *testing.T) {
	for _, valid := range []string{"Please revise the opening.", "Line one\nLine two"} {
		if !validStrikeFlowReply(valid) {
			t.Fatalf("valid reply rejected: %q", valid)
		}
	}
	for _, invalid := range []string{
		"",
		" \n ",
		"mention://agent/00000000-0000-0000-0000-000000000000",
		"[strikeflow-agent-inbox:00000000-0000-0000-0000-000000000000]",
		"[STRIKEFLOW-FEEDBACK:00000000-0000-0000-0000-000000000000]",
		"[strikeflow-content-reply:00000000-0000-4000-8000-000000000000]",
		"nul\x00hidden",
		strings.Repeat("x", strikeFlowReplyMaxBytes+1),
	} {
		if validStrikeFlowReply(invalid) {
			t.Fatalf("unsafe reply accepted: %q", invalid)
		}
	}
}

func TestValidateStrikeFlowContentReplyTokenRequestIsPurposeBound(t *testing.T) {
	base := createStrikeFlowConnectorTokenRequest{
		Name: "content replies", RecipientID: "00000000-0000-4000-8000-000000000001",
		AgentID:    "00000000-0000-4000-8000-000000000002",
		ProjectIDs: []string{"00000000-0000-4000-8000-000000000003"},
		Scopes:     []string{"content:reply"}, ExpiresAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}
	if _, _, ok := validateStrikeFlowTokenRequest(httptest.NewRecorder(), base); !ok {
		t.Fatal("exact content reply credential rejected")
	}
	for name, mutate := range map[string]func(*createStrikeFlowConnectorTokenRequest){
		"missing_agent": func(req *createStrikeFlowConnectorTokenRequest) { req.AgentID = "" },
		"mixed_scope":   func(req *createStrikeFlowConnectorTokenRequest) { req.Scopes = append(req.Scopes, "inbox:read") },
		"multiple_projects": func(req *createStrikeFlowConnectorTokenRequest) {
			req.ProjectIDs = append(req.ProjectIDs, "00000000-0000-4000-8000-000000000004")
		},
		"agent_on_inbox_token": func(req *createStrikeFlowConnectorTokenRequest) { req.Scopes = []string{"inbox:read"} },
	} {
		t.Run(name, func(t *testing.T) {
			req := base
			req.ProjectIDs = append([]string(nil), base.ProjectIDs...)
			req.Scopes = append([]string(nil), base.Scopes...)
			mutate(&req)
			if _, _, ok := validateStrikeFlowTokenRequest(httptest.NewRecorder(), req); ok {
				t.Fatal("unsafe content credential accepted")
			}
		})
	}
}

func TestMintStrikeFlowConnectorTokenShape(t *testing.T) {
	a, err := mintStrikeFlowConnectorToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := mintStrikeFlowConnectorToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasPrefix(a, "msc_") || len(a) != 68 {
		t.Fatalf("unexpected connector token shape or collision")
	}
}
