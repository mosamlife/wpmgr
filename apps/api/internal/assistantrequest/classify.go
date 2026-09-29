package assistantrequest

import (
	"errors"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// outcome is what one send is recorded as.
type outcome struct {
	Outcome             string
	NotSentReason       *string
	HostingCleared      []string
	HostingSkipped      []string
	OriginOnlyConfirmed *bool
	WpmgrCDN            *string
	SiteReportedText    *string
}

// The integration actions the agent reports under origin_only.
const (
	integrationPurgedAll       = "purged_all"
	integrationPurgedURLs      = "purged_urls"
	integrationPurgedURLsExact = "purged_urls_exact"
	integrationSkipped         = "skipped_reach_unconfirmed"
)

// classify turns one send into its recorded outcome.
//
// Neither failure class claims that nothing changed on the site: a clear can
// fail part way through. Only a failure before any byte could reach the site
// is recorded as not sent.
func classify(res perf.AssistantPurgeResult, err error) outcome {
	switch {
	case err == nil:
		return purgedOutcome(res)
	case errors.Is(err, agentcmd.ErrCommandNotSent), errors.Is(err, perf.ErrAssistantPurgeNotWired):
		r := ReasonTransportPreSend
		return outcome{Outcome: OutcomeNotSent, NotSentReason: &r}
	case errors.Is(err, agentcmd.ErrAgentReportedFailure):
		return outcome{Outcome: OutcomeSiteReportedFailure, SiteReportedText: nonEmpty(humantext.Reason(res.Agent.Detail, maxSiteReportedText))}
	}
	if ce, ok := agentcmd.AsCommandError(err); ok && ce.AgentFailed() {
		return outcome{Outcome: OutcomeAgentFailed, SiteReportedText: nonEmpty(ce.OperatorMessage(operatorMessageAction))}
	}
	return outcome{Outcome: OutcomeUnknown}
}

// purgedOutcome records a successful clear and what the site said it did
// with each hosting cache. The report is kept only when the site confirmed it
// applied origin_only; any slug or action outside the closed sets is dropped.
func purgedOutcome(res perf.AssistantPurgeResult) outcome {
	confirmed := res.Agent.OriginOnlyHonoured != nil && *res.Agent.OriginOnlyHonoured
	oc := outcome{Outcome: OutcomePurged, OriginOnlyConfirmed: &confirmed}
	if cdn := res.WpmgrCDN; cdn != "" {
		oc.WpmgrCDN = &cdn
	}
	if !confirmed {
		return oc
	}
	known := map[string]struct{}{}
	for _, s := range mcp.HostingCacheSlugs() {
		known[s] = struct{}{}
	}
	cleared, skipped := []string{}, []string{}
	seen := map[string]struct{}{}
	for _, in := range res.Agent.Integrations {
		if _, ok := known[in.Slug]; !ok {
			continue
		}
		if _, dup := seen[in.Slug]; dup {
			continue
		}
		switch in.Action {
		case integrationPurgedAll, integrationPurgedURLs, integrationPurgedURLsExact:
			cleared = append(cleared, in.Slug)
		case integrationSkipped:
			skipped = append(skipped, in.Slug)
		default:
			continue
		}
		seen[in.Slug] = struct{}{}
	}
	oc.HostingCleared, oc.HostingSkipped = cleared, skipped
	return oc
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
