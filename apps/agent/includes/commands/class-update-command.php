<?php
/**
 * Update command: applies plugin/theme/core updates in response to a verified,
 * signed control-plane request.
 *
 * Contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/update
 *   body: { "dry_run": bool, "snapshot": bool, "items": [ { "type", "slug", "version" } ] }
 *   response: { "ok": bool, "results": [ { type, slug, from_version, to_version,
 *               status, snapshot_id, log } ] }
 *
 * Execution strategy:
 *   - dry_run: never touches the filesystem; reports would_update / up_to_date
 *     / unknown (GitHub issue #208 — availability could not be determined
 *     even after a forced fresh WordPress update check; see this class's
 *     "Stale-availability visibility fix" note below).
 *   - Pre-update snapshot (GitHub issue #131): for plugin/theme items a
 *     snapshot is ALWAYS captured before applying, regardless of the
 *     request's `snapshot` flag — it is the precondition for the auto-restore
 *     below, and a same-filesystem directory copy under
 *     wp-content/uploads/wpmgr-snapshots/<snapshot_id>/ is cheap. `core` keeps
 *     the original opt-in behavior (`snapshot=true` records the prior version
 *     for RollbackCommand's downgrade-by-version; there is no directory to
 *     snapshot for core, and no directory-level auto-restore — see D3 below).
 *   - apply: prefers WP-CLI when available, else falls back to the WordPress
 *     upgrader APIs under a quiet skin.
 *   - Completeness check + auto-restore (GitHub issue #131): a version-header
 *     bump alone does not prove an apply finished — WordPress's
 *     install_package()/copy_dir() is a non-atomic recursive copy that clears
 *     the destination first, so a hard resource kill mid-copy can leave a
 *     half-written plugin/theme whose main file WAS replaced (so the version
 *     reads as updated) while the rest of the tree is missing. Every apply is
 *     re-verified via UpdateRunner::isComplete() (plugin: validate_plugin();
 *     theme: a re-readable style.css `Name` header); an incomplete result is
 *     automatically rolled back from the pre-update snapshot and reported as
 *     `failed`, never `succeeded`. A WPMgr\Agent\Support\UpdateGuard, armed
 *     via register_shutdown_function() before the apply call, is the backstop
 *     for a kill severe enough that execution never returns from apply() at
 *     all. For `core` (D3) this directory-level rollback is deliberately NOT
 *     built — core relies on execute()'s resource guard (B1) plus WordPress's
 *     own Core_Upgrader temp-backup mechanism.
 *   - Out-of-band reconcile (S4, adversarial review of issue #131): the
 *     shutdown guard above cannot run at all when PHP-FPM's own
 *     `request_terminate_timeout` SIGTERMs the worker — that kill resets the
 *     timeout handler to SIG_DFL and tears the process down with NO shutdown
 *     functions invoked. WPMgr\Agent\Support\UpdateInFlight persists a small
 *     marker before each guarded apply and reconciles (auto-restores +
 *     clears) any marker left stale by exactly that kill, both at the start
 *     of the next `update` command and via a recurring cron sweep — see its
 *     class doc for the full design and the staleness-margin reasoning.
 *   - Unprotected-apply refusal, superseded by a graceful degraded-mode
 *     proceed (S8, adversarial review; revised by the agent-only regression
 *     fix below): S8 originally refused (`failed`) any plugin/theme item
 *     whose pre-update snapshot capture failed, rather than ever applying it
 *     unguarded. That over-fired on a healthy site whenever
 *     SnapshotManager::liveDir()'s realpath()-based containment check
 *     itself failed — open_basedir excluding a parent, or a
 *     symlinked/relocated wp-content tree — making EVERY plugin/theme
 *     update fail with a snapshot error while core (exempt from S8) kept
 *     working. SnapshotManager::liveDir() now has a safe, anchored fallback
 *     for that exact case (see its own class doc), which closes off the
 *     most common trigger; capture() can still legitimately fail for other
 *     reasons (a disk-constrained host, a genuinely missing source
 *     directory), and for those S8 now PROCEEDS with a best-effort,
 *     unprotected apply — matching capture()'s own "...proceeding without
 *     snapshot" log intent — rather than refusing the item outright. No
 *     guard is armed and no in-flight marker is written in this case (both
 *     remain gated on a non-empty `$snapshotId`, unchanged); the apply
 *     instead relies on WordPress core's own Plugin_Upgrader/Theme_Upgrader
 *     temp-backup/rollback (WP 6.3+), the post-apply isComplete() verify
 *     below, and the control plane's post-update health-probe +
 *     auto-rollback.
 *   - Final-hardening round (issue #131 final-hardening review): the
 *     UpdateInFlight marker written before each guarded apply is now paired
 *     with an flock() liveness lock held for the apply's whole duration (B)
 *     and reconciled only after verifying the live directory is genuinely
 *     incomplete (F2) — see UpdateInFlight's own class doc for both. This
 *     method also acquires/releases that lock ($inFlightLock, alongside the
 *     marker in the `finally` below) and opportunistically invokes
 *     SnapshotManager::gcExpired() (C) so orphaned snapshots/asides are
 *     reclaimed even on a `DISABLE_WP_CRON`/dormant site, not cron-only.
 *   - False-rollback regression fix (agent-only, 0.61.18): on an
 *     open_basedir/RunCloud-style host a perfectly GOOD apply was being
 *     auto-reverted here — the completeness re-read (UpdateRunner::isComplete())
 *     busts WordPress's OWN plugin/theme metadata cache but was reading
 *     through a separate, still-stale PHP stat/realpath cache for the
 *     just-swapped directory, producing a false "incomplete" verdict with no
 *     genuine half-write behind it. UpdateRunner::isComplete() now busts that
 *     cache too before its re-read (see its own class doc); this call site is
 *     unchanged except that it now also passes `$available` (the version the
 *     apply targeted) through as an optional third argument purely so a
 *     `false` verdict's DebugLog line can report the expected-vs-on-disk
 *     version — it never influences the verdict.
 *   - Failure-reason visibility fix (agent-only, 0.61.19, GitHub issue #182's
 *     sibling fix): the concrete reason UpdateRunner::isComplete() decided
 *     `false` (validate_plugin() error / basename-unresolved / empty-style-
 *     name, plus the raw on-disk-version-vs-expected diagnostic) used to be
 *     written ONLY to DebugLog, which is a silent no-op without WPMGR_DEBUG —
 *     an operator watching the control plane's "View logs" for a rolled-back
 *     item saw nothing more specific than "Update incomplete; auto-restored
 *     the pre-update snapshot." UpdateRunner::lastIncompleteReason() now
 *     exposes that same text; this method appends it (read ONLY immediately
 *     after an isComplete() call that itself returned false — never
 *     unconditionally, so a stale reason from an earlier item can never leak)
 *     to the rollback log line below. Concise and non-secret (slug/version/WP
 *     error message only); never changes the boolean gate semantics above.
 *   - Stale-availability visibility fix (agent-only, GitHub issue #208):
 *     UpdateRunner::availableVersion() previously read WordPress's cached
 *     update_core/update_plugins/update_themes transient AS-IS, with no
 *     forced fresh check — a momentarily stale, expired, or never-yet-
 *     populated transient was indistinguishable from "genuinely no update
 *     available", so a real pending update could be silently reported here
 *     as `up_to_date` while WordPress's own background auto-updater applied
 *     it moments later. availableVersion() now forces exactly ONE fresh
 *     check first (wp_version_check()/wp_update_plugins()/wp_update_themes(),
 *     the same calls the apply path below already makes) and returns `null`
 *     — distinct from `''` — when availability still cannot be determined
 *     even after that check. `isUpdatable()` never treats `null` as
 *     updatable; a `null` result must never be silently folded into
 *     `up_to_date` either. Two call sites, two different fixes appropriate
 *     to what each may do: dryRun() (which the CP contract forbids from ever
 *     mutating) reports a distinct `unknown` status rather than guessing.
 *     processItem() (the real, non-dry-run apply) instead falls through to
 *     the normal snapshot+apply path unconditionally on `null` — apply()
 *     performs its OWN forced fresh check right before mutating anything, so
 *     a genuine pending update is still applied correctly, and a genuinely
 *     already-current site still safely resolves to `up_to_date` once
 *     to_version is re-read below and found unchanged. This also keeps the
 *     real-apply response status within the control plane's existing
 *     {succeeded,failed,skipped,up_to_date} vocabulary
 *     (apps/api/internal/agentcmd/contract.go) — introducing a brand-new
 *     status there would fall through the CP worker's runApply() switch to
 *     its default "post-update probe healthy => succeeded" branch, which
 *     would misreport an item that was never even attempted as a successful
 *     update: worse than the bug being fixed here.
 *
 * Every input is treated as untrusted: type is whitelisted, slug is sanitized to
 * reject path traversal, and snapshot paths are bounded to wp-content.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\DebugLog;
use WPMgr\Agent\Support\Maintenance;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateGuard;
use WPMgr\Agent\Support\UpdateInFlight;
use WPMgr\Agent\Support\UpdateRunner;
use WPMgr\Agent\Support\UpdateWatchdogMarker;

/**
 * Performs core/plugin/theme updates with optional pre-update snapshots.
 */
final class UpdateCommand implements CommandInterface
{
    /** Valid item types. */
    private const TYPES = ['plugin', 'theme', 'core'];

    /**
     * S4 (issue #131 adversarial review) — bounded-but-generous apply time
     * limit. The original `set_time_limit(0)` (infinite) threw away the ONE
     * timer whose fatal WOULD run register_shutdown_function() callbacks
     * (max_execution_time) in favor of relying solely on PHP-FPM's own
     * `request_terminate_timeout`, which does NOT run shutdown functions at
     * all (SIGTERM resets the timeout handler to SIG_DFL and kills the
     * worker immediately — see UpdateGuard's class doc). 900s (15 minutes)
     * is generous for even a very large plugin/theme copy, while still
     * giving a truly-hung apply a chance to hit PHP's own RECOVERABLE fatal
     * before something else (FPM's timeout, when configured shorter) tears
     * the process down with no recovery at all. See
     * WPMgr\Agent\Support\UpdateInFlight for the out-of-band backstop that
     * covers the case where FPM's own timeout IS shorter than this bound.
     */
    private const APPLY_TIME_LIMIT_SECONDS = 900;

    private SnapshotManager $snapshots;

    private UpdateRunner $runner;

    /**
     * @param SnapshotManager|null $snapshots Snapshot store (defaults to real one).
     * @param UpdateRunner|null    $runner    Update executor (defaults to real one).
     */
    public function __construct(?SnapshotManager $snapshots = null, ?UpdateRunner $runner = null)
    {
        $this->snapshots = $snapshots ?? new SnapshotManager();
        $this->runner    = $runner ?? new UpdateRunner();
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'update';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters.
     * @return array{ok:bool,results:array<int,array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}>}
     */
    public function execute(array $claims, array $params): array
    {
        // B1 (GitHub issue #131) — survive the apply itself. UpdateRunner
        // applies plugin/theme/core updates via WordPress's Plugin_Upgrader /
        // Theme_Upgrader / Core_Upgrader INLINE in this REST request — there
        // is no WP-CLI subprocess and no cron handoff. Their install_package()
        // does a non-atomic recursive copy (clear destination, then copy new
        // files in); the most likely cause of a half-written result is a hard
        // resource kill (max_execution_time or a PHP-FPM
        // request_terminate_timeout) partway through that copy. This mirrors
        // BackupCommand's long-running-job guard.
        //
        // S4 (adversarial review) — bounded, not infinite: see
        // APPLY_TIME_LIMIT_SECONDS's doc for why 0 (infinite) was itself part
        // of the bug.
        @set_time_limit(self::APPLY_TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,Squiz.PHP.DiscouragedFunctions.Discouraged -- bounded-but-generous cap so a hung apply hits a recoverable max_execution_time fatal rather than only an unrecoverable FPM hard-kill; @-guarded, no-op when disabled
        if (function_exists('wp_raise_memory_limit')) {
            wp_raise_memory_limit('admin');
        }
        @ignore_user_abort(true); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- keep applying even if the control plane's HTTP client disconnects mid-request; @-guarded

        // Heal a `.maintenance` flag left behind by a prior interrupted
        // update/rollback before starting new work, and arm the shutdown
        // backstop so a fatal error or a timeout mid-update still clears
        // whatever flag THIS run may leave set.
        Maintenance::healStaleIfPresent();
        Maintenance::armShutdownGuard();

        // S4 (adversarial review) — reconcile any update-in-flight marker
        // left stale by a prior request that was hard-killed severely enough
        // that UpdateGuard's own shutdown hook never ran (see
        // UpdateInFlight's class doc). This is the "next agent request" half
        // of that recovery; HOOK_GC's recurring cron sweep is the other half,
        // covering the case where no further `update` command ever arrives.
        //
        // F2 (final-hardening review) — pass THIS command's own injected
        // $this->runner, not a fresh ad-hoc UpdateRunner, so the F2
        // live-directory health check reconcile now performs uses exactly
        // the same WP-CLI/upgrader-aware runner the rest of this command
        // does (and so a test that injects a runner double controls this
        // path too, rather than always hitting a real UpdateRunner).
        UpdateInFlight::healStaleIfPresent($this->snapshots, $this->runner);

        // C (issue #131 final-hardening review) — invoke the snapshot-store
        // GC backstop OPPORTUNISTICALLY here too, not cron-only. WP-Cron is
        // unreliable on `DISABLE_WP_CRON`/dormant sites (no visitor traffic
        // ever fires the wp-cron.php pseudo-cron request), so an orphaned
        // snapshot — a since-uninstalled/renamed slug, a crashed capture, or
        // a stranded restore-failure aside (F4) — could otherwise sit
        // unreclaimed indefinitely on such a site. Belt-and-suspenders,
        // mirroring the same reasoning already applied to
        // UpdateInFlight::healStaleIfPresent() immediately above (which is
        // itself the opportunistic half of ITS OWN cron-bound counterpart,
        // gcSweep() — already covered here via the injected $this->snapshots
        // call above, so it is deliberately NOT invoked a second time
        // through a fresh, non-injected SnapshotManager instance, which
        // would both duplicate that work and bypass any snapshot test double
        // a caller constructed this command with). Cheap: a bounded,
        // mostly-no-op scandir() pass over a small, self-contained directory.
        SnapshotManager::gcExpired();

        $dryRun   = isset($params['dry_run']) && (bool) $params['dry_run'];
        $snapshot = isset($params['snapshot']) && (bool) $params['snapshot'];

        $items = isset($params['items']) && is_array($params['items']) ? $params['items'] : [];

        $results = [];
        $ok      = true;

        foreach ($items as $item) {
            $result = $this->processItem(is_array($item) ? $item : [], $dryRun, $snapshot);
            if ($result['status'] === 'failed') {
                $ok = false;
            }
            $results[] = $result;
        }

        return ['ok' => $ok, 'results' => $results];
    }

    /**
     * Process a single update item, catching errors so the batch continues.
     *
     * @param array<string,mixed> $item     One request item.
     * @param bool                $dryRun   Whether to avoid mutation.
     * @param bool                $snapshot Whether to capture a pre-update snapshot for a
     *                                       `core` item. Ignored for plugin/theme, which
     *                                       always snapshot — see class doc (D2, issue #131).
     * @return array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}
     */
    private function processItem(array $item, bool $dryRun, bool $snapshot): array
    {
        $type    = isset($item['type']) && is_string($item['type']) ? $item['type'] : '';
        $rawSlug = isset($item['slug']) && is_string($item['slug']) ? $item['slug'] : '';
        $version = isset($item['version']) && is_string($item['version']) && $item['version'] !== ''
            ? $item['version']
            : 'latest';

        // --- Validate type. ---
        if (!in_array($type, self::TYPES, true)) {
            return $this->result('', $rawSlug, '', '', 'failed', '', 'Invalid type.');
        }

        // --- Validate / sanitize slug (rejects path traversal). ---
        if ($type === 'core') {
            $slug = 'core';
        } else {
            $slug = self::sanitizeSlug($rawSlug);
            if ($slug === '' || $slug !== $rawSlug) {
                return $this->result($type, $rawSlug, '', '', 'failed', '', 'Invalid or unsafe slug.');
            }
        }

        // Declared here (rather than inside the inner try below) so the
        // outer catch(\Throwable) can still see a snapshot that WAS captured
        // — and a guard that WAS armed — before apply() threw, and fire the
        // exact same restore path the synchronous incomplete-apply case uses.
        $snapshotId = '';
        $guard      = null;
        // B (issue #131 final-hardening review) — the UpdateInFlight liveness
        // lock returned by UpdateInFlight::mark(); released in the `finally`
        // below alongside UpdateInFlight::clear(). See UpdateInFlight's class
        // doc for why this must stay open for the whole apply.
        $inFlightLock = null;

        try {
            // --- Not-applicable target: the plugin/theme isn't present on
            // this site at all (a stale or mistargeted task). Report this as
            // skipped, not failed, so it never pollutes the run's failure
            // count — there is nothing here to have failed.
            if (!$this->runner->isInstalled($type, $slug)) {
                return $this->result($type, $slug, '', '', 'skipped', '', 'Not installed on this site.');
            }

            $fromVersion = $this->runner->currentVersion($type, $slug);

            if ($dryRun) {
                return $this->dryRun($type, $slug, $version, $fromVersion);
            }

            // --- Not-applicable target: installed but nothing to do (already
            // at, or beyond, the requested/available version). Report this
            // up front as up_to_date without running a snapshot+apply — both
            // avoids needless work and keeps a target with no real update to
            // apply from ever being able to surface as a failure.
            //
            // GitHub issue #208 — $available === null means UpdateRunner
            // could not determine availability AT ALL, even after forcing a
            // fresh WordPress update check (see
            // UpdateRunner::availableVersion()'s doc). This must never be
            // treated as "up to date": isUpdatable() only ever returns true
            // for a genuinely available, newer version, so the guard below
            // is skipped entirely on null rather than short-circuiting —
            // execution falls through to the normal snapshot+apply path,
            // whose own apply() call performs its OWN forced fresh check
            // before mutating anything (see this class's doc for the full
            // rationale, including why the real-apply path deliberately does
            // NOT introduce a new status here).
            $available = $this->runner->availableVersion($type, $slug, $version);
            if ($available !== null && !self::isUpdatable($available, $fromVersion, $version)) {
                return $this->result(
                    $type,
                    $slug,
                    $fromVersion,
                    $fromVersion,
                    'up_to_date',
                    '',
                    'Already up to date; no update applied.'
                );
            }

            // GUARANTEE: whatever this item's snapshot/apply work does below —
            // succeed, fail, or throw — a `finally` clears any `.maintenance`
            // flag before we move on, so a failed item can never leave the
            // whole site dark for the rest of the batch (or indefinitely).
            try {
                $log = '';

                // --- Pre-update snapshot (D2, GitHub issue #131) -----------
                // plugin/theme: ALWAYS captured, regardless of the request's
                // `snapshot` flag — it is the precondition for the
                // auto-restore below and a cheap same-filesystem copy under
                // wpmgr-snapshots. `core` keeps the original opt-in behavior:
                // there is no directory to snapshot/restore for core, and D3
                // deliberately does not build one (see UpdateRunner::isComplete()).
                $shouldSnapshot = $type === 'core' ? $snapshot : true;
                if ($shouldSnapshot) {
                    $snap        = $this->snapshots->capture($type, $slug, $fromVersion);
                    $snapshotId  = $snap['snapshot_id'];
                    $log        .= $snap['log'];
                }

                // --- S8, revised (agent-only regression fix): degrade
                // gracefully instead of refusing the apply -----------------
                // plugin/theme ALWAYS expects a snapshot (shouldSnapshot is
                // unconditionally true above). S8 originally REFUSED
                // (`failed`) the item outright when capture failed here,
                // rather than ever falling through to an unguarded apply().
                // That refusal itself became the bug: on a healthy site
                // where SnapshotManager::liveDir()'s realpath()-based
                // containment check failed (open_basedir excluding a
                // parent, or a symlinked/relocated wp-content tree —
                // liveDir() now has a safe anchored fallback for exactly
                // this, see its class doc), EVERY plugin/theme update
                // refused here while core (exempt from S8) kept applying
                // fine. capture() can still legitimately fail for other
                // reasons (a disk-constrained host, a genuinely missing
                // source directory); for those, PROCEED with a best-effort,
                // unprotected apply rather than block it — this matches
                // capture()'s own "...proceeding without snapshot" log
                // intent below, which this refusal previously contradicted.
                // No guard is armed and no in-flight marker is written for
                // this item (both remain gated on `$snapshotId !== ''`
                // below) — there is nothing to roll back to — so this apply
                // instead relies on WordPress core's own
                // Plugin_Upgrader/Theme_Upgrader temp-backup/rollback (WP
                // 6.3+), the post-apply isComplete() verify further down,
                // and the control plane's post-update health-probe +
                // auto-rollback. `core` was already exempt from this gate —
                // D3 gives it no directory-level snapshot/restore
                // protection by design (it relies on B1 + WordPress's own
                // Core_Upgrader temp-backup mechanism instead), so a core
                // snapshot capture failure (only reachable when
                // `snapshot=true` was requested at all) never blocked a
                // core apply that never depended on it, and still doesn't.
                if ($type !== 'core' && $snapshotId === '') {
                    $log .= "\nApplied without a pre-update snapshot (host could not capture one).";
                }

                // --- Arm the shutdown-time restore backstop (D2) -----------
                // Only for plugin/theme with a successfully captured
                // directory snapshot — core never gets one (D3). WordPress's
                // Plugin_Upgrader/Theme_Upgrader apply via
                // install_package()/copy_dir(), a NON-atomic recursive copy
                // that clears the destination FIRST; a hard resource kill
                // mid-copy tears the whole PHP process down without
                // unwinding any try/catch/finally, so this registered
                // shutdown callback is the only code that still runs
                // afterwards. See UpdateGuard's class doc for the full
                // arm/markClean/fire lifecycle.
                if ($type !== 'core' && $snapshotId !== '') {
                    $guard = new UpdateGuard($this->snapshots, $type, $slug, $snapshotId);
                    $guard->arm();

                    // S4 (adversarial review) — persist the out-of-band
                    // reconcile marker BEFORE apply() so a kill severe
                    // enough that the shutdown guard above never runs either
                    // (a PHP-FPM request_terminate_timeout SIGTERM) is still
                    // recoverable later. See UpdateInFlight's class doc.
                    //
                    // B (final-hardening review) — mark() also acquires and
                    // returns the liveness lock this marker is paired with;
                    // it MUST stay held (open) through apply() below and is
                    // only released in this method's `finally`.
                    $inFlightLock = UpdateInFlight::mark($type, $slug, $snapshotId);
                }

                // --- Apply the update. ---
                $applied = $this->runner->apply($type, $slug, $version);
                $log    .= ($log !== '' ? "\n" : '') . $applied['log'];

                // --- Post-apply verification (S7, issue #131 adversarial
                // review) — currentVersion()/isComplete() run AFTER a
                // successful apply() but before the guard is told the result
                // is clean, still inside this same try/finally. A THROWN
                // error here is evidence that VERIFICATION is broken, not
                // that the apply itself was bad — rolling back a perfectly
                // good update because the code checking it happened to throw
                // would be strictly worse than keeping the update and
                // logging a warning. Only an AFFIRMATIVE incomplete
                // ($complete === false) may trigger the restore below; a
                // throw is treated as inconclusive and the update is KEPT.
                // The CP's own post-update health probe remains the backstop
                // for a genuinely bad apply that happens to also break
                // verification.
                $toVersion        = $fromVersion;
                $complete         = false;
                // GitHub issue #182's sibling visibility fix (agent-only,
                // 0.61.19): the concrete reason isComplete() decided `false`,
                // read ONLY immediately after a call that itself returned
                // false — see UpdateRunner::lastIncompleteReason()'s doc for
                // why this must never be read unconditionally.
                $incompleteReason = '';
                try {
                    $toVersion = $this->runner->currentVersion($type, $slug);
                    // Preserves the original short-circuit intent (a genuine
                    // upgrader failure never pays for the extra WP API round
                    // trip isComplete() costs), but as an explicit `if` rather
                    // than `&&` so lastIncompleteReason() is only ever read
                    // right after an isComplete() call that actually ran.
                    // $available (the version this apply targeted) is passed
                    // through only to enrich isComplete()'s diagnostic on a
                    // `false` verdict (agent-only regression fix, 0.61.18) —
                    // it never affects the verdict itself. Coerced to '' when
                    // null (GitHub issue #208's undetermined-availability
                    // case) since isComplete()'s $expectedVersion parameter
                    // is a plain string; its own diagnostic already renders
                    // '' as "expected version unknown", which is accurate here.
                    if ($applied['ok']) {
                        $complete = $this->runner->isComplete($type, $slug, $available ?? '');
                        if (!$complete) {
                            $incompleteReason = $this->runner->lastIncompleteReason();
                        }
                    }
                } catch (\Throwable $verifyError) {
                    DebugLog::write(
                        'WPMgr Agent: post-apply verification threw for ' . $type . ':' . $slug
                        . ': ' . $verifyError->getMessage() . '.'
                    );
                    if ($applied['ok']) {
                        // The apply ITSELF reported success; a throw while
                        // verifying it afterward is evidence verification is
                        // broken, not that the apply was bad — keep the
                        // update rather than false-roll-it-back. Only an
                        // AFFIRMATIVE incomplete (never a throw) may trigger
                        // the restore below.
                        $log     .= "\nPost-apply verification error; update KEPT (not rolled back) — verification itself failed, not the apply.";
                        $complete = true;
                    } else {
                        // The apply had already reported failure on its own
                        // terms — this still proceeds through the normal
                        // incomplete/failed/restore path below; the throw
                        // only means to_version could not be re-read here (it
                        // may still be corrected below if a restore runs).
                        $log .= "\nCould not re-read the installed version after a failed apply.";
                    }
                }

                if ($complete) {
                    // Verified-good: tell the guard (if armed) not to
                    // restore. MUST happen before returning — an un-cleared
                    // guard would otherwise undo a perfectly good update if
                    // this request is torn down by something unrelated later
                    // in the same shutdown chain.
                    if ($guard !== null) {
                        $guard->markClean();
                    }

                    $status = $toVersion !== $fromVersion ? 'succeeded' : 'up_to_date';

                    // GitHub issue #210 — arm the update-watchdog mu-plugin's
                    // marker ONLY for a genuinely applied, verified-good
                    // plugin/theme change: never `up_to_date` (nothing
                    // changed, nothing to guard against), never `core` (no
                    // directory-level snapshot exists for core — see this
                    // class's own D3). Best-effort and isolated in its own
                    // method so a resolution failure can never affect this
                    // response — see UpdateWatchdogMarker::arm()'s own doc.
                    if ($type !== 'core' && $status === 'succeeded' && $snapshotId !== '') {
                        $this->armUpdateWatchdog($type, $slug, $snapshotId, $toVersion);

                        // GitHub issue #226 — mark this snapshot's meta as
                        // succeeded so SnapshotManager::gcExpired()'s two-tier
                        // reclaim can drop it in ~1h instead of waiting out
                        // the full 72h orphan backstop (the root cause of
                        // #226: a successful update never had a reclaim path
                        // of its own). Orthogonal to arming the watchdog
                        // above — the snapshot must still exist for that —
                        // and gated on the exact same "genuinely succeeded AND
                        // a snapshot was captured" condition. Best-effort:
                        // markSucceeded() itself never throws; a failure here
                        // just leaves this snapshot on the unchanged 72h
                        // backstop.
                        $this->snapshots->markSucceeded($snapshotId);
                    }

                    return $this->result($type, $slug, $fromVersion, $toVersion, $status, $snapshotId, $log);
                }

                // GitHub issue #182's sibling visibility fix (agent-only,
                // 0.61.19): append the concrete isComplete() reason — the same
                // facts UpdateRunner already writes to DebugLog — to the
                // CP-visible item log below, so "View logs" explains WHY an
                // apply that reported success was still rolled back, without
                // requiring WPMGR_DEBUG. Empty when the incompleteness came
                // from $applied['ok'] === false instead (that case already has
                // its own descriptive $applied['log'] appended above) or when
                // isComplete() itself threw (S7; never treated as incomplete).
                $reasonSuffix = $incompleteReason !== '' ? ' Reason: ' . $incompleteReason . '.' : '';

                // Incomplete or failed apply: roll back synchronously right
                // now rather than waiting on the shutdown backstop, so the
                // directory is restored before we even respond to the
                // control plane. fire() is idempotent, so a later real
                // shutdown invocation of the same (already-fired) guard is
                // always safe.
                if ($guard !== null) {
                    $restore = $guard->fire();
                    if ($restore['fired']) {
                        $log .= "\n" . ($restore['ok']
                            ? 'Update incomplete; auto-restored the pre-update snapshot.' . $reasonSuffix
                            : 'Update incomplete; auto-restore FAILED: ' . $restore['log'] . $reasonSuffix);
                        // Re-read the version now the directory should be
                        // back to its pre-update state, so the response
                        // reflects what is actually on disk.
                        $toVersion = $this->runner->currentVersion($type, $slug);
                    }
                } else {
                    $log .= "\nUpdate incomplete; no pre-update snapshot was available to auto-restore." . $reasonSuffix;
                }

                return $this->result($type, $slug, $fromVersion, $toVersion, 'failed', $snapshotId, $log);
            } finally {
                Maintenance::clear();

                // S4 (adversarial review) — every SYNCHRONOUS outcome that
                // reaches this `finally` (success, a synchronous
                // incomplete-restore, or a throw about to be caught by the
                // outer catch below) means this item's fate was resolved
                // in-process, so the out-of-band reconcile marker is no
                // longer needed. A marker survives past this point ONLY when
                // the request never reached here at all — the exact hard-kill
                // case UpdateInFlight::healStaleIfPresent() exists to recover
                // later. Safe to call unconditionally even when mark() was
                // never written for this item (e.g. type === 'core', or the
                // S8 refusal above returned before arming anything) — clear()
                // is a no-op when there is nothing to clear.
                //
                // B (final-hardening review) — release the liveness lock
                // FIRST: by the time the marker is gone, its lock must
                // already be free too. release() is a safe no-op when
                // $inFlightLock is still null (mark() was never reached, or
                // itself could not acquire the lock).
                UpdateInFlight::release($inFlightLock);
                UpdateInFlight::clear($type, $slug);
            }
        } catch (\Throwable $e) {
            // A thrown exception mid-apply (rather than a returned
            // ok:false) still needs the same rollback: fire the guard
            // synchronously here — same idempotent method the completeness
            // check above and the shutdown backstop both use — rather than
            // only relying on the shutdown path.
            if ($guard !== null) {
                $guard->fire();
            }

            // Never leak internals; keep the per-item failure contained.
            return $this->result($type, $slug, '', '', 'failed', $snapshotId, 'Update error.');
        }
    }

    /**
     * GitHub issue #210 — resolve this apply's absolute live/payload paths
     * via SnapshotManager (the single source of truth for that path
     * resolution; see resolvedRestorePaths()'s own doc) and, only when both
     * resolve, persist the update-watchdog marker. Isolated into its own
     * method — rather than inlined at the call site — precisely so that any
     * failure here can never bubble into, or otherwise affect, the
     * already-decided `succeeded` response for the item that triggered it.
     *
     * @param string $type       'plugin'|'theme'.
     * @param string $slug       Sanitized slug.
     * @param string $snapshotId Snapshot identifier captured for this apply.
     * @param string $toVersion  The version this apply moved the item to —
     *                            recorded on the marker so MEDIUM-1b's
     *                            healthy-boot disarm (see
     *                            UpdateWatchdogMarker::disarmHealthy()) can
     *                            confirm the on-disk version still matches
     *                            before clearing the marker early.
     * @return void
     */
    private function armUpdateWatchdog(string $type, string $slug, string $snapshotId, string $toVersion): void
    {
        try {
            $paths = $this->snapshots->resolvedRestorePaths($type, $slug, $snapshotId);
        } catch (\Throwable $e) {
            DebugLog::write(
                'WPMgr Agent: update-watchdog path resolution threw for ' . $type . ':' . $slug
                . ': ' . $e->getMessage()
            );
            return;
        }

        if ($paths['live'] === '' || $paths['payload'] === '') {
            return;
        }

        UpdateWatchdogMarker::arm($type, $slug, $snapshotId, $paths['live'], $paths['payload'], $toVersion);
    }

    /**
     * Compute the dry-run outcome without touching the filesystem.
     *
     * @param string $type        Item type.
     * @param string $slug        Sanitized slug.
     * @param string $requested   Requested version ('latest' or x.y.z).
     * @param string $fromVersion Currently installed version.
     * @return array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}
     */
    private function dryRun(string $type, string $slug, string $requested, string $fromVersion): array
    {
        $available = $this->runner->availableVersion($type, $slug, $requested);

        // GitHub issue #208 — a dry run may never mutate the site (the
        // control-plane contract's hard rule: "dry_run true => the agent
        // MUST NOT mutate the site"), so unlike processItem()'s real-apply
        // path there is no forced-fresh-check-on-apply() to fall back on
        // here. Report a status distinct from BOTH `up_to_date` (would
        // falsely claim we know nothing is pending) and `would_update`
        // (would falsely claim we know something IS pending) so a dry-run
        // report never silently misrepresents an undetermined result as
        // either. Safe for the control plane's existing dry-run handling,
        // which already treats any status other than `would_update` as
        // "nothing to report" informationally, without mutating anything
        // either way.
        if ($available === null) {
            return $this->result(
                $type,
                $slug,
                $fromVersion,
                $fromVersion,
                'unknown',
                '',
                'Dry run: could not determine update availability.'
            );
        }

        $updatable = self::isUpdatable($available, $fromVersion, $requested);

        $status = $updatable ? 'would_update' : 'up_to_date';
        $to     = $updatable ? $available : $fromVersion;

        return $this->result($type, $slug, $fromVersion, $to, $status, '', 'Dry run: no changes applied.');
    }

    /**
     * Is there an actual update to apply?
     *
     * Shared by the dry-run report and the real-apply pre-check so both agree
     * on what "nothing to do" means: no available version, the available
     * version already matches what's installed, or (for a 'latest' request)
     * the available version isn't actually newer.
     *
     * GitHub issue #208 — deliberately accepts `$available === null` (the
     * "availability could not be determined at all" signal from
     * UpdateRunner::availableVersion(), distinct from '') and always returns
     * `false` for it, exactly as it already does for `''`. This method only
     * ever answers "should an apply be attempted right now"; it never
     * conflates "unknown" with "definitely up to date" — that distinction is
     * the CALLER's responsibility (see both call sites' surrounding comments
     * for how processItem() and dryRun() report `null` differently).
     *
     * @param string|null $available   Version an update would move to ('' when
     *                                  a fresh check confirms none is
     *                                  available; null when undetermined).
     * @param string      $fromVersion Currently installed version.
     * @param string      $requested   'latest' or an explicit x.y.z.
     * @return bool
     */
    private static function isUpdatable(?string $available, string $fromVersion, string $requested): bool
    {
        if ($available === null || $available === '') {
            return false;
        }

        return $available !== $fromVersion
            && (version_compare($available, $fromVersion, '>') || $requested !== 'latest');
    }

    /**
     * Build a single normalized result row matching the contract shape exactly.
     *
     * @param string $type        Item type.
     * @param string $slug        Slug.
     * @param string $fromVersion From version.
     * @param string $toVersion   To version.
     * @param string $status      Status enum.
     * @param string $snapshotId  Snapshot id (or empty).
     * @param string $log         Concise log (no secrets).
     * @return array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}
     */
    private function result(
        string $type,
        string $slug,
        string $fromVersion,
        string $toVersion,
        string $status,
        string $snapshotId,
        string $log
    ): array {
        return [
            'type'         => $type,
            'slug'         => $slug,
            'from_version' => $fromVersion,
            'to_version'   => $toVersion,
            'status'       => $status,
            'snapshot_id'  => $snapshotId,
            'log'          => $log,
        ];
    }

    /**
     * Sanitize an untrusted slug, rejecting anything that could escape its
     * intended directory. Accepts a plugin folder, a "folder/file.php" plugin
     * basename, or a theme stylesheet.
     *
     * @param string $slug Raw slug from the request body.
     * @return string Sanitized slug, or '' when unsafe.
     */
    public static function sanitizeSlug(string $slug): string
    {
        $slug = trim($slug);
        if ($slug === '') {
            return '';
        }

        // Reject path traversal and absolute paths outright.
        if (str_contains($slug, '..') || str_contains($slug, "\0")) {
            return '';
        }
        if ($slug[0] === '/' || $slug[0] === '\\') {
            return '';
        }
        // Reject Windows drive-letter absolute paths (e.g. C:\...).
        if (preg_match('#^[A-Za-z]:#', $slug) === 1) {
            return '';
        }

        // Allow only: alphanumerics, dash, underscore, dot, and a single forward
        // slash separating folder/file (plugin basename form).
        if (preg_match('#^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$#', $slug) !== 1) {
            return '';
        }

        return $slug;
    }
}
