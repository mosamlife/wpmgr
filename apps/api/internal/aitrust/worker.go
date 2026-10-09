package aitrust

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// The launch notice (ADR-065): when approval tiers launched, every site with
// AI editing on moved to Auto for AI drafts, chosen by the person who turned
// AI editing on. Each organisation's owners and admins are told once, in one
// email that lists those sites.
//
// The contract:
//
//   - A run claims an organisation's sites in that organisation's own
//     transaction, then sends, outside any transaction.
//   - A notice that is not delivered (the send failed, mail is not set up, or
//     no owner or admin has an active account) releases its claim, so a later
//     run tells them.
//   - A site is claimed by one run at a time, so one notice lists it at most
//     once. A run stopped between its claim and its send leaves those sites
//     claimed; each still shows the notice on its own AI editing card.
//   - The job runs when the control plane starts and every
//     LaunchNoticeInterval after that, and does nothing once every
//     organisation has been told.

// LaunchNoticeInterval is how often the notice job runs after the run at
// start-up.
const LaunchNoticeInterval = time.Hour

// launchNoticeMaxListed caps the sites one email names; the rest are counted.
const launchNoticeMaxListed = 50

// launchNoticeReleaseTimeout bounds the release after a failed send. It runs
// even when the job's own context has ended, so a stopped run does not leave
// a failed notice claimed.
const launchNoticeReleaseTimeout = 30 * time.Second

// LaunchNoticeStore is the database side of the launch notice. *Repo
// satisfies it.
type LaunchNoticeStore interface {
	TenantsAwaitingLaunchNotice(ctx context.Context) ([]uuid.UUID, error)
	ClaimLaunchNotice(ctx context.Context, tenantID uuid.UUID) (LaunchNoticeClaim, error)
	ReleaseLaunchNotice(ctx context.Context, tenantID uuid.UUID, c LaunchNoticeClaim) (int64, error)
}

// LaunchNoticeMailer sends one plain-text message to an organisation's
// owners and admins, and reports whether it was delivered. cmd/wpmgr adapts
// the mailer service to it.
type LaunchNoticeMailer interface {
	SendLaunchNotice(ctx context.Context, tenantID uuid.UUID, recipients []string, subject, text string) (delivered bool, err error)
}

// LaunchNotifier sends the launch notice.
type LaunchNotifier struct {
	store   LaunchNoticeStore
	mailer  LaunchNoticeMailer
	baseURL string
	log     *slog.Logger
}

// NewLaunchNotifier builds the notifier. publicBaseURL is
// WPMGR_PUBLIC_BASE_URL, for the link to each site's setting.
func NewLaunchNotifier(store LaunchNoticeStore, mailer LaunchNoticeMailer, publicBaseURL string, log *slog.Logger) *LaunchNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &LaunchNotifier{
		store:   store,
		mailer:  mailer,
		baseURL: strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		log:     log,
	}
}

// LaunchNoticeRun counts what one run did.
type LaunchNoticeRun struct {
	// Tenants is how many organisations had sites awaiting the notice.
	Tenants int
	// Sent is how many organisations were told in this run.
	Sent int
	// Released is how many organisations' claims were released for a later
	// run, because their notice was not delivered.
	Released int
	// Failed is how many organisations could not be claimed, or whose claim
	// could not be released.
	Failed int
}

// Run tells every organisation that has sites awaiting the notice. One
// organisation's failure never stops the others; it is logged, counted, and
// retried by a later run.
func (n *LaunchNotifier) Run(ctx context.Context) (LaunchNoticeRun, error) {
	var run LaunchNoticeRun
	tenants, err := n.store.TenantsAwaitingLaunchNotice(ctx)
	if err != nil {
		return run, fmt.Errorf("list the organisations awaiting the AI launch notice: %w", err)
	}
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		run.Tenants++
		switch out, err := n.notifyTenant(ctx, tenantID); {
		case err != nil:
			run.Failed++
			n.log.WarnContext(ctx, "ai launch notice: organisation not told; a later run retries",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
		case out == launchNoticeSent:
			run.Sent++
		case out == launchNoticeReleased:
			run.Released++
		}
	}
	return run, nil
}

type launchNoticeOutcome int

const (
	launchNoticeNothing launchNoticeOutcome = iota
	launchNoticeSent
	launchNoticeReleased
)

// notifyTenant claims one organisation's sites, sends its notice, and
// releases the claim when the notice was not delivered.
func (n *LaunchNotifier) notifyTenant(ctx context.Context, tenantID uuid.UUID) (launchNoticeOutcome, error) {
	claim, err := n.store.ClaimLaunchNotice(ctx, tenantID)
	if err != nil {
		return launchNoticeNothing, fmt.Errorf("claim: %w", err)
	}
	if len(claim.Sites) == 0 {
		// Another run claimed them first, or a person chose the mode since.
		return launchNoticeNothing, nil
	}
	delivered, sendErr := false, error(nil)
	if n.mailer != nil && len(claim.Recipients) > 0 {
		subject, text := launchNoticeMessage(n.baseURL, claim.Sites)
		delivered, sendErr = n.mailer.SendLaunchNotice(ctx, tenantID, claim.Recipients, subject, text)
	}
	if delivered {
		return launchNoticeSent, nil
	}
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchNoticeReleaseTimeout)
	defer cancel()
	released, err := n.store.ReleaseLaunchNotice(relCtx, tenantID, claim)
	if err != nil {
		return launchNoticeNothing, fmt.Errorf("release after an undelivered notice: %w", err)
	}
	n.log.InfoContext(ctx, "ai launch notice: not delivered; released for a later run",
		slog.String("tenant_id", tenantID.String()), slog.Int("recipients", len(claim.Recipients)),
		slog.Int64("sites_released", released), slog.Any("send_error", sendErr))
	return launchNoticeReleased, nil
}

// launchNoticeMessage is the notice's subject and plain-text body. Site
// names and addresses are cleaned and capped; nothing an AI wrote is in it,
// and it carries no approve link.
func launchNoticeMessage(baseURL string, sites []LaunchNoticeSite) (subject, text string) {
	type line struct {
		name, host string
		id         uuid.UUID
	}
	lines := make([]line, 0, len(sites))
	for _, s := range sites {
		lines = append(lines, line{
			name: humantext.CapRunes(humantext.Clean(s.Name), 80),
			host: launchNoticeHost(s.URL),
			id:   s.ID,
		})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].name != lines[j].name {
			return lines[i].name < lines[j].name
		}
		return lines[i].id.String() < lines[j].id.String()
	})

	subject = fmt.Sprintf("AI drafts now run without asking on %d of your sites", len(sites))
	var b strings.Builder
	b.WriteString("AI editing is on for these sites:\n\n")
	for i, l := range lines {
		if i == launchNoticeMaxListed {
			fmt.Fprintf(&b, "And %d more.\n", len(lines)-launchNoticeMaxListed)
			break
		}
		switch {
		case l.name != "" && l.host != "":
			fmt.Fprintf(&b, "* %s (%s)\n", l.name, l.host)
		case l.name != "":
			fmt.Fprintf(&b, "* %s\n", l.name)
		default:
			fmt.Fprintf(&b, "* %s\n", l.host)
		}
		if baseURL != "" {
			fmt.Fprintf(&b, "  Its setting: %s/sites/%s/content\n", baseURL, l.id)
		}
	}
	b.WriteString("\nFrom today, drafts the AI makes there run without asking. Each is saved with a copy, " +
		"listed in AI activity, and can be undone. Published pages and everything else still wait for you.\n\n" +
		"To keep approving every draft, open a site and choose Keep asking every time.\n")
	return subject, b.String()
}

// launchNoticeHost is the host of a site's address, cleaned and capped, or
// the cleaned address when it has no host.
func launchNoticeHost(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Host != "" {
		return humantext.CapRunes(humantext.Clean(u.Host), 120)
	}
	return humantext.CapRunes(humantext.Clean(raw), 120)
}

// LaunchNoticeArgs is the River job that runs the launch notice.
type LaunchNoticeArgs struct{}

// Kind implements river.JobArgs.
func (LaunchNoticeArgs) Kind() string { return "ai_mode_launch_notice" }

// LaunchNoticeWorker runs the launch notice.
type LaunchNoticeWorker struct {
	river.WorkerDefaults[LaunchNoticeArgs]
	notifier *LaunchNotifier
}

// NewLaunchNoticeWorker builds the worker.
func NewLaunchNoticeWorker(n *LaunchNotifier) *LaunchNoticeWorker {
	return &LaunchNoticeWorker{notifier: n}
}

// Timeout bounds one run; each send is bounded by the mailer.
func (w *LaunchNoticeWorker) Timeout(*river.Job[LaunchNoticeArgs]) time.Duration {
	return 10 * time.Minute
}

// Work runs the notice once. Organisations it could not tell are retried by
// the next run, so only a failure to list them fails the job.
func (w *LaunchNoticeWorker) Work(ctx context.Context, _ *river.Job[LaunchNoticeArgs]) error {
	run, err := w.notifier.Run(ctx)
	if run.Tenants > 0 {
		w.notifier.log.InfoContext(ctx, "ai launch notice: run finished",
			slog.Int("organisations", run.Tenants), slog.Int("sent", run.Sent),
			slog.Int("released", run.Released), slog.Int("failed", run.Failed))
	}
	return err
}

var _ LaunchNoticeStore = (*Repo)(nil)
