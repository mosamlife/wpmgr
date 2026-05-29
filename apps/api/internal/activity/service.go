package activity

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SecurityAlerter is the seam into the existing uptime alert Dispatcher. The
// activity service does NOT build a parallel notification system: a
// high-severity event with the site's notify_security flag set flows into the
// SAME Dispatcher (email + webhook) that uptime down/recovery alerts use. The
// uptime package implements this against its Dispatcher.FireSecurityEvent.
type SecurityAlerter interface {
	// NotifySecurity is best-effort: it resolves the tenant's alert config,
	// checks notify_security, and dispatches if enabled. Errors are swallowed
	// by the implementation (delivery is best-effort, like uptime alerts).
	NotifySecurity(ctx context.Context, tenantID, siteID uuid.UUID, summary, eventType, severity, siteURL, siteName string)
}

// SiteLookup resolves the human-facing site URL/name for an alert subject.
// Optional; when nil the alert falls back to the site UUID.
type SiteLookup interface {
	URLAndName(ctx context.Context, tenantID, siteID uuid.UUID) (url, name string)
}

// Service orchestrates ingest (with server-side chain re-verification), list,
// and verify. The Repo is the persistence seam; the SecurityAlerter is the
// optional wiring into the uptime Dispatcher.
type Service struct {
	repo    *Repo
	alerter SecurityAlerter
	sites   SiteLookup
}

// NewService builds a Service. alerter/sites may be nil (no alerting / UUID
// fallback in the subject line).
func NewService(repo *Repo, alerter SecurityAlerter, sites SiteLookup) *Service {
	return &Service{repo: repo, alerter: alerter, sites: sites}
}

// IngestActivity stores the agent-shipped event batch with server-side hash
// re-verification:
//
//  1. Order events by seq ASC (the chain folds forward).
//  2. For each event: look up the PRIOR stored row's this_hash (or genesis on
//     the very first link), recompute this_hash from the event fields, and set
//     chain_valid = (recomputed == shipped this_hash) AND (shipped prev_hash ==
//     prior stored hash). Extract severity from meta.severity (default low).
//  3. Upsert by (tenant_id, site_id, seq) — idempotent on agent retry.
//  4. For any high-severity event, fire a security alert via the Dispatcher
//     (the implementation gates on the site's notify_security flag).
//
// Returns the count ingested and the count of rows flagged chain_valid=false.
func (s *Service) IngestActivity(ctx context.Context, tenantID, siteID uuid.UUID, req IngestRequest) (ingested int, chainBreaks int, err error) {
	if len(req.Events) == 0 {
		return 0, 0, nil
	}

	events := make([]IngestEvent, len(req.Events))
	copy(events, req.Events)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	// Collect high-severity events to alert on AFTER the tx commits (so we never
	// hold a DB tx open across email/webhook I/O).
	type pending struct {
		summary, eventType, severity string
	}
	var alerts []pending

	err = s.repo.IngestTx(ctx, tenantID, func(tx pgx.Tx) error {
		// prevHash tracks the chain head as we fold forward within this batch.
		// It is seeded from the stored prior row of the FIRST event's seq so a
		// multi-batch chain re-verifies against what is already persisted.
		var prevHash string
		seeded := false

		for _, ev := range events {
			if !seeded {
				prior, ok, perr := GetPriorHash(ctx, tx, tenantID, siteID, ev.Seq)
				if perr != nil {
					return perr
				}
				if ok {
					prevHash = prior
				} else {
					prevHash = GenesisPrevHash
				}
				seeded = true
			}

			recomputed := ComputeHash(prevHash, ev)
			chainValid := recomputed == ev.ThisHash && ev.PrevHash == prevHash
			if !chainValid {
				chainBreaks++
			}
			sev := severityFromMeta(ev.Meta)

			row := Event{
				TenantID:    tenantID,
				SiteID:      siteID,
				Seq:         ev.Seq,
				EventType:   ev.EventType,
				ObjectType:  ev.ObjectType,
				ObjectID:    ev.ObjectID,
				ObjectLabel: ev.ObjectLabel,
				ActorUserID: ev.ActorUserID,
				ActorLogin:  ev.ActorLogin,
				ActorIP:     ev.ActorIP,
				Summary:     ev.Summary,
				// Meta = parsed map (display/query JSONB). MetaRaw = verbatim
				// agent bytes (canonicalized "{}" when empty) — the exact
				// preimage the hash chain verifies against.
				Meta:       parseMeta(ev.Meta),
				MetaRaw:    canonicalMetaRaw(ev.Meta),
				Severity:   sev,
				PrevHash:   ev.PrevHash,
				ThisHash:   ev.ThisHash,
				ChainValid: chainValid,
				OccurredAt: ev.OccurredAt.UTC(),
			}
			if uerr := UpsertEvent(ctx, tx, row); uerr != nil {
				return uerr
			}
			ingested++

			// The chain head advances to THIS row's shipped this_hash so the next
			// event re-verifies against the same value the agent chained against,
			// even when this row is itself a break (so a single tampered row does
			// not cascade every subsequent row into a false break).
			prevHash = ev.ThisHash

			if sev == SeverityHigh {
				alerts = append(alerts, pending{summary: ev.Summary, eventType: ev.EventType, severity: sev})
			}
		}
		return nil
	})
	if err != nil {
		return ingested, chainBreaks, err
	}

	if s.alerter != nil && len(alerts) > 0 {
		url, name := "", ""
		if s.sites != nil {
			url, name = s.sites.URLAndName(ctx, tenantID, siteID)
		}
		for _, a := range alerts {
			s.alerter.NotifySecurity(ctx, tenantID, siteID, a.summary, a.eventType, a.severity, url, name)
		}
	}

	return ingested, chainBreaks, nil
}

// ListActivity returns a filtered, paginated page of a site's activity, newest
// first.
func (s *Service) ListActivity(ctx context.Context, tenantID, siteID uuid.UUID, f ListFilter) ([]Event, error) {
	return s.repo.List(ctx, tenantID, siteID, f)
}

// Verify recomputes the whole chain for a site and reports the first broken
// link. Mirrors audit.Recorder.Verify: fold from genesis, recompute each
// this_hash, and flag the first row whose prev_hash != prior hash OR whose
// recomputed hash != stored this_hash.
func (s *Service) Verify(ctx context.Context, tenantID, siteID uuid.UUID) (VerifyResult, error) {
	rows, err := s.repo.ListForVerify(ctx, tenantID, siteID)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyChain(rows), nil
}
