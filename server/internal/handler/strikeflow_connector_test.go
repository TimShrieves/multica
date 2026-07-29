package handler

import (
	"strings"
	"testing"
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
		"nul\x00hidden",
		strings.Repeat("x", strikeFlowReplyMaxBytes+1),
	} {
		if validStrikeFlowReply(invalid) {
			t.Fatalf("unsafe reply accepted: %q", invalid)
		}
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
