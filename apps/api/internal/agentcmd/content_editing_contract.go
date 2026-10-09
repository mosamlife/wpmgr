package agentcmd

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// CmdContentEditingEnable creates (or confirms) the site's WPMgr content
// service user, the principal every engine write runs as. The body is
// exactly {} (class-content-editing-enable-command.php). Idempotent on the
// agent: a second call confirms the existing principal.
const CmdContentEditingEnable = "content_editing_enable"

// ContentEditingEnableResponse is the agent's reply.
type ContentEditingEnableResponse struct {
	OK        bool     `json:"ok"`
	Outcome   string   `json:"outcome"`
	UserID    int64    `json:"user_id"`
	Role      string   `json:"role"`
	Caps      []string `json:"caps"`
	Code      string   `json:"code,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Retryable bool     `json:"retryable,omitempty"`
}

// ContentEditingEnableRefusal is an ok=false reply.
type ContentEditingEnableRefusal struct {
	Code   string
	Detail string
}

func (e *ContentEditingEnableRefusal) Error() string {
	return fmt.Sprintf("content_editing_enable refused by agent: %s", e.Code)
}

var contentEditingCodeRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// ContentEditingEnable sends the signed enable command.
func (c *Client) ContentEditingEnable(ctx context.Context, siteID uuid.UUID, siteURL string) (ContentEditingEnableResponse, error) {
	var out ContentEditingEnableResponse
	if err := c.post(ctx, siteID, siteURL, CmdContentEditingEnable, struct{}{}, &out); err != nil {
		return ContentEditingEnableResponse{}, err
	}
	if !out.OK {
		code := out.Code
		if !contentEditingCodeRe.MatchString(code) {
			code = "unknown"
		}
		return out, &ContentEditingEnableRefusal{Code: code, Detail: humantext.CapBytes(humantext.Clean(out.Detail), 200)}
	}
	return out, nil
}
