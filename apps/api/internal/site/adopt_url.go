package site

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// urlSourceAgentMetadata names the agent's metadata push as the source of an
// address adopted after enrollment, on the site.url_changed audit row. The
// diagnostics push passes its own source, "agent_diagnostics".
const urlSourceAgentMetadata = "agent_metadata"

// CommandRedirectProber asks whether a site's saved address redirects its
// command route, and where to. agentcmd.Client implements it with one signed
// ping sent to the saved address.
type CommandRedirectProber interface {
	// CommandRedirectTarget returns redirected=true when the command sent to
	// siteURL was answered with a redirect that was not followed, and the
	// address the saved one would become (RedirectError.SuggestedSiteURL,
	// which is empty when the redirect names no adoptable address).
	CommandRedirectTarget(ctx context.Context, siteID uuid.UUID, siteURL string) (suggested string, redirected bool)
}

// SetCommandRedirectProber wires the prober AdoptReportedURL needs before it
// adopts a host change. Without one, a host change is never adopted after
// enrollment; an http to https upgrade on the same host still is.
func (s *Service) SetCommandRedirectProber(p CommandRedirectProber) { s.redirectProber = p }

// AdoptReportedURL decides whether the address an enrolled site's agent
// reports (its WordPress home_url) replaces the saved address. It is
// best-effort: a refusal is logged, never returned, and a load or write
// failure is logged and returned for the caller to drop, so it can never fail
// the push that carried the address.
//
// The rule is siteaddr.Plan, the one enrollment applies: only a leading
// "www." toggle and/or an http to https upgrade, on the same port and path.
// A scheme-only upgrade is written directly. A host change (the "www."
// toggle) is written only when a signed ping to the saved address is
// redirected right now to exactly the planned address. Because the scheme
// only ever goes up, and a host change needs the saved address to redirect
// there at the moment of the push, two installs cannot flip the address back
// and forth.
func (s *Service) AdoptReportedURL(ctx context.Context, tenantID, siteID uuid.UUID, reported, source, agentVersion string) error {
	_, err := s.adoptReportedURL(ctx, tenantID, siteID, reported, source, agentVersion)
	return err
}

// adoptReportedURL is AdoptReportedURL, also reporting whether the address
// was written.
func (s *Service) adoptReportedURL(ctx context.Context, tenantID, siteID uuid.UUID, reported, source, agentVersion string) (bool, error) {
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return false, nil
	}
	log := s.logger.With(
		slog.String("site_id", siteID.String()),
		slog.String("source", source),
	)
	st, err := s.repo.Get(ctx, tenantID, siteID)
	if err != nil {
		log.Warn("adopt reported address: load site failed", slog.Any("error", err))
		return false, err
	}
	switch st.ConnectionState {
	case StateConnected, StateDegraded, StateDisconnected:
	default:
		// pending_enrollment belongs to enrollment; revoked and archived
		// sites are never re-addressed.
		return false, nil
	}

	plan := planEnrollURL(st.URL, reported)
	switch plan.Decision {
	case enrollURLSame:
		return false, nil
	case enrollURLAdopt:
	default:
		// Not audited: it would repeat on every push.
		log.Debug("adopt reported address: not equivalent to the saved address",
			slog.String("saved", st.URL),
			slog.String("reported", sanitizeReportedURL(reported)))
		return false, nil
	}

	saved, _, okS := parseSiteAddress(st.URL)
	to, _, okT := parseSiteAddress(plan.To)
	if !okS || !okT {
		return false, nil
	}
	if saved.Host != to.Host {
		if s.redirectProber == nil {
			log.Info("adopt reported address: not adopted: no redirect prober to confirm a host change",
				slog.String("saved", st.URL), slog.String("to", plan.To))
			return false, nil
		}
		suggested, redirected := s.redirectProber.CommandRedirectTarget(ctx, siteID, st.URL)
		if !redirected || suggested != plan.To {
			log.Info("adopt reported address: not adopted: saved address does not redirect to the reported address",
				slog.String("saved", st.URL), slog.String("to", plan.To))
			return false, nil
		}
	}

	adopted, err := s.repo.AdoptSiteURL(ctx, tenantID, siteID, st.URL, plan.To)
	if err != nil {
		log.Warn("adopt reported address: write failed", slog.Any("error", err))
		return false, err
	}
	if !adopted {
		log.Info("adopt reported address: not adopted: the address changed, the site left the enrolled states, or another site holds the address",
			slog.String("saved", st.URL), slog.String("to", plan.To))
		return false, nil
	}
	meta := map[string]any{
		"from":   st.URL,
		"to":     plan.To,
		"source": source,
	}
	if agentVersion != "" {
		meta["agent_version"] = agentVersion
	}
	s.recordAudit(ctx, tenantID, siteID, audit.ActionSiteURLChanged, meta)
	log.Info("adopt reported address: adopted", slog.String("from", st.URL), slog.String("to", plan.To))
	return true, nil
}

// AdoptSiteURL replaces a site's saved address, compare-and-set on from. It
// reports false, with no error, when the site no longer holds from, is not in
// an enrolled state, or another site in the tenant holds to (the NOT EXISTS
// guard, or the unique index under a race): each of those is "not adopted".
// A caller cannot tell them apart, so a site-scoped caller learns nothing
// about a site outside its scope.
func (r *pgRepo) AdoptSiteURL(ctx context.Context, tenantID, siteID uuid.UUID, from, to string) (bool, error) {
	adopted := false
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).AdoptSiteURL(ctx, sqlc.AdoptSiteURLParams{
			To:       to,
			ID:       siteID,
			TenantID: tenantID,
			From:     from,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		adopted = true
		return nil
	})
	if err != nil {
		if isSiteURLUniqueViolation(err) {
			return false, nil
		}
		return false, err
	}
	return adopted, nil
}
