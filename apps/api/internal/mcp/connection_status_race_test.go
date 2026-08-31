// connection_status_race_test.go -- the regression proof for
// Repo.ConnectionStatusSnapshot's STATEMENT ORDER.
//
// ===========================================================================
// WHY THIS FILE IS IN internal/mcp AND NOT IN tests/
// ===========================================================================
//
// The seam under test is BETWEEN TWO STATEMENTS INSIDE ONE TRANSACTION.
// getGrantTx and findFirstToolCallTx are unexported, and the ordering that
// makes the pair consistent is invisible from outside the package: through the
// HTTP surface a caller sees one JSON body and cannot say which read produced
// which half of it. package tests can only assert the endpoint's output, which
// is exactly the weaker test that would pass against the broken code whenever
// the race did not happen to be provoked.
//
// ===========================================================================
// WHY THIS IS NOT A TEST ABOUT ATOMICITY, AND WHY ONE WOULD BE A FAKE PROOF
// ===========================================================================
//
// The obvious reading of the fix is "the two reads now share a transaction, so
// they are consistent". They are not, and asserting that would pin a property
// the code does not have. Every helper in internal/db opens with p.Begin --
// there is no BeginTx and no pgx.TxOptions anywhere in the package, so every
// transaction is READ COMMITTED, and under READ COMMITTED EACH STATEMENT TAKES
// ITS OWN SNAPSHOT. Two statements in one transaction straddle a concurrent
// commit exactly as two transactions did. The shared transaction buys the
// shared RLS GUCs; it does not buy a shared instant.
//
// What makes the pair consistent is ORDER. mcp_grants.client_identity_recorded_
// at is stamped by RecordClientIdentity and never written back to NULL, so the
// handshake fact is monotonic and always at least as old as a tool call's audit
// row. Reading the LATER fact first (the audit scan) and the EARLIER fact last
// (the grant) means anything committing in between can only make the handshake
// MORE advanced. So this file provokes a real concurrent commit at the seam and
// asserts the PAIR BY VALUE.
//
// ===========================================================================
// HOW THE COMMIT IS PLACED AT THE SEAM WITHOUT A SLEEP OR A GOROUTINE
// ===========================================================================
//
// A timing-dependent test would be worse than no test: it would go green on a
// fast machine against the broken code. So there is no sleep, no goroutine and
// no scheduler dependency here.
//
// Instead a pgx.QueryTracer counts the CONTENT statements the snapshot
// transaction issues (the mcp_grants reads and the audit_log scan; BEGIN,
// COMMIT and the set_config GUC statements are not content and are not
// counted) and performs the concurrent write synchronously in TraceQueryEnd of
// the SECOND one, on a different pooled connection, before the third statement
// is ever sent. The injection point is POSITIONAL -- "after content statement
// 2" -- and that is deliberate, because it is what makes the mutation redden:
//
//	CORRECT ORDER   [gate grant, audit scan, grant]
//	                write lands after the scan, so the scan saw no call and the
//	                grant read that follows sees the handshake => connected +
//	                awaiting_call. A REAL state.
//
//	SWAPPED (BUG)   [gate grant, grant, audit scan]
//	                write lands after the grant read, so the handshake is stale
//	                and the scan that follows sees the call => awaiting_client +
//	                succeeded. THE IMPOSSIBLE PAIR.
//
// The tracer observes; it never reorders. The statements are the production
// ones, issued by the production function, on a pool connected as wpmgr_app.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/tests/testinfra"
)

// ---------------------------------------------------------------------------
// The statement tracer -- the deterministic seam.
// ---------------------------------------------------------------------------

// stmtKind is what a traced statement was about. Only these two are content;
// everything else the transaction issues (begin, commit, the set_config calls
// that install the RLS GUCs) is infrastructure and is not counted, because
// counting it would make the injection point depend on how many GUCs the
// dispatch helper happens to set.
type stmtKind string

const (
	stmtGrant stmtKind = "grant" // a single-row read of mcp_grants
	stmtAudit stmtKind = "audit" // the bounded audit_log scan
)

// seamTracer records the ORDER of content statements and fires a callback
// immediately after the Nth of them completes.
//
// It is a pgx.QueryTracer, so it sits on the real connection and sees the real
// statements the real repo issues. It changes nothing about them.
type seamTracer struct {
	mu sync.Mutex

	// recording is off until the test arms it, so the container bootstrap, the
	// migrations and the seeds are not counted.
	recording bool
	seen      []stmtKind

	// injectAfter is the 1-based content-statement position after which inject
	// runs. Zero disables injection (used by the 404 proof, which only needs
	// the recording).
	injectAfter int
	inject      func()
	injected    bool
}

// seamKindKey carries the classification from TraceQueryStart to
// TraceQueryEnd. pgx.TraceQueryEndData exposes only the CommandTag and the
// error -- the SQL text is available at START only -- and the context returned
// by TraceQueryStart is the one handed back to TraceQueryEnd, which is the
// documented channel for exactly this.
//
// The injection must happen at END and not at START: the statement whose
// position we are counting has to have COMPLETED before the concurrent write
// commits, or the write would land in the middle of the read it is supposed to
// follow.
type seamKindKey struct{}

func (s *seamTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	kind, ok := classifyStatement(data.SQL)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, seamKindKey{}, kind)
}

func (s *seamTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	kind, ok := ctx.Value(seamKindKey{}).(stmtKind)
	if !ok {
		return
	}

	s.mu.Lock()
	if !s.recording {
		s.mu.Unlock()
		return
	}
	s.seen = append(s.seen, kind)
	n := len(s.seen)
	fire := s.inject != nil && !s.injected && s.injectAfter > 0 && n == s.injectAfter
	if fire {
		// Disarm BEFORE running, and disable recording for the duration. The
		// injected write issues its own statements on another connection of the
		// same pool, and this tracer would otherwise see them, count them, and
		// re-enter itself.
		s.injected = true
		s.recording = false
	}
	s.mu.Unlock()

	if !fire {
		return
	}
	s.inject()
	s.mu.Lock()
	s.recording = true
	s.mu.Unlock()
}

// arm starts recording. injectAfter is the 1-based content-statement position
// after which inject fires; pass 0 and nil to record only.
func (s *seamTracer) arm(injectAfter int, inject func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recording, s.seen, s.injectAfter, s.inject, s.injected = true, nil, injectAfter, inject, false
}

func (s *seamTracer) disarm() []stmtKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recording = false
	out := make([]stmtKind, len(s.seen))
	copy(out, s.seen)
	return out
}

func (s *seamTracer) didInject() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.injected
}

// classifyStatement decides whether a SQL string is one of the two content
// reads. It matches on the TABLE rather than on a generated query name, because
// sqlc's emitted SQL is what reaches the wire.
func classifyStatement(sql string) (stmtKind, bool) {
	lowered := strings.ToLower(sql)
	if !strings.Contains(lowered, "select") {
		return "", false
	}
	switch {
	case strings.Contains(lowered, "audit_log"):
		return stmtAudit, true
	case strings.Contains(lowered, "mcp_grants"):
		return stmtGrant, true
	default:
		// begin / commit / set_config / pool health checks.
		return "", false
	}
}

func kindsString(k []stmtKind) string {
	parts := make([]string, len(k))
	for i, v := range k {
		parts[i] = string(v)
	}
	return "[" + strings.Join(parts, " -> ") + "]"
}

// ---------------------------------------------------------------------------
// The harness -- Postgres, real migrations, real wpmgr_app role, plus a tracer.
// ---------------------------------------------------------------------------

// startTracedPostgres mirrors tests.startPostgres -- ephemeral Postgres, the
// embedded migrations applied as the bootstrap superuser, then a pool connected
// as the NON-SUPERUSER, NON-BYPASSRLS wpmgr_app role -- and additionally
// installs a QueryTracer on the application pool.
//
// The role is not a detail. Under a superuser or a BYPASSRLS role every policy
// on mcp_grants is inert and this proof would pass having tested nothing, which
// is precisely how m112's proofs passed while the email domain was cross-site
// readable. assertAppRoleForSnapshot prints the role from pg_roles INSIDE the
// transaction under test.
//
// db.Pool is `struct{ *pgxpool.Pool }` with an exported embedded field, so the
// traced pool is a real db.Pool and every helper in internal/db -- including
// RunTenantTx, the dispatch the production read uses -- works on it unchanged.
func startTracedPostgres(t *testing.T) (*db.Pool, *seamTracer) {
	t.Helper()
	ctx := context.Background()

	testinfra.SkipIfDockerUnavailable(t, ctx, "postgres")

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	// Registered BEFORE err is inspected: tcpostgres.Run can return a non-nil
	// container alongside a non-nil error on a partial start, and SetupFatalf
	// calls t.Fatalf, which never returns.
	if container != nil {
		t.Cleanup(func() { _ = container.Terminate(ctx) })
	}
	if err != nil {
		testinfra.SetupFatalf(t, err, "postgres: container start")
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		testinfra.SetupFatalf(t, err, "postgres: connection string")
	}

	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		testinfra.SetupFatalf(t, err, "postgres: connect as bootstrap superuser")
	}
	if err := adminPool.Migrate(ctx); err != nil {
		testinfra.SetupFatalf(t, err, "postgres: run embedded migrations")
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		// audit_log is append-only in production. Re-revoking is what stops
		// this test running with privileges no real install has.
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON org_context_versions FROM wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON site_context_versions FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			testinfra.SetupFatalf(t, err, "postgres: provision app role ("+stmt+")")
		}
	}
	adminPool.Close()

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	cfg, err := pgxpool.ParseConfig(appDSN)
	if err != nil {
		testinfra.SetupFatalf(t, err, "postgres: parse app dsn")
	}
	tracer := &seamTracer{}
	cfg.ConnConfig.Tracer = tracer
	// More than one connection is required: the injected write must commit on a
	// DIFFERENT connection while the snapshot transaction is still open.
	cfg.MinConns = 2
	cfg.MaxConns = 6

	raw, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		testinfra.SetupFatalf(t, err, "postgres: connect as wpmgr_app")
	}
	pool := &db.Pool{Pool: raw}
	t.Cleanup(pool.Close)

	return pool, tracer
}

// assertAppRoleForSnapshot prints and enforces the connection's own role from
// inside the transaction under test.
func assertAppRoleForSnapshot(t *testing.T, tx pgx.Tx, where string) {
	t.Helper()
	var role string
	var super, bypass bool
	if err := tx.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls
		   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("%s: read the connection's own role: %v", where, err)
	}
	t.Logf("%s: current_user=%s rolsuper=%t rolbypassrls=%t", where, role, super, bypass)
	if super || bypass {
		t.Fatalf("%s: running as %q with rolsuper=%v rolbypassrls=%v; either one bypasses "+
			"every policy and this proof would pass without testing anything",
			where, role, super, bypass)
	}
	if role != "wpmgr_app" {
		t.Fatalf("%s: running as %q, not wpmgr_app, which is the role every real install "+
			"connects as", where, role)
	}
}

// ---------------------------------------------------------------------------
// Seeds
// ---------------------------------------------------------------------------

func seedSnapshotTenant(t *testing.T, pool *db.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id", slug, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedSnapshotUser(t *testing.T, pool *db.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO users (email) VALUES ($1) RETURNING id", email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedSnapshotGrant inserts one active grant through RunTenantTx -- the same
// dispatch the production write uses, so the RESTRICTIVE
// mcp_grants_site_scope_insert policy is live for the INSERT too.
func seedSnapshotGrant(t *testing.T, pool *db.Pool, p domain.Principal, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.RunTenantTx(context.Background(), p, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateMCPGrant(context.Background(), sqlc.CreateMCPGrantParams{
			TenantID:            p.TenantID,
			Name:                name,
			Status:              string(GrantStatusActive),
			SiteScopeMode:       string(SiteScopeModeAll),
			ScopeTagIds:         []uuid.UUID{},
			ScopeSiteIds:        []uuid.UUID{},
			ClientID:            nil,
			CreatedByUserID:     uuidToPG(p.UserID),
			Capabilities:        capabilityNames(DefaultGrantCapabilities()),
			ExpiresAt:           time.Now().UTC().Add(grantAbsoluteTTL),
			IdleExpireAfterDays: nil,
			SetupClient:         nil,
		})
		if err != nil {
			return err
		}
		id = row.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// PROOF 1 -- a handshake and a tool call committing AT THE SEAM cannot produce
// awaiting_client + succeeded.
// ---------------------------------------------------------------------------

func TestConnectionStatusSnapshot_ConcurrentInitializeCannotProduceTheImpossiblePair(t *testing.T) {
	ctx := context.Background()
	pool, tracer := startTracedPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantID := seedSnapshotTenant(t, pool, "mcp-seam-"+suffix)
	userID := seedSnapshotUser(t, pool, "mcp-seam-"+suffix+"@example.test")
	admin := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}

	grantID := seedSnapshotGrant(t, pool, admin, "seam "+suffix)

	repo := NewRepo(pool)
	svc := NewService(repo).WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	// The transport's own two writes, in the transport's own order. These are
	// the calls TransportHandler makes at transport.go:432 (initialize) and
	// transport.go:597 (a successful tools/call). A NORMAL INITIALIZE COMES
	// FIRST -- the separately-filed "a client can skip initialize" gap is not
	// what this test is about, and constructing the case without the
	// initialize would confuse the two.
	auth := AuthorizedRequest{TenantID: tenantID, GrantID: grantID, GrantName: "seam " + suffix}
	protocol := ProtocolTarget
	var injectErr error
	inject := func() {
		if err := svc.RecordConnect(ctx, auth, "Claude Desktop", "1.2.3", &protocol); err != nil {
			injectErr = fmt.Errorf("record connect: %w", err)
			return
		}
		if err := svc.RecordToolCall(ctx, auth, "list_sites", "read"); err != nil {
			injectErr = fmt.Errorf("record tool call: %w", err)
		}
	}

	// Fire after the SECOND content statement, whichever read that turns out to
	// be. See this file's header for why the position and not the identity is
	// what catches the swap.
	tracer.arm(2, inject)

	snap, err := repo.ConnectionStatusSnapshot(ctx, admin, grantID, firstCallScanLimit)
	order := tracer.disarm()
	if err != nil {
		t.Fatalf("ConnectionStatusSnapshot: %v", err)
	}
	if injectErr != nil {
		t.Fatalf("the concurrent handshake/tool-call did not commit: %v", injectErr)
	}

	// The seam must actually have been reached. Without this the whole test
	// could pass having provoked nothing -- a guard that finds nothing going
	// green is this repository's signature defect.
	if !tracer.didInject() {
		t.Fatalf("the concurrent write never fired: the snapshot issued %s, fewer than 2 "+
			"content statements. This test proved NOTHING", kindsString(order))
	}
	if len(order) != 3 {
		t.Fatalf("the snapshot issued %s, want exactly 3 content statements "+
			"(gate grant, audit scan, grant)", kindsString(order))
	}
	t.Logf("statement order under test: %s, concurrent commit injected after #2", kindsString(order))

	// Confirm the concurrent write really landed and is visible afterwards --
	// otherwise "no impossible pair" could just mean "nothing happened".
	var recordedAt *time.Time
	if err := pool.RunTenantTx(ctx, admin, func(tx pgx.Tx) error {
		g, err := getGrantTx(ctx, tx, tenantID, grantID)
		if err != nil {
			return err
		}
		recordedAt = timestamptzTimeOrNil(g.ClientIdentityRecordedAt)
		return nil
	}); err != nil {
		t.Fatalf("re-read the grant after the injected commit: %v", err)
	}
	if recordedAt == nil {
		t.Fatal("the injected initialize did not stamp client_identity_recorded_at, so no " +
			"handshake was ever concurrent with the read and this test proved nothing")
	}

	// ===================================================================
	// THE ASSERTION. Both halves BY VALUE, together. Asserting that the two
	// fields are merely present would pass against the broken version.
	// ===================================================================
	gotHandshake := handshakeFromGrant(snap.Grant).State
	gotFirstCall := firstCallFrom(snap.FirstCall, timestamptzTimeOrNil(snap.Grant.LastUsedAt)).State

	if gotHandshake == HandshakeAwaitingClient && gotFirstCall == FirstCallSucceeded {
		t.Fatalf("THE IMPOSSIBLE PAIR: handshake.state=%q with first_call.state=%q. "+
			"Nothing can call a tool before it initializes, so this pair is a "+
			"contradiction the wizard would render as a broken connection that is in "+
			"fact working. The snapshot read the EARLIER fact (the grant) before the "+
			"LATER fact (the audit scan), so a commit between them made the handshake "+
			"stale. Statement order was %s; steps 2 and 3 of "+
			"Repo.ConnectionStatusSnapshot are the wrong way round",
			gotHandshake, gotFirstCall, kindsString(order))
	}

	// And the pair must be the specific real state this interleaving produces,
	// not merely "not the impossible one": the scan ran before the write so it
	// saw no call, and the grant was read after it so the handshake is fresh.
	if gotHandshake != HandshakeConnected || gotFirstCall != FirstCallAwaiting {
		t.Fatalf("pair = (%q, %q), want (%q, %q). The write was injected between the "+
			"audit scan and the grant read, so the scan must miss the call and the "+
			"grant read must see the handshake. Statement order was %s",
			gotHandshake, gotFirstCall,
			HandshakeConnected, FirstCallAwaiting, kindsString(order))
	}
	t.Logf("PROOF 1 ok: a handshake and a tool call committing at the seam yielded "+
		"(%q, %q) -- a real state", gotHandshake, gotFirstCall)
}

// ---------------------------------------------------------------------------
// PROOF 2 -- the fix did not cost the cheap 404.
//
// The grant is read TWICE on purpose, and the first read is an existence-and-
// RLS gate. Without it, a poll for an absent or another organisation's grant id
// would run the bounded audit scan -- up to firstCallScanLimit+1 rows -- before
// discovering it had nothing to answer about. That turns a cheap 404 into an
// audit-log scan ANY AUTHENTICATED ORG PRINCIPAL COULD TRIGGER BY GUESSING IDS.
//
// The property is asserted on the STATEMENTS ISSUED, not on the response: a
// test that only checked for pgx.ErrNoRows would pass whether or not the scan
// ran.
// ---------------------------------------------------------------------------

func TestConnectionStatusSnapshot_UnknownGrantDoesNotRunTheAuditScan(t *testing.T) {
	ctx := context.Background()
	pool, tracer := startTracedPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantID := seedSnapshotTenant(t, pool, "mcp-404-"+suffix)
	userID := seedSnapshotUser(t, pool, "mcp-404-"+suffix+"@example.test")
	admin := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}

	// Prove the role inside the very transaction the read opens.
	if err := pool.RunTenantTx(ctx, admin, func(tx pgx.Tx) error {
		assertAppRoleForSnapshot(t, tx, "RunTenantTx (ConnectionStatusSnapshot read path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	repo := NewRepo(pool)

	// Record only; nothing is injected.
	tracer.arm(0, nil)
	_, err := repo.ConnectionStatusSnapshot(ctx, admin, uuid.New(), firstCallScanLimit)
	order := tracer.disarm()

	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an unknown grant id returned %v, want pgx.ErrNoRows verbatim so the "+
			"service can 404 on it", err)
	}
	if len(order) == 0 {
		t.Fatalf("the snapshot issued NO content statements for an unknown id; the tracer " +
			"saw nothing, so this test cannot distinguish a working gate from a broken " +
			"one and proves nothing")
	}
	for i, k := range order {
		if k == stmtAudit {
			t.Fatalf("a poll for an UNKNOWN grant id ran the bounded audit scan "+
				"(statement #%d of %s). The existence-and-RLS gate is gone or has moved "+
				"after the scan, so any authenticated org principal can make the server "+
				"read up to %d audit_log rows by guessing ids, and every 404 now costs "+
				"an audit-log scan", i+1, kindsString(order), firstCallScanLimit+1)
		}
	}
	if len(order) != 1 {
		t.Fatalf("an unknown grant id issued %s, want exactly one statement -- the gate "+
			"read that produces the 404", kindsString(order))
	}
	t.Logf("PROOF 2 ok: an unknown grant id issued %s and no audit scan", kindsString(order))
}

// ---------------------------------------------------------------------------
// PROOF 3 -- the existing states still behave, read through the same ordered
// snapshot. Awaiting, connected, and BOTH protocol variants.
//
// These are the states the reordering must not have disturbed. They are
// asserted through Repo.ConnectionStatusSnapshot rather than through a fake, so
// the columns, the RLS policies and the derivation are all the real ones.
// ---------------------------------------------------------------------------

func TestConnectionStatusSnapshot_ExistingStatesSurviveTheReordering(t *testing.T) {
	ctx := context.Background()
	pool, tracer := startTracedPostgres(t)
	tracer.arm(0, nil)

	suffix := uuid.NewString()[:8]
	tenantID := seedSnapshotTenant(t, pool, "mcp-states-"+suffix)
	userID := seedSnapshotUser(t, pool, "mcp-states-"+suffix+"@example.test")
	admin := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}

	repo := NewRepo(pool)
	svc := NewService(repo).WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	target := ProtocolTarget
	unrecognised := "1999-01-01"

	cases := []struct {
		name string
		// protocol is the MCP-Protocol-Version header value the client sent.
		// nil means it connected and sent NO header, which is a different fact
		// from never having connected (connect=false).
		connect       bool
		protocol      *string
		wantHandshake HandshakeState
	}{
		{
			name:          "never connected",
			connect:       false,
			wantHandshake: HandshakeAwaitingClient,
		},
		{
			name:          "connected with a revision we speak",
			connect:       true,
			protocol:      &target,
			wantHandshake: HandshakeConnected,
		},
		{
			name:          "connected sending no version header at all",
			connect:       true,
			protocol:      nil,
			wantHandshake: HandshakeConnectedProtocolAssumed,
		},
		{
			name:          "connected with a revision we no longer speak",
			connect:       true,
			protocol:      &unrecognised,
			wantHandshake: HandshakeConnectedProtocolUnrecognised,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grantID := seedSnapshotGrant(t, pool, admin, tc.name+" "+suffix)
			auth := AuthorizedRequest{TenantID: tenantID, GrantID: grantID, GrantName: tc.name}

			if tc.connect {
				if err := svc.RecordConnect(ctx, auth, "Claude Desktop", "1.2.3", tc.protocol); err != nil {
					t.Fatalf("record connect: %v", err)
				}
			}

			snap, err := repo.ConnectionStatusSnapshot(ctx, admin, grantID, firstCallScanLimit)
			if err != nil {
				t.Fatalf("ConnectionStatusSnapshot: %v", err)
			}

			gotHandshake := handshakeFromGrant(snap.Grant).State
			gotFirstCall := firstCallFrom(snap.FirstCall, timestamptzTimeOrNil(snap.Grant.LastUsedAt)).State

			// Both halves, by value, together -- the same bar as PROOF 1. No
			// tool call has happened in any of these cases, so the first-call
			// half must be the NOT-YET in every one of them.
			if gotHandshake != tc.wantHandshake || gotFirstCall != FirstCallAwaiting {
				t.Fatalf("pair = (%q, %q), want (%q, %q)",
					gotHandshake, gotFirstCall, tc.wantHandshake, FirstCallAwaiting)
			}

			// A tool call now happens, with no concurrency at all: the ordinary
			// connected+succeeded state the wizard's success frame renders.
			if !tc.connect {
				return
			}
			if err := svc.RecordToolCall(ctx, auth, "list_sites", "read"); err != nil {
				t.Fatalf("record tool call: %v", err)
			}
			snap, err = repo.ConnectionStatusSnapshot(ctx, admin, grantID, firstCallScanLimit)
			if err != nil {
				t.Fatalf("ConnectionStatusSnapshot after the tool call: %v", err)
			}
			gotHandshake = handshakeFromGrant(snap.Grant).State
			gotFirstCall = firstCallFrom(snap.FirstCall, timestamptzTimeOrNil(snap.Grant.LastUsedAt)).State
			if gotHandshake != tc.wantHandshake || gotFirstCall != FirstCallSucceeded {
				t.Fatalf("after a tool call, pair = (%q, %q), want (%q, %q)",
					gotHandshake, gotFirstCall, tc.wantHandshake, FirstCallSucceeded)
			}
			if snap.FirstCall.ToolName != "list_sites" {
				t.Errorf("first call tool_name = %q, want %q", snap.FirstCall.ToolName, "list_sites")
			}
		})
	}
}
