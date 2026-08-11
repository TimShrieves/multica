package strikeflowresponse

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	testWorkspace = "ca4961ec-70fa-4738-8167-1331d82ebb21"
	testProject   = "d98f5700-8946-4054-b763-001d85767036"
	testRecipient = "92008a79-f6ce-438d-b60f-4dd6580f94e4"
	testAgent     = "eb361a09-be12-4626-9d03-faadc99a3933"
	testSTR94     = protectedSTR94IssueID
	testSTR166    = protectedSTR166IssueID
	testSTR172    = protectedSTR172IssueID
	testCommand   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func validConfig() Config {
	return Config{
		Enabled: true, WebhookURL: "https://strikeflow.example.test/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "multica-v1",
		WorkspaceID: testWorkspace, WorkspaceKey: "strike", ProjectIDs: []string{testProject},
		AuthorizationMode: AuthorizationModeExplicitCommands,
		CommandIDs:        []string{testCommand},
		RecipientID:       testRecipient, AgentID: testAgent, STR94IssueID: testSTR94,
		ExcludedIssueIDs: []string{testSTR94, testSTR166, testSTR172},
		NotBefore:        time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
	}
}

func TestConfigDisabledNeedsNoSecrets(t *testing.T) {
	if err := (Config{Enabled: false}).Validate(); err != nil {
		t.Fatalf("disabled config must remain dormant: %v", err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", "false")
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET", validConfig().HMACSecret)
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("disabled configuration retained a raw signing secret")
	}
}

func TestConfigEnabledRequiresEveryExactScope(t *testing.T) {
	fields := []struct {
		name string
		edit func(*Config)
	}{
		{"webhook", func(c *Config) { c.WebhookURL = "http://strikeflow.example.test/api/responses" }},
		{"webhook path", func(c *Config) { c.WebhookURL = "https://strikeflow.example.test/api/responses" }},
		{"secret", func(c *Config) { c.HMACSecret = "short" }},
		{"key id", func(c *Config) { c.HMACKeyID = "bad key" }},
		{"workspace", func(c *Config) { c.WorkspaceID = "" }},
		{"workspace key", func(c *Config) { c.WorkspaceKey = "" }},
		{"projects", func(c *Config) { c.ProjectIDs = nil }},
		{"authorization mode", func(c *Config) { c.AuthorizationMode = "" }},
		{"commands", func(c *Config) { c.CommandIDs = nil }},
		{"recipient", func(c *Config) { c.RecipientID = "" }},
		{"agent", func(c *Config) { c.AgentID = "" }},
		{"STR-94 exclusion", func(c *Config) { c.STR94IssueID = "" }},
		{"not before", func(c *Config) { c.NotBefore = time.Time{} }},
	}
	for _, test := range fields {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.edit(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("expected fail-closed validation error")
			}
		})
	}
}

func TestConfigEnabledAcceptsExactValidScope(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("exact enabled config rejected: %v", err)
	}
}

func TestConfigAuthorizationModesAreExplicitAndNonAmbiguous(t *testing.T) {
	explicit := validConfig()
	if err := explicit.Validate(); err != nil {
		t.Fatalf("explicit_commands mode rejected: %v", err)
	}

	lineage := validConfig()
	lineage.AuthorizationMode = AuthorizationModeReceiptLineage
	lineage.CommandIDs = nil
	if err := lineage.Validate(); err != nil {
		t.Fatalf("receipt_lineage mode rejected: %v", err)
	}

	lineage.CommandIDs = []string{testCommand}
	if err := lineage.Validate(); err == nil {
		t.Fatal("receipt_lineage accepted an ambiguous command allowlist")
	}

	unknown := validConfig()
	unknown.AuthorizationMode = "all_receipts"
	if err := unknown.Validate(); err == nil {
		t.Fatal("unknown authorization mode was accepted")
	}
}

func TestHistoricalExclusionsAlwaysContainSTR94AndBindAsAnArray(t *testing.T) {
	config := validConfig()
	config.ExcludedIssueIDs = []string{testSTR166, testSTR172}
	if err := config.Validate(); err == nil {
		t.Fatal("historical exclusions omitted STR-94")
	}
	if !strings.Contains(recoverAgentCommentsSQL, "i.id<>ALL($5::uuid[])") ||
		!strings.Contains(recoverTaskCompletionsSQL, "i.id<>ALL($5::uuid[])") {
		t.Fatal("recovery queries do not exclude the entire protected issue ledger")
	}
	config = validConfig()
	config.ExcludedIssueIDs = []string{testSTR94, testSTR94, testSTR166}
	if err := config.Validate(); err == nil {
		t.Fatal("duplicate exclusions hid a missing protected issue")
	}
}

func TestConfigFromEnvRequiresExactRFC3339Cutoff(t *testing.T) {
	previousOwner := requiredSecretOwnerUID
	requiredSecretOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() { requiredSecretOwnerUID = previousOwner })
	secretFile := filepath.Join(t.TempDir(), "response-hmac")
	if err := os.WriteFile(secretFile, []byte(validConfig().HMACSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", "true")
	t.Setenv("STRIKEFLOW_RESPONSE_WEBHOOK_URL", validConfig().WebhookURL)
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE", secretFile)
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_KEY_ID", validConfig().HMACKeyID)
	t.Setenv("STRIKEFLOW_RESPONSE_WORKSPACE_ID", testWorkspace)
	t.Setenv("STRIKEFLOW_RESPONSE_WORKSPACE_KEY", "strike")
	t.Setenv("STRIKEFLOW_RESPONSE_PROJECT_IDS", testProject)
	t.Setenv("STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE", AuthorizationModeExplicitCommands)
	t.Setenv("STRIKEFLOW_RESPONSE_COMMAND_IDS", testCommand)
	t.Setenv("STRIKEFLOW_RESPONSE_RECIPIENT_ID", testRecipient)
	t.Setenv("STRIKEFLOW_RESPONSE_AGENT_ID", testAgent)
	t.Setenv("STRIKEFLOW_RESPONSE_STR94_ISSUE_ID", testSTR94)
	t.Setenv("STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS", testSTR94+","+testSTR166+","+testSTR172)
	t.Setenv("STRIKEFLOW_RESPONSE_NOT_BEFORE", "2026-08-08 00:00:00")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("non-RFC3339 activation cutoff was accepted")
	}
	t.Setenv("STRIKEFLOW_RESPONSE_NOT_BEFORE", "2026-08-08T00:00:00Z")
	config, err := ConfigFromEnv()
	if err != nil || !config.NotBefore.Equal(time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("RFC3339 activation cutoff rejected: config=%v err=%v", config.NotBefore, err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE", AuthorizationModeReceiptLineage)
	t.Setenv("STRIKEFLOW_RESPONSE_COMMAND_IDS", "")
	config, err = ConfigFromEnv()
	if err != nil || config.AuthorizationMode != AuthorizationModeReceiptLineage || len(config.CommandIDs) != 0 {
		t.Fatalf("receipt_lineage environment rejected: mode=%q commands=%v err=%v", config.AuthorizationMode, config.CommandIDs, err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("enabled environment accepted a missing historical exclusion ledger")
	}
	t.Setenv("STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS", testSTR94+","+testSTR166+","+testSTR172)
	t.Setenv("STRIKEFLOW_RESPONSE_NOT_BEFORE", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("future activation cutoff was accepted")
	}
}

func TestConfigFromEnvRejectsRawSecretAndWhitespaceFile(t *testing.T) {
	previousOwner := requiredSecretOwnerUID
	requiredSecretOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() { requiredSecretOwnerUID = previousOwner })
	t.Setenv("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", "true")
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET", validConfig().HMACSecret)
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("raw signing secret env was accepted")
	}
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET", "")
	secretFile := filepath.Join(t.TempDir(), "response-hmac")
	if err := os.WriteFile(secretFile, []byte(validConfig().HMACSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE", secretFile)
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("signing secret file with surrounding whitespace was accepted")
	}
}

func TestConfigFromEnvRejectsUnsafeSecretFileMetadata(t *testing.T) {
	previousOwner := requiredSecretOwnerUID
	requiredSecretOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() { requiredSecretOwnerUID = previousOwner })
	directory := t.TempDir()
	secretFile := filepath.Join(directory, "response-hmac")
	if err := os.WriteFile(secretFile, []byte(validConfig().HMACSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", "true")
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE", secretFile)
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("world-readable signing secret was accepted")
	}
	if err := os.Chmod(secretFile, 0o600); err != nil {
		t.Fatal(err)
	}
	secretLink := filepath.Join(directory, "response-hmac-link")
	if err := os.Symlink(secretFile, secretLink); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE", secretLink)
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("symlink signing secret was accepted")
	}
}

func TestSignCoversTimestampEventIDAndExactRawBody(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	timestamp := "1786172400"
	body := []byte(`{"event_type":"task.completed","command_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if got := Sign(secret, timestamp, body); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if Sign(secret, timestamp, append(body, '\n')) == want {
		t.Fatal("signature did not cover exact raw body")
	}
	if Sign(secret, timestamp+"1", body) == want {
		t.Fatal("signature did not cover timestamp")
	}
}

func TestNotifyEventOnlyWakesForApprovedEvents(t *testing.T) {
	p := &Publisher{wake: make(chan struct{}, 1)}
	p.NotifyEvent(events.Event{Type: protocol.EventIssueUpdated})
	select {
	case <-p.wake:
		t.Fatal("unapproved event woke response publisher")
	default:
	}
	p.NotifyEvent(events.Event{Type: protocol.EventCommentCreated})
	select {
	case <-p.wake:
	default:
		t.Fatal("comment:created did not wake response publisher")
	}
	p.NotifyEvent(events.Event{Type: protocol.EventTaskCompleted})
	select {
	case <-p.wake:
	default:
		t.Fatal("task:completed did not wake response publisher")
	}
}

func TestRetryDelayIsBounded(t *testing.T) {
	if got := retryDelay(1); got != 5*time.Second {
		t.Fatalf("first retry = %s", got)
	}
	if got := retryDelay(100); got != 15*time.Minute {
		t.Fatalf("bounded retry = %s", got)
	}
}

func TestOnlyBoundedHTTPFailuresAreTransient(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if !isTransientStatus(status) {
			t.Fatalf("status %d must remain transient", status)
		}
	}
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge} {
		if isTransientStatus(status) {
			t.Fatalf("status %d must require permanent attention", status)
		}
	}
}

func TestAcknowledgementRequiresExactEventIdentity(t *testing.T) {
	eventID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if !validAcknowledgement([]byte(`{"ok":true,"data":{"event_id":"`+eventID+`"}}`), eventID) {
		t.Fatal("exact successful acknowledgement was rejected")
	}
	for name, body := range map[string][]byte{
		"malformed":        []byte(`{"ok":`),
		"negative":         []byte(`{"ok":false,"data":{"event_id":"` + eventID + `"}}`),
		"missing identity": []byte(`{"ok":true,"data":{}}`),
		"wrong identity":   []byte(`{"ok":true,"data":{"event_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}`),
		"oversized":        []byte(strings.Repeat("x", 4097)),
	} {
		t.Run(name, func(t *testing.T) {
			if validAcknowledgement(body, eventID) {
				t.Fatal("invalid acknowledgement was accepted")
			}
		})
	}
}

func TestDeliveryQueryPreservesParentBeforeChildBeforeCompletion(t *testing.T) {
	for _, contract := range []string{
		"o.agent_comment_parent_id=o.member_comment_id",
		"parent.agent_comment_id=o.agent_comment_parent_id",
		"parent.delivered_at IS NOT NULL",
		"o.event_type='task.completed' AND NOT EXISTS",
		"comment.delivered_at IS NULL",
		"CASE WHEN o.event_type='task.completed' THEN 1 ELSE 0 END",
	} {
		if !strings.Contains(deliverCandidateSQL, contract) {
			t.Fatalf("delivery ordering query is missing %q", contract)
		}
	}
}

func TestPayloadMatchesStrikeFlowStrictWireShape(t *testing.T) {
	commentID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	actualParentID := "22222222-2222-4222-8222-222222222222"
	content := "Test received"
	row := outboxRow{
		EventID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", EventType: "agent_comment.created",
		CommandID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", WorkspaceKey: "strike",
		ProjectID: testProject, IssueID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		IssueIdentifier: "STR-172", RootCommentID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		MemberCommentID:    "ffffffff-ffff-4fff-8fff-ffffffffffff",
		ContinuationTaskID: "11111111-1111-4111-8111-111111111111",
		RecipientID:        testRecipient, AgentID: testAgent, AgentCommentID: &commentID,
		AgentCommentParent: &actualParentID, AgentCommentContent: &content,
		OccurredAt: time.Date(2026, 8, 8, 7, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(row.payload())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"agent_id", "command_id", "comment_id", "event_type", "issue_identifier", "occurred_at", "parent_comment_id", "project_id", "recipient_id", "reply_root_id", "response_text", "source_issue_id", "task_id", "workspace_key"}
	gotKeys := make([]string, 0, len(got))
	for key := range got {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("payload keys = %v, want %v", gotKeys, wantKeys)
	}
	if got["comment_id"] != commentID || got["response_text"] != content {
		t.Fatalf("agent comment binding missing from payload: %s", raw)
	}
	if got["parent_comment_id"] != actualParentID {
		t.Fatalf("chained agent comment parent = %v, want %s", got["parent_comment_id"], actualParentID)
	}
}

func TestCrossRepositoryWireAndHMACFixture(t *testing.T) {
	commentID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	parentID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	content := "Test received"
	row := outboxRow{
		EventID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", EventType: "agent_comment.created",
		CommandID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", WorkspaceKey: "strike",
		ProjectID: testProject, IssueID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		IssueIdentifier: "STR-173", RootCommentID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		MemberCommentID: parentID, ContinuationTaskID: "11111111-1111-4111-8111-111111111111",
		RecipientID: testRecipient, AgentID: testAgent, AgentCommentID: &commentID,
		AgentCommentParent: &parentID, AgentCommentContent: &content,
		OccurredAt: time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(row.payload())
	if err != nil {
		t.Fatal(err)
	}
	wantBody := `{"event_type":"agent_comment.created","command_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","workspace_key":"strike","project_id":"d98f5700-8946-4054-b763-001d85767036","source_issue_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","issue_identifier":"STR-173","reply_root_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","recipient_id":"92008a79-f6ce-438d-b60f-4dd6580f94e4","agent_id":"eb361a09-be12-4626-9d03-faadc99a3933","task_id":"11111111-1111-4111-8111-111111111111","parent_comment_id":"ffffffff-ffff-4fff-8fff-ffffffffffff","comment_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","response_text":"Test received","occurred_at":"2026-08-08T08:00:00Z"}`
	if string(raw) != wantBody {
		t.Fatalf("wire body = %s, want %s", raw, wantBody)
	}
	const timestamp = "1786176000"
	const wantSignature = "d837937568916815ee04786094cd817e4bc4f09d942df80d1dc2721135465a20"
	if got := Sign(validConfig().HMACSecret, timestamp, raw); got != wantSignature {
		t.Fatalf("fixture signature = %s, want %s", got, wantSignature)
	}
}

func TestCompletionPayloadUsesTaskIdentityAndNoResponseText(t *testing.T) {
	row := outboxRow{
		EventID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", EventType: "task.completed",
		CommandID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", WorkspaceKey: "strike",
		ContinuationTaskID: "11111111-1111-4111-8111-111111111111",
	}
	raw, err := json.Marshal(row.payload())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["comment_id"] != row.ContinuationTaskID {
		t.Fatalf("completion comment_id = %v, want continuation task identity", got["comment_id"])
	}
	if _, exists := got["response_text"]; exists {
		t.Fatal("task.completed must not include response_text")
	}
	row.EventID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if raced := row.payload(); raced.CommentID != row.ContinuationTaskID {
		t.Fatalf("completion retry/race identity = %s, want %s", raced.CommentID, row.ContinuationTaskID)
	}
}
