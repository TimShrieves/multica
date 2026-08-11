package strikeflowresponse

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
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReplayResult is safe to record as rollout evidence. It contains no secret or
// signature material.
type ReplayResult struct {
	EventID       string `json:"event_id"`
	Replay        bool   `json:"replay"`
	ResponseState string `json:"response_state"`
	RecordedAt    string `json:"recorded_at"`
	PayloadSHA256 string `json:"payload_sha256"`
}

// ReplayDeliveredComment replays one already-delivered comment event without
// mutating the Multica outbox. It is deliberately restricted to the exact
// single-command canary configuration.
func ReplayDeliveredComment(ctx context.Context, pool *pgxpool.Pool, config Config, commandID, eventID, expectedPayloadSHA, expectedRecordedAt string) (ReplayResult, error) {
	if config.AuthorizationMode != AuthorizationModeExplicitCommands || len(config.CommandIDs) != 1 || config.CommandIDs[0] != commandID {
		return ReplayResult{}, errors.New("replay requires the exact single-command canary authorization")
	}
	if !sha256Pattern.MatchString(expectedPayloadSHA) {
		return ReplayResult{}, errors.New("expected payload SHA256 must be 64 lowercase hexadecimal characters")
	}
	if _, err := time.Parse(time.RFC3339Nano, expectedRecordedAt); err != nil {
		return ReplayResult{}, errors.New("expected recorded_at must be RFC3339")
	}
	publisher, err := New(pool, config)
	if err != nil {
		return ReplayResult{}, err
	}
	if publisher == nil {
		return ReplayResult{}, errors.New("publisher must be enabled for replay")
	}
	row := outboxRow{}
	var deliveredAt time.Time
	var needsAttention *time.Time
	err = pool.QueryRow(ctx, `
SELECT event_id::text,event_type,strikeflow_command_id::text,workspace_key,
       workspace_id::text,project_id::text,issue_id::text,issue_identifier,
       inbox_item_id::text,root_comment_id::text,member_comment_id::text,
       continuation_task_id::text,recipient_id::text,agent_id::text,
       agent_comment_id::text,agent_comment_parent_id::text,agent_comment_content,
       agent_comment_type,occurred_at,attempt_count,delivered_at,needs_attention_at
FROM strikeflow_response_outbox
WHERE event_id=$1::uuid AND strikeflow_command_id=$2::uuid
  AND event_type='agent_comment.created'
`, eventID, commandID).Scan(
		&row.EventID, &row.EventType, &row.CommandID, &row.WorkspaceKey,
		&row.WorkspaceID, &row.ProjectID, &row.IssueID, &row.IssueIdentifier,
		&row.InboxItemID, &row.RootCommentID, &row.MemberCommentID,
		&row.ContinuationTaskID, &row.RecipientID, &row.AgentID,
		&row.AgentCommentID, &row.AgentCommentParent, &row.AgentCommentContent,
		&row.AgentCommentType, &row.OccurredAt, &row.AttemptCount, &deliveredAt, &needsAttention,
	)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("load exact delivered comment event: %w", err)
	}
	if deliveredAt.IsZero() || row.AttemptCount != 1 || needsAttention != nil || row.AgentCommentID == nil || row.AgentCommentContent == nil {
		return ReplayResult{}, errors.New("comment event is not an exact once-delivered canary event")
	}
	return sendDeliveredCommentReplay(ctx, publisher.config, row, expectedPayloadSHA, expectedRecordedAt)
}

func sendDeliveredCommentReplay(ctx context.Context, config Config, row outboxRow, expectedPayloadSHA, expectedRecordedAt string) (ReplayResult, error) {
	body, err := json.Marshal(row.payload())
	if err != nil {
		return ReplayResult{}, err
	}
	digest := sha256.Sum256(body)
	payloadSHA := hex.EncodeToString(digest[:])
	if payloadSHA != expectedPayloadSHA {
		return ReplayResult{}, errors.New("reconstructed payload SHA256 does not match the immutable StrikeFlow ledger")
	}
	timestamp := fmt.Sprintf("%d", config.Now().Unix())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return ReplayResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Multica-Event-ID", row.EventID)
	req.Header.Set("X-Multica-Timestamp", timestamp)
	req.Header.Set("X-Multica-Key-ID", config.HMACKeyID)
	req.Header.Set("X-Multica-Signature", "sha256="+Sign(config.HMACSecret, timestamp, body))
	resp, err := config.HTTPClient.Do(req)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("post exact replay: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return ReplayResult{}, err
	}
	if resp.StatusCode != http.StatusOK || len(responseBody) > 4096 {
		return ReplayResult{}, fmt.Errorf("replay receiver returned HTTP %d", resp.StatusCode)
	}
	var ack struct {
		OK   bool `json:"ok"`
		Data struct {
			EventID       string `json:"event_id"`
			Replay        bool   `json:"replay"`
			ResponseState string `json:"response_state"`
			RecordedAt    string `json:"recorded_at"`
		} `json:"data"`
	}
	if json.Unmarshal(responseBody, &ack) != nil || !ack.OK || !ack.Data.Replay || ack.Data.EventID != row.EventID || ack.Data.ResponseState != "responding" {
		return ReplayResult{}, errors.New("StrikeFlow returned an invalid replay acknowledgement")
	}
	wantRecorded, _ := time.Parse(time.RFC3339Nano, expectedRecordedAt)
	gotRecorded, err := time.Parse(time.RFC3339Nano, ack.Data.RecordedAt)
	if err != nil || !gotRecorded.Equal(wantRecorded) {
		return ReplayResult{}, errors.New("replay changed the immutable recorded_at identity")
	}
	return ReplayResult{
		EventID: row.EventID, Replay: true, ResponseState: ack.Data.ResponseState,
		RecordedAt: ack.Data.RecordedAt, PayloadSHA256: payloadSHA,
	}, nil
}
