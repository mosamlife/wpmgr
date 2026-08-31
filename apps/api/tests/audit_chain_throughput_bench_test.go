package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// BenchmarkAuditChain measures the throughput ceiling of the per-tenant audit
// chain lock, which ADR-061 A10 requires to be a measured number rather than
// an estimate ("The chain lock becomes a throughput ceiling, and it must be
// measured ... it is still a number somebody has to take, before production
// takes it for us").
//
// # What serialises
//
// Every append runs `SELECT pg_advisory_xact_lock(hashtext('wpmgr_audit:' ||
// $1))` inside the append's own transaction (internal/audit/audit.go:688,
// lockChain), taken by BOTH entry points — Record (audit.go:736, opens its own
// InTenantTx) and RecordInTx (audit.go:794, joins the caller's tx). The lock
// is transaction-scoped, so it is held from the moment it is taken until that
// transaction commits or rolls back. The work inside the lock is
// GetLastAuditHash (one indexed SELECT), a SHA-256 over a small canonical
// byte string, and one INSERT. The lock key is derived from the tenant id, so
// the serialisation is per tenant, and that is what "SameTenant" vs
// "SeparateTenants" below tests rather than assumes.
//
// # How to run it
//
//	cd apps/api && go test ./tests/ -run '^$' -bench BenchmarkAuditChain -benchtime 1000x -count 3 -timeout 20m -v
//
// That is the command behind Run B in the RESULTS block (about 50 s). Drop to
// -benchtime 300x -count 1 for a five-second look; that is Run A.
//
// -run '^$' excludes every ordinary test in the package (each starts its own
// container). A pinned -benchtime is not optional: Go's adaptive default picks
// a different b.N per sub-benchmark, which makes the degradation shape across
// writer counts unreadable. -count 3 is there because a single sample on a
// shared machine is not a measurement — see the LOAD note in RESULTS. It needs
// Docker: the whole benchmark shares ONE Postgres testcontainer, started once
// at the top.
//
// If another integration run holds the machine lock, run it under the same
// lock as the suite: scripts/with-machine-lock.sh api-integration -- sh -c '...'.
//
// # MEASURED RESULT — see the RESULTS block at the bottom of this file.
//
// The numbers, the machine they came from, what they do not cover, and the
// A10 interpretation are recorded there, next to the code that produced them,
// because that is where the next person looks.
func BenchmarkAuditChain(b *testing.B) {
	pool := startPostgres(b)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	// State the machine. A throughput number without the hardware and the
	// Postgres it ran against is not reproducible, and it is the first thing
	// anyone re-running this will want to compare against.
	var pgVersion string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&pgVersion); err != nil {
		b.Fatalf("read server_version: %v", err)
	}
	b.Logf("MACHINE: GOOS=%s GOARCH=%s NumCPU=%d GOMAXPROCS=%d",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	b.Logf("POSTGRES: server_version=%s, containerised (testcontainers postgres:16-alpine, same host)", pgVersion)
	b.Logf("POOL: MaxConns=%d (db.Connect leaves pgx's default; production's db.ConnectApp pins MaxConns=5 per instance)",
		pool.Stat().MaxConns())

	// The canonical hash input, with no database at all. This is the "what
	// does the chain itself cost" half of the question: if the full append is
	// three orders of magnitude slower than this, the hash is not the thing to
	// attack.
	b.Run("HashChainCPUOnly", func(b *testing.B) { benchHashCPU(b) })

	// Two decompositions of the DB half, both through pool.InTenantTx — the
	// same helper Record uses — so the difference between them is the lock and
	// nothing else.
	txTenant := seedTenant(b, pool, "audit-bench-tx-"+uuid.NewString()[:8])
	b.Run("TxOnly", func(b *testing.B) { benchTxOnly(b, pool, txTenant, false) })
	b.Run("TxPlusChainLock", func(b *testing.B) { benchTxOnly(b, pool, txTenant, true) })

	// The headline: one writer, one tenant, the real Record path.
	b.Run("Record/SameTenant/writers=1", func(b *testing.B) {
		benchRecordSameTenant(b, pool, rec, 1)
	})

	// Degradation shape under concurrent writers on ONE tenant. The shape is
	// the point: linear queueing (aggregate appends/s flat, per-op latency
	// rising in proportion to the writer count) is a healthy serialisation
	// point; a cliff is not.
	for _, w := range []int{2, 4, 8} {
		b.Run(fmt.Sprintf("Record/SameTenant/writers=%d", w), func(b *testing.B) {
			benchRecordSameTenant(b, pool, rec, w)
		})
	}

	// The same writer counts spread across DISTINCT tenants. If the lock is
	// genuinely per-tenant these should scale where SameTenant does not. If
	// they do not scale, that is a finding, not a number.
	for _, w := range []int{2, 4, 8} {
		b.Run(fmt.Sprintf("Record/SeparateTenants/writers=%d", w), func(b *testing.B) {
			benchRecordSeparateTenants(b, pool, rec, w)
		})
	}
}

// benchHashCPU measures the pure CPU cost of one chain link: the JSON encode
// of the metadata map, the canonical field concatenation, and the SHA-256.
//
// This MIRRORS internal/audit/audit.go's canonical() (audit.go:653) and
// hashHex() (audit.go:677); it does not call them, because both are unexported
// and this is an external test package. It is here to bound the cost of the
// hashing half against the database half, and it is only accurate as a bound
// while it keeps the same shape as canonical. If canonical grows a field, this
// number changes only when someone updates this mirror — which is why the
// interpretation below leans on the ORDER of magnitude between the two halves,
// not on this figure to three significant digits.
func benchHashCPU(b *testing.B) {
	e := benchEvent(0)
	tenant := uuid.New()
	prev := "9f2c" + hex.EncodeToString(make([]byte, 30))
	createdAt := time.Now().UTC().Truncate(time.Microsecond)

	var sink string
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		metaJSON, err := json.Marshal(e.Metadata)
		if err != nil {
			b.Fatalf("marshal: %v", err)
		}
		s := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s",
			prev, tenant.String(), e.ActorType, e.ActorID, e.Action,
			e.TargetType, e.TargetID, string(metaJSON),
			createdAt.Format(time.RFC3339Nano))
		sum := sha256.Sum256([]byte(s))
		sink = hex.EncodeToString(sum[:])
	}
	b.StopTimer()

	// Make the work observable. Without this the loop above is dead code the
	// compiler is entitled to delete, and Go would happily report a number for
	// a benchmark that computed nothing.
	if len(sink) != 64 {
		b.Fatalf("hash did not run: got %d hex chars, want 64", len(sink))
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "hashes/s")
}

// benchTxOnly opens an empty InTenantTx per iteration, optionally taking the
// same advisory lock lockChain takes. The difference between the two runs is
// the cost of acquiring an UNCONTENDED chain lock (one extra round trip);
// TxOnly on its own is the floor no append can beat, because every append pays
// BEGIN + set_config + COMMIT whatever else it does.
func benchTxOnly(b *testing.B, pool *db.Pool, tenant uuid.UUID, takeLock bool) {
	ctx := context.Background()
	var got int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			if takeLock {
				// Byte-for-byte the statement in audit.go:689.
				if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('wpmgr_audit:' || $1))", tenant.String()); err != nil {
					return err
				}
			}
			// One trivial round trip so an empty tx is not optimised into
			// nothing at the driver level, and so the result is observable.
			return tx.QueryRow(ctx, "SELECT 1").Scan(&got)
		})
		if err != nil {
			b.Fatalf("tx: %v", err)
		}
	}
	b.StopTimer()
	if got != 1 {
		b.Fatalf("transaction body did not run: got %d, want 1", got)
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
}

// benchRecordSameTenant drives b.N real Record calls for ONE tenant, spread
// over `writers` goroutines, then proves the rows actually landed.
func benchRecordSameTenant(b *testing.B, pool *db.Pool, rec *audit.Recorder, writers int) {
	ctx := context.Background()
	tenant := seedTenant(b, pool, "audit-bench-same-"+uuid.NewString()[:8])
	before := benchCountAuditRows(b, pool, tenant)

	runAppends(b, writers, func(w, i int) error {
		_, err := rec.Record(ctx, benchEventFor(tenant, w*1_000_000+i))
		return err
	})

	after := benchCountAuditRows(b, pool, tenant)
	if landed := after - before; landed != int64(b.N) {
		b.Fatalf("rows did not land: %d appended, %d rows added (the benchmark measured nothing)", b.N, landed)
	}
	// The chain must still be intact: a throughput number taken while the
	// chain was silently breaking would be measuring the wrong system.
	ok, brokenAt, err := rec.Verify(ctx, tenant)
	if err != nil {
		b.Fatalf("verify: %v", err)
	}
	if !ok {
		b.Fatalf("chain broken at %s after %d concurrent appends", brokenAt, b.N)
	}
}

// benchRecordSeparateTenants gives each writer its OWN tenant, so the appends
// contend on the pool and on Postgres but NOT on each other's chain lock.
func benchRecordSeparateTenants(b *testing.B, pool *db.Pool, rec *audit.Recorder, writers int) {
	ctx := context.Background()
	tenants := make([]uuid.UUID, writers)
	before := make([]int64, writers)
	for w := range tenants {
		tenants[w] = seedTenant(b, pool, fmt.Sprintf("audit-bench-sep-%s-%d", uuid.NewString()[:8], w))
		before[w] = benchCountAuditRows(b, pool, tenants[w])
	}

	runAppends(b, writers, func(w, i int) error {
		_, err := rec.Record(ctx, benchEventFor(tenants[w], i))
		return err
	})

	var landed int64
	for w := range tenants {
		landed += benchCountAuditRows(b, pool, tenants[w]) - before[w]
	}
	if landed != int64(b.N) {
		b.Fatalf("rows did not land: %d appended, %d rows added across %d tenants", b.N, landed, writers)
	}
}

// runAppends splits b.N operations evenly across `writers` goroutines, times
// the wall clock across all of them, and reports aggregate appends/s alongside
// Go's own ns/op (which, for a concurrent run, is wall time divided by total
// ops — i.e. the inverse of aggregate throughput, not per-writer latency).
//
// It deliberately does NOT use b.RunParallel: RunParallel's goroutine count is
// GOMAXPROCS×parallelism, which is a property of the machine rather than of
// the scenario, and the scenario here is "N clients writing at once".
func runAppends(b *testing.B, writers int, op func(w, i int) error) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for w := 0; w < writers; w++ {
		lo := b.N * w / writers
		hi := b.N * (w + 1) / writers
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				if err := op(w, i); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}(w, lo, hi)
	}
	wg.Wait()
	elapsed := time.Since(start)
	b.StopTimer()

	select {
	case err := <-errCh:
		b.Fatalf("append failed: %v", err)
	default:
	}
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "appends/s")
		b.ReportMetric(float64(writers), "writers")
	}
}

// countAuditRows reads through pool.InTenantTx — the same helper the append
// path uses, as the same wpmgr_app role — rather than opening its own
// connection, so the count is taken under the RLS the real path runs under.
func benchCountAuditRows(b *testing.B, pool *db.Pool, tenant uuid.UUID) int64 {
	b.Helper()
	ctx := context.Background()
	var n int64
	err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE tenant_id = $1", tenant).Scan(&n)
	})
	if err != nil {
		b.Fatalf("count audit rows: %v", err)
	}
	return n
}

// benchEvent is the fixed row shape every measurement here uses: an
// assistant-surface tool call with four small metadata keys. Row size is part
// of the result — a 4 KB metadata blob would give a different number — so it
// is pinned here and stated in the RESULTS block rather than varied.
func benchEvent(i int) audit.Event {
	return audit.Event{
		ActorType:  audit.ActorUser,
		ActorID:    "00000000-0000-0000-0000-000000000001",
		Action:     "assistant.tool.call",
		TargetType: "site",
		TargetID:   "00000000-0000-0000-0000-000000000002",
		Metadata: map[string]any{
			"tool":     "site.list",
			"seq":      i,
			"decision": "allowed",
			"conn":     "bench",
		},
	}
}

func benchEventFor(tenant uuid.UUID, i int) audit.Event {
	e := benchEvent(i)
	e.TenantID = tenant
	return e
}

// ---------------------------------------------------------------------------
// RESULTS — taken 2026-08-31. ADR-061 A10's third requirement.
// ---------------------------------------------------------------------------
//
// # The machine, and why it is the first thing here
//
//	goos darwin, goarch arm64, cpu Apple M4, NumCPU=10, GOMAXPROCS=10
//	Postgres 16.15, CONTAINERISED on the same host (testcontainers
//	  postgres:16-alpine, Docker Desktop 29.5.2, 7936 MB)
//	Test pool MaxConns=10 (db.Connect's pgx default). Production's
//	  db.ConnectApp pins MaxConns=5 per instance.
//	Role: wpmgr_app — non-superuser, non-BYPASSRLS, the role every install
//	  runs as. Every append went through audit.Recorder.Record, which opens
//	  pool.InTenantTx; nothing here opens its own connection.
//
// LOAD: the machine was BUSY while these ran. `uptime` immediately after the
// -count 3 run reported load averages 14.77 13.49 11.28 on 10 cores, with 4
// concurrent `go test` processes and 3 other Postgres containers up (other
// agents' integration suites). That is not an aside. It is why the two runs
// below disagree by up to 3x on absolute throughput, and why the conclusions
// drawn are the ones that survive that spread.
//
// # Run A — quieter, `-benchtime 300x`
//
//	HashChainCPUOnly            1257 ns/op        795317 hashes/s
//	TxOnly                    683238 ns/op          1464 tx/s
//	TxPlusChainLock           966146 ns/op          1035 tx/s
//	Record/SameTenant/w=1    1269900 ns/op           787.5 appends/s
//	Record/SameTenant/w=2     514138 ns/op          1945 appends/s
//	Record/SameTenant/w=4     685420 ns/op          1459 appends/s
//	Record/SameTenant/w=8     902252 ns/op          1108 appends/s
//	Record/SeparateTenants/w=2 462084 ns/op         2164 appends/s
//	Record/SeparateTenants/w=4 338690 ns/op         2953 appends/s
//	Record/SeparateTenants/w=8 228560 ns/op         4375 appends/s
//
// # Run B — `-benchtime 1000x -count 3`, MEDIAN of 3, under the load above
//
//	HashChainCPUOnly           915.2 ns/op       1092647 hashes/s
//	TxOnly                    674954 ns/op          1482 tx/s
//	TxPlusChainLock           934038 ns/op          1071 tx/s
//	Record/SameTenant/w=1    2077246 ns/op           481.4 appends/s
//	Record/SameTenant/w=2    2155502 ns/op           463.9 appends/s
//	Record/SameTenant/w=4    3736571 ns/op           267.6 appends/s
//	Record/SameTenant/w=8    1775479 ns/op           563.2 appends/s
//	Record/SeparateTenants/w=2 1301916 ns/op          768.1 appends/s
//	Record/SeparateTenants/w=4  569694 ns/op         1755 appends/s
//	Record/SeparateTenants/w=8  402466 ns/op         2485 appends/s
//
// # The four answers
//
//  1. APPENDS PER SECOND, ONE TENANT. Between ~800 and ~1,900 on a quiet
//     machine; ~270 to ~630 with the host at load 14. Call it ~1,000/s, i.e.
//     of the order of 60,000 appends per minute for a single tenant, and read
//     the low figures as what a saturated host does rather than as the lock's
//     doing.
//
//  2. DEGRADATION SHAPE UNDER CONCURRENT SAME-TENANT WRITERS: linear
//     queueing, no cliff. Aggregate throughput stays inside one band from 1 to
//     8 writers (Run A 1945 → 1459 → 1108; Run B 464 → 268 → 563) while
//     per-writer latency rises roughly in proportion to the writer count
//     (Run A: ~1.0 ms at w=2, ~2.7 ms at w=4, ~7.2 ms at w=8). Writers queue,
//     they do not collapse, and nothing times out or errors — every run
//     asserted its rows landed and re-verified the chain. Note w=1 is NOT the
//     peak: a lone writer is round-trip bound, and a second connection
//     overlaps client latency with server work, so two writers beat one.
//
//  3. SEPARATE TENANTS DO NOT CONTEND. This is the cleanest signal in the
//     data and it survives both runs: with one tenant per writer throughput
//     scales roughly linearly (Run A 2164 → 2953 → 4375; Run B 768 → 1755 →
//     2485) where the same writer counts on ONE tenant do not scale at all.
//     The lock is per-tenant in behaviour and not only in intent. No finding.
//
//  4. HASH COST vs LOCK WAIT — not close. One chain link's JSON encode +
//     canonical concatenation + SHA-256 costs ~0.9-1.3 us. One append costs
//     1.0-2.3 ms. The cryptography is ~0.05% of an append: three orders of
//     magnitude apart. The whole cost is database round trips, and the
//     decomposition says where: BEGIN + set_config + trivial query + COMMIT is
//     ~683 us; adding the advisory lock costs ~260-280 us more (one extra
//     round trip, not lock work — it is uncontended in that sub-benchmark);
//     the prev-hash SELECT plus the INSERT add the remaining ~300 us. Anyone
//     asked to make this faster should be cutting round trips, never touching
//     the hash.
//
// # What this does NOT cover — read before quoting any number above
//
//   - It is a developer laptop against a SAME-HOST container. Every figure
//     here is round-trip bound, so production (Cloud Run to Cloud SQL over a
//     VPC connector) will track ITS round-trip time and not this one. This
//     measurement establishes the shape and the order of magnitude. It is not
//     a production ceiling.
//   - The host was under heavy load (above). These are lower bounds.
//   - Single process. That is less of a limitation than it sounds: the lock
//     lives in Postgres, so the per-tenant ceiling is a property of the
//     DATABASE and does not multiply across the 4 Cloud Run instances. What
//     is NOT covered is more than 8 concurrent writers on one tenant —
//     production could present up to 5 x 4 = 20 — because this pool's
//     MaxConns=10 would make the pool, not the lock, the constraint.
//   - One row shape: the four-key metadata in benchEvent. A large metadata
//     blob would move the INSERT cost and the hash cost together.
//   - Appends only. List, Verify and Rebaseline are not measured here, and
//     Verify in particular is O(all entries) for a tenant.
//
// # Interpretation against the surface as it stands
//
// COMFORTABLE, with one named place the pressure will actually come from.
//
// ADR-061 A4 caps an organization at 4 concurrently dispatching steps (River
// MaxWorkers, default 4, sharded per tenant). At 4 concurrent same-tenant
// writers this benchmark measured 268-1,459 appends/s — of the order of
// 16,000 to 87,000 audit rows per minute for one organization, against a
// dispatch ceiling that permits 4 steps at a time each writing a handful of
// rows. Two orders of magnitude of headroom. Dispatch is nowhere near the
// chain.
//
// The pressure is on the READ side, and A10 puts it there itself. A10 requires
// reads to be audited, and A4 states that "Reads are freely concurrent and take
// no lock" — so the one workload with no ceiling above it is the one that would
// funnel through this per-tenant serialisation point if every AI-originated
// read wrote its own fail-closed row. That is the case that could turn ~1,000
// appends/s from comfortable into binding, and it is exactly why A10 already
// allows read records to be "batched or sampled for volume, but the batching
// rule is written down with a stated retention".
//
// NOT IMPLEMENTED HERE, deliberately, and listed only so the option is on the
// record for whoever takes that decision: batching read records into one
// append, or moving read audit off the chained table entirely. Both are
// changes to the audit path, which is a separate job with its own review.
//
// One correction for anyone reasoning about this from the surface docs: there
// is no per-tenant TOOL-CALL rate limiter to compare against. The limiters
// that exist are on connection registration (mcp.RegisterGlobalPerMin=60 and
// mcp.RegisterPerPeerPerMin=10, internal/mcp/register_limit.go:88 and :93) and
// on token minting (mcp.MintGlobalPerMin=30, mcp.MintPerUserPerMin=5,
// internal/mcp/mint.go:50 and :60) — all per process or per peer, none per
// tenant, and none on the tool-call path. A4's step ceiling is the only
// per-organization bound above the audit chain today.
