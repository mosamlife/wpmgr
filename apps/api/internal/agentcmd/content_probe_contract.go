package agentcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// This file is the CP->agent contract for `content_probe`, the read-only check
// of who owns the content of a page. Wire: POST
// {site_url}/wp-json/wpmgr/v1/command/content_probe.
//
// Two request modes, exactly one per call: single (PostID) and list (List).
// The control plane's S1 inventory job uses list mode. Every value the agent
// returns is site-supplied and therefore untrusted: the content package
// validates it against closed sets and shapes before anything is stored.

// ContentProbeVersion is the probe version this contract was written against.
const ContentProbeVersion = 2

// Limits mirrored from the agent. The agent enforces them too; the control
// plane refuses to send past them so a request is never refused for size.
const (
	ContentProbeMaxDescriptors = 32
	ContentProbeMaxListLimit   = 200
	ContentProbeMaxIndicators  = 32
	// ContentProbeTitleMaxBytes is the agent's title cap and the column CHECK.
	ContentProbeTitleMaxBytes = 120
)

// ContentProbeList is list mode's cursor. Offset pages by position; the agent
// orders by post id ascending.
type ContentProbeList struct {
	Types  []string `json:"types"`
	Status []string `json:"status"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

// ContentProbeIndicators are the site-level builder hints. The agent reports a
// hint only for an entry that is active on the site.
type ContentProbeIndicators struct {
	PluginSlugs     []string `json:"plugin_slugs"`
	ThemeSlugs      []string `json:"theme_slugs"`
	MetaKeyPrefixes []string `json:"meta_key_prefixes"`
}

// ContentProbeRequest is the POST body. Exactly one of PostID and List is set.
// Descriptors are data objects built from content_integrations rows, already
// filtered to the keys the agent accepts.
type ContentProbeRequest struct {
	PostID           *int64                 `json:"post_id,omitempty"`
	List             *ContentProbeList      `json:"list,omitempty"`
	AllowedPostTypes []string               `json:"allowed_post_types,omitempty"`
	Descriptors      []json.RawMessage      `json:"descriptors"`
	Indicators       ContentProbeIndicators `json:"indicators"`
}

// ContentProbeRoute is the write route the agent decided on for one page.
type ContentProbeRoute struct {
	Number int    `json:"number"`
	Reason string `json:"reason"`
}

// ContentProbeOwner names the editor that owns a page, as the agent found it.
type ContentProbeOwner struct {
	IntegrationID string  `json:"integration_id"`
	Version       *string `json:"version"`
}

// ContentProbeRow is one page in list mode.
type ContentProbeRow struct {
	Post    int64              `json:"post"`
	Type    string             `json:"type"`
	Status  string             `json:"status"`
	Verdict string             `json:"verdict"`
	Route   ContentProbeRoute  `json:"route"`
	Owner   *ContentProbeOwner `json:"owner"`
	Title   *string            `json:"title"`
}

// ContentProbeListResponse is the list-mode reply. When OK is false the
// Outcome/Code/Detail/Retryable refusal fields are set instead.
type ContentProbeListResponse struct {
	OK           bool              `json:"ok"`
	ProbeVersion int               `json:"probe_version"`
	Mode         string            `json:"mode"`
	Rows         []ContentProbeRow `json:"rows"`
	NextOffset   *int              `json:"next_offset"`
	WPVersion    string            `json:"wp_version"`
	PHPVersion   string            `json:"php_version"`

	Outcome   string `json:"outcome,omitempty"`
	Code      string `json:"code,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// ContentProbeRefusal is a refusal the agent returned as an ok=false body.
// Code is bounded to a short token; Detail is site text, cleaned and capped.
type ContentProbeRefusal struct {
	Code      string
	Detail    string
	Retryable bool
}

func (e *ContentProbeRefusal) Error() string {
	return fmt.Sprintf("content_probe refused by agent: %s", e.Code)
}

// contentProbeCodeRe bounds a refusal code before it is echoed anywhere.
var contentProbeCodeRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// ContentProbeList sends the signed `content_probe` command in list mode. An
// ok=false reply is returned as a *ContentProbeRefusal.
func (c *Client) ContentProbeList(ctx context.Context, siteID uuid.UUID, siteURL string, req ContentProbeRequest) (ContentProbeListResponse, error) {
	var out ContentProbeListResponse
	if req.List == nil || req.PostID != nil {
		return out, fmt.Errorf("content_probe: list mode needs List and no PostID")
	}
	if err := c.post(ctx, siteID, siteURL, "content_probe", req, &out); err != nil {
		return ContentProbeListResponse{}, err
	}
	if !out.OK {
		code := out.Code
		if !contentProbeCodeRe.MatchString(code) {
			code = "unknown"
		}
		return out, &ContentProbeRefusal{
			Code:      code,
			Detail:    humantext.CapBytes(humantext.Clean(out.Detail), 200),
			Retryable: out.Retryable,
		}
	}
	return out, nil
}
