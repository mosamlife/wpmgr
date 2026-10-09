// GH #428: the agent-release mirror's boot check, proven against River's real
// uniqueness rules in Postgres, as wpmgr_app.
//
// River enforces job uniqueness in SQL (a unique index over unique_key, live
// only while a row's state is one of its own unique_states), so these
// properties cannot be shown with a fake inserter:
//
//   - a periodic tick that COMPLETED shortly before a restart does not
//     deduplicate the boot check (River's default ByState includes completed,
//     so a boot check sharing the tick's args and 5h window would have been
//     swallowed; the positive control below shows exactly that happening);
//   - an outstanding boot check does not deduplicate the next periodic tick;
//   - at most one boot check is outstanding, so a restart loop queues one;
//   - a finished boot check does not block the next start's;
//   - a request recorded in agent_mirror_state defers the boot check past the
//     spacing window instead of dropping it.
//
// Every insert goes through agentupstream.EnqueueBootCheck with a River client
// on the app pool, the same call cmd/wpmgr makes after River starts.
package tests

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentupstream"
)

type mirrorJobRow struct {
	id          int64
	state       string
	scheduledAt time.Time
	boot        bool
	trigger     string
	queue       string
	maxAttempts int
}

func TestAgentMirrorBootCheck_RiverUniqueness(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	// River's schema, migrated as the owner, as cmd/wpmgr does.
	owner := connectOwner(t, pool)
	defer owner.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(owner.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	// An insert-only client on the app pool: the role every install runs as.
	client, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	repo := agentmirror.NewRepo(pool)

	reset := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM river_job WHERE kind = $1`, agentupstream.MirrorArgs{}.Kind()); err != nil {
			t.Fatalf("clear mirror jobs: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_mirror_state SET last_request_at = NULL WHERE id = 1`); err != nil {
			t.Fatalf("clear last_request_at: %v", err)
		}
	}
	complete := func(t *testing.T, id int64) {
		t.Helper()
		tag, err := pool.Exec(ctx, `UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, id)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("complete job %d: rows=%d err=%v", id, tag.RowsAffected(), err)
		}
	}
	jobs := func(t *testing.T) []mirrorJobRow {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT id, state::text, scheduled_at, COALESCE((args->>'boot')::boolean, false),
			       args->>'trigger', queue, max_attempts
			FROM river_job WHERE kind = $1 ORDER BY id`, agentupstream.MirrorArgs{}.Kind())
		if err != nil {
			t.Fatalf("list mirror jobs: %v", err)
		}
		defer rows.Close()
		var out []mirrorJobRow
		for rows.Next() {
			var r mirrorJobRow
			if err := rows.Scan(&r.id, &r.state, &r.scheduledAt, &r.boot, &r.trigger, &r.queue, &r.maxAttempts); err != nil {
				t.Fatalf("scan mirror job: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate mirror jobs: %v", err)
		}
		return out
	}
	bootRows := func(t *testing.T) []mirrorJobRow {
		t.Helper()
		var out []mirrorJobRow
		for _, r := range jobs(t) {
			if r.boot {
				out = append(out, r)
			}
		}
		return out
	}
	tick := func(t *testing.T, scheduledAt time.Time) *river.InsertOpts {
		t.Helper()
		opts := agentupstream.PeriodicInsertOpts()
		if !scheduledAt.IsZero() {
			opts.ScheduledAt = scheduledAt
		}
		return opts
	}
	periodicArgs := agentupstream.MirrorArgs{Trigger: agentupstream.TriggerPeriodic}

	t.Run("the boot check is stored to run within minutes of start", func(t *testing.T) {
		reset(t)
		start := time.Now()
		bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, start)
		if err != nil || !bc.Queued {
			t.Fatalf("EnqueueBootCheck = %+v, %v; want queued", bc, err)
		}
		got := bootRows(t)
		if len(got) != 1 {
			t.Fatalf("boot check rows = %d, want 1", len(got))
		}
		r := got[0]
		if d := r.scheduledAt.Sub(start); d < agentupstream.BootCheckMinDelay-time.Second || d > 10*time.Minute {
			t.Fatalf("stored scheduled_at is %v after start, want within %v..10m", d, agentupstream.BootCheckMinDelay)
		}
		if r.state != "scheduled" || r.trigger != agentupstream.TriggerPeriodic || r.queue != agentupstream.MirrorQueue || r.maxAttempts != 1 {
			t.Fatalf("stored row = %+v, want scheduled, trigger periodic, queue %q, 1 attempt", r, agentupstream.MirrorQueue)
		}
	})

	t.Run("a periodic tick completed shortly before the restart does not swallow the boot check", func(t *testing.T) {
		reset(t)
		window := agentupstream.MirrorArgs{}.InsertOpts().UniqueOpts.ByPeriod
		bootAt := time.Now().Add(3 * time.Minute)
		// The previous process's tick, in the same unique window as the boot
		// check, already finished.
		res, err := client.Insert(ctx, periodicArgs, tick(t, bootAt.Truncate(window)))
		if err != nil || res.UniqueSkippedAsDuplicate {
			t.Fatalf("insert completed tick: dup=%v err=%v", res != nil && res.UniqueSkippedAsDuplicate, err)
		}
		complete(t, res.Job.ID)

		// Positive control: an insert with the tick's own args and window IS
		// swallowed by that completed tick. A boot check shaped like the tick
		// would have been lost the same way.
		ctl, err := client.Insert(ctx, periodicArgs, tick(t, bootAt))
		if err != nil {
			t.Fatalf("control insert: %v", err)
		}
		if !ctl.UniqueSkippedAsDuplicate {
			t.Fatal("control: a tick-shaped insert in the same window was NOT deduplicated; this test can no longer detect suppression")
		}

		bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, time.Now())
		if err != nil {
			t.Fatalf("EnqueueBootCheck: %v", err)
		}
		if !bc.Queued || len(bootRows(t)) != 1 {
			t.Fatalf("boot check queued=%v rows=%d after a completed tick; want queued, 1 row", bc.Queued, len(bootRows(t)))
		}
	})

	t.Run("an outstanding boot check does not swallow the next periodic tick", func(t *testing.T) {
		reset(t)
		if bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, time.Now()); err != nil || !bc.Queued {
			t.Fatalf("EnqueueBootCheck = %+v, %v; want queued", bc, err)
		}
		res, err := client.Insert(ctx, periodicArgs, tick(t, time.Time{}))
		if err != nil {
			t.Fatalf("insert tick: %v", err)
		}
		if res.UniqueSkippedAsDuplicate {
			t.Fatal("the periodic tick was deduplicated against the outstanding boot check")
		}
	})

	t.Run("a restart loop queues one boot check, not one per start", func(t *testing.T) {
		reset(t)
		for i := 0; i < 3; i++ {
			bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, time.Now())
			if err != nil {
				t.Fatalf("start %d: %v", i, err)
			}
			if want := i == 0; bc.Queued != want {
				t.Fatalf("start %d: queued=%v, want %v", i, bc.Queued, want)
			}
		}
		if n := len(bootRows(t)); n != 1 {
			t.Fatalf("boot check rows after three starts = %d, want 1", n)
		}
	})

	t.Run("a finished boot check does not block the next start's", func(t *testing.T) {
		reset(t)
		if bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, time.Now()); err != nil || !bc.Queued {
			t.Fatalf("first start: %+v, %v", bc, err)
		}
		first := bootRows(t)
		if len(first) != 1 {
			t.Fatalf("boot check rows after the first start = %d, want 1", len(first))
		}
		complete(t, first[0].id)
		bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, time.Now())
		if err != nil || !bc.Queued {
			t.Fatalf("next start after the first boot check finished: %+v, %v; want queued", bc, err)
		}
		if n := len(bootRows(t)); n != 2 {
			t.Fatalf("boot check rows = %d, want 2", n)
		}
	})

	t.Run("a recorded request defers the boot check past the spacing window", func(t *testing.T) {
		reset(t)
		start := time.Now()
		last := start.Add(-5 * time.Minute)
		if err := repo.RecordAttempt(ctx, agentmirror.AttemptInput{
			Trigger:       agentmirror.TriggerPeriodic,
			Outcome:       agentmirror.OutcomeCurrent,
			LastRequestAt: last,
		}); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
		bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, start)
		if err != nil || !bc.Queued || !bc.Deferred {
			t.Fatalf("EnqueueBootCheck = %+v, %v; want queued and deferred", bc, err)
		}
		got := bootRows(t)
		if len(got) != 1 {
			t.Fatalf("boot check rows = %d, want 1", len(got))
		}
		open := last.Add(agentupstream.MinRequestSpacing)
		if d := got[0].scheduledAt.Sub(open); d < agentupstream.BootCheckMinDelay-time.Second || d > agentupstream.BootCheckMaxDelay {
			t.Fatalf("stored scheduled_at is %v after the spacing window closes, want within %v..%v",
				d, agentupstream.BootCheckMinDelay, agentupstream.BootCheckMaxDelay)
		}
	})

	t.Run("a request time ahead of the clock is lowered in storage, so the stored check is due after the window it closes", func(t *testing.T) {
		reset(t)
		start := time.Now()
		if err := repo.RecordAttempt(ctx, agentmirror.AttemptInput{
			Trigger:       agentmirror.TriggerPeriodic,
			Outcome:       agentmirror.OutcomeCurrent,
			LastRequestAt: start.Add(3 * time.Hour),
		}); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
		bc, err := agentupstream.EnqueueBootCheck(ctx, client, repo, start)
		if err != nil || !bc.Queued || !bc.Deferred || !bc.RequestTimeLowered || bc.LowerErr != nil {
			t.Fatalf("EnqueueBootCheck = %+v, %v; want queued, deferred, and the request time lowered", bc, err)
		}

		st, err := repo.Load(ctx)
		if err != nil || st.LastRequestAt == nil {
			t.Fatalf("load after the start: %+v, %v", st, err)
		}
		stored := *st.LastRequestAt
		// timestamptz keeps microseconds, so compare to the millisecond.
		if d := stored.Sub(start).Abs(); d > time.Millisecond {
			t.Fatalf("stored last_request_at is %v from the start clock, want it lowered to it", d)
		}
		got := bootRows(t)
		if len(got) != 1 {
			t.Fatalf("boot check rows = %d, want 1", len(got))
		}
		// What the job will read when it runs is a request time at least one
		// spacing window older than the moment it is due, so the guard passes.
		if gap := got[0].scheduledAt.Sub(stored); gap < agentupstream.MinRequestSpacing {
			t.Fatalf("the stored check is due %v after the stored request time, want at least %v", gap, agentupstream.MinRequestSpacing)
		}

		// Over-fire, in SQL: a time at or before the ceiling is not touched, and
		// neither is a missing one.
		if changed, err := repo.ClampLastRequestAt(ctx, start.Add(time.Hour)); err != nil || changed {
			t.Fatalf("ClampLastRequestAt above the stored time = %v, %v; want no change", changed, err)
		}
		if again, err := repo.Load(ctx); err != nil || again.LastRequestAt == nil || !again.LastRequestAt.Equal(stored) {
			t.Fatalf("stored request time moved to %+v (err %v), want it unchanged at %v", again.LastRequestAt, err, stored)
		}
		reset(t)
		if changed, err := repo.ClampLastRequestAt(ctx, start); err != nil || changed {
			t.Fatalf("ClampLastRequestAt with no stored time = %v, %v; want no change", changed, err)
		}
		if empty, err := repo.Load(ctx); err != nil || empty.LastRequestAt != nil {
			t.Fatalf("no-stored-time row became %+v (err %v), want it left NULL", empty.LastRequestAt, err)
		}
	})
}
