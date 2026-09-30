package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// ProbeClient is the slice of agentcmd.Client the refresh uses.
type ProbeClient interface {
	ContentProbeList(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.ContentProbeRequest) (agentcmd.ContentProbeListResponse, error)
}

const (
	// pageSize is the list-mode page: the agent's maximum.
	pageSize = agentcmd.ContentProbeMaxListLimit
	// maxPages bounds one refresh. 25 pages of 200 is 5000 pages of a site; a
	// site with more is recorded up to that and the rest are not reported.
	maxPages = 25
	// callTimeout bounds one agent call.
	callTimeout = 25 * time.Second
	// RefreshTimeout bounds a whole refresh (the River job timeout).
	RefreshTimeout = maxPages*callTimeout + time.Minute
)

var (
	agentVersionShape = regexp.MustCompile(`^\d+(\.\d+){0,3}$`)
	slugRe            = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// AgentMeetsFloor reports whether a site's reported agent version is at or
// above MinAgentVersionForContentProbe. An empty or unparseable version is
// below it.
func AgentMeetsFloor(v string) bool {
	v = strings.TrimSpace(v)
	if !agentVersionShape.MatchString(v) {
		return false
	}
	return wpversion.Compare(v, MinAgentVersionForContentProbe) >= 0
}

// ErrNotSendable marks a refresh that was declined without contacting the
// site (agent below the floor, site not connected, paused for a scheduled
// run). It is an outcome, not a failure: the job does not retry it.
var ErrNotSendable = errors.New("content refresh not sent")

// RefreshResult summarises one refresh.
type RefreshResult struct {
	Stored         int
	SkippedUnknown int
	Truncated      bool
	CheckedAt      time.Time
}

// Service holds the inventory business logic.
type Service struct {
	repo   Repo
	agent  ProbeClient
	logger *slog.Logger
	now    func() time.Time
}

// NewService builds the service. agent may be nil in a build without the agent
// command client; a refresh then reports the feature as unavailable.
func NewService(repo Repo, agent ProbeClient, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, agent: agent, logger: logger, now: time.Now}
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Refresh replaces a site's inventory with a fresh probe. scheduled runs are
// skipped for a paused site; an operator's Refresh never is.
func (s *Service) Refresh(ctx context.Context, tenantID, siteID uuid.UUID, scheduled bool) (RefreshResult, error) {
	if s.agent == nil {
		return RefreshResult{}, domain.ServiceUnavailable("content_probe_unavailable", "page checks are not available on this install")
	}
	target, err := s.repo.GetSiteTarget(ctx, tenantID, siteID)
	if err != nil {
		return RefreshResult{}, err
	}
	if !target.Enrolled || (target.ConnectionState != "connected" && target.ConnectionState != "degraded") {
		return RefreshResult{}, fmt.Errorf("%w: site is not connected", ErrNotSendable)
	}
	if scheduled && target.Paused {
		return RefreshResult{}, fmt.Errorf("%w: monitoring is paused", ErrNotSendable)
	}
	if !AgentMeetsFloor(target.AgentVersion) {
		return RefreshResult{}, fmt.Errorf("%w: agent below %s", ErrNotSendable, MinAgentVersionForContentProbe)
	}

	integrations, err := s.repo.ListEnabledIntegrations(ctx, tenantID)
	if err != nil {
		return RefreshResult{}, err
	}
	names := make(map[string]string, len(integrations))
	for _, it := range integrations {
		names[it.ID] = it.DisplayName
	}
	descriptors := BuildDescriptors(integrations)
	indicators := BuildIndicators(target.Components, integrations)
	types := []string{"page", "post"}

	var rows []Row
	var skipped int
	truncated := false
	offset := 0
	for page := 0; ; page++ {
		if page == maxPages {
			truncated = true
			break
		}
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		resp, err := s.agent.ContentProbeList(callCtx, siteID, target.URL, agentcmd.ContentProbeRequest{
			List: &agentcmd.ContentProbeList{
				Types: types, Status: []string{"publish"}, Limit: pageSize, Offset: offset,
			},
			AllowedPostTypes: types,
			Descriptors:      descriptors,
			Indicators:       indicators,
		})
		cancel()
		if err != nil {
			return RefreshResult{}, err
		}
		got, unknown, verr := ValidateListResponse(resp, pageSize, types, names)
		if verr != nil {
			return RefreshResult{}, verr
		}
		rows = append(rows, got...)
		skipped += unknown
		if resp.NextOffset == nil || *resp.NextOffset <= offset {
			break
		}
		offset = *resp.NextOffset
	}

	// A post can appear on two pages when the site changes between calls; the
	// upsert would reject the whole statement on a repeated key, so the last
	// row seen wins.
	rows = dedupeRows(rows)
	checkedAt := s.now().UTC()
	if err := s.repo.ReplaceInventory(ctx, tenantID, siteID, checkedAt, rows, truncated); err != nil {
		return RefreshResult{}, err
	}
	if skipped > 0 {
		s.logger.Warn("content refresh: rows skipped for an unknown verdict or reason",
			slog.String("site_id", siteID.String()), slog.Int("skipped", skipped))
	}
	return RefreshResult{Stored: len(rows), SkippedUnknown: skipped, Truncated: truncated, CheckedAt: checkedAt}, nil
}

// BuildIndicators derives the site-level builder hints. It sends ONLY hints
// for builders on the platform allowlist: a plugin directory named by an
// allowlist row's descriptor.plugin_dir, and the active theme only when it
// matches an allowlist row's theme_slug. The agent treats every hint it finds
// as a possible builder, so an ordinary plugin or theme sent here would strip
// the classic verdict from every page on the site.
func BuildIndicators(components []byte, integrations []Integration) agentcmd.ContentProbeIndicators {
	var comp struct {
		Plugins []struct {
			Slug string `json:"slug"`
		} `json:"plugins"`
		Themes []struct {
			Slug   string `json:"slug"`
			Active bool   `json:"active"`
		} `json:"themes"`
	}
	_ = json.Unmarshal(components, &comp) // an unreadable inventory yields no hints

	installed := make(map[string]bool)
	for _, p := range comp.Plugins {
		slug := p.Slug
		if i := strings.Index(slug, "/"); i >= 0 {
			slug = slug[:i]
		}
		installed[strings.TrimSuffix(slug, ".php")] = true
	}
	activeThemes := make(map[string]bool)
	for _, t := range comp.Themes {
		if t.Active {
			activeThemes[t.Slug] = true
		}
	}

	plugins := make(map[string]bool)
	themes := make(map[string]bool)
	for _, it := range integrations {
		var d struct {
			PluginDir string `json:"plugin_dir"`
		}
		if json.Unmarshal(it.Descriptor, &d) != nil {
			continue
		}
		// The agent reports a hint only for an active entry, so offering an
		// allowlisted directory the site does not have costs a slot and no more.
		if slugRe.MatchString(d.PluginDir) && installed[d.PluginDir] {
			plugins[d.PluginDir] = true
		}
		if slugRe.MatchString(it.ThemeSlug) && activeThemes[it.ThemeSlug] {
			themes[it.ThemeSlug] = true
		}
	}
	return agentcmd.ContentProbeIndicators{
		PluginSlugs:     capOrdered(plugins, agentcmd.ContentProbeMaxIndicators),
		ThemeSlugs:      capOrdered(themes, agentcmd.ContentProbeMaxIndicators),
		MetaKeyPrefixes: []string{},
	}
}

func capOrdered(set map[string]bool, max int) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func dedupeRows(rows []Row) []Row {
	idx := make(map[int64]int, len(rows))
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if i, ok := idx[r.PostID]; ok {
			out[i] = r
			continue
		}
		idx[r.PostID] = len(out)
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// MaxInventoryPage is the largest page the inventory endpoint returns.
const MaxInventoryPage = 200

// InventoryPage is one page of a site's inventory plus the site's state.
type InventoryPage struct {
	State          string
	AgentVersion   string
	MinAgent       string
	Rows           []InventoryRow
	NextAfterPost  *int64
	LastCheckedAt  *time.Time
	TitlesIncluded bool
	Truncated      bool
}

// Inventory returns one page of a site's inventory. withTitles says whether
// the caller holds the content-read permission; without it titles are not
// returned at all.
func (s *Service) Inventory(ctx context.Context, p domain.Principal, siteID uuid.UUID, after int64, owner string, limit int, withTitles bool) (InventoryPage, error) {
	if limit < 1 || limit > MaxInventoryPage {
		limit = 50
	}
	if after < 0 {
		after = 0
	}
	var ownerFilter *string
	if owner != "" {
		if owner != "classic" && !integrationIDRe.MatchString(owner) {
			return InventoryPage{}, domain.Validation("invalid_editor", "editor filter is not a valid value")
		}
		ownerFilter = &owner
	}
	target, err := s.repo.GetSiteTarget(ctx, p.TenantID, siteID)
	if err != nil {
		return InventoryPage{}, err
	}
	page := InventoryPage{MinAgent: MinAgentVersionForContentProbe, AgentVersion: humantext.CapBytes(humantext.Clean(target.AgentVersion), 32), TitlesIncluded: withTitles}
	switch {
	case !AgentMeetsFloor(target.AgentVersion):
		page.State = StateAgentUpdateNeeded
	case target.ConnectionState != "connected" && target.ConnectionState != "degraded":
		page.State = StateNotConnected
	default:
		page.State = StateOK
	}

	rows, err := s.repo.ListInventory(ctx, p, siteID, after, ownerFilter, int32(limit+1))
	if err != nil {
		return InventoryPage{}, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1].PostID
		page.NextAfterPost = &last
	}
	for i := range rows {
		if !withTitles {
			rows[i].Title = nil
		}
	}
	page.Rows = rows
	run, err := s.repo.GetRun(ctx, p, siteID)
	if err != nil {
		return InventoryPage{}, err
	}
	if run != nil {
		t := run.CheckedAt
		page.LastCheckedAt = &t
		page.Truncated = run.Truncated
	}
	return page, nil
}

// FleetReport is the owner report.
type FleetReport struct {
	ByVerdict []FleetVerdictShare
	ByBuilder []FleetBuilderShare
	Pages     int64
}

// Fleet aggregates every tenant's inventory. The caller must already be a
// superadmin; nothing here checks that.
func (s *Service) Fleet(ctx context.Context, actor uuid.UUID) (FleetReport, error) {
	vs, bs, err := s.repo.FleetReport(ctx, actor)
	if err != nil {
		return FleetReport{}, err
	}
	rep := FleetReport{ByVerdict: vs, ByBuilder: bs}
	for _, v := range vs {
		rep.Pages += v.Pages
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Admin: the allowlist
// ---------------------------------------------------------------------------

// EntrySHA256 is sha256 over the canonical JSON of a whole allowlist entry:
// keys sorted at every level, no insignificant whitespace, a fixed field set
// with nulls kept, so the same entry always hashes the same.
func EntrySHA256(in AdminUpsertInput) (string, error) {
	var desc, abil any
	if err := json.Unmarshal(in.Descriptor, &desc); err != nil {
		return "", fmt.Errorf("descriptor: %w", err)
	}
	if len(in.Abilities) > 0 {
		if err := json.Unmarshal(in.Abilities, &abil); err != nil {
			return "", fmt.Errorf("abilities: %w", err)
		}
	}
	entry := map[string]any{
		"integration_id":     in.IntegrationID,
		"display_name":       in.DisplayName,
		"enabled":            in.Enabled,
		"status":             in.Status,
		"descriptor":         desc,
		"abilities":          abil,
		"min_version":        in.MinVersion,
		"max_tested_version": in.MaxTestedVersion,
		"min_wp_version":     in.MinWPVersion,
		"theme_slug":         in.ThemeSlug,
	}
	// encoding/json sorts map keys at every level and emits no whitespace.
	b, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// UpsertIntegration validates and writes one allowlist row through the
// superadmin-only SQL writer. The caller must already have passed
// requireSuperadmin; the SQL function checks the actor again.
func (s *Service) UpsertIntegration(ctx context.Context, in AdminUpsertInput) (IntegrationRecord, error) {
	if !integrationIDRe.MatchString(in.IntegrationID) || len(in.IntegrationID) > 64 {
		return IntegrationRecord{}, domain.Validation("invalid_integration_id", "integration_id must be lower-case words joined by hyphens, at most 64 characters")
	}
	name := strings.TrimSpace(humantext.Clean(in.DisplayName))
	if name == "" || len([]rune(name)) > 80 {
		return IntegrationRecord{}, domain.Validation("invalid_display_name", "display_name must be 1 to 80 characters")
	}
	in.DisplayName = name
	if in.Status != "detect_only" {
		return IntegrationRecord{}, domain.Validation("invalid_status", "status must be detect_only")
	}
	if len(in.Descriptor) == 0 {
		in.Descriptor = []byte(`{}`)
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal(in.Descriptor, &d); err != nil || d == nil {
		return IntegrationRecord{}, domain.Validation("invalid_descriptor", "descriptor must be a JSON object")
	}
	for k := range d {
		if _, ok := descriptorKeys[k]; !ok {
			return IntegrationRecord{}, domain.Validation("invalid_descriptor", "descriptor has a field the agent does not accept")
		}
	}
	if len(in.Descriptor) > 16<<10 {
		return IntegrationRecord{}, domain.Validation("invalid_descriptor", "descriptor is too large")
	}
	if in.ThemeSlug != nil && *in.ThemeSlug != "" && !slugRe.MatchString(*in.ThemeSlug) {
		return IntegrationRecord{}, domain.Validation("invalid_theme_slug", "theme_slug is not a valid theme directory name")
	}
	if in.ThemeSlug != nil && *in.ThemeSlug == "" {
		in.ThemeSlug = nil
	}
	sum, err := EntrySHA256(in)
	if err != nil {
		return IntegrationRecord{}, domain.Validation("invalid_entry", "entry is not valid JSON")
	}
	in.IntegrationEntrySHA256 = sum
	rec, err := s.repo.AdminUpsertIntegration(ctx, in)
	if err != nil {
		return IntegrationRecord{}, mapAdminErr(err)
	}
	return rec, nil
}

// ListIntegrations reads the whole allowlist for the admin screen.
func (s *Service) ListIntegrations(ctx context.Context, actor uuid.UUID) ([]IntegrationRecord, error) {
	return s.repo.ListIntegrations(ctx, actor)
}
