<?php
/**
 * SizeProbe: decoupled directory-size computation for the WPMgr agent.
 *
 * Owns ALL size logic so DiagnosticsCommand::execute() never blocks the daily
 * push on a tree walk. The push reads last-good from wp_options; the walk runs
 * in a dedicated WP-Cron event (wpmgr_agent_sizes_daily) and optionally after
 * fastcgi_finish_request has released the push response.
 *
 * Computation order (first success wins per directory):
 *   1. `du -sb <dir>` — O(1) on most Linux/macOS hosts. Gated by
 *      execAvailable() (function_exists + disable_functions + safe_mode +
 *      open_basedir + live smoke test).
 *   2. WP core recurse_dirsize() under set_time_limit(0) with an exclude list
 *      (own staging dirs, node_modules, vendor, cache layers). Method = "php".
 *   3. Per-dir miss: keep prior value from lastGood, mark result partial=true.
 *
 * ALWAYS collects disk_total_space / disk_free_space on WP_CONTENT_DIR as an
 * O(1) volume signal — these are always available even when exec and
 * recurse_dirsize both fail.
 *
 * Persists to non-autoloaded wp_option wpmgr_agent_dir_sizes:
 *   {
 *     "computed_at": <unix int>,
 *     "method": "du"|"php"|"disk",
 *     "partial": <bool>,
 *     "disk_total_bytes": <int|null>,
 *     "disk_free_bytes": <int|null>,
 *     "sizes": {
 *       "wordpress_size":  {"bytes": <int>, "human": <string>},
 *       "uploads_size":    {"bytes": <int>, "human": <string>},
 *       "themes_size":     {"bytes": <int>, "human": <string>},
 *       "plugins_size":    {"bytes": <int>, "human": <string>},
 *       "database_size":   {"bytes": <int>, "human": <string>}
 *     }
 *   }
 *
 * Registers the pre_recurse_dirsize filter (WP 5.6+) so both WP core's own
 * Site Health screen AND our PHP fallback short-circuit to du when exec is
 * available, populating WP's dirsize_cache as a side-effect.
 *
 * @package WPMgr\Agent\Diagnostics
 */

declare(strict_types=1);

namespace WPMgr\Agent\Diagnostics;

/**
 * Stateless collaborator; instances are cheap (no DB hit in __construct).
 */
final class SizeProbe
{
    /**
     * wp_option key for the persisted last-good sizes blob.
     * Non-autoloaded (update_option(..., false)) to avoid autoload bloat.
     */
    public const OPTION_SIZES = 'wpmgr_agent_dir_sizes';

    /**
     * Transient key caching the exec availability probe result (bool).
     * TTL: 1 hour. Invalidated by compute() when exec smoke-test fails live.
     */
    private const EXEC_PROBE_KEY = 'wpmgr_agent_exec_probe';

    /**
     * Freshness threshold for getOrCompute(): if last-good is younger than this
     * many seconds, return it as-is (warm-cache fast path). 6 hours matches the
     * Site Health screen's own re-fetch interval and the heartbeat backstop window.
     */
    private const FRESH_THRESHOLD_SECONDS = 6 * 3600; // 21600 s

    /**
     * Return the persisted last-good sizes blob, or null when not yet computed.
     *
     * @return array<string,mixed>|null
     */
    public function lastGood(): ?array
    {
        if (!function_exists('get_option')) {
            return null;
        }
        $raw = get_option(self::OPTION_SIZES, null);
        if (!is_array($raw) || empty($raw['computed_at'])) {
            return null;
        }
        return $raw;
    }

    /**
     * Just-in-time size resolver for the diagnostics collection path.
     *
     * Decision tree (mirrors the WP Site Health screen's own on-demand approach):
     *   (a) FRESH last-good exists (< FRESH_THRESHOLD_SECONDS old) — return it
     *       immediately; no compute, no I/O beyond the option read.
     *   (b) No fresh last-good — compute NOW: bump set_time_limit best-effort,
     *       then run compute() (du fast path + PHP/recurse_dirsize fallback,
     *       exactly as the cron handler does). Persist the result and return it.
     *   (c) compute() throws / returns an empty blob — fall back to the previous
     *       last-good (stale is better than nothing). Return it with partial=true.
     *   (d) Nothing at all (no prior, compute failed) — return null; the caller
     *       will emit 'pending' and schedule a HOOK_SIZES one-shot.
     *
     * Called inline from DiagnosticsCommand::mergeDirectorySizes() so the FIRST
     * diagnostics push already ships real sizes — no separate cron tick required.
     * The dedicated HOOK_SIZES cron + post-ship warm in Plugin::runDiagnostics()
     * are kept as secondary cache-warmers so big sites stay warm without
     * blocking subsequent pushes.
     *
     * @return array<string,mixed>|null Fresh/computed blob, stale fallback, or null.
     */
    public function getOrCompute(): ?array
    {
        $prior = $this->lastGood();

        // (a) Fresh cache hit — return immediately.
        if ($prior !== null) {
            $computedAt = (int) ($prior['computed_at'] ?? 0);
            if ($computedAt > 0 && (time() - $computedAt) < self::FRESH_THRESHOLD_SECONDS) {
                return $prior;
            }
        }

        // (b) Stale or missing — compute just-in-time.
        // Raise the time limit so a cold recurse_dirsize walk does not get
        // killed by the push request's max_execution_time (same guard the
        // dedicated cron handler applies in Plugin::runSizeProbe).
        if (function_exists('set_time_limit')) {
            @set_time_limit(0);
        }

        try {
            $blob = $this->compute();
            // (c) compute() succeeded but produced an empty sizes array — treat
            //     as partial failure and prefer prior if available.
            $hasAnySizes = !empty($blob['sizes']) && is_array($blob['sizes'])
                && count($blob['sizes']) > 0;
            if ($hasAnySizes) {
                return $blob;
            }
            // Empty sizes (both du and PHP recursion failed for every dir).
            if ($prior !== null) {
                // Return stale prior; mark it partial so the UI shows a chip.
                $prior['partial'] = true;
                return $prior;
            }
            // We got a blob with volume stats only (disk_total/free) — still
            // useful; return it so disk_total/free appear even if dir sizes are 0.
            return $blob;
        } catch (\Throwable $e) {
            // (d) compute() threw — fall back to stale prior.
            if ($prior !== null) {
                $prior['partial'] = true;
                return $prior;
            }
            return null;
        }
    }

    /**
     * Run the full size computation and persist the result.
     *
     * Safe to call under a dedicated cron event (set_time_limit(0) is called
     * by the handler in Plugin::registerHooks before invoking this). Also safe
     * to call after fastcgi_finish_request — a PHP-FPM kill mid-walk leaves
     * the previously-persisted last-good intact.
     *
     * @return array<string,mixed> The persisted blob (same shape as lastGood()).
     */
    public function compute(): array
    {
        $prior = $this->lastGood();

        $useExec = $this->execAvailable();
        $method  = 'disk'; // fallback if we can only get volume stats

        // Directories to measure (keyed by the wp-paths-sizes field key).
        $dirs = $this->targetDirs();

        $sizes   = [];
        $partial = false;

        foreach ($dirs as $key => $dir) {
            if ($key === 'database_size') {
                // Database size handled separately below.
                continue;
            }
            if ($dir === '' || !is_string($dir)) {
                if ($prior !== null && isset($prior['sizes'][$key])) {
                    $sizes[$key] = $prior['sizes'][$key];
                    $partial = true;
                }
                continue;
            }

            $bytes = null;
            if ($useExec && $dir !== '') {
                $bytes = $this->duBytes($dir);
                if ($bytes !== null) {
                    $method = 'du';
                }
            }

            if ($bytes === null) {
                // PHP fallback — recurse_dirsize if available.
                $bytes = $this->phpBytes($dir);
                if ($bytes !== null && $method !== 'du') {
                    $method = 'php';
                }
            }

            if ($bytes === null) {
                // Miss — keep prior value and mark partial.
                if ($prior !== null && isset($prior['sizes'][$key])) {
                    $sizes[$key] = $prior['sizes'][$key];
                }
                $partial = true;
                continue;
            }

            $sizes[$key] = [
                'bytes' => $bytes,
                'human' => $this->formatBytes($bytes),
            ];
        }

        // Database size — via WP_Debug_Data::get_sizes() or wpdb->get_var.
        $dbBytes = $this->databaseBytes();
        if ($dbBytes !== null) {
            $sizes['database_size'] = [
                'bytes' => $dbBytes,
                'human' => $this->formatBytes($dbBytes),
            ];
        } elseif ($prior !== null && isset($prior['sizes']['database_size'])) {
            $sizes['database_size'] = $prior['sizes']['database_size'];
            $partial = true;
        }

        // Volume: always O(1).
        $diskTotal = null;
        $diskFree  = null;
        $contentDir = defined('WP_CONTENT_DIR') ? (string) constant('WP_CONTENT_DIR') : '';
        if ($contentDir !== '' && function_exists('disk_total_space')) {
            $dt = @disk_total_space($contentDir);
            if (is_numeric($dt)) {
                $diskTotal = (int) $dt;
            }
        }
        if ($contentDir !== '' && function_exists('disk_free_space')) {
            $df = @disk_free_space($contentDir);
            if (is_numeric($df)) {
                $diskFree = (int) $df;
            }
        }

        $blob = [
            'computed_at'      => time(),
            'method'           => $method,
            'partial'          => $partial,
            'disk_total_bytes' => $diskTotal,
            'disk_free_bytes'  => $diskFree,
            'sizes'            => $sizes,
        ];

        if (function_exists('update_option')) {
            update_option(self::OPTION_SIZES, $blob, false); // false = non-autoloaded
        }

        return $blob;
    }

    /**
     * Register the pre_recurse_dirsize filter so WP core's own Site Health
     * screen and our PHP fallback both short-circuit to du (WP 5.6+).
     *
     * Returns an int byte count (short-circuits the walk) or false (lets WP
     * continue with its own recursion). Bound via add_filter so the filter is
     * only installed when the probe class is instantiated (Plugin::registerHooks
     * calls this once during boot).
     *
     * @return void
     */
    public function registerPreRecurseFilter(): void
    {
        if (!function_exists('add_filter')) {
            return;
        }
        add_filter(
            'pre_recurse_dirsize',
            [$this, 'preRecurseDirsizeFilter'],
            10,
            5
        );
    }

    /**
     * Filter callback for pre_recurse_dirsize.
     *
     * @param false|int            $size           Existing filtered value.
     * @param string               $directory      Directory being measured.
     * @param array<string>|null   $exclude        Exclusion list (WP core).
     * @param int                  $maxExecTime    Budget from WP (ignored here).
     * @param array<string,int>    $directoryCache WP's in-memory cache.
     * @return false|int
     */
    public function preRecurseDirsizeFilter(
        $size,
        string $directory,
        $exclude,
        int $maxExecTime,
        array $directoryCache
    ) {
        if ($size !== false) {
            return $size; // already handled upstream
        }
        if (!$this->execAvailable()) {
            return false; // let WP continue with PHP recursion
        }
        $bytes = $this->duBytes($directory);
        if ($bytes === null) {
            return false;
        }
        return $bytes;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Map wp-paths-sizes field keys to their filesystem paths.
     *
     * @return array<string,string>
     */
    private function targetDirs(): array
    {
        $abspath    = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
        $contentDir = defined('WP_CONTENT_DIR') ? rtrim((string) constant('WP_CONTENT_DIR'), '/\\') : '';

        $uploadsDir = '';
        if (function_exists('wp_get_upload_dir')) {
            $u = wp_get_upload_dir();
            if (is_array($u) && isset($u['basedir']) && is_string($u['basedir'])) {
                $uploadsDir = rtrim($u['basedir'], '/\\');
            }
        }

        $themesDir  = $contentDir !== '' ? $contentDir . '/themes' : '';
        $pluginsDir = $contentDir !== '' ? $contentDir . '/plugins' : '';

        // wordpress_size excludes uploads/themes/plugins to avoid double-counting
        // (mirrors WP core's own get_sizes() exclusion list).
        return [
            'wordpress_size' => $abspath,
            'uploads_size'   => $uploadsDir,
            'themes_size'    => $themesDir,
            'plugins_size'   => $pluginsDir,
            'database_size'  => '', // placeholder — handled by databaseBytes()
        ];
    }

    /**
     * Exclusion paths for the `du` and PHP fallback (relative segments to skip).
     * These are expensive / irrelevant / pathological subtrees.
     *
     * @return array<string>
     */
    private function excludeList(): array
    {
        $contentDir = defined('WP_CONTENT_DIR') ? rtrim((string) constant('WP_CONTENT_DIR'), '/\\') : '';
        $exclude    = [
            'node_modules',
            'vendor',
            '.git',
            '.svn',
        ];
        // Own staging + backup scratch dirs (avoid measuring our own temp output).
        if ($contentDir !== '') {
            $exclude[] = $contentDir . '/.wpmgr-staging-';
            $exclude[] = $contentDir . '/.wpmgr-old-files-';
        }
        return $exclude;
    }

    /**
     * Run `du -sb <dir>` to get byte count (GNU coreutils).
     * Falls back to `du -sk <dir>` * 1024 (BSD / macOS).
     *
     * Returns null on any failure.
     *
     * @param string $dir Absolute path, already validated as readable.
     * @return int|null
     */
    private function duBytes(string $dir): ?int
    {
        if ($dir === '' || !is_dir($dir)) {
            return null;
        }

        // Build exclude args from the list.
        $excludeArgs = '';
        foreach ($this->excludeList() as $exc) {
            // du --exclude works with basenames and full paths on GNU du.
            $excludeArgs .= ' --exclude=' . escapeshellarg($exc);
        }

        // Try GNU `du -sb` (bytes).
        $escaped = escapeshellarg($dir);
        $output  = [];
        $retval  = 0;
        $cmd     = 'du -sb' . $excludeArgs . ' ' . $escaped . ' 2>/dev/null';
        @exec($cmd, $output, $retval);
        if ($retval === 0 && !empty($output[0])) {
            $parts = preg_split('/\s+/', trim($output[0]), 2);
            if (is_array($parts) && isset($parts[0]) && ctype_digit($parts[0])) {
                return (int) $parts[0];
            }
        }

        // BSD / macOS fallback: `du -sk` (1 KiB blocks) * 1024.
        $output = [];
        $retval = 0;
        $cmd    = 'du -sk' . $excludeArgs . ' ' . $escaped . ' 2>/dev/null';
        @exec($cmd, $output, $retval);
        if ($retval === 0 && !empty($output[0])) {
            $parts = preg_split('/\s+/', trim($output[0]), 2);
            if (is_array($parts) && isset($parts[0]) && ctype_digit($parts[0])) {
                return (int) $parts[0] * 1024;
            }
        }

        return null;
    }

    /**
     * Measure a directory using WP core's recurse_dirsize() with set_time_limit(0)
     * and our exclude list. Called only from the dedicated cron context.
     *
     * @param string $dir Absolute directory path.
     * @return int|null Byte count, or null if recurse_dirsize is unavailable or returns null.
     */
    private function phpBytes(string $dir): ?int
    {
        if ($dir === '' || !is_dir($dir) || !function_exists('recurse_dirsize')) {
            return null;
        }
        // Ensure there is no time ceiling on this call (caller has already
        // called set_time_limit(0) in the cron handler, but be explicit).
        if (function_exists('set_time_limit')) {
            @set_time_limit(0);
        }

        $exclude = $this->excludeList();
        // recurse_dirsize signature: ($directory, $exclude=null, $max_execution_time=null, &$directory_cache=null)
        // Passing max_execution_time=0 means no time budget.
        try {
            $result = recurse_dirsize($dir, $exclude, 0);
        } catch (\Throwable $e) {
            return null;
        }
        if (!is_int($result) || $result < 0) {
            return null;
        }
        return $result;
    }

    /**
     * Get database size in bytes via wpdb.
     *
     * Uses `SELECT SUM(data_length + index_length) FROM information_schema.TABLES
     * WHERE table_schema = DATABASE()` — works on MySQL 5.6+ and MariaDB.
     *
     * @return int|null
     */
    private function databaseBytes(): ?int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        try {
            $bytes = $wpdb->get_var(
                "SELECT SUM(data_length + index_length)
                 FROM information_schema.TABLES
                 WHERE table_schema = DATABASE()"
            );
            if (is_numeric($bytes)) {
                return (int) $bytes;
            }
        } catch (\Throwable $e) {
            // Swallow — database unavailable.
        }
        return null;
    }

    /**
     * Hard feature-detect for exec availability. Result is cached in a 1-hour
     * transient so the smoke-test does not run on every cron tick.
     *
     * Gates: function_exists('exec'), exec not in disable_functions, safe_mode
     * off, live smoke test `exec('echo wpmgrprobe')` === 'wpmgrprobe', and
     * (when open_basedir is set) the ABSPATH is inside open_basedir.
     *
     * @return bool
     */
    private function execAvailable(): bool
    {
        // Check transient cache first.
        if (function_exists('get_transient')) {
            $cached = get_transient(self::EXEC_PROBE_KEY);
            if ($cached === '1') {
                return true;
            }
            if ($cached === '0') {
                return false;
            }
        }

        $result = $this->runExecProbe();

        // Cache the result for 1 hour to avoid repeated smoke tests.
        if (function_exists('set_transient')) {
            set_transient(self::EXEC_PROBE_KEY, $result ? '1' : '0', 3600);
        }

        return $result;
    }

    /**
     * Run the actual exec capability probe. No caching.
     *
     * @return bool
     */
    private function runExecProbe(): bool
    {
        // 1. function_exists check.
        if (!function_exists('exec')) {
            return false;
        }

        // 2. disable_functions check.
        $disabled = (string) ini_get('disable_functions');
        if ($disabled !== '') {
            $disabledList = array_map('trim', explode(',', strtolower($disabled)));
            if (in_array('exec', $disabledList, true)) {
                return false;
            }
        }

        // 3. safe_mode check (PHP < 5.4 only; always false on 8.x but guard anyway).
        if ((bool) ini_get('safe_mode')) {
            return false;
        }

        // 4. open_basedir check — ABSPATH must be accessible.
        $openBasedir = (string) ini_get('open_basedir');
        if ($openBasedir !== '') {
            $abspath = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
            if ($abspath !== '' && !$this->pathInsideOpenBasedir($abspath, $openBasedir)) {
                return false;
            }
        }

        // 5. Live smoke test — avoids false-positives on restrictive hosting.
        $output = [];
        $retval = 0;
        @exec('echo wpmgrprobe', $output, $retval);
        if ($retval !== 0 || !isset($output[0]) || trim($output[0]) !== 'wpmgrprobe') {
            // Cache as unavailable so we don't repeat the smoke test every call.
            if (function_exists('set_transient')) {
                set_transient(self::EXEC_PROBE_KEY, '0', 3600);
            }
            return false;
        }

        return true;
    }

    /**
     * Check whether $path is accessible under the open_basedir restriction.
     *
     * @param string $path Absolute path to check.
     * @param string $openBasedir Raw ini_get('open_basedir') value.
     * @return bool
     */
    private function pathInsideOpenBasedir(string $path, string $openBasedir): bool
    {
        $separator = defined('PATH_SEPARATOR') ? PATH_SEPARATOR : (DIRECTORY_SEPARATOR === '\\' ? ';' : ':');
        $allowed   = explode($separator, $openBasedir);
        foreach ($allowed as $allowedPath) {
            $allowedPath = rtrim(trim($allowedPath), '/\\');
            if ($allowedPath === '') {
                continue;
            }
            if (str_starts_with($path, $allowedPath)) {
                return true;
            }
        }
        return false;
    }

    /**
     * Human-readable byte formatter matching WP core's size_format() output.
     *
     * @param int $bytes Raw byte count.
     * @return string Human-readable string, e.g. "1.5 GB".
     */
    private function formatBytes(int $bytes): string
    {
        if (function_exists('size_format')) {
            $result = size_format($bytes, 2);
            if (is_string($result) && $result !== '') {
                return $result;
            }
        }
        // Fallback if WP is not loaded.
        $units = ['B', 'KB', 'MB', 'GB', 'TB'];
        $i     = 0;
        $value = (float) $bytes;
        while ($value >= 1024 && $i < count($units) - 1) {
            $value /= 1024;
            $i++;
        }
        return round($value, 2) . ' ' . $units[$i];
    }
}
