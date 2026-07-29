package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
)

const StrikeFlowConnectorTokenPrefix = "msc_"

type strikeFlowConnectorScopeKey struct{}

// StrikeFlowConnectorScope is authoritative database state, never a client
// claim. It deliberately cannot be converted into a normal user/PAT context.
type StrikeFlowConnectorScope struct {
	TokenID     string
	WorkspaceID string
	RecipientID string
	AgentID     string
	Projects    map[string]struct{}
	Scopes      map[string]struct{}
	ExpiresAt   time.Time
}

func StrikeFlowConnectorScopeFromContext(ctx context.Context) (StrikeFlowConnectorScope, bool) {
	scope, ok := ctx.Value(strikeFlowConnectorScopeKey{}).(StrikeFlowConnectorScope)
	return scope, ok
}

func (s StrikeFlowConnectorScope) Allows(permission string) bool {
	_, ok := s.Scopes[permission]
	return ok
}

func (s StrikeFlowConnectorScope) AllowsProject(projectID string) bool {
	_, ok := s.Projects[projectID]
	return ok
}

// StrikeFlowConnectorAuth accepts only msc_ service credentials and is mounted
// only on /api/integrations/strikeflow. It never stamps X-User-ID, so the token
// cannot inherit the generic member route surface.
func StrikeFlowConnectorAuth(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("X-User-ID")
			r.Header.Del("X-Actor-Source")
			raw := bearerToken(r.Header.Get("Authorization"))
			if !strings.HasPrefix(raw, StrikeFlowConnectorTokenPrefix) {
				http.Error(w, `{"error":"invalid connector token"}`, http.StatusUnauthorized)
				return
			}

			var tokenID, workspaceID, recipientID, agentID pgtype.UUID
			var projects []pgtype.UUID
			var permissions []string
			var expiresAt time.Time
			err := pool.QueryRow(r.Context(), `
				SELECT id, workspace_id, recipient_id, agent_id, project_ids, scopes, expires_at
				FROM strikeflow_connector_token
				WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
			`, auth.HashToken(raw)).Scan(
				&tokenID, &workspaceID, &recipientID, &agentID, &projects, &permissions, &expiresAt,
			)
			if err != nil {
				slog.Warn("strikeflow connector auth rejected", "path", r.URL.Path)
				http.Error(w, `{"error":"invalid connector token"}`, http.StatusUnauthorized)
				return
			}

			scope := StrikeFlowConnectorScope{
				TokenID:     util.UUIDToString(tokenID),
				WorkspaceID: util.UUIDToString(workspaceID),
				RecipientID: util.UUIDToString(recipientID),
				AgentID:     util.UUIDToString(agentID),
				Projects:    make(map[string]struct{}, len(projects)),
				Scopes:      make(map[string]struct{}, len(permissions)),
				ExpiresAt:   expiresAt,
			}
			for _, id := range projects {
				scope.Projects[util.UUIDToString(id)] = struct{}{}
			}
			for _, permission := range permissions {
				scope.Scopes[permission] = struct{}{}
			}
			_, _ = pool.Exec(context.Background(),
				`UPDATE strikeflow_connector_token SET last_used_at = now() WHERE id = $1`,
				tokenID,
			)
			next.ServeHTTP(w, r.WithContext(context.WithValue(
				r.Context(), strikeFlowConnectorScopeKey{}, scope,
			)))
		})
	}
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}
