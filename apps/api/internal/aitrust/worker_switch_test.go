package aitrust

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// These tests drive the launch notice through the River worker, the path a
// running control plane takes, under each value of WPMGR_AI_LAUNCH_NOTICE as
// config.AIConfig.LaunchNoticeOn reads it.

// TestLaunchNoticeHeldClaimsAndSendsNothing proves off: held runs touch
// neither the store nor the mailer, and every site keeps waiting.
//
// Mutation: drop the held check in Run; the held runs then claim both
// organisations and send to each.
func TestLaunchNoticeHeldClaimsAndSendsNothing(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	store, mail := newNoticeStore(), newNoticeMailer()
	store.waiting[a] = noticeClaim(2, "owner-a@example.com")
	store.waiting[b] = noticeClaim(1, "owner-b@example.com")
	held := NewLaunchNoticeWorker(switchedNotifier(store, mail, false))
	for i := 0; i < 3; i++ {
		if err := held.Work(context.Background(), nil); err != nil {
			t.Fatalf("held run %d: %v", i+1, err)
		}
	}
	if store.lists != 0 || store.claims != 0 || len(store.releases) != 0 || len(mail.attempts) != 0 {
		t.Fatalf("held runs listed %d times, claimed %d times, released %d organisations and sent to %d; want none of it",
			store.lists, store.claims, len(store.releases), len(mail.attempts))
	}
	if len(store.waiting[a].Sites) != 2 || len(store.waiting[b].Sites) != 1 {
		t.Fatalf("after held runs A has %d sites waiting and B %d, want 2 and 1",
			len(store.waiting[a].Sites), len(store.waiting[b].Sites))
	}
}

// TestLaunchNoticeOnTellsEachOrganisationOnce proves on, the default: the
// first run tells each organisation once, and later runs send nothing more.
//
// Mutation: hold the notice whatever the setting says; no organisation is
// told.
func TestLaunchNoticeOnTellsEachOrganisationOnce(t *testing.T) {
	orgs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	store, mail := newNoticeStore(), newNoticeMailer()
	for i, o := range orgs {
		store.waiting[o] = noticeClaim(i+1, fmt.Sprintf("owner-%d@example.com", i))
	}
	on := NewLaunchNoticeWorker(switchedNotifier(store, mail, true))
	for i := 0; i < 3; i++ {
		if err := on.Work(context.Background(), nil); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	for i, o := range orgs {
		if len(mail.delivered[o]) != 1 || mail.attempts[o] != 1 {
			t.Fatalf("organisation %d: %d delivered in %d attempts, want one of one", i, len(mail.delivered[o]), mail.attempts[o])
		}
	}
	if store.claims != len(orgs) || len(store.releases) != 0 {
		t.Fatalf("claimed %d times and released %d organisations, want one claim each and no release", store.claims, len(store.releases))
	}
}

// TestLaunchNoticeSwitchedOnLaterSendsThen proves a held notice is not lost:
// after held runs, the notifier the next boot builds with the setting on
// tells each organisation once, and a run after that sends nothing more.
//
// Mutation: hold by claiming and not sending (the held check after the claim
// in notifyTenant); the sites stay claimed and the run after the switch has
// nothing to send.
func TestLaunchNoticeSwitchedOnLaterSendsThen(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	store, mail := newNoticeStore(), newNoticeMailer()
	store.waiting[a] = noticeClaim(1, "owner-a@example.com")
	store.waiting[b] = noticeClaim(3, "owner-b@example.com", "admin-b@example.com")
	held := NewLaunchNoticeWorker(switchedNotifier(store, mail, false))
	for i := 0; i < 2; i++ {
		if err := held.Work(context.Background(), nil); err != nil {
			t.Fatalf("held run %d: %v", i+1, err)
		}
	}
	if len(mail.attempts) != 0 {
		t.Fatalf("held runs sent to %d organisations, want none", len(mail.attempts))
	}

	on := switchedNotifier(store, mail, true)
	first, err := on.Run(context.Background())
	if err != nil {
		t.Fatalf("first run with the notice on: %v", err)
	}
	if first != (LaunchNoticeRun{Tenants: 2, Sent: 2}) {
		t.Fatalf("first run with the notice on %+v, want both organisations told", first)
	}
	again, err := on.Run(context.Background())
	if err != nil {
		t.Fatalf("second run with the notice on: %v", err)
	}
	if again != (LaunchNoticeRun{}) {
		t.Fatalf("second run with the notice on %+v, want nothing left to tell", again)
	}
	if len(mail.delivered[a]) != 1 || len(mail.delivered[b]) != 1 {
		t.Fatalf("delivered %d to A and %d to B, want one each", len(mail.delivered[a]), len(mail.delivered[b]))
	}
}
