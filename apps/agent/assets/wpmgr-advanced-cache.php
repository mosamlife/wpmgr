<?php
/**
 * WPMgr Page Cache drop-in (advanced-cache.php).
 *
 * WordPress loads this file VERY early (from wp-content/advanced-cache.php) when
 * the WP_CACHE constant is true — before plugins, before the theme, before most
 * of WordPress. It therefore runs with almost nothing loaded and must be fully
 * self-contained: the live cache config is inlined into $wpmgr_config below at install
 * time (the CacheManager var_export()s it over the CONFIG_TO_REPLACE token), so
 * this file makes zero DB/plugin calls on a cache hit.
 *
 * On a HIT it streams the pre-gzipped page straight from disk and exit()s. On a
 * MISS (or any non-cacheable request) it `return false` and hands control back
 * to WordPress, which boots normally and (via the plugin's output-buffer writer)
 * may populate the cache for next time.
 *
 * The cache-key algorithm here MUST stay byte-identical to the PHP-side builder
 * (WPMgr\Agent\Cache\CacheKey): same logged-in/role/include-cookie/mobile/query
 * segments, same ksort + md5(serialize()) query hash, same path normalisation.
 *
 * Standard WordPress disk-cache serving technique.
 *
 * Direct access is blocked by the ABSPATH guard below. WordPress includes this
 * drop-in from wp-settings.php, by which point ABSPATH is already defined, so
 * the guard never fires during a normal cache hit; it only stops a direct web
 * request to the file.
 *
 * @package WPMgr\Agent\Cache
 */

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Drop-in version — bumped whenever the installed content changes structurally.
 * DropinInstaller compares this constant in the template against the string it
 * finds in the on-disk copy; a mismatch triggers a transparent reinstall so
 * existing sites always run the current drop-in logic without manual intervention.
 */
define('WPMGR_PAGE_CACHE_DROPIN_VERSION', '0.45.1');

if (!defined('WP_CACHE')) {
    return;
}

// The live config is inlined here at install time.
$wpmgr_config = CONFIG_TO_REPLACE; // phpcs:ignore WordPress.WP.GlobalVariablesOverride.Prohibited

// ---------------------------------------------------------------------------
// WP-Cron kick helper — P4a: keep WP-Cron running on fully page-cached sites.
//
// On a cache HIT WordPress never boots, so WP-Cron never gets the chance to
// spawn. This function fires a single fire-and-forget loopback to wp-cron.php
// when the kick marker file is stale (older than $interval seconds) or absent.
//
// Design constraints:
//   - ZERO DB calls. The overdue check is a single stat() on a marker file.
//   - The visitor response is flushed BEFORE the socket is opened; the kick
//     is best-effort and never holds the worker if the loopback is slow.
//   - The marker is touched BEFORE the socket connect to prevent concurrent
//     HITs within the same window from stampeding (benign race accepted).
//   - Every timeout is bounded; all errors are silently swallowed.
//   - Host is derived from the already-sanitised $wpmgr_host (validated via
//     /^[a-z0-9.-]+(:[0-9]{1,5})?$/ earlier in this file) — never from raw
//     superglobals.
//   - Disabled when $wpmgr_config['cron_kick_enabled'] is explicitly false.
// ---------------------------------------------------------------------------

/**
 * Decide whether a WP-Cron loopback kick is overdue and, if so, fire one.
 *
 * Extracted as a named function so it can be called from all HIT exit points
 * (full body, 304, HEAD) and tested in isolation without running the drop-in.
 *
 * @param string $markerFile Absolute path to the throttle-marker file.
 * @param int    $interval   Minimum seconds between kicks.
 * @param string $host       Already-validated hostname (+ optional :port).
 * @param string $scheme     'https' or 'http'.
 * @return bool True when a kick was fired (marker was stale/absent), false otherwise.
 */
function wpmgr_cron_kick_if_overdue(string $markerFile, int $interval, string $host, string $scheme): bool // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedFunctionFound -- drop-in runs in global scope pre-WP; wpmgr_ prefix applied
{
    // ---- Overdue check: single stat(), zero DB --------------------------------
    $now   = time();
    $mtime = @filemtime($markerFile);
    if ($mtime !== false && ($now - $mtime) < $interval) {
        return false; // marker fresh — no kick
    }

    // ---- Mark immediately to prevent concurrent stampede ----------------------
    // Touch the marker BEFORE the socket connect. A benign race (two concurrent
    // HITs both see stale and both touch) is accepted — the worst case is two
    // cron calls within the same second, which is harmless.
    $markerDir = dirname($markerFile);
    if (!@is_dir($markerDir)) {
        @mkdir($markerDir, 0755, true); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir,WordPress.Security.PluginDirectoryWrite.PluginDirectoryWrite -- advanced-cache drop-in runs pre-WP; wp_mkdir_p unavailable; writes to wp-content/cache/wpmgr
    }
    @file_put_contents($markerFile, (string) $now); // phpcs:ignore PluginCheck.CodeAnalysis.WriteFile.PluginDirectoryWrite -- writes to wp-content/cache/wpmgr, a persistent install target outside the plugin folder

    // ---- Fire a non-blocking loopback to wp-cron.php -------------------------
    // Host is already validated (/^[a-z0-9.-]+(:[0-9]{1,5})?$/) earlier in the
    // drop-in. We reuse it verbatim; never parse raw superglobals here.
    // Strip any :port suffix for the TCP connect; use the full value in Host:.
    $hostOnly = (string) preg_replace('/:[0-9]{1,5}$/', '', $host);
    $port     = ($scheme === 'https') ? 443 : 80;
    if (preg_match('/:([0-9]{1,5})$/', $host, $m) === 1) {
        $port = (int) $m[1];
    }
    $cronPath = '/wp-cron.php?doing_wp_cron=' . $now;
    $request  = 'GET ' . $cronPath . ' HTTP/1.1' . "\r\n"
        . 'Host: ' . $hostOnly . "\r\n"
        . 'Connection: close' . "\r\n"
        . 'User-Agent: WPMgr-CronKick/1.0' . "\r\n\r\n";

    try {
        $connectHost = ($scheme === 'https') ? 'ssl://' . $hostOnly : $hostOnly;
        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fsockopen -- pre-WP drop-in; WP_HTTP not loaded; fsockopen is the only non-blocking loopback primitive available at this execution stage; fire-and-forget cron kick
        $sock = @fsockopen($connectHost, $port, $errno, $errstr, 1.0);
        if ($sock !== false) {
            @stream_set_timeout($sock, 1); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort; failure is intentionally ignored
            @fwrite($sock, $request); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite,WordPress.PHP.NoSilencedErrors.Discouraged -- fire-and-forget write; no response read
            @fclose($sock); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose,WordPress.PHP.NoSilencedErrors.Discouraged -- close immediately; no response read
        }
    } catch (\Throwable $e) { // phpcs:ignore Generic.CodeAnalysis.EmptyStatement.DetectedCatch -- best-effort; kick failure must never affect the served page
        // Swallow — kick failure must never affect the served page.
    }

    return true;
}

if (!is_array($wpmgr_config)) {
    return false;
}

// Emit the MISS markers up-front; overwritten to HIT on a cache hit below.
if (!headers_sent()) {
    header('x-wpmgr-cache: MISS');
    header('x-wpmgr-source: PHP');
}

// --- Skip gates: hand back to WordPress (return false) -----------------------

// WP-CLI requests must never serve a cached page.
if (defined('WP_CLI') && WP_CLI) {
    return false;
}

// ---------------------------------------------------------------------------
// NOTE ON $_SERVER SANITIZATION IN THIS FILE
//
// This drop-in runs before WordPress loads — sanitize_text_field(), wp_unslash(),
// and all other core helper functions are unavailable here. Every $_SERVER value
// is therefore validated and sanitized using strict plain-PHP primitives:
//   - allowlists via in_array() with strict comparison
//   - character-class whitelists via preg_match() / preg_replace()
//   - control-character stripping via preg_replace('/[\x00-\x1F\x7F]/', '', ...)
//   - length caps before any value reaches cache-key or header logic
// Validation failures fall through to the existing bypass or default value —
// never fatal. See each read below for its per-value rationale.
// ---------------------------------------------------------------------------

// Preload warming: let WordPress render fresh HTML so the writer can store it.
// Only the presence of the header is checked; the value is not consumed.
if (isset($_SERVER['HTTP_X_WPMGR_PRELOAD'])) { // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- value not consumed; presence-check only; no WP sanitizers available at drop-in load time
    return false;
}

// Only GET / HEAD are cacheable. Allowlist via strtoupper + in_array —
// no WP sanitizers available at drop-in load time.
// phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; value allowlisted via strtoupper+in_array below
$wpmgr_method_raw = isset($_SERVER['REQUEST_METHOD']) ? strtoupper((string) $_SERVER['REQUEST_METHOD']) : 'GET';
$wpmgr_method     = in_array($wpmgr_method_raw, array('GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'), true)
    ? $wpmgr_method_raw : 'GET';
if ($wpmgr_method !== 'GET' && $wpmgr_method !== 'HEAD') {
    return false;
}

// Bypass cookies: any matching cookie name disables the cache for this request.
// When woo_cacheable_session is ON the three WooCommerce cart/session cookie
// patterns are listed in woo_ignore_cookies instead of bypass_cookies, so they
// neither bypass nor key the cache — the anonymous visitor maps to the same
// shared shell as a no-cookie visitor. When the flag is OFF this array is empty
// and the bypass set is byte-identical to the pre-feature behaviour.
$wpmgr_bypass_cookies = isset($wpmgr_config['bypass_cookies']) && is_array($wpmgr_config['bypass_cookies'])
    ? $wpmgr_config['bypass_cookies'] : array();
$wpmgr_woo_ignore = isset($wpmgr_config['woo_ignore_cookies']) && is_array($wpmgr_config['woo_ignore_cookies'])
    ? array_map('strtolower', $wpmgr_config['woo_ignore_cookies']) : array();
if (!empty($_COOKIE) && $wpmgr_bypass_cookies) {
    $wpmgr_cookie_names = array_keys($_COOKIE);
    foreach ($wpmgr_bypass_cookies as $wpmgr_bypass) {
        if ($wpmgr_bypass === '') {
            continue;
        }
        foreach ($wpmgr_cookie_names as $wpmgr_cn) {
            if (stripos((string) $wpmgr_cn, (string) $wpmgr_bypass) !== false) {
                return false;
            }
        }
    }
}
// Logged-in guard: even when woo_cacheable_session is ON, a wordpress_logged_in_*
// cookie always forces a cache bypass (logged-in users never receive a shared shell).
// This is already in the bypass_cookies list above, but we make it explicit as a
// defence-in-depth guard for readability and to document the invariant.
// (No additional code needed — wordpress_logged_in_ remains in bypass_cookies.)

// --- Build the cache file name (mirrors CacheKey::build) ----------------------

$wpmgr_name = 'index';

// 1. logged-in segment.
$wpmgr_logged_in = false;
if (!empty($_COOKIE)) {
    foreach (array_keys($_COOKIE) as $wpmgr_ck) {
        if (preg_match('/^wordpress_logged_in_/i', (string) $wpmgr_ck) === 1) {
            $wpmgr_logged_in = true;
            break;
        }
    }
}
if ($wpmgr_logged_in) {
    if (empty($wpmgr_config['cache_logged_in'])) {
        return false; // logged-in caching disabled — serve via PHP
    }
    $wpmgr_name .= '-logged-in';

    // 2. role segment (from the non-HTTPOnly role cookie).
    if (isset($_COOKIE['wpmgr_logged_in_roles'])) {
        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; value regex-filtered below
        $wpmgr_role = strtolower((string) $_COOKIE['wpmgr_logged_in_roles']);
        $wpmgr_role = preg_replace('/[^a-z0-9\-_]/', '', $wpmgr_role);
        if ($wpmgr_role !== '' && $wpmgr_role !== null) {
            $wpmgr_name .= '-' . $wpmgr_role;
        }
    }
}

// 3. include-cookie segments, in configured order.
$wpmgr_include_cookies = isset($wpmgr_config['include_cookies']) && is_array($wpmgr_config['include_cookies'])
    ? $wpmgr_config['include_cookies'] : array();
foreach ($wpmgr_include_cookies as $wpmgr_inc) {
    if ($wpmgr_inc !== '' && isset($_COOKIE[$wpmgr_inc]) && is_scalar($_COOKIE[$wpmgr_inc])) {
        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; value regex-filtered below
        $wpmgr_val = strtolower((string) $_COOKIE[$wpmgr_inc]);
        $wpmgr_val = preg_replace('/[^a-z0-9\-_]/', '', $wpmgr_val);
        if ($wpmgr_val !== '' && $wpmgr_val !== null) {
            $wpmgr_name .= '-' . $wpmgr_val;
        }
    }
}

// 4. mobile segment.
// Strip control characters and cap length before regex matching — no WP
// sanitizers available at drop-in load time. The regex match is read-only
// (produces a name suffix); the raw UA value is never echoed or stored.
if (!empty($wpmgr_config['cache_mobile']) && isset($_SERVER['HTTP_USER_AGENT'])) {
    // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; control chars stripped + length capped before use
    $wpmgr_ua_raw = (string) $_SERVER['HTTP_USER_AGENT'];
    $wpmgr_ua     = substr(preg_replace('/[\x00-\x1F\x7F]/', '', $wpmgr_ua_raw), 0, 512);
    if (preg_match(
        '/Mobile|Android|Silk\/|Kindle|BlackBerry|Opera (Mini|Mobi)|iPhone|iPad|iPod|IEMobile/i',
        $wpmgr_ua
    ) === 1) {
        $wpmgr_name .= '-mobile';
    }
}

// 5. query-hash segment (drop marketing params, ksort, md5(serialize())).
// MUST stay byte-identical to WPMgr\Agent\Cache\CacheKey, including the
// 12-distinct-key cap: over the cap the request is non-cacheable (return false)
// so an attacker cannot mint unbounded cache files via arbitrary distinct params.
$wpmgr_ignore = isset($wpmgr_config['ignore_queries']) && is_array($wpmgr_config['ignore_queries'])
    ? array_map('strtolower', $wpmgr_config['ignore_queries']) : array();
// phpcs:ignore WordPress.Security.NonceVerification.Recommended -- advanced-cache drop-in runs pre-WP; nonce verification unavailable; query keys are key-hashed for cache routing only (read-only, no state change)
if (!empty($_GET)) {
    $wpmgr_kept = array();
    // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- advanced-cache drop-in runs pre-WP; nonce verification unavailable; query keys are key-hashed for cache routing only
    foreach ($_GET as $wpmgr_qk => $wpmgr_qv) {
        if (in_array(strtolower((string) $wpmgr_qk), $wpmgr_ignore, true)) {
            continue;
        }
        $wpmgr_kept[(string) $wpmgr_qk] = $wpmgr_qv;
    }
    if (count($wpmgr_kept) > 12) {
        return false; // non-cacheable — too many cache-varying query keys
    }
    if (!empty($wpmgr_kept)) {
        ksort($wpmgr_kept);
        $wpmgr_name .= '-' . md5(serialize($wpmgr_kept));
    }
}

// --- Locate the cache file ----------------------------------------------------

// HTTP_HOST: strict charset validation guards against cache-poisoning via a
// crafted Host header. Accept only hostname characters + optional port; reject
// (treat as cache bypass via 'unknown-host') anything else. No WP sanitizers
// available at drop-in load time.
// phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; value strictly validated via preg_match allowlist below
$wpmgr_host_raw = isset($_SERVER['HTTP_HOST']) ? strtolower((string) $_SERVER['HTTP_HOST']) : '';
if ($wpmgr_host_raw !== '' && preg_match('/^[a-z0-9.-]+(:[0-9]{1,5})?$/', $wpmgr_host_raw) === 1) {
    $wpmgr_host = $wpmgr_host_raw;
} else {
    $wpmgr_host = 'unknown-host';
}

// REQUEST_URI: strip control characters and cap length before use as a
// cache-key component. Path-traversal sequences are removed below.
// No WP sanitizers available at drop-in load time.
// phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; control chars stripped + length capped before use as cache key
$wpmgr_uri_raw = isset($_SERVER['REQUEST_URI']) ? (string) $_SERVER['REQUEST_URI'] : '/';
$wpmgr_uri     = substr(preg_replace('/[\x00-\x1F\x7F]/', '', $wpmgr_uri_raw), 0, 2048);
unset($wpmgr_uri_raw);
// Treat an empty URI (after stripping) as root.
if ($wpmgr_uri === '') {
    $wpmgr_uri = '/';
}
$wpmgr_qpos = strpos($wpmgr_uri, '?');
if ($wpmgr_qpos !== false) {
    $wpmgr_uri = substr($wpmgr_uri, 0, $wpmgr_qpos);
}
$wpmgr_path = strtolower(rawurldecode($wpmgr_uri));
$wpmgr_path = str_replace(array('\\', "\0"), array('/', ''), $wpmgr_path);
$wpmgr_path = preg_replace('#/+#', '/', $wpmgr_path);
$wpmgr_path = preg_replace('#(\.\./|/\.\.)#', '', (string) $wpmgr_path);
$wpmgr_path = '/' . ltrim((string) $wpmgr_path, '/');
$wpmgr_path = rtrim($wpmgr_path, '/'); // '' for root

// Bypass URLs: any configured substring in the request URI/path disables the
// cache. This runs BEFORE locating an existing cache file so an already-warmed
// file cannot be served for a URL the operator marked as non-cacheable.
// Matches are case-insensitive substring containment, identical to the PHP
// cacheability path, so the pre-WP fast path and the write layer agree.
$wpmgr_bypass_urls = isset($wpmgr_config['bypass_urls']) && is_array($wpmgr_config['bypass_urls'])
    ? $wpmgr_config['bypass_urls'] : array();
if (!empty($wpmgr_bypass_urls) && $wpmgr_uri !== '') {
    foreach ($wpmgr_bypass_urls as $wpmgr_bypass_url) {
        if ($wpmgr_bypass_url === '') {
            continue;
        }
        if (stripos($wpmgr_uri, (string) $wpmgr_bypass_url) !== false) {
            return false;
        }
    }
}

$wpmgr_content = defined('WP_CONTENT_DIR') ? (string) WP_CONTENT_DIR : (dirname(__DIR__));
$wpmgr_file = rtrim($wpmgr_content, '/\\')
    . '/cache/wpmgr/' . $wpmgr_host . $wpmgr_path . '/' . $wpmgr_name . '.html.gz';

// Resolve the tally metrics dir once, before the HIT/MISS branch.
// Stored in a local var so both branches share the same computation.
$wpmgr_metrics_dir = rtrim($wpmgr_content, '/\\') . '/cache/wpmgr/.metrics';
$wpmgr_tally_hour  = gmdate('YmdH');

// --- Cron-kick config (resolved once; used only on a HIT below) ---------------
// Read from the baked-in config so no DB/WP calls are needed at runtime.
// cron_kick_enabled defaults true; cron_kick_interval defaults 60 seconds.
$wpmgr_cron_kick_enabled  = !isset($wpmgr_config['cron_kick_enabled']) || (bool) $wpmgr_config['cron_kick_enabled'];
$wpmgr_cron_kick_interval = isset($wpmgr_config['cron_kick_interval']) ? max(10, (int) $wpmgr_config['cron_kick_interval']) : 60;
$wpmgr_cron_kick_marker   = rtrim($wpmgr_content, '/\\') . '/cache/wpmgr/.wpmgr-cron-kick';
// Detect scheme from HTTPS server variable (same approach WordPress core uses
// internally before it fully boots). Already sanitised host ($wpmgr_host) is
// reused verbatim — no additional superglobal reads.
$wpmgr_cron_kick_scheme = (isset($_SERVER['HTTPS']) && $_SERVER['HTTPS'] !== '' && $_SERVER['HTTPS'] !== 'off') // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- presence/value check only; 'off' guard; no WP sanitizers available at drop-in load time
    ? 'https' : 'http';

if (!is_file($wpmgr_file)) {
    // MISS — append one line to the hour-bucket miss file, then hand back to WordPress.
    // One file_put_contents per miss: no DB, no WP calls, no flock.
    // The mkdir guard runs only when the bucket file is new (first miss of the hour).
    $wpmgr_miss_file = $wpmgr_metrics_dir . '/miss-' . $wpmgr_tally_hour;
    if (!@file_exists($wpmgr_miss_file)) {
        @mkdir($wpmgr_metrics_dir, 0755, true); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir,WordPress.Security.PluginDirectoryWrite.PluginDirectoryWrite -- advanced-cache drop-in runs pre-WP; wp_mkdir_p unavailable; writes to wp-content/cache/wpmgr, a persistent location outside the plugin folder
    }
    @file_put_contents($wpmgr_miss_file, "\n", FILE_APPEND); // phpcs:ignore PluginCheck.CodeAnalysis.WriteFile.PluginDirectoryWrite -- writes to wp-content/cache, a persistent install target outside the plugin folder
    return false; // MISS — boot WordPress
}

// --- Serve the cache hit ------------------------------------------------------

// HIT — append one line to the hour-bucket hit file BEFORE any early exits
// (304 Not Modified, HEAD). This counts every request served by the drop-in —
// full body, 304-revalidation, and HEAD — as a hit, matching the intended metric
// "served by the drop-in without booting WordPress".
// One file_put_contents: no DB, no WP calls, no flock.
// The mkdir guard runs only when the bucket file is new (first hit of the hour).
$wpmgr_hit_file = $wpmgr_metrics_dir . '/hit-' . $wpmgr_tally_hour;
if (!@file_exists($wpmgr_hit_file)) {
    @mkdir($wpmgr_metrics_dir, 0755, true); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir,WordPress.Security.PluginDirectoryWrite.PluginDirectoryWrite -- advanced-cache drop-in runs pre-WP; wp_mkdir_p unavailable; writes to wp-content/cache/wpmgr, a persistent location outside the plugin folder
}
@file_put_contents($wpmgr_hit_file, "\n", FILE_APPEND); // phpcs:ignore PluginCheck.CodeAnalysis.WriteFile.PluginDirectoryWrite -- writes to wp-content/cache, a persistent install target outside the plugin folder

if (function_exists('ini_set')) {
    @ini_set('zlib.output_compression', '0'); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- required runtime ini tweak; @-guarded
}

if (!headers_sent()) {
    header('Content-Encoding: gzip');
    header('Cache-Tag: ' . $wpmgr_host);
    header('CDN-Cache-Control: max-age=2592000');
    header('Cache-Control: no-cache, must-revalidate');
    header('x-wpmgr-cache: HIT');
    header('x-wpmgr-source: PHP');

    $wpmgr_mtime = filemtime($wpmgr_file);
    if ($wpmgr_mtime !== false) {
        header('Last-Modified: ' . gmdate('D, d M Y H:i:s', $wpmgr_mtime) . ' GMT');

        // HTTP_IF_MODIFIED_SINCE: strip control characters and cap length before
        // passing to strtotime() — garbage input already returns false, but
        // control chars could confuse logging. No WP sanitizers available.
        $wpmgr_ims_hdr = isset($_SERVER['HTTP_IF_MODIFIED_SINCE']) ? (string) $_SERVER['HTTP_IF_MODIFIED_SINCE'] : ''; // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; control chars stripped + length capped on next line before strtotime()
        $wpmgr_ims_raw = substr(preg_replace('/[\x00-\x1F\x7F]/', '', $wpmgr_ims_hdr), 0, 128);
        $wpmgr_ims = ($wpmgr_ims_raw !== '') ? strtotime($wpmgr_ims_raw) : 0;
        if ($wpmgr_ims !== false && $wpmgr_ims >= $wpmgr_mtime) {
            // SERVER_PROTOCOL: allowlist via preg_match before emitting in the
            // 304 status line — defense-in-depth against header injection.
            // No WP sanitizers available at drop-in load time.
            // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- advanced-cache drop-in runs pre-WP; wp_unslash/sanitize_* unavailable; value validated via preg_match allowlist before header() emission
            $wpmgr_proto_raw = isset($_SERVER['SERVER_PROTOCOL']) ? (string) $_SERVER['SERVER_PROTOCOL'] : 'HTTP/1.1';
            // Accept HTTP/1.0, HTTP/1.1, HTTP/2, HTTP/2.0, HTTP/3, HTTP/3.0.
            $wpmgr_proto = preg_match('#^HTTP/[0-9](\.[0-9])?$#', $wpmgr_proto_raw) === 1
                ? $wpmgr_proto_raw : 'HTTP/1.1';
            header($wpmgr_proto . ' 304 Not Modified', true, 304);
            // 304 is a HIT — fire the cron kick after the response headers are
            // committed. No body to flush, so the kick happens immediately.
            if ($wpmgr_cron_kick_enabled && $wpmgr_host !== 'unknown-host') {
                wpmgr_cron_kick_if_overdue(
                    $wpmgr_cron_kick_marker,
                    $wpmgr_cron_kick_interval,
                    $wpmgr_host,
                    $wpmgr_cron_kick_scheme
                );
            }
            exit();
        }
    }

    header('Content-Type: text/html; charset=UTF-8');
}

// HEAD requests get headers only.
if ($wpmgr_method === 'HEAD') {
    // HEAD is a HIT — fire the cron kick after headers are committed.
    if ($wpmgr_cron_kick_enabled && $wpmgr_host !== 'unknown-host') {
        wpmgr_cron_kick_if_overdue(
            $wpmgr_cron_kick_marker,
            $wpmgr_cron_kick_interval,
            $wpmgr_host,
            $wpmgr_cron_kick_scheme
        );
    }
    exit();
}

// Flush the cached page body to the visitor FIRST, then fire the cron kick
// as a best-effort background action. The visitor's response is complete
// before the socket is even opened.
readfile($wpmgr_file); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_readfile -- advanced-cache drop-in streams the cached body before WordPress is loaded; readfile is the canonical low-memory emit

// Flush to the client before the loopback kick so the visitor never waits.
if (function_exists('fastcgi_finish_request')) {
    @fastcgi_finish_request(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort FPM flush; failure falls back to ob_end_flush below
} else {
    @ob_end_flush(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort flush; not all SAPI/OB setups support this
    flush();
}

// Fire the WP-Cron loopback kick after the response has been handed off.
if ($wpmgr_cron_kick_enabled && $wpmgr_host !== 'unknown-host') {
    wpmgr_cron_kick_if_overdue(
        $wpmgr_cron_kick_marker,
        $wpmgr_cron_kick_interval,
        $wpmgr_host,
        $wpmgr_cron_kick_scheme
    );
}
exit();
