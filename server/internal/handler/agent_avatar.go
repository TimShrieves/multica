package handler

import (
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// newAgentAvatar normalises a create-time avatar_url. An omitted, empty, or
// whitespace-only value stores NULL rather than a generated placeholder: an
// agent without an uploaded avatar renders the icon of the runtime it is bound
// to, and that has to stay correct when the agent is later rebound to another
// runtime. Persisting a snapshot of the runtime — or a random glyph, as the
// former emoji default did — would freeze the wrong identity into the row.
func newAgentAvatar(avatarURL *string) pgtype.Text {
	if avatarURL == nil || strings.TrimSpace(*avatarURL) == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *avatarURL, Valid: true}
}
