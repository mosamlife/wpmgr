<?php
/**
 * Plugin Name: WPMgr Update Watchdog (mu-plugin loader)
 * Description: Registers a shutdown handler at the very top of the
 *              WordPress plugin-loading pass that restores a plugin/theme
 *              from its pre-update snapshot when the update itself applied
 *              cleanly but the new code fatals on EVERY subsequent
 *              bootstrap. GitHub issue #210.
 * Version:     1.0.0
 * Author:      WPMgr contributors
 * License:     MIT
 *
 * ---------------------------------------------------------------------------
 * THE PROBLEM (GitHub issue #210)
 * ---------------------------------------------------------------------------
 * WPMgr's normal update-recovery path is `RollbackCommand`, a regular signed
 * REST route registered on `rest_api_init`. That hook fires only AFTER every
 * regular plugin has finished loading. When an update applies cleanly (files
 * copied correctly, the version header bumped) but the NEW code itself fatals
 * on every bootstrap — e.g. `Class ... not found` in the updated plugin's own
 * loader — the site is down on every single request, including the very
 * request that would otherwise deliver a rollback command: the fatal happens
 * before `rest_api_init` is ever reached. The one automated recovery path
 * shares a single point of failure with the exact condition it exists to
 * catch.
 *
 * ---------------------------------------------------------------------------
 * THE FIX: a self-contained update-watchdog mu-plugin
 * ---------------------------------------------------------------------------
 * An mu-plugin loads BEFORE any regular plugin. A `register_shutdown_function`
 * registered here still fires when a regular plugin fatals mid-bootstrap,
 * exactly like `a-wpmgr-error-trap.php`'s shutdown handler already does for
 * error CAPTURE. This file performs an actual RECOVERY instead:
 * `WPMgr\Agent\Support\SnapshotManager::restore()` is a pure filesystem
 * operation (move the live directory aside, copy the snapshot payload back)
 * that needs no plugin bootstrap at all — the only reason it normally runs
 * inside the fully-booted plugin is that `RollbackCommand` is the thing that
 * calls it, not that it depends on anything the plugin bootstrap provides.
 *
 * The ARM side lives in `WPMgr\Agent\Commands\UpdateCommand` (fully-booted
 * PHP, run inside a signed REST request): on a successful plugin/theme apply
 * it calls `WPMgr\Agent\Support\UpdateWatchdogMarker::arm()`, which persists a
 * short-TTL, one-shot marker FILE — see that class's doc for the exact
 * schema, storage location rationale, and TTL reasoning — containing the
 * ABSOLUTE live directory and ABSOLUTE snapshot payload directory, both
 * already resolved and validated by `SnapshotManager::resolvedRestorePaths()`
 * at arm time.
 *
 * Even the control plane's OWN post-update health-probe request — which also
 * fatals, same as any other request — trips this same shutdown handler, so
 * recovery happens without any CP round-trip, REST call, or cron tick.
 *
 * ---------------------------------------------------------------------------
 * BOOTSTRAP-SAFE, FULLY SELF-CONTAINED (mirrors a-wpmgr-waf.php's posture)
 * ---------------------------------------------------------------------------
 * When a regular plugin fatals mid-bootstrap, the WPMgr autoloader is NOT
 * guaranteed to be registered — the `wpmgr-agent` plugin folder loads
 * alphabetically at 'w', which is AFTER many third-party plugin folders (e.g.
 * 'suremembers-core' at 's'). This file therefore:
 *   - Is pure procedural PHP with ZERO references to any `WPMgr\...` class.
 *   - Re-derives and re-validates every path itself (see PATH SAFETY below)
 *     rather than trusting the marker's recorded path strings — the marker
 *     is used only as a fast identity check against an INDEPENDENTLY
 *     re-derived expectation, exactly mirroring the containment discipline
 *     in `SnapshotManager::liveDir()`/`resolveSnapshotDir()`.
 *   - Wraps every function that can reach a filesystem or WordPress API call
 *     in try/catch, so the watchdog can NEVER itself cause, or add to, a
 *     fatal — the one thing worse than not recovering a broken site is
 *     breaking a healthy one.
 *
 * ---------------------------------------------------------------------------
 * THE HOT PATH: a single stat on every request
 * ---------------------------------------------------------------------------
 * This file's top-level code runs on EVERY request to EVERY page, whether or
 * not an update was ever applied. The common case (no marker present) MUST
 * cost essentially nothing: exactly one `is_file()` call against a path built
 * from a plain PHP constant (`WP_CONTENT_DIR`) — no database query, no
 * `wp_upload_dir()` call (which can itself trigger a directory-creation
 * write; see `UpdateWatchdogMarker`'s own doc for why the marker deliberately
 * does NOT live under the uploads-first convention every other agent
 * user-data store uses), no filter application. Only when a marker actually
 * exists (a narrow post-update window, see TTL_SECONDS) does any heavier work
 * — registering the shutdown callback — happen at all; the RESTORE-CAPABLE
 * work below only ever runs from inside that shutdown callback, and even then
 * only when `error_get_last()` reports a genuine fatal.
 *
 * ---------------------------------------------------------------------------
 * PATH SAFETY (GitHub issue #147 discipline, mirrored exactly)
 * ---------------------------------------------------------------------------
 * The repository has a real restore-anchoring data-loss history: GH #147, an
 * UNANCHORED `strpos()` substring check silently dropped unrelated plugin
 * files at restore time. Every path decision in this file uses exact-path /
 * exact-segment / anchored-prefix / anchored-containment comparisons — never
 * a bare substring match — and mirrors `SnapshotManager`'s own containment
 * logic function-for-function (`liveDir()`'s realpath()-based check plus its
 * documented open_basedir/symlink anchored fallback, `resolveSnapshotDir()`'s
 * strict realpath containment, `copyDir()`/`deleteDir()`'s symlink-skip
 * recursion). There is no globbing and no wildcard exclude anywhere below.
 * The running agent's OWN plugin directory (`WPMGR_AGENT_DIR`) is refused as
 * a restore target outright — mirrors `FilesRestorer::PRESERVE_FROM_LIVE`'s
 * protection of `plugins/wpmgr-agent`: a directory-replacement recovery
 * mechanism must never be pointed at the very code that could run it.
 *
 * ---------------------------------------------------------------------------
 * CONCURRENCY: never race a live apply, never restore the same marker twice
 * ---------------------------------------------------------------------------
 *   - Before touching disk, this file attempts the SAME non-blocking
 *     `flock()` liveness lock `WPMgr\Agent\Support\UpdateInFlight::mark()`
 *     holds for the duration of a real, in-progress apply of the exact same
 *     type/slug (same lock file path, independently recomputed from the
 *     `sha256(type:slug)` key — no class dependency, pure hashing). If that
 *     lock cannot be acquired, a genuine apply owns this slug right now:
 *     stand down entirely rather than race it.
 *   - One-shot: the first shutdown handler to reach a given marker entry
 *     atomically "claims" it via `fopen($claimFile, 'x')` — exclusive
 *     create, which only ever succeeds for exactly one concurrent caller.
 *     Every other simultaneously-fataling request (multiple visitors hitting
 *     the same down site at once, each in its own PHP-FPM worker) loses that
 *     race and does nothing. A marker entry is therefore restored AT MOST
 *     ONCE, no matter how many requests fatal while it is live.
 *
 * Installed by:
 *   `WPMgr\Agent\Support\MuPluginInstaller::installUpdateWatchdog()` — called
 *   from the agent plugin's boot pass whenever the site is enrolled
 *   (idempotent: same content -> no-op). On a managed host where
 *   `mu-plugins/` is not writable, this file is silently absent — the same
 *   accepted degradation as the error-trap and WAF mu-plugins today; the
 *   control plane already surfaces a "manual filesystem recovery" message
 *   for that case.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

// =============================================================================
// Cheap per-request short-circuit (see class doc, THE HOT PATH).
// =============================================================================

/**
 * Absolute path to the watchdog state directory:
 * `WP_CONTENT_DIR/wpmgr-update-watchdog`. Deliberately WP_CONTENT_DIR-based,
 * not uploads-based — see this file's class doc, THE HOT PATH, and
 * `WPMgr\Agent\Support\UpdateWatchdogMarker`'s own doc, STORAGE LOCATION.
 *
 * @return string Absolute path, or '' when WP_CONTENT_DIR is not defined.
 */
function wpmgr_watchdog_state_dir(): string
{
    if (!defined('WP_CONTENT_DIR')) {
        return '';
    }
    $contentDir = rtrim((string) constant('WP_CONTENT_DIR'), '/\\');
    if ($contentDir === '') {
        return '';
    }

    return $contentDir . '/wpmgr-update-watchdog';
}

/**
 * Absolute path to the marker file. Filename MUST match
 * `WPMgr\Agent\Support\UpdateWatchdogMarker::MARKER_BASENAME` exactly.
 *
 * @return string Absolute path, or '' when the state dir cannot be resolved.
 */
function wpmgr_watchdog_marker_path(): string
{
    $dir = wpmgr_watchdog_state_dir();

    return $dir === '' ? '' : $dir . '/watchdog-marker.json';
}

// =============================================================================
// Shutdown handler — the actual recovery logic. Everything below this point
// only ever runs when the cheap check at the bottom of this file found a
// marker file AND a genuine fatal actually happened.
// =============================================================================

/**
 * Whether $path, once normalized, is exactly $dir or a descendant of it.
 * Anchored (segment/prefix) comparison, never a bare substring match — see
 * this file's class doc, PATH SAFETY.
 *
 * @param string $path Absolute candidate path (already normalized/resolved).
 * @param string $dir  Absolute directory to test containment against.
 * @return bool
 */
function wpmgr_watchdog_path_is_within(string $path, string $dir): bool
{
    $path = str_replace('\\', '/', $path);
    $dir  = rtrim(str_replace('\\', '/', $dir), '/');
    if ($dir === '' || $path === '') {
        return false;
    }

    return $path === $dir || strncmp($path, $dir . '/', strlen($dir) + 1) === 0;
}

/**
 * String/segment-anchored containment check — mirrors
 * `WPMgr\Agent\Support\SnapshotManager::anchoredContainment()` line for line.
 * Used ONLY as the realpath()-unavailable fallback for the LIVE-directory
 * side (mirroring `liveDir()`'s own documented open_basedir/symlinked-
 * wp-content fallback); $root here is always WP-native (`WP_PLUGIN_DIR` or
 * `get_theme_root()`'s return), never attacker-controlled.
 *
 * @param string $root Trusted root directory (never attacker-controlled).
 * @param string $path Candidate path, expected to be "$root/<segment...>".
 * @return bool
 */
function wpmgr_watchdog_anchored_containment(string $root, string $path): bool
{
    $root = rtrim($root, '/\\');
    if ($root === '' || $path === '') {
        return false;
    }

    $normalizedRoot = str_replace('\\', '/', $root);
    $normalizedPath = str_replace('\\', '/', $path);

    if ($normalizedPath !== $normalizedRoot && strncmp($normalizedPath, $normalizedRoot . '/', strlen($normalizedRoot) + 1) !== 0) {
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
 * Whether $slug passes the SAME allowlist
 * `WPMgr\Agent\Commands\UpdateCommand::sanitizeSlug()` enforces upstream.
 * Re-validated independently here — this file never trusts marker content as
 * pre-sanitized (defense in depth; the marker is written by trusted, already
 * -booted PHP, but this file has no way to prove that from inside a
 * fatal-triggered shutdown handler with no autoloader).
 *
 * @param string $slug Candidate slug read from the marker.
 * @return bool
 */
function wpmgr_watchdog_slug_is_safe(string $slug): bool
{
    $trimmed = trim($slug);
    if ($trimmed === '' || $trimmed !== $slug) {
        return false;
    }
    if (strpos($slug, '..') !== false || strpos($slug, "\0") !== false) {
        return false;
    }
    if ($slug[0] === '/' || $slug[0] === '\\') {
        return false;
    }
    if (preg_match('#^[A-Za-z]:#', $slug) === 1) {
        return false;
    }

    return preg_match('#^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$#', $slug) === 1;
}

/**
 * Theme root directory — mirrors `SnapshotManager::themeRoot()`.
 *
 * @param string $contentDir wp-content path.
 * @return string
 */
function wpmgr_watchdog_theme_root(string $contentDir): string
{
    if (function_exists('get_theme_root')) {
        $root = @get_theme_root();
        if (is_string($root) && $root !== '') {
            return rtrim($root, '/\\');
        }
    }

    return $contentDir . '/themes';
}

/**
 * Independently re-derive the EXPECTED absolute live directory for a
 * type/slug pair, from WP-native roots only (`WP_PLUGIN_DIR` /
 * `get_theme_root()`) — mirrors `SnapshotManager::liveDir()` exactly,
 * including its realpath()-based primary containment check and its
 * documented open_basedir/symlinked-wp-content anchored fallback.
 *
 * Additional hardening beyond `liveDir()`'s own logic: explicitly rejects a
 * plugin-folder or theme-slug segment of exactly `.` or `..`, which
 * `wpmgr_watchdog_slug_is_safe()`'s character-class regex alone does not
 * exclude (a lone `.` is a legal character-class member) and which would
 * otherwise let `dirname()`/`realpath()` collapse the resolved path to the
 * plugin/theme ROOT itself rather than a single slug's subdirectory — see
 * this file's class doc, PATH SAFETY. This mu-plugin enforces that extra
 * check independently rather than relying on it being fixed upstream.
 *
 * @param string $type 'plugin'|'theme'.
 * @param string $slug Already validated by wpmgr_watchdog_slug_is_safe().
 * @return string Absolute path, or '' when it cannot be safely resolved.
 */
function wpmgr_watchdog_expected_live_dir(string $type, string $slug): string
{
    if (!defined('WP_CONTENT_DIR')) {
        return '';
    }
    $contentDir = rtrim((string) constant('WP_CONTENT_DIR'), '/\\');
    if ($contentDir === '') {
        return '';
    }

    if ($type === 'plugin') {
        if (!defined('WP_PLUGIN_DIR')) {
            return '';
        }
        $root     = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');
        $slashPos = strpos($slug, '/');
        $folder   = $slashPos !== false ? substr($slug, 0, $slashPos) : $slug;
        if ($folder === '' || $folder === '.' || $folder === '..') {
            return '';
        }
        $path = $root . '/' . $folder;
    } elseif ($type === 'theme') {
        if ($slug === '.' || $slug === '..') {
            return '';
        }
        $root = wpmgr_watchdog_theme_root($contentDir);
        $path = $root . '/' . $slug;
    } else {
        return '';
    }

    if ($root === '') {
        return '';
    }

    // Primary: realpath()-based containment (mirrors containedRealpath()).
    $parent       = dirname($path);
    $realParent   = @realpath($parent);
    $realContent  = @realpath($contentDir);
    if ($realParent !== false && $realContent !== false) {
        $realContent = rtrim($realContent, '/\\');
        if ($realParent === $realContent || strncmp($realParent, $realContent . DIRECTORY_SEPARATOR, strlen($realContent) + 1) === 0) {
            return rtrim($path, '/\\');
        }
    }

    // Fallback: anchored string/segment containment against the WP-native
    // root (never attacker-controlled) — mirrors liveDir()'s own documented
    // open_basedir/symlinked-wp-content fallback exactly.
    if (wpmgr_watchdog_anchored_containment($root, $path) && is_dir($path)) {
        return rtrim($path, '/\\');
    }

    return '';
}

/**
 * Resolve the absolute uploads basedir. Safe to call `wp_upload_dir()` here
 * — unlike the top-level per-request check, this function is only ever
 * reached from inside the rare "marker + genuine fatal" shutdown branch, by
 * which point core WordPress (the code that defines `wp_upload_dir()`) is
 * always fully loaded regardless of which regular plugin fatally failed to
 * load.
 *
 * @return string Absolute path, or '' when it cannot be resolved.
 */
function wpmgr_watchdog_uploads_base(): string
{
    if (function_exists('wp_upload_dir')) {
        $u = @wp_upload_dir();
        if (is_array($u) && isset($u['basedir']) && is_string($u['basedir']) && $u['basedir'] !== '') {
            return rtrim($u['basedir'], '/\\');
        }
    }
    if (defined('WP_CONTENT_DIR')) {
        return rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/uploads';
    }

    return '';
}

/**
 * LOW-1 (GitHub issue #210 security review) — mirrors
 * `WPMgr\Agent\Support\StoragePaths::dataBase($purpose)` EXACTLY, which is
 * NOT the same fallback as wpmgr_watchdog_uploads_base() above.
 * wpmgr_watchdog_uploads_base() mirrors `SnapshotManager::uploadsBaseDir()`'s
 * DIFFERENT fallback — `<WP_CONTENT_DIR>/uploads` — which is correct for the
 * snapshot-payload side (SnapshotManager itself falls back the same way).
 * `StoragePaths::dataBase()` (used by `UpdateInFlight` for its lock-file
 * store) instead falls back to `<WP_CONTENT_DIR>/wpmgr-<purpose>` — WITHOUT
 * `/uploads`. Using the wrong helper for the in-flight lock path made this
 * mu-plugin compute a DIFFERENT lock file than `UpdateInFlight::mark()`
 * itself uses whenever `wp_upload_dir()` returns no usable basedir, silently
 * defeating the in-flight stand-down check in exactly that fallback
 * scenario. This function is used ONLY by wpmgr_watchdog_try_lock_inflight()
 * below; wpmgr_watchdog_expected_payload_dir() correctly keeps using
 * wpmgr_watchdog_uploads_base().
 *
 * @param string $purpose Lowercase slug, same convention as StoragePaths.
 * @return string Absolute path, or '' when neither base is available.
 */
function wpmgr_watchdog_storage_data_base(string $purpose): string
{
    if (function_exists('wp_upload_dir')) {
        $u = @wp_upload_dir();
        if (is_array($u) && isset($u['basedir']) && is_string($u['basedir']) && $u['basedir'] !== '') {
            return rtrim($u['basedir'], '/\\') . '/wpmgr-' . $purpose;
        }
    }
    if (defined('WP_CONTENT_DIR')) {
        return rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/wpmgr-' . $purpose;
    }

    return '';
}

/**
 * Independently re-derive the EXPECTED absolute snapshot payload directory
 * for a snapshot id — mirrors `SnapshotManager::resolveSnapshotDir()`'s
 * identifier-shape regex plus its strict realpath()-based containment
 * exactly (no anchored fallback on this side; `resolveSnapshotDir()` itself
 * has none), plus the `/payload` suffix and its own `is_dir()` check.
 * Re-validates the identifier shape independently rather than relying
 * solely on the caller having already done so (defense in depth).
 *
 * @param string $snapshotId Snapshot identifier, re-validated here.
 * @return string Absolute payload path, or '' when it cannot be resolved.
 */
function wpmgr_watchdog_expected_payload_dir(string $snapshotId): string
{
    if (preg_match('#^snap_[A-Za-z0-9_]+$#', $snapshotId) !== 1) {
        return '';
    }

    $uploadsBase = wpmgr_watchdog_uploads_base();
    if ($uploadsBase === '') {
        return '';
    }
    $base = $uploadsBase . '/wpmgr-snapshots';

    $candidate = $base . '/' . $snapshotId;
    if (!is_dir($candidate)) {
        return '';
    }

    $realBase = @realpath($base);
    $real     = @realpath($candidate);
    if ($realBase === false || $real === false) {
        return '';
    }
    $realBase = rtrim($realBase, '/\\');
    if ($real !== $realBase && strncmp($real, $realBase . DIRECTORY_SEPARATOR, strlen($realBase) + 1) !== 0) {
        return '';
    }

    $payload = rtrim($real, '/\\') . '/payload';

    return is_dir($payload) ? $payload : '';
}

/**
 * Attempt to acquire the SAME non-blocking `flock()` liveness lock
 * `WPMgr\Agent\Support\UpdateInFlight::mark()` holds for the duration of a
 * real, in-progress apply of this exact type/slug (see that class's own
 * doc). The lock FILE path is recomputed independently — pure
 * `sha256(type:slug)` hashing, no class dependency — via
 * wpmgr_watchdog_storage_data_base('update-inflight'), which mirrors
 * `StoragePaths::dataBase()` exactly (see that function's own doc for LOW-1
 * — why this must NOT reuse wpmgr_watchdog_uploads_base()'s different
 * fallback). Recomputing this path here is safe because this function only
 * ever runs from the rare shutdown branch, not the hot path.
 *
 * @param string $type 'plugin'|'theme'.
 * @param string $slug Sanitized slug.
 * @return resource|null|false A held, locked resource on success (safe to
 *     proceed — release via wpmgr_watchdog_unlock_inflight()); `false` when
 *     the lock is currently held by a genuine in-progress apply (stand
 *     down); `null` when the lock file itself could not even be opened
 *     (treated identically to `false` by the caller — never proceed on an
 *     inconclusive liveness check).
 */
function wpmgr_watchdog_try_lock_inflight(string $type, string $slug)
{
    $dir = wpmgr_watchdog_storage_data_base('update-inflight');
    if ($dir === '') {
        return null;
    }
    $key  = substr(hash('sha256', $type . ':' . $slug), 0, 32);
    $lock = $dir . '/' . $key . '.lock';

    // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- mu-plugin, no autoloader/WP_Filesystem available; recomputes the same coordination-file path WPMgr\Agent\Support\UpdateInFlight::acquireLock() uses
    $handle = @fopen($lock, 'c');
    if ($handle === false) {
        return null;
    }
    if (!flock($handle, LOCK_EX | LOCK_NB)) {
        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- releasing our own failed-lock-attempt handle
        return false;
    }

    return $handle;
}

/**
 * Release a lock handle returned by wpmgr_watchdog_try_lock_inflight().
 *
 * @param resource|null|false $handle Handle to release.
 * @return void
 */
function wpmgr_watchdog_unlock_inflight($handle): void
{
    if (is_resource($handle)) {
        flock($handle, LOCK_UN);
        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- releasing our own advisory lock handle
    }
}

/**
 * Recursively copy a directory — mirrors `SnapshotManager::copyDir()` line
 * for line (symlinks never followed, dirs recursed, first failure aborts).
 *
 * @param string $src Source directory.
 * @param string $dst Destination directory.
 * @return bool
 */
function wpmgr_watchdog_copy_dir(string $src, string $dst): bool
{
    if (!is_dir($src)) {
        return false;
    }
    if (!is_dir($dst) && !@mkdir($dst, 0755, true) && !is_dir($dst)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- mu-plugin, no autoloader/WP_Filesystem available; wp_mkdir_p() itself is not safely callable this early on every path
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

        if (is_link($from)) {
            continue;
        }

        if (is_dir($from)) {
            if (!wpmgr_watchdog_copy_dir($from, $to)) {
                return false;
            }
        } elseif (!@copy($from, $to)) {
            return false;
        }
    }

    return true;
}

/**
 * Recursively delete a directory — mirrors `SnapshotManager::deleteDir()`.
 *
 * @param string $dir Directory to remove.
 * @return bool
 */
function wpmgr_watchdog_delete_dir(string $dir): bool
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
            wpmgr_watchdog_delete_dir($path);
        } else {
            @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- mu-plugin, no autoloader/wp_delete_file() available this early on every path
        }
    }

    return @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- mu-plugin, no autoloader/WP_Filesystem available; removes an already-emptied scratch/aside dir
}

/**
 * The actual directory swap: move $live aside, copy $payload into its place,
 * drop the aside on success or roll back on failure. Mirrors
 * `SnapshotManager::restore()`'s core swap (including its S5 fix: on a
 * mid-copy failure, unconditionally clear whatever partial content copyDir()
 * left at $live before restoring the aside) — with ONE deliberate,
 * conservative omission: `restore()`'s F1 "heal a re-interrupted restore"
 * branch (which recovers from an aside that already exists from a PRIOR
 * interrupted attempt) is NOT reproduced here. If $asideName already exists,
 * this function does nothing at all rather than guess at how to reconcile an
 * unexpected on-disk state — this mu-plugin favors doing nothing over doing
 * something clever whenever reality does not match a fresh restore's
 * expectations. A genuinely stuck state left behind by this function's own
 * prior (failed) attempt is recoverable via `RollbackCommand` once the site
 * is next reachable, or by an operator.
 *
 * @param string $live       Absolute, already-validated live directory.
 * @param string $payload    Absolute, already-validated snapshot payload directory.
 * @param string $snapshotId Snapshot identifier (already regex-validated).
 * @return void
 */
function wpmgr_watchdog_swap_directories(string $live, string $payload, string $snapshotId): void
{
    $asideName = $live . '.wpmgr-watchdog-old-' . $snapshotId;

    if (file_exists($asideName)) {
        return;
    }

    $liveExisted = is_dir($live);

    if ($liveExisted) {
        if (!@rename($live, $asideName)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; mirrors SnapshotManager::restore()'s own rename-based staging
            return;
        }
    }

    if (!wpmgr_watchdog_copy_dir($payload, $live)) {
        // Mirrors SnapshotManager::restore()'s S5 fix: copyDir() may have
        // already created $live as an empty/partial directory even though
        // the copy failed — clear it unconditionally before restoring the
        // aside, rather than gate that restore on a check copyDir()'s own
        // behavior would defeat.
        if (is_dir($live)) {
            wpmgr_watchdog_delete_dir($live);
        }
        if ($liveExisted && is_dir($asideName)) {
            @rename($asideName, $live); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem rollback swap
        }

        return;
    }

    if ($liveExisted && is_dir($asideName)) {
        wpmgr_watchdog_delete_dir($asideName);
    }
}

/**
 * Whether $path is exactly the running agent's own plugin directory
 * (`WPMGR_AGENT_DIR`) — mirrors `FilesRestorer::PRESERVE_FROM_LIVE`'s
 * protection of `plugins/wpmgr-agent`: a directory-replacement recovery
 * mechanism must never be pointed at the very code that could run it.
 * Extracted into its own function (rather than inlined) so it is directly
 * unit-testable in isolation.
 *
 * @param string $path Absolute, already-resolved candidate path.
 * @return bool
 */
function wpmgr_watchdog_is_self_path(string $path): bool
{
    if (!defined('WPMGR_AGENT_DIR') || $path === '') {
        return false;
    }
    $selfDir = rtrim(str_replace('\\', '/', (string) constant('WPMGR_AGENT_DIR')), '/');

    return $selfDir !== '' && str_replace('\\', '/', $path) === $selfDir;
}

/**
 * Validate, independently re-derive, and (only when everything checks out)
 * restore ONE marker entry. Every early `return` is a deliberate, silent
 * stand-down — see this function's inline comments for exactly which
 * safety condition each one enforces.
 *
 * @param array<string,mixed> $entry Decoded marker entry.
 * @param int                 $now   Current time.
 * @return void
 */
function wpmgr_watchdog_attempt_restore(array $entry, int $now): void
{
    $type            = isset($entry['type']) && is_string($entry['type']) ? $entry['type'] : '';
    $slug            = isset($entry['slug']) && is_string($entry['slug']) ? $entry['slug'] : '';
    $snapshotId      = isset($entry['snapshot_id']) && is_string($entry['snapshot_id']) ? $entry['snapshot_id'] : '';
    $recordedLive    = isset($entry['live_dir']) && is_string($entry['live_dir']) ? rtrim($entry['live_dir'], '/\\') : '';
    $recordedPayload = isset($entry['payload_dir']) && is_string($entry['payload_dir']) ? rtrim($entry['payload_dir'], '/\\') : '';
    $expiresAt       = isset($entry['expires_at']) && is_numeric($entry['expires_at']) ? (int) $entry['expires_at'] : 0;

    // Only plugin/theme carry a directory-level snapshot at all (see
    // UpdateCommand's own class doc, D3 — core never arms this watchdog).
    if ($type !== 'plugin' && $type !== 'theme') {
        return;
    }
    if ($slug === '' || $snapshotId === '' || $recordedLive === '' || $recordedPayload === '') {
        return;
    }
    // Fresh, one-shot window: an expired marker must never trigger a restore.
    if ($expiresAt <= 0 || $now >= $expiresAt) {
        return;
    }
    // Same identifier shape SnapshotManager itself generates/validates —
    // never let an arbitrary string reach a filesystem path.
    if (preg_match('#^snap_[A-Za-z0-9_]+$#', $snapshotId) !== 1) {
        return;
    }
    if (!wpmgr_watchdog_slug_is_safe($slug)) {
        return;
    }

    // The marker's recorded live_dir is used ONLY as an identity check
    // against an INDEPENDENTLY re-derived expectation — never trusted as
    // the sole source of truth. See this file's class doc, PATH SAFETY.
    $expectedLive = wpmgr_watchdog_expected_live_dir($type, $slug);
    if ($expectedLive === '' || $expectedLive !== $recordedLive) {
        return;
    }

    // Never restore over the running agent's own plugin directory — mirrors
    // FilesRestorer::PRESERVE_FROM_LIVE's protection of plugins/wpmgr-agent.
    if (wpmgr_watchdog_is_self_path($expectedLive)) {
        return;
    }

    $expectedPayload = wpmgr_watchdog_expected_payload_dir($snapshotId);
    if ($expectedPayload === '' || $expectedPayload !== $recordedPayload) {
        return;
    }

    // Stand down entirely if a genuine apply is in progress for this exact
    // type/slug right now — never race a live UpdateCommand apply.
    $inFlightLock = wpmgr_watchdog_try_lock_inflight($type, $slug);
    if ($inFlightLock === null || $inFlightLock === false) {
        return;
    }

    try {
        $stateDir = wpmgr_watchdog_state_dir();
        if ($stateDir === '') {
            return;
        }
        $claimFile = $stateDir . '/watchdog-claim-' . $snapshotId . '.claim';

        // One-shot atomic claim: only the FIRST concurrent shutdown handler
        // to reach this exact snapshot id ever proceeds past this point.
        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- mu-plugin, no autoloader/WP_Filesystem available; exclusive create is the one-shot mutual-exclusion primitive (see this file's class doc, CONCURRENCY)
        $claimHandle = @fopen($claimFile, 'x');
        if ($claimHandle === false) {
            return;
        }
        fclose($claimHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closing our own just-created claim sentinel; the file's mere existence is the signal, no content needed
        @chmod($claimFile, 0600); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0600); WP_Filesystem would coerce to wider FS_CHMOD_FILE

        wpmgr_watchdog_swap_directories($expectedLive, $expectedPayload, $snapshotId);
    } finally {
        wpmgr_watchdog_unlock_inflight($inFlightLock);
    }
}

/**
 * Decode the marker file's entries. Returns an empty list on any absence,
 * unreadable content, or unexpected shape — never throws.
 *
 * @param string $file Absolute marker file path.
 * @return list<array<string,mixed>>
 */
function wpmgr_watchdog_read_markers(string $file): array
{
    $raw = @file_get_contents($file);
    if (!is_string($raw) || $raw === '') {
        return [];
    }
    $decoded = json_decode($raw, true);
    if (!is_array($decoded) || !isset($decoded['markers']) || !is_array($decoded['markers'])) {
        return [];
    }

    return array_values($decoded['markers']);
}

/**
 * MEDIUM-1a (GitHub issue #210 security review) — whether $message indicates
 * a PHP resource-exhaustion fatal (out-of-memory / execution-timeout) rather
 * than genuinely broken code. Matched case-insensitively against the exact
 * substrings PHP's own core fatal-error messages use for these two classes:
 * "Allowed memory size ... exhausted" and "Maximum execution time ...
 * exceeded". An intermittent resource spike inside the updated plugin's own
 * directory is NOT evidence that the update broke bootstrap — a large
 * import, a slow report, or simply a momentarily busy host can trip either
 * one on an otherwise perfectly good update. Reverting a good update because
 * of a single transient resource spike is a real trust problem for an
 * automated recovery feature, so these two classes are excluded from
 * attribution entirely in wpmgr_watchdog_process_fatal() below — even when
 * the fatal's file otherwise falls within an armed slug's live_dir.
 *
 * @param string $message Raw error message from error_get_last()/$err['message'].
 * @return bool
 */
function wpmgr_watchdog_is_resource_exhaustion_message(string $message): bool
{
    if ($message === '') {
        return false;
    }

    return stripos($message, 'Allowed memory size') !== false
        || stripos($message, 'Maximum execution time') !== false;
}

/**
 * Pure decision-plus-action core, separated from the real
 * `error_get_last()` call so tests can drive it with a controlled error
 * array — mirrors `a-wpmgr-waf.php`'s own `wpmgr_waf_should_deny()`
 * extraction (a test loads THIS file directly and calls the real function,
 * never a hand-copied replica). Acts ONLY when $err reports a genuine fatal
 * (E_ERROR / E_PARSE / E_COMPILE_ERROR / E_CORE_ERROR — deliberately
 * narrower than a-wpmgr-error-trap.php's own capture mask, which also
 * includes E_USER_ERROR/E_RECOVERABLE_ERROR; this watchdog only ever acts on
 * the class of fatal a broken bootstrap actually produces) that is NOT a
 * resource-exhaustion fatal (MEDIUM-1a — see
 * wpmgr_watchdog_is_resource_exhaustion_message()'s own doc), a marker file
 * exists, and at least one entry's recorded live_dir CONTAINS the file the
 * fatal was reported in — so a batch that updated several plugins/themes at
 * once restores ONLY the one that actually caused THIS fatal, never the
 * others. When no entry's live_dir contains the fatal's file (an ambiguous
 * case — e.g. the fatal is reported inside a WP core file rather than the
 * plugin/theme's own tree), this function deliberately does nothing rather
 * than guess which entry to restore.
 *
 * @param array<string,mixed>|null $err The value `error_get_last()` returned.
 * @return void
 */
function wpmgr_watchdog_process_fatal(?array $err): void
{
    if (!is_array($err)) {
        return;
    }
    $code      = (int) ($err['type'] ?? 0);
    $fatalMask = E_ERROR | E_PARSE | E_COMPILE_ERROR | E_CORE_ERROR;
    if (($code & $fatalMask) === 0) {
        return;
    }

    $message = isset($err['message']) && is_string($err['message']) ? $err['message'] : '';
    if (wpmgr_watchdog_is_resource_exhaustion_message($message)) {
        return;
    }

    $markerFile = wpmgr_watchdog_marker_path();
    if ($markerFile === '' || !is_file($markerFile)) {
        return;
    }

    $markers = wpmgr_watchdog_read_markers($markerFile);
    if ($markers === []) {
        return;
    }

    $errFile = isset($err['file']) && is_string($err['file']) ? $err['file'] : '';
    if ($errFile === '') {
        return;
    }
    $errFileReal = @realpath($errFile);
    $errFileReal = is_string($errFileReal) && $errFileReal !== '' ? $errFileReal : $errFile;
    $errFileReal = str_replace('\\', '/', $errFileReal);

    $now = time();
    foreach ($markers as $entry) {
        if (!is_array($entry)) {
            continue;
        }
        $liveDir = isset($entry['live_dir']) && is_string($entry['live_dir']) ? $entry['live_dir'] : '';
        if ($liveDir === '') {
            continue;
        }
        // realpath() the candidate live_dir the SAME way $errFileReal was
        // just canonicalized above, falling back to the raw string when it
        // cannot be resolved (e.g. the directory no longer exists). Without
        // this, a host where wp-content (or one of its ancestors) is itself
        // a symlink would realpath() the fatal's file to its resolved
        // target while leaving live_dir un-resolved, producing a false
        // "not contained" verdict for a genuinely matching entry — never
        // restoring a site this watchdog exists to fix.
        $liveDirReal = @realpath($liveDir);
        $liveDirReal = is_string($liveDirReal) && $liveDirReal !== '' ? $liveDirReal : $liveDir;
        $liveDirReal = str_replace('\\', '/', $liveDirReal);

        if (!wpmgr_watchdog_path_is_within($errFileReal, $liveDirReal)) {
            continue;
        }

        // Found the entry that owns the fatal's file — attempt exactly
        // one restore for it, then stop (never touch any other entry on
        // this same fatal).
        wpmgr_watchdog_attempt_restore($entry, $now);

        return;
    }
}

/**
 * The registered shutdown callback. Thin real-environment wrapper around
 * wpmgr_watchdog_process_fatal() — see that function's doc for the actual
 * decision logic. The entire body is wrapped in try/catch: this callback
 * must NEVER itself throw or trigger a fatal, on top of every individual
 * filesystem call already being `@`-guarded.
 *
 * @return void
 */
function wpmgr_watchdog_shutdown_check(): void
{
    try {
        wpmgr_watchdog_process_fatal(error_get_last());
    } catch (\Throwable $e) {
        // The watchdog itself must never add a fatal.
        return;
    }
}

// =============================================================================
// The cheap per-request check (see this file's class doc, THE HOT PATH).
// WPMGR_WATCHDOG_TESTING: when defined, this file is being `require_once`'d
// by the test suite to load function definitions only — skip the top-level
// stat + register_shutdown_function() call (mirrors a-wpmgr-waf.php's own
// WPMGR_WAF_TESTING convention).
// =============================================================================

if (!defined('WPMGR_WATCHDOG_TESTING')) {
    try {
        $wpmgr_watchdog_marker_file = wpmgr_watchdog_marker_path();
        if ($wpmgr_watchdog_marker_file !== '' && is_file($wpmgr_watchdog_marker_file)) {
            register_shutdown_function('wpmgr_watchdog_shutdown_check');
        }
    } catch (\Throwable $e) {
        // Never let the top-level check itself become a fatal.
    }
    unset($wpmgr_watchdog_marker_file);
}
