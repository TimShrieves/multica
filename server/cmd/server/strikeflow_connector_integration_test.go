package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

type strikeFlowIntegrationFixture struct {
	projectID, otherProjectID string
	issueID, rootID, itemID   string
	agentID, contentTokenID   string
	valid, wrongProject       string
	wrongRecipient            string
	expired, revoked          string
	readOnly                  string
	contentReply              string
}

func seedStrikeFlowIntegrationFixture(t *testing.T) strikeFlowIntegrationFixture {
	t.Helper()
	ctx := t.Context()
	var f strikeFlowIntegrationFixture
	var runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id,runtime_id FROM agent WHERE workspace_id=$1 ORDER BY created_at LIMIT 1`,
		testWorkspaceID).Scan(&f.agentID, &runtimeID); err != nil {
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
	`, testWorkspaceID, f.agentID, testUserID, f.projectID).Scan(&f.issueID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type)
		VALUES($1,$2,'agent',$3,'Review requested','comment') RETURNING id
	`, f.issueID, testWorkspaceID, f.agentID).Scan(&f.rootID); err != nil {
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
	`, f.agentID, runtimeID, f.issueID, f.rootID, testUserID).Scan(&sourceTaskID); err != nil {
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
	`, testWorkspaceID, testUserID, f.issueID, f.agentID, details).Scan(&f.itemID); err != nil {
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
	f.contentReply = "msc_content_" + f.itemID + "_integration"
	if err := testPool.QueryRow(ctx, `
		INSERT INTO strikeflow_connector_token(
			workspace_id,recipient_id,agent_id,name,token_hash,token_prefix,project_ids,
			scopes,created_at,expires_at,created_by
		) VALUES($1,$2,$3,'content reply',$4,'msc_test',$5,
			ARRAY['content:reply']::text[],$6,$7,$2)
		RETURNING id
	`, testWorkspaceID, testUserID, f.agentID, auth.HashToken(f.contentReply),
		[]string{f.projectID}, now, now.Add(24*time.Hour)).Scan(&f.contentTokenID); err != nil {
		t.Fatal(err)
	}
	return f
}

func strikeFlowTestRootHash(rootID string) string {
	raw, _ := json.Marshal(map[string]string{"reply_root_id": rootID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func strikeFlowContentReplyBody(f strikeFlowIntegrationFixture, key, message string) map[string]any {
	return map[string]any{
		"workspace_id":         testWorkspaceID,
		"recipient_id":         testUserID,
		"project_id":           f.projectID,
		"source_issue_id":      f.issueID,
		"reply_root_id":        f.rootID,
		"reply_root_hash":      strikeFlowTestRootHash(f.rootID),
		"package_id":           "00000000-0000-4000-8000-000000000123",
		"package_payload_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"source_revision":      1,
		"idempotency_key":      key,
		"message":              message,
	}
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

func TestStrikeFlowContentReplyCredentialIsPurposeScoped(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO strikeflow_connector_token(
			workspace_id,recipient_id,agent_id,name,token_hash,token_prefix,
			project_ids,scopes,expires_at,created_by
		) VALUES($1,$2,$3,'invalid mixed content scope',$4,'msc_test',$5,
			ARRAY['content:reply']::text[],now()+interval '1 day',$2)
	`, testWorkspaceID, testUserID, f.agentID,
		auth.HashToken("msc_invalid_multi_project_"+f.itemID),
		[]string{f.projectID, f.otherProjectID}); err == nil {
		t.Fatal("database accepted a content credential with multiple projects")
	}
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO strikeflow_connector_content_reply_receipt(
			token_id,workspace_id,recipient_id,agent_id,idempotency_key,issue_id,
			root_comment_id,reply_root_hash,package_id,package_payload_hash,
			source_revision,payload_hash
		) VALUES($1,$2,$3,$4,'00000000-0000-4000-8000-000000000093',$5,$6,
			'bad','00000000-0000-4000-8000-000000000123',
			'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			1,'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')
	`, f.contentTokenID, testWorkspaceID, testUserID, f.agentID,
		f.issueID, f.rootID); err == nil {
		t.Fatal("database accepted a malformed forensic receipt hash")
	}
	base := "/api/workspaces/" + testWorkspaceID + "/strikeflow-connector-tokens"
	valid := map[string]any{
		"name": "content package replies", "recipient_id": testUserID,
		"agent_id": f.agentID, "project_ids": []string{f.projectID},
		"scopes":     []string{"content:reply"},
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	for name, mutate := range map[string]func(map[string]any){
		"mixed_scope": func(body map[string]any) {
			body["scopes"] = []string{"content:reply", "inbox:read"}
		},
		"multiple_projects": func(body map[string]any) {
			body["project_ids"] = []string{f.projectID, f.otherProjectID}
		},
		"missing_agent": func(body map[string]any) {
			delete(body, "agent_id")
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := make(map[string]any, len(valid))
			for key, value := range valid {
				body[key] = value
			}
			mutate(body)
			resp := strikeFlowRequest(t, testToken, http.MethodPost, base, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("purpose-scope violation = %d, want 400", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
	resp := strikeFlowRequest(t, testToken, http.MethodPost, base, valid)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid content credential = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID      string `json:"id"`
		AgentID string `json:"agent_id"`
	}
	readJSON(t, resp, &created)
	if created.ID == "" || created.AgentID != f.agentID {
		t.Fatalf("content credential binding mismatch: %#v", created)
	}
	resp = strikeFlowRequest(t, testToken, http.MethodDelete, base+"/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("content credential revoke = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStrikeFlowContentReplyScopeBindingReplayAndRotation(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	path := "/api/integrations/strikeflow/content-replies"
	key := "00000000-0000-4000-8000-000000000095"
	body := strikeFlowContentReplyBody(f, key, "Please revise the package opening.")

	resp := strikeFlowRequest(t, f.valid, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("inbox credential reached content reply = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = strikeFlowRequest(t, f.contentReply, http.MethodGet, "/api/integrations/strikeflow/inbox", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("content credential reached inbox = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	resp = strikeFlowRequest(t, f.contentReply, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first content reply = %d, want 201", resp.StatusCode)
	}
	var first struct {
		OK   bool `json:"ok"`
		Data struct {
			CommentID string         `json:"comment_id"`
			TaskID    string         `json:"task_id"`
			Task      map[string]any `json:"task"`
		} `json:"data"`
	}
	readJSON(t, resp, &first)
	if !first.OK || first.Data.CommentID == "" || first.Data.TaskID == "" ||
		first.Data.Task["agent_id"] != f.agentID {
		t.Fatalf("content reply receipt mismatch: %#v", first)
	}

	rotatePath := "/api/workspaces/" + testWorkspaceID +
		"/strikeflow-connector-tokens/" + f.contentTokenID + "/rotate"
	resp = strikeFlowRequest(t, testToken, http.MethodPost, rotatePath, map[string]any{
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("content credential rotate = %d, want 201", resp.StatusCode)
	}
	var rotated struct {
		ID      string `json:"id"`
		Token   string `json:"token"`
		AgentID string `json:"agent_id"`
	}
	readJSON(t, resp, &rotated)
	if rotated.ID == "" || rotated.Token == "" || rotated.AgentID != f.agentID {
		t.Fatal("rotation did not preserve content agent binding")
	}
	resp = strikeFlowRequest(t, f.contentReply, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rotated credential remained valid = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = strikeFlowRequest(t, rotated.Token, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotation-stable replay = %d, want 200", resp.StatusCode)
	}
	var replay struct {
		Data struct {
			CommentID string `json:"comment_id"`
			TaskID    string `json:"task_id"`
		} `json:"data"`
	}
	readJSON(t, resp, &replay)
	if replay.Data.CommentID != first.Data.CommentID || replay.Data.TaskID != first.Data.TaskID {
		t.Fatalf("rotation replay diverged: first=%#v replay=%#v", first.Data, replay.Data)
	}

	conflict := strikeFlowContentReplyBody(f, key, "Different payload.")
	resp = strikeFlowRequest(t, rotated.Token, http.MethodPost, path, conflict)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("divergent replay = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
	badMarker := strikeFlowContentReplyBody(
		f, "00000000-0000-4000-8000-000000000094",
		"[strikeflow-content-reply:00000000-0000-4000-8000-000000000001]",
	)
	resp = strikeFlowRequest(t, rotated.Token, http.MethodPost, path, badMarker)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reserved marker injection = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = strikeFlowRequest(t, testToken, http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/strikeflow-connector-tokens/"+rotated.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rotated credential revoke = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	resp = strikeFlowRequest(t, rotated.Token, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked credential remained valid = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStrikeFlowContentReplySerializesWithLifecycle(t *testing.T) {
	for lifecycle, key := range map[string]string{
		"revoke": "00000000-0000-4000-8000-000000000092",
		"rotate": "00000000-0000-4000-8000-000000000093",
	} {
		t.Run(lifecycle, func(t *testing.T) {
			f := seedStrikeFlowIntegrationFixture(t)
			body := strikeFlowContentReplyBody(f, key, "Lifecycle race must fail closed.")
			type requestResult struct {
				status int
				err    error
			}
			send := func(token, method, path string, requestBody any) <-chan requestResult {
				result := make(chan requestResult, 1)
				go func() {
					var reader io.Reader
					if requestBody != nil {
						raw, _ := json.Marshal(requestBody)
						reader = bytes.NewReader(raw)
					}
					req, err := http.NewRequest(method, testServer.URL+path, reader)
					if err != nil {
						result <- requestResult{err: err}
						return
					}
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("Content-Type", "application/json")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						result <- requestResult{err: err}
						return
					}
					resp.Body.Close()
					result <- requestResult{status: resp.StatusCode}
				}()
				return result
			}

			blockerTx, err := testPool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := blockerTx.Exec(t.Context(), `
				LOCK TABLE strikeflow_connector_content_reply_receipt
				IN ACCESS EXCLUSIVE MODE
			`); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := blockerTx.QueryRow(t.Context(),
				`SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			releaseBarrier := func() {
				_ = blockerTx.Rollback(context.Background())
			}
			t.Cleanup(releaseBarrier)

			replyResult := send(
				f.contentReply, http.MethodPost,
				"/api/integrations/strikeflow/content-replies", body,
			)
			deadline := time.Now().Add(3 * time.Second)
			var replyPID int
			for time.Now().Before(deadline) {
				err := testPool.QueryRow(t.Context(), `
					SELECT pid
					FROM pg_stat_activity
					WHERE datname=current_database()
					  AND $1=ANY(pg_blocking_pids(pid))
					  AND query LIKE '%INSERT INTO strikeflow_connector_content_reply_receipt%'
					LIMIT 1
				`, blockerPID).Scan(&replyPID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					t.Fatal(err)
				}
				if replyPID != 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if replyPID == 0 {
				t.Fatal("content reply did not reach the post-token-lock receipt barrier")
			}

			lifecyclePath := "/api/workspaces/" + testWorkspaceID +
				"/strikeflow-connector-tokens/" + f.contentTokenID
			lifecycleMethod := http.MethodDelete
			var lifecycleBody any
			wantLifecycle := http.StatusNoContent
			if lifecycle == "rotate" {
				lifecycleMethod = http.MethodPost
				lifecyclePath += "/rotate"
				lifecycleBody = map[string]any{
					"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
				}
				wantLifecycle = http.StatusCreated
			}
			lifecycleResult := send(
				testToken, lifecycleMethod, lifecyclePath, lifecycleBody,
			)
			deadline = time.Now().Add(3 * time.Second)
			lifecycleBlocked := false
			for time.Now().Before(deadline) {
				var waits int
				if err := testPool.QueryRow(t.Context(), `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname=current_database()
					  AND $1=ANY(pg_blocking_pids(pid))
					  AND query LIKE '%UPDATE strikeflow_connector_token SET revoked_at%'
				`, replyPID).Scan(&waits); err != nil {
					t.Fatal(err)
				}
				if waits > 0 {
					lifecycleBlocked = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !lifecycleBlocked {
				t.Fatal("credential lifecycle did not wait on the content reply token lock")
			}
			select {
			case early := <-lifecycleResult:
				t.Fatalf("%s returned before content mutation committed: %#v", lifecycle, early)
			default:
			}

			if err := blockerTx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			reply := <-replyResult
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			if reply.status != http.StatusCreated {
				t.Fatalf("serialized content reply = %d, want 201", reply.status)
			}
			completed := <-lifecycleResult
			if completed.err != nil {
				t.Fatal(completed.err)
			}
			if completed.status != wantLifecycle {
				t.Fatalf("%s after content commit = %d, want %d",
					lifecycle, completed.status, wantLifecycle)
			}

			// Lifecycle has returned. The old credential can no longer enter a
			// second mutation, and exactly the pre-lifecycle mutation remains.
			secondKey := "00000000-0000-4000-8000-000000000090"
			secondBody := strikeFlowContentReplyBody(f, secondKey, "Must not run.")
			resp := strikeFlowRequest(t, f.contentReply, http.MethodPost,
				"/api/integrations/strikeflow/content-replies", secondBody)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("old credential after %s = %d, want 401", lifecycle, resp.StatusCode)
			}
			resp.Body.Close()
			var comments, receipts int
			marker := "[strikeflow-content-reply:" + key + "]"
			if err := testPool.QueryRow(t.Context(), `
				SELECT count(*) FROM comment
				WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
			`, f.issueID, marker).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if err := testPool.QueryRow(t.Context(), `
				SELECT count(*) FROM strikeflow_connector_content_reply_receipt
				WHERE idempotency_key=$1 AND issue_id=$2
			`, key, f.issueID).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if comments != 1 || receipts != 1 {
				t.Fatalf("serialized effects: comments=%d receipts=%d", comments, receipts)
			}
		})
	}
}

func TestStrikeFlowContentReplySerializesWithSourceAuthorizationChanges(t *testing.T) {
	type sourceRace struct {
		key, denyKey string
		sql, pattern string
		args         func(strikeFlowIntegrationFixture) []any
		cleanup      func(*testing.T, strikeFlowIntegrationFixture)
		allowPending bool
	}
	cases := map[string]sourceRace{
		"project_move": {
			key:     "00000000-0000-4000-8000-000000000081",
			denyKey: "00000000-0000-4000-8000-000000000071",
			sql:     `/* strikeflow_source_race_project_move */ UPDATE issue SET project_id=$2 WHERE id=$1`,
			pattern: "%strikeflow_source_race_project_move%",
			args: func(f strikeFlowIntegrationFixture) []any {
				return []any{f.issueID, f.otherProjectID}
			},
		},
		"reassignment": {
			key:     "00000000-0000-4000-8000-000000000082",
			denyKey: "00000000-0000-4000-8000-000000000072",
			sql: `/* strikeflow_source_race_reassignment */
				UPDATE issue SET assignee_type='member',assignee_id=$2 WHERE id=$1`,
			pattern: "%strikeflow_source_race_reassignment%",
			args: func(f strikeFlowIntegrationFixture) []any {
				return []any{f.issueID, testUserID}
			},
		},
		"agent_archive": {
			key:          "00000000-0000-4000-8000-000000000083",
			denyKey:      "00000000-0000-4000-8000-000000000073",
			sql:          `/* strikeflow_source_race_agent_archive */ UPDATE agent SET archived_at=now() WHERE id=$1`,
			pattern:      "%strikeflow_source_race_agent_archive%",
			allowPending: true,
			args: func(f strikeFlowIntegrationFixture) []any {
				return []any{f.agentID}
			},
			cleanup: func(t *testing.T, f strikeFlowIntegrationFixture) {
				_, _ = testPool.Exec(context.Background(),
					`UPDATE agent SET archived_at=NULL WHERE id=$1`, f.agentID)
			},
		},
		"member_removal": {
			key:     "00000000-0000-4000-8000-000000000084",
			denyKey: "00000000-0000-4000-8000-000000000074",
			sql: `/* strikeflow_source_race_member_removal */
				DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`,
			pattern: "%strikeflow_source_race_member_removal%",
			args: func(f strikeFlowIntegrationFixture) []any {
				return []any{testWorkspaceID, testUserID}
			},
			cleanup: func(t *testing.T, f strikeFlowIntegrationFixture) {
				_, _ = testPool.Exec(context.Background(), `
					INSERT INTO member(workspace_id,user_id,role)
					VALUES($1,$2,'owner') ON CONFLICT DO NOTHING
				`, testWorkspaceID, testUserID)
			},
		},
		"root_author_change": {
			key:     "00000000-0000-4000-8000-000000000085",
			denyKey: "00000000-0000-4000-8000-000000000075",
			sql: `/* strikeflow_source_race_root_author */
				UPDATE comment SET author_type='member',author_id=$2 WHERE id=$1`,
			pattern: "%strikeflow_source_race_root_author%",
			args: func(f strikeFlowIntegrationFixture) []any {
				return []any{f.rootID, testUserID}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := seedStrikeFlowIntegrationFixture(t)
			if tc.cleanup != nil {
				t.Cleanup(func() { tc.cleanup(t, f) })
			}
			body := strikeFlowContentReplyBody(f, tc.key, "Authorize against locked source state.")

			blockerTx, err := testPool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := blockerTx.Exec(t.Context(), `
				LOCK TABLE strikeflow_connector_content_reply_receipt
				IN ACCESS EXCLUSIVE MODE
			`); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := blockerTx.QueryRow(t.Context(),
				`SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = blockerTx.Rollback(context.Background()) })

			type httpResult struct {
				status int
				err    error
			}
			replyResult := make(chan httpResult, 1)
			go func() {
				raw, _ := json.Marshal(body)
				req, requestErr := http.NewRequest(
					http.MethodPost,
					testServer.URL+"/api/integrations/strikeflow/content-replies",
					bytes.NewReader(raw),
				)
				if requestErr != nil {
					replyResult <- httpResult{err: requestErr}
					return
				}
				req.Header.Set("Authorization", "Bearer "+f.contentReply)
				req.Header.Set("Content-Type", "application/json")
				httpResp, requestErr := http.DefaultClient.Do(req)
				if requestErr != nil {
					replyResult <- httpResult{err: requestErr}
					return
				}
				httpResp.Body.Close()
				replyResult <- httpResult{status: httpResp.StatusCode}
			}()

			deadline := time.Now().Add(3 * time.Second)
			var replyPID int
			for time.Now().Before(deadline) {
				err := testPool.QueryRow(t.Context(), `
					SELECT pid FROM pg_stat_activity
					WHERE datname=current_database()
					  AND $1=ANY(pg_blocking_pids(pid))
					  AND query LIKE '%INSERT INTO strikeflow_connector_content_reply_receipt%'
					LIMIT 1
				`, blockerPID).Scan(&replyPID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					t.Fatal(err)
				}
				if replyPID != 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if replyPID == 0 {
				t.Fatal("content reply did not reach the post-source-lock barrier")
			}

			mutationResult := make(chan error, 1)
			go func() {
				_, mutationErr := testPool.Exec(context.Background(), tc.sql, tc.args(f)...)
				mutationResult <- mutationErr
			}()
			deadline = time.Now().Add(3 * time.Second)
			mutationBlocked := false
			for time.Now().Before(deadline) {
				var waits int
				if err := testPool.QueryRow(t.Context(), `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname=current_database()
					  AND $1=ANY(pg_blocking_pids(pid))
					  AND query LIKE $2
				`, replyPID, tc.pattern).Scan(&waits); err != nil {
					t.Fatal(err)
				}
				if waits > 0 {
					mutationBlocked = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !mutationBlocked {
				t.Fatal("source authorization change did not wait on the reply lock")
			}
			select {
			case early := <-mutationResult:
				t.Fatalf("source change returned before reply commit: %v", early)
			default:
			}

			if err := blockerTx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			reply := <-replyResult
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			// Archival may win immediately after the durable comment commit and
			// prevent its post-commit continuation; 503 reports that exact
			// durable-comment/pending-continuation state.
			if reply.status != http.StatusCreated &&
				!(tc.allowPending && reply.status == http.StatusServiceUnavailable) {
				t.Fatalf("serialized source reply = %d, want 201", reply.status)
			}
			if err := <-mutationResult; err != nil {
				t.Fatal(err)
			}

			denyBody := strikeFlowContentReplyBody(f, tc.denyKey, "Must fail after source change.")
			resp := strikeFlowRequest(t, f.contentReply, http.MethodPost,
				"/api/integrations/strikeflow/content-replies", denyBody)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("reply after source change = %d, want 404", resp.StatusCode)
			}
			resp.Body.Close()
			var comments int
			marker := "[strikeflow-content-reply:" + tc.key + "]"
			if err := testPool.QueryRow(t.Context(), `
				SELECT count(*) FROM comment
				WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
			`, f.issueID, marker).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if comments != 1 {
				t.Fatalf("source race comments=%d, want 1", comments)
			}
		})
	}
}

func TestStrikeFlowContentReplyRecoversContinuationAfterEnqueueFailure(t *testing.T) {
	f := seedStrikeFlowIntegrationFixture(t)
	key := "00000000-0000-4000-8000-000000000091"
	body := strikeFlowContentReplyBody(f, key, "Retry the continuation safely.")
	constraintSQL := fmt.Sprintf(`
		ALTER TABLE agent_task_queue
		ADD CONSTRAINT strikeflow_test_reject_content_trigger
		CHECK (issue_id IS DISTINCT FROM '%s'::uuid OR trigger_comment_id IS NULL)
		NOT VALID
	`, f.issueID)
	if _, err := testPool.Exec(t.Context(), constraintSQL); err != nil {
		t.Fatal(err)
	}
	dropConstraint := func() {
		_, _ = testPool.Exec(context.Background(), `
			ALTER TABLE agent_task_queue
			DROP CONSTRAINT IF EXISTS strikeflow_test_reject_content_trigger
		`)
	}
	t.Cleanup(dropConstraint)

	resp := strikeFlowRequest(t, f.contentReply, http.MethodPost,
		"/api/integrations/strikeflow/content-replies", body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first enqueue failure = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	var comments, receipts, tasks int
	marker := "[strikeflow-content-reply:" + key + "]"
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM comment
		WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
	`, f.issueID, marker).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM strikeflow_connector_content_reply_receipt
		WHERE idempotency_key=$1 AND issue_id=$2 AND comment_id IS NOT NULL
	`, key, f.issueID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id=$1 AND trigger_comment_id IN (
			SELECT id FROM comment
			WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
		)
	`, f.issueID, marker).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if comments != 1 || receipts != 1 || tasks != 0 {
		t.Fatalf("failed enqueue state: comments=%d receipts=%d tasks=%d",
			comments, receipts, tasks)
	}

	dropConstraint()
	rotatePath := "/api/workspaces/" + testWorkspaceID +
		"/strikeflow-connector-tokens/" + f.contentTokenID + "/rotate"
	resp = strikeFlowRequest(t, testToken, http.MethodPost, rotatePath, map[string]any{
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("content credential rotate = %d, want 201", resp.StatusCode)
	}
	var rotated struct {
		Token string `json:"token"`
	}
	readJSON(t, resp, &rotated)

	lockKey := int64(905091)
	lockConn, err := testPool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.Exec(t.Context(), `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	locked := true
	unlock := func() {
		if locked {
			_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
			locked = false
		}
	}
	t.Cleanup(func() {
		unlock()
		lockConn.Release()
	})
	if _, err := testPool.Exec(t.Context(), fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION strikeflow_test_block_continuation_receipt()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.issue_id = '%s'::uuid AND NEW.continuation_task_id IS NOT NULL THEN
				PERFORM pg_advisory_xact_lock(%d);
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER strikeflow_test_block_continuation_receipt
			BEFORE UPDATE OF continuation_task_id
			ON strikeflow_connector_content_reply_receipt
			FOR EACH ROW EXECUTE FUNCTION strikeflow_test_block_continuation_receipt()
	`, f.issueID, lockKey)); err != nil {
		t.Fatal(err)
	}
	dropTrigger := func() {
		_, _ = testPool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS strikeflow_test_block_continuation_receipt
				ON strikeflow_connector_content_reply_receipt;
			DROP FUNCTION IF EXISTS strikeflow_test_block_continuation_receipt()
		`)
	}
	t.Cleanup(dropTrigger)

	type recoveryResponse struct {
		status int
		data   struct {
			CommentID string         `json:"comment_id"`
			TaskID    string         `json:"task_id"`
			Task      map[string]any `json:"task"`
		}
		err error
	}
	sendRecovery := func() <-chan recoveryResponse {
		result := make(chan recoveryResponse, 1)
		go func() {
			raw, _ := json.Marshal(body)
			req, requestErr := http.NewRequest(
				http.MethodPost, testServer.URL+"/api/integrations/strikeflow/content-replies",
				bytes.NewReader(raw),
			)
			if requestErr != nil {
				result <- recoveryResponse{err: requestErr}
				return
			}
			req.Header.Set("Authorization", "Bearer "+rotated.Token)
			req.Header.Set("Content-Type", "application/json")
			httpResp, requestErr := http.DefaultClient.Do(req)
			if requestErr != nil {
				result <- recoveryResponse{err: requestErr}
				return
			}
			defer httpResp.Body.Close()
			var envelope struct {
				Data struct {
					CommentID string         `json:"comment_id"`
					TaskID    string         `json:"task_id"`
					Task      map[string]any `json:"task"`
				} `json:"data"`
			}
			if decodeErr := json.NewDecoder(httpResp.Body).Decode(&envelope); decodeErr != nil {
				result <- recoveryResponse{status: httpResp.StatusCode, err: decodeErr}
				return
			}
			result <- recoveryResponse{status: httpResp.StatusCode, data: envelope.Data}
		}()
		return result
	}

	firstResult := sendRecovery()
	deadline := time.Now().Add(3 * time.Second)
	var recoveryPID int
	for time.Now().Before(deadline) {
		err := testPool.QueryRow(t.Context(), `
			SELECT pid FROM pg_stat_activity
			WHERE datname=current_database()
			  AND $1=ANY(pg_blocking_pids(pid))
			  AND query LIKE '%UPDATE strikeflow_connector_content_reply_receipt%'
			LIMIT 1
		`, lockConn.Conn().PgConn().PID()).Scan(&recoveryPID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if recoveryPID != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if recoveryPID == 0 {
		t.Fatal("first recovery did not reach continuation receipt barrier")
	}

	secondResult := sendRecovery()
	deadline = time.Now().Add(3 * time.Second)
	secondBlocked := false
	for time.Now().Before(deadline) {
		var waits int
		if err := testPool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database()
			  AND $1=ANY(pg_blocking_pids(pid))
			  AND query LIKE '%strikeflow_connector_content_reply_receipt%'
		`, recoveryPID).Scan(&waits); err != nil {
			t.Fatal(err)
		}
		if waits > 0 {
			secondBlocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !secondBlocked {
		t.Fatal("concurrent same-key recovery did not wait on the receipt row")
	}

	var taskID string
	if err := testPool.QueryRow(t.Context(), `
		SELECT id FROM agent_task_queue
		WHERE issue_id=$1 AND trigger_comment_id IN (
			SELECT id FROM comment
			WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
		)
	`, f.issueID, marker).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `
		UPDATE agent_task_queue
		SET status='running',started_at=now()
		WHERE id=$1
	`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `
		UPDATE agent_task_queue
		SET status='completed',completed_at=now()
		WHERE id=$1
	`, taskID); err != nil {
		t.Fatal(err)
	}
	unlock()

	recovered := <-firstResult
	stable := <-secondResult
	for index, got := range []recoveryResponse{recovered, stable} {
		if got.err != nil {
			t.Fatalf("recovery %d failed: %v", index+1, got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("recovery %d = %d, want 200", index+1, got.status)
		}
	}
	if recovered.data.CommentID == "" || recovered.data.TaskID == "" ||
		recovered.data.Task["agent_id"] != f.agentID ||
		recovered.data.Task["originator_user_id"] != testUserID ||
		recovered.data.Task["accountable_user_id"] != testUserID {
		t.Fatalf("recovered continuation attribution mismatch: %#v", recovered.data)
	}
	if stable.data.CommentID != recovered.data.CommentID ||
		stable.data.TaskID != recovered.data.TaskID {
		t.Fatalf("concurrent replay diverged: recovered=%#v stable=%#v",
			recovered.data, stable.data)
	}
	var persistedTaskID, taskStatus string
	if err := testPool.QueryRow(t.Context(), `
		SELECT r.continuation_task_id,t.status
		FROM strikeflow_connector_content_reply_receipt r
		JOIN agent_task_queue t ON t.id=r.continuation_task_id
		WHERE r.issue_id=$1 AND r.idempotency_key=$2
	`, f.issueID, key).Scan(&persistedTaskID, &taskStatus); err != nil {
		t.Fatal(err)
	}
	if persistedTaskID != recovered.data.TaskID || taskStatus != "completed" {
		t.Fatalf("persisted terminal continuation mismatch: id=%s status=%s",
			persistedTaskID, taskStatus)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id=$1 AND trigger_comment_id IN (
			SELECT id FROM comment
			WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
		)
	`, f.issueID, marker).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM comment
		WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'
	`, f.issueID, marker).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if comments != 1 || tasks != 1 {
		t.Fatalf("recovery duplicated effects: comments=%d tasks=%d", comments, tasks)
	}
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

	contentKey := "00000000-0000-4000-8000-000000000096"
	resp = strikeFlowRequest(t, f.contentReply, http.MethodPost,
		"/api/integrations/strikeflow/content-replies",
		strikeFlowContentReplyBody(f, contentKey, "Audit must commit with this package reply."))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("content reply audit failure = %d, want 500", resp.StatusCode)
	}
	resp.Body.Close()
	var contentComments, contentReceipts int
	contentMarker := "[strikeflow-content-reply:" + contentKey + "]"
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM comment WHERE issue_id=$1 AND content LIKE '%' || $2 || '%'`,
		f.issueID, contentMarker).Scan(&contentComments); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM strikeflow_connector_content_reply_receipt WHERE idempotency_key=$1`,
		contentKey).Scan(&contentReceipts); err != nil {
		t.Fatal(err)
	}
	if contentComments != 0 || contentReceipts != 0 {
		t.Fatalf("audit failure persisted content reply: comments=%d receipts=%d",
			contentComments, contentReceipts)
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
