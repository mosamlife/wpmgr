package update

// worker_core_test.go: GH #415, the control-plane half. A WordPress core
// update is the one target where the agent's "up_to_date" on an apply is not
// taken as proof that nothing changed: the post-update health check still
// runs. A site that fails it is recorded as failed, and nothing is rolled
// back, because by the agent's own account there is nothing to roll back to.
// Plugins and themes keep their plain "already up to date".

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// itemCommander answers every Update with one scripted item result and
// records every Rollback request it is sent. A non-nil rollbackErr makes
// every Rollback fail in transport, after the request is recorded.
type itemCommander struct {
	result      agentcmd.ItemResult
	rollbacks   []agentcmd.RollbackRequest
	rollbackErr error
}

func (c *itemCommander) Update(context.Context, uuid.UUID, string, agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	return agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{c.result}}, nil
}

func (c *itemCommander) Rollback(_ context.Context, _ uuid.UUID, _ string, req agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	c.rollbacks = append(c.rollbacks, req)
	if c.rollbackErr != nil {
		return agentcmd.RollbackResponse{}, c.rollbackErr
	}
	return agentcmd.RollbackResponse{OK: true, RestoredVersion: req.ToVersion}, nil
}

// verifyingItemCommander adds the signed agent-first check, answering
// every call with one fixed outcome.
type verifyingItemCommander struct {
	itemCommander
	alive  bool
	reason agentcmd.ReachabilityReason
	calls  int
}

func (c *verifyingItemCommander) VerifyReachableWithReason(context.Context, uuid.UUID, string) (bool, bool, agentcmd.ReachabilityReason, error) {
	c.calls++
	return c.alive, false, c.reason, nil
}

func coreTask() Task {
	t := testTask()
	t.TargetType = TargetCore
	t.TargetSlug = agentcmd.CoreSlug
	t.FromVersion = "7.0"
	return t
}

func coreItem() agentcmd.UpdateItem {
	return agentcmd.UpdateItem{Type: TargetCore, Slug: agentcmd.CoreSlug, Version: "latest"}
}

// coreReportedUpToDate is the agent's answer to a core apply that, by its own
// account, changed nothing.
func coreReportedUpToDate() agentcmd.ItemResult {
	return agentcmd.ItemResult{Type: TargetCore, Slug: agentcmd.CoreSlug, FromVersion: "7.0", ToVersion: "7.0", Status: agentcmd.ItemUpToDate}
}

func errorStep(err error) probeStep { return probeStep{err: err} }

func onlyFinish(t *testing.T, repo *probeFakeRepo) FinishTaskInput {
	t.Helper()
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one terminal finish, got %d: %+v", len(repo.finished), repo.finished)
	}
	return repo.finished[0]
}

// TestRunApply_CoreUpToDate_FailedHealthCheck_FailsWithoutRollback is the
// #415 defence in depth: the site fails the check after a core apply the
// agent called a no-op, so the task fails, and no rollback is sent.
func TestRunApply_CoreUpToDate_FailedHealthCheck_FailsWithoutRollback(t *testing.T) {
	cases := []struct {
		name string
		// cmd and prober are built per case.
		build      func(t *testing.T) (Commander, HealthProber, *itemCommander)
		wantErrSub string
	}{
		{
			name: "homepage returns 503 on every attempt",
			build: func(t *testing.T) (Commander, HealthProber, *itemCommander) {
				c := &itemCommander{result: coreReportedUpToDate()}
				return c, &scriptedProber{script: []probeStep{unhealthyStep(503)}}, c
			},
			wantErrSub: "status=503",
		},
		{
			name: "homepage unreachable on every attempt",
			build: func(t *testing.T) (Commander, HealthProber, *itemCommander) {
				c := &itemCommander{result: coreReportedUpToDate()}
				return c, &scriptedProber{script: []probeStep{errorStep(errors.New("dial tcp: i/o timeout"))}}, c
			},
			wantErrSub: "i/o timeout",
		},
		{
			name: "signed agent check returns a server error on every attempt",
			build: func(t *testing.T) (Commander, HealthProber, *itemCommander) {
				c := &verifyingItemCommander{itemCommander: itemCommander{result: coreReportedUpToDate()}, reason: agentcmd.ReasonHTTP5xx}
				return c, &panicProber{t: t}, &c.itemCommander
			},
			wantErrSub: "server error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, prober, rec := tc.build(t)
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, prober)

			if err := w.runApply(context.Background(), coreTask(), "https://example.test", coreItem()); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			got := onlyFinish(t, repo)
			if got.Status != TaskFailed {
				t.Fatalf("status = %q (detail %q), want %q: a core apply the agent called a no-op must still be checked", got.Status, got.Detail, TaskFailed)
			}
			if len(rec.rollbacks) != 0 {
				t.Fatalf("rollback sent %d time(s); by the agent's own account nothing changed, so nothing may be rolled back", len(rec.rollbacks))
			}
			for _, want := range []string{"WordPress core reported no change", "did not pass the health check", "Nothing was rolled back"} {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			if !strings.Contains(got.Error, tc.wantErrSub) {
				t.Errorf("error = %q, want it to carry the check's reason (%q)", got.Error, tc.wantErrSub)
			}
		})
	}
}

// TestRunApply_CoreUpToDate_HealthySite_StaysAlreadyUpToDate proves the check
// runs and, when the site passes it, the outcome is exactly today's.
func TestRunApply_CoreUpToDate_HealthySite_StaysAlreadyUpToDate(t *testing.T) {
	cases := []struct {
		name  string
		probe probeStep
	}{
		{"homepage healthy", healthyStep()},
		{"homepage served from a cache (inconclusive, not a failure)", probeStep{result: agentcmd.ProbeResult{StatusCode: 200, CacheHit: true, Detail: "cache hit (cf-cache-status: HIT)"}}},
		{"homepage 404 (no baseline, inconclusive)", probeStep{result: agentcmd.ProbeResult{StatusCode: 404}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &itemCommander{result: coreReportedUpToDate()}
			prober := &scriptedProber{script: []probeStep{tc.probe}}
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, prober)

			if err := w.runApply(context.Background(), coreTask(), "https://example.test", coreItem()); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			if prober.calls == 0 {
				t.Fatal("the post-update health check never ran for a core apply reported as up to date")
			}
			got := onlyFinish(t, repo)
			if got.Status != TaskSkipped || got.Detail != "already up to date" || got.Error != "" {
				t.Errorf("finish = (%q, %q, %q), want (%q, %q, \"\")", got.Status, got.Detail, got.Error, TaskSkipped, "already up to date")
			}
			if len(cmd.rollbacks) != 0 {
				t.Errorf("rollback sent %d time(s), want none", len(cmd.rollbacks))
			}
		})
	}
}

// TestRunApply_PluginOrThemeUpToDate_IsNotHealthChecked is the over-fire
// guard: a plugin's or a theme's "up_to_date" is honest, so neither check
// runs and the outcome is the plain skip.
func TestRunApply_PluginOrThemeUpToDate_IsNotHealthChecked(t *testing.T) {
	for _, target := range []string{TargetPlugin, TargetTheme} {
		t.Run(target, func(t *testing.T) {
			cmd := &verifyingItemCommander{
				itemCommander: itemCommander{result: agentcmd.ItemResult{Type: target, Slug: "x", FromVersion: "1.0", ToVersion: "1.0", Status: agentcmd.ItemUpToDate}},
				alive:         true,
				reason:        agentcmd.ReasonAlive,
			}
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, &panicProber{t: t})

			task := testTask()
			task.TargetType = target
			task.TargetSlug = "x"
			if err := w.runApply(context.Background(), task, "https://example.test", agentcmd.UpdateItem{Type: target, Slug: "x", Version: "latest"}); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			if cmd.calls != 0 {
				t.Errorf("signed agent check ran %d time(s) for a %s reported up to date, want 0", cmd.calls, target)
			}
			got := onlyFinish(t, repo)
			if got.Status != TaskSkipped || got.Detail != "already up to date" {
				t.Errorf("finish = (%q, %q), want (%q, %q)", got.Status, got.Detail, TaskSkipped, "already up to date")
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Core rollback policy (GH #415). A core rollback is a forced downgrade, so it
// is sent only after a CONFIRMED fatal (the signed agent check's own server
// error, a fatal-error page, or a 5xx homepage), and always with an explicit
// allow_core_downgrade. Anything weaker records the failure and leaves core
// as it is.
// ----------------------------------------------------------------------------

// coreUpdated is the agent's answer to a core apply that changed the version.
func coreUpdated() agentcmd.ItemResult {
	return agentcmd.ItemResult{Type: TargetCore, Slug: agentcmd.CoreSlug, FromVersion: "7.0", ToVersion: "7.1", Status: agentcmd.ItemSucceeded}
}

// TestRunApply_CoreUpdated_ProbeTimeout_LeavesCoreAndFails: a homepage that
// cannot be reached is not a confirmed fatal, so core is not downgraded.
func TestRunApply_CoreUpdated_ProbeTimeout_LeavesCoreAndFails(t *testing.T) {
	cases := []struct {
		name  string
		probe probeStep
	}{
		{"probe times out on every attempt", errorStep(context.DeadlineExceeded)},
		{"probe cannot connect on every attempt", errorStep(errors.New("dial tcp: connection refused"))},
		{"probe answers with no status (not a confirmed fatal)", probeStep{result: agentcmd.ProbeResult{StatusCode: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &itemCommander{result: coreUpdated()}
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, &scriptedProber{script: []probeStep{tc.probe}})

			if err := w.runApply(context.Background(), coreTask(), "https://example.test", coreItem()); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			if len(cmd.rollbacks) != 0 {
				t.Fatalf("rollback sent %d time(s) (%+v): an unconfirmed failure must never downgrade core", len(cmd.rollbacks), cmd.rollbacks)
			}
			got := onlyFinish(t, repo)
			if got.Status != TaskFailed {
				t.Fatalf("status = %q, want %q", got.Status, TaskFailed)
			}
			for _, want := range []string{"WordPress core was updated", "did not pass the health check", "left as is"} {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			if got.FromVersion != "7.0" || got.ToVersion != "7.1" {
				t.Errorf("versions = %q -> %q, want 7.0 -> 7.1 (core is on the new version)", got.FromVersion, got.ToVersion)
			}
			if got.Error == "" {
				t.Error("error is empty, want the check's reason in the task's error log")
			}
		})
	}
}

// TestRunApply_CoreUpdated_ConfirmedFatal_RollsBackWithDowngradeFlag: each
// confirmed fatal sends exactly one core rollback, flagged.
func TestRunApply_CoreUpdated_ConfirmedFatal_RollsBackWithDowngradeFlag(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) (Commander, HealthProber, *itemCommander)
	}{
		{
			name: "homepage 5xx on every attempt",
			build: func(t *testing.T) (Commander, HealthProber, *itemCommander) {
				c := &itemCommander{result: coreUpdated()}
				return c, &scriptedProber{script: []probeStep{unhealthyStep(500)}}, c
			},
		},
		{
			name: "homepage shows a PHP fatal",
			build: func(t *testing.T) (Commander, HealthProber, *itemCommander) {
				c := &itemCommander{result: coreUpdated()}
				return c, &scriptedProber{script: []probeStep{{result: agentcmd.ProbeResult{StatusCode: 200, Fatal: true, Detail: "fatal-error signature in response body"}}}}, c
			},
		},
		{
			name: "signed agent check returns a server error on every attempt",
			build: func(t *testing.T) (Commander, HealthProber, *itemCommander) {
				c := &verifyingItemCommander{itemCommander: itemCommander{result: coreUpdated()}, reason: agentcmd.ReasonHTTP5xx}
				return c, &panicProber{t: t}, &c.itemCommander
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, prober, rec := tc.build(t)
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, prober)

			if err := w.runApply(context.Background(), coreTask(), "https://example.test", coreItem()); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			if len(rec.rollbacks) != 1 {
				t.Fatalf("rollback sent %d time(s), want exactly 1 on a confirmed fatal", len(rec.rollbacks))
			}
			req := rec.rollbacks[0]
			if !req.AllowCoreDowngrade {
				t.Errorf("rollback request %+v: a core rollback must carry allow_core_downgrade", req)
			}
			if req.Type != TargetCore || req.ToVersion != "7.0" {
				t.Errorf("rollback request %+v, want type core back to 7.0", req)
			}
			if got := onlyFinish(t, repo); got.Status != TaskRolledBack {
				t.Errorf("status = %q (detail %q), want %q", got.Status, got.Detail, TaskRolledBack)
			}
		})
	}
}

// TestRunApply_CoreUpdated_ConfirmedFatal_RollbackUndeliverable_NeedsManualRecovery:
// a core rollback whose command cannot be delivered fails the task with the
// core detail. The agent's automatic recovery covers plugins and themes only,
// so the detail must say manual recovery is needed and must not promise it.
func TestRunApply_CoreUpdated_ConfirmedFatal_RollbackUndeliverable_NeedsManualRecovery(t *testing.T) {
	cmd := &itemCommander{result: coreUpdated(), rollbackErr: errors.New("dial tcp: connection refused")}
	repo := &probeFakeRepo{}
	w := newApplyTestWorker(repo, cmd, &scriptedProber{script: []probeStep{unhealthyStep(500)}})

	if err := w.runApply(context.Background(), coreTask(), "https://example.test", coreItem()); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if len(cmd.rollbacks) != 1 || !cmd.rollbacks[0].AllowCoreDowngrade {
		t.Fatalf("rollbacks = %+v, want exactly one, carrying allow_core_downgrade", cmd.rollbacks)
	}
	got := onlyFinish(t, repo)
	if got.Status != TaskFailed {
		t.Fatalf("status = %q, want %q", got.Status, TaskFailed)
	}
	if got.Detail != coreRollbackUndeliverableDetail {
		t.Errorf("detail = %q, want the core detail %q", got.Detail, coreRollbackUndeliverableDetail)
	}
	if !strings.Contains(got.Detail, "manual recovery") || strings.Contains(got.Detail, "watchdog") {
		t.Errorf("detail = %q, want it to name manual recovery and never the agent's watchdog", got.Detail)
	}
	if got.Error == "" {
		t.Error("error is empty, want the rollback command's own error in the task's error log")
	}
}

// TestConfirmedFatal pins what counts as a confirmed fatal for a core
// rollback: the signed agent check's server error, a homepage 5xx, or a PHP
// fatal-error page. No answer, a cached answer and a 4xx do not count.
func TestConfirmedFatal(t *testing.T) {
	cases := []struct {
		name  string
		probe agentcmd.ProbeResult
		agent bool
		want  bool
	}{
		{"signed agent check returned a server error", agentcmd.ProbeResult{}, true, true},
		{"homepage 500", agentcmd.ProbeResult{StatusCode: 500}, false, true},
		{"homepage 503", agentcmd.ProbeResult{StatusCode: 503}, false, true},
		{"homepage PHP fatal served with 200", agentcmd.ProbeResult{StatusCode: 200, Fatal: true}, false, true},
		{"no answer (timeout or refused connection)", agentcmd.ProbeResult{}, false, false},
		{"cached 500", agentcmd.ProbeResult{StatusCode: 500, CacheHit: true}, false, false},
		{"cached PHP fatal page", agentcmd.ProbeResult{StatusCode: 200, Fatal: true, CacheHit: true}, false, false},
		{"homepage 404", agentcmd.ProbeResult{StatusCode: 404}, false, false},
		{"homepage 200", agentcmd.ProbeResult{StatusCode: 200}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confirmedFatal(tc.probe, tc.agent); got != tc.want {
				t.Errorf("confirmedFatal(%+v, %v) = %v, want %v", tc.probe, tc.agent, got, tc.want)
			}
		})
	}
}

// TestRunApply_PluginRollback_NeverCarriesDowngradeFlag is the over-fire
// guard: plugin rollbacks are unchanged, including the one after an
// unreachable homepage, and never carry the core flag.
func TestRunApply_PluginRollback_NeverCarriesDowngradeFlag(t *testing.T) {
	cases := []struct {
		name  string
		probe probeStep
	}{
		{"homepage 5xx", unhealthyStep(503)},
		{"homepage unreachable", errorStep(errors.New("dial tcp: connection refused"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &itemCommander{result: agentcmd.ItemResult{Type: TargetPlugin, Slug: "suremail", FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded, SnapshotID: "snap-1"}}
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, &scriptedProber{script: []probeStep{tc.probe}})

			if err := w.runApply(context.Background(), testTask(), "https://example.test", updateItem()); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			if len(cmd.rollbacks) != 1 {
				t.Fatalf("rollback sent %d time(s), want 1 (plugin behaviour unchanged)", len(cmd.rollbacks))
			}
			if cmd.rollbacks[0].AllowCoreDowngrade {
				t.Errorf("plugin rollback request %+v carries allow_core_downgrade", cmd.rollbacks[0])
			}
			if got := onlyFinish(t, repo); got.Status != TaskRolledBack {
				t.Errorf("status = %q, want %q", got.Status, TaskRolledBack)
			}
		})
	}
}
