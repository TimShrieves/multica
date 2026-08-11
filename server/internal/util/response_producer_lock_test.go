package util

import (
	"strings"
	"testing"
)

func TestResponseProducerAdvisoryLockIdentityIsExact(t *testing.T) {
	wantKey := "multica.strikeflow.response.producer.freeze"
	if strings.Count(ResponseProducerAdvisoryLockSQL, wantKey) != 1 {
		t.Fatalf("response producer lock must contain exact shared key once: %q", ResponseProducerAdvisoryLockSQL)
	}
	if !strings.Contains(ResponseProducerAdvisoryLockSQL, "pg_advisory_xact_lock") ||
		!strings.Contains(ResponseProducerAdvisoryLockSQL, "hashtextextended") {
		t.Fatal("producer lock must be transaction-scoped and derived from the shared textual identity")
	}
}
