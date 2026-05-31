package tests

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestSiteFirstEnrollHappyPath: MintEnrollmentCode creates a pending_enrollment
// site + a site-bound code; consuming it (via the public enroll branch)
// transitions THAT site to connected under the same site_id (no second row).
func TestSiteFirstEnrollHappyPath(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "conn-happy")

	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), rec, nil, domain.SystemClock{}, nil)

	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	svc.SetConnectionService(conn)

	code, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
		TenantID: tenant,
		URL:      "https://site-first.example.com",
		Name:     "Site First",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if code.SiteID == uuid.Nil || code.Plaintext == "" {
		t.Fatalf("expected a site_id + plaintext, got %+v", code)
	}

	// The site exists in pending_enrollment before any enroll.
	pending, err := svc.Get(ctx, tenant, code.SiteID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.ConnectionState != site.StatePendingEnrollment {
		t.Fatalf("expected pending_enrollment, got %s", pending.ConnectionState)
	}

	// Consume via the public enroll branch (site-bound path).
	_, _, pubB64 := genKey(t)
	s, err := svc.Enroll(ctx, site.EnrollRequest{
		PairingCode:    code.Plaintext,
		SiteURL:        "https://site-first.example.com",
		AgentPublicKey: pubB64,
		WPVersion:      "6.5",
	})
	if err != nil {
		t.Fatalf("enroll (site-bound): %v", err)
	}
	// Same site_id (no second row), now connected with the agent key.
	if s.ID != code.SiteID {
		t.Fatalf("site-first enroll created a new row: got %s want %s", s.ID, code.SiteID)
	}
	if s.ConnectionState != site.StateConnected {
		t.Fatalf("expected connected, got %s", s.ConnectionState)
	}
	if s.AgentPublicKey != pubB64 {
		t.Fatalf("agent key not stored")
	}

	// The default list still shows the site (connected, not archived).
	list, err := svc.List(ctx, site.ListInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == code.SiteID {
			found = true
		}
	}
	if !found {
		t.Fatal("connected site missing from default list")
	}
}

// TestConsumeSiteBoundCode_ExactlyOneWinner proves the atomic single-use consume
// invariant: two goroutines racing to consume the SAME site-bound code → exactly
// one succeeds, the other is rejected (the conditional UPDATE is the lock).
func TestConsumeSiteBoundCode_ExactlyOneWinner(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "conn-race")

	repo := site.NewRepo(pool)
	conn := site.NewConnectionService(repo, domain.NewValidator(),
		audit.NewRecorder(pool, domain.SystemClock{}), nil, domain.SystemClock{}, nil)

	code, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
		TenantID: tenant,
		URL:      "https://race.example.com",
		Name:     "Race",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Two distinct valid keys so the agent_public_key unique index is not what
	// serializes the two callers — the consume UPDATE must be.
	_, _, key1 := genKey(t)
	_, _, key2 := genKey(t)
	keys := []string{key1, key2}

	codeHash := site.HashPairingCodeForTest(code.Plaintext)

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, results[idx] = conn.ConsumeEnrollmentCode(ctx, site.ConsumeEnrollmentInput{
				CodeHash:       codeHash,
				AgentPublicKey: keys[idx],
				SiteURL:        "https://race.example.com",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, e := range results {
		if e == nil {
			wins++
		} else if de, ok := domain.AsDomain(e); !ok || (de.Kind != domain.KindConflict && de.Kind != domain.KindUnauthorized) {
			t.Fatalf("loser got an unexpected error kind: %v", e)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d (results: %v)", wins, results)
	}
}
