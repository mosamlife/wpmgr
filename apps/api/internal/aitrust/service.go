package aitrust

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// Service reads and writes the AI trust settings.
type Service struct {
	repo      *Repo
	setters   SetterResolver
	abilities AbilityRequestRenderer
	purges    CachePurgeRenderer
	log       *slog.Logger
}

// NewService builds the service. setters is the session authenticator's
// setter builder; without it no setting reads as valid.
func NewService(repo *Repo, setters SetterResolver, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, setters: setters, log: log}
}

// SetRenderers wires how the activity feed renders each kind of request: the
// request queues' own renderers, so a card shows the same object either way.
func (s *Service) SetRenderers(a AbilityRequestRenderer, c CachePurgeRenderer) {
	s.abilities, s.purges = a, c
}

// MinAgentVersion is the least WPMgr plugin version a site needs for Auto
// for AI drafts: the version that runs the AI's draft tools.
const MinAgentVersion = agentcmd.MinAgentVersionForPageCreate

// offeredModes is the modes the dashboard offers in this build, in display
// order.
var offeredModes = []aipolicy.Mode{aipolicy.ModeAsk, aipolicy.ModeAIDrafts}

// ---------------------------------------------------------------------------
// The site's mode
// ---------------------------------------------------------------------------

// GetMode reads a site's mode as the caller may see it.
func (s *Service) GetMode(ctx context.Context, p domain.Principal, siteID uuid.UUID) (SiteMode, error) {
	f, err := s.repo.ReadMode(ctx, p, siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SiteMode{}, domain.NotFound("site_not_found", "site not found")
	}
	if err != nil {
		return SiteMode{}, domain.Internal("ai_mode_read_failed", "failed to read the site's AI setting").WithCause(err)
	}
	return s.siteMode(ctx, p, siteID, f), nil
}

func (s *Service) siteMode(ctx context.Context, p domain.Principal, siteID uuid.UUID, f modeFacts) SiteMode {
	m := SiteMode{
		SiteID:              siteID,
		Mode:                aipolicy.Mode(f.row.AiMode),
		Source:              f.row.AiModeSource,
		Version:             f.row.AiModeVersion,
		SetByUserID:         uuidPtr(f.row.AiModeSetBy),
		SetByAccountDeleted: f.row.SetByAccountDeleted,
		SetAt:               tsPtr(f.row.AiModeSetAt),
		AIPaused:            f.paused,
		MinAgentVersion:     MinAgentVersion,
	}
	if !f.row.SetByAccountDeleted {
		m.SetByName = f.row.SetByName
	}
	m.SetterValid = s.siteSetterValid(ctx, p.TenantID, siteID, m.Mode, m.SetByUserID)
	m.Options = modeOptions(p, siteID, f)
	m.Kinds = changeKinds(f.entries, f.routes)
	return m
}

// siteSetterValid is the decision engine's check on the site's setter, as it
// would run for a change the mode's own authority covers: the person must
// still be a member with an active account who may choose the mode and
// reach the site. Ask has nothing to honour.
func (s *Service) siteSetterValid(ctx context.Context, tenantID, siteID uuid.UUID, mode aipolicy.Mode, setBy *uuid.UUID) bool {
	if mode == aipolicy.ModeAsk {
		return true
	}
	if setBy == nil || s.setters == nil {
		return false
	}
	setter := s.setters.ResolveSetter(ctx, tenantID, *setBy)
	return aipolicy.CheckSiteSetter(setter, tenantID, siteID, mode, string(authz.PermSiteContentEdit)).OK
}

// modeOptions says, for each offered mode, whether this caller may choose it
// now and, when not, the first reason in the contract's order: role, scope,
// session, pause, plugin version.
func modeOptions(p domain.Principal, siteID uuid.UUID, f modeFacts) []ModeOption {
	canEdit := authz.PrincipalAllows(p, authz.PermSiteContentEdit) && p.CanAccessSite(siteID)
	person := authz.AuthorizeLoosening(p) == nil
	out := make([]ModeOption, 0, len(offeredModes))
	for _, mode := range offeredModes {
		o := ModeOption{Mode: mode}
		switch {
		case !canEdit:
			o.Reason = CodeRoleRequired
		case mode == aipolicy.ModeAsk:
			// Lowering is open to every caller who may edit the site's content.
		case !person:
			o.Reason = CodeSessionRequired
		case f.paused:
			o.Reason = CodePaused
		case !agentMeetsFloor(f.agentVersion, MinAgentVersion):
			o.Reason = CodeAgentOutdated
		}
		o.Choosable = o.Reason == ""
		out = append(out, o)
	}
	return out
}

// changeKinds is the table of what each offered mode covers: one row per
// kind of change a reviewed write tool makes, in display order. A tool whose
// kind comes from its target's status is listed under every kind that
// status can give it. always_ask is never listed: it waits in every mode.
func changeKinds(entries []sqlc.AbilityCatalogue, routes []sqlc.RestRouteCatalogue) []ChangeKind {
	byClass := map[aipolicy.Class][]KindAbility{}
	add := func(stored aipolicy.Class, a KindAbility) {
		for _, c := range effectiveClassesOf(stored) {
			byClass[c] = append(byClass[c], a)
		}
	}
	for _, e := range entries {
		if e.Class == "write" {
			add(aipolicy.Class(e.ChangeClass), KindAbility{Name: e.Name, Title: e.Title})
		}
	}
	for _, r := range routes {
		if r.Class == "write" {
			add(aipolicy.Class(r.ChangeClass), KindAbility{Name: r.RouteID, Title: r.Title})
		}
	}
	out := []ChangeKind{}
	for _, c := range aipolicy.Classes() {
		abilities := byClass[c]
		if c == aipolicy.ClassAlwaysAsk || len(abilities) == 0 {
			continue
		}
		k := ChangeKind{Class: c, Name: c.KindName(), Abilities: abilities}
		for _, mode := range offeredModes {
			outcome := "ask"
			if aipolicy.Allows(mode, c) {
				outcome = "auto"
			}
			k.Decisions = append(k.Decisions, ModeDecision{Mode: mode, Outcome: outcome})
		}
		out = append(out, k)
	}
	return out
}

// effectiveClassesOf is the effective classes a stored class can give a
// change, derived from aipolicy.Classify so the table cannot drift from the
// engine. An unknown stored value gives always_ask.
func effectiveClassesOf(stored aipolicy.Class) []aipolicy.Class {
	if stored != aipolicy.StoredByTargetStatus {
		c := aipolicy.Classify(aipolicy.ClassFacts{Stored: stored, Undo: aipolicy.UndoExact})
		return []aipolicy.Class{c.Class}
	}
	seen := map[aipolicy.Class]bool{}
	var out []aipolicy.Class
	for _, status := range []string{"publish", "private", "future", "pending", "draft"} {
		for _, aiDraft := range []bool{true, false} {
			st := status
			c := aipolicy.Classify(aipolicy.ClassFacts{
				Stored: stored, Undo: aipolicy.UndoExact, TargetStatus: &st, AIDraft: aiDraft,
			})
			if c.Class.Effective() && !seen[c.Class] {
				seen[c.Class] = true
				out = append(out, c.Class)
			}
		}
	}
	return out
}

// SetMode sets a site's mode to ask or ai_drafts as a compare-and-set on
// the version the caller read.
//
// Writing ai_drafts loosens the site, so only a signed-in person may, and it
// records that person as the setter even when the mode is already
// ai_drafts. Lowering to ask is open to every caller the route admits: a
// person is recorded as the setter, any other caller as a tightening with no
// person. The write takes the tenant's policy lock and then the site's
// dispatch lock, so it waits for a decision or a reservation in flight, and
// commits with its audit row.
func (s *Service) SetMode(ctx context.Context, p domain.Principal, siteID uuid.UUID, mode aipolicy.Mode, version int64) (SiteMode, error) {
	switch mode {
	case aipolicy.ModeAsk, aipolicy.ModeAIDrafts:
	case aipolicy.ModeFull:
		return SiteMode{}, domain.Validation(CodeUseFullAutoRoute, "This route does not turn on full auto.")
	default:
		return SiteMode{}, domain.Validation("invalid_mode", "mode must be ask or ai_drafts")
	}
	if version < 0 {
		return SiteMode{}, domain.Validation("invalid_version", "version must not be negative")
	}
	person := authz.AuthorizeLoosening(p) == nil
	raise := mode != aipolicy.ModeAsk
	if raise {
		if err := authz.AuthorizeLoosening(p); err != nil {
			return SiteMode{}, err
		}
	}
	arg := sqlc.SetSiteAIModeParams{
		Mode: string(mode), Source: SourceTightened,
		TenantID: p.TenantID, SiteID: siteID, ExpectedVersion: version,
	}
	if person {
		arg.Source = SourcePerson
		arg.SetBy = pgtype.UUID{Bytes: p.UserID, Valid: true}
	}
	err := s.repo.inSettingTx(ctx, p, &siteID, func(st *settingTx) error {
		before, err := st.siteMode(ctx, p.TenantID, siteID)
		if err != nil {
			return err
		}
		if raise {
			paused, err := st.paused(ctx, p.TenantID)
			if err != nil {
				return err
			}
			if paused {
				return domain.Conflict(CodePaused, "AI is paused for your organisation. Nothing was saved.")
			}
			v, err := st.agentVersion(ctx, p.TenantID, siteID)
			if err != nil {
				return err
			}
			if !agentMeetsFloor(v, MinAgentVersion) {
				return domain.Conflict(CodeAgentOutdated,
					"Update the WPMgr plugin on this site to "+MinAgentVersion+" or later, then choose this.").
					WithDetails(map[string]any{"min_agent_version": MinAgentVersion})
			}
		}
		after, err := st.setMode(ctx, arg)
		if err != nil {
			return err
		}
		if !after.Applied {
			return domain.Conflict(CodeStaleVersion, "This setting was changed a moment ago. Nothing was saved.").
				WithDetails(map[string]any{"mode": after.AiMode, "version": after.AiModeVersion})
		}
		if after.AiModeVersion == before.AiModeVersion {
			// The setting already read this way; nothing changed.
			return nil
		}
		actorType, actorID := audit.ActorFor(p)
		return st.record(ctx, audit.Event{
			TenantID: p.TenantID, ActorType: actorType, ActorID: actorID,
			Action: ActionModeChanged, TargetType: "site", TargetID: siteID.String(),
			Metadata: map[string]any{
				"old_mode": before.AiMode, "old_source": before.AiModeSource, "old_version": before.AiModeVersion,
				"new_mode": after.AiMode, "new_source": after.AiModeSource, "new_version": after.AiModeVersion,
				"step_up": nil,
			},
		})
	})
	if err != nil {
		return SiteMode{}, writeError(err, "site_not_found", "site not found")
	}
	return s.GetMode(ctx, p, siteID)
}

// ---------------------------------------------------------------------------
// The connection's switch and usage
// ---------------------------------------------------------------------------

// requireOrgScope refuses a site-constrained principal: a connection is an
// organisation-wide credential.
func requireOrgScope(p domain.Principal) error {
	if p.IsSiteConstrained() {
		return domain.Forbidden(CodeOrgScopeRequired, "this action requires full organisation membership")
	}
	return nil
}

// ConnectionUsage reads a connection's switch and what it ran automatically
// in the window.
func (s *Service) ConnectionUsage(ctx context.Context, p domain.Principal, grantID uuid.UUID) (ConnectionUsage, error) {
	if err := requireOrgScope(p); err != nil {
		return ConnectionUsage{}, err
	}
	row, count, err := s.repo.ReadUsage(ctx, p.TenantID, grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionUsage{}, domain.NotFound("connection_not_found", "connection not found")
	}
	if err != nil {
		return ConnectionUsage{}, domain.Internal("ai_usage_read_failed", "failed to read the connection's usage").WithCause(err)
	}
	return ConnectionUsage{
		ConnectionAuto: s.connectionAuto(ctx, p.TenantID, row),
		WindowMinutes:  int32(aipolicy.BudgetWindow / time.Minute),
		DraftChanges:   UsageBucket{Used: clampInt32(count.Changes), Limit: aipolicy.DraftChangesPerConnection},
		DraftSites:     UsageBucket{Used: clampInt32(count.Sites), Limit: aipolicy.DraftSitesPerConnection},
	}, nil
}

func (s *Service) connectionAuto(ctx context.Context, tenantID uuid.UUID, row sqlc.GetAIConnectionAutoRow) ConnectionAuto {
	a := ConnectionAuto{
		GrantID:             row.ID,
		AIAuto:              aipolicy.ConnectionAuto(row.AiAuto),
		SetByUserID:         uuidPtr(row.AiAutoSetBy),
		SetByAccountDeleted: row.AutoSetByAccountDeleted,
		SetAt:               tsPtr(row.AiAutoSetAt),
		CreatedWithAPIKey:   !row.CreatedByUserID.Valid,
	}
	if !row.AutoSetByAccountDeleted {
		a.SetByName = row.AutoSetByName
	}
	a.SetterValid = s.connectionSetterValid(ctx, tenantID, a.AIAuto, a.SetByUserID)
	return a
}

// connectionSetterValid is the decision engine's check on the person who
// allowed the connection: a full member who can manage connections, with an
// active account. never has nothing to honour.
func (s *Service) connectionSetterValid(ctx context.Context, tenantID uuid.UUID, auto aipolicy.ConnectionAuto, setBy *uuid.UUID) bool {
	if auto != aipolicy.AutoSiteSetting {
		return true
	}
	if setBy == nil || s.setters == nil {
		return false
	}
	return aipolicy.CheckConnectionSetter(s.setters.ResolveSetter(ctx, tenantID, *setBy), tenantID).OK
}

// SetConnectionAuto sets a connection's switch.
//
// site_setting loosens the connection, so only a signed-in person may, and
// it records that person as the one who allowed it even when the switch
// already reads site_setting. never is open to every caller the route
// admits. A site-constrained principal is refused either way. The write
// takes the tenant's policy lock, so it waits for a decision in flight, and
// commits with its audit row.
func (s *Service) SetConnectionAuto(ctx context.Context, p domain.Principal, grantID uuid.UUID, auto aipolicy.ConnectionAuto) (ConnectionAuto, error) {
	if !auto.Known() {
		return ConnectionAuto{}, domain.Validation("invalid_ai_auto", "ai_auto must be site_setting or never")
	}
	if err := requireOrgScope(p); err != nil {
		return ConnectionAuto{}, err
	}
	raise := auto == aipolicy.AutoSiteSetting
	arg := sqlc.SetAIConnectionAutoParams{AiAuto: string(auto), TenantID: p.TenantID, GrantID: grantID}
	if raise {
		if err := authz.AuthorizeLoosening(p); err != nil {
			return ConnectionAuto{}, err
		}
		arg.SetBy = pgtype.UUID{Bytes: p.UserID, Valid: true}
	}
	err := s.repo.inSettingTx(ctx, p, nil, func(st *settingTx) error {
		before, err := st.connectionAuto(ctx, p.TenantID, grantID)
		if err != nil {
			return err
		}
		if raise {
			paused, err := st.paused(ctx, p.TenantID)
			if err != nil {
				return err
			}
			if paused {
				return domain.Conflict(CodePaused, "AI is paused for your organisation. Nothing was saved.")
			}
		}
		after, err := st.setConnectionAuto(ctx, arg)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Conflict(CodeConnectionInactive, "This connection is revoked or expired. Nothing was saved.")
		}
		if err != nil {
			return err
		}
		actorType, actorID := audit.ActorFor(p)
		meta := map[string]any{"old_ai_auto": before.AiAuto, "new_ai_auto": after.AiAuto}
		if before.AiAutoSetBy.Valid {
			meta["old_set_by"] = uuid.UUID(before.AiAutoSetBy.Bytes).String()
		}
		if after.AiAutoSetBy.Valid {
			meta["new_set_by"] = uuid.UUID(after.AiAutoSetBy.Bytes).String()
		}
		return st.record(ctx, audit.Event{
			TenantID: p.TenantID, ActorType: actorType, ActorID: actorID,
			Action: ActionConnectionAutoChanged, TargetType: "mcp_grant", TargetID: grantID.String(),
			Metadata: meta,
		})
	})
	if err != nil {
		return ConnectionAuto{}, writeError(err, "connection_not_found", "connection not found")
	}
	row, err := s.repo.ReadConnectionAuto(ctx, p.TenantID, grantID)
	if err != nil {
		return ConnectionAuto{}, domain.Internal("ai_auto_read_failed", "failed to read the connection's switch").WithCause(err)
	}
	return s.connectionAuto(ctx, p.TenantID, row), nil
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

// Activity reads one page of the feed: every approved request from both
// request tables, newest first, keyset paged on (created_at, id).
func (s *Service) Activity(ctx context.Context, p domain.Principal, q ActivityQuery) (ActivityPage, error) {
	if !KnownFilter(q.Filter) {
		return ActivityPage{}, domain.Validation("invalid_filter", "filter is not a known activity filter")
	}
	if q.Limit < 1 || q.Limit > ActivityMaxLimit {
		return ActivityPage{}, domain.Validation("invalid_limit", "limit must be between 1 and 100")
	}
	if s.abilities == nil || s.purges == nil {
		return ActivityPage{}, domain.Internal("ai_activity_not_wired", "AI activity is not available")
	}
	rows, err := s.repo.ActivityPage(ctx, p, q)
	if err != nil {
		return ActivityPage{}, domain.Internal("ai_activity_read_failed", "failed to read AI activity").WithCause(err)
	}
	refs := rows.refs
	page := ActivityPage{Items: []ActivityItem{}}
	if len(refs) > int(q.Limit) {
		refs = refs[:q.Limit]
		last := refs[len(refs)-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	var (
		abilityRows []sqlc.AssistantAbilityRequest
		purgeRows   []sqlc.AssistantCachePurgeRequest
	)
	for _, ref := range refs {
		switch ref.Kind {
		case KindAbilityRequest:
			if row, ok := rows.abilities[ref.ID]; ok {
				abilityRows = append(abilityRows, row)
			}
		case KindCachePurgeRequest:
			if row, ok := rows.purges[ref.ID]; ok {
				purgeRows = append(purgeRows, row)
			}
		}
	}
	abilityOut, err := s.abilities.RenderAbilityRequests(ctx, p, abilityRows)
	if err != nil {
		return ActivityPage{}, err
	}
	purgeOut, err := s.purges.RenderCachePurgeRequests(ctx, p, purgeRows)
	if err != nil {
		return ActivityPage{}, err
	}
	if len(abilityOut) != len(abilityRows) || len(purgeOut) != len(purgeRows) {
		return ActivityPage{}, domain.Internal("ai_activity_render_failed", "failed to read AI activity")
	}
	ai, pi := 0, 0
	for _, ref := range refs {
		switch ref.Kind {
		case KindAbilityRequest:
			if _, ok := rows.abilities[ref.ID]; ok {
				page.Items = append(page.Items, ActivityItem{Kind: KindAbilityRequest, Request: abilityOut[ai]})
				ai++
			}
		case KindCachePurgeRequest:
			if _, ok := rows.purges[ref.ID]; ok {
				page.Items = append(page.Items, ActivityItem{Kind: KindCachePurgeRequest, Request: purgeOut[pi]})
				pi++
			}
		}
	}
	return page, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeError maps a setting write's error: a domain refusal as it is, a
// missing row as notFound, anything else as an internal failure.
func writeError(err error, notFoundCode, notFoundMsg string) error {
	if de, ok := domain.AsDomain(err); ok {
		return de
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(notFoundCode, notFoundMsg)
	}
	return domain.Internal("ai_setting_write_failed", "WPMgr could not save this. Nothing changed.").WithCause(err)
}

func agentMeetsFloor(v, floor string) bool {
	return v != "" && wpversion.Compare(v, floor) >= 0
}

func uuidPtr(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	id := uuid.UUID(u.Bytes)
	return &id
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func clampInt32(n int64) int32 {
	const maxInt32 = 1<<31 - 1
	switch {
	case n < 0:
		return 0
	case n > maxInt32:
		return maxInt32
	}
	return int32(n)
}
