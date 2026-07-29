package middleware

import "testing"

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
