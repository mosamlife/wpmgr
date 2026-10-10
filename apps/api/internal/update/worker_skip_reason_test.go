package update

// worker_skip_reason_test.go: GH #367, the control-plane half. The agent now
// says why it skipped an item (skip_reason), and the task detail says it too.
// A Composer-managed core skip used to read "already up to date", which is
// false: core was out of date and was left alone on purpose.
//
// Each case starts from the agent's reply bytes, decoded the way
// agentcmd.Client.Update decodes them, so the closed-set decode and the
// worker's mapping are tested together.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// agentSkipReply is the agent's reply to a one-item update that it skipped,
// in the shape class-update-command.php's result() builds: empty versions, a
// log sentence, and skip_reason only when skipReasonJSON is not "". ok stays
// true: the agent sets ok=false only for a failed or site_busy row.
func agentSkipReply(t *testing.T, target, slug, skipReasonJSON string) agentcmd.UpdateResponse {
	t.Helper()
	field := ""
	if skipReasonJSON != "" {
		field = `,"skip_reason":` + skipReasonJSON
	}
	body := `{"ok":true,"results":[{"type":"` + target + `","slug":"` + slug + `","from_version":"","to_version":"",` +
		`"status":"skipped","snapshot_id":"","log":"agent log sentence"` + field + `}]}`
	var resp agentcmd.UpdateResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp
}

// skipReasonCase is one agent skip and the detail the operator must read.
type skipReasonCase struct {
	name       string
	target     string
	skipReason string // raw JSON for skip_reason; "" sends none
	wantDetail string
}

func applySkipReasonCases() []skipReasonCase {
	return []skipReasonCase{
		{
			name: "core_managed", target: TargetCore, skipReason: `"core_managed"`,
			wantDetail: "WordPress core is managed by Composer on this site, so WPMgr did not update it. " +
				"Update core in composer.json and redeploy.",
		},
		{
			name: "file_mods_disallowed", target: TargetCore, skipReason: `"file_mods_disallowed"`,
			wantDetail: "This site does not allow file changes (DISALLOW_FILE_MODS or the file_mod_allowed filter), so WPMgr did not update it.",
		},
		{
			name: "not_installed", target: TargetPlugin, skipReason: `"not_installed"`,
			wantDetail: "Not installed on this site, so there was nothing to update.",
		},
		{
			name: "self_target", target: TargetPlugin, skipReason: `"self_target"`,
			wantDetail: "This is the WPMgr agent itself, which updates over its own channel, so WPMgr did not update it as a plugin.",
		},
		{
			name: "an unknown value", target: TargetCore, skipReason: `"composer_v2"`,
			wantDetail: "already up to date",
		},
		{
			name: "a known value in another case", target: TargetCore, skipReason: `"CORE_MANAGED"`,
			wantDetail: "already up to date",
		},
		{
			name: "a value that is not a string", target: TargetCore, skipReason: `7`,
			wantDetail: "already up to date",
		},
		{
			name: "no reason", target: TargetPlugin, skipReason: "",
			wantDetail: "already up to date",
		},
	}
}

func skipTask(target string) (Task, agentcmd.UpdateItem, string) {
	task := testTask()
	task.TargetType = target
	slug := "suremail"
	if target == TargetCore {
		slug = agentcmd.CoreSlug
		task.FromVersion = "7.0"
	}
	task.TargetSlug = slug
	return task, agentcmd.UpdateItem{Type: target, Slug: slug, Version: "latest"}, slug
}

// TestRunApply_SkippedItem_DetailSaysWhy: every reason in the closed set gets
// its own sentence, and "already up to date" is left only for a skip with no
// reason, or with one this build does not know. An unknown value's text never
// reaches the task. A skip is never health-checked and never rolled back.
func TestRunApply_SkippedItem_DetailSaysWhy(t *testing.T) {
	for _, tc := range applySkipReasonCases() {
		t.Run(tc.name, func(t *testing.T) {
			task, item, slug := skipTask(tc.target)
			cmd := &scriptedUpdateCommander{updateResp: agentSkipReply(t, tc.target, slug, tc.skipReason)}
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, &panicProber{t: t})

			if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			got := onlyFinish(t, repo)
			if got.Status != TaskSkipped {
				t.Errorf("status = %q (detail %q), want %q", got.Status, got.Detail, TaskSkipped)
			}
			if got.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", got.Detail, tc.wantDetail)
			}
			if got.Error != "" {
				t.Errorf("error = %q, want empty", got.Error)
			}
			if got.FromVersion != task.FromVersion {
				t.Errorf("from_version = %q, want the task's %q", got.FromVersion, task.FromVersion)
			}
			if raw := strings.Trim(tc.skipReason, `"`); tc.wantDetail == "already up to date" && raw != "" &&
				strings.Contains(strings.ToLower(got.Detail), strings.ToLower(raw)) {
				t.Errorf("detail %q echoes the unknown skip_reason %s", got.Detail, tc.skipReason)
			}
			if cmd.rollbackHit {
				t.Error("a skipped item was rolled back")
			}
		})
	}
}

// TestRunApply_SkippedItem_UnknownReasonPastTheDecoder holds the worker to the
// closed set on its own: a reason that reached it without going through the
// decoder still reads as no reason.
func TestRunApply_SkippedItem_UnknownReasonPastTheDecoder(t *testing.T) {
	task, item, slug := skipTask(TargetCore)
	cmd := &scriptedUpdateCommander{updateResp: agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{{
		Type: TargetCore, Slug: slug, Status: agentcmd.ItemSkipped, SkipReason: agentcmd.SkipReason("bedrock_detected"),
	}}}}
	repo := &probeFakeRepo{}
	w := newApplyTestWorker(repo, cmd, &panicProber{t: t})

	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	got := onlyFinish(t, repo)
	if got.Status != TaskSkipped || got.Detail != "already up to date" {
		t.Errorf("finish = (%q, %q), want (%q, %q)", got.Status, got.Detail, TaskSkipped, "already up to date")
	}
}

// TestRunDry_SkippedItem_DetailSaysWhy: a dry run keeps its status and its
// "no change" for a skip with no reason, and says why for a skip with one, so
// a dry run of Composer-managed core does not read as a plain no-op.
func TestRunDry_SkippedItem_DetailSaysWhy(t *testing.T) {
	for _, tc := range applySkipReasonCases() {
		t.Run(tc.name, func(t *testing.T) {
			task, item, slug := skipTask(tc.target)
			cmd := &scriptedUpdateCommander{updateResp: agentSkipReply(t, tc.target, slug, tc.skipReason)}
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, cmd, &panicProber{t: t})

			if err := w.runDry(context.Background(), task, "https://example.test", item); err != nil {
				t.Fatalf("runDry: %v", err)
			}
			got := onlyFinish(t, repo)
			want := tc.wantDetail
			if want == "already up to date" {
				want = "no change"
			}
			if got.Status != TaskSucceeded || got.Detail != want {
				t.Errorf("finish = (%q, %q), want (%q, %q)", got.Status, got.Detail, TaskSucceeded, want)
			}
		})
	}
}
