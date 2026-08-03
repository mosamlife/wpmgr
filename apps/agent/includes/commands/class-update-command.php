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
 *   - Per-site serialisation and the restore-skip (GitHub issue #328): the
 *     control plane sends one item per command but runs several commands
 *     against one site at once, and WordPress core is not built for that -
 *     unpack_package() deletes EVERY entry under wp-content/upgrade/ as its
 *     first act, so a second upgrader destroys the first one's extracted
 *     source mid-flight. execute() therefore takes a per-SITE update lock
 *     (WPMgr\Agent\Support\SiteUpdateLock, core's own create_lock primitive)
 *     before anything that writes, holds it for the whole command, renews it
 *     between items and releases it in a `finally`. A command that arrives
 *     while another update, rollback or agent upgrade holds it is refused with
 *     the `site_busy` status, having written NOTHING at all. Separately, an
 *     update that failed BEFORE core could touch the destination no longer
 *     gets a precautionary restore over a directory nobody modified: see the
 *     four-combination fail-safe table at the decision site, and
 *     WPMgr\Agent\Support\UpdateOutcome plus
 *     WPMgr\Agent\Support\DestinationVerifier for the two independent
 *     conditions that must both hold before a restore is skipped.
 *   - Self-target refusal (agent-only hardening): an item that resolves to
 *     the agent's own plugin is refused outright, before any detection,
 *     download, staging or write, and reported with the existing `skipped`
 *     status. Applying an update to the agent from inside a control-plane
 *     command means deleting and re-copying the very directory that is
 *     running the request and still has to serialize its response, with the
 *     rollback watchdog unavailable by design (see UpdateWatchdogMarker::arm()),
 *     so success and a bricked site become indistinguishable to the control
 *     plane. The agent's own updates travel over its dedicated update channel.
 *     See isSelfTarget() for how a renamed, symlinked or differently-cased
 *     install directory is matched as well as the stock plugin key.
 *
 * Every input is treated as untrusted: type is whitelisted, slug is sanitized to
 * reject path traversal, and snapshot paths are bounded to wp-content.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\DebugLog;
use WPMgr\Agent\Support\DestinationVerifier;
use WPMgr\Agent\Support\Maintenance;
use WPMgr\Agent\Support\SiteUpdateLock;
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
     * The agent's own plugin FOLDER name in a stock install. Deliberately a
     * self-contained literal rather than a reference to UpdateChecker (the
     * wp.org build ships without that class entirely), so the self-target
     * refusal below is present in every build. An install whose directory was
     * renamed is covered by the WPMGR_AGENT_DIR resolution in isSelfTarget(),
     * not by this constant.
     */
    private const SELF_PLUGIN_FOLDER = 'wpmgr-agent';

    /** The agent's own plugin key ("folder/main-file.php") in a stock install. */
    private const SELF_PLUGIN_KEY = self::SELF_PLUGIN_FOLDER . '/' . self::SELF_PLUGIN_FOLDER . '.php';

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

    /**
     * Per-item status meaning "this site is already running another update,
     * rollback or agent upgrade; NOTHING was attempted and NOTHING on this site
     * was changed" (GitHub issue #328).
     *
     * THE WIRE CONTRACT, pinned and not negotiable here: the control plane
     * declares the same literal beside its other item statuses and, on seeing
     * it, defers the task back to `pending` and retries later rather than
     * producing any terminal status. Renaming this string on one side only is a
     * build error on the other.
     *
     * WHY THE COMMAND STILL ANSWERS ok=false ALONGSIDE IT. An older control
     * plane does not know this status. Its per-item switch has no case for it,
     * so an unknown status with ok=TRUE would fall through to the post-probe
     * branch and could record an update that never ran as SUCCEEDED. With
     * ok=false the same old control plane takes its "agent rejected the update
     * command" path and carries the log sentence below to the operator, which
     * is wrong-but-safe rather than wrong-and-silent. A newer control plane
     * must therefore test for this status BEFORE it tests ok, and its own
     * comment says so.
     */
    private const STATUS_SITE_BUSY = 'site_busy';

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

        // GitHub issue #328 - PARSE THE REQUEST BEFORE DOING ANYTHING THAT
        // WRITES. This ordering is a bug fix, not a tidy-up. `dry_run` used to
        // be read AFTER Maintenance::healStaleIfPresent() (which deletes a
        // file), Maintenance::armShutdownGuard() (which registers a callback
        // that deletes ANY .maintenance flag it finds at shutdown, including
        // one a human's wp-admin bulk update legitimately owns),
        // UpdateInFlight::healStaleIfPresent() (which can perform a FULL
        // DIRECTORY RESTORE) and SnapshotManager::gcExpired() (which deletes).
        // Every one of those violated the control-plane contract's hard rule
        // that a dry run must not mutate the site.
        $dryRun   = isset($params['dry_run']) && (bool) $params['dry_run'];
        $snapshot = isset($params['snapshot']) && (bool) $params['snapshot'];
        $items    = isset($params['items']) && is_array($params['items']) ? $params['items'] : [];

        if ($dryRun) {
            // A dry run reads and reports. It takes NO site lock either: it
            // mutates no file, activation state or snapshot, so it needs no
            // mutual exclusion; taking the lock would let a preview BLOCK a
            // real apply, and being refused by one would make a preview fail
            // for no benefit. A dry run running alongside somebody else's
            // apply may read a mid-swap version, which is a slightly wrong
            // preview and never a wrong mutation.
            //
            // THE ONE HONEST EXCEPTION, named rather than hidden: the
            // availability lookup below forces exactly one fresh WordPress
            // update check per run, which deletes and repopulates the
            // update_* transients. That is a write to WordPress's own cache
            // and it is required for an accurate answer (GitHub issue #208:
            // reading the cached transient made a real pending update look
            // up_to_date). It touches no plugin, theme or snapshot.
            return $this->runItems($items, true, $snapshot);
        }

        // --- PIECE 2, per-site serialisation (GitHub issue #328) ----------
        // ONE update writer per site. Taken here, above every mutating call
        // below, and released in the `finally` at the end of this method: one
        // command, one hold, never per item. See SiteUpdateLock's class doc for
        // why the third outcome (UNAVAILABLE) proceeds rather than refuses.
        $lock = SiteUpdateLock::acquire();
        if ($lock === SiteUpdateLock::HELD_BY_OTHER) {
            // NOTHING below this line runs. No flag is healed, no shutdown
            // guard is armed, no marker is reconciled, no snapshot is GC'd, no
            // snapshot is captured and no upgrader is constructed. That is the
            // whole point of refusing here rather than three calls later.
            return $this->refuseSiteBusy($items);
        }

        try {
            // Heal a `.maintenance` flag left behind by a prior interrupted
            // update/rollback before starting new work, and arm the shutdown
            // backstop so a fatal error or a timeout mid-update still clears
            // whatever flag THIS run may leave set.
            //
            // BELOW THE LOCK ON PURPOSE (GitHub issue #328): the shutdown
            // backstop clears ANY .maintenance file it finds, so arming it in a
            // command that turns out to have nothing to do would silently strip
            // a flag another in-flight upgrade legitimately owns.
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
            //
            // Losing this on the dry-run path above costs nothing: Plugin
            // already binds maybeGcSnapshots() to `plugins_loaded` on every
            // enrolled request, so the sweep still happens cron-independently.
            SnapshotManager::gcExpired();

            return $this->runItems($items, false, $snapshot);
        } finally {
            SiteUpdateLock::release();
        }
    }

    /**
     * Run every requested item, in order, under whatever locking the caller has
     * already established.
     *
     * @param array<int,mixed> $items    Raw request items.
     * @param bool             $dryRun   Whether to avoid mutation.
     * @param bool             $snapshot Whether a `core` item should record its version.
     * @return array{ok:bool,results:array<int,array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}>}
     */
    private function runItems(array $items, bool $dryRun, bool $snapshot): array
    {
        $results = [];
        $ok      = true;

        foreach ($items as $item) {
            if (!$dryRun) {
                // ONE COMMAND, ONE HOLD, restamped between items so a
                // legitimately long multi-item command cannot expire its own
                // lock mid-run. Effective TTL becomes 900s since the last item
                // boundary. A no-op when this request does not hold the lock.
                SiteUpdateLock::renew();
            }

            $result = $this->processItem(is_array($item) ? $item : [], $dryRun, $snapshot);
            if ($result['status'] === 'failed' || $result['status'] === self::STATUS_SITE_BUSY) {
                $ok = false;
            }
            $results[] = $result;
        }

        return ['ok' => $ok, 'results' => $results];
    }

    /**
     * Refuse every item in a command that arrived while another update,
     * rollback or agent upgrade already holds this site's update lock.
     *
     * Writes NOTHING. The whole value of this path is that it is reached before
     * any snapshot, marker, flag or upgrader exists, so a refused command is
     * indistinguishable on disk from a command that never arrived.
     *
     * @param array<int,mixed> $items Raw request items.
     * @return array{ok:bool,results:array<int,array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}>}
     */
    private function refuseSiteBusy(array $items): array
    {
        $heldUntil = SiteUpdateLock::heldUntil();
        DebugLog::write(
            'WPMgr Agent: update command refused, this site is already running another update'
            . ($heldUntil > 0 ? ' (lock self-expires at ' . $heldUntil . ')' : '') . '.'
        );

        $results = [];
        foreach ($items as $item) {
            $entry = is_array($item) ? $item : [];
            $type  = isset($entry['type']) && is_string($entry['type']) ? $entry['type'] : '';
            $slug  = isset($entry['slug']) && is_string($entry['slug']) ? $entry['slug'] : '';

            $results[] = $this->result(
                $type,
                $slug,
                '',
                '',
                self::STATUS_SITE_BUSY,
                '',
                'Another update, rollback or agent upgrade is already running on this site. '
                . 'Nothing was attempted and nothing on this site was changed. '
                . 'This will be retried automatically.'
            );
        }

        return ['ok' => false, 'results' => $results];
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

        // --- Self-target refusal (agent-only hardening) ------------------
        // A control-plane update task may never point at the agent's own
        // plugin. WordPress applies an update by clearing and re-copying the
        // target directory, so an update aimed here would delete the code
        // that is still running this request and still has to serialize its
        // own response: the control plane could not tell a finished update
        // apart from a bricked site, and no rollback would be deliverable
        // because the receiver is the thing that broke. That is also why
        // UpdateWatchdogMarker::arm() refuses to arm the rollback watchdog
        // against this directory at all, leaving such an apply unprotected on
        // top of everything else. Refused HERE, before installed/availability
        // detection and long before anything is downloaded, staged or
        // written, and on every item path including dry runs. Reported with
        // the existing `skipped` status (never a new one) so the control
        // plane records "nothing to do" rather than a failure. The agent's
        // own updates travel over its dedicated update channel instead.
        if (self::isSelfTarget($type, $slug)) {
            return $this->result(
                $type,
                $slug,
                '',
                '',
                'skipped',
                '',
                'Refused: this target is the management agent itself. The agent updates through its own update channel, not through a plugin update task.'
            );
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

                // --- PIECE 1, the restore-skip (GitHub issue #328) ---------
                // Do not restore over a directory WordPress provably never
                // opened. Two independent conditions must BOTH hold, and each
                // one alone is deliberately not enough:
                //   (1) the classification, from core's own error code, says
                //       the failure happened above install_package()'s first
                //       destructive line (UpdateOutcome's two-table doc);
                //   (2) the destination, re-read from disk right now, still
                //       parses as this exact target at its pre-update version
                //       (DestinationVerifier).
                // (1) is an inference about code paths. (2) is an observation.
                // The skip is irreversible in the only sense that matters -
                // nothing downstream restores what we decline to restore - so
                // it requires the observation, not merely the absence of a
                // contradiction.
                //
                // FAIL-SAFE DIRECTION, all four combinations:
                //   classification null (any verification)   -> RESTORE
                //   classification false, verification null  -> RESTORE
                //   classification false, verification false -> RESTORE
                //   classification false, verification true  -> SKIP  (only)
                // A missing `destination_touched` key (an older or minimal
                // runner double) reads as null and therefore restores, which
                // is exactly the pre-0.61.114 behaviour. This release can only
                // ever NARROW the set of restores, never widen it.
                $touched  = $applied['destination_touched'] ?? null;
                $skip     = false;
                $verify   = ['verdict' => null, 'detail' => 'not evaluated', 'signals' => []];
                // Whether the gate was even EVALUATED. Every sentence this
                // decision can add to the item log is gated on this, so a path
                // it never looked at (a `core` item, an ok apply that only
                // failed verification, an unclassified failure) keeps the
                // previous release's log byte for byte.
                $gateOpen = $type !== 'core' && ($applied['ok'] ?? null) === false && $touched === false;

                if ($gateOpen) {
                    $payload = $snapshotId !== '' ? $this->snapshots->payloadDir($snapshotId) : '';
                    $verify  = DestinationVerifier::verify($type, $slug, $fromVersion, $payload);
                    $skip    = $verify['verdict'] === true;

                    DebugLog::write(
                        'WPMgr Agent: restore decision for ' . $type . ':' . $slug
                        . ' classification=untouched code=' . (string) ($applied['failure_code'] ?? '')
                        . ' stage=' . (string) ($applied['failure_stage'] ?? '')
                        . ' verification=' . self::verdictLabel($verify['verdict'])
                        . ' signals=' . implode(',', $verify['signals'])
                        . ' action=' . ($skip ? 'skip-restore' : 'restore')
                        . ' detail=' . $verify['detail']
                    );
                }

                if ($skip) {
                    // NO fire(): stand the shutdown backstop down instead, so
                    // it cannot restore later in this same request's teardown.
                    // The null-guard is mandatory: $guard is only constructed
                    // when a snapshot was captured, and this path is reachable
                    // on any host where capture() failed.
                    if ($guard !== null) {
                        $guard->standDown('pre-install failure, destination verified intact');
                    }

                    // NEVER markSucceeded() here. Its own doc forbids calling
                    // it before the update it documents has been independently
                    // verified complete, and it would flip the snapshot's
                    // reclaim threshold from 72h to 1h - deleting the one piece
                    // of evidence that would prove this decision wrong, within
                    // an hour of a FAILED update. markRestoreSkipped() changes
                    // no TTL.
                    if ($snapshotId !== '') {
                        $this->snapshots->markRestoreSkipped(
                            $snapshotId,
                            (string) ($applied['failure_code'] ?? ''),
                            (string) $verify['detail']
                        );
                    }

                    $log .= "\n" . self::skipCopy($type, $applied, $verify, $snapshotId !== '');
                    $log .= $this->reactivationCopy($type, $slug, $applied);

                    // to_version stays at from_version (nothing moved) and the
                    // status stays `failed` (the update genuinely did not
                    // happen). Only the restore is skipped.
                    return $this->result($type, $slug, $fromVersion, $fromVersion, 'failed', $snapshotId, $log);
                }

                if ($gateOpen && $verify['verdict'] === false) {
                    $log .= "\n" . self::restoreBecauseModifiedCopy($type, $applied, $verify);
                } elseif ($gateOpen) {
                    // Classified untouched, but this host could not confirm it.
                    $log .= "\n" . self::restoreBecauseUnverifiedCopy($type, $applied, $verify);
                }

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
     * OPERATOR COPY, GitHub issue #328. Four outcomes, four sentences.
     *
     * HARD INVARIANT, enforced by a test: the string "Update incomplete;
     * auto-restored the pre-update snapshot." must NEVER appear in an item log
     * where UpdateGuard::fire() did not run. That sentence used to be the only
     * thing an operator saw for this whole class of failure, and on the skip
     * path it would be a plain lie. None of the sentences below contain it, and
     * the skip path returns before the restore block that does.
     *
     * The existing restore wording (D5 below) is left BYTE-IDENTICAL for every
     * outcome that does not open the new gate, which is what makes this release
     * auditable as a strict narrowing rather than a rewrite.
     *
     *   D1 skipped, snapshot kept   this method
     *   D2 skipped, no snapshot     this method
     *   D3 restored, modified       restoreBecauseModifiedCopy()
     *   D4 restored, unverifiable   restoreBecauseUnverifiedCopy()
     *   D5 gate closed              unchanged, see the restore block
     *   D6 reactivation             reactivationCopy()
     *   D7 site busy                refuseSiteBusy()
     *
     * @param string                    $type        'plugin'|'theme'.
     * @param array<string,mixed>       $applied     The runner's apply outcome.
     * @param array{verdict:bool|null,detail:string,signals:array<int,string>} $verify Verification result.
     * @param bool                      $hasSnapshot Whether a snapshot exists to keep.
     * @return string
     */
    private static function skipCopy(string $type, array $applied, array $verify, bool $hasSnapshot): string
    {
        $noun = $type === 'theme' ? 'theme' : 'plugin';

        return 'Update did not start. The ' . $noun . ' directory was re-checked after the failure and is '
            . 'unchanged, so no restore was ' . ($hasSnapshot ? 'performed' : 'needed') . '. '
            . 'Reason: ' . self::failureReason($applied) . '. '
            . 'Check: ' . $verify['detail'] . '.'
            . ($hasSnapshot ? ' The pre-update snapshot was kept.' : '');
    }

    /**
     * D3: the classification said the destination was untouched, but the disk
     * disagreed. Precedes the unchanged restore wording rather than replacing
     * it, so the operator learns why a precautionary restore ran.
     *
     * @param string                    $type    'plugin'|'theme'.
     * @param array<string,mixed>       $applied The runner's apply outcome.
     * @param array{verdict:bool|null,detail:string,signals:array<int,string>} $verify Verification result.
     * @return string
     */
    private static function restoreBecauseModifiedCopy(string $type, array $applied, array $verify): string
    {
        $noun = $type === 'theme' ? 'theme' : 'plugin';

        return 'The package failed before installing any file (' . self::failureReason($applied) . '), but the '
            . $noun . ' directory no longer matches its pre-update state (' . $verify['detail'] . '), so the '
            . 'pre-update snapshot is being restored as a precaution.';
    }

    /**
     * D4: the classification said the destination was untouched and this host
     * could not confirm it either way. Restoring is the safe direction.
     *
     * @param string                    $type    'plugin'|'theme'.
     * @param array<string,mixed>       $applied The runner's apply outcome.
     * @param array{verdict:bool|null,detail:string,signals:array<int,string>} $verify Verification result.
     * @return string
     */
    private static function restoreBecauseUnverifiedCopy(string $type, array $applied, array $verify): string
    {
        $noun = $type === 'theme' ? 'theme' : 'plugin';

        return 'The package failed before installing any file (' . self::failureReason($applied) . '), but this '
            . 'host could not verify the ' . $noun . ' directory afterwards (' . $verify['detail'] . '), so the '
            . 'pre-update snapshot is being restored as a precaution.';
    }

    /**
     * D6: core silently deactivated an active plugin at `upgrader_pre_install`
     * and, on a non-cron request, never puts it back. On the skip path the
     * directory has just been verified byte-consistent, which is the ONLY
     * condition under which reactivating is a pure undo rather than a new risk:
     * activate_plugin() includes the plugin file in this process, so it must
     * never be pointed at a directory we are not sure about.
     *
     * Deliberately NOT applied to the restore path in this release: that path's
     * wording is byte-identical to the previous version, which is what makes
     * the narrowing auditable. A restored plugin stays deactivated exactly as
     * it did before, so nothing regresses.
     *
     * @param string              $type    'plugin'|'theme'.
     * @param string              $slug    Sanitized slug.
     * @param array<string,mixed> $applied The runner's apply outcome.
     * @return string Empty string, or a leading-newline sentence.
     */
    private function reactivationCopy(string $type, string $slug, array $applied): string
    {
        if ($type !== 'plugin' || ($applied['may_have_deactivated'] ?? false) !== true) {
            return '';
        }

        $wasActive        = ($applied['was_active'] ?? false) === true;
        $wasNetworkActive = ($applied['was_network_active'] ?? false) === true;
        if (!$wasActive && !$wasNetworkActive) {
            return '';
        }

        // Only act when the plugin really is off now. Without this check a
        // plugin the operator deliberately disabled between the capture and
        // here would be switched back on by a failure path.
        if (!function_exists('is_plugin_active') || \is_plugin_active($slug)) {
            return '';
        }

        $reactivated = $this->runner->reactivateIfCoreDeactivated($slug, $wasActive, $wasNetworkActive);
        if (!$reactivated['attempted']) {
            return '';
        }

        if ($reactivated['ok']) {
            return "\nThe plugin was active before the update and WordPress deactivated it before failing; "
                . 'it has been reactivated.';
        }

        return "\nThe plugin was active before the update and WordPress deactivated it before failing; "
            . 'reactivation FAILED: ' . $reactivated['message'] . '. Reactivate it from the Plugins screen.';
    }

    /**
     * The human reason a failure is reported with: the resolved WordPress error
     * code plus its data when there is any. Both are already length-bounded and
     * control-character stripped by UpdateOutcome, because both can originate
     * inside a downloaded package.
     *
     * @param array<string,mixed> $applied The runner's apply outcome.
     * @return string
     */
    private static function failureReason(array $applied): string
    {
        $code = (string) ($applied['failure_code'] ?? '');
        $data = (string) ($applied['failure_data'] ?? '');

        if ($code === '') {
            return 'the update did not complete';
        }

        return $code . ($data !== '' ? ' (' . $data . ')' : '');
    }

    /**
     * Render a three-valued verification verdict for a debug line.
     *
     * @param bool|null $verdict Verification verdict.
     * @return string
     */
    private static function verdictLabel(?bool $verdict): string
    {
        if ($verdict === true) {
            return 'verified_intact';
        }

        return $verdict === false ? 'modified' : 'unverified';
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

    /**
     * Does this item resolve to the running agent's own plugin?
     *
     * Matches on BOTH signals, because either one alone leaves a hole:
     *   - the stock plugin key ("folder/main-file.php") and its folder, which
     *     still identify the agent on a site whose WP_PLUGIN_DIR/
     *     WP_CONTENT_DIR constants are unavailable in this runtime;
     *   - any slug whose directory resolves (through realpath(), so a
     *     symlinked or renamed install directory resolves too) to
     *     WPMGR_AGENT_DIR, which is what identifies the agent when it is
     *     installed under a different directory name than the stock one.
     *
     * Every one of those comparisons is case-INSENSITIVE (see sameName()), so
     * a renamed, symlinked or differently-cased install is matched as well as
     * the stock plugin key, and the control plane's own matcher
     * (apps/api/internal/agentplugin) and this last line of defence agree.
     *
     * Read-only: it inspects constants and resolves paths, and never creates,
     * moves or writes anything. Public and static so RollbackCommand shares
     * exactly this definition rather than growing a second, drifting one.
     * `core` is never a self-target.
     *
     * @param string $type 'plugin'|'theme'|'core'.
     * @param string $slug Sanitized slug (see sanitizeSlug()).
     * @return bool
     */
    public static function isSelfTarget(string $type, string $slug): bool
    {
        if ($slug === '' || ($type !== 'plugin' && $type !== 'theme')) {
            return false;
        }

        $folder = self::slugFolder($slug);
        if ($folder === '') {
            return false;
        }

        $selfDirs = self::selfDirVariants();

        if ($type === 'plugin') {
            if (self::sameName($slug, self::SELF_PLUGIN_KEY) || self::sameName($folder, self::SELF_PLUGIN_FOLDER)) {
                return true;
            }

            // The agent's actual directory NAME, for an install renamed off
            // the stock folder above. Covers the case where the plugin-root
            // constants needed by the path comparison below are missing.
            foreach ($selfDirs as $selfDir) {
                if (self::sameName(basename($selfDir), $folder)) {
                    return true;
                }
            }
        }

        foreach (self::itemDirVariants($type, $folder) as $candidate) {
            foreach ($selfDirs as $selfDir) {
                if (self::sameName($candidate, $selfDir)) {
                    return true;
                }
            }
        }

        return false;
    }

    /**
     * Case-insensitive equality for a slug, folder name or path.
     *
     * Nothing here is a secret, so this is an identity test and not a
     * constant-time one: a timing-safe compare would buy nothing and cannot
     * fold case anyway. Case must fold because a hand-built or replayed
     * command can carry any casing it likes, and because a case-insensitive
     * filesystem (macOS, Windows, a casefolded or SMB/NFS-backed wp-content)
     * resolves "WPMgr-Agent" and "wpmgr-agent" to the SAME directory: a
     * byte-exact match would let exactly that payload through to an apply
     * that overwrites the running agent. The control plane's matcher folds
     * case for the same reason; the agent is the only guard left once a
     * signed command has been forged or replayed, so it must not be the
     * weaker of the two.
     *
     * Comparison is ASCII-only, which is all sanitizeSlug() can produce. The
     * cost of folding case is at worst refusing a third-party plugin whose
     * directory differs from the agent's by letter case alone, which is the
     * safe direction to be wrong in.
     *
     * @param string $a First value.
     * @param string $b Second value.
     * @return bool
     */
    private static function sameName(string $a, string $b): bool
    {
        return strcasecmp($a, $b) === 0;
    }

    /**
     * The folder component of a slug that may be a bare folder ("akismet") or
     * a full "folder/main-file.php" plugin basename.
     *
     * @param string $slug Sanitized slug.
     * @return string Folder name (never contains a '/').
     */
    private static function slugFolder(string $slug): string
    {
        $pos = strpos($slug, '/');

        return $pos === false ? $slug : substr($slug, 0, $pos);
    }

    /**
     * Normalize a path for comparison: forward slashes, no trailing slash.
     *
     * @param string $path Raw path.
     * @return string Normalized path ('' when there is nothing to compare).
     */
    private static function normalizePath(string $path): string
    {
        return rtrim(str_replace('\\', '/', trim($path)), '/');
    }

    /**
     * Comparable forms of a path: as written, plus its realpath() when that
     * resolves to something different (a symlinked plugins tree, a relocated
     * wp-content). Never throws; an unresolvable path simply yields fewer
     * forms.
     *
     * @param string $path Raw path.
     * @return array<int,string>
     */
    private static function pathVariants(string $path): array
    {
        $normalized = self::normalizePath($path);
        if ($normalized === '') {
            return [];
        }

        $variants = [$normalized];

        $resolved = realpath($normalized);
        if (is_string($resolved) && $resolved !== '') {
            $resolved = self::normalizePath($resolved);
            if ($resolved !== '' && $resolved !== $normalized) {
                $variants[] = $resolved;
            }
        }

        return $variants;
    }

    /**
     * Comparable forms of the running agent's own directory.
     *
     * @return array<int,string>
     */
    private static function selfDirVariants(): array
    {
        if (!defined('WPMGR_AGENT_DIR')) {
            return [];
        }

        return self::pathVariants((string) constant('WPMGR_AGENT_DIR'));
    }

    /**
     * Comparable forms of the directory an update item would be applied to.
     *
     * @param string $type   'plugin'|'theme'.
     * @param string $folder Folder component of the item's slug.
     * @return array<int,string>
     */
    private static function itemDirVariants(string $type, string $folder): array
    {
        $roots = [];
        if ($type === 'plugin') {
            if (defined('WP_PLUGIN_DIR')) {
                $roots[] = (string) constant('WP_PLUGIN_DIR');
            } elseif (defined('WP_CONTENT_DIR')) {
                $roots[] = (string) constant('WP_CONTENT_DIR') . '/plugins';
            }
        } elseif (defined('WP_CONTENT_DIR')) {
            $roots[] = (string) constant('WP_CONTENT_DIR') . '/themes';
        }

        $variants = [];
        foreach ($roots as $root) {
            $root = self::normalizePath($root);
            if ($root === '') {
                continue;
            }
            foreach (self::pathVariants($root . '/' . $folder) as $variant) {
                $variants[] = $variant;
            }
        }

        return $variants;
    }
}
