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
 * Emit a 403 and exit. Centralised so all gate paths use identical headers.
 *
 * @return never
 */
function wpmgr_waf_deny(): void
{
    if (!headers_sent()) {
        http_response_code(403);
        header('Cache-Control: no-cache, no-store, must-revalidate');
        header('Pragma: no-cache');
        header('Expires: 0');
        header('Content-Type: text/plain; charset=utf-8');
    }
    exit('Access denied.');
}

/**
 * Pure gate-decision function — no side effects, no exit().
 *
 * Evaluates the two-layer deny logic against a pre-resolved config array and
 * a pre-resolved client IP string. Returns true when the gate WOULD deny the
 * request; false when it would pass.
 *
 * Layer order (must be preserved exactly — tests assert on this order):
 *   (1) allow_cidrs match           → no-deny (always wins)
 *   (2) private/loopback IP         → no-deny (operator lock-out guard)
 *   (3) hardening_deny_cidrs        → deny    (all modes, mode-independent)
 *   (4) mode != protect             → no-deny (deny_cidrs gate is mode-gated)
 *   (5) deny_cidrs in protect mode  → deny
 *
 * THERE IS NO WPMGR_DISABLE_SITE_2FA LAYER HERE, AND ITS ABSENCE IS DELIBERATE
 * (GH #529). A recovery layer was added to this function and has been removed
 * again; do not re-add it without reading this paragraph.
 *
 * Both lists this function enforces are OPERATOR POLICY:
 *   - hardening_deny_cidrs is written by HardeningModule::syncWafDenyCidrs()
 *     from HardeningConfig::ipRangeBans().
 *   - deny_cidrs arrives from the control plane through
 *     SyncSecurityConfigCommand, whose wire contract calls them "always-block
 *     ranges". Nothing in this plugin ever writes to it.
 *
 * Neither is generated by this plugin, so neither is a self-inflicted lockout
 * and neither is what a recovery constant is for. The one surface this plugin
 * closes on an administrator by itself is the sliding-window event count in
 * {prefix}wpmgr_login_events, and that is enforced in LoginProtection, not here
 * — this file has no database access to those rows at all. THE RECOVERY PATH
 * FOR THE EVENT-BACKED LOCKOUT THEREFORE LIVES ENTIRELY IN
 * LoginProtection::onAuthenticate() STEP 6b, WHICH IS THE ONLY PLACE THAT CAN
 * IMPLEMENT IT.
 *
 * The earlier layer was added on the belief that deny_cidrs was auto-populated
 * by the brute-force protection. It is not. While that layer existed, setting
 * the constant silently disabled the operator's control-plane always-block
 * ranges on every request before WordPress booted — and on a managed site the
 * party who can edit wp-config.php is frequently NOT the operator (an agency's
 * client, a host's tenant), so it let the managed party override the manager.
 * A constant in wp-config.php proves local file access, never operator
 * authority.
 *
 * Note also that moving such a layer BELOW (5) instead of deleting it would be
 * dead code: every path after (5) already returns false. A gate that reads like
 * a live escape hatch and decides nothing is worse than no gate, because the
 * next reader trusts it.
 *
 * Extracting the decision into this pure function means:
 *   - Tests `require_once` this file and call wpmgr_waf_should_deny() directly,
 *     so any reordering of the layers above breaks the tests immediately.
 *   - wpmgr_waf_gate() remains the only caller that executes the exit path.
 *
 * @param array<string,mixed> $config Decoded wpmgr_security_config array.
 * @param string              $ip     Pre-resolved client IP (already sanitised).
 * @param string              $mode   Login-protection mode from $config.
 * @return bool True → should deny; false → should pass.
 */
function wpmgr_waf_should_deny(array $config, string $ip, string $mode): bool
{
    if ($ip === '') {
        return false;
    }

    $denyCidrs = isset($config['deny_cidrs']) && is_array($config['deny_cidrs'])
        ? $config['deny_cidrs']
        : [];

    $allowCidrs = isset($config['allow_cidrs']) && is_array($config['allow_cidrs'])
        ? $config['allow_cidrs']
        : [];

    $hardeningCidrs = isset($config['hardening_deny_cidrs']) && is_array($config['hardening_deny_cidrs'])
        ? $config['hardening_deny_cidrs']
        : [];

    // Fast path: nothing to deny in any list.
    if ($denyCidrs === [] && $hardeningCidrs === []) {
        return false;
    }

    // (1) allow_cidrs always wins — checked once, applies to ALL paths.
    if ($allowCidrs !== [] && wpmgr_waf_matches_any_cidr($ip, $allowCidrs)) {
        return false;
    }

    // (2) Private/loopback IPs are auto-bypassed in ALL paths.
    if (wpmgr_waf_is_private($ip)) {
        return false;
    }

    // (3) Operator hardening bans — enforced in ALL modes, mode-independent.
    // ITEM 5 FIX: explicit operator bans must block regardless of login-protection
    // mode. Stored under 'hardening_deny_cidrs' so they evaluate independently.
    if ($hardeningCidrs !== [] && wpmgr_waf_matches_any_cidr($ip, $hardeningCidrs)) {
        return true;
    }

    // NOTE: there is deliberately NO recovery-constant layer here — see the
    // layer-order note above for why WPMGR_DISABLE_SITE_2FA has no role in this
    // gate. Both lists this function enforces are operator policy, and the
    // constant's recovery path lives in LoginProtection, post-boot.

    // (4) Brute-force protect gate — only active when mode == protect.
    if ($mode !== 'protect') {
        return false;
    }

    if ($denyCidrs === []) {
        return false;
    }

    // (5) deny_cidrs in protect mode.
    if (wpmgr_waf_matches_any_cidr($ip, $denyCidrs)) {
        return true;
    }

    return false;
}

/**
 * Main WAF gate. Reads config from wp_options via $wpdb and applies two
 * independent enforcement layers:
 *
 *   1. Operator hardening bans (deny_cidrs sourced from the hardening config via
 *      syncWafDenyCidrs) — enforced in ALL modes, regardless of login-protection
 *      mode. An explicit operator ban must always block (ITEM 5 FIX).
 *
 *   2. Brute-force protect gate (mode == "protect") — the original behaviour,
 *      unchanged. Only active when the operator has explicitly enabled protect
 *      mode for login protection.
 *
 * Safety invariants preserved in both paths:
 *   - allow_cidrs always wins (checked before any deny).
 *   - Private/loopback IPs are auto-bypassed.
 *   - A DB failure or any exception NEVER blocks a real request (try/catch).
 *
 * The actual deny decision is delegated to wpmgr_waf_should_deny() so that
 * tests can exercise the real logic without triggering exit().
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
        $raw = $wpdb->get_var( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,PluginCheck.Security.DirectDB.UnescapedDBParameter -- mu-plugin pre-boot read of options table; no WP caching available yet; value is the output of $wpdb->prepare()
            $wpdb->prepare(
                // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is $wpdb->prefix + 'options' (trusted core table); value bound via %s placeholder
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

        $ipHeader = isset($config['ip_header']) && is_string($config['ip_header']) && $config['ip_header'] !== ''
            ? strtoupper(trim($config['ip_header']))
            : 'REMOTE_ADDR';

        $ip = wpmgr_waf_client_ip($ipHeader, $_SERVER);

        if (wpmgr_waf_should_deny($config, $ip, $mode)) {
            wpmgr_waf_deny();
        }
    } catch (\Throwable $e) {
        // A DB failure or any unexpected error must NEVER block a real request.
        return;
    }
}

// Execute immediately — $wpdb is available at mu-plugin load time.
// WPMGR_WAF_TESTING: when this constant is defined, the file is being included
// by the test suite (require_once to load function definitions only). In that
// case, skip the top-level wpmgr_waf_gate() call — $wpdb is not available in
// the test environment and the gate would throw/exit anyway.
if (!defined('WPMGR_WAF_TESTING')) {
    wpmgr_waf_gate();
}
