package tests

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/aitrust"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// lnMailer records the launch notices the notifier sends. failFirst makes
// the first send to an organisation fail.
type lnMailer struct {
	mu        sync.Mutex
	failFirst map[uuid.UUID]bool
	attempts  map[uuid.UUID]int
	delivered map[uuid.UUID][]lnNotice
}

type lnNotice struct {
	recipients    []string
	subject, text string
}

func (m *lnMailer) SendLaunchNotice(_ context.Context, tenantID uuid.UUID, recipients []string, subject, text string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts[tenantID]++
	if m.failFirst[tenantID] && m.attempts[tenantID] == 1 {
		return false, errors.New("smtp: 451 try again later")
	}
	m.delivered[tenantID] = append(m.delivered[tenantID], lnNotice{recipients: recipients, subject: subject, text: text})
	return true, nil
}

// lnOrg is one organisation: who must be told, and the sites the notice must
// and must not name.
type lnOrg struct {
	id                   uuid.UUID
	told                 []string
	notTold              []string
	launchSites, others  []uuid.UUID
	launchNames, otherNs []string
}

// lnSeedSite seeds a site in the state m174's backfill leaves. Only the
// migration writes launch_default, so the seed runs with the triggers out of
// the way, on the bootstrap connection; everything under test runs as
// wpmgr_app.
func lnSeedSite(t *testing.T, pool, admin *db.Pool, tenant, enabledBy uuid.UUID, name, source string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	site := seedSite(t, pool, tenant, "")
	if err := admin.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		var err error
		switch source {
		case "launch_default":
			_, err = tx.Exec(ctx, `
UPDATE sites SET name = $3, content_editing_enabled_at = now(), content_editing_enabled_by = $2,
    content_editing_principal_user_id = 7,
    ai_mode = 'ai_drafts', ai_mode_source = 'launch_default', ai_mode_set_by = $2,
    ai_mode_set_at = now(), ai_mode_version = 1
WHERE id = $1`, site, enabledBy, name)
		case "person":
			_, err = tx.Exec(ctx, `
UPDATE sites SET name = $3, content_editing_enabled_at = now(), content_editing_enabled_by = $2,
    content_editing_principal_user_id = 7,
    ai_mode = 'ask', ai_mode_source = 'person', ai_mode_set_by = $2,
    ai_mode_set_at = now(), ai_mode_version = 2
WHERE id = $1`, site, enabledBy, name)
		default:
			_, err = tx.Exec(ctx, `UPDATE sites SET name = $2 WHERE id = $1`, site, name)
		}
		return err
	}); err != nil {
		t.Fatalf("seed site %s (%s): %v", name, source, err)
	}
	return site
}

func lnSeedOrg(t *testing.T, pool, admin *db.Pool, authRepo *auth.Repo, tag string) lnOrg {
	t.Helper()
	ctx := context.Background()
	o := lnOrg{id: seedTenant(t, pool, "ln-"+tag+"-"+uuid.NewString()[:8])}
	mail := func(who string) string { return who + "-" + tag + "-" + uuid.NewString()[:8] + "@example.com" }
	owner := seedUserMembership(t, authRepo, mail("owner"), o.id, authz.RoleOwner)
	adm := seedUserMembership(t, authRepo, mail("admin"), o.id, authz.RoleAdmin)
	op := seedUserMembership(t, authRepo, mail("operator"), o.id, authz.RoleOperator)
	off := seedUserMembership(t, authRepo, mail("disabled-admin"), o.id, authz.RoleAdmin)
	if _, err := admin.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, off.ID); err != nil {
		t.Fatalf("disable an admin: %v", err)
	}
	o.told = []string{owner.Email, adm.Email}
	sort.Strings(o.told)
	o.notTold = []string{op.Email, off.Email}
	for _, n := range []string{"Launch one " + tag, "Launch two " + tag} {
		o.launchSites = append(o.launchSites, lnSeedSite(t, pool, admin, o.id, owner.ID, n, "launch_default"))
		o.launchNames = append(o.launchNames, n)
	}
	o.others = append(o.others,
		lnSeedSite(t, pool, admin, o.id, owner.ID, "Chosen "+tag, "person"),
		lnSeedSite(t, pool, admin, o.id, owner.ID, "Off "+tag, "unset"))
	o.otherNs = []string{"Chosen " + tag, "Off " + tag}
	return o
}

// lnStamped reads, as wpmgr_app in the organisation's own transaction,
// whether each site carries the notice's stamp.
func lnStamped(t *testing.T, pool *db.Pool, tenant uuid.UUID, sites []uuid.UUID) []bool {
	t.Helper()
	ctx := context.Background()
	out := make([]bool, 0, len(sites))
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (launch notice stamps)")
		for _, s := range sites {
			var stamped bool
			if err := tx.QueryRow(ctx, `SELECT ai_mode_launch_emailed_at IS NOT NULL FROM sites WHERE tenant_id = $1 AND id = $2`,
				tenant, s).Scan(&stamped); err != nil {
				return err
			}
			out = append(out, stamped)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the notice stamps: %v", err)
	}
	return out
}

func lnAll(bs []bool, want bool) bool {
	for _, b := range bs {
		if b != want {
			return false
		}
	}
	return len(bs) > 0
}

// TestLaunchNoticeEmailsOncePerTenant (§11 test 6, N-b) runs the production
// notifier and repo as wpmgr_app over two organisations. Each organisation's
// active owners and admins get exactly one notice across three runs, naming
// only that organisation's launch-default sites; a notice whose send failed
// is released and sent by the next run; sites a person chose, or with AI
// editing off, are never named or stamped. Before any of that, a run with the
// notice held (WPMGR_AI_LAUNCH_NOTICE=off) stamps no site and sends nothing,
// so the runs with it on are the ones that tell each organisation.
//
// Mutation: list the organisations with no tenant or agent scope in the
// transaction; as wpmgr_app that list is empty under FORCE row security, and
// no organisation is told.
func TestLaunchNoticeEmailsOncePerTenant(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	authRepo := auth.NewRepo(pool)
	a := lnSeedOrg(t, pool, admin, authRepo, "a")
	b := lnSeedOrg(t, pool, admin, authRepo, "b")

	mail := &lnMailer{failFirst: map[uuid.UUID]bool{b.id: true}, attempts: map[uuid.UUID]int{}, delivered: map[uuid.UUID][]lnNotice{}}
	repo := aitrust.NewRepo(pool, audit.NewRecorder(pool, domain.SystemClock{}))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	held, err := aitrust.NewLaunchNotifier(repo, mail, "https://wpmgr.test", false, quiet).Run(ctx)
	if err != nil || held != (aitrust.LaunchNoticeRun{}) || len(mail.attempts) != 0 {
		t.Fatalf("held run %+v, err %v, %d organisations sent to; want nothing done", held, err, len(mail.attempts))
	}
	if !lnAll(lnStamped(t, pool, a.id, a.launchSites), false) || !lnAll(lnStamped(t, pool, b.id, b.launchSites), false) {
		t.Fatalf("a held run stamped a launch-default site")
	}

	notifier := aitrust.NewLaunchNotifier(repo, mail, "https://wpmgr.test", true, quiet)
	run1, err := notifier.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if run1.Tenants != 2 || run1.Sent != 1 || run1.Released != 1 || run1.Failed != 0 {
		t.Fatalf("first run %+v, want two organisations, one told and one released", run1)
	}
	if !lnAll(lnStamped(t, pool, a.id, a.launchSites), true) || !lnAll(lnStamped(t, pool, b.id, b.launchSites), false) {
		t.Fatalf("after the first run: A's sites should be stamped and B's released")
	}

	run2, err := notifier.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if run2.Tenants != 1 || run2.Sent != 1 {
		t.Fatalf("second run %+v, want B told", run2)
	}
	run3, err := notifier.Run(ctx)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if run3 != (aitrust.LaunchNoticeRun{}) {
		t.Fatalf("third run %+v, want nothing left to tell", run3)
	}

	if mail.attempts[a.id] != 1 || mail.attempts[b.id] != 2 {
		t.Fatalf("send attempts A=%d B=%d, want 1 and 2", mail.attempts[a.id], mail.attempts[b.id])
	}
	for _, o := range []struct {
		name       string
		org, other lnOrg
	}{{"A", a, b}, {"B", b, a}} {
		got := mail.delivered[o.org.id]
		if len(got) != 1 {
			t.Fatalf("organisation %s told %d times, want once", o.name, len(got))
		}
		n := got[0]
		rec := append([]string(nil), n.recipients...)
		sort.Strings(rec)
		if strings.Join(rec, ",") != strings.Join(o.org.told, ",") {
			t.Fatalf("organisation %s told %v, want its active owner and admin %v", o.name, rec, o.org.told)
		}
		if n.subject != "AI drafts now run without asking on 2 of your sites" {
			t.Fatalf("organisation %s subject %q", o.name, n.subject)
		}
		for i, s := range o.org.launchSites {
			if !strings.Contains(n.text, o.org.launchNames[i]) || !strings.Contains(n.text, "https://wpmgr.test/sites/"+s.String()+"/content") {
				t.Fatalf("organisation %s notice does not name %s with its link:\n%s", o.name, o.org.launchNames[i], n.text)
			}
		}
		for _, name := range append(append([]string{}, o.org.otherNs...), append(o.other.launchNames, o.other.otherNs...)...) {
			if strings.Contains(n.text, name) {
				t.Fatalf("organisation %s notice names %q, which it must not:\n%s", o.name, name, n.text)
			}
		}
		if !lnAll(lnStamped(t, pool, o.org.id, o.org.others), false) {
			t.Fatalf("organisation %s: a site not on the launch default was stamped", o.name)
		}
	}
	t.Logf("runs: %+v %+v %+v; attempts A=%d B=%d", run1, run2, run3, mail.attempts[a.id], mail.attempts[b.id])
}
