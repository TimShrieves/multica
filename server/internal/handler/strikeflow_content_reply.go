package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const strikeFlowContentReplyBodyLimit = 32 * 1024

type strikeFlowContentReplyRequest struct {
	WorkspaceID        string `json:"workspace_id"`
	RecipientID        string `json:"recipient_id"`
	ProjectID          string `json:"project_id"`
	SourceIssueID      string `json:"source_issue_id"`
	ReplyRootID        string `json:"reply_root_id"`
	ReplyRootHash      string `json:"reply_root_hash"`
	PackageID          string `json:"package_id"`
	PackagePayloadHash string `json:"package_payload_hash"`
	SourceRevision     int32  `json:"source_revision"`
	IdempotencyKey     string `json:"idempotency_key"`
	Message            string `json:"message"`
}

func decodeStrikeFlowContentReply(w http.ResponseWriter, r *http.Request) (strikeFlowContentReplyRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, strikeFlowContentReplyBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req strikeFlowContentReplyRequest
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid content reply")
		return strikeFlowContentReplyRequest{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid content reply")
		return strikeFlowContentReplyRequest{}, false
	}
	req.Message = strings.TrimSpace(req.Message)
	if !validStrikeFlowReply(req.Message) {
		writeError(w, http.StatusBadRequest, "invalid content reply")
		return strikeFlowContentReplyRequest{}, false
	}
	return req, true
}

func strikeFlowReplyRootHash(rootID string) string {
	raw, _ := json.Marshal(struct {
		ReplyRootID string `json:"reply_root_id"`
	}{ReplyRootID: rootID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validStrikeFlowSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func contentReplyPayloadHash(req strikeFlowContentReplyRequest) string {
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (h *Handler) ReplyStrikeFlowContentPackage(w http.ResponseWriter, r *http.Request) {
	scope, ok := requireStrikeFlowScope(w, r, "content:reply")
	if !ok {
		return
	}
	if len(scope.Scopes) != 1 || scope.AgentID == "" {
		writeError(w, http.StatusForbidden, "connector scope denied")
		return
	}
	req, ok := decodeStrikeFlowContentReply(w, r)
	if !ok {
		return
	}
	if req.WorkspaceID != scope.WorkspaceID ||
		req.RecipientID != scope.RecipientID ||
		req.ProjectID == "" ||
		!scope.AllowsProject(req.ProjectID) {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}
	issueID, err := util.ParseUUID(req.SourceIssueID)
	if err != nil {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}
	rootID, err := util.ParseUUID(req.ReplyRootID)
	if err != nil || req.ReplyRootHash != strikeFlowReplyRootHash(util.UUIDToString(rootID)) {
		writeError(w, http.StatusUnprocessableEntity, "content reply binding mismatch")
		return
	}
	projectID, err := util.ParseUUID(req.ProjectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}
	packageID, err := util.ParseUUID(req.PackageID)
	if err != nil || !validStrikeFlowSHA256(req.PackagePayloadHash) || req.SourceRevision < 1 {
		writeError(w, http.StatusUnprocessableEntity, "content reply binding mismatch")
		return
	}
	key, err := util.ParseUUID(req.IdempotencyKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "idempotency_key must be a UUID")
		return
	}
	payloadHash := contentReplyPayloadHash(req)
	item := strikeFlowBoundItem{
		IssueID: issueID, ProjectID: projectID, RootCommentID: rootID,
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start content reply")
		return
	}
	defer tx.Rollback(r.Context())

	// Lock order is invariant across content replies and credential lifecycle:
	// connector token first, then member/agent/source rows, then receipt/comment.
	// Revoke and rotate UPDATE the same token row before touching anything else.
	// Therefore either this mutation commits before lifecycle returns, or it
	// observes the revoked/rotated row and performs no mutation.
	var tokenLocked bool
	err = tx.QueryRow(r.Context(), `
		SELECT true
		FROM strikeflow_connector_token
		WHERE id=$1 AND revoked_at IS NULL AND expires_at > now()
		  AND workspace_id=$2 AND recipient_id=$3 AND agent_id=$4
		  AND scopes=ARRAY['content:reply']::text[]
		  AND cardinality(project_ids)=1 AND $5=ANY(project_ids)
		FOR UPDATE
	`, scope.TokenID, scope.WorkspaceID, scope.RecipientID,
		scope.AgentID, projectID).Scan(&tokenLocked)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock connector token")
		return
	}
	if !tokenLocked {
		writeError(w, http.StatusInternalServerError, "failed to lock connector token")
		return
	}

	// Body fields can only narrow this database authority; they cannot widen it.
	var bindingOK bool
	err = tx.QueryRow(r.Context(), `
		SELECT EXISTS(
			SELECT 1
			FROM member m
			JOIN agent a ON a.id=$3 AND a.workspace_id=m.workspace_id
			JOIN issue i ON i.id=$4 AND i.workspace_id=m.workspace_id
			JOIN comment c ON c.id=$5 AND c.issue_id=i.id AND c.workspace_id=i.workspace_id
			WHERE m.workspace_id=$1 AND m.user_id=$2
			  AND i.project_id=$6
			  AND i.assignee_type='agent' AND i.assignee_id=a.id
			  AND i.status IN ('in_review','in_progress','done')
			  AND a.archived_at IS NULL
			  AND c.parent_id IS NULL AND c.author_type='agent' AND c.author_id=a.id
		)
	`, scope.WorkspaceID, scope.RecipientID, scope.AgentID,
		issueID, rootID, projectID).Scan(&bindingOK)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to validate content reply")
		return
	}
	if !bindingOK {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}

	tag, err := tx.Exec(r.Context(), `
		INSERT INTO strikeflow_connector_content_reply_receipt
			(token_id,workspace_id,recipient_id,agent_id,idempotency_key,
			 issue_id,root_comment_id,reply_root_hash,package_id,
			 package_payload_hash,source_revision,payload_hash)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT DO NOTHING
	`, scope.TokenID, scope.WorkspaceID, scope.RecipientID, scope.AgentID, key,
		issueID, rootID, req.ReplyRootHash, packageID, req.PackagePayloadHash,
		req.SourceRevision, payloadHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reserve content reply")
		return
	}

	var storedIssueID, storedRootID, storedPackageID, commentID pgtype.UUID
	var storedRootHash, storedPackageHash, storedPayloadHash string
	var storedRevision int32
	err = tx.QueryRow(r.Context(), `
		SELECT issue_id,root_comment_id,reply_root_hash,package_id,
		       package_payload_hash,source_revision,payload_hash,comment_id
		FROM strikeflow_connector_content_reply_receipt
		WHERE workspace_id=$1 AND recipient_id=$2 AND agent_id=$3 AND idempotency_key=$4
	`, scope.WorkspaceID, scope.RecipientID, scope.AgentID, key).Scan(
		&storedIssueID, &storedRootID, &storedRootHash, &storedPackageID,
		&storedPackageHash, &storedRevision, &storedPayloadHash, &commentID,
	)
	if err != nil ||
		storedIssueID != issueID ||
		storedRootID != rootID ||
		storedRootHash != req.ReplyRootHash ||
		storedPackageID != packageID ||
		storedPackageHash != req.PackagePayloadHash ||
		storedRevision != req.SourceRevision ||
		storedPayloadHash != payloadHash {
		writeError(w, http.StatusConflict, "idempotency key conflict")
		return
	}

	qtx := h.Queries.WithTx(tx)
	issue, err := qtx.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID: issueID, WorkspaceID: parseUUID(scope.WorkspaceID),
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}
	parent, err := qtx.GetComment(r.Context(), rootID)
	if err != nil {
		writeError(w, http.StatusNotFound, "content reply source not found")
		return
	}

	replayed := commentID.Valid
	var comment db.Comment
	if replayed {
		comment, err = qtx.GetComment(r.Context(), commentID)
		if err != nil {
			writeError(w, http.StatusConflict, "content reply receipt conflict")
			return
		}
	} else {
		body := "Tim reply via StrikeFlow\n[strikeflow-content-reply:" +
			req.IdempotencyKey + "]\n\n" + req.Message
		comment, err = qtx.CreateComment(r.Context(), db.CreateCommentParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "member",
			AuthorID: parseUUID(scope.RecipientID), Content: body, Type: "comment",
			ParentID: rootID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create content reply")
			return
		}
		receiptTag, err := tx.Exec(r.Context(), `
			UPDATE strikeflow_connector_content_reply_receipt
			SET comment_id=$5,committed_at=now()
			WHERE workspace_id=$1 AND recipient_id=$2 AND agent_id=$3
			  AND idempotency_key=$4 AND comment_id IS NULL
		`, scope.WorkspaceID, scope.RecipientID, scope.AgentID, key, comment.ID)
		if err != nil || receiptTag.RowsAffected() != 1 {
			writeError(w, http.StatusInternalServerError, "failed to commit content reply receipt")
			return
		}
	}

	outcome := "allowed"
	if replayed || tag.RowsAffected() == 0 {
		outcome = "replayed"
	}
	if err := auditStrikeFlowConnector(
		tx, r, scope, "content.reply", outcome, item, comment.ID, key, payloadHash,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to audit content reply")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit content reply")
		return
	}

	if !replayed {
		resp := commentToResponse(comment, nil, nil)
		h.publish(protocol.EventCommentCreated, scope.WorkspaceID, "member", scope.RecipientID, map[string]any{
			"comment": resp, "issue_title": issue.Title,
			"issue_assignee_type": textToPtr(issue.AssigneeType),
			"issue_assignee_id":   uuidToPtr(issue.AssigneeID), "issue_status": issue.Status,
		})
		h.TaskService.AutoUnresolveThreadOnReply(
			r.Context(), &parent, scope.WorkspaceID, "member", scope.RecipientID,
		)
	}
	task := h.strikeFlowContentReplyTaskReceipt(r, issueID, comment.ID, scope.AgentID)
	if task == nil {
		// Enqueue is intentionally recoverable after the durable comment commit.
		// A serial replay first queries by the exact trigger; enqueue admission
		// coalesces or blocks while a matching issue-agent task is active.
		_ = h.triggerTasksForComment(
			r.Context(), issue, comment, &parent,
			"member", scope.RecipientID, scope.RecipientID, "", nil,
		)
		task = h.strikeFlowContentReplyTaskReceipt(r, issueID, comment.ID, scope.AgentID)
	}
	if task == nil {
		// The durable comment and idempotency receipt are already committed.
		// A retry replays them and can recover the continuation receipt without
		// ever creating a second comment.
		writeError(w, http.StatusServiceUnavailable, "content reply continuation pending")
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"ok": true,
		"data": map[string]any{
			"comment_id": util.UUIDToString(comment.ID),
			"task_id":    task["id"],
			"task":       task,
		},
	})
}

func (h *Handler) strikeFlowContentReplyTaskReceipt(r *http.Request, issueID, commentID pgtype.UUID, agentID string) map[string]any {
	var id, taskAgentID, triggerID, originatorID, accountableID pgtype.UUID
	err := h.DB.QueryRow(r.Context(), `
		SELECT id,agent_id,trigger_comment_id,originator_user_id,accountable_user_id
		FROM agent_task_queue
		WHERE issue_id=$1 AND trigger_comment_id=$2 AND agent_id=$3
		ORDER BY created_at DESC LIMIT 1
	`, issueID, commentID, agentID).Scan(
		&id, &taskAgentID, &triggerID, &originatorID, &accountableID,
	)
	if err != nil {
		return nil
	}
	return map[string]any{
		"id":                  util.UUIDToString(id),
		"agent_id":            util.UUIDToString(taskAgentID),
		"trigger_comment_id":  uuidToPtr(triggerID),
		"originator_user_id":  uuidToPtr(originatorID),
		"accountable_user_id": uuidToPtr(accountableID),
	}
}
