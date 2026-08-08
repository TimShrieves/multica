package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/strikeflowresponse"
	"github.com/multica-ai/multica/server/internal/realtime"
)

type strikeFlowIntegrationFixture struct {
	projectID, otherProjectID string
	issueID, rootID, itemID   string
	valid, wrongProject       string
	wrongRecipient            string
	expired, revoked          string
	readOnly                  string
}

func seedStrikeFlowIntegrationFixture(t *testing.T) strikeFlowIntegrationFixture {
	t.Helper()
	ctx := t.Context()
	var f strikeFlowIntegrationFixture
	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id,runtime_id FROM agent WHERE workspace_id=$1 ORDER BY created_at LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*string{&f.projectID, &f.otherProjectID} {
		if err := testPool.QueryRow(ctx,
			`INSERT INTO project(workspace_id,title) VALUES($1,$2) RETURNING id`,
			testWorkspaceID, "StrikeFlow scoped integration").Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue(workspace_id,title,status,priority,assignee_type,assignee_id,
			creator_type,creator_id,position,project_id,number)
		VALUES($1,'Scoped inbox integration','done','none','agent',$2,'member',$3,0,$4,
			COALESCE((SELECT max(number)+1 FROM issue WHERE workspace_id=$1),1))
		RETURNING id
	`, testWorkspaceID, agentID, testUserID, f.projectID).Scan(&f.issueID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type)
		VALUES($1,$2,'agent',$3,'Review requested','comment') RETURNING id
	`, f.issueID, testWorkspaceID, agentID).Scan(&f.rootID); err != nil {
		t.Fatal(err)
	}
	var sourceTaskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue(
			agent_id,runtime_id,issue_id,status,delivered_comment_ids,originator_user_id,
			accountable_user_id,originator_source,trigger_evidence_kind,trigger_evidence_ref_id,
			completed_at
		) VALUES($1,$2,$3,'completed',ARRAY[$4]::uuid[],$5,$5,'direct_human','comment',$4,now())
		RETURNING id
	`, agentID, runtimeID, f.issueID, f.rootID, testUserID).Scan(&sourceTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE comment SET source_task_id=$1 WHERE id=$2`, sourceTaskID, f.rootID); err != nil {
		t.Fatal(err)
	}
	details, _ := json.Marshal(map[string]string{"comment_id": f.rootID})
	if err := testPool.QueryRow(ctx, `
		INSERT INTO inbox_item(workspace_id,recipient_type,recipient_id,type,severity,
			issue_id,title,body,actor_type,actor_id,details)
		VALUES($1,'member',$2,'agent_action_required','action_required',$3,
			'Review requested','Please review','agent',$4,$5)
		RETURNING id
	`, testWorkspaceID, testUserID, f.issueID, agentID, details).Scan(&f.itemID); err != nil {
		t.Fatal(err)
	}
	var otherUserID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user"(name,email) VALUES('Scoped Other',$1) RETURNING id
	`, "scoped-other-"+f.itemID+"@example.invalid").Scan(&otherUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO member(workspace_id,user_id,role) VALUES($1,$2,'member')`,
		testWorkspaceID, otherUserID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, otherUserID)
	})

	insertToken := func(name string, recipient string, projects []string, scopes []string, created, expires time.Time, revoked bool) string {
		plain := "msc_" + name + "_" + f.itemID + "_integration"
		_, err := testPool.Exec(ctx, `
			INSERT INTO strikeflow_connector_token(
				workspace_id,recipient_id,name,token_hash,token_prefix,project_ids,scopes,
				created_at,expires_at,revoked_at,created_by
			) VALUES($1,$2,$3,$4,'msc_test',$5,$6,$7,$8,
				CASE WHEN $9 THEN now() ELSE NULL END,$10)
		`, testWorkspaceID, recipient, name, auth.HashToken(plain), projects, scopes,
			created, expires, revoked, testUserID)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	now := time.Now()
	allScopes := []string{"inbox:read", "inbox:read_receipt", "inbox:archive", "inbox:reply"}
	f.valid = insertToken("valid", testUserID, []string{f.projectID}, allScopes, now, now.Add(24*time.Hour), false)
	f.wrongProject = insertToken("project", testUserID, []string{f.otherProjectID}, allScopes, now, now.Add(24*time.Hour), false)
	f.wrongRecipient = insertToken("recipient", otherUserID, []string{f.projectID}, allScopes, now, now.Add(24*time.Hour), false)
	f.expired = insertToken("expired", testUserID, []string{f.projectID}, allScopes, now.Add(-2*time.Hour), now.Add(-time.Hour), false)
	f.revoked = insertToken("revoked", testUserID, []string{f.projectID}, allScopes, now, now.Add(24*time.Hour), true)
	f.readOnly = insertToken("readonly", testUserID, []string{f.projectID}, []string{"inbox:read"}, now, now.Add(24*time.Hour), false)
	return f
}

func strikeFlowRequest(t *testing.T, token, method, path string, body any) *http.Response {
	return strikeFlowRequestAt(t, testServer.URL, token, method, path, body)
}

func strikeFlowRequestAt(t *testing.T, serverURL, token, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, serverURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestStrikeFlowConnectorCredentialRoutesRequireHumanActor(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	base := "/api/workspaces/" + testWorkspaceID + "/strikeflow-connector-tokens"
	createBody := map[string]any{
		"name": "single-item activation test", "recipient_id": testUserID,
		"project_ids": []string{f.projectID},
		"scopes":      []string{"inbox:read", "inbox:read_receipt", "inbox:archive", "inbox:reply"},
		"expires_at":  time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}

	resp := strikeFlowRequest(t, testToken, http.MethodPost, base, createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("human create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	readJSON(t, resp, &created)
	if created.ID == "" {
		t.Fatal("human create returned no token id")
	}

	var agentID, runtimeID string
	if err := testPool.QueryRow(t.Context(),
		`SELECT id,runtime_id FROM agent WHERE workspace_id=$1 ORDER BY created_at LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := testPool.QueryRow(t.Context(), `
		INSERT INTO agent_task_queue(agent_id,runtime_id,status,priority)
		VALUES($1,$2,'queued',0) RETURNING id
	`, agentID, runtimeID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	taskToken := "mat_strikeflow_credential_guard_" + f.itemID
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO task_token(token_hash,task_id,agent_id,workspace_id,user_id,expires_at)
		VALUES($1,$2,$3,$4,$5,now()+interval '1 hour')
	`, auth.HashToken(taskToken), taskID, agentID, testWorkspaceID, testUserID); err != nil {
		t.Fatal(err)
	}

	fleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/pat/verify" {
			t.Errorf("unexpected fleet request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"valid":true,"owner_id":"`+testUserID+`","instance_id":"test","instance_record_id":"test"}`)
	}))
	defer fleet.Close()
	t.Setenv("MULTICA_CLOUD_FLEET_URL", fleet.URL)
	hub := realtime.NewHub()
	go hub.Run()
	bus := events.New()
	registerListeners(bus, hub)
	cloudServer := httptest.NewServer(NewRouter(testPool, hub, bus, analytics.NoopClient{}, nil))
	defer cloudServer.Close()

	machineActors := []struct {
		name, serverURL, token string
	}{
		{name: "task_token", serverURL: testServer.URL, token: taskToken},
		{name: "cloud_pat", serverURL: cloudServer.URL, token: "mcn_strikeflow_credential_guard"},
	}
	rotateBody := map[string]any{
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	for _, actor := range machineActors {
		t.Run(actor.name, func(t *testing.T) {
			for _, request := range []struct {
				method, path string
				body         any
			}{
				{method: http.MethodPost, path: base, body: createBody},
				{method: http.MethodPost, path: base + "/" + created.ID + "/rotate", body: rotateBody},
				{method: http.MethodDelete, path: base + "/" + created.ID},
			} {
				resp := strikeFlowRequestAt(t, actor.serverURL, actor.token, request.method, request.path, request.body)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("%s %s = %d, want 403", request.method, request.path, resp.StatusCode)
				}
				resp.Body.Close()
			}
		})
	}

	resp = strikeFlowRequest(t, testToken, http.MethodPost, base+"/"+created.ID+"/rotate", rotateBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("human rotate = %d, want 201", resp.StatusCode)
	}
	var rotated struct {
		ID string `json:"id"`
	}
	readJSON(t, resp, &rotated)
	resp = strikeFlowRequest(t, testToken, http.MethodDelete, base+"/"+rotated.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("human revoke = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStrikeFlowConnectorMutationAuditFailureRollsBack(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	if _, err := testPool.Exec(t.Context(), `
		ALTER TABLE strikeflow_connector_audit
		ADD CONSTRAINT strikeflow_test_reject_audit CHECK (false) NOT VALID
	`); err != nil {
		t.Fatal(err)
	}
	dropConstraint := func() {
		_, _ = testPool.Exec(context.Background(), `
			ALTER TABLE strikeflow_connector_audit
			DROP CONSTRAINT IF EXISTS strikeflow_test_reject_audit
		`)
	}
	t.Cleanup(dropConstraint)

	base := "/api/integrations/strikeflow/inbox/" + f.itemID
	for _, suffix := range []string{"/read", "/archive"} {
		resp := strikeFlowRequest(t, f.valid, http.MethodPost, base+suffix, nil)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s audit failure = %d, want 500", suffix, resp.StatusCode)
		}
		resp.Body.Close()
	}
	var read, archived bool
	if err := testPool.QueryRow(t.Context(),
		`SELECT read,archived FROM inbox_item WHERE id=$1`, f.itemID).
		Scan(&read, &archived); err != nil || read || archived {
		t.Fatalf("audit failure persisted inbox mutation: read=%v archived=%v err=%v", read, archived, err)
	}

	key := "00000000-0000-4000-8000-000000000097"
	resp := strikeFlowRequest(t, f.valid, http.MethodPost, base+"/replies",
		map[string]any{"idempotency_key": key, "strikeflow_command_id": "10000000-0000-4000-8000-000000000001", "message": "Audit must commit with this reply."})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("reply audit failure = %d, want 500", resp.StatusCode)
	}
	resp.Body.Close()
	var comments, receipts int
	marker := "[strikeflow-agent-inbox:" + key + "]"
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM comment WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'`,
		f.issueID, marker).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM strikeflow_connector_reply_receipt WHERE idempotency_key=$1`,
		key).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if comments != 0 || receipts != 0 {
		t.Fatalf("audit failure persisted reply state: comments=%d receipts=%d", comments, receipts)
	}

	dropConstraint()
	resp = strikeFlowRequest(t, f.valid, http.MethodPost, base+"/replies",
		map[string]any{"idempotency_key": key, "strikeflow_command_id": "10000000-0000-4000-8000-000000000001", "message": "Audit must commit with this reply."})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry after audit recovery = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStrikeFlowConnectorSecurityBoundary(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	base := "/api/integrations/strikeflow"

	resp := strikeFlowRequest(t, f.valid, http.MethodGet, base+"/inbox", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid list = %d", resp.StatusCode)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	readJSON(t, resp, &list)
	if len(list.Items) != 1 || list.Items[0]["id"] != f.itemID {
		t.Fatalf("valid list escaped binding: %#v", list.Items)
	}

	for _, suffix := range []string{"/issue", "/thread"} {
		resp = strikeFlowRequest(t, f.valid, http.MethodGet, base+"/inbox/"+f.itemID+suffix, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("valid %s = %d", suffix, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp = strikeFlowRequest(t, f.wrongProject, http.MethodGet, base+"/inbox", nil)
	readJSON(t, resp, &list)
	if len(list.Items) != 0 {
		t.Fatal("wrong-project principal listed item")
	}
	for _, token := range []string{f.wrongProject, f.wrongRecipient} {
		resp = strikeFlowRequest(t, token, http.MethodGet, base+"/inbox/"+f.itemID+"/issue", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("cross-boundary item read = %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	for _, token := range []string{f.expired, f.revoked} {
		resp = strikeFlowRequest(t, token, http.MethodGet, base+"/inbox", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expired/revoked auth = %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = strikeFlowRequest(t, f.readOnly, http.MethodPost, base+"/inbox/"+f.itemID+"/replies",
		map[string]any{"idempotency_key": "00000000-0000-4000-8000-000000000099", "strikeflow_command_id": "10000000-0000-4000-8000-000000000099", "message": "No"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing reply scope = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = strikeFlowRequest(t, f.valid, http.MethodPost, base+"/inbox/"+f.itemID+"/read", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid read receipt = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = strikeFlowRequest(t, f.valid, http.MethodPost, base+"/inbox/"+f.itemID+"/archive", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid archive = %d", resp.StatusCode)
	}
	resp.Body.Close()
	var read, archived bool
	if err := testPool.QueryRow(t.Context(),
		`SELECT read,archived FROM inbox_item WHERE id=$1`, f.itemID).
		Scan(&read, &archived); err != nil || !read || !archived {
		t.Fatalf("read/archive not persisted: read=%v archived=%v err=%v", read, archived, err)
	}
}

func TestStrikeFlowConnectorReplyIdempotency(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	path := "/api/integrations/strikeflow/inbox/" + f.itemID + "/replies"
	key := "00000000-0000-4000-8000-000000000098"
	body := map[string]any{"idempotency_key": key, "strikeflow_command_id": "10000000-0000-4000-8000-000000000002", "message": "Please revise this."}

	resp := strikeFlowRequest(t, f.valid, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first reply = %d", resp.StatusCode)
	}
	var first map[string]any
	readJSON(t, resp, &first)
	var storedCommandID string
	if err := testPool.QueryRow(t.Context(), `
		SELECT strikeflow_command_id::text FROM strikeflow_connector_reply_receipt
		WHERE idempotency_key=$1
	`, key).Scan(&storedCommandID); err != nil || storedCommandID != body["strikeflow_command_id"] {
		t.Fatalf("immutable command binding = %q err=%v", storedCommandID, err)
	}
	resp = strikeFlowRequest(t, f.valid, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay = %d", resp.StatusCode)
	}
	var replay map[string]any
	readJSON(t, resp, &replay)
	if first["comment_id"] != replay["comment_id"] || replay["replayed"] != true {
		t.Fatalf("bad replay receipt: first=%v replay=%v", first, replay)
	}
	body["message"] = "Different payload."
	resp = strikeFlowRequest(t, f.valid, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting replay = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStrikeFlowConnectorLegacyReplyHashReplayAndPublisherExclusion(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	path := "/api/integrations/strikeflow/inbox/" + f.itemID + "/replies"
	key := "00000000-0000-4000-8000-000000000097"
	message := "Legacy reply remains unchanged."
	resp := strikeFlowRequest(t, f.valid, http.MethodPost, path,
		map[string]any{"idempotency_key": key, "message": message})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("legacy reply = %d", resp.StatusCode)
	}
	var first map[string]any
	readJSON(t, resp, &first)
	var tokenID, storedHash string
	var commandIsNull bool
	if err := testPool.QueryRow(t.Context(), `
		SELECT token_id::text,payload_hash,strikeflow_command_id IS NULL
		FROM strikeflow_connector_reply_receipt WHERE idempotency_key=$1
	`, key).Scan(&tokenID, &storedHash, &commandIsNull); err != nil {
		t.Fatal(err)
	}
	legacyHashInput := strings.Join([]string{tokenID, f.itemID, f.issueID, f.rootID, message}, "\x00")
	wantHashBytes := sha256.Sum256([]byte(legacyHashInput))
	if wantHash := hex.EncodeToString(wantHashBytes[:]); !commandIsNull || storedHash != wantHash {
		t.Fatalf("legacy binding/hash changed: null=%v hash=%s want=%s", commandIsNull, storedHash, wantHash)
	}
	resp = strikeFlowRequest(t, f.valid, http.MethodPost, path,
		map[string]any{"idempotency_key": key, "message": message})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy replay = %d", resp.StatusCode)
	}
	var replay map[string]any
	readJSON(t, resp, &replay)
	if replay["comment_id"] != first["comment_id"] || replay["replayed"] != true {
		t.Fatalf("legacy replay changed: first=%v replay=%v", first, replay)
	}
	resp = strikeFlowRequest(t, f.valid, http.MethodPost, path, map[string]any{
		"idempotency_key": key, "strikeflow_command_id": "10000000-0000-4000-8000-000000000003", "message": message,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("legacy key rebound to command = %d", resp.StatusCode)
	}
	resp.Body.Close()

	taskID, _ := first["task_id"].(string)
	commentID, _ := first["comment_id"].(string)
	var agentID string
	if taskID == "" || commentID == "" || testPool.QueryRow(t.Context(), `SELECT agent_id::text FROM agent_task_queue WHERE id=$1`, taskID).Scan(&agentID) != nil {
		t.Fatalf("legacy continuation receipt incomplete: %v", first)
	}
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,parent_id,source_task_id)
		VALUES($1,$2,'agent',$3,'Legacy response','comment',$4,$5)
	`, f.issueID, testWorkspaceID, agentID, commentID, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	publisher, err := strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: "https://strikeflow.example.test/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: "11111111-1111-4111-8111-111111111194",
		NotBefore: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.RecoverOnce(t.Context()); err != nil {
		t.Fatalf("legacy exclusion recovery failed: %v", err)
	}
	var outboxCount int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM strikeflow_response_outbox WHERE continuation_task_id=$1`, taskID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("legacy receipt entered response outbox: count=%d err=%v", outboxCount, err)
	}
}

func TestStrikeFlowResponseRecoveryRequiresExactConnectorLineage(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	path := "/api/integrations/strikeflow/inbox/" + f.itemID + "/replies"
	commandID := "10000000-0000-4000-8000-000000000005"
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM strikeflow_response_outbox WHERE strikeflow_command_id=$1`, commandID)
	})
	resp := strikeFlowRequest(t, f.valid, http.MethodPost, path, map[string]any{
		"idempotency_key":       "00000000-0000-4000-8000-000000000096",
		"strikeflow_command_id": commandID,
		"message":               "Create an exact-bound continuation.",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bound reply = %d", resp.StatusCode)
	}
	var receipt map[string]any
	readJSON(t, resp, &receipt)
	commentID, _ := receipt["comment_id"].(string)
	taskID, _ := receipt["task_id"].(string)
	if commentID == "" || taskID == "" {
		t.Fatalf("missing connector continuation binding: %v", receipt)
	}
	var agentID string
	if err := testPool.QueryRow(t.Context(), `SELECT agent_id::text FROM agent_task_queue WHERE id=$1`, taskID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	var agentCommentID string
	if err := testPool.QueryRow(t.Context(), `
		INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,parent_id,source_task_id)
		VALUES($1,$2,'agent',$3,'Exact response','comment',$4,$5) RETURNING id
	`, f.issueID, testWorkspaceID, agentID, commentID, taskID).Scan(&agentCommentID); err != nil {
		t.Fatal(err)
	}
	var chainedAgentCommentID string
	if err := testPool.QueryRow(t.Context(), `
		INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,parent_id,source_task_id)
		VALUES($1,$2,'agent',$3,'Chained exact response','comment',$4,$5) RETURNING id
	`, f.issueID, testWorkspaceID, agentID, agentCommentID, taskID).Scan(&chainedAgentCommentID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `
		UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1
	`, taskID); err != nil {
		t.Fatal(err)
	}
	excludedPublisher, err := strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: "https://strikeflow.example.test/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: f.issueID,
		NotBefore: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := excludedPublisher.RecoverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var excludedCount int
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM strikeflow_response_outbox WHERE strikeflow_command_id=$1
	`, commandID).Scan(&excludedCount); err != nil || excludedCount != 0 {
		t.Fatalf("STR-94-excluded outbox rows = %d err=%v", excludedCount, err)
	}
	futurePublisher, err := strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: "https://strikeflow.example.test/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: "11111111-1111-4111-8111-111111111194",
		NotBefore: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := futurePublisher.RecoverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM strikeflow_response_outbox WHERE strikeflow_command_id=$1
	`, commandID).Scan(&excludedCount); err != nil || excludedCount != 0 {
		t.Fatalf("pre-activation response entered outbox: count=%d err=%v", excludedCount, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE agent_task_queue SET trigger_evidence_kind='issue_assignment' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	provenancePublisher, err := strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: "https://strikeflow.example.test/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: "11111111-1111-4111-8111-111111111194",
		NotBefore: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := provenancePublisher.RecoverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM strikeflow_response_outbox WHERE strikeflow_command_id=$1
	`, commandID).Scan(&excludedCount); err != nil || excludedCount != 0 {
		t.Fatalf("wrong task provenance entered outbox: count=%d err=%v", excludedCount, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE agent_task_queue SET trigger_evidence_kind='comment' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	publisher, err := strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: "https://strikeflow.example.test/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: "11111111-1111-4111-8111-111111111194",
		NotBefore: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.RecoverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM strikeflow_response_outbox
		WHERE strikeflow_command_id=$1 AND member_comment_id=$2 AND continuation_task_id=$3
		  AND (event_type='task.completed' OR agent_comment_id=ANY($4::uuid[]))
	`, commandID, commentID, taskID, []string{agentCommentID, chainedAgentCommentID}).Scan(&count); err != nil || count != 3 {
		t.Fatalf("exact response outbox rows = %d err=%v", count, err)
	}
	type deliveredEvent struct {
		eventType string
		err       string
	}
	deliveries := make(chan deliveredEvent, 3)
	webhook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		observation := deliveredEvent{}
		if readErr != nil {
			observation.err = readErr.Error()
		} else {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				observation.err = err.Error()
			}
			observation.eventType, _ = body["event_type"].(string)
			timestamp := r.Header.Get("X-Multica-Timestamp")
			wantSignature := "sha256=" + strikeflowresponse.Sign(
				"0123456789abcdef0123456789abcdef", timestamp, raw,
			)
			if r.URL.Path != "/api/integrations/multica/content-delivery/responses" ||
				r.Header.Get("X-Multica-Key-Id") != "test-v1" ||
				r.Header.Get("X-Multica-Signature") != wantSignature ||
				r.Header.Get("X-Multica-Event-Id") == "" {
				observation.err = "webhook route or authentication headers did not match"
			}
		}
		deliveries <- observation
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	publisher, err = strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: webhook.URL + "/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: "11111111-1111-4111-8111-111111111194",
		HTTPClient: webhook.Client(), Now: func() time.Time { return time.Unix(1786172400, 0) },
		NotBefore: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	seenTypes := map[string]bool{}
	for range 3 {
		processed, err := publisher.DeliverOnce(t.Context())
		if err != nil || !processed {
			t.Fatalf("deliver response event: processed=%v err=%v", processed, err)
		}
		observation := <-deliveries
		if observation.err != "" {
			t.Fatal(observation.err)
		}
		seenTypes[observation.eventType] = true
	}
	if !seenTypes["agent_comment.created"] || !seenTypes["task.completed"] {
		t.Fatalf("delivered event types = %v", seenTypes)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM strikeflow_response_outbox
		WHERE strikeflow_command_id=$1 AND delivered_at IS NOT NULL
	`, commandID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("delivered outbox rows = %d err=%v", count, err)
	}
	if _, err := testPool.Exec(t.Context(), `
		UPDATE strikeflow_response_outbox
		SET delivered_at=NULL,attempt_count=11,next_attempt_at=now(),needs_attention_at=NULL
		WHERE strikeflow_command_id=$1 AND event_type='task.completed'
	`, commandID); err != nil {
		t.Fatal(err)
	}
	failingWebhook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failingWebhook.Close()
	retryPublisher, err := strikeflowresponse.New(testPool, strikeflowresponse.Config{
		Enabled: true, WebhookURL: failingWebhook.URL + "/api/integrations/multica/content-delivery/responses",
		HMACSecret: "0123456789abcdef0123456789abcdef", HMACKeyID: "test-v1",
		WorkspaceID: testWorkspaceID, WorkspaceKey: "strike", ProjectIDs: []string{f.projectID},
		RecipientID: testUserID, AgentID: agentID, STR94IssueID: "11111111-1111-4111-8111-111111111194",
		HTTPClient: failingWebhook.Client(), NotBefore: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := retryPublisher.DeliverOnce(t.Context()); err != nil || !processed {
		t.Fatalf("attention transition delivery: processed=%v err=%v", processed, err)
	}
	var attempts int
	var attentionAt, nextAttempt time.Time
	if err := testPool.QueryRow(t.Context(), `
		SELECT attempt_count,needs_attention_at,next_attempt_at
		FROM strikeflow_response_outbox
		WHERE strikeflow_command_id=$1 AND event_type='task.completed'
	`, commandID).Scan(&attempts, &attentionAt, &nextAttempt); err != nil || attempts != 12 || nextAttempt.Before(attentionAt.Add(5*time.Hour)) {
		t.Fatalf("needs-attention retry state: attempts=%d attention=%v next=%v err=%v", attempts, attentionAt, nextAttempt, err)
	}
	if _, err := testPool.Exec(t.Context(), `
		UPDATE strikeflow_response_outbox SET next_attempt_at=now()
		WHERE strikeflow_command_id=$1 AND event_type='task.completed'
	`, commandID); err != nil {
		t.Fatal(err)
	}
	if processed, err := publisher.DeliverOnce(t.Context()); err != nil || !processed {
		t.Fatalf("attention recovery delivery: processed=%v err=%v", processed, err)
	}
	if observation := <-deliveries; observation.err != "" || observation.eventType != "task.completed" {
		t.Fatalf("attention recovery webhook = %+v", observation)
	}
	var attentionCleared, delivered bool
	if err := testPool.QueryRow(t.Context(), `
		SELECT needs_attention_at IS NULL,delivered_at IS NOT NULL
		FROM strikeflow_response_outbox
		WHERE strikeflow_command_id=$1 AND event_type='task.completed'
	`, commandID).Scan(&attentionCleared, &delivered); err != nil || !attentionCleared || !delivered {
		t.Fatalf("attention recovery not cleared: cleared=%v delivered=%v err=%v", attentionCleared, delivered, err)
	}
	if _, err := testPool.Exec(t.Context(), `
		UPDATE strikeflow_connector_reply_receipt
		SET strikeflow_command_id='10000000-0000-4000-8000-000000000006'
		WHERE strikeflow_command_id=$1
	`, commandID); err == nil {
		t.Fatal("immutable command binding accepted an update")
	}
}
