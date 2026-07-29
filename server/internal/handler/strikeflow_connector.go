package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	strikeFlowReplyMaxBytes = 10_000
	strikeFlowTokenMaxTTL   = 30 * 24 * time.Hour
)

var strikeFlowAllowedScopes = map[string]struct{}{
	"inbox:read":         {},
	"inbox:read_receipt": {},
	"inbox:archive":      {},
	"inbox:reply":        {},
	"content:reply":      {},
}

type createStrikeFlowConnectorTokenRequest struct {
	Name        string   `json:"name"`
	RecipientID string   `json:"recipient_id"`
	AgentID     string   `json:"agent_id,omitempty"`
	ProjectIDs  []string `json:"project_ids"`
	Scopes      []string `json:"scopes"`
	ExpiresAt   string   `json:"expires_at"`
}

type strikeFlowConnectorTokenResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Token       string   `json:"token,omitempty"`
	TokenPrefix string   `json:"token_prefix"`
	RecipientID string   `json:"recipient_id"`
	AgentID     string   `json:"agent_id,omitempty"`
	ProjectIDs  []string `json:"project_ids"`
	Scopes      []string `json:"scopes"`
	ExpiresAt   string   `json:"expires_at"`
}

func mintStrikeFlowConnectorToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return middleware.StrikeFlowConnectorTokenPrefix + hex.EncodeToString(b), nil
}

func validateStrikeFlowTokenRequest(w http.ResponseWriter, req createStrikeFlowConnectorTokenRequest) ([]pgtype.UUID, time.Time, bool) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 120 {
		writeError(w, http.StatusBadRequest, "name must be 1-120 characters")
		return nil, time.Time{}, false
	}
	if len(req.ProjectIDs) == 0 || len(req.ProjectIDs) > 32 || len(req.Scopes) == 0 {
		writeError(w, http.StatusBadRequest, "project_ids and scopes are required")
		return nil, time.Time{}, false
	}
	seenScopes := map[string]struct{}{}
	for _, permission := range req.Scopes {
		if _, ok := strikeFlowAllowedScopes[permission]; !ok {
			writeError(w, http.StatusBadRequest, "unsupported connector scope")
			return nil, time.Time{}, false
		}
		seenScopes[permission] = struct{}{}
	}
	if len(seenScopes) != len(req.Scopes) {
		writeError(w, http.StatusBadRequest, "duplicate connector scope")
		return nil, time.Time{}, false
	}
	_, contentReply := seenScopes["content:reply"]
	if contentReply && (len(seenScopes) != 1 || len(req.ProjectIDs) != 1 || strings.TrimSpace(req.AgentID) == "") {
		writeError(w, http.StatusBadRequest, "content reply credentials require one project, one bound agent, and cannot combine scopes")
		return nil, time.Time{}, false
	}
	if !contentReply && strings.TrimSpace(req.AgentID) != "" {
		writeError(w, http.StatusBadRequest, "agent_id is only valid for content reply credentials")
		return nil, time.Time{}, false
	}
	projectIDs := make([]pgtype.UUID, 0, len(req.ProjectIDs))
	seenProjects := map[string]struct{}{}
	for _, raw := range req.ProjectIDs {
		id, err := util.ParseUUID(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid project id")
			return nil, time.Time{}, false
		}
		canonical := util.UUIDToString(id)
		if _, exists := seenProjects[canonical]; exists {
			writeError(w, http.StatusBadRequest, "duplicate project id")
			return nil, time.Time{}, false
		}
		seenProjects[canonical] = struct{}{}
		projectIDs = append(projectIDs, id)
	}
	expiresAt, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil || expiresAt.Before(time.Now().Add(time.Hour)) || expiresAt.After(time.Now().Add(strikeFlowTokenMaxTTL)) {
		writeError(w, http.StatusBadRequest, "expires_at must be 1 hour to 30 days from now")
		return nil, time.Time{}, false
	}
	return projectIDs, expiresAt, true
}

// CreateStrikeFlowConnectorToken is owner/admin gated by the router. The
// returned token is one-time material; only its SHA-256 hash is persisted.
func (h *Handler) CreateStrikeFlowConnectorToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	creatorID, ok := parseUUIDOrBadRequest(w, requestUserID(r), "user id")
	if !ok {
		return
	}
	var req createStrikeFlowConnectorTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	projectIDs, expiresAt, ok := validateStrikeFlowTokenRequest(w, req)
	if !ok {
		return
	}
	recipientID, ok := parseUUIDOrBadRequest(w, req.RecipientID, "recipient id")
	if !ok {
		return
	}
	var memberExists bool
	if err := h.DB.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM member WHERE workspace_id=$1 AND user_id=$2)`,
		workspaceID, recipientID,
	).Scan(&memberExists); err != nil || !memberExists {
		writeError(w, http.StatusBadRequest, "recipient is not a workspace member")
		return
	}
	var agentID pgtype.UUID
	if strings.TrimSpace(req.AgentID) != "" {
		parsed, ok := parseUUIDOrBadRequest(w, req.AgentID, "agent id")
		if !ok {
			return
		}
		if err := h.DB.QueryRow(r.Context(),
			`SELECT id FROM agent WHERE id=$1 AND workspace_id=$2 AND archived_at IS NULL`,
			parsed, workspaceID,
		).Scan(&agentID); err != nil {
			writeError(w, http.StatusBadRequest, "agent is not active in the workspace")
			return
		}
	}
	var projectCount int
	if err := h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM project WHERE workspace_id=$1 AND id=ANY($2::uuid[])`,
		workspaceID, projectIDs,
	).Scan(&projectCount); err != nil || projectCount != len(projectIDs) {
		writeError(w, http.StatusBadRequest, "one or more projects are outside the workspace")
		return
	}
	plain, err := mintStrikeFlowConnectorToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint connector token")
		return
	}
	prefix := plain[:8]
	var id pgtype.UUID
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO strikeflow_connector_token
			(workspace_id, recipient_id, agent_id, name, token_hash, token_prefix, project_ids, scopes, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id
	`, workspaceID, recipientID, agentID, strings.TrimSpace(req.Name), auth.HashToken(plain),
		prefix, projectIDs, req.Scopes, expiresAt, creatorID).Scan(&id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create connector token")
		return
	}
	writeJSON(w, http.StatusCreated, strikeFlowConnectorTokenResponse{
		ID: util.UUIDToString(id), Name: strings.TrimSpace(req.Name), Token: plain,
		TokenPrefix: prefix, RecipientID: req.RecipientID, ProjectIDs: req.ProjectIDs,
		AgentID: req.AgentID, Scopes: req.Scopes, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) RevokeStrikeFlowConnectorToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	tokenID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "tokenId"), "token id")
	if !ok {
		return
	}
	// Credential lifecycle locks the token row first. Content reply mutation
	// uses the same first lock, then source/receipt rows, preventing deadlocks
	// and ensuring no reply mutation can commit after this UPDATE returns.
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE strikeflow_connector_token SET revoked_at=COALESCE(revoked_at,now())
		WHERE id=$1 AND workspace_id=$2
	`, tokenID, workspaceID)
	if err != nil || tag.RowsAffected() != 1 {
		writeError(w, http.StatusNotFound, "connector token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RotateStrikeFlowConnectorToken atomically revokes the old credential and
// creates an otherwise identical replacement with a caller-selected expiry.
func (h *Handler) RotateStrikeFlowConnectorToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	tokenID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "tokenId"), "token id")
	if !ok {
		return
	}
	var body struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil || expiresAt.Before(time.Now().Add(time.Hour)) || expiresAt.After(time.Now().Add(strikeFlowTokenMaxTTL)) {
		writeError(w, http.StatusBadRequest, "expires_at must be 1 hour to 30 days from now")
		return
	}
	plain, err := mintStrikeFlowConnectorToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint connector token")
		return
	}
	creatorID, ok := parseUUIDOrBadRequest(w, requestUserID(r), "user id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rotate connector token")
		return
	}
	defer tx.Rollback(r.Context())
	// Lock order matches content replies: old token row first. The UPDATE in
	// the CTE obtains that lock before the replacement row is inserted.
	var newID, recipientID, agentID pgtype.UUID
	var name string
	var projects []pgtype.UUID
	var scopes []string
	err = tx.QueryRow(r.Context(), `
		WITH old AS (
			UPDATE strikeflow_connector_token SET revoked_at=now()
			WHERE id=$1 AND workspace_id=$2 AND revoked_at IS NULL AND expires_at > now()
			RETURNING *
		)
		INSERT INTO strikeflow_connector_token
			(workspace_id,recipient_id,agent_id,name,token_hash,token_prefix,project_ids,scopes,expires_at,rotated_from_id,created_by)
		SELECT workspace_id,recipient_id,agent_id,name,$3,$4,project_ids,scopes,$5,id,$6 FROM old
		RETURNING id,recipient_id,agent_id,name,project_ids,scopes
	`, tokenID, workspaceID, auth.HashToken(plain), plain[:8], expiresAt, creatorID).
		Scan(&newID, &recipientID, &agentID, &name, &projects, &scopes)
	if err != nil {
		writeError(w, http.StatusNotFound, "active connector token not found")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rotate connector token")
		return
	}
	projectStrings := make([]string, len(projects))
	for i, id := range projects {
		projectStrings[i] = util.UUIDToString(id)
	}
	writeJSON(w, http.StatusCreated, strikeFlowConnectorTokenResponse{
		ID: util.UUIDToString(newID), Name: name, Token: plain, TokenPrefix: plain[:8],
		RecipientID: util.UUIDToString(recipientID), ProjectIDs: projectStrings,
		AgentID: uuidToString(agentID), Scopes: scopes, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

func requireStrikeFlowScope(w http.ResponseWriter, r *http.Request, permission string) (middleware.StrikeFlowConnectorScope, bool) {
	scope, ok := middleware.StrikeFlowConnectorScopeFromContext(r.Context())
	if !ok || !scope.Allows(permission) {
		http.Error(w, `{"error":"connector scope denied"}`, http.StatusForbidden)
		return middleware.StrikeFlowConnectorScope{}, false
	}
	return scope, true
}

type strikeFlowBoundItem struct {
	ItemID        pgtype.UUID
	IssueID       pgtype.UUID
	ProjectID     pgtype.UUID
	RootCommentID pgtype.UUID
}

func (h *Handler) loadStrikeFlowBoundItem(w http.ResponseWriter, r *http.Request, scope middleware.StrikeFlowConnectorScope) (strikeFlowBoundItem, bool) {
	itemID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "itemId"), "inbox item id")
	if !ok {
		return strikeFlowBoundItem{}, false
	}
	var item strikeFlowBoundItem
	var details []byte
	err := h.DB.QueryRow(r.Context(), `
		SELECT ii.id,ii.issue_id,i.project_id,ii.details
		FROM inbox_item ii JOIN issue i ON i.id=ii.issue_id
		WHERE ii.id=$1 AND ii.workspace_id=$2 AND ii.recipient_type='member'
		  AND ii.recipient_id=$3 AND ii.type IN ('agent_action_required','mentioned')
	`, itemID, scope.WorkspaceID, scope.RecipientID).
		Scan(&item.ItemID, &item.IssueID, &item.ProjectID, &details)
	if err != nil || !scope.AllowsProject(util.UUIDToString(item.ProjectID)) {
		writeError(w, http.StatusNotFound, "inbox item not found")
		return strikeFlowBoundItem{}, false
	}
	var parsed struct {
		CommentID string `json:"comment_id"`
	}
	if json.Unmarshal(details, &parsed) != nil {
		writeError(w, http.StatusConflict, "inbox item has no authoritative root")
		return strikeFlowBoundItem{}, false
	}
	root, err := util.ParseUUID(parsed.CommentID)
	if err != nil {
		writeError(w, http.StatusConflict, "inbox item has no authoritative root")
		return strikeFlowBoundItem{}, false
	}
	var exists bool
	if err := h.DB.QueryRow(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM comment WHERE id=$1 AND issue_id=$2 AND workspace_id=$3)
	`, root, item.IssueID, scope.WorkspaceID).Scan(&exists); err != nil || !exists {
		writeError(w, http.StatusConflict, "authoritative root not found")
		return strikeFlowBoundItem{}, false
	}
	item.RootCommentID = root
	return item, true
}

type strikeFlowAuditExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func auditStrikeFlowConnector(exec strikeFlowAuditExecutor, r *http.Request, scope middleware.StrikeFlowConnectorScope, action, outcome string, item strikeFlowBoundItem, commentID pgtype.UUID, key pgtype.UUID, hash string) error {
	_, err := exec.Exec(r.Context(), `
		INSERT INTO strikeflow_connector_audit
			(token_id,workspace_id,recipient_id,request_id,action,outcome,
			 inbox_item_id,issue_id,root_comment_id,comment_id,idempotency_key,payload_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, scope.TokenID, scope.WorkspaceID, scope.RecipientID,
		chimw.GetReqID(r.Context()), action, outcome,
		item.ItemID, item.IssueID, item.RootCommentID, commentID, key, nullIfEmpty(hash))
	return err
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// ListStrikeFlowInbox exposes only actionable, non-archived rows for the bound
// recipient and allowlisted projects. It cannot be widened by query parameters.
func (h *Handler) ListStrikeFlowInbox(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireStrikeFlowScope(w, r, "inbox:read")
	if !ok {
		return
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT ii.id,ii.workspace_id,ii.recipient_type,ii.recipient_id,ii.type,ii.severity,
		       ii.issue_id,ii.title,ii.body,ii.read,ii.archived,ii.created_at,
		       ii.actor_type,ii.actor_id,ii.details,i.status,i.project_id
		FROM inbox_item ii JOIN issue i ON i.id=ii.issue_id
		WHERE ii.workspace_id=$1 AND ii.recipient_type='member' AND ii.recipient_id=$2
		  AND ii.type IN ('agent_action_required','mentioned') AND ii.archived=false
		  AND i.project_id=ANY($3::uuid[])
		ORDER BY ii.created_at DESC LIMIT 200
	`, scope.WorkspaceID, scope.RecipientID, mapKeys(scope.Projects))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connector inbox")
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, ws, recipient, issueID, actorID, projectID pgtype.UUID
		var recipientType, typ, severity, title, status string
		var body, actorType pgtype.Text
		var read, archived bool
		var created time.Time
		var details []byte
		if err := rows.Scan(&id, &ws, &recipientType, &recipient, &typ, &severity,
			&issueID, &title, &body, &read, &archived, &created,
			&actorType, &actorID, &details, &status, &projectID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list connector inbox")
			return
		}
		result = append(result, map[string]any{
			"id": util.UUIDToString(id), "workspace_id": util.UUIDToString(ws),
			"recipient_type": recipientType, "recipient_id": util.UUIDToString(recipient),
			"type": "agent_action_required", "source_type": typ,
			"severity": severity, "issue_id": util.UUIDToString(issueID),
			"title": title, "body": textToPtr(body), "read": read, "archived": archived,
			"created_at": created.UTC().Format(time.RFC3339Nano), "actor_type": textToPtr(actorType),
			"actor_id": uuidToPtr(actorID), "details": json.RawMessage(details),
			"issue_status": status, "project_id": util.UUIDToString(projectID),
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connector inbox")
		return
	}
	if err := auditStrikeFlowConnector(h.DB, r, scope, "inbox.list", "allowed", strikeFlowBoundItem{}, pgtype.UUID{}, pgtype.UUID{}, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to audit connector request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": result})
}

func mapKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}

func (h *Handler) GetStrikeFlowInboxIssue(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireStrikeFlowScope(w, r, "inbox:read")
	if !ok {
		return
	}
	item, ok := h.loadStrikeFlowBoundItem(w, r, scope)
	if !ok {
		return
	}
	issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID: item.IssueID, WorkspaceID: parseUUID(scope.WorkspaceID),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	identifier := fmt.Sprintf("%s-%d", h.getIssuePrefix(r.Context(), issue.WorkspaceID), issue.Number)
	var rootAuthorType string
	var rootAuthorID, rootSourceTaskID pgtype.UUID
	if err := h.DB.QueryRow(r.Context(), `
		SELECT author_type,author_id,source_task_id FROM comment
		WHERE id=$1 AND issue_id=$2 AND workspace_id=$3
	`, item.RootCommentID, item.IssueID, scope.WorkspaceID).
		Scan(&rootAuthorType, &rootAuthorID, &rootSourceTaskID); err != nil {
		writeError(w, http.StatusConflict, "authoritative root not found")
		return
	}
	tasks, working, err := h.strikeFlowTaskEvidence(r, item.IssueID, item.RootCommentID, rootSourceTaskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load connector task evidence")
		return
	}
	if err := auditStrikeFlowConnector(h.DB, r, scope, "issue.read", "allowed", item, pgtype.UUID{}, pgtype.UUID{}, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to audit connector request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": util.UUIDToString(issue.ID), "workspace_id": util.UUIDToString(issue.WorkspaceID),
		"project_id": uuidToPtr(issue.ProjectID), "title": issue.Title, "status": issue.Status,
		"identifier": identifier, "assignee_type": textToPtr(issue.AssigneeType),
		"assignee_id": uuidToPtr(issue.AssigneeID), "working": working,
		"root_comment_id":  util.UUIDToString(item.RootCommentID),
		"root_author_type": rootAuthorType, "root_author_id": uuidToPtr(rootAuthorID),
		"root_source_task_id": uuidToPtr(rootSourceTaskID), "tasks": tasks,
	})
}

func (h *Handler) strikeFlowTaskEvidence(r *http.Request, issueID, rootCommentID, sourceTaskID pgtype.UUID) ([]map[string]any, bool, error) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT id,agent_id,status,trigger_comment_id,originator_user_id,
		       accountable_user_id,originator_source,trigger_evidence_kind,
		       trigger_evidence_ref_id,delivered_comment_ids,created_at,completed_at
		FROM agent_task_queue
		WHERE issue_id=$1 AND (
		  ($2::uuid IS NOT NULL AND id=$2) OR trigger_comment_id=$3 OR $3=ANY(delivered_comment_ids)
		)
		ORDER BY created_at
	`, issueID, sourceTaskID, rootCommentID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	working := false
	for rows.Next() {
		var id, agentID, triggerID, originatorID, accountableID, evidenceID pgtype.UUID
		var status string
		var originatorSource, evidenceKind pgtype.Text
		var delivered []pgtype.UUID
		var created, completed pgtype.Timestamptz
		if err := rows.Scan(&id, &agentID, &status, &triggerID, &originatorID,
			&accountableID, &originatorSource, &evidenceKind, &evidenceID,
			&delivered, &created, &completed); err != nil {
			return nil, false, err
		}
		if status == "queued" || status == "dispatched" || status == "running" || status == "waiting_local_directory" {
			working = true
		}
		deliveredIDs := make([]string, 0, len(delivered))
		for _, value := range delivered {
			deliveredIDs = append(deliveredIDs, util.UUIDToString(value))
		}
		out = append(out, map[string]any{
			"id": util.UUIDToString(id), "agent_id": util.UUIDToString(agentID),
			"status": status, "trigger_comment_id": uuidToPtr(triggerID),
			"originator_user_id":      uuidToPtr(originatorID),
			"accountable_user_id":     uuidToPtr(accountableID),
			"originator_source":       textToPtr(originatorSource),
			"trigger_evidence_kind":   textToPtr(evidenceKind),
			"trigger_evidence_ref_id": uuidToPtr(evidenceID),
			"delivered_comment_ids":   deliveredIDs,
			"created_at":              timestampToString(created), "completed_at": timestampToString(completed),
		})
	}
	return out, working, rows.Err()
}

func (h *Handler) ListStrikeFlowInboxThread(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireStrikeFlowScope(w, r, "inbox:read")
	if !ok {
		return
	}
	item, ok := h.loadStrikeFlowBoundItem(w, r, scope)
	if !ok {
		return
	}
	rows, err := h.DB.Query(r.Context(), `
		WITH RECURSIVE thread AS (
			SELECT * FROM comment WHERE id=$1 AND issue_id=$2 AND workspace_id=$3
			UNION ALL
			SELECT c.* FROM comment c JOIN thread p ON c.parent_id=p.id
			WHERE c.issue_id=$2 AND c.workspace_id=$3
		)
		SELECT id,author_type,author_id,content,type,created_at,updated_at,parent_id,source_task_id
		FROM thread ORDER BY created_at,id LIMIT 101
	`, item.RootCommentID, item.IssueID, scope.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connector thread")
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, authorID, parentID, sourceTaskID pgtype.UUID
		var authorType, content, typ string
		var created, updated time.Time
		if err := rows.Scan(&id, &authorType, &authorID, &content, &typ, &created, &updated, &parentID, &sourceTaskID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list connector thread")
			return
		}
		result = append(result, map[string]any{
			"id": util.UUIDToString(id), "author_type": authorType, "author_id": uuidToPtr(authorID),
			"content": content, "body": content, "type": typ,
			"created_at": created.UTC().Format(time.RFC3339Nano),
			"updated_at": updated.UTC().Format(time.RFC3339Nano), "parent_id": uuidToPtr(parentID),
			"source_task_id": uuidToPtr(sourceTaskID),
		})
	}
	if len(result) > 100 {
		writeError(w, http.StatusConflict, "connector thread exceeds limit")
		return
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connector thread")
		return
	}
	if err := auditStrikeFlowConnector(h.DB, r, scope, "thread.read", "allowed", item, pgtype.UUID{}, pgtype.UUID{}, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to audit connector request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comments": result, "root_comment_id": util.UUIDToString(item.RootCommentID)})
}

func (h *Handler) MarkStrikeFlowInboxRead(w http.ResponseWriter, r *http.Request) {
	h.mutateStrikeFlowInbox(w, r, "inbox:read_receipt", "read")
}

func (h *Handler) ArchiveStrikeFlowInbox(w http.ResponseWriter, r *http.Request) {
	h.mutateStrikeFlowInbox(w, r, "inbox:archive", "archive")
}

func (h *Handler) mutateStrikeFlowInbox(w http.ResponseWriter, r *http.Request, permission, action string) {
	scope, ok := requireStrikeFlowScope(w, r, permission)
	if !ok {
		return
	}
	item, ok := h.loadStrikeFlowBoundItem(w, r, scope)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start connector mutation")
		return
	}
	defer tx.Rollback(r.Context())
	if action == "read" {
		if _, err := tx.Exec(r.Context(), `UPDATE inbox_item SET read=true WHERE id=$1`, item.ItemID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to mark connector item read")
			return
		}
	} else {
		if _, err := tx.Exec(r.Context(), `
				UPDATE inbox_item SET archived=true
				WHERE workspace_id=$1 AND recipient_type='member' AND recipient_id=$2 AND issue_id=$3
			`, scope.WorkspaceID, scope.RecipientID, item.IssueID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to archive connector item")
			return
		}
	}
	if err := auditStrikeFlowConnector(tx, r, scope, "inbox."+action, "allowed", item, pgtype.UUID{}, pgtype.UUID{}, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to audit connector mutation")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit connector mutation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type strikeFlowReplyRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	Message        string `json:"message"`
}

func validStrikeFlowReply(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > strikeFlowReplyMaxBytes || strings.ContainsRune(message, '\x00') {
		return false
	}
	lower := strings.ToLower(message)
	return !strings.Contains(lower, "mention://") &&
		!strings.Contains(lower, "[strikeflow-agent-inbox:") &&
		!strings.Contains(lower, "[strikeflow-feedback:") &&
		!strings.Contains(lower, "[strikeflow-content-reply:")
}

func (h *Handler) ReplyStrikeFlowInbox(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireStrikeFlowScope(w, r, "inbox:reply")
	if !ok {
		return
	}
	item, ok := h.loadStrikeFlowBoundItem(w, r, scope)
	if !ok {
		return
	}
	var req strikeFlowReplyRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || !validStrikeFlowReply(req.Message) {
		writeError(w, http.StatusBadRequest, "invalid connector reply")
		return
	}
	key, err := util.ParseUUID(req.IdempotencyKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "idempotency_key must be a UUID")
		return
	}
	message := strings.TrimSpace(req.Message)
	hashInput := strings.Join([]string{
		scope.TokenID,
		util.UUIDToString(item.ItemID),
		util.UUIDToString(item.IssueID),
		util.UUIDToString(item.RootCommentID),
		message,
	}, "\x00")
	sum := sha256.Sum256([]byte(hashInput))
	payloadHash := hex.EncodeToString(sum[:])
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start connector reply")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	tag, err := tx.Exec(r.Context(), `
			INSERT INTO strikeflow_connector_reply_receipt
				(token_id,idempotency_key,inbox_item_id,issue_id,root_comment_id,payload_hash)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING
	`, scope.TokenID, key, item.ItemID, item.IssueID, item.RootCommentID, payloadHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reserve connector reply")
		return
	}
	var storedHash string
	var storedItemID, storedIssueID, storedRootID, commentID pgtype.UUID
	var createdAt time.Time
	if err := tx.QueryRow(r.Context(), `
			SELECT inbox_item_id,issue_id,root_comment_id,payload_hash,comment_id,created_at
			FROM strikeflow_connector_reply_receipt
			WHERE token_id=$1 AND idempotency_key=$2
	`, scope.TokenID, key).Scan(
		&storedItemID, &storedIssueID, &storedRootID, &storedHash, &commentID, &createdAt,
	); err != nil ||
		storedHash != payloadHash ||
		storedItemID != item.ItemID ||
		storedIssueID != item.IssueID ||
		storedRootID != item.RootCommentID {
		writeError(w, http.StatusConflict, "idempotency key conflict")
		return
	}
	if commentID.Valid {
		if err := auditStrikeFlowConnector(tx, r, scope, "inbox.reply", "replayed", item, commentID, key, payloadHash); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to audit connector reply")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to commit connector reply")
			return
		}
		taskID := h.strikeFlowReplyTaskID(r, item.IssueID, commentID)
		taskReceipt := h.strikeFlowReplyTaskReceipt(r, item.IssueID, commentID)
		writeJSON(w, http.StatusOK, map[string]any{
			"comment_id": util.UUIDToString(commentID), "task_id": taskID,
			"task": taskReceipt, "replayed": true,
		})
		return
	}
	if tag.RowsAffected() == 0 {
		// A concurrent caller owns the fresh reservation. After one minute a
		// crashed owner can be recovered, but first find the durable marker.
		marker := "[strikeflow-agent-inbox:" + req.IdempotencyKey + "]"
		err = tx.QueryRow(r.Context(), `
				SELECT id FROM comment WHERE issue_id=$1 AND parent_id=$2 AND content LIKE '%' || $3 || '%'
				ORDER BY created_at LIMIT 1
			`, item.IssueID, item.RootCommentID, marker).Scan(&commentID)
		if err == nil {
			receiptTag, updateErr := tx.Exec(r.Context(), `
					UPDATE strikeflow_connector_reply_receipt SET comment_id=$3,committed_at=now()
					WHERE token_id=$1 AND idempotency_key=$2
				`, scope.TokenID, key, commentID)
			if updateErr != nil || receiptTag.RowsAffected() != 1 {
				writeError(w, http.StatusInternalServerError, "failed to recover connector reply")
				return
			}
			if err := auditStrikeFlowConnector(tx, r, scope, "inbox.reply", "replayed", item, commentID, key, payloadHash); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to audit connector reply")
				return
			}
			if err := tx.Commit(r.Context()); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to commit connector reply")
				return
			}
			taskID := h.strikeFlowReplyTaskID(r, item.IssueID, commentID)
			taskReceipt := h.strikeFlowReplyTaskReceipt(r, item.IssueID, commentID)
			writeJSON(w, http.StatusOK, map[string]any{
				"comment_id": util.UUIDToString(commentID), "task_id": taskID,
				"task": taskReceipt, "replayed": true,
			})
			return
		}
		claim, claimErr := tx.Exec(r.Context(), `
				UPDATE strikeflow_connector_reply_receipt SET created_at=now()
				WHERE token_id=$1 AND idempotency_key=$2 AND comment_id IS NULL
				  AND created_at < now()-interval '1 minute'
		`, scope.TokenID, key)
		if claimErr != nil || claim.RowsAffected() != 1 {
			writeError(w, http.StatusConflict, "connector reply is already in progress")
			return
		}
	}

	issue, err := qtx.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID: item.IssueID, WorkspaceID: parseUUID(scope.WorkspaceID),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	parent, err := qtx.GetComment(r.Context(), item.RootCommentID)
	if err != nil {
		writeError(w, http.StatusConflict, "authoritative root not found")
		return
	}
	body := "Tim reply via StrikeFlow\n[strikeflow-agent-inbox:" + req.IdempotencyKey + "]\n\n" + message
	comment, err := qtx.CreateComment(r.Context(), db.CreateCommentParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "member",
		AuthorID: parseUUID(scope.RecipientID), Content: body, Type: "comment",
		ParentID: item.RootCommentID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create connector reply")
		return
	}
	receiptTag, err := tx.Exec(r.Context(), `
			UPDATE strikeflow_connector_reply_receipt SET comment_id=$3,committed_at=now()
			WHERE token_id=$1 AND idempotency_key=$2 AND comment_id IS NULL
		`, scope.TokenID, key, comment.ID)
	if err != nil || receiptTag.RowsAffected() != 1 {
		writeError(w, http.StatusInternalServerError, "failed to commit connector reply receipt")
		return
	}
	if err := auditStrikeFlowConnector(tx, r, scope, "inbox.reply", "allowed", item, comment.ID, key, payloadHash); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to audit connector reply")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit connector reply")
		return
	}
	resp := commentToResponse(comment, nil, nil)
	h.publish(protocol.EventCommentCreated, scope.WorkspaceID, "member", scope.RecipientID, map[string]any{
		"comment": resp, "issue_title": issue.Title, "issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id": uuidToPtr(issue.AssigneeID), "issue_status": issue.Status,
	})
	h.TaskService.AutoUnresolveThreadOnReply(r.Context(), &parent, scope.WorkspaceID, "member", scope.RecipientID)
	resp.TriggerOutcomes = h.triggerTasksForComment(r.Context(), issue, comment, &parent,
		"member", scope.RecipientID, scope.RecipientID, "", nil)
	taskID := h.strikeFlowReplyTaskID(r, item.IssueID, comment.ID)
	taskReceipt := h.strikeFlowReplyTaskReceipt(r, item.IssueID, comment.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"comment_id": util.UUIDToString(comment.ID), "task_id": taskID,
		"task": taskReceipt, "replayed": false,
		"trigger_outcomes": resp.TriggerOutcomes,
	})
}

func (h *Handler) strikeFlowReplyTaskReceipt(r *http.Request, issueID, commentID pgtype.UUID) map[string]any {
	var id, agentID, triggerID, originatorID, accountableID, evidenceID pgtype.UUID
	var status string
	var originatorSource, evidenceKind pgtype.Text
	var delivered []pgtype.UUID
	err := h.DB.QueryRow(r.Context(), `
		SELECT id,agent_id,status,trigger_comment_id,originator_user_id,
		       accountable_user_id,originator_source,trigger_evidence_kind,
		       trigger_evidence_ref_id,delivered_comment_ids
		FROM agent_task_queue
		WHERE issue_id=$1 AND trigger_comment_id=$2
		ORDER BY created_at DESC LIMIT 1
	`, issueID, commentID).Scan(&id, &agentID, &status, &triggerID, &originatorID,
		&accountableID, &originatorSource, &evidenceKind, &evidenceID, &delivered)
	if err != nil {
		return nil
	}
	deliveredIDs := make([]string, 0, len(delivered))
	for _, value := range delivered {
		deliveredIDs = append(deliveredIDs, util.UUIDToString(value))
	}
	return map[string]any{
		"id": util.UUIDToString(id), "agent_id": util.UUIDToString(agentID),
		"status": status, "trigger_comment_id": uuidToPtr(triggerID),
		"originator_user_id":      uuidToPtr(originatorID),
		"accountable_user_id":     uuidToPtr(accountableID),
		"originator_source":       textToPtr(originatorSource),
		"trigger_evidence_kind":   textToPtr(evidenceKind),
		"trigger_evidence_ref_id": uuidToPtr(evidenceID),
		"delivered_comment_ids":   deliveredIDs,
	}
}

func (h *Handler) strikeFlowReplyTaskID(r *http.Request, issueID, commentID pgtype.UUID) *string {
	var taskID pgtype.UUID
	err := h.DB.QueryRow(r.Context(), `
		SELECT id FROM agent_task_queue
		WHERE issue_id=$1 AND trigger_comment_id=$2
		ORDER BY created_at DESC LIMIT 1
	`, issueID, commentID).Scan(&taskID)
	if err != nil {
		return nil
	}
	return uuidToPtr(taskID)
}
