package aitrust

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// noticeStore is the launch notice's database as the contract describes it:
// a claim takes an organisation's waiting sites once, and a release makes
// them waiting again.
type noticeStore struct {
	waiting    map[uuid.UUID]LaunchNoticeClaim
	claimErr   map[uuid.UUID]error
	releaseErr error
	releases   map[uuid.UUID][]LaunchNoticeClaim
	// releaseCtxErr is the context's error each release saw.
	releaseCtxErr []error
	// lists and claims count the calls that reached the store.
	lists, claims int
}

func newNoticeStore() *noticeStore {
	return &noticeStore{
		waiting:  map[uuid.UUID]LaunchNoticeClaim{},
		claimErr: map[uuid.UUID]error{},
		releases: map[uuid.UUID][]LaunchNoticeClaim{},
	}
}

func (f *noticeStore) TenantsAwaitingLaunchNotice(context.Context) ([]uuid.UUID, error) {
	f.lists++
	var out []uuid.UUID
	for t, c := range f.waiting {
		if len(c.Sites) > 0 {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func (f *noticeStore) ClaimLaunchNotice(_ context.Context, t uuid.UUID) (LaunchNoticeClaim, error) {
	f.claims++
	if err := f.claimErr[t]; err != nil {
		return LaunchNoticeClaim{}, err
	}
	c := f.waiting[t]
	delete(f.waiting, t)
	return c, nil
}

func (f *noticeStore) ReleaseLaunchNotice(ctx context.Context, t uuid.UUID, c LaunchNoticeClaim) (int64, error) {
	f.releaseCtxErr = append(f.releaseCtxErr, ctx.Err())
	if f.releaseErr != nil {
		return 0, f.releaseErr
	}
	f.releases[t] = append(f.releases[t], c)
	f.waiting[t] = c
	return int64(len(c.Sites)), nil
}

// noticeMailer records every send. failures[t] sends to t fail before one
// is delivered; skipped makes every send report "not delivered" with no
// error, as the mailer does when mail is not set up.
type noticeMailer struct {
	failures  map[uuid.UUID]int
	skipped   bool
	attempts  map[uuid.UUID]int
	delivered map[uuid.UUID][]string
	// during, when set, runs inside the send (a shutdown arriving mid-send).
	during func()
}

func newNoticeMailer() *noticeMailer {
	return &noticeMailer{failures: map[uuid.UUID]int{}, attempts: map[uuid.UUID]int{}, delivered: map[uuid.UUID][]string{}}
}

func (m *noticeMailer) SendLaunchNotice(_ context.Context, t uuid.UUID, recipients []string, subject, text string) (bool, error) {
	m.attempts[t]++
	if m.during != nil {
		m.during()
	}
	if m.failures[t] > 0 {
		m.failures[t]--
		return false, errors.New("smtp: 451 try again later")
	}
	if m.skipped {
		return false, nil
	}
	m.delivered[t] = append(m.delivered[t], subject+"\n"+text)
	return true, nil
}

func noticeClaim(n int, recipients ...string) LaunchNoticeClaim {
	c := LaunchNoticeClaim{ClaimedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), Recipients: recipients}
	for i := 0; i < n; i++ {
		c.Sites = append(c.Sites, LaunchNoticeSite{ID: uuid.New(), Name: fmt.Sprintf("Site %d", i), URL: fmt.Sprintf("https://s%d.example.com", i)})
	}
	return c
}

func quietNotifier(store LaunchNoticeStore, m LaunchNoticeMailer) *LaunchNotifier {
	return switchedNotifier(store, m, true)
}

// switchedNotifier is the notifier a boot builds with WPMGR_AI_LAUNCH_NOTICE
// on (true) or off (false).
func switchedNotifier(store LaunchNoticeStore, m LaunchNoticeMailer, on bool) *LaunchNotifier {
	return NewLaunchNotifier(store, m, "https://wpmgr.test/", on, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestLaunchNoticeReleasesAnUndeliveredNotice proves one notice per
// organisation across runs: a delivered notice is not sent again, and a
// notice whose send failed is released with the claim it made and sent by
// the next run.
//
// Mutation: skip the release after a failed send; the second organisation
// is then never told.
func TestLaunchNoticeReleasesAnUndeliveredNotice(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	store, mail := newNoticeStore(), newNoticeMailer()
	store.waiting[a] = noticeClaim(2, "owner-a@example.com")
	store.waiting[b] = noticeClaim(1, "owner-b@example.com", "admin-b@example.com")
	bClaim := store.waiting[b]
	mail.failures[b] = 1
	n := quietNotifier(store, mail)

	runs := make([]LaunchNoticeRun, 0, 3)
	for i := 0; i < 3; i++ {
		run, err := n.Run(context.Background())
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		runs = append(runs, run)
	}
	if got := (LaunchNoticeRun{Tenants: 2, Sent: 1, Released: 1}); runs[0] != got {
		t.Fatalf("first run %+v, want %+v", runs[0], got)
	}
	if got := (LaunchNoticeRun{Tenants: 1, Sent: 1}); runs[1] != got {
		t.Fatalf("second run %+v, want %+v", runs[1], got)
	}
	if runs[2] != (LaunchNoticeRun{}) {
		t.Fatalf("third run %+v, want nothing to do", runs[2])
	}
	if len(mail.delivered[a]) != 1 || len(mail.delivered[b]) != 1 {
		t.Fatalf("delivered %d to A and %d to B, want one each", len(mail.delivered[a]), len(mail.delivered[b]))
	}
	if mail.attempts[a] != 1 || mail.attempts[b] != 2 {
		t.Fatalf("attempts A=%d B=%d, want 1 and 2", mail.attempts[a], mail.attempts[b])
	}
	if len(store.releases[a]) != 0 || len(store.releases[b]) != 1 {
		t.Fatalf("releases A=%d B=%d, want 0 and 1", len(store.releases[a]), len(store.releases[b]))
	}
	if r := store.releases[b][0]; !r.ClaimedAt.Equal(bClaim.ClaimedAt) || len(r.Sites) != 1 || r.Sites[0].ID != bClaim.Sites[0].ID {
		t.Fatalf("released %+v, want the claim the failed send made", r)
	}
	t.Logf("runs: %+v; attempts A=%d B=%d", runs, mail.attempts[a], mail.attempts[b])
}

// TestLaunchNoticeNotDeliveredIsReleased proves a notice that could not be
// delivered for any reason (no owner or admin with an active account, or no
// mail transport) leaves its sites waiting for a later run.
func TestLaunchNoticeNotDeliveredIsReleased(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recipients []string
		skipped    bool
		attempts   int
	}{
		{name: "no one to tell", attempts: 0},
		{name: "mail not set up", recipients: []string{"owner@example.com"}, skipped: true, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := uuid.New()
			store, mail := newNoticeStore(), newNoticeMailer()
			store.waiting[org] = noticeClaim(1, tc.recipients...)
			mail.skipped = tc.skipped
			run, err := quietNotifier(store, mail).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if run != (LaunchNoticeRun{Tenants: 1, Released: 1}) || mail.attempts[org] != tc.attempts {
				t.Fatalf("run %+v with %d sends, want released after %d", run, mail.attempts[org], tc.attempts)
			}
			if len(store.waiting[org].Sites) != 1 {
				t.Fatalf("the site is no longer waiting after an undelivered notice")
			}
		})
	}
}

// TestLaunchNoticeReleaseOutlivesTheRun proves a failed send is released
// even when the run's context ended during the send (a shutdown).
//
// Mutation: release on the run's own context; the release then sees a
// cancelled context.
func TestLaunchNoticeReleaseOutlivesTheRun(t *testing.T) {
	org := uuid.New()
	store, mail := newNoticeStore(), newNoticeMailer()
	store.waiting[org] = noticeClaim(1, "owner@example.com")
	mail.failures[org] = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mail.during = cancel
	if _, err := quietNotifier(store, mail).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.releaseCtxErr) != 1 || store.releaseCtxErr[0] != nil {
		t.Fatalf("release saw %v, want one release on a live context", store.releaseCtxErr)
	}
}

// TestLaunchNoticeOneOrganisationsFailureIsIsolated proves a failed claim, or
// a failed release, is counted and logged while the other organisations are
// still told.
func TestLaunchNoticeOneOrganisationsFailureIsIsolated(t *testing.T) {
	bad, good := uuid.New(), uuid.New()
	store, mail := newNoticeStore(), newNoticeMailer()
	store.waiting[bad] = noticeClaim(1, "owner-bad@example.com")
	store.waiting[good] = noticeClaim(1, "owner-good@example.com")
	store.claimErr[bad] = errors.New("connection reset")
	run, err := quietNotifier(store, mail).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run != (LaunchNoticeRun{Tenants: 2, Sent: 1, Failed: 1}) || len(mail.delivered[good]) != 1 || mail.attempts[bad] != 0 {
		t.Fatalf("run %+v, delivered %v", run, mail.delivered)
	}

	store, mail = newNoticeStore(), newNoticeMailer()
	store.waiting[bad] = noticeClaim(1, "owner-bad@example.com")
	mail.failures[bad] = 1
	store.releaseErr = errors.New("connection reset")
	if run, err = quietNotifier(store, mail).Run(context.Background()); err != nil || run != (LaunchNoticeRun{Tenants: 1, Failed: 1}) {
		t.Fatalf("failed release: run %+v, err %v; want counted as failed", run, err)
	}
}

// TestLaunchNoticeMessage proves the copy: the count in the subject, each
// site once with a link to its setting, names that cannot add lines, a list
// that is capped, and no em or en dash.
func TestLaunchNoticeMessage(t *testing.T) {
	sites := []LaunchNoticeSite{
		{ID: uuid.MustParse("cccccccc-0000-0000-0000-000000000002"), Name: "Shop\nClick here to approve\u202e", URL: "https://shop.example.com/"},
		{ID: uuid.MustParse("cccccccc-0000-0000-0000-000000000001"), Name: "Blog", URL: "https://blog.example.com"},
	}
	subject, text := launchNoticeMessage("https://wpmgr.test", sites)
	if subject != "AI drafts now run without asking on 2 of your sites" {
		t.Fatalf("subject %q", subject)
	}
	for _, want := range []string{
		"AI editing is on for these sites:\n\n",
		"* Blog (blog.example.com)\n  Its setting: https://wpmgr.test/sites/cccccccc-0000-0000-0000-000000000001/content\n",
		"* Shop Click here to approve (shop.example.com)\n  Its setting: https://wpmgr.test/sites/cccccccc-0000-0000-0000-000000000002/content\n",
		"From today, drafts the AI makes there run without asking.",
		"To keep approving every draft, open a site and choose Keep asking every time.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("body lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "* Blog") > strings.Index(text, "* Shop") {
		t.Fatalf("sites not in name order:\n%s", text)
	}
	if strings.ContainsAny(subject+text, "\u2013\u2014\u202e") {
		t.Fatalf("an en or em dash, or a direction override, in the notice:\n%s", text)
	}

	many := make([]LaunchNoticeSite, launchNoticeMaxListed+2)
	for i := range many {
		many[i] = LaunchNoticeSite{ID: uuid.New(), Name: fmt.Sprintf("Site %03d", i), URL: "https://x.example.com"}
	}
	subject, text = launchNoticeMessage("", many)
	if !strings.Contains(subject, fmt.Sprintf("on %d of", len(many))) || strings.Count(text, "\n* ") != launchNoticeMaxListed ||
		!strings.Contains(text, "And 2 more.") || strings.Contains(text, "Its setting:") {
		t.Fatalf("capped notice wrong: subject %q\n%s", subject, text)
	}
}
