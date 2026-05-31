package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxMetadataBytes bounds the agent metadata body (untrusted input).
const maxMetadataBytes = 16 << 20 // 16 MiB

// flexString decodes a JSON string, OR an object (best-effort: stylesheet→slug→
// name→title), OR a number/bool (stringified), OR null → "". Real agents have
// sent e.g. active_theme as an OBJECT, which a plain string field would 422 on.
// We never error: telemetry must not fail a sync over a field's shape.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	switch b[0] {
	case '"':
		var str string
		_ = json.Unmarshal(b, &str)
		*s = flexString(str)
	case '{':
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			for _, k := range []string{"stylesheet", "slug", "name", "title"} {
				if v, ok := m[k].(string); ok && v != "" {
					*s = flexString(v)
					return nil
				}
			}
		}
	default: // number / bool / array → stringify scalars, else empty
		*s = flexString(strings.Trim(string(b), `"`))
	}
	return nil
}

// flexBool decodes bool, "true"/"1"/"yes"/"on" strings, or non-zero numbers.
type flexBool bool

func (x *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.ToLower(strings.TrimSpace(string(b))), `"`)
	switch s {
	case "true", "1", "yes", "on":
		*x = true
	case "", "null", "false", "0", "no", "off":
		*x = false
	default:
		n, err := strconv.ParseFloat(s, 64)
		*x = flexBool(err == nil && n != 0)
	}
	return nil
}

// metadataDTO is a TOLERANT decode target for agent metadata. Agent telemetry
// comes from arbitrary real WordPress sites, so we deliberately do NOT use the
// strict OpenAPI-generated decoder (which requires SiteComponent.slug and is
// type-strict). All fields optional and shape-tolerant; unknown fields ignored;
// the service layer sanitizes (truncates/drops) before persisting.
type metadataDTO struct {
	WPVersion   flexString `json:"wp_version"`
	PHPVersion  flexString `json:"php_version"`
	ServerInfo  flexString `json:"server_info"`
	Multisite   flexBool   `json:"multisite"`
	ActiveTheme flexString `json:"active_theme"`
	// AgeRecipient is the agent's per-site age PUBLIC recipient ("age1…"). The
	// CP stores it on sites.age_recipient so M4 backups can be triggered without
	// a separate registration call. Optional; empty/missing leaves the stored
	// recipient unchanged.
	AgeRecipient flexString     `json:"age_recipient"`
	Plugins      []componentDTO `json:"plugins"`
	Themes       []componentDTO `json:"themes"`
	// CoreUpdate is set when WordPress core has an update available. New in the
	// Updates feature: OLD agents send no `core_update` and this stays nil — the
	// metadata sync still succeeds end-to-end.
	CoreUpdate *coreUpdateDTO `json:"core_update,omitempty"`

	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. All optional; old
	// agents send none of these and the sync still succeeds. We expose them on
	// the agent.Metadata struct so the service layer can surface them to UI
	// (Site Health card, host platform badge). No migration today: these are
	// additive on the wire and currently round-tripped via the Site domain's
	// existing JSONB extras column or left to a future migration.
	HostFlags  *hostFlagsDTO `json:"host_flags,omitempty"`
	Disk       *diskDTO      `json:"disk,omitempty"`
	UserCount  *int          `json:"user_count,omitempty"`
	AdminCount *int          `json:"admin_count,omitempty"`
}

// hostFlagsDTO mirrors the defined()-based hosting fingerprint the agent
// computes. Every field is a flexBool so "1"/"true"/true all decode.
type hostFlagsDTO struct {
	IsPressable flexBool `json:"is_pressable"`
	IsGridpane  flexBool `json:"is_gridpane"`
	IsWPEngine  flexBool `json:"is_wpengine"`
	IsAtomic    flexBool `json:"is_atomic"`
	IsKinsta    flexBool `json:"is_kinsta"`
	IsFlywheel  flexBool `json:"is_flywheel"`
	IsRunCloud  flexBool `json:"is_runcloud"`
	IsCloudways flexBool `json:"is_cloudways"`
}

// diskDTO is the sampled disk-usage snapshot the agent ships. Sizes are bytes;
// the wp-content and uploads measurements are capped to a 2-second walk so a
// huge uploads tree doesn't make the metadata push slow.
type diskDTO struct {
	WPContentBytes *int64 `json:"wp_content_bytes,omitempty"`
	UploadsBytes   *int64 `json:"uploads_bytes,omitempty"`
	FreeBytes      *int64 `json:"free_bytes,omitempty"`
}

// componentDTO is the per-plugin/theme tolerant decode target. AvailableUpdate
// is new in the Updates feature and is optional/nullable: OLD agents send no
// `available_update` and the metadata sync still succeeds.
type componentDTO struct {
	Slug            flexString          `json:"slug"`
	Name            flexString          `json:"name"`
	Version         flexString          `json:"version"`
	Active          flexBool            `json:"active"`
	AvailableUpdate *availableUpdateDTO `json:"available_update,omitempty"`
	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. URIs from the plugin
	// header (PluginURI/UpdateURI/AuthorURI) plus the Network flag. All
	// optional + tolerantly decoded. Old agents send none of these.
	PluginURI flexString `json:"plugin_uri,omitempty"`
	UpdateURI flexString `json:"update_uri,omitempty"`
	AuthorURI flexString `json:"author_uri,omitempty"`
	Network   flexBool   `json:"network,omitempty"`
}

// availableUpdateDTO is the per-item update advisory the agent reports. Only
// new_version is required by the contract; the other fields are best-effort
// surface from update_plugins/update_themes transients.
type availableUpdateDTO struct {
	NewVersion  flexString  `json:"new_version"`
	Package     *flexString `json:"package,omitempty"`
	Tested      *flexString `json:"tested,omitempty"`
	RequiresPHP *flexString `json:"requires_php,omitempty"`
}

// coreUpdateDTO is the WordPress core update advisory. Both versions are
// reported as strings; flex decoding tolerates the agent reporting them as
// numbers.
type coreUpdateDTO struct {
	NewVersion     flexString `json:"new_version"`
	CurrentVersion flexString `json:"current_version"`
}

func (d metadataDTO) toMetadata() Metadata {
	conv := func(cs []componentDTO) []Component {
		out := make([]Component, 0, len(cs))
		for _, c := range cs {
			comp := Component{
				Slug:      string(c.Slug),
				Name:      string(c.Name),
				Version:   string(c.Version),
				Active:    bool(c.Active),
				PluginURI: string(c.PluginURI),
				UpdateURI: string(c.UpdateURI),
				AuthorURI: string(c.AuthorURI),
				Network:   bool(c.Network),
			}
			if c.AvailableUpdate != nil && string(c.AvailableUpdate.NewVersion) != "" {
				comp.AvailableUpdate = &AvailableUpdate{
					NewVersion:  string(c.AvailableUpdate.NewVersion),
					Package:     flexStringPtr(c.AvailableUpdate.Package),
					Tested:      flexStringPtr(c.AvailableUpdate.Tested),
					RequiresPHP: flexStringPtr(c.AvailableUpdate.RequiresPHP),
				}
			}
			out = append(out, comp)
		}
		return out
	}
	m := Metadata{
		WPVersion:    string(d.WPVersion),
		PHPVersion:   string(d.PHPVersion),
		ServerInfo:   string(d.ServerInfo),
		Multisite:    bool(d.Multisite),
		ActiveTheme:  string(d.ActiveTheme),
		AgeRecipient: string(d.AgeRecipient),
		Plugins:      conv(d.Plugins),
		Themes:       conv(d.Themes),
	}
	if d.CoreUpdate != nil && string(d.CoreUpdate.NewVersion) != "" {
		m.CoreUpdate = &CoreUpdate{
			NewVersion:     string(d.CoreUpdate.NewVersion),
			CurrentVersion: string(d.CoreUpdate.CurrentVersion),
		}
	}
	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. All optional; old
	// agents send none of these.
	if d.HostFlags != nil {
		m.HostFlags = &HostFlags{
			IsPressable: bool(d.HostFlags.IsPressable),
			IsGridpane:  bool(d.HostFlags.IsGridpane),
			IsWPEngine:  bool(d.HostFlags.IsWPEngine),
			IsAtomic:    bool(d.HostFlags.IsAtomic),
			IsKinsta:    bool(d.HostFlags.IsKinsta),
			IsFlywheel:  bool(d.HostFlags.IsFlywheel),
			IsRunCloud:  bool(d.HostFlags.IsRunCloud),
			IsCloudways: bool(d.HostFlags.IsCloudways),
		}
	}
	if d.Disk != nil {
		m.Disk = &Disk{
			WPContentBytes: int64PtrOrZero(d.Disk.WPContentBytes),
			UploadsBytes:   int64PtrOrZero(d.Disk.UploadsBytes),
			FreeBytes:      int64PtrOrZero(d.Disk.FreeBytes),
		}
	}
	if d.UserCount != nil {
		m.UserCount = *d.UserCount
	}
	if d.AdminCount != nil {
		m.AdminCount = *d.AdminCount
	}
	return m
}

func int64PtrOrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// flexStringPtr nil-safely converts a *flexString to a plain string ("" when nil).
func flexStringPtr(p *flexString) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// Metadata mirrors the site domain's metadata input without importing it (the
// site package imports this package for the signature helpers, so this package
// must not import site — that would be a cycle).
type Metadata struct {
	WPVersion    string
	PHPVersion   string
	ServerInfo   string
	Multisite    bool
	ActiveTheme  string
	AgeRecipient string // optional; agent's per-site age PUBLIC recipient ("age1…")
	Plugins      []Component
	Themes       []Component
	// CoreUpdate is the optional WordPress core update advisory. nil when there
	// is no core update OR when the agent is old enough that it does not send
	// the field at all.
	CoreUpdate *CoreUpdate
	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. All optional and
	// best-effort: old agents send nothing here; the sink layer treats absence
	// as "no signal" (does not overwrite previously-stored values).
	HostFlags  *HostFlags
	Disk       *Disk
	UserCount  int
	AdminCount int
}

// Component is one installed plugin/theme. AvailableUpdate is set when the
// agent reports an update is available for this item.
type Component struct {
	Slug            string
	Name            string
	Version         string
	Active          bool
	AvailableUpdate *AvailableUpdate
	// ADR-037 Sprint 1, 1C — optional plugin-header URIs and Network flag.
	// Surfaced from get_plugins(); empty when the plugin header omits them.
	PluginURI string
	UpdateURI string
	AuthorURI string
	Network   bool
}

// HostFlags is the hosting-platform fingerprint. Mirrors agent's defined()-
// based probes. All false on a host the agent doesn't recognise.
type HostFlags struct {
	IsPressable bool
	IsGridpane  bool
	IsWPEngine  bool
	IsAtomic    bool
	IsKinsta    bool
	IsFlywheel  bool
	IsRunCloud  bool
	IsCloudways bool
}

// Disk is the sampled disk-usage snapshot the agent ships. Bytes; the
// wp_content/uploads measurements are capped to a 2-second walk so the
// metadata push stays cheap.
type Disk struct {
	WPContentBytes int64
	UploadsBytes   int64
	FreeBytes      int64
}

// AvailableUpdate is the per-item update advisory. Only NewVersion is required;
// the other fields are best-effort surface from the WP update transients.
type AvailableUpdate struct {
	NewVersion  string
	Package     string
	Tested      string
	RequiresPHP string
}

// CoreUpdate is the WordPress core update advisory.
type CoreUpdate struct {
	NewVersion     string
	CurrentVersion string
}

// MetadataSink applies agent-pushed metadata and heartbeats. Implemented by the
// site service (wired in main) so this package needs no site import. The
// metadata call returns the updated site in its OpenAPI form.
type MetadataSink interface {
	ApplyAgentMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata) (gen.Site, error)
	Heartbeat(ctx context.Context, tenantID, siteID uuid.UUID) error
}

// LifecycleSink handles the M21 connection-lifecycle agent calls: the 60s
// heartbeat (returns pending instructions, e.g. a queued revoke) and the signed
// last-will disconnect. Implemented by the site connection service (wired in
// main). Optional on the Handler: when nil, /heartbeat falls back to the legacy
// liveness-only Heartbeat and /disconnect returns 501.
type LifecycleSink interface {
	// RecordHeartbeat refreshes liveness, recovers degraded/disconnected→
	// connected, and returns any pending agent instructions plus, for a "revoke"
	// instruction, a signed token (aud=site_id, cmd="revoke") the agent must
	// verify before acting (Phase 6 finding B).
	RecordHeartbeat(ctx context.Context, tenantID, siteID uuid.UUID, payload map[string]any) (instructions []string, revokeToken string, err error)
	// RecordLastWill transitions connected/degraded→disconnected on a signed
	// agent disconnect (ADR-040).
	RecordLastWill(ctx context.Context, tenantID, siteID uuid.UUID, reason string) error
}

// Handler serves the agent-authenticated endpoints under /agent/v1. Every route
// runs behind the agent Authenticator; the site/tenant come from the verified
// identity on the context.
type Handler struct {
	sink      MetadataSink
	lifecycle LifecycleSink
}

// NewHandler builds an agent Handler.
func NewHandler(sink MetadataSink) *Handler {
	return &Handler{sink: sink}
}

// SetLifecycleSink wires the M21 connection-lifecycle sink (heartbeat
// instructions + signed disconnect). Call once at boot; nil disables the
// lifecycle behaviour (legacy liveness-only heartbeat; /disconnect → 501).
func (h *Handler) SetLifecycleSink(l LifecycleSink) { h.lifecycle = l }

// Register mounts the agent routes on the given group (already wrapped with the
// agent Authenticator middleware).
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/metadata", h.metadata)
	r.POST("/heartbeat", h.heartbeat)
	// M21 signed last-will (ADR-040). Same Ed25519 signed-request middleware as
	// every other agent route — the signature is verified and bound to the site
	// BEFORE this handler runs, so possession of a site_id alone cannot
	// disconnect a site.
	r.POST("/disconnect", h.disconnect)
}

func (h *Handler) metadata(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, rerr := io.ReadAll(io.LimitReader(c.Request.Body, maxMetadataBytes))
	if rerr != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	// Tolerant decode (see metadataDTO). Only genuinely malformed JSON is rejected.
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		slog.WarnContext(c.Request.Context(), "agent metadata: malformed JSON body",
			slog.String("site_id", id.SiteID.String()),
			slog.Int("bytes", len(body)),
			slog.String("error", err.Error()))
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	m := dto.toMetadata()
	slog.InfoContext(c.Request.Context(), "agent metadata received",
		slog.String("site_id", id.SiteID.String()),
		slog.Int("plugins", len(m.Plugins)), slog.Int("themes", len(m.Themes)),
		slog.String("active_theme", m.ActiveTheme))
	out, err := h.sink.ApplyAgentMetadata(c.Request.Context(), id.TenantID, id.SiteID, m)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, &out)
}

// heartbeat records the 60s agent beat (ADR-039). With the M21 lifecycle sink
// wired it refreshes liveness, recovers a degraded/disconnected site to
// connected, and returns {ok, instructions?} (e.g. ["revoke"] for a revoked
// site). Without the lifecycle sink it falls back to the legacy liveness-only
// Heartbeat (204). The light metadata body is accepted and decoded best-effort;
// it is currently not persisted (the M21 migration added no column for it) —
// see the note in the brief; the heartbeat never fails over the payload.
func (h *Handler) heartbeat(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}

	if h.lifecycle == nil {
		// Legacy path: liveness only.
		if err := h.sink.Heartbeat(c.Request.Context(), id.TenantID, id.SiteID); err != nil {
			httpx.Error(c, err)
			return
		}
		c.Status(http.StatusNoContent)
		return
	}

	// Best-effort decode of the light heartbeat metadata. A malformed/empty body
	// is fine — the beat is about liveness, not the payload.
	var payload map[string]any
	if c.Request.ContentLength != 0 {
		body, _ := io.ReadAll(io.LimitReader(c.Request.Body, maxMetadataBytes))
		if len(body) > 0 {
			_ = json.Unmarshal(body, &payload)
		}
	}

	instructions, revokeToken, err := h.lifecycle.RecordHeartbeat(c.Request.Context(), id.TenantID, id.SiteID, payload)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	resp := gin.H{"ok": true}
	if len(instructions) > 0 {
		resp["instructions"] = instructions
	}
	if revokeToken != "" {
		// Signed proof for the revoke instruction — the agent verifies this
		// (CP pubkey + aud=site_id + cmd=revoke + exp) before self-teardown.
		resp["revoke_token"] = revokeToken
	}
	c.JSON(http.StatusOK, resp)
}

// disconnectRequest is the signed last-will body.
type disconnectRequest struct {
	Reason string `json:"reason"`
}

// disconnect handles a signed agent last-will (ADR-040): connected/degraded→
// disconnected with the supplied reason. The agent's signature was already
// verified and bound to id.SiteID by the agent Authenticator middleware, so the
// disconnect is provably from the holder of that site's private key.
func (h *Handler) disconnect(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	if h.lifecycle == nil {
		httpx.Error(c, domain.Unavailable("lifecycle_disabled", "connection lifecycle is not enabled on this control plane"))
		return
	}
	var req disconnectRequest
	if c.Request.ContentLength != 0 {
		body, _ := io.ReadAll(io.LimitReader(c.Request.Body, maxMetadataBytes))
		if len(body) > 0 {
			_ = json.Unmarshal(body, &req)
		}
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "user_initiated"
	}
	if len(reason) > 64 {
		reason = reason[:64]
	}
	if err := h.lifecycle.RecordLastWill(c.Request.Context(), id.TenantID, id.SiteID, reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
