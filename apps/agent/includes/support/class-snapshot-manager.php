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
     * GitHub issue #226 — throttle for maybeGc()'s WP-Cron-independent GC
     * trigger (see that method's doc for the root cause this closes: a
     * successful update never had ANY reclaim path of its own, and the two
     * that exist for other cases — pruneForSlug() at the slug's NEXT
     * capture(), and gcExpired() from the daily cron event or the
     * opportunistic call at the start of an `update` command — both require
     * something else to happen first that a quiet, done-updating site may
     * never see again). 1 hour bounds the full snapshot-store sweep this
     * throttle guards to a single cheap get_option() read on every OTHER
     * request in between, on every request the agent serves (not cron-only).
     */
    private const GC_THROTTLE_SECONDS = 3600; // 1h

    /**
     * GitHub issue #226 — reclaim TTL for a snapshot whose meta has been
     * explicitly marked succeeded via markSucceeded(). 1 hour is 6x
     * MIN_KEEP_AGE_SECONDS's 10-minute CP-rollback/watchdog-marker survival
     * floor and comfortably past the 45-minute stale-task reaper window that
     * terminalizes a stuck task WITHOUT ever issuing a rollback — by the time
     * this TTL elapses nothing in the system can still need this snapshot.
     * Reclaiming a genuinely-succeeded update's snapshot on this short a
     * cadence, rather than only via the 72h GC_BACKSTOP_TTL_SECONDS meant for
     * orphans that never resolve on their own, is what closes the root cause
     * of a fleet that bulk-updates once and goes quiet never reclaiming disk.
     */
    private const SUCCESS_RECLAIM_TTL_SECONDS = 3600; // 1h

    /**
     * GitHub issue #226 — autoloaded option backing maybeGc()'s throttle
     * timestamp. Autoloaded (not opt-out like the opcache-version option
     * elsewhere in this codebase) since it is read on effectively every
     * enrolled request via Plugin::maybeGcSnapshots().
     */
    private const OPTION_GC_LAST = 'wpmgr_snapshot_gc_last';

    /**
     * Before-state kinds, recorded in each snapshot's meta.json as
     * `before_state` and reported back on capture()'s receipt.
     *
     * The whole point of naming them is that "the source directory was not
     * there" and "the copy failed" stop being the same outcome. They never
     * were the same thing: the first is a complete before-state whose undo is
     * a delete, the second is an error with no before-state at all.
     *
     *   DIRECTORY    — a full copy of the live directory sits in payload/.
     *   ABSENT       — the live path did not exist; rollback deletes whatever
     *                  was created in its place. No payload/ directory.
     *   CORE_VERSION — only the prior core version string was recorded;
     *                  rollback is a downgrade-by-version (see D3).
     *   NONE         — nothing was recorded. Always paired with a
     *                  CAPTURE_FAILURE_* code and an empty snapshot id.
     *
     * A snapshot written before this constant existed has no `before_state`
     * key at all; readMeta() consumers therefore treat a missing/unknown
     * value as DIRECTORY, which is what every pre-existing snapshot is (the
     * only other pre-existing kind, core, has no payload and is never passed
     * to restore()).
     */
    public const BEFORE_STATE_DIRECTORY    = 'directory';
    public const BEFORE_STATE_ABSENT       = 'absent';
    public const BEFORE_STATE_CORE_VERSION = 'core_version';
    public const BEFORE_STATE_NONE         = 'none';

    /**
     * Machine-readable reasons a capture recorded no before-state at all.
     * Reported on the receipt's `failure` key and carried by
     * SnapshotCaptureFailed, so a caller can tell an environment problem
     * (no writable store) from a real copy error, and neither from the
     * legitimate absence that is BEFORE_STATE_ABSENT.
     */
    public const CAPTURE_FAILURE_STORE_UNAVAILABLE = 'store_unavailable';
    public const CAPTURE_FAILURE_COPY_FAILED       = 'copy_failed';
    public const CAPTURE_FAILURE_NOT_RESTORABLE    = 'not_restorable';

    /**
     * The live path resolved to something that is not a directory — a
     * single-file plugin such as `hello.php`, or a symlink standing in for
     * one. This class snapshots DIRECTORIES; it must not record such a source
     * as absent, because "absent" promises a rollback (delete what was
     * created) that would silently do nothing for a file that already exists.
     * An honest non-restorable failure leaves the item visibly unprotected,
     * which is exactly what it was before this class learned about absence.
     */
    public const CAPTURE_FAILURE_UNSUPPORTED_SOURCE = 'unsupported_source';

    /**
     * liveDir() could not resolve the item's path at all (open_basedir, a
     * relocated wp-content tree, an unknown type). Nothing is known about the
     * path, so nothing can be claimed about it — recording "absent" here would
     * be a before-state asserted about a path this class never saw.
     */
    public const CAPTURE_FAILURE_UNRESOLVED_SOURCE = 'unresolved_source';

    /**
     * The copy succeeded but the payload measured zero files. isRestorable()
     * refuses a directory before-state with nothing in it, so this is the
     * failure code that goes with that refusal — distinct from a copy failure
     * (which copied nothing because it broke) and from an unavailable store.
     */
    public const CAPTURE_FAILURE_EMPTY_PAYLOAD = 'empty_payload';

    /**
     * Capture a pre-update snapshot for an item.
     *
     * For plugin/theme the live source directory is copied into the snapshot
     * store. For core only the prior version is recorded (no copy). When the
     * live source directory does not exist, the ABSENCE ITSELF is recorded as
     * a first-class before-state (see BEFORE_STATE_ABSENT) rather than being
     * reported as "no snapshot".
     *
     * The returned array is a RECEIPT, not just an id: `before_state` says
     * which kind of before-state was recorded, `restorable` says whether
     * rollback() can put the item back the way it was, and `files`/`bytes`
     * quantify a directory payload so a caller can tell a real capture from a
     * nominal one. `failure` carries a machine-readable reason (one of the
     * CAPTURE_FAILURE_* constants) whenever nothing was recorded, so a copy
     * failure — an error — is distinguishable from a legitimate absence.
     *
     * @param string $type        plugin|theme|core.
     * @param string $slug        Sanitized slug.
     * @param string $fromVersion Currently installed version (recorded for core).
     * @return array{snapshot_id:string,log:string,before_state:string,restorable:bool,files:int,bytes:int,failure:string}
     */
    public function capture(string $type, string $slug, string $fromVersion): array
    {
        $snapshotId = $this->newSnapshotId();
        $base       = $this->snapshotBaseDir();
        if ($base === '') {
            return $this->receipt(
                '',
                'Snapshot store unavailable; proceeding without snapshot.',
                self::BEFORE_STATE_NONE,
                0,
                0,
                self::CAPTURE_FAILURE_STORE_UNAVAILABLE
            );
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
            $this->writeMeta($dest, $type, $slug, $fromVersion, self::BEFORE_STATE_CORE_VERSION);

            return $this->receipt(
                $snapshotId,
                'Recorded core version ' . $fromVersion . ' for rollback.',
                self::BEFORE_STATE_CORE_VERSION,
                0,
                0,
                ''
            );
        }

        $source = $this->liveDir($type, $slug);

        if ($source === '') {
            // Not absence — ignorance. liveDir() returning '' means the path
            // could not be resolved (open_basedir, a relocated wp-content
            // tree), so this class never saw it and cannot testify that it
            // does not exist. Recording BEFORE_STATE_ABSENT here would promise
            // a rollback for a path we could not identify.
            return $this->receipt(
                '',
                'Live path could not be resolved; no before-state was captured.',
                self::BEFORE_STATE_NONE,
                0,
                0,
                self::CAPTURE_FAILURE_UNRESOLVED_SOURCE
            );
        }

        if (!is_dir($source) && (file_exists($source) || is_link($source))) {
            // A SINGLE-FILE PLUGIN (hello.php and friends) resolves to a FILE,
            // not a directory. It exists, so it is not absent, and this class
            // snapshots directories, so it cannot be captured either.
            //
            // Classifying it as absent would be strictly worse than having no
            // snapshot: the receipt would claim a valid before-state, a
            // rollback would be attempted, restoreAbsent() would find no
            // directory, report "already in the absent before-state" and
            // return success having restored nothing. A silent successful
            // no-op is the exact failure shape this class exists to remove.
            //
            // An honest non-restorable failure instead returns the empty
            // snapshot id this branch returned before absence was modelled at
            // all, so a single-file plugin update behaves precisely as it does
            // on main: unprotected, and visibly so.
            return $this->receipt(
                '',
                'Live path is a file, not a directory; no before-state was captured.',
                self::BEFORE_STATE_NONE,
                0,
                0,
                self::CAPTURE_FAILURE_UNSUPPORTED_SOURCE
            );
        }

        if (!is_dir($source)) {
            // A MISSING SOURCE IS NOT A FAILED CAPTURE. "This path did not
            // exist" is a complete, perfectly recoverable before-state: the
            // undo is to delete whatever gets created. Recording it as a real
            // snapshot (with a real, non-empty id) is what lets every
            // downstream rollback gate — all of which key on a non-empty
            // snapshot id — treat it as the genuine before-state it is,
            // instead of throwing the information away and running unguarded.
            //
            // liveDir() returning '' lands here too, deliberately: its own
            // anchored fallback requires is_dir(), so a path that does not
            // exist yet legitimately resolves to '' on the hosts that
            // fallback exists for (open_basedir, relocated/symlinked
            // wp-content). restore() re-resolves the live path itself at
            // rollback time and treats an unresolvable/absent path as
            // "already in the recorded state" — never as a licence to delete
            // something it could not identify.
            $this->writeMeta($dest, $type, $slug, $fromVersion, self::BEFORE_STATE_ABSENT);

            return $this->receipt(
                $snapshotId,
                'Recorded absent-source before-state ' . $snapshotId . '; rollback removes whatever is created.',
                self::BEFORE_STATE_ABSENT,
                0,
                0,
                ''
            );
        }

        if (!$this->copyDir($source, $dest . '/payload')) {
            // A COPY FAILURE IS AN ERROR, not an absence: disk full,
            // permissions, or a half-written tree. Two things follow.
            //
            // First, the partial payload is deleted rather than left behind.
            // copyDir() creates directories as it descends, so a failure
            // leaves a truncated tree on disk; without meta.json nothing
            // would restore FROM it today, but leaving a directory that looks
            // like a snapshot is exactly the "nominal, not real" state this
            // receipt exists to make impossible. Best effort — a delete that
            // itself fails still leaves an id-less, meta-less directory the
            // GC backstop reclaims.
            //
            // Second, the receipt says so: BEFORE_STATE_NONE plus
            // CAPTURE_FAILURE_COPY_FAILED, so a caller can distinguish this
            // from the absent case above and refuse to proceed (see
            // captureRequired()). Existing callers that only read
            // `snapshot_id` see the same empty id, and the same log intent,
            // that they saw before this change.
            $this->deleteDir($dest);

            return $this->receipt(
                '',
                'Snapshot copy failed; no before-state was captured.',
                self::BEFORE_STATE_NONE,
                0,
                0,
                self::CAPTURE_FAILURE_COPY_FAILED
            );
        }

        $measured = $this->measureDir($dest . '/payload');

        $this->writeMeta(
            $dest,
            $type,
            $slug,
            $fromVersion,
            self::BEFORE_STATE_DIRECTORY,
            $measured['files'],
            $measured['bytes']
        );

        return $this->receipt(
            $snapshotId,
            $measured['files'] > 0
                ? 'Captured snapshot ' . $snapshotId . '.'
                : 'Captured snapshot ' . $snapshotId . ', but its payload is empty.',
            self::BEFORE_STATE_DIRECTORY,
            $measured['files'],
            $measured['bytes'],
            // isRestorable() refuses a directory before-state that copied
            // nothing, so the receipt names that case rather than leaving a
            // caller to infer it from a zero.
            $measured['files'] > 0 ? '' : self::CAPTURE_FAILURE_EMPTY_PAYLOAD
        );
    }

    /**
     * capture(), for a caller that will NOT proceed without a real before-state.
     *
     * This is the demand-a-guarantee entry point. capture() itself stays
     * best-effort on purpose — UpdateCommand deliberately degrades to an
     * unprotected apply rather than refusing an update outright (see its
     * class doc, S8 revised), and that behaviour is unchanged. A caller that
     * cannot make that trade — anything whose only undo is the snapshot —
     * calls this instead and gets an exception rather than an empty id it
     * has to remember to check.
     *
     * @param string $type        plugin|theme|core.
     * @param string $slug        Sanitized slug.
     * @param string $fromVersion Currently installed version.
     * @return array{snapshot_id:string,log:string,before_state:string,restorable:bool,files:int,bytes:int,failure:string}
     * @throws SnapshotCaptureFailed When no restorable before-state was recorded.
     */
    public function captureRequired(string $type, string $slug, string $fromVersion): array
    {
        $receipt = $this->capture($type, $slug, $fromVersion);

        if (!self::isRestorable($receipt)) {
            // The three arguments below are this class's own receipt values
            // (a log line it composed, one of its own CAPTURE_FAILURE_*
            // constants, and the receipt array), never request input, and
            // this exception is never echoed — the headless agent turns it
            // into a JSON command response, which escapes at its own output
            // boundary.
            throw new SnapshotCaptureFailed(
                $receipt['log'], // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- self-composed log line; never echoed (JSON response escapes at its own boundary)
                $receipt['failure'] !== '' ? $receipt['failure'] : self::CAPTURE_FAILURE_NOT_RESTORABLE, // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- one of this class's own CAPTURE_FAILURE_* constants; never echoed
                $receipt // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- structured receipt array for the handler; never echoed
            );
        }

        return $receipt;
    }

    /**
     * Whether a capture receipt describes a before-state rollback can actually
     * return to.
     *
     * Both halves are required and neither implies the other: an id with no
     * recorded before-state is a bookkeeping artefact, and a before-state kind
     * with no id cannot be addressed by any rollback path (they all key on the
     * id). A directory before-state additionally has to have copied at least
     * one file — a zero-file payload for a source directory that existed is
     * nominal, not real.
     *
     * Accepts a loose array so a receipt that has been round-tripped through
     * JSON (an in-flight marker, a command response) can still be checked.
     *
     * @param array<string,mixed> $receipt Receipt from capture().
     * @return bool
     */
    public static function isRestorable(array $receipt): bool
    {
        $id    = isset($receipt['snapshot_id']) && is_string($receipt['snapshot_id']) ? $receipt['snapshot_id'] : '';
        $state = isset($receipt['before_state']) && is_string($receipt['before_state']) ? $receipt['before_state'] : '';

        if ($id === '') {
            return false;
        }

        if ($state === self::BEFORE_STATE_DIRECTORY) {
            $files = isset($receipt['files']) && is_int($receipt['files']) ? $receipt['files'] : 0;

            return $files > 0;
        }

        return $state === self::BEFORE_STATE_ABSENT || $state === self::BEFORE_STATE_CORE_VERSION;
    }

    /**
     * Build a capture receipt.
     *
     * `restorable` is DERIVED, never passed in: it is isRestorable() applied
     * to the receipt being built. A hand-passed flag could disagree with the
     * predicate every reader uses — a zero-file directory payload is the case
     * that actually did disagree — and a receipt whose own fields contradict
     * each other is worse than no receipt, because both halves look
     * authoritative. Deriving it makes the contradiction unrepresentable.
     *
     * @param string $snapshotId  Snapshot id ('' when nothing was recorded).
     * @param string $log         Human-readable log line.
     * @param string $beforeState One of the BEFORE_STATE_* constants.
     * @param int    $files       Files copied into the payload.
     * @param int    $bytes       Bytes copied into the payload.
     * @param string $failure     One of the CAPTURE_FAILURE_* constants, or ''.
     * @return array{snapshot_id:string,log:string,before_state:string,restorable:bool,files:int,bytes:int,failure:string}
     */
    private function receipt(
        string $snapshotId,
        string $log,
        string $beforeState,
        int $files,
        int $bytes,
        string $failure
    ): array {
        $receipt = [
            'snapshot_id'  => $snapshotId,
            'log'          => $log,
            'before_state' => $beforeState,
            'restorable'   => false,
            'files'        => $files,
            'bytes'        => $bytes,
            'failure'      => $failure,
        ];

        $receipt['restorable'] = self::isRestorable($receipt);

        return $receipt;
    }

    /**
     * Count the files and bytes under a directory, so a receipt can prove a
     * payload is real rather than nominal.
     *
     * Symlinks are skipped, matching copyDir()'s own refusal to follow them
     * out of the tree, so the count describes exactly what was copied.
     *
     * @param string $dir Directory to measure.
     * @return array{files:int,bytes:int}
     */
    private function measureDir(string $dir): array
    {
        $files = 0;
        $bytes = 0;

        if (!is_dir($dir)) {
            return ['files' => $files, 'bytes' => $bytes];
        }

        $items = @scandir($dir);
        if ($items === false) {
            return ['files' => $files, 'bytes' => $bytes];
        }

        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_link($path)) {
                continue;
            }
            if (is_dir($path)) {
                $nested = $this->measureDir($path);
                $files += $nested['files'];
                $bytes += $nested['bytes'];
                continue;
            }
            ++$files;
            $size   = @filesize($path);
            $bytes += $size === false ? 0 : (int) $size;
        }

        return ['files' => $files, 'bytes' => $bytes];
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

        $meta = $this->readMeta($snapshotDir);
        if (is_array($meta)
            && isset($meta['before_state'])
            && $meta['before_state'] === self::BEFORE_STATE_ABSENT
        ) {
            return $this->restoreAbsent($type, $slug, $snapshotId, $meta);
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
     * Roll back to a BEFORE_STATE_ABSENT snapshot: the recorded before-state
     * is "this path did not exist", so the undo is to remove what was created
     * in its place.
     *
     * This is the only rollback path in this class that DELETES rather than
     * copies, so it is deliberately narrow. It runs only when the snapshot's
     * own meta.json says `before_state: absent` — a key this class writes and
     * nothing else does — and only after three further checks:
     *
     *   1. The meta's type and slug must match the ones the caller asked to
     *      roll back. A snapshot id addresses one item; a mismatch means the
     *      caller and the snapshot disagree about what is being undone, and
     *      the safe response to that disagreement is to delete nothing.
     *   2. liveDir() must resolve the path, with the same containment rules
     *      every other path in this class goes through. An unresolvable path
     *      is reported as already-absent, never guessed at.
     *   3. The resolved path must have a real parent (dirname() !== itself)
     *      and a non-empty final segment, so a degenerate slug can never make
     *      this the plugin root, the theme root, or the filesystem root.
     *
     * An already-absent live path is a SUCCESS, not a failure: the site is
     * already in the state this snapshot records, which makes rollback
     * idempotent and safe to retry (the watchdog and the in-flight reconcile
     * can both re-enter it).
     *
     * @param string              $type       plugin|theme.
     * @param string              $slug       Sanitized slug.
     * @param string              $snapshotId Snapshot identifier.
     * @param array<string,mixed> $meta       The snapshot's decoded meta.json.
     * @return array{ok:bool,log:string}
     */
    private function restoreAbsent(string $type, string $slug, string $snapshotId, array $meta): array
    {
        $metaType = isset($meta['type']) && is_string($meta['type']) ? $meta['type'] : '';
        $metaSlug = isset($meta['slug']) && is_string($meta['slug']) ? $meta['slug'] : '';
        if ($metaType !== $type || $metaSlug !== $slug) {
            return [
                'ok'  => false,
                'log' => 'Snapshot ' . $snapshotId . ' records an absent before-state for a different item; refusing to remove anything.',
            ];
        }

        // Validate the slug HERE, in the method that deletes, rather than
        // inheriting the guarantee from UpdateCommand::sanitizeSlug().
        // liveDir()'s containment argument is explicitly "the slug was already
        // sanitized upstream" — sound for the update path, but restore() is
        // reachable by callers holding nothing but a snapshot id, and that
        // guarantee lives in a class this one never calls. A delete must not
        // rest on a promise made somewhere else.
        //
        // The concrete case this closes, which the positional checks below
        // cannot see: an EMPTY slug on the plugin branch makes liveDir() build
        // "<plugins root>/", so is_dir() is true, basename() returns the
        // root's own name rather than '', and dirname() differs from the path
        // — all three positional guards pass and deleteDir() would be handed
        // the entire plugins directory. It is not reachable today (an empty
        // slug at capture time finds a real directory, so capture() records
        // BEFORE_STATE_DIRECTORY and never reaches this method), but the guard
        // must not depend on that reasoning continuing to hold.
        $folder = $this->itemFolderSegment($type, $slug);
        if ($folder === '') {
            return [
                'ok'  => false,
                'log' => 'Refusing to remove the item recorded by snapshot ' . $snapshotId
                    . ': its slug is not a single plain path segment.',
            ];
        }

        $live = $this->liveDir($type, $slug);
        if ($live === '') {
            // Unresolvable is not "already absent": claiming success for a
            // state that could not be verified is the silent no-op this class
            // exists to avoid.
            return [
                'ok'  => false,
                'log' => 'Could not resolve the live path for ' . $slug . '; the absent before-state could not be verified.',
            ];
        }

        $parent = dirname($live);
        if ($parent === $live || $parent === '' || basename($live) === '') {
            return [
                'ok'  => false,
                'log' => 'Refusing to remove ' . $slug . ': the resolved live path has no distinct parent directory.',
            ];
        }

        // Strictly BELOW the item's own root, not merely somewhere under
        // wp-content: the final segment of the path about to be deleted must
        // be the item's own folder name. liveDir() builds "<root>/<folder>",
        // so a resolved path whose last segment is anything else — a root
        // directory, a parent, a path some future liveDir() change resolved
        // differently — is not this item's directory and is not deleted.
        if (basename($live) !== $folder) {
            return [
                'ok'  => false,
                'log' => 'Refusing to remove ' . $slug . ': the resolved live path is not that item\'s own directory.',
            ];
        }

        // Checked AFTER the guards above, so an unmatched or degenerate path
        // is refused rather than reported as "nothing to do".
        if (!file_exists($live) && !is_link($live)) {
            return [
                'ok'  => true,
                'log' => 'Nothing to remove for ' . $slug . '; already in the absent before-state recorded by snapshot ' . $snapshotId . '.',
            ];
        }

        // What was created in place of the absence may be a FILE, not a
        // directory — a single-file plugin is the obvious case. Deleting only
        // directories here would leave the file in place and still report
        // success, which is the same silent no-op the capture side now refuses
        // to set up.
        if (!is_dir($live) || is_link($live)) {
            wp_delete_file($live);

            if (file_exists($live) || is_link($live)) {
                return [
                    'ok'  => false,
                    'log' => 'Could not remove the file created for ' . $slug . ' to restore the absent before-state recorded by snapshot ' . $snapshotId . '.',
                ];
            }

            return [
                'ok'  => true,
                'log' => 'Removed ' . $slug . ', restoring the absent before-state recorded by snapshot ' . $snapshotId . '.',
            ];
        }

        if (!$this->deleteDir($live)) {
            return [
                'ok'  => false,
                'log' => 'Could not remove ' . $slug . ' to restore the absent before-state recorded by snapshot ' . $snapshotId . '.',
            ];
        }

        return [
            'ok'  => true,
            'log' => 'Removed ' . $slug . ', restoring the absent before-state recorded by snapshot ' . $snapshotId . '.',
        ];
    }

    /**
     * The single directory segment an item's live path must end in, or '' when
     * the slug cannot safely name one.
     *
     * Used only by restoreAbsent() — the one rollback path in this class that
     * deletes — so it is deliberately stricter than the update path needs. A
     * plugin slug is "<folder>/<file>.php" (or a bare single-file plugin
     * name); a theme slug is the theme directory name. Either way the part
     * that names a DIRECTORY is one plain segment, and anything else ('', '.',
     * '..', a nested path, a Windows drive letter, an embedded null byte, a
     * segment of nothing but dots) is refused rather than normalized: for a
     * delete, "I cannot tell exactly which directory this is" has to end the
     * operation, not start a best guess.
     *
     * @param string $type plugin|theme.
     * @param string $slug Recorded slug.
     * @return string The folder segment, or '' when the slug is unusable here.
     */
    private function itemFolderSegment(string $type, string $slug): string
    {
        if ($type !== 'plugin' && $type !== 'theme') {
            return '';
        }
        if ($slug === '' || strpos($slug, "\0") !== false) {
            return '';
        }

        $normalized = str_replace('\\', '/', $slug);

        $folder = $normalized;
        if ($type === 'plugin') {
            $separator = strpos($normalized, '/');
            if ($separator !== false) {
                $folder = substr($normalized, 0, $separator);
            }
        }

        if ($folder === '' || strpos($folder, '/') !== false || strpos($folder, ':') !== false) {
            return '';
        }

        // Rejects '.', '..' and any all-dots segment in one check.
        if (trim($folder, '.') === '') {
            return '';
        }

        return $folder;
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
     * GitHub issue #328 - the snapshot's payload directory, for a caller that
     * needs to COMPARE the live tree against the pre-update copy rather than
     * restore it (see DestinationVerifier's R5/R6 signals).
     *
     * Deliberately NOT resolvedRestorePaths(): that method returns an empty
     * pair whenever liveDir() fails, which is exactly the open_basedir /
     * relocated-wp-content population the verifier exists to serve, and it
     * resolves a live path this caller has already resolved for itself.
     *
     * @param string $snapshotId Snapshot identifier.
     * @return string Absolute payload path, or '' when it cannot be resolved
     *                 or does not exist (a core snapshot has no payload).
     */
    public function payloadDir(string $snapshotId): string
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return '';
        }

        $snapshotDir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($snapshotDir === '') {
            return '';
        }

        $payload = $snapshotDir . '/payload';

        return is_dir($payload) ? $payload : '';
    }

    /**
     * GitHub issue #328 - label a snapshot whose restore was deliberately
     * SKIPPED because the destination was verified unchanged after a failure
     * that happened before WordPress ever touched it.
     *
     * THIS IS NOT markSucceeded() AND MUST NEVER BECOME IT. markSucceeded()
     * flips the reclaim threshold from GC_BACKSTOP_TTL_SECONDS (72h) to
     * SUCCESS_RECLAIM_TTL_SECONDS (1h) via shouldReclaim(), and its own doc
     * forbids calling it before the update it documents has been independently
     * verified complete. On this path the update FAILED. The snapshot's entire
     * value here is the case where both the classification and the verification
     * were wrong, so any policy that shortens its life in proportion to our
     * confidence destroys exactly the evidence that would prove the decision
     * wrong. This marker therefore changes NO TTL: `restore_skipped_at` is not
     * read by shouldReclaim() or isMarkedSucceeded() (which keys on
     * `succeeded_at` only), so the snapshot stays on the unchanged 72h tier,
     * which is the tier already documented as "kept for manual recovery".
     * Disk stays bounded by machinery that already exists: MAX_SNAPSHOTS_PER_SLUG
     * plus pruneForSlug() at the top of capture(), so the next attempt on this
     * slug reclaims it.
     *
     * Best-effort and never throws, exactly like markSucceeded(): a failed
     * write simply leaves an unlabelled snapshot on the same 72h tier, which is
     * where it was going anyway.
     *
     * @param string $snapshotId   Snapshot captured for the apply that failed.
     * @param string $failureCode  Resolved WP error code behind the failure.
     * @param string $verification The verifier's own one-line detail.
     * @return void
     */
    public function markRestoreSkipped(string $snapshotId, string $failureCode, string $verification): void
    {
        try {
            $base = $this->snapshotBaseDir();
            if ($base === '') {
                return;
            }
            $dir = $this->resolveSnapshotDir($base, $snapshotId);
            if ($dir === '') {
                return;
            }
            $meta = $this->readMeta($dir);
            if ($meta === null) {
                return;
            }
            $meta['restore_skipped_at']        = time();
            $meta['restore_skip_code']         = $failureCode;
            $meta['restore_skip_verification'] = $verification;
            @file_put_contents($dir . '/meta.json', (string) json_encode($meta));
        } catch (\Throwable $e) {
            // Labelling is diagnostics, never correctness - see this method's
            // doc for why the snapshot's retention is deliberately unaffected.
        }
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

    /**
     * GitHub issue #226 — stamp a snapshot's meta.json with a terminal-success
     * marker once UpdateCommand has verified the item it was captured for
     * genuinely succeeded (see UpdateCommand's call site for the exact gate).
     * This is the other half of gcExpired()'s two-tier reclaim decision (see
     * its doc): a marked-succeeded snapshot is reclaimed after only
     * SUCCESS_RECLAIM_TTL_SECONDS (1h) instead of waiting out the full
     * GC_BACKSTOP_TTL_SECONDS (72h) meant for orphans that never resolve.
     *
     * Best-effort and deliberately never throws: marking succeeded is a
     * disk-reclaim OPTIMIZATION, not a correctness requirement — a snapshot
     * whose meta could not be read or rewritten here (a since-vanished
     * directory, a permissions hiccup, a genuinely unreadable meta.json)
     * simply falls back to the unchanged 72h backstop, which still reclaims
     * it eventually. Never call this before the update it documents has been
     * independently verified complete; it is purely a GC-timing hint.
     *
     * @param string $snapshotId Snapshot identifier captured for the apply
     *                            that just succeeded.
     * @return void
     */
    public function markSucceeded(string $snapshotId): void
    {
        try {
            $base = $this->snapshotBaseDir();
            if ($base === '') {
                return;
            }
            $dir = $this->resolveSnapshotDir($base, $snapshotId);
            if ($dir === '') {
                return;
            }
            $meta = $this->readMeta($dir);
            if ($meta === null) {
                return;
            }
            $meta['succeeded_at'] = time();
            @file_put_contents($dir . '/meta.json', (string) json_encode($meta));
        } catch (\Throwable $e) {
            // Never let a GC-timing optimization affect the caller — the
            // snapshot simply falls back to the 72h backstop (safe
            // degradation, see this method's doc).
        }
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
     * GitHub issue #226 — WP-Cron-INDEPENDENT, throttled trigger for
     * gcExpired(). Root cause this closes: gcExpired() itself only ever ran
     * from the `wpmgr_snapshot_gc` daily cron event (dead on
     * `DISABLE_WP_CRON`/an idle site with no visitor traffic to fire the
     * wp-cron.php pseudo-cron request) or opportunistically at the START of
     * an `update` command (never arrives once a fleet stops sending updates)
     * — so a site that bulk-updates once and goes quiet never reclaimed a
     * single snapshot again. Plugin::maybeGcSnapshots() binds this to
     * `plugins_loaded`, so it now runs on EVERY request the agent serves post
     * enrollment — including the control plane's own uptime probes and every
     * signed command — independent of cron entirely.
     *
     * Throttled to at most once per GC_THROTTLE_SECONDS (1h) via a stored
     * option, so the actual sweep (a scandir() + per-entry meta read) still
     * only runs hourly at most; every other request pays just one cheap
     * get_option() read. The throttle timestamp is stamped BEFORE gcExpired()
     * runs — not after a successful return — so a slow or failing sweep can
     * never be retried on every single request within the same hour; the
     * next throttled attempt an hour later gets a fresh try regardless of
     * whether this one succeeded.
     *
     * gcExpired() itself is wrapped in try/catch: a GC sweep is disk-hygiene
     * housekeeping, never a condition that may break the request (or hook)
     * that happened to trigger it.
     *
     * `static::gcExpired()`, not `self::gcExpired()` — late static binding
     * keeps this in the same test-double seam gcExpired() itself already
     * documents (a subclass overriding gcExpired() is still reached via
     * `SubclassName::maybeGc()`).
     *
     * @return void
     */
    public static function maybeGc(): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $now  = time();
        $last = (int) get_option(self::OPTION_GC_LAST, 0);
        if ($now - $last < self::GC_THROTTLE_SECONDS) {
            return;
        }

        // Stamp BEFORE running — see this method's doc for why.
        update_option(self::OPTION_GC_LAST, $now, true);

        try {
            static::gcExpired();
        } catch (\Throwable $e) {
            // A GC sweep failure must never break the caller (a request
            // handler bound to plugins_loaded); the next throttled attempt
            // an hour from now gets a fresh try.
        }
    }

    /**
     * GC cron backstop — sweep the WHOLE snapshot store for any directory
     * eligible under {@see shouldReclaim()}'s two-tier age decision,
     * independent of the per-slug prune in pruneForSlug() above. This is the
     * safety net for what count-based pruning structurally cannot reach: a
     * snapshot whose slug was later uninstalled or renamed (so it will never
     * again be the target of a capture() call that would prune it), a core
     * meta-only snapshot from a repeatedly-requested `snapshot=true` core
     * update, or a crashed capture that left a directory behind with no
     * readable meta.json at all.
     * ALSO sweeps stranded `.wpmgr-old-<snap_id>` asides left behind in the
     * live plugins/themes tree (F4, issue #131 final-hardening review) — see
     * sweepStrandedAsides()'s doc. Bound to the `wpmgr_snapshot_gc` recurring
     * cron event — see scheduleGc() — invoked opportunistically from the
     * start of the `update` command (C, issue #131 final-hardening review),
     * and (GitHub issue #226) driven WP-Cron-independently on every enrolled
     * request via maybeGc() above, since WP-Cron is unreliable on
     * `DISABLE_WP_CRON`/dormant sites and a quiet fleet may never send
     * another `update` command either.
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
                    if (!$mgr->shouldReclaim($age, $mgr->isMarkedSucceeded($dir))) {
                        continue;
                    }
                    $mgr->deleteDir($dir);
                }
            }
        }

        $mgr->sweepStrandedAsides();
    }

    /**
     * GitHub issue #226 — the two-tier reclaim decision every snapshot
     * directory in gcExpired()'s sweep is evaluated against:
     *
     *   - DEFENSIVE FLOOR: never reclaim anything younger than
     *     MIN_KEEP_AGE_SECONDS (10 min), regardless of the success marker.
     *     This is belt-and-suspenders — SUCCESS_RECLAIM_TTL_SECONDS (1h) is
     *     already 6x this floor, so it can never actually fire under real
     *     constant values today — but it is kept as an explicit, independent
     *     check rather than relying on that ordering never changing.
     *   - Marked succeeded (see markSucceeded()): reclaimed once older than
     *     SUCCESS_RECLAIM_TTL_SECONDS (1h) — comfortably past both the CP's
     *     post-update health-probe/rollback window and the 45-minute
     *     stale-task reaper that terminalizes a stuck task without ever
     *     issuing a rollback.
     *   - Unmarked (an in-progress apply that never reached the success
     *     marker, a failed-rollback broken-site snapshot kept for manual
     *     recovery, an orphan/renamed slug, a core meta-only snapshot, or a
     *     crashed capture with no readable meta at all): reclaimed only once
     *     older than the original GC_BACKSTOP_TTL_SECONDS (72h), unchanged.
     *
     * Isolated into its own pure method (rather than inlined in gcExpired()'s
     * loop) so this exact decision matrix is independently testable.
     *
     * @param int  $age       Snapshot age in seconds (see snapshotAge()).
     * @param bool $succeeded Whether the snapshot's meta carries a
     *                         markSucceeded() marker.
     * @return bool True when this snapshot should be reclaimed now.
     */
    private function shouldReclaim(int $age, bool $succeeded): bool
    {
        if ($age < self::MIN_KEEP_AGE_SECONDS) {
            return false;
        }

        $threshold = $succeeded ? self::SUCCESS_RECLAIM_TTL_SECONDS : self::GC_BACKSTOP_TTL_SECONDS;

        return $age >= $threshold;
    }

    /**
     * Whether a snapshot directory's meta.json carries a markSucceeded()
     * marker. Returns false (never reclaim early) for a directory whose meta
     * is missing or unreadable — exactly the crashed-capture / orphan cases
     * gcExpired()'s unmarked branch (the unchanged 72h backstop) exists for.
     *
     * @param string $dir Absolute snapshot directory.
     * @return bool
     */
    private function isMarkedSucceeded(string $dir): bool
    {
        $meta = $this->readMeta($dir);

        return $meta !== null && isset($meta['succeeded_at']);
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
     * @param string $beforeState One of the BEFORE_STATE_* constants.
     * @param int    $files       Files in the payload (0 when there is none).
     * @param int    $bytes       Bytes in the payload (0 when there is none).
     * @return void
     */
    private function writeMeta(
        string $dir,
        string $type,
        string $slug,
        string $fromVersion,
        string $beforeState = self::BEFORE_STATE_DIRECTORY,
        int $files = 0,
        int $bytes = 0
    ): void {
        if (!is_dir($dir)) {
            wp_mkdir_p($dir);
        }
        $meta = [
            'type'         => $type,
            'slug'         => $slug,
            'from_version' => $fromVersion,
            // The recorded before-state kind. restore() reads this to decide
            // whether rollback means "copy the payload back" or "delete what
            // was created", so it is written for EVERY snapshot, including
            // the ones that have a payload.
            'before_state' => $beforeState,
            // Payload size, for a reader that wants to confirm the snapshot
            // is real rather than nominal without walking it again.
            'files'        => $files,
            'bytes'        => $bytes,
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
