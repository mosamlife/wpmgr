<?php
/**
 * Plugin Name: WPMgr WAF (mu-plugin IP gate)
 * Description: Early IP-deny gate loaded at -PHP_INT_MAX, before WordPress and any
 *              other plugin boots. Reads wpmgr_security_config from wp_options (via
 *              a direct $wpdb query — the class autoloader is not available here).
 *              When mode == "protect" AND the client IP matches deny_cidrs AND does
 *              NOT match allow_cidrs, emits a 403 with no-cache headers and exits.
 *
 *              Filename starts with `a-` so alphabetical sort places it FIRST among
 *              installed mu-plugins (WordPress loads mu-plugins via glob() which is
 *              alphabetical). This guarantees the gate fires before any third-party
 *              mu-plugin loads.
 *
 * Installed by:
 *   `WPMgr\Agent\Support\MuPluginInstaller::installWaf()` — called from the agent
 *   plugin activation hook + on every `plugins_loaded` (idempotent: same content →
 *   no-op).
 *
 * Bootstrap-safe:
 *   - Pure procedural, no autoloader, no WPMgr namespace dependency.
 *   - Inline minimal CIDR match (IPv4 + IPv6) mirroring IpUtils::cidrMatch().
 *   - Direct wpdb SELECT via $wpdb — available this early because WordPress sets
 *     up $wpdb in wp-settings.php before loading mu-plugins.
 *   - All logic wrapped in try/catch so a DB failure NEVER blocks a real request.
 *   - Only calls exit() when a deliberate block decision is made.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

if (!defined('ABSPATH')) {
    exit; // No direct access outside WP.
}

// ---------------------------------------------------------------------------
// Priority: arm at -PHP_INT_MAX so this runs before any other mu-plugin.
// We wrap everything in a closure added to the `muplugins_loaded` action at
// the lowest possible priority so WordPress has initialised $wpdb already
// (it does so in wp-settings.php before calling muplugins_loaded).
// We CANNOT use add_action here because WP's action system is not ready yet
// at mu-plugin load time. Instead we execute immediately: $wpdb IS available
// at this point because wp-settings.php sets it up before including mu-plugins.
// ---------------------------------------------------------------------------

/**
 * Inline CIDR match — mirrors IpUtils::cidrMatch() without the autoloader.
 *
 * @param string $ip   IPv4 or IPv6 address string.
 * @param string $cidr CIDR notation, e.g. "203.0.113.0/24" or "2001:db8::/32".
 * @return bool
 */
function wpmgr_waf_cidr_match(string $ip, string $cidr): bool
{
    $ip   = trim($ip);
    $cidr = trim($cidr);

    if ($ip === '' || $cidr === '') {
        return false;
    }

    $parts = explode('/', $cidr, 2);
    if (count($parts) !== 2) {
        return false;
    }
    [$base, $prefixStr] = $parts;

    if (!ctype_digit($prefixStr)) {
        return false;
    }
    $prefix = (int) $prefixStr;

    $ipBin   = @inet_pton($ip);
    $baseBin = @inet_pton($base);

    if ($ipBin === false || $baseBin === false) {
        return false;
    }

    if (strlen($ipBin) !== strlen($baseBin)) {
        return false;
    }

    $addrLen  = strlen($ipBin);
    $maxPrefix = $addrLen * 8;

    if ($prefix < 0 || $prefix > $maxPrefix) {
        return false;
    }

    if ($prefix === 0) {
        return true;
    }

    $fullBytes = intdiv($prefix, 8);
    $remainder = $prefix % 8;

    for ($i = 0; $i < $fullBytes; $i++) {
        if (ord($ipBin[$i]) !== ord($baseBin[$i])) {
            return false;
        }
    }

    if ($remainder > 0 && $fullBytes < $addrLen) {
        $mask = 0xFF & (0xFF << (8 - $remainder));
        if ((ord($ipBin[$fullBytes]) & $mask) !== (ord($baseBin[$fullBytes]) & $mask)) {
            return false;
        }
    }

    return true;
}

/**
 * Test whether an address matches any CIDR in a list.
 *
 * @param string            $ip    Address to test.
 * @param array<int,string> $cidrs List of CIDR strings.
 * @return bool
 */
function wpmgr_waf_matches_any_cidr(string $ip, array $cidrs): bool
{
    foreach ($cidrs as $cidr) {
        if (is_string($cidr) && $cidr !== '' && wpmgr_waf_cidr_match($ip, $cidr)) {
            return true;
        }
    }
    return false;
}

/**
 * Return true when $ip is in a private/loopback/link-local range.
 * Mirrors IpUtils::isPrivate() without the autoloader.
 *
 * @param string $ip IPv4 or IPv6 address string.
 * @return bool
 */
function wpmgr_waf_is_private(string $ip): bool
{
    $ip = trim($ip);
    if ($ip === '') {
        return true;
    }

    $ipv4 = filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4);
    if ($ipv4 !== false) {
        $public = filter_var(
            $ipv4,
            FILTER_VALIDATE_IP,
            FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE
        );
        if ($public === false) {
            return true;
        }
        if (str_starts_with($ipv4, '169.254.')) {
            return true;
        }
        return false;
    }

    $ipv6 = filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV6);
    if ($ipv6 !== false) {
        $bin = @inet_pton($ipv6);
        if ($bin === false || strlen($bin) !== 16) {
            return true;
        }
        // ::1/128 loopback
        if ($bin === str_repeat("\x00", 15) . "\x01") {
            return true;
        }
        // ::ffff:0:0/96 — IPv4-mapped
        $v4Prefix = str_repeat("\x00", 10) . "\xff\xff";
        if (substr($bin, 0, 12) === $v4Prefix) {
            $mapped = inet_ntop(substr($bin, 12));
            if ($mapped !== false) {
                return wpmgr_waf_is_private($mapped);
            }
        }
        $first  = ord($bin[0]);
        $second = ord($bin[1]);
        // fc00::/7 ULA
        if (($first & 0xFE) === 0xFC) {
            return true;
        }
        // fe80::/10 link-local
        if ($first === 0xFE && ($second & 0xC0) === 0x80) {
            return true;
        }
        return false;
    }

    return true; // unparseable → treat as private (fail-safe)
}

/**
 * Resolve the client IP using the configured header.
 *
 * @param string              $headerName $_SERVER key to read.
 * @param array<string,mixed> $server     $_SERVER super-global (or injectable for tests).
 * @return string
 */
function wpmgr_waf_client_ip(string $headerName, array $server): string
{
    $raw = isset($server[$headerName]) ? (string) $server[$headerName] : '';
    if ($raw === '') {
        return '';
    }
    if ($headerName === 'REMOTE_ADDR') {
        return trim($raw);
    }
    // Forwarded header: pick first non-private entry in comma-separated list.
    $candidates = array_map('trim', explode(',', $raw));
    $fallback   = '';
    foreach ($candidates as $candidate) {
        if ($candidate === '') {
            continue;
        }
        if ($fallback === '') {
            $fallback = $candidate;
        }
        if (!wpmgr_waf_is_private($candidate)) {
            return $candidate;
        }
    }
    return $fallback;
}

/**
 * Main WAF gate. Reads config from wp_options via $wpdb and, when protect mode
 * with a matching deny_cidrs entry is detected, sends a 403 and exits.
 *
 * @return void
 */
function wpmgr_waf_gate(): void
{
    // $wpdb is a global set by wp-settings.php before mu-plugins are loaded.
    global $wpdb;
    if (!is_object($wpdb)) {
        return;
    }

    try {
        $optionTable = $wpdb->prefix . 'options';
        // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared
        $raw = $wpdb->get_var(
            $wpdb->prepare(
                "SELECT option_value FROM {$optionTable} WHERE option_name = %s LIMIT 1",
                'wpmgr_security_config'
            )
        );

        if (!is_string($raw) || $raw === '') {
            return; // No config → no gate.
        }

        $config = json_decode($raw, true);
        if (!is_array($config)) {
            return;
        }

        $mode = isset($config['mode']) && is_string($config['mode']) ? $config['mode'] : 'protect';
        if ($mode !== 'protect') {
            return; // Only hard-gate in protect mode.
        }

        $denyCidrs  = isset($config['deny_cidrs']) && is_array($config['deny_cidrs'])
            ? $config['deny_cidrs']
            : [];
        if ($denyCidrs === []) {
            return; // Nothing to deny.
        }

        $allowCidrs = isset($config['allow_cidrs']) && is_array($config['allow_cidrs'])
            ? $config['allow_cidrs']
            : [];

        $ipHeader = isset($config['ip_header']) && is_string($config['ip_header']) && $config['ip_header'] !== ''
            ? strtoupper(trim($config['ip_header']))
            : 'REMOTE_ADDR';

        $ip = wpmgr_waf_client_ip($ipHeader, $_SERVER);
        if ($ip === '') {
            return;
        }

        // SAFETY RAIL: allow_cidrs always wins first.
        if ($allowCidrs !== [] && wpmgr_waf_matches_any_cidr($ip, $allowCidrs)) {
            return;
        }

        // Private/loopback IPs are auto-bypassed (LAN admin can never be locked out).
        if (wpmgr_waf_is_private($ip)) {
            return;
        }

        // If IP matches deny_cidrs → 403 and exit BEFORE WordPress boots.
        if (wpmgr_waf_matches_any_cidr($ip, $denyCidrs)) {
            if (!headers_sent()) {
                http_response_code(403);
                header('Cache-Control: no-cache, no-store, must-revalidate');
                header('Pragma: no-cache');
                header('Expires: 0');
                header('Content-Type: text/plain; charset=utf-8');
            }
            exit('Access denied.');
        }
    } catch (\Throwable $e) {
        // A DB failure or any unexpected error must NEVER block a real request.
        return;
    }
}

// Execute immediately — $wpdb is available at mu-plugin load time.
wpmgr_waf_gate();
