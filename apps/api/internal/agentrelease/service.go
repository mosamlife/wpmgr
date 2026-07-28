package agentrelease

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// SiteRollup is one site's classified row for the fleet agent-version
// dashboard.
type SiteRollup struct {
	SiteID       uuid.UUID
	SiteName     string
	AgentVersion string
	Status       Status
}

// Counts tallies SiteRollup.Status across the fleet.
type Counts struct {
	Current    int
	Outdated   int
	Unknown    int
	Ineligible int
}

// ReferenceSource names where the version the fleet was classified against
// came from. It is part of the response because the two sources answer
// genuinely different questions, and the caller must never have to guess
// which one it got from an empty string.
type ReferenceSource string

const (
	// ReferenceSourcePublished: the published release channel, i.e. the
	// agent-releases/latest.json pointer manifest this control plane can
	// read. "current" against this reference means the site runs the newest
	// agent that exists.
	ReferenceSourcePublished ReferenceSource = "published"
	// ReferenceSourceFleet: the newest well-formed agent version present in
	// this tenant's OWN fleet, used only when the published manifest cannot
	// be read. A self-hosted install has its own object storage and nothing
	// ever writes the release pipeline's pointer manifest into it, so the
	// published version is permanently unreadable there and every site would
	// otherwise classify "unknown" forever. "current" against this reference
	// means only that no newer agent has been seen in this fleet, NOT that
	// the site runs the newest agent that exists; callers presenting this
	// must say so.
	ReferenceSourceFleet ReferenceSource = "fleet"
	// ReferenceSourceNone: no manifest and no well-formed agent version
	// anywhere in the fleet, so there is nothing safe to compare against and
	// every site is "unknown". The honest answer, never a guess.
	ReferenceSourceNone ReferenceSource = "none"
)

// FleetSummary is the GET /api/v1/fleet/agents payload, at the service layer
// (DTO-agnostic: the handler maps this to the wire shape).
type FleetSummary struct {
	// ReferenceVersion is the single version every row in Sites was
	// classified against, "" when there was none (ReferenceSourceNone).
	// ReferenceSource says where it came from.
	ReferenceVersion string
	ReferenceSource  ReferenceSource
	Counts           Counts
	Sites            []SiteRollup
}

// SiteLister is the narrow repo surface Service needs. Satisfied by *Repo.
type SiteLister interface {
	ListSiteAgentVersions(ctx context.Context, tenantID uuid.UUID) ([]SiteAgentVersion, error)
}

// VersionReader is the narrow reader surface Service needs. Satisfied by
// *Reader.
type VersionReader interface {
	LatestVersion(ctx context.Context) string
}

// PublishHistoryReader is an OPTIONAL capability of a VersionReader: whether a
// published version has ever been read successfully. *Reader implements it.
// A VersionReader that does not is treated as never having published, which
// keeps the fleet-derived fallback available to it (see referenceVersion).
type PublishHistoryReader interface {
	EverPublished() bool
}

// Service composes the tenant-scoped site rollup with the cached published
// version.
type Service struct {
	repo   SiteLister
	reader VersionReader
}

// NewService builds a Service. reader may be nil (object storage not
// configured); LatestVersion and FleetRollup then always treat the published
// version as unknown.
func NewService(repo SiteLister, reader VersionReader) *Service {
	return &Service{repo: repo, reader: reader}
}

// LatestVersion returns the currently published agent version, or "" when
// unknown (see Reader.LatestVersion).
func (s *Service) LatestVersion(ctx context.Context) string {
	if s.reader == nil {
		return ""
	}
	return s.reader.LatestVersion(ctx)
}

// everPublished reports whether the reader has ever successfully read a
// published version. A nil reader, or one that does not carry the optional
// PublishHistoryReader capability, answers false: that is the conservative
// answer here, because false is what keeps the self-hosted fleet fallback
// working (see referenceVersion).
func (s *Service) everPublished() bool {
	if s.reader == nil {
		return false
	}
	if hr, ok := s.reader.(PublishHistoryReader); ok {
		return hr.EverPublished()
	}
	return false
}

// referenceVersion picks the single version the fleet is classified against,
// and reports where it came from.
//
// The published manifest always wins when it is readable, so a hosted install
// behaves exactly as it did before this fallback existed. On an install that
// has never had a readable manifest, the reference is derived from the rows
// the rollup ALREADY loaded (fleetReference): a self-hosted install never
// receives the release pipeline's pointer manifest (nothing writes
// agent-releases/latest.json into its own bucket), and reporting "unknown"
// for every site there answers none of the operator's question, which is
// "which of my sites are lagging?". This is a single pass over data already
// in hand: no second query, no extra round trip.
//
// The control plane's own build version is deliberately NOT a candidate: the
// agent version only moves when the agent changes, so a control plane ahead
// of the agent would mark a correctly updated fleet outdated.
//
// Every site contributes, including the plugin-directory build. Its version
// is a real agent version, and the site itself is still classified
// StatusIneligible whatever the reference turns out to be.
//
// rows carries exactly one tenant's sites (ListSiteAgentVersions is
// tenant-scoped, under both RLS and an explicit tenant_id filter), so the
// derived reference is tenant-local by construction and can never be pulled
// up by a busier neighbour's fleet.
//
// PRECEDENCE, in order:
//
//  1. The published manifest, whenever it is well-formed. Reader serves the
//     last known good version across a read failure, so a blip does not reach
//     here at all; that value is age bounded (maxLastKnownGoodAge), so a
//     SUSTAINED outage eventually does.
//  2. Nothing (ReferenceSourceNone) when no version is readable right now but
//     this install HAS a release channel (everPublished). A gap in a channel
//     that does exist is not a licence to reclassify the whole fleet against
//     itself: the fleet-derived reference would report every site "current"
//     against its own newest agent on the exact day a release lands, which
//     reads as a confident all-clear and hides the "outdated" call to action.
//     Saying plainly that no reference can be determined is the lesser harm,
//     and it is the honest one: the distinction that matters to an operator is
//     "this install has no channel" (self-hosted, case 3) versus "this install
//     has a channel and it is currently unreachable" (here).
//  3. The fleet-derived reference (fleetReference), only when no published
//     version has ever been read. That is precisely the self-hosted steady
//     state this fallback was built for: nothing ever writes the release
//     pipeline's pointer manifest into a self-hosted install's own bucket, so
//     the published version is permanently unreadable there and every site
//     would otherwise classify "unknown" forever.
func referenceVersion(published string, everPublished bool, rows []SiteAgentVersion) (string, ReferenceSource) {
	if isWellFormedVersion(published) {
		return published, ReferenceSourcePublished
	}
	if everPublished {
		return "", ReferenceSourceNone
	}
	derived := fleetReference(rows)
	if derived == "" {
		return "", ReferenceSourceNone
	}
	return derived, ReferenceSourceFleet
}

// fleetReference derives the reference version from the tenant's own reported
// agent versions: the HIGHEST well-formed, non-pre-release version any site in
// this fleet reports, or "" when nothing usable is present. A single site is
// enough to move it.
//
// DO NOT REINSTATE CORROBORATION. An earlier revision required a version to be
// reported by two or more distinct sites before it could become the reference,
// on the theory that one machine should not speak for the whole tenant. It was
// removed because it trades a rare problem for a common one:
//
//   - It suppresses real rollouts, and one site updated first is the NORMAL
//     shape of a rollout in progress. A fleet of 24 sites on 0.61.97 with one
//     site legitimately updated to 0.62.0 answered "25 current, 0 outdated":
//     a confident all-clear while 24 sites were genuinely behind an agent that
//     demonstrably exists in that very fleet. That is the same "everything
//     looks current while things are behind" failure the published path was
//     already fixed for, reintroduced on the fallback path.
//   - It could not deliver its guarantee anyway. When no version reached two
//     reporters the rule was abandoned and the bare highest won, so a three
//     site fleet on 0.61.90 / 0.61.95 / 9.9.9 still answered 9.9.9. It failed
//     on precisely the small self-hosted fleets this fallback exists to serve.
//   - The threat it targeted clears a threshold of two trivially: a sideloaded
//     or locally built agent is rarely on only one machine.
//
// The unguarded newest is correct here because of what the reference actually
// claims. Under ReferenceSourceFleet the product says "newest seen in this
// fleet", not "newest that exists". If one site reports 9.9.9 then 9.9.9 IS
// the newest seen in that fleet, the statement stays true, and that site is
// visible in the very list the operator is looking at. A heuristic that hides
// a real update in order to avoid an unusual one is the worse trade.
//
// Pre-release and build-tagged versions (any -suffix or +suffix, e.g.
// 0.62.0-rc.1) remain excluded from BECOMING the reference: a pre-release is
// not something you would tell an operator to move to. Unlike corroboration,
// this exclusion cannot suppress a legitimate stable rollout, because a stable
// release carries no such suffix. Sites running a pre-release are still
// classified normally and come out current, being ahead of the reference.
func fleetReference(rows []SiteAgentVersion) string {
	highest := ""
	for _, row := range rows {
		v := strings.TrimSpace(row.AgentVersion)
		if !isWellFormedVersion(v) || isPreRelease(v) {
			continue
		}
		if highest == "" || wpversion.Compare(v, highest) > 0 {
			highest = v
		}
	}
	return highest
}

// isPreRelease reports whether a well-formed version carries a pre-release or
// build suffix ("0.62.0-rc.1", "0.61.97+build.3"), the part versionPattern
// admits after the dotted-numeric core.
func isPreRelease(v string) bool {
	return strings.ContainsAny(strings.TrimSpace(v), "-+")
}

// FleetRollup returns the tenant-scoped per-site agent-version rollup plus
// counts, classified against the published agent version when it can be read
// and against the newest agent version present in the tenant's own fleet when
// this install has no readable release channel at all (see referenceVersion).
// The returned FleetSummary always states which of the two, or neither, it
// used.
//
// This fallback is confined to this read-only dashboard on purpose. The
// write path that actually arms an agent self-update (internal/update.
// Service.planAgentTasks) still requires the published version and refuses
// the run without it: a fleet-derived reference describes what is already
// installed somewhere, and there is no artifact to install from it.
func (s *Service) FleetRollup(ctx context.Context, tenantID uuid.UUID) (FleetSummary, error) {
	published := s.LatestVersion(ctx)

	rows, err := s.repo.ListSiteAgentVersions(ctx, tenantID)
	if err != nil {
		return FleetSummary{}, err
	}

	reference, source := referenceVersion(published, s.everPublished(), rows)

	summary := FleetSummary{
		ReferenceVersion: reference,
		ReferenceSource:  source,
		Sites:            make([]SiteRollup, 0, len(rows)),
	}
	for _, row := range rows {
		status := Classify(row.AgentVersion, reference, row.Distribution)
		switch status {
		case StatusCurrent:
			summary.Counts.Current++
		case StatusOutdated:
			summary.Counts.Outdated++
		case StatusIneligible:
			summary.Counts.Ineligible++
		default:
			summary.Counts.Unknown++
		}
		summary.Sites = append(summary.Sites, SiteRollup{
			SiteID:       row.SiteID,
			SiteName:     row.SiteName,
			AgentVersion: row.AgentVersion,
			Status:       status,
		})
	}
	return summary, nil
}
