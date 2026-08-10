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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"golang.org/x/sys/unix"
)

const (
	maxAttempts      = 12
	maxResponseBytes = 10_000
	requestTimeout   = 10 * time.Second
	leaseDuration    = 30 * time.Second
	pollInterval     = time.Second
	recoveryInterval = 30 * time.Second
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var requiredSecretOwnerUID uint32 = 0

const responseWebhookPath = "/api/integrations/multica/content-delivery/responses"

const (
	protectedSTR94IssueID  = "b41bcb97-8b63-43f6-9d6c-4ee9e9ada891"
	protectedSTR166IssueID = "39dcf540-bedf-4449-bc71-2e9e15fa0573"
	protectedSTR172IssueID = "b1839f3d-97e5-449a-9059-21b3b393d096"
)

const (
	AuthorizationModeExplicitCommands = "explicit_commands"
	AuthorizationModeReceiptLineage   = "receipt_lineage"
)

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
	// AuthorizationMode chooses whether recovery is constrained to an
	// operator-maintained command allowlist or to every post-floor receipt
	// that satisfies the same exact connector lineage and scope joins.
	AuthorizationMode string
	CommandIDs        []string
	RecipientID       string
	AgentID           string
	STR94IssueID      string
	ExcludedIssueIDs  []string
	NotBefore         time.Time
	HTTPClient        *http.Client
	Now               func() time.Time
}

func ConfigFromEnv() (Config, error) {
	enabled := strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED")) == "true"
	config := Config{Enabled: enabled}
	if os.Getenv("STRIKEFLOW_RESPONSE_HMAC_SECRET") != "" {
		return config, errors.New("STRIKEFLOW_RESPONSE_HMAC_SECRET is forbidden; use STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE")
	}
	if !enabled {
		return config, nil
	}
	config.WebhookURL = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_WEBHOOK_URL"))
	secretFile := strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE"))
	if !filepath.IsAbs(secretFile) {
		return config, errors.New("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE must be an absolute path")
	}
	secretFD, err := unix.Open(secretFile, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return config, fmt.Errorf("open STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE without following symlinks: %w", err)
	}
	secretHandle := os.NewFile(uintptr(secretFD), secretFile)
	if secretHandle == nil {
		_ = unix.Close(secretFD)
		return config, errors.New("open STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE returned an invalid descriptor")
	}
	defer secretHandle.Close()
	secretInfo, err := secretHandle.Stat()
	if err != nil {
		return config, fmt.Errorf("stat open STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE: %w", err)
	}
	secretStat, ok := secretInfo.Sys().(*syscall.Stat_t)
	if !ok || !secretInfo.Mode().IsRegular() || secretInfo.Mode().Perm() != 0o600 || secretStat.Uid != requiredSecretOwnerUID {
		return config, errors.New("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE must be a regular root-owned mode-0600 file")
	}
	secret, err := io.ReadAll(io.LimitReader(secretHandle, 4097))
	if err != nil {
		return config, fmt.Errorf("read STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE: %w", err)
	}
	if len(secret) > 4096 {
		return config, errors.New("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE must not exceed 4096 bytes")
	}
	config.HMACSecret = string(secret)
	if config.HMACSecret != strings.TrimSpace(config.HMACSecret) {
		return config, errors.New("STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE must not contain surrounding whitespace")
	}
	config.HMACKeyID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_HMAC_KEY_ID"))
	config.WorkspaceID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_WORKSPACE_ID"))
	config.WorkspaceKey = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_WORKSPACE_KEY"))
	config.ProjectIDs = splitExactList(os.Getenv("STRIKEFLOW_RESPONSE_PROJECT_IDS"))
	config.AuthorizationMode = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE"))
	config.CommandIDs = splitExactList(os.Getenv("STRIKEFLOW_RESPONSE_COMMAND_IDS"))
	config.RecipientID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_RECIPIENT_ID"))
	config.AgentID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_AGENT_ID"))
	config.STR94IssueID = strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_STR94_ISSUE_ID"))
	config.ExcludedIssueIDs = splitExactList(os.Getenv("STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS"))
	if config.STR94IssueID != protectedSTR94IssueID || !sameStringSet(config.ExcludedIssueIDs, []string{
		protectedSTR94IssueID, protectedSTR166IssueID, protectedSTR172IssueID,
	}) {
		return config, errors.New("STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS must exactly protect STR-94, STR-166, and STR-172")
	}
	notBefore := strings.TrimSpace(os.Getenv("STRIKEFLOW_RESPONSE_NOT_BEFORE"))
	parsedNotBefore, err := time.Parse(time.RFC3339, notBefore)
	if err != nil {
		return config, errors.New("STRIKEFLOW_RESPONSE_NOT_BEFORE must be an exact RFC3339 timestamp")
	}
	config.NotBefore = parsedNotBefore
	if config.NotBefore.After(time.Now().UTC()) {
		return config, errors.New("STRIKEFLOW_RESPONSE_NOT_BEFORE must not be in the future")
	}
	return config, config.Validate()
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[string]struct{}, len(right))
	for _, value := range right {
		want[value] = struct{}{}
	}
	for _, value := range left {
		if _, ok := want[value]; !ok {
			return false
		}
	}
	return true
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
	ids = append(ids, c.ExcludedIssueIDs...)
	if len(c.ExcludedIssueIDs) > 0 {
		protected := false
		for _, issueID := range c.ExcludedIssueIDs {
			if issueID == c.STR94IssueID {
				protected = true
				break
			}
		}
		if !protected {
			return errors.New("STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS must include STRIKEFLOW_RESPONSE_STR94_ISSUE_ID")
		}
	}
	if len(c.ProjectIDs) == 0 || len(c.ProjectIDs) > 32 {
		return errors.New("STRIKEFLOW_RESPONSE_PROJECT_IDS must contain 1-32 exact project UUIDs")
	}
	switch c.AuthorizationMode {
	case AuthorizationModeExplicitCommands:
		if len(c.CommandIDs) == 0 || len(c.CommandIDs) > 32 {
			return errors.New("STRIKEFLOW_RESPONSE_COMMAND_IDS must contain 1-32 exact command UUIDs in explicit_commands mode")
		}
		ids = append(ids, c.CommandIDs...)
	case AuthorizationModeReceiptLineage:
		if len(c.CommandIDs) != 0 {
			return errors.New("STRIKEFLOW_RESPONSE_COMMAND_IDS must be empty in receipt_lineage mode")
		}
	default:
		return errors.New("STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE must be explicit_commands or receipt_lineage")
	}
	for _, value := range ids {
		if _, err := uuid.Parse(value); err != nil {
			return fmt.Errorf("StrikeFlow response publisher contains an invalid exact-scope UUID: %q", value)
		}
	}
	return nil
}

func (c Config) excludedIssueIDs() []string {
	if len(c.ExcludedIssueIDs) > 0 {
		return c.ExcludedIssueIDs
	}
	return []string{c.STR94IssueID}
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
		p.config.AgentID, p.config.excludedIssueIDs(), maxResponseBytes, p.config.WorkspaceKey,
		p.config.NotBefore, p.config.AuthorizationMode, p.config.CommandIDs); err != nil {
		return fmt.Errorf("recover agent comments: %w", err)
	}
	if _, err := p.pool.Exec(ctx, recoverTaskCompletionsSQL,
		p.config.WorkspaceID, p.config.ProjectIDs, p.config.RecipientID,
		p.config.AgentID, p.config.excludedIssueIDs(), p.config.WorkspaceKey,
		p.config.NotBefore, p.config.AuthorizationMode, p.config.CommandIDs); err != nil {
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
JOIN comment member
  ON member.id=rr.comment_id AND member.issue_id=i.id AND member.workspace_id=i.workspace_id
  AND member.parent_id=rr.root_comment_id AND member.author_type='member'
  AND member.author_id=t.recipient_id
JOIN comment root
  ON root.id=rr.root_comment_id AND root.issue_id=i.id AND root.workspace_id=i.workspace_id
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
  AND i.id<>ALL($5::uuid[])
  AND octet_length(c.content)<=$6
  AND rr.strikeflow_command_id IS NOT NULL
  AND ($9::text='receipt_lineage' OR rr.strikeflow_command_id=ANY($10::uuid[]))
  AND rr.created_at >= $8::timestamptz
  AND rr.committed_at >= $8::timestamptz
  AND c.created_at >= $8::timestamptz
  AND (
      SELECT count(*) FROM comment prior
      WHERE prior.source_task_id=q.id
        AND prior.author_type='agent' AND prior.author_id=q.agent_id
        AND (prior.created_at,prior.id)<=(c.created_at,c.id)
  ) <= 100
  AND EXISTS (
      WITH RECURSIVE canonical_chain(id,parent_id) AS (
          SELECT c.id,c.parent_id
          UNION ALL
          SELECT parent.id,parent.parent_id
          FROM comment parent JOIN canonical_chain child ON parent.id=child.parent_id
          WHERE parent.issue_id=i.id AND parent.workspace_id=i.workspace_id
            AND parent.author_type='agent' AND parent.author_id=q.agent_id
            AND parent.source_task_id=q.id
      )
      SELECT 1 FROM canonical_chain WHERE parent_id=rr.comment_id
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
JOIN comment member
  ON member.id=rr.comment_id AND member.issue_id=i.id AND member.workspace_id=i.workspace_id
  AND member.parent_id=rr.root_comment_id AND member.author_type='member'
  AND member.author_id=t.recipient_id
JOIN comment root
  ON root.id=rr.root_comment_id AND root.issue_id=i.id AND root.workspace_id=i.workspace_id
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
  AND i.id<>ALL($5::uuid[])
  AND rr.strikeflow_command_id IS NOT NULL
  AND ($8::text='receipt_lineage' OR rr.strikeflow_command_id=ANY($9::uuid[]))
  AND rr.created_at >= $7::timestamptz
  AND rr.committed_at >= $7::timestamptz
  AND q.completed_at >= $7::timestamptz
  AND (
      SELECT count(*) FROM comment response
      WHERE response.source_task_id=q.id
        AND response.author_type='agent' AND response.author_id=q.agent_id
  ) <= 100
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
            SELECT o.event_id FROM strikeflow_response_outbox o
            WHERE o.delivered_at IS NULL
              AND o.needs_attention_at IS NULL
              AND o.next_attempt_at<=now() AND (o.lease_until IS NULL OR o.lease_until<now())
              AND (
                (o.event_type='agent_comment.created' AND (
                  o.agent_comment_parent_id=o.member_comment_id
                  OR EXISTS (
                    SELECT 1 FROM strikeflow_response_outbox parent
                    WHERE parent.strikeflow_command_id=o.strikeflow_command_id
                      AND parent.continuation_task_id=o.continuation_task_id
                      AND parent.event_type='agent_comment.created'
                      AND parent.agent_comment_id=o.agent_comment_parent_id
                      AND parent.delivered_at IS NOT NULL
                  )
                ))
                OR (o.event_type='task.completed' AND NOT EXISTS (
                  SELECT 1 FROM strikeflow_response_outbox comment
                  WHERE comment.strikeflow_command_id=o.strikeflow_command_id
                    AND comment.continuation_task_id=o.continuation_task_id
                    AND comment.event_type='agent_comment.created'
                    AND comment.delivered_at IS NULL
                ))
              )
            ORDER BY o.occurred_at,
              CASE WHEN o.event_type='task.completed' THEN 1 ELSE 0 END,
              o.event_id
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
		return true, p.attention(ctx, row, err)
	}
	timestamp := fmt.Sprintf("%d", p.config.Now().Unix())
	signature := Sign(p.config.HMACSecret, timestamp, body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return true, p.attention(ctx, row, err)
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
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if readErr != nil {
		return true, p.retry(ctx, row, fmt.Errorf("read StrikeFlow webhook response: %w", readErr))
	}
	if resp.StatusCode != http.StatusOK {
		cause := fmt.Errorf("StrikeFlow webhook returned HTTP %d", resp.StatusCode)
		if isTransientStatus(resp.StatusCode) {
			return true, p.retry(ctx, row, cause)
		}
		return true, p.attention(ctx, row, cause)
	}
	var ack struct {
		OK   bool `json:"ok"`
		Data struct {
			EventID string `json:"event_id"`
		} `json:"data"`
	}
	if len(responseBody) > 4096 || json.Unmarshal(responseBody, &ack) != nil || !ack.OK || ack.Data.EventID != row.EventID {
		return true, p.attention(ctx, row, errors.New("StrikeFlow webhook returned an invalid acknowledgement"))
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
                lease_until=NULL,last_error=$2
            WHERE event_id=$1 AND delivered_at IS NULL
		`, row.EventID, message)
		if err == nil && tag.RowsAffected() == 1 && row.AttemptCount == maxAttempts {
			slog.Warn("strikeflow response delivery requires operator attention",
				"event_id", row.EventID, "attempt_count", row.AttemptCount,
				"error", message)
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

func (p *Publisher) attention(ctx context.Context, row outboxRow, cause error) error {
	message := cause.Error()
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := p.pool.Exec(ctx, `
        UPDATE strikeflow_response_outbox
        SET needs_attention_at=COALESCE(needs_attention_at,now()),
            lease_until=NULL,last_error=$2
        WHERE event_id=$1 AND delivered_at IS NULL
    `, row.EventID, message)
	return err
}

func isTransientStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests || status >= 500
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
