<?php
/**
 * SnapshotManager: captures and restores pre-update snapshots of plugin/theme
 * directories so updates can be rolled back.
 *
 * Snapshots live under wp-content/uploads/wpmgr-snapshots/<snapshot_id>/ and the
 * base directory is hardened against web listing (index.php + .htaccess deny).
 * For core, only the prior version string is recorded (the directory itself is
 * not copied); rollback is then a downgrade-by-version.
 *
 * All paths derived from request input are bounded to wp-content via realpath
 * containment checks, and slugs are sanitized upstream by UpdateCommand. When
 * `realpath()` itself cannot confirm containment (open_basedir excluding a
 * parent, or a symlinked/relocated wp-content tree), `liveDir()` falls back to
 * a string/segment-anchored containment check against the trusted,
 * WP-native root (WP_PLUGIN_DIR / get_theme_root()) — see its own doc for why
 * that fallback can never itself become a path-escape.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Captures and restores filesystem snapshots for safe rollback.
 */
class SnapshotManager
{
    /** Snapshot directory name under uploads. */
    private const DIR = 'wpmgr-snapshots';

    /**
     * M1 (GitHub issue #131 adversarial review) — per-slug retention cap
     * enforced at capture time, BEFORE the new snapshot for this apply is
     * written. Every plugin/theme update now ALWAYS captures a full
     * directory copy (see UpdateCommand's class doc, D2) and — until this
     * fix — nothing ever deleted them: an install that updates the same
     * plugin repeatedly accumulated one full copy per update forever, until
     * disk filled up and took the site down.
     *
     * This is a HARD bound, not a courtesy cleanup, and it is deliberately
     * scoped to never endanger the control plane's post-update health-probe
     * window (~1 minute, including the #127 retry):
     *   - pruneForSlug() (called from capture(), below) only ever considers
     *     snapshots that ALREADY exist for $type/$slug — the snapshot THIS
     *     capture() call is about to create does not exist yet when
     *     pruneForSlug() runs, so it can never be pruned by its own capture.
     *   - Keeping the most recent (MAX_SNAPSHOTS_PER_SLUG - 1) EXISTING
     *     snapshots before adding the new one means the PREVIOUS update's
     *     snapshot — the one a same-session CP rollback would target —
     *     survives at least until the update AFTER NEXT for that same slug.
     *     In practice that is always far past any single health-probe
     *     window; two update cycles back-to-back on the same slug inside
     *     ~1 minute is not a scenario the control plane produces.
     * See gcExpired()'s doc below for the separate, age-only cron backstop
     * that catches what this count-based prune cannot (a slug later
     * uninstalled/renamed, a core meta-only snapshot, or a crashed capture
     * that never got as far as writing a readable meta.json).
     */
    private const MAX_SNAPSHOTS_PER_SLUG = 2;

    /**
     * Belt (issue #131 final-hardening review) — a floor UNDER
     * MAX_SNAPSHOTS_PER_SLUG's count cap: pruneForSlug() must never delete a
     * snapshot younger than this, REGARDLESS of its rank or the capture-time
     * TTL below. The count cap alone assumes updates for one slug are
     * reasonably spaced out; it breaks down if the SAME slug is captured
     * multiple times in quick succession — for example two concurrent
     * `update` requests for the same site racing on the same plugin (a CP
     * dedup bug is being fixed separately to prevent that race at the
     * source, but the agent should not depend on the CP never sending it).
     * In that race, a later capture()'s own prune pass could otherwise
     * delete the snapshot an EARLIER, still-in-flight request's post-update
     * health probe or rollback still needs. 10 minutes is comfortably longer
     * than the CP's ~1-minute post-update health-probe window (including the
     * #127 retry) while still being short enough that this floor never
     * meaningfully weakens the disk bound MAX_SNAPSHOTS_PER_SLUG exists to
     * enforce under normal, non-racing update cadence.
     */
    private const MIN_KEEP_AGE_SECONDS = 600; // 10 minutes

    /**
     * Capture-time TTL: an existing snapshot for a slug is pruned once it is
     * older than this, even if it is still within the count-based cap above.
     * 24h is two orders of magnitude larger than the ~1 minute CP
     * health-probe window, so this can never race a legitimate in-flight
     * rollback — it only ever removes snapshots from update cycles that
     * finished, one way or another, the better part of a day ago.
     */
    private const CAPTURE_PRUNE_TTL_SECONDS = 86400; // 24h

    /** Recurring cron hook name for the GC backstop (gcExpired()). */
    public const HOOK_GC = 'wpmgr_snapshot_gc';

    /**
     * GC cron backstop age threshold. Sweeps ANY snapshot dir — regardless
     * of type/slug — independent of the per-slug count cap in
     * pruneForSlug(), so orphans the count-based prune cannot reach (a
     * renamed/uninstalled slug that will never trigger another capture() for
     * itself, a core meta-only snapshot, or a crashed capture that left a
     * directory with no meta.json at all) are still hard-bounded. 72h is
     * comfortably longer than the 24h capture-time TTL above (so the two
     * never fight over the same snapshot under normal update cadence) and
     * vastly longer than the ~1 minute CP health-probe window.
     */
    private const GC_BACKSTOP_TTL_SECONDS = 259200; // 72h

    /**
     * Capture a pre-update snapshot for an item.
     *
     * For plugin/theme the live source directory is copied into the snapshot
     * store. For core only the prior version is recorded (no copy).
     *
     * @param string $type        plugin|theme|core.
     * @param string $slug        Sanitized slug.
     * @param string $fromVersion Currently installed version (recorded for core).
     * @return array{snapshot_id:string,log:string}
     */
    public function capture(string $type, string $slug, string $fromVersion): array
    {
        $snapshotId = $this->newSnapshotId();
        $base       = $this->snapshotBaseDir();
        if ($base === '') {
            return ['snapshot_id' => '', 'log' => 'Snapshot store unavailable; proceeding without snapshot.'];
        }

        $this->protectBaseDir($base);

        // M1 (issue #131) — bound disk BEFORE writing a new snapshot. See
        // MAX_SNAPSHOTS_PER_SLUG's doc above for why this can never remove
        // the snapshot this very call is about to create, nor one still
        // needed for the CP's post-update health-probe window.
        $this->pruneForSlug($base, $type, $slug);

        $dest = $base . '/' . $snapshotId;

        if ($type === 'core') {
            // Record the prior version for downgrade-by-version on rollback.
            $this->writeMeta($dest, $type, $slug, $fromVersion);

            return ['snapshot_id' => $snapshotId, 'log' => 'Recorded core version ' . $fromVersion . ' for rollback.'];
        }

        $source = $this->liveDir($type, $slug);
        if ($source === '' || !is_dir($source)) {
            return ['snapshot_id' => '', 'log' => 'Source directory not found; proceeding without snapshot.'];
        }

        if (!$this->copyDir($source, $dest . '/payload')) {
            return ['snapshot_id' => '', 'log' => 'Snapshot copy failed; proceeding without snapshot.'];
        }

        $this->writeMeta($dest, $type, $slug, $fromVersion);

        return ['snapshot_id' => $snapshotId, 'log' => 'Captured snapshot ' . $snapshotId . '.'];
    }

    /**
     * Restore a plugin/theme directory from a previously captured snapshot.
     *
     * The live directory is moved aside, the snapshot payload copied back, and
     * the set-aside copy removed on success (rolled back on failure).
     *
     * @param string $type       plugin|theme.
     * @param string $slug       Sanitized slug.
     * @param string $snapshotId Snapshot identifier.
     * @return array{ok:bool,log:string}
     */
    public function restore(string $type, string $slug, string $snapshotId): array
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return ['ok' => false, 'log' => 'Snapshot store unavailable.'];
        }

        $snapshotDir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($snapshotDir === '') {
            return ['ok' => false, 'log' => 'Invalid snapshot id.'];
        }

        $payload = $snapshotDir . '/payload';
        if (!is_dir($payload)) {
            return ['ok' => false, 'log' => 'Snapshot payload missing.'];
        }

        $live = $this->liveDir($type, $slug);
        if ($live === '') {
            return ['ok' => false, 'log' => 'Live directory could not be resolved.'];
        }

        $asideName = $live . '.wpmgr-old-' . $snapshotId;

        // F1 (issue #131 final-hardening review) — heal a RE-INTERRUPTED
        // restore for this EXACT snapshot before attempting to stage
        // anything aside. If $asideName already exists here, a PRIOR
        // restore() call for this exact snapshot id was itself hard-killed
        // mid-copy: $asideName holds whatever was live right before that
        // first attempt started, and $live (if it exists at all right now)
        // holds only a partial copy() of $payload from the interrupted
        // attempt — content that is never worth preserving, since the whole
        // point of THIS call is to overwrite $live with $payload anyway.
        //
        // Without this heal, the rename() immediately below would hit an
        // existing, NON-EMPTY $asideName and fail outright (ENOTEMPTY),
        // wedging recovery permanently: every subsequent restore attempt for
        // this snapshot — including a manual RollbackCommand retry, or the
        // out-of-band UpdateInFlight reconcile — would bail the exact same
        // way forever, with the good pre-update snapshot payload sitting
        // right there, unreachable.
        //
        // Recover deterministically by discarding the partial $live and
        // putting the prior aside back in its place, so this call resumes
        // from precisely the clean starting state a never-interrupted
        // restore() would have seen — then falls straight through into the
        // normal stage-aside / copy-back flow below, i.e. this is a full,
        // fresh retry, not a partial patch-up. The only unavoidable gap is
        // the brief moment between deleteDir($live) and the rename() call
        // succeeding (the same delete-then-rename shape already used by the
        // mid-copy failure path further down); a rename() failure there is
        // reported rather than silently swallowed, rather than risk ending
        // this call with NEITHER live nor aside present.
        if (is_dir($asideName)) {
            if (is_dir($live)) {
                $this->deleteDir($live);
            }
            if (!@rename($asideName, $live)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem recovery swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                return [
                    'ok'  => false,
                    'log' => 'A prior interrupted restore left ' . $asideName
                        . ' stranded and it could not be recovered automatically — manual recovery required.',
                ];
            }
        }

        // Move the current live dir aside (if present).
        if (is_dir($live)) {
            if (!@rename($live, $asideName)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                return ['ok' => false, 'log' => 'Could not stage live directory aside.'];
            }

            // Fix 1 (issue #131 final-hardening review) — rename() PRESERVES
            // the source's mtime; it does not reset it to "now". Without
            // this, an aside carved from a plugin/theme directory whose
            // CONTENTS happened to be untouched for longer than
            // GC_BACKSTOP_TTL_SECONDS (72h) would be "born" already past the
            // sweepStrandedAsides() age threshold, making it eligible for
            // immediate GC sweep during the narrow window this very
            // restore() call has it staged aside — i.e. while the copy-back
            // below is in progress, or if it fails and the aside is the only
            // remaining good copy. Stamping the mtime to the moment the
            // aside was actually staged gives it the FULL 72h grace period,
            // same as any other freshly created aside.
            @touch($asideName); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_touch -- resets the aside's mtime to staging time so sweepStrandedAsides()'s age-based GC gets the full 72h grace; not a WP_Filesystem-eligible path (headless agent, no FTP context)
        }

        if (!$this->copyDir($payload, $live)) {
            // S5 (issue #131 adversarial review) — copyDir() creates $live as
            // its very first step (mkdir), so at this point $live almost
            // always exists again as an empty or partially-populated
            // directory even though the copy itself failed. The original
            // `is_dir($asideName) && !is_dir($live)` guard therefore was
            // false in the exact case it was meant to catch — a mid-copy
            // failure — so the aside was never renamed back: the live dir
            // was left half-written AND the good pre-restore original sat
            // stranded forever at .wpmgr-old-<id>. Unconditionally clear
            // whatever partial content is at $live (best effort) before
            // restoring the set-aside original, rather than gating that
            // restore on a check that copyDir()'s own behavior defeats.
            if (is_dir($live)) {
                $this->deleteDir($live);
            }

            if (is_dir($asideName)) {
                $restored = @rename($asideName, $live); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem rollback swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety

                return [
                    'ok'  => false,
                    'log' => $restored
                        ? 'Restore copy failed; rolled back to the pre-restore original.'
                        : 'Restore copy failed; pre-restore original retained at ' . $asideName . ' but could not be renamed back — manual recovery required.',
                ];
            }

            // Nothing was staged aside (the live directory did not exist
            // before this restore attempt began) — there is no pre-existing
            // original to roll back to.
            return ['ok' => false, 'log' => 'Restore copy failed; no pre-existing directory to roll back to.'];
        }

        // Success: drop the set-aside copy.
        if (is_dir($asideName)) {
            $this->deleteDir($asideName);
        }

        return ['ok' => true, 'log' => 'Restored ' . $slug . ' from snapshot ' . $snapshotId . '.'];
    }

    /**
     * Read the recorded prior version from a snapshot's metadata.
     *
     * @param string $snapshotId Snapshot identifier.
     * @return string Recorded version, or '' when unavailable.
     */
    public function recordedVersion(string $snapshotId): string
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return '';
        }
        $dir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($dir === '') {
            return '';
        }
        $metaFile = $dir . '/meta.json';
        if (!is_file($metaFile)) {
            return '';
        }
        $raw = (string) @file_get_contents($metaFile);
        $meta = json_decode($raw, true);
        if (is_array($meta) && isset($meta['from_version']) && is_string($meta['from_version'])) {
            return $meta['from_version'];
        }

        return '';
    }

    /**
     * Whether a snapshot id still resolves to a real, on-disk snapshot
     * directory.
     *
     * F2 (issue #131 final-hardening review) — used by
     * UpdateInFlight::healStaleIfPresent() to verify a stale marker's
     * referenced snapshot is still present BEFORE attempting a restore from
     * it. The snapshot may already have been pruned (M1's per-slug cap or
     * the gcExpired() backstop) or consumed by an earlier successful
     * RollbackCommand call for the same marker; calling restore() on a
     * vanished snapshot id would just fail with 'Invalid snapshot id.' after
     * the fact — checking first lets the caller log a clear, specific reason
     * and clear the marker without a doomed restore attempt.
     *
     * @param string $snapshotId Snapshot identifier.
     * @return bool
     */
    public function snapshotExists(string $snapshotId): bool
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return false;
        }

        return $this->resolveSnapshotDir($base, $snapshotId) !== '';
    }

    /**
     * GitHub issue #210 — resolve the ABSOLUTE, already-validated live
     * directory and snapshot payload directory for a captured snapshot, for
     * callers that need those two paths as plain strings without duplicating
     * this class's containment logic.
     *
     * The sole caller today is UpdateCommand's update-watchdog ARM step: on a
     * successful apply it persists these two absolute paths into a small
     * marker file consumed by the autoloader-free mu-plugin
     * `mu-plugin-loader/a-wpmgr-update-watchdog.php`, which restores this
     * exact snapshot from a `register_shutdown_function()` callback if a
     * LATER, unrelated request fatals during another plugin's bootstrap
     * (before `rest_api_init` — and therefore RollbackCommand's REST route —
     * would ever fire). This method is the single source of truth both that
     * ARM step and any other same-process caller should use to resolve those
     * paths; the mu-plugin itself cannot call it (no autoloader at the point
     * it runs) and independently re-derives + re-validates the same paths in
     * plain procedural PHP — see that file's own doc for why the paths are
     * still re-validated there rather than simply trusted from the marker.
     *
     * @param string $type       plugin|theme.
     * @param string $slug       Sanitized slug.
     * @param string $snapshotId Snapshot identifier captured for this apply.
     * @return array{live:string,payload:string} Both '' when either side
     *     cannot be safely resolved right now — callers MUST treat that as
     *     "do not arm the watchdog for this item", never as a fatal
     *     condition; the update itself already succeeded.
     */
    public function resolvedRestorePaths(string $type, string $slug, string $snapshotId): array
    {
        $empty = ['live' => '', 'payload' => ''];

        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return $empty;
        }

        $snapshotDir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($snapshotDir === '') {
            return $empty;
        }

        $payload = $snapshotDir . '/payload';
        if (!is_dir($payload)) {
            return $empty;
        }

        $live = $this->liveDir($type, $slug);
        if ($live === '') {
            return $empty;
        }

        return ['live' => $live, 'payload' => $payload];
    }

    /**
     * Remove a snapshot directory and its contents.
     *
     * @param string $snapshotId Snapshot identifier.
     * @return bool
     */
    public function cleanup(string $snapshotId): bool
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return false;
        }
        $dir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($dir === '') {
            return false;
        }

        return $this->deleteDir($dir);
    }

    // ---------------------------------------------------------------------
    // M1 (issue #131) — bounded snapshot GC
    // ---------------------------------------------------------------------

    /**
     * Enforce the per-slug retention cap + capture-time TTL for $type/$slug,
     * BEFORE a new snapshot for it is written. See MAX_SNAPSHOTS_PER_SLUG's
     * class-level doc for the full reasoning and the CP health-probe-window
     * safety argument; in short: this only ever touches snapshots that
     * ALREADY exist for this exact type/slug pair, never the one this
     * capture() call is about to create.
     *
     * @param string $base Absolute snapshot base directory.
     * @param string $type plugin|theme|core.
     * @param string $slug Sanitized slug.
     * @return void
     */
    private function pruneForSlug(string $base, string $type, string $slug): void
    {
        $entries = $this->listSnapshotsForSlug($base, $type, $slug);
        if ($entries === []) {
            return;
        }

        // Newest first, so the rank check below keeps the MOST RECENT
        // (MAX_SNAPSHOTS_PER_SLUG - 1) existing entries.
        usort($entries, static function (array $a, array $b): int {
            return $b['created_at'] <=> $a['created_at'];
        });

        $now = time();
        foreach ($entries as $rank => $entry) {
            $age = $now - $entry['created_at'];

            // Belt (issue #131 final-hardening review) — the min-keep-age
            // floor overrides everything below it: a snapshot younger than
            // MIN_KEEP_AGE_SECONDS is NEVER pruned here, regardless of rank
            // or the capture-time TTL. See MIN_KEEP_AGE_SECONDS's class-level
            // doc for the concurrent-same-slug-capture race this guards.
            if ($age < self::MIN_KEEP_AGE_SECONDS) {
                continue;
            }

            // Reserve one slot for the snapshot capture() is about to create.
            $withinCountCap = $rank < (self::MAX_SNAPSHOTS_PER_SLUG - 1);
            $withinTtl      = $age < self::CAPTURE_PRUNE_TTL_SECONDS;
            if ($withinCountCap && $withinTtl) {
                // Kept: both recent enough by rank AND young enough by age.
                continue;
            }
            $this->deleteDir($base . '/' . $entry['id']);
        }
    }

    /**
     * Scan the snapshot base for entries whose recorded meta matches
     * $type/$slug. The base directory only ever holds a small, bounded
     * number of entries per slug (this pruning is exactly what keeps it
     * bounded), so a full scandir()+meta-read pass is cheap.
     *
     * @param string $base Absolute snapshot base directory.
     * @param string $type plugin|theme|core.
     * @param string $slug Sanitized slug.
     * @return list<array{id:string,created_at:float}>
     */
    private function listSnapshotsForSlug(string $base, string $type, string $slug): array
    {
        $items = @scandir($base);
        if (!is_array($items)) {
            return [];
        }

        $out = [];
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            if (preg_match('#^snap_[A-Za-z0-9_]+$#', $item) !== 1) {
                continue;
            }
            $dir = $base . '/' . $item;
            if (!is_dir($dir)) {
                continue;
            }
            $meta = $this->readMeta($dir);
            if ($meta === null) {
                continue;
            }
            if (($meta['type'] ?? '') !== $type || ($meta['slug'] ?? '') !== $slug) {
                continue;
            }
            $out[] = [
                'id'         => $item,
                'created_at' => isset($meta['created_at']) && is_numeric($meta['created_at']) ? (float) $meta['created_at'] : 0.0,
            ];
        }

        return $out;
    }

    /**
     * Read a snapshot's meta.json as an associative array, or null when
     * missing/unreadable.
     *
     * @param string $dir Absolute snapshot directory.
     * @return array<string,mixed>|null
     */
    private function readMeta(string $dir): ?array
    {
        $metaFile = $dir . '/meta.json';
        if (!is_file($metaFile)) {
            return null;
        }
        $raw = @file_get_contents($metaFile);
        if (!is_string($raw) || $raw === '') {
            return null;
        }
        $meta = json_decode($raw, true);

        return is_array($meta) ? $meta : null;
    }

    /**
     * GC cron backstop — sweep the WHOLE snapshot store for any directory
     * older than GC_BACKSTOP_TTL_SECONDS, independent of the per-slug prune
     * in pruneForSlug() above. This is the safety net for what count-based
     * pruning structurally cannot reach: a snapshot whose slug was later
     * uninstalled or renamed (so it will never again be the target of a
     * capture() call that would prune it), a core meta-only snapshot from a
     * repeatedly-requested `snapshot=true` core update, or a crashed capture
     * that left a directory behind with no readable meta.json at all.
     * ALSO sweeps stranded `.wpmgr-old-<snap_id>` asides left behind in the
     * live plugins/themes tree (F4, issue #131 final-hardening review) — see
     * sweepStrandedAsides()'s doc. Bound to the `wpmgr_snapshot_gc` recurring
     * cron event — see scheduleGc() — AND invoked opportunistically from the
     * start of the `update` command (C, same review) since WP-Cron is
     * unreliable on `DISABLE_WP_CRON`/dormant sites.
     *
     * `new static()`, not `new self()` — late static binding lets a test
     * double subclass override strandedAsideRoots() and still exercise this
     * exact production code path via `SubclassName::gcExpired()`, the same
     * way `SnapshotManager::gcExpired()` behaves identically to a plain
     * `new self()` for real callers.
     *
     * @return void
     */
    public static function gcExpired(): void
    {
        $mgr  = new static();
        $base = $mgr->snapshotBaseDir();
        if ($base !== '') {
            $items = @scandir($base);
            if (is_array($items)) {
                $now = time();
                foreach ($items as $item) {
                    if ($item === '.' || $item === '..') {
                        continue;
                    }
                    if (preg_match('#^snap_[A-Za-z0-9_]+$#', $item) !== 1) {
                        continue;
                    }
                    $dir = $base . '/' . $item;
                    if (!is_dir($dir)) {
                        continue;
                    }
                    $age = $now - $mgr->snapshotAge($dir);
                    if ($age < self::GC_BACKSTOP_TTL_SECONDS) {
                        continue;
                    }
                    $mgr->deleteDir($dir);
                }
            }
        }

        $mgr->sweepStrandedAsides();
    }

    /**
     * F4 (issue #131 final-hardening review) — sweep stranded
     * `<slug>.wpmgr-old-<snap_id>` sibling directories left behind in the
     * live plugins/themes tree by a restore() call whose FINAL
     * rename-back-on-failure step itself failed (the "pre-restore original
     * retained ... but could not be renamed back — manual recovery
     * required" path in restore()'s doc — a disk-full/permissions edge
     * case, not the common case). Left alone, a stranded aside is a full
     * duplicate copy of a plugin/theme directory that wastes disk
     * indefinitely and can surface in wp-admin's plugin/theme list as a
     * bogus "ghost" entry on some configurations (WordPress enumerates
     * every directory under the plugins root looking for a header). Swept
     * on the SAME backstop TTL as the snapshot store itself, so both
     * classes of #131 orphan are reclaimed on one cadence.
     *
     * MEDIUM-2 (GitHub issue #210 security review) — ALSO matches
     * `<slug>.wpmgr-watchdog-old-<snap_id>`, the aside name
     * `wpmgr_watchdog_swap_directories()` (the update-watchdog mu-plugin's
     * own rename-based swap, mirroring this class's restore()) leaves
     * behind on an interrupted watchdog restore. Before this fix, the
     * regex here only matched the ORIGINAL `.wpmgr-old-` prefix, so a
     * watchdog-interrupted aside was never reclaimed by this backstop —
     * `strandedAsideRoots()` below already covers the right ROOTS (the same
     * WP_PLUGIN_DIR / theme root the watchdog's own aside is a sibling
     * under), only the pattern itself needed widening.
     *
     * @return void
     */
    private function sweepStrandedAsides(): void
    {
        $now = time();
        foreach ($this->strandedAsideRoots() as $root) {
            if ($root === '' || !is_dir($root)) {
                continue;
            }
            $items = @scandir($root);
            if (!is_array($items)) {
                continue;
            }
            foreach ($items as $item) {
                if ($item === '.' || $item === '..') {
                    continue;
                }
                if (preg_match('#\.wpmgr-(?:watchdog-)?old-snap_[A-Za-z0-9_]+$#', $item) !== 1) {
                    continue;
                }
                $path = $root . '/' . $item;
                if (!is_dir($path) || is_link($path)) {
                    continue;
                }
                $mtime = @filemtime($path);
                $age   = $mtime !== false ? ($now - $mtime) : PHP_INT_MAX;
                if ($age < self::GC_BACKSTOP_TTL_SECONDS) {
                    continue;
                }
                $this->deleteDir($path);
            }
        }
    }

    /**
     * Absolute roots that could hold a stranded `.wpmgr-old-<id>` aside:
     * WP_PLUGIN_DIR (every plugin slug resolves to exactly one folder level
     * under it — see liveDir()) and the active theme root. Both are the
     * exact parents liveDir() resolves a plugin/theme's live directory
     * under, i.e. exactly where restore() would have left an aside sibling.
     * Protected (not private) so a test double can override it with a
     * fully controlled path — mirrors liveDir()'s own testability seam —
     * without depending on the real, process-global WP_PLUGIN_DIR/
     * WP_CONTENT_DIR constants.
     *
     * @return list<string>
     */
    protected function strandedAsideRoots(): array
    {
        $roots = [];
        if (defined('WP_PLUGIN_DIR')) {
            $roots[] = rtrim((string) WP_PLUGIN_DIR, '/\\');
        }
        $contentDir = defined('WP_CONTENT_DIR') ? rtrim((string) WP_CONTENT_DIR, '/\\') : '';
        if ($contentDir !== '') {
            $roots[] = $this->themeRoot($contentDir);
        }

        return $roots;
    }

    /**
     * Age reference for a snapshot directory: its recorded meta.json
     * `created_at` when readable, else the directory's own filesystem mtime
     * (covers a crashed capture that never got as far as writeMeta()).
     *
     * @param string $dir Absolute snapshot directory.
     * @return int Unix timestamp.
     */
    private function snapshotAge(string $dir): int
    {
        $meta = $this->readMeta($dir);
        if ($meta !== null && isset($meta['created_at']) && is_numeric($meta['created_at'])) {
            return (int) $meta['created_at'];
        }

        $mtime = @filemtime($dir);

        return $mtime !== false ? (int) $mtime : time();
    }

    /**
     * Schedule the recurring GC backstop (gcExpired()). Safe to call on
     * every activation / reschedule pass — a no-op when already scheduled.
     * Mirrors EmailLogger::schedule_prune()'s idempotent-schedule shape.
     *
     * @param int $now Current time.
     * @return void
     */
    public static function scheduleGc(int $now): void
    {
        if (!function_exists('wp_next_scheduled') || !function_exists('wp_schedule_event')) {
            return;
        }
        if (wp_next_scheduled(self::HOOK_GC) !== false) {
            return;
        }
        wp_schedule_event($now + 3600, 'daily', self::HOOK_GC);
    }

    // ---------------------------------------------------------------------
    // Path resolution / containment
    // ---------------------------------------------------------------------

    /**
     * Generate a unique snapshot identifier (no path-significant characters).
     *
     * @return string
     */
    protected function newSnapshotId(): string
    {
        try {
            return 'snap_' . bin2hex(random_bytes(12));
        } catch (\Throwable $e) {
            return 'snap_' . (string) time() . '_' . (string) random_int(1000, 9999);
        }
    }

    /**
     * Resolve and validate a snapshot directory inside the base, rejecting any
     * id that escapes the base directory.
     *
     * @param string $base       Snapshot base directory (absolute).
     * @param string $snapshotId Snapshot identifier.
     * @return string Absolute snapshot dir, or '' when invalid.
     */
    private function resolveSnapshotDir(string $base, string $snapshotId): string
    {
        // Snapshot ids are generated as snap_<hex>; never allow separators.
        if (preg_match('#^snap_[A-Za-z0-9_]+$#', $snapshotId) !== 1) {
            return '';
        }

        $candidate = $base . '/' . $snapshotId;
        if (!is_dir($candidate)) {
            return '';
        }

        return $this->containedRealpath($candidate, $base);
    }

    /**
     * The absolute snapshot base directory under uploads. Created if missing.
     *
     * @return string Absolute path, or '' when uploads is unavailable.
     */
    protected function snapshotBaseDir(): string
    {
        $uploads = $this->uploadsBaseDir();
        if ($uploads === '') {
            return '';
        }

        $base = rtrim($uploads, '/\\') . '/' . self::DIR;
        if (!is_dir($base)) {
            if (!wp_mkdir_p($base) && !is_dir($base)) {
                return '';
            }
        }

        return $base;
    }

    /**
     * Resolve the uploads base directory.
     *
     * @return string
     */
    protected function uploadsBaseDir(): string
    {
        if (function_exists('wp_upload_dir')) {
            $u = wp_upload_dir();
            if (is_array($u) && isset($u['basedir']) && is_string($u['basedir']) && $u['basedir'] !== '') {
                return $u['basedir'];
            }
        }
        if (defined('WP_CONTENT_DIR')) {
            return rtrim((string) WP_CONTENT_DIR, '/\\') . '/uploads';
        }

        return '';
    }

    /**
     * Resolve the live directory for a plugin/theme, bounded to wp-content.
     *
     * Agent-only regression fix (open_basedir / symlinked-relocated
     * wp-content, follow-up to GitHub issue #131): the primary containment
     * check below (`containedRealpath()`) rejects a path whenever either
     * side's `realpath()` returns false OR the two resolved paths diverge.
     * That is exactly what happens on a perfectly HEALTHY site when
     * `open_basedir` excludes a parent of the real, resolved path, or when
     * wp-content/the plugin or theme root is a symlink or bind mount whose
     * `realpath()`'d target textually diverges from wp-content's own
     * `realpath()`'d target even though both point at the same logical
     * tree. Before this fix, that false negative propagated all the way up
     * through `capture()` returning `''` and UpdateCommand's S8 gate then
     * REFUSING the whole apply — the root cause of every plugin/theme
     * update failing with a snapshot error while core (exempt from S8)
     * kept working.
     *
     * @param string $type plugin|theme.
     * @param string $slug Sanitized slug.
     * @return string Absolute path, or '' when it cannot be safely resolved.
     */
    protected function liveDir(string $type, string $slug): string
    {
        $contentDir = defined('WP_CONTENT_DIR')
            ? rtrim((string) WP_CONTENT_DIR, '/\\')
            : '';
        if ($contentDir === '') {
            return '';
        }

        if ($type === 'plugin') {
            $folder = strpos($slug, '/') !== false ? substr($slug, 0, strpos($slug, '/')) : $slug;
            // WP_PLUGIN_DIR is always defined in a real WP load; bail if missing
            // rather than guess a path that may be wrong on relocated-content installs.
            $root   = defined('WP_PLUGIN_DIR') ? rtrim((string) WP_PLUGIN_DIR, '/\\') : '';
            $path   = $root . '/' . $folder;
        } elseif ($type === 'theme') {
            $root = $this->themeRoot($contentDir);
            $path = $root . '/' . $slug;
        } else {
            return '';
        }

        if ($root === '') {
            return '';
        }

        // Containment: the resolved path's parent must sit under wp-content.
        $parent = dirname($path);
        if ($this->containedRealpath($parent, $contentDir) !== '') {
            return $path;
        }

        // Fallback: realpath()-based containment could not confirm this
        // path, which on a healthy host is most often open_basedir or a
        // symlinked/relocated wp-content tree defeating realpath() itself
        // (see this method's class doc above) rather than a genuine escape
        // attempt. Re-check containment with a STRING/SEGMENT-ANCHORED
        // comparison against $root directly instead of realpath()'ing
        // either side. This is safe here specifically because $root is
        // NEVER attacker-controlled — it is WP_PLUGIN_DIR (a core-defined
        // constant) or get_theme_root()'s return (a core API), never a
        // value derived from request input — and $folder/$slug were already
        // rejected upstream by UpdateCommand::sanitizeSlug() for `..`, null
        // bytes, absolute paths, and drive letters before ever reaching
        // here. anchoredContainment() additionally re-validates that no
        // residual traversal segment survived, and `is_dir()` confirms a
        // real, on-disk directory — a path that is merely wrong (not just
        // realpath()-hostile) still resolves to '' here exactly as before.
        if ($this->anchoredContainment($root, $path) && is_dir($path)) {
            return $path;
        }

        return '';
    }

    /**
     * String/segment-anchored containment check: is $path exactly $root, or
     * a descendant of $root, with no residual traversal segment (`.`/`..`)
     * in the remainder? Used by liveDir() as the open_basedir/symlink
     * fallback when `realpath()`-based containment cannot resolve a
     * genuinely valid path — see liveDir()'s class doc for why that fallback
     * is safe (both inputs here are WP-native/already-sanitized, never raw
     * request input).
     *
     * @param string $root Trusted root directory (never attacker-controlled).
     * @param string $path Candidate path, expected to be "$root/<segment...>".
     * @return bool
     */
    private function anchoredContainment(string $root, string $path): bool
    {
        $root = rtrim($root, '/\\');
        if ($root === '' || $path === '') {
            return false;
        }

        $normalizedRoot = str_replace('\\', '/', $root);
        $normalizedPath = str_replace('\\', '/', $path);

        if ($normalizedPath !== $normalizedRoot && !str_starts_with($normalizedPath, $normalizedRoot . '/')) {
            return false;
        }

        $relative = ltrim(substr($normalizedPath, strlen($normalizedRoot)), '/');
        if ($relative === '') {
            return true;
        }

        foreach (explode('/', $relative) as $segment) {
            if ($segment === '' || $segment === '.' || $segment === '..') {
                return false;
            }
        }

        return true;
    }

    /**
     * Theme root directory.
     *
     * @param string $contentDir wp-content path.
     * @return string
     */
    private function themeRoot(string $contentDir): string
    {
        if (function_exists('get_theme_root')) {
            $root = get_theme_root();
            if (is_string($root) && $root !== '') {
                return rtrim($root, '/\\');
            }
        }

        return $contentDir . '/themes';
    }

    /**
     * Verify that a path resolves inside (or equal to) a trusted base directory.
     *
     * @param string $path Candidate path (may not yet exist for child dirs).
     * @param string $base Trusted base directory.
     * @return string Canonicalized path when contained, '' otherwise.
     */
    private function containedRealpath(string $path, string $base): string
    {
        $realBase = realpath($base);
        if ($realBase === false) {
            return '';
        }

        $real = realpath($path);
        if ($real === false) {
            return '';
        }

        $realBase = rtrim($realBase, '/\\');
        if ($real !== $realBase && !str_starts_with($real, $realBase . DIRECTORY_SEPARATOR)) {
            return '';
        }

        return $real;
    }

    // ---------------------------------------------------------------------
    // Filesystem primitives
    // ---------------------------------------------------------------------

    /**
     * Harden the snapshot base directory against web listing/access.
     *
     * @param string $base Absolute base directory.
     * @return void
     */
    private function protectBaseDir(string $base): void
    {
        $index = $base . '/index.php';
        if (!file_exists($index)) {
            @file_put_contents($index, "<?php\n// Silence is golden.\n");
        }
        $htaccess = $base . '/.htaccess';
        if (!file_exists($htaccess)) {
            @file_put_contents($htaccess, "Deny from all\nRequire all denied\n");
        }
    }

    /**
     * Write snapshot metadata.
     *
     * @param string $dir         Snapshot directory.
     * @param string $type        Item type.
     * @param string $slug        Slug.
     * @param string $fromVersion Recorded version.
     * @return void
     */
    private function writeMeta(string $dir, string $type, string $slug, string $fromVersion): void
    {
        if (!is_dir($dir)) {
            wp_mkdir_p($dir);
        }
        $meta = [
            'type'         => $type,
            'slug'         => $slug,
            'from_version' => $fromVersion,
            // M1 (issue #131 adversarial review) — microtime(true), not
            // time(): pruneForSlug() orders same-slug snapshots by this
            // field to decide which to keep, and two captures for the same
            // slug landing within the same wall-clock SECOND (plausible
            // under rapid successive updates, or simply a fast disk) would
            // otherwise tie under time()'s 1-second resolution, making the
            // "keep the most recent" ordering fall back to arbitrary
            // scandir() order — exactly the kind of ambiguity that could
            // prune the wrong snapshot. Microsecond resolution makes a tie
            // between two real capture() calls practically impossible.
            'created_at'   => microtime(true),
        ];
        @file_put_contents($dir . '/meta.json', (string) json_encode($meta));
    }

    /**
     * Recursively copy a directory.
     *
     * @param string $src Source directory.
     * @param string $dst Destination directory.
     * @return bool
     */
    protected function copyDir(string $src, string $dst): bool
    {
        if (!is_dir($src)) {
            return false;
        }
        if (!is_dir($dst) && !wp_mkdir_p($dst) && !is_dir($dst)) {
            return false;
        }

        $items = @scandir($src);
        if ($items === false) {
            return false;
        }

        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $from = $src . '/' . $item;
            $to   = $dst . '/' . $item;

            // Do not follow symlinks out of the tree.
            if (is_link($from)) {
                continue;
            }

            if (is_dir($from)) {
                if (!$this->copyDir($from, $to)) {
                    return false;
                }
            } elseif (!@copy($from, $to)) {
                return false;
            }
        }

        return true;
    }

    /**
     * Recursively delete a directory.
     *
     * @param string $dir Directory to remove.
     * @return bool
     */
    protected function deleteDir(string $dir): bool
    {
        if (!is_dir($dir)) {
            return false;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return false;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->deleteDir($path);
            } else {
                wp_delete_file($path);
            }
        }

        return @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch/snapshot dir; WP_Filesystem not initialized
    }
}
