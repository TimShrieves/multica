package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
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
		map[string]any{"idempotency_key": key, "message": "Audit must commit with this reply."})
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
		map[string]any{"idempotency_key": key, "message": "Audit must commit with this reply."})
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
		map[string]any{"idempotency_key": "00000000-0000-4000-8000-000000000099", "message": "No"})
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
	body := map[string]any{"idempotency_key": key, "message": "Please revise this."}

	resp := strikeFlowRequest(t, f.valid, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first reply = %d", resp.StatusCode)
	}
	var first map[string]any
	readJSON(t, resp, &first)
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
