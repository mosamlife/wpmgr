package security

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// AgentSecurityClient is the subset of agentcmd.Client the service needs to
// push security config and issue IP unblocks. *agentcmd.Client satisfies it
// via its SyncSecurityConfig and UnblockIP methods. Declared as an interface
// so tests can substitute a fake without spinning up the SSRF transport.
type AgentSecurityClient interface {
	SyncSecurityConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.SecurityConfigRequest) (agentcmd.SecurityConfigResult, error)
	UnblockIP(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.UnblockIPRequest) (agentcmd.UnblockIPResult, error)
}

// SiteLookup resolves a site's agent URL from the site service (wired in main
// via a narrow adapter, keeping this package free of the site import).
type SiteLookup interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
}

// Service orchestrates the security domain: repo + optional agentcmd client.
type Service struct {
	repo        *Repo
	agentClient AgentSecurityClient
	siteLookup  SiteLookup
}

// NewService builds a Service.
func NewService(repo *Repo) *Service {
	return &Service{repo: repo}
}

// SetAgentClient wires the agentcmd client for pushing config and unblocking
// IPs. The SiteLookup is required alongside it so the service can resolve the
// site URL without a hard dependency on the site package.
func (s *Service) SetAgentClient(client AgentSecurityClient, sites SiteLookup) {
	s.agentClient = client
	s.siteLookup = sites
}

// validModes are the three allowed protection modes.
var validModes = map[string]bool{
	"disabled": true,
	"audit":    true,
	"protect":  true,
}

// GetConfig returns the stored config, or the default when no row exists yet.
func (s *Service) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (SecurityConfig, error) {
	cfg, found, err := s.repo.GetConfig(ctx, tenantID, siteID)
	if err != nil {
		return SecurityConfig{}, err
	}
	if !found {
		return defaultConfig(tenantID, siteID), nil
	}
	return cfg, nil
}

// SaveConfig validates the incoming config, applies the safety rail for
// protect+empty-allowlist, upserts it in the database, and pushes it to the
// agent. Returns the stored (potentially modified) config.
//
// Safety rail: when mode=="protect" and allow_cidrs is empty after the
// operator's edit, we auto-add the requesting operator's client IP as a /32
// (IPv4) or /128 (IPv6) so enabling protection cannot lock the operator out.
// operatorIP is derived by the handler from X-Forwarded-For / RemoteAddr.
func (s *Service) SaveConfig(ctx context.Context, tenantID, siteID uuid.UUID, cfg SecurityConfig, operatorIP string) (SecurityConfig, error) {
	// --- mode validation ---
	if !validModes[cfg.Mode] {
		return SecurityConfig{}, domain.Validation("invalid_mode",
			fmt.Sprintf("mode must be one of: disabled, audit, protect; got %q", cfg.Mode))
	}

	// --- CIDR validation ---
	cfg.AllowCIDRs = normalizeCIDRs(cfg.AllowCIDRs)
	cfg.DenyCIDRs = normalizeCIDRs(cfg.DenyCIDRs)
	for _, cidr := range append(cfg.AllowCIDRs, cfg.DenyCIDRs...) {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return SecurityConfig{}, domain.Validation("invalid_cidr",
				fmt.Sprintf("CIDR %q is not a valid IPv4 or IPv6 network", cidr))
		}
	}

	// --- threshold clamping (sane positive ranges) ---
	cfg.Thresholds = clampThresholds(cfg.Thresholds)

	// --- IP header default ---
	if cfg.IPHeader == "" {
		cfg.IPHeader = "REMOTE_ADDR"
	}

	// --- safety rail: protect + empty allowlist ---
	// When the operator enables protect mode with no allowlist, auto-add their
	// own IP as a /32 or /128 so they cannot lock themselves out immediately.
	if cfg.Mode == "protect" && len(cfg.AllowCIDRs) == 0 && operatorIP != "" {
		cidr := ipToCIDR(operatorIP)
		if cidr != "" {
			cfg.AllowCIDRs = []string{cidr}
		}
	}

	cfg.TenantID = tenantID
	cfg.SiteID = siteID

	// --- persist ---
	saved, err := s.repo.UpsertConfig(ctx, cfg)
	if err != nil {
		return SecurityConfig{}, err
	}

	// --- push to agent (best-effort) ---
	if s.agentClient != nil && s.siteLookup != nil {
		siteURL, lookupErr := s.siteLookup.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr == nil {
			req := agentcmd.SecurityConfigRequest{
				Mode:       saved.Mode,
				Thresholds: saved.Thresholds,
				IPHeader:   saved.IPHeader,
				AllowCIDRs: coalesceSlice(saved.AllowCIDRs),
				DenyCIDRs:  coalesceSlice(saved.DenyCIDRs),
			}
			if _, pushErr := s.agentClient.SyncSecurityConfig(ctx, siteID, siteURL, req); pushErr != nil {
				// Non-fatal: config is already persisted. Return stored config +
				// wrapped push error so the handler can surface it as a warning.
				return saved, fmt.Errorf("config stored but agent push failed: %w", pushErr)
			}
		}
		// site URL lookup failure is also non-fatal — config is still stored.
	}

	return saved, nil
}

// UnblockIP validates the IP string and sends the unblock_ip command to the
// agent. Returns the agent's ok+detail.
func (s *Service) UnblockIP(ctx context.Context, tenantID, siteID uuid.UUID, ip string) (bool, string, error) {
	if net.ParseIP(ip) == nil {
		return false, "", domain.Validation("invalid_ip", fmt.Sprintf("%q is not a valid IP address", ip))
	}
	if s.agentClient == nil || s.siteLookup == nil {
		return false, "", domain.ServiceUnavailable("security_agent_unwired", "security agent client is not wired")
	}
	siteURL, err := s.siteLookup.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return false, "", err
	}
	res, err := s.agentClient.UnblockIP(ctx, siteID, siteURL, agentcmd.UnblockIPRequest{IP: ip})
	if err != nil {
		// The client already wraps ok=false as an error; surface it as-is.
		return false, err.Error(), err
	}
	return res.OK, res.Detail, nil
}

// IngestLoginEvents dedup-upserts the agent-shipped batch and returns the
// highest agent_event_id so the caller (agent ingest handler) can echo the
// cursor advance back.
func (s *Service) IngestLoginEvents(ctx context.Context, tenantID, siteID uuid.UUID, batch LoginEventBatch) (int64, error) {
	events := make([]LoginEvent, 0, len(batch.LoginEvents))
	for _, e := range batch.LoginEvents {
		ev := LoginEvent{
			AgentEventID: int64(e.ID),
			IP:           e.IP,
			Status:       LoginEventStatus(int16(e.Status)),
			Category:     e.Category,
			Username:     e.Username,
			RequestID:    e.RequestID,
		}
		if ts := int64(e.OccurredAt); ts > 0 {
			ev.OccurredAt = time.Unix(ts, 0).UTC()
		}
		events = append(events, ev)
	}
	return s.repo.InsertLoginEventsBatch(ctx, tenantID, siteID, events)
}

// ListLoginEvents lists login events for a site.
func (s *Service) ListLoginEvents(ctx context.Context, tenantID, siteID uuid.UUID, limit int, statusFilter *LoginEventStatus) ([]LoginEvent, error) {
	return s.repo.ListLoginEvents(ctx, tenantID, siteID, limit, statusFilter)
}

// ---------------------------------------------------------------------------
// wire-format batch types (flexInt tolerance for wpdb ARRAY_A numeric strings)
// ---------------------------------------------------------------------------

// LoginEventEntry is one entry in the agent-pushed batch. All numeric fields
// use flexInt64 / flexInt16 so they tolerate wpdb ARRAY_A's quoted-number
// encoding ("5" instead of 5).
type LoginEventEntry struct {
	ID         flexInt64 `json:"id"`
	IP         string    `json:"ip"`
	Status     flexInt16 `json:"status"`
	Category   string    `json:"category"`
	Username   string    `json:"username"`
	RequestID  string    `json:"request_id"`
	OccurredAt flexInt64 `json:"occurred_at"`
}

// LoginEventBatch is the top-level body the agent POSTs to /agent/v1/security/login-events.
type LoginEventBatch struct {
	LoginEvents []LoginEventEntry `json:"login_events"`
}

// flexInt64 unmarshals a JSON value that may arrive as a number or a quoted
// numeric string (wpdb ARRAY_A always encodes numeric columns as strings).
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] != '"' {
		var n int64
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = flexInt64(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(n)
	return nil
}

// flexInt16 is like flexInt64 but for smallint-sized fields (status).
type flexInt16 int16

func (f *flexInt16) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] != '"' {
		var n int16
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = flexInt16(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 16)
	if err != nil {
		return err
	}
	*f = flexInt16(n)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func defaultConfig(tenantID, siteID uuid.UUID) SecurityConfig {
	return SecurityConfig{
		TenantID:   tenantID,
		SiteID:     siteID,
		Mode:       "protect",
		Thresholds: agentcmd.DefaultSecurityThresholds,
		IPHeader:   "REMOTE_ADDR",
		AllowCIDRs: []string{},
		DenyCIDRs:  []string{},
	}
}

// clampThresholds enforces sane positive ranges; zero values fall back to the
// compiled-in defaults.
func clampThresholds(t agentcmd.SecurityThresholds) agentcmd.SecurityThresholds {
	def := agentcmd.DefaultSecurityThresholds
	clamp := func(v, lo, hi int) int {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	const maxCount = 10000
	const maxSecs = 365 * 24 * 3600
	if t.CaptchaLimit <= 0 {
		t.CaptchaLimit = def.CaptchaLimit
	}
	if t.TempBlockLimit <= 0 {
		t.TempBlockLimit = def.TempBlockLimit
	}
	if t.BlockAllLimit <= 0 {
		t.BlockAllLimit = def.BlockAllLimit
	}
	if t.FailedLoginGap <= 0 {
		t.FailedLoginGap = def.FailedLoginGap
	}
	if t.SuccessLoginGap <= 0 {
		t.SuccessLoginGap = def.SuccessLoginGap
	}
	if t.AllBlockedGap <= 0 {
		t.AllBlockedGap = def.AllBlockedGap
	}
	t.CaptchaLimit = clamp(t.CaptchaLimit, 1, maxCount)
	t.TempBlockLimit = clamp(t.TempBlockLimit, 1, maxCount)
	t.BlockAllLimit = clamp(t.BlockAllLimit, 1, maxCount)
	t.FailedLoginGap = clamp(t.FailedLoginGap, 1, maxSecs)
	t.SuccessLoginGap = clamp(t.SuccessLoginGap, 1, maxSecs)
	t.AllBlockedGap = clamp(t.AllBlockedGap, 1, maxSecs)
	return t
}

// normalizeCIDRs trims whitespace and filters out empty strings.
func normalizeCIDRs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ipToCIDR converts a raw IP string (possibly host:port) to the minimal
// host-CIDR (/32 for IPv4, /128 for IPv6). Returns "" on parse failure.
func ipToCIDR(rawIP string) string {
	host, _, err := net.SplitHostPort(rawIP)
	if err != nil {
		host = rawIP
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return ip.String() + "/32"
	}
	return ip.String() + "/128"
}

// coalesceSlice returns an empty (non-nil) slice when the input is nil.
func coalesceSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
