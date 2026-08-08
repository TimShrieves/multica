package strikeflowresponse

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	maxAttempts      = 12
	maxResponseBytes = 10_000
	requestTimeout   = 10 * time.Second
	leaseDuration    = 30 * time.Second
	pollInterval     = time.Second
	recoveryInterval = 30 * time.Second
	attentionRetry   = 6 * time.Hour
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const responseWebhookPath = "/api/integrations/multica/content-delivery/responses"

// Config is intentionally exact and fail-closed. A publisher cannot be
// enabled with a wildcard workspace, project, recipient, agent, or legacy
// exclusion.
type Config struct {
	Enabled      bool
	WebhookURL   string
	HMACSecret   string
	HMACKeyID    string
	WorkspaceID  string
	WorkspaceKey string
	ProjectIDs   []string
	RecipientID  string
	AgentID      string
	STR94IssueID string
	NotBefore    time.Time
	HTTPClient   *http.Client
	Now          func() time.Time
}

func ConfigFromEnv() (Config, error) {
	enabled := strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED")) == "true"
	config := Config{Enabled: enabled}
	if !enabled {
		return config, nil
	}
	config.WebhookURL = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_WEBHOOK_URL"))
	config.HMACSecret = os.Getenv("STRIKEFLOW_RESPONSE_HMAC_SECRET")
	config.HMACKeyID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_HMAC_KEY_ID"))
	config.WorkspaceID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_WORKSPACE_ID"))
	config.WorkspaceKey = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_WORKSPACE_KEY"))
	config.ProjectIDs = splitExactList(os.Getenv("STRIKEFLOW_RESPONSE_PROJECT_IDS"))
	config.RecipientID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_RECIPIENT_ID"))
	config.AgentID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_AGENT_ID"))
	config.STR94IssueID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_STR94_ISSUE_ID"))
	notBefore := strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_NOT_BEFORE"))
	parsedNotBefore, err := time.Parse(time.RFC3339, notBefore)
	if err != nil {
		return config, errors.New("STRIKEFLOW_RESPONSE_NOT_BEFORE must be an exact RFC3339 timestamp")
	}
	config.NotBefore = parsedNotBefore
	return config, config.Validate()
}

func splitExactList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, exists := seen[part]; exists {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	sort.Strings(out)
	return out
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	parsedURL, err := url.Parse(c.WebhookURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.Path != responseWebhookPath || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return fmt.Errorf("STRIKEFLOW_RESPONSE_WEBHOOK_URL must be an HTTPS origin plus exact path %s", responseWebhookPath)
	}
	if len(c.HMACSecret) < 32 {
		return errors.New("STRIKEFLOW_RESPONSE_HMAC_SECRET must contain at least 32 bytes")
	}
	if !keyIDPattern.MatchString(c.HMACKeyID) {
		return errors.New("STRIKEFLOW_RESPONSE_HMAC_KEY_ID is invalid")
	}
	if c.WorkspaceKey == "" || len(c.WorkspaceKey) > 100 {
		return errors.New("STRIKEFLOW_RESPONSE_WORKSPACE_KEY must be an exact non-empty key")
	}
	if c.NotBefore.IsZero() {
		return errors.New("STRIKEFLOW_RESPONSE_NOT_BEFORE is required when the publisher is enabled")
	}
	ids := append([]string{c.WorkspaceID, c.RecipientID, c.AgentID, c.STR94IssueID}, c.ProjectIDs...)
	if len(c.ProjectIDs) == 0 || len(c.ProjectIDs) > 32 {
		return errors.New("STRIKEFLOW_RESPONSE_PROJECT_IDS must contain 1-32 exact project UUIDs")
	}
	for _, value := range ids {
		if _, err := uuid.Parse(value); err != nil {
			return fmt.Errorf("StrikeFlow response publisher contains an invalid exact-scope UUID: %q", value)
		}
	}
	return nil
}

type Publisher struct {
	pool   *pgxpool.Pool
	config Config
	wake   chan struct{}
}

func New(pool *pgxpool.Pool, config Config) (*Publisher, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if !config.Enabled {
		return nil, nil
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Publisher{pool: pool, config: config, wake: make(chan struct{}, 1)}, nil
}

func Register(bus *events.Bus, publisher *Publisher) {
	if publisher == nil {
		return
	}
	bus.Subscribe(protocol.EventCommentCreated, publisher.NotifyEvent)
	bus.Subscribe(protocol.EventTaskCompleted, publisher.NotifyEvent)
}

// NotifyEvent is safe to register directly on the synchronous domain bus. It
// never performs I/O; the durable scan in Run resolves and verifies all scope
// bindings from PostgreSQL.
func (p *Publisher) NotifyEvent(event events.Event) {
	if p == nil || (event.Type != protocol.EventCommentCreated && event.Type != protocol.EventTaskCompleted) {
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Publisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	poll := time.NewTicker(pollInterval)
	recoverTicker := time.NewTicker(recoveryInterval)
	defer poll.Stop()
	defer recoverTicker.Stop()
	if err := p.RecoverOnce(ctx); err != nil {
		slog.Error("strikeflow response publisher recovery failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			if err := p.RecoverOnce(ctx); err != nil {
				slog.Error("strikeflow response publisher event recovery failed", "error", err)
			}
			p.drain(ctx)
		case <-recoverTicker.C:
			if err := p.RecoverOnce(ctx); err != nil {
				slog.Error("strikeflow response publisher periodic recovery failed", "error", err)
			}
		case <-poll.C:
			p.drain(ctx)
		}
	}
}

func (p *Publisher) drain(ctx context.Context) {
	for {
		processed, err := p.DeliverOnce(ctx)
		if err != nil {
			slog.Error("strikeflow response publisher delivery failed", "error", err)
			return
		}
		if !processed {
			return
		}
	}
}

// RecoverOnce makes the in-memory event notification crash-safe. It inserts
// only rows whose complete connector lineage and configured scope match.
func (p *Publisher) RecoverOnce(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if _, err := p.pool.Exec(ctx, recoverAgentCommentsSQL,
		p.config.WorkspaceID, p.config.ProjectIDs, p.config.RecipientID,
		p.config.AgentID, p.config.STR94IssueID, maxResponseBytes, p.config.WorkspaceKey,
		p.config.NotBefore); err != nil {
		return fmt.Errorf("recover agent comments: %w", err)
	}
	if _, err := p.pool.Exec(ctx, recoverTaskCompletionsSQL,
		p.config.WorkspaceID, p.config.ProjectIDs, p.config.RecipientID,
		p.config.AgentID, p.config.STR94IssueID, p.config.WorkspaceKey,
		p.config.NotBefore); err != nil {
		return fmt.Errorf("recover task completions: %w", err)
	}
	return nil
}

const recoverAgentCommentsSQL = `
INSERT INTO strikeflow_response_outbox (
    event_type,strikeflow_command_id,workspace_key,workspace_id,project_id,issue_id,issue_identifier,inbox_item_id,
    root_comment_id,member_comment_id,continuation_task_id,recipient_id,agent_id,
    agent_comment_id,agent_comment_parent_id,agent_comment_content,agent_comment_type,occurred_at
)
SELECT 'agent_comment.created',rr.strikeflow_command_id,$7,i.workspace_id,i.project_id,i.id,
       w.issue_prefix || '-' || i.number,rr.inbox_item_id,rr.root_comment_id,
       rr.comment_id,q.id,t.recipient_id,q.agent_id,c.id,c.parent_id,c.content,c.type,c.created_at
FROM comment c
JOIN agent_task_queue q ON q.id=c.source_task_id AND q.agent_id=c.author_id
JOIN strikeflow_connector_reply_receipt rr
  ON rr.comment_id=q.trigger_comment_id AND rr.issue_id=q.issue_id
JOIN strikeflow_connector_token t ON t.id=rr.token_id
JOIN issue i ON i.id=q.issue_id AND i.workspace_id=c.workspace_id
JOIN workspace w ON w.id=i.workspace_id
WHERE c.author_type='agent'
  AND i.workspace_id=$1::uuid
  AND i.project_id=ANY($2::uuid[])
  AND t.workspace_id=i.workspace_id
  AND t.recipient_id=$3::uuid
  AND q.agent_id=$4::uuid
  AND q.originator_user_id=$3::uuid
  AND q.accountable_user_id=$3::uuid
  AND q.originator_source='direct_human'
  AND q.trigger_evidence_kind='comment'
  AND q.trigger_evidence_ref_id=rr.comment_id
  AND i.id<>$5::uuid
  AND octet_length(c.content)<=$6
  AND rr.strikeflow_command_id IS NOT NULL
  AND rr.created_at >= $8::timestamptz
  AND rr.committed_at >= $8::timestamptz
  AND c.created_at >= $8::timestamptz
  AND EXISTS (
      WITH RECURSIVE ancestors(id) AS (
          SELECT c.parent_id
          UNION ALL
          SELECT parent.parent_id
          FROM comment parent JOIN ancestors a ON parent.id=a.id
          WHERE parent.issue_id=i.id AND parent.workspace_id=i.workspace_id
      )
      SELECT 1 FROM ancestors WHERE id=rr.comment_id
  )
ON CONFLICT DO NOTHING`

const recoverTaskCompletionsSQL = `
INSERT INTO strikeflow_response_outbox (
    event_type,strikeflow_command_id,workspace_key,workspace_id,project_id,issue_id,issue_identifier,inbox_item_id,
    root_comment_id,member_comment_id,continuation_task_id,recipient_id,agent_id,occurred_at
)
SELECT 'task.completed',rr.strikeflow_command_id,$6,i.workspace_id,i.project_id,i.id,
       w.issue_prefix || '-' || i.number,rr.inbox_item_id,rr.root_comment_id,
       rr.comment_id,q.id,t.recipient_id,q.agent_id,q.completed_at
FROM agent_task_queue q
JOIN strikeflow_connector_reply_receipt rr
  ON rr.comment_id=q.trigger_comment_id AND rr.issue_id=q.issue_id
JOIN strikeflow_connector_token t ON t.id=rr.token_id
JOIN issue i ON i.id=q.issue_id
JOIN workspace w ON w.id=i.workspace_id
WHERE q.status='completed' AND q.completed_at IS NOT NULL
  AND i.workspace_id=$1::uuid
  AND i.project_id=ANY($2::uuid[])
  AND t.workspace_id=i.workspace_id
  AND t.recipient_id=$3::uuid
  AND q.agent_id=$4::uuid
  AND q.originator_user_id=$3::uuid
  AND q.accountable_user_id=$3::uuid
  AND q.originator_source='direct_human'
  AND q.trigger_evidence_kind='comment'
  AND q.trigger_evidence_ref_id=rr.comment_id
  AND i.id<>$5::uuid
  AND rr.strikeflow_command_id IS NOT NULL
  AND rr.created_at >= $7::timestamptz
  AND rr.committed_at >= $7::timestamptz
  AND q.completed_at >= $7::timestamptz
ON CONFLICT DO NOTHING`

type outboxRow struct {
	EventID             string
	EventType           string
	CommandID           string
	WorkspaceKey        string
	WorkspaceID         string
	ProjectID           string
	IssueID             string
	IssueIdentifier     string
	InboxItemID         string
	RootCommentID       string
	MemberCommentID     string
	ContinuationTaskID  string
	RecipientID         string
	AgentID             string
	AgentCommentID      *string
	AgentCommentParent  *string
	AgentCommentContent *string
	AgentCommentType    *string
	OccurredAt          time.Time
	AttemptCount        int
}

type eventPayload struct {
	EventType       string  `json:"event_type"`
	CommandID       string  `json:"command_id"`
	WorkspaceKey    string  `json:"workspace_key"`
	ProjectID       string  `json:"project_id"`
	SourceIssueID   string  `json:"source_issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	ReplyRootID     string  `json:"reply_root_id"`
	RecipientID     string  `json:"recipient_id"`
	AgentID         string  `json:"agent_id"`
	TaskID          string  `json:"task_id"`
	ParentCommentID string  `json:"parent_comment_id"`
	CommentID       string  `json:"comment_id"`
	ResponseText    *string `json:"response_text,omitempty"`
	OccurredAt      string  `json:"occurred_at"`
}

func (row outboxRow) payload() eventPayload {
	payload := eventPayload{
		EventType: row.EventType, CommandID: row.CommandID, WorkspaceKey: row.WorkspaceKey,
		OccurredAt: row.OccurredAt.UTC().Format(time.RFC3339Nano),
		ProjectID:  row.ProjectID, SourceIssueID: row.IssueID, IssueIdentifier: row.IssueIdentifier,
		ReplyRootID: row.RootCommentID, RecipientID: row.RecipientID, AgentID: row.AgentID,
		TaskID: row.ContinuationTaskID, ParentCommentID: row.MemberCommentID,
		CommentID: row.ContinuationTaskID,
	}
	if row.AgentCommentID != nil && row.AgentCommentContent != nil {
		payload.CommentID = *row.AgentCommentID
		payload.ResponseText = row.AgentCommentContent
		if row.AgentCommentParent != nil {
			payload.ParentCommentID = *row.AgentCommentParent
		}
	}
	return payload
}

func (p *Publisher) DeliverOnce(ctx context.Context) (bool, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	row := outboxRow{}
	err = tx.QueryRow(ctx, `
        WITH candidate AS (
            SELECT event_id FROM strikeflow_response_outbox
            WHERE delivered_at IS NULL
              AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now())
            ORDER BY next_attempt_at,created_at
            FOR UPDATE SKIP LOCKED LIMIT 1
        )
        UPDATE strikeflow_response_outbox o
        SET lease_until=now()+$1::interval,attempt_count=attempt_count+1
        FROM candidate WHERE o.event_id=candidate.event_id
        RETURNING o.event_id::text,o.event_type,o.strikeflow_command_id::text,o.workspace_key,
          o.workspace_id::text,o.project_id::text,
          o.issue_id::text,o.issue_identifier,o.inbox_item_id::text,o.root_comment_id::text,
          o.member_comment_id::text,o.continuation_task_id::text,o.recipient_id::text,o.agent_id::text,
          o.agent_comment_id::text,o.agent_comment_parent_id::text,o.agent_comment_content,
          o.agent_comment_type,o.occurred_at,o.attempt_count
    `, leaseDuration.String()).Scan(
		&row.EventID, &row.EventType, &row.CommandID, &row.WorkspaceKey,
		&row.WorkspaceID, &row.ProjectID,
		&row.IssueID, &row.IssueIdentifier, &row.InboxItemID, &row.RootCommentID,
		&row.MemberCommentID, &row.ContinuationTaskID, &row.RecipientID, &row.AgentID,
		&row.AgentCommentID, &row.AgentCommentParent, &row.AgentCommentContent,
		&row.AgentCommentType, &row.OccurredAt, &row.AttemptCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	body, err := json.Marshal(row.payload())
	if err != nil {
		return true, p.retry(ctx, row, err)
	}
	timestamp := fmt.Sprintf("%d", p.config.Now().Unix())
	signature := Sign(p.config.HMACSecret, timestamp, body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return true, p.retry(ctx, row, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Multica-Event-ID", row.EventID)
	req.Header.Set("X-Multica-Timestamp", timestamp)
	req.Header.Set("X-Multica-Key-ID", p.config.HMACKeyID)
	req.Header.Set("X-Multica-Signature", "sha256="+signature)
	resp, err := p.config.HTTPClient.Do(req)
	if err != nil {
		return true, p.retry(ctx, row, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return true, p.retry(ctx, row, fmt.Errorf("StrikeFlow webhook returned HTTP %d", resp.StatusCode))
	}
	_, err = p.pool.Exec(ctx, `
        UPDATE strikeflow_response_outbox
        SET delivered_at=now(),lease_until=NULL,last_error=NULL,needs_attention_at=NULL
        WHERE event_id=$1 AND delivered_at IS NULL
    `, row.EventID)
	return true, err
}

func (p *Publisher) retry(ctx context.Context, row outboxRow, cause error) error {
	message := cause.Error()
	if len(message) > 1000 {
		message = message[:1000]
	}
	if row.AttemptCount >= maxAttempts {
		tag, err := p.pool.Exec(ctx, `
            UPDATE strikeflow_response_outbox
            SET needs_attention_at=COALESCE(needs_attention_at,now()),
                next_attempt_at=now()+$2::interval,lease_until=NULL,last_error=$3
            WHERE event_id=$1 AND delivered_at IS NULL
		`, row.EventID, attentionRetry.String(), message)
		if err == nil && tag.RowsAffected() == 1 && row.AttemptCount == maxAttempts {
			slog.Warn("strikeflow response delivery entered needs-attention retry",
				"event_id", row.EventID, "attempt_count", row.AttemptCount,
				"next_retry_in", attentionRetry.String(), "error", message)
		}
		return err
	}
	delay := retryDelay(row.AttemptCount)
	_, err := p.pool.Exec(ctx, `
        UPDATE strikeflow_response_outbox
        SET next_attempt_at=now()+$2::interval,lease_until=NULL,last_error=$3
        WHERE event_id=$1 AND delivered_at IS NULL
    `, row.EventID, delay.String(), message)
	return err
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 5 * time.Second * time.Duration(1<<min(attempt-1, 8))
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

// Sign returns the lowercase hexadecimal HMAC over the Unix timestamp and
// exact raw request body separated by one period.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
