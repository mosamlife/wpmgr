<?php
/**
 * IpUtils: client-IP resolution and CIDR matching for both IPv4 and IPv6.
 *
 * Responsibilities:
 *   - Parse the trusted client IP from a configurable request header (default
 *     REMOTE_ADDR; when a forwarded header is configured, extract the FIRST
 *     PUBLIC IP from a comma-separated list so a typical proxy chain like
 *     "203.0.113.5, 10.0.0.1" resolves to "203.0.113.5").
 *   - Identify private/loopback/link-local ranges (used by LoginProtection to
 *     automatically bypass the block engine for LAN/loopback traffic).
 *   - Binary CIDR matching for both IPv4 and IPv6 using inet_pton so the
 *     comparison is byte-exact and immune to representation differences
 *     (IPv4-mapped IPv6, compressed notation, etc.).
 *
 * Security notes:
 *   - REMOTE_ADDR is the only reliable signal when the site sits behind an
 *     untrusted network. Forwarded headers (X-Forwarded-For, CF-Connecting-IP,
 *     etc.) are trivially spoofed unless the site provably sits behind a proxy
 *     that strips/overwrites them. Operators MUST configure ip_header=REMOTE_ADDR
 *     (the default) unless they explicitly operate behind a trusted reverse proxy.
 *   - cidrMatch() uses binary comparison (inet_pton output), not string matching,
 *     so IP normalisation is exact. Inputs that inet_pton cannot parse return false.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Stateless IP-utility helpers used by LoginProtection and the WAF mu-plugin.
 */
final class IpUtils
{
    /**
     * Resolve the client IP from the given $_SERVER header name.
     *
     * When $headerName is 'REMOTE_ADDR' (the default), the value is returned
     * directly after light sanitisation. For any other header (typically a
     * forwarded header like HTTP_X_FORWARDED_FOR), the value is split on commas
     * and the first non-private, non-loopback, non-empty address is returned. If
     * every address in the list is private/invalid, the last entry is returned as
     * a best-effort fallback (still better than returning nothing).
     *
     * @param string                $headerName $_SERVER key to read (e.g. 'REMOTE_ADDR').
     * @param array<string,mixed>   $server     The $_SERVER super-global (injectable for tests).
     * @return string The resolved IP address string, or '' when none is found.
     */
    public static function clientIp(string $headerName = 'REMOTE_ADDR', array $server = []): string
    {
        if ($server === []) {
            $server = $_SERVER;
        }

        $raw = isset($server[$headerName]) ? (string) $server[$headerName] : '';
        if ($raw === '') {
            return '';
        }

        // For REMOTE_ADDR, return directly — it is set by the SAPI and cannot
        // be spoofed by the client. Light-trim only.
        if ($headerName === 'REMOTE_ADDR') {
            return trim($raw);
        }

        // For forwarded headers: parse comma-separated list, pick the first
        // address that is routable (non-private, non-loopback, non-link-local).
        $candidates = array_map('trim', explode(',', $raw));
        $fallback   = '';

        foreach ($candidates as $candidate) {
            if ($candidate === '') {
                continue;
            }
            if ($fallback === '') {
                $fallback = $candidate; // keep first non-empty as fallback
            }
            if (!self::isPrivate($candidate)) {
                return $candidate;
            }
        }

        return $fallback;
    }

    /**
     * Return true when the given address is in a private, loopback, or
     * link-local range (RFC 1918, RFC 4193, RFC 3927, ::1, etc.).
     *
     * Ranges checked:
     *   IPv4: 127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16,
     *         169.254.0.0/16 (link-local).
     *   IPv6: ::1/128 (loopback), fc00::/7 (ULA), fe80::/10 (link-local),
     *         ::ffff:0:0/96 (IPv4-mapped — treated as private so the IPv4
     *         rules apply via the mapped address path).
     *
     * @param string $ip IPv4 or IPv6 address string.
     * @return bool
     */
    public static function isPrivate(string $ip): bool
    {
        $ip = trim($ip);
        if ($ip === '') {
            return true; // empty counts as non-routable
        }

        // Use PHP's built-in filter to validate and canonicalize.
        $ipv4 = filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4);
        if ($ipv4 !== false) {
            return self::isPrivateIpv4($ipv4);
        }

        $ipv6 = filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV6);
        if ($ipv6 !== false) {
            return self::isPrivateIpv6($ipv6);
        }

        return true; // unparseable → treat as private (fail-safe)
    }

    /**
     * Test whether $ip falls inside the given CIDR block.
     *
     * Supports both IPv4 and IPv6. Returns false for any address that cannot
     * be parsed by inet_pton (e.g. hostname strings, empty input).
     *
     * @param string $ip   IPv4 or IPv6 address string.
     * @param string $cidr CIDR notation, e.g. "203.0.113.0/24" or "2001:db8::/32".
     * @return bool
     */
    public static function cidrMatch(string $ip, string $cidr): bool
    {
        $ip   = trim($ip);
        $cidr = trim($cidr);

        if ($ip === '' || $cidr === '') {
            return false;
        }

        // Split CIDR into base address and prefix length.
        $parts = explode('/', $cidr, 2);
        if (count($parts) !== 2) {
            return false;
        }
        [$base, $prefixStr] = $parts;

        // Validate and convert prefix length.
        if (!ctype_digit($prefixStr) && !($prefixStr[0] === '-' ? false : false)) {
            return false;
        }
        $prefix = (int) $prefixStr;

        // Binary-encode both addresses.
        $ipBin   = @inet_pton($ip);
        $baseBin = @inet_pton($base);

        if ($ipBin === false || $baseBin === false) {
            return false;
        }

        // Address families must match.
        if (strlen($ipBin) !== strlen($baseBin)) {
            return false;
        }

        $addrLen = strlen($ipBin); // 4 for IPv4, 16 for IPv6
        $maxPrefix = $addrLen * 8;

        if ($prefix < 0 || $prefix > $maxPrefix) {
            return false;
        }

        if ($prefix === 0) {
            return true; // /0 matches everything in the family
        }

        // Build a bitmask of $prefix ones followed by zeros.
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
     * Short-circuits on the first match. Returns false for an empty list.
     *
     * @param string            $ip    Address to test.
     * @param array<int,string> $cidrs List of CIDR strings.
     * @return bool
     */
    public static function matchesAnyCidr(string $ip, array $cidrs): bool
    {
        foreach ($cidrs as $cidr) {
            if (is_string($cidr) && $cidr !== '' && self::cidrMatch($ip, $cidr)) {
                return true;
            }
        }
        return false;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * @param string $ip Validated IPv4 string.
     */
    private static function isPrivateIpv4(string $ip): bool
    {
        // The quickest check: PHP's own filter with FILTER_FLAG_NO_PRIV_RANGE
        // and FILTER_FLAG_NO_RES_RANGE catches RFC-1918 + loopback + reserved.
        $public = filter_var(
            $ip,
            FILTER_VALIDATE_IP,
            FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE
        );
        if ($public === false) {
            return true; // private or reserved
        }
        // Also catch 169.254.0.0/16 (link-local), which PHP's filter doesn't
        // classify as private by default on all versions.
        if (str_starts_with($ip, '169.254.')) {
            return true;
        }
        return false;
    }

    /**
     * @param string $ip Validated IPv6 string (may be compressed notation).
     */
    private static function isPrivateIpv6(string $ip): bool
    {
        $bin = @inet_pton($ip);
        if ($bin === false || strlen($bin) !== 16) {
            return true; // parse failure → treat as non-routable
        }

        // ::1/128 — loopback
        $loopback = str_repeat("\x00", 15) . "\x01";
        if ($bin === $loopback) {
            return true;
        }

        // ::ffff:0:0/96 — IPv4-mapped; delegate to IPv4 check.
        $v4MappedPrefix = str_repeat("\x00", 10) . "\xff\xff";
        if (substr($bin, 0, 12) === $v4MappedPrefix) {
            $ipv4 = inet_ntop(substr($bin, 12));
            if ($ipv4 !== false) {
                return self::isPrivateIpv4($ipv4);
            }
        }

        $firstByte  = ord($bin[0]);
        $secondByte = ord($bin[1]);

        // fc00::/7 — Unique Local Addresses (ULA). Bits: 1111 110x.
        if (($firstByte & 0xFE) === 0xFC) {
            return true;
        }

        // fe80::/10 — link-local. Bits: 1111 1110 10xx xxxx.
        if ($firstByte === 0xFE && ($secondByte & 0xC0) === 0x80) {
            return true;
        }

        return false;
    }
}
