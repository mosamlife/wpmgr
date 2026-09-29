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

// CommandRedirectProber confirms an address change with one signed ping.
// agentcmd.Client implements it.
//
// Each method also reports answered: true when the ping got a definitive
// answer, a 2xx or a redirect whose target was read, whatever that answer
// means for the change. A transport failure, a timeout and any other status
// are not answered. AdoptReportedURL holds an address back for a day after
// an answered probe, and for an hour after one that was not.
type CommandRedirectProber interface {
	// CommandRedirectTarget returns redirected=true when the command sent to
	// siteURL was answered with a redirect that was not followed, and the
	// address the saved one would become (RedirectError.SuggestedSiteURL,
	// which is empty when the redirect names no adoptable address).
	CommandRedirectTarget(ctx context.Context, siteID uuid.UUID, siteURL string) (suggested string, redirected, answered bool)
	// CommandPingOK reports whether a signed ping sent to exactly siteURL was
	// answered with a 2xx by the agent. A redirect is false.
	CommandPingOK(ctx context.Context, siteID uuid.UUID, siteURL string) (ok, answered bool)
}

// SetCommandRedirectProber wires the prober AdoptReportedURL needs before it
// adopts any address change. Without one, no address change is adopted after
// enrollment.
func (s *Service) SetCommandRedirectProber(p CommandRedirectProber) { s.redirectProber = p }

// AdoptReportedURL decides whether the address an enrolled site's agent
// reports (its WordPress home_url) replaces the saved address. Agent pushes
// never call it: they queue an AdoptReportedURLArgs job
// (EnqueueAdoptReportedURL), whose worker calls it with its own timeout, so
// the signed probe below never holds up a push. It is best-effort: a refusal
// is logged, never returned, and a load or write failure is logged and
// returned, so the job retries it.
//
// The rule is siteaddr.PlanStrict: enrollment's rule (only a leading "www."
// toggle and/or an http to https upgrade, on the same port and path, with the
// adopted address dialling the host that was compared; a spelling of the
// saved host with the same HostKey is the saved address, and changes
// nothing). Nothing is written without a signed ping confirming it when the
// job runs:
//
//   - A host change (the "www." toggle) is written only when a ping to the
//     saved address is redirected to the planned address (siteaddr.SameAddress:
//     the same address once normalised, so a trailing slash on either side
//     does not matter and any other difference does).
//   - A scheme-only upgrade is written only when a ping to the https form of
//     the saved host is answered with a 2xx. A failure or a redirect leaves
//     the saved address as it is.
//
// Each (site, planned address) is probed by one caller at a time. After a
// probe with a definitive answer (a 2xx, or a redirect whose target was
// read) it is not probed again for adoptProbeWindow, so an address the site
// reports but does not serve costs one ping a day, not one per push. After a
// probe without one (a transport failure or a timeout) it waits only
// adoptProbeBackoff, so a passing outage does not cost a day. When the probe
// confirms the change but the address write then fails, nothing is held: the
// error is returned and the job's retry probes and writes again. Because the
// scheme only ever goes up, and a host change needs the saved address to
// redirect there when the job runs, two installs cannot flip the address back
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

	plan := planReportedURL(st.URL, reported)
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
	if s.redirectProber == nil {
		log.Info("adopt reported address: not adopted: no prober to confirm the change",
			slog.String("saved", st.URL), slog.String("to", plan.To))
		return false, nil
	}
	limiter, key := s.probeLimiter(), probeKey{site: siteID, address: plan.To}
	if !limiter.begin(key, s.now()) {
		// Not Info: it would repeat on every push inside the window.
		log.Debug("adopt reported address: not adopted: this address is being probed or was probed recently",
			slog.String("saved", st.URL), slog.String("to", plan.To))
		return false, nil
	}
	// The hold is recorded once the probe is over, from its outcome: a day
	// after a definitive answer, an hour after none. A write that fails
	// after a confirming probe records no hold (writeFailed), so the retry
	// River runs for the returned error is not refused here.
	hold := adoptProbeBackoff
	writeFailed := false
	defer func() {
		if writeFailed {
			limiter.release(key)
			return
		}
		limiter.finish(key, s.now(), hold)
	}()
	if saved.Host != to.Host {
		// The suggestion is built from the command URL, which never carries
		// the saved address's trailing slash, so it is compared as an
		// address, not as a string. plan.To keeps the saved path form.
		suggested, redirected, answered := s.redirectProber.CommandRedirectTarget(ctx, siteID, st.URL)
		if answered {
			hold = adoptProbeWindow
		}
		if !redirected || !sameSiteAddress(suggested, plan.To) {
			log.Info("adopt reported address: not adopted: saved address does not redirect to the reported address",
				slog.String("saved", st.URL), slog.String("to", plan.To))
			return false, nil
		}
	} else {
		ok, answered := s.redirectProber.CommandPingOK(ctx, siteID, plan.To)
		if answered {
			hold = adoptProbeWindow
		}
		if !ok {
			log.Info("adopt reported address: not adopted: the https address did not answer a signed ping with a 2xx",
				slog.String("saved", st.URL), slog.String("to", plan.To))
			return false, nil
		}
	}

	adopted, err := s.repo.AdoptSiteURL(ctx, tenantID, siteID, st.URL, plan.To)
	if err != nil {
		// A database error, not the compare-and-set finding no row (that is
		// adopted == false with no error, and keeps its hold).
		writeFailed = true
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
