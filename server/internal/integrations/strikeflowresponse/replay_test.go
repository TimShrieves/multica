package strikeflowresponse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendDeliveredCommentReplayUsesExactPayloadAndRequiresReplayAck(t *testing.T) {
	recordedAt := "2026-08-11T07:00:00Z"
	row := outboxRow{
		EventID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", EventType: "agent_comment.created",
		CommandID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", WorkspaceKey: "strike",
		ProjectID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", IssueID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		IssueIdentifier: "STR-999", RootCommentID: "root", MemberCommentID: "member",
		ContinuationTaskID: "task", RecipientID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		AgentID: "ffffffff-ffff-4fff-8fff-ffffffffffff", OccurredAt: time.Date(2026, 8, 11, 6, 59, 0, 0, time.UTC),
	}
	commentID, parentID, content := "comment", "member", "Test received"
	row.AgentCommentID, row.AgentCommentParent, row.AgentCommentContent = &commentID, &parentID, &content
	body, err := json.Marshal(row.payload())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	expectedSHA := hex.EncodeToString(digest[:])
	secret := "0123456789abcdef0123456789abcdef"
	now := time.Unix(1_786_431_600, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		if req.Header.Get("X-Multica-Event-ID") != row.EventID || req.Header.Get("X-Multica-Key-ID") != "test-key" {
			t.Fatal("replay identity headers changed")
		}
		timestamp := req.Header.Get("X-Multica-Timestamp")
		if req.Header.Get("X-Multica-Signature") != "sha256="+Sign(secret, timestamp, body) {
			t.Fatal("replay signature does not cover the exact original bytes")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"data":{"event_id":"` + row.EventID + `","replay":true,"response_state":"responding","recorded_at":"` + recordedAt + `"}}`))
	}))
	defer server.Close()
	result, err := sendDeliveredCommentReplay(context.Background(), Config{
		WebhookURL: server.URL, HMACSecret: secret, HMACKeyID: "test-key",
		HTTPClient: server.Client(), Now: func() time.Time { return now },
	}, row, expectedSHA, recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Replay || result.EventID != row.EventID || result.PayloadSHA256 != expectedSHA || result.RecordedAt != recordedAt {
		t.Fatalf("unexpected replay result: %+v", result)
	}
}
