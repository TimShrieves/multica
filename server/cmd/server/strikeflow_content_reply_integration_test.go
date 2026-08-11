package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
)

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
