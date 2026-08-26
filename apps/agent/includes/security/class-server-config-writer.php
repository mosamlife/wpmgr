<?php
/**
 * ServerConfigWriter — writes / removes the WPMgr Security managed blocks in
 * the site-root .htaccess (Apache / LiteSpeed) and generates a display-only
 * nginx snippet.
 *
 * Block structure mirrors the HtaccessManager pattern in the cache suite:
 * delimited by exact BEGIN/END markers so blocks are cleanly replaceable and
 * never duplicated.
 *
 * Apache is the only server type where we auto-write; nginx sites need operator
 * action (the rules are shown in the dashboard as a snippet). The writer is
 * completely idempotent: re-running with the same config is a no-op.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

/**
 * Manages the "# BEGIN WPMgr Security" / "# END WPMgr Security" block in
 * the site-root .htaccess.
 */
final class ServerConfigWriter
{
    /** Opening marker — byte-exact for idempotency. */
    public const BEGIN = '# BEGIN WPMgr Security';

    /** Closing marker — byte-exact for idempotency. */
    public const END = '# END WPMgr Security';

    /**
     * Maximum number of IP/range ban rules written to server config.
     *
     * Overflow entries are still enforced at the PHP (mu-plugin) layer; we cap
     * here to keep .htaccess from growing unbounded on sites with large ban lists.
     */
    private const MAX_SERVER_BAN_RULES = 200;

    /**
     * Install / refresh the managed block. Returns false when the site is on
     * nginx (no .htaccess auto-write), when the path is unresolvable, or when
     * the file is not writable. Returns true when the block is in place (even
     * if we skipped the write because content is already current).
     *
     * @param HardeningConfig $config Validated hardening config.
     * @return bool Whether the block is (or was already) in place.
     */
    public function install(HardeningConfig $config): bool
    {
        if ($this->isNginx()) {
            return false;
        }

        $path = $this->htaccessPath();
        if ($path === '') {
            return false;
        }

        $existing = '';
        if (@is_file($path) && @is_readable($path)) {
            $read = @file_get_contents($path);
            if ($read !== false) {
                $existing = $read;
            }
        }

        $updated = $this->renderInto($existing, $config);
        if ($updated === $existing) {
            return true; // already current
        }

        if (!@is_writable($path) && !@is_writable(dirname($path))) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            return false;
        }

        $result = @file_put_contents($path, $updated, LOCK_EX);
        return $result !== false;
    }

    /**
     * Remove the managed security block. Idempotent (absent => no-op true).
     *
     * @return bool
     */
    public function uninstall(): bool
    {
        $path = $this->htaccessPath();
        if ($path === '' || !@is_file($path)) {
            return true;
        }

        $content = @file_get_contents($path);
        if ($content === false) {
            return true;
        }

        $stripped = $this->stripBlock($content);
        if ($stripped === $content) {
            return true; // not present
        }

        if (!@is_writable($path) && !@is_writable(dirname($path))) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            return false;
        }

        $result = @file_put_contents($path, $stripped, LOCK_EX);
        return $result !== false;
    }

    /**
     * Whether the site is using nginx (no .htaccess auto-write possible).
     *
     * @return bool
     */
    public function isNginx(): bool
    {
        if (!isset($_SERVER['SERVER_SOFTWARE']) || !is_string($_SERVER['SERVER_SOFTWARE'])) {
            return false;
        }
        return str_contains(
            strtolower(sanitize_text_field(wp_unslash($_SERVER['SERVER_SOFTWARE']))), // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized via sanitize_text_field(wp_unslash())
            'nginx'
        );
    }

    /**
     * Pure transform: render the block into $existing. Used by tests without
     * disk access.
     *
     * @param string          $existing Current .htaccess content.
     * @param HardeningConfig $config   Validated config.
     * @return string
     */
    public function renderInto(string $existing, HardeningConfig $config): string
    {
        $body = $this->blockBody($config);

        if ($body === '') {
            // Nothing to write — strip any existing block and return.
            return $this->stripBlock($existing);
        }

        $block = self::BEGIN . "\n" . $body . "\n" . self::END;
        return $this->spliceBlock($existing, $block);
    }

    // -------------------------------------------------------------------------
    // Private block-body builder
    // -------------------------------------------------------------------------

    /**
     * Build the directive body (everything between the markers). Returns '' when
     * no toggles are active and there are no bans, so the block is cleanly
     * removed when all features are disabled.
     *
     * @param HardeningConfig $config
     * @return string
     */
    private function blockBody(HardeningConfig $config): string
    {
        $lines = [];

        // SSL redirect (Apache) — redirect http:// -> https:// + HSTS header.
        if ($config->forceSsl) {
            $lines[] = '<IfModule mod_rewrite.c>';
            $lines[] = '    RewriteEngine On';
            $lines[] = '    RewriteCond %{HTTPS} off';
            $lines[] = '    RewriteRule ^ https://%{HTTP_HOST}%{REQUEST_URI} [L,R=301]';
            $lines[] = '</IfModule>';
            $lines[] = '<IfModule mod_headers.c>';
            $lines[] = '    Header always set Strict-Transport-Security "max-age=31536000; includeSubDomains"';
            $lines[] = '</IfModule>';
        }

        // Directory browsing off.
        if ($config->disableDirectoryBrowsing) {
            $lines[] = 'Options -Indexes';
        }

        // Block PHP execution in uploads directory.
        if ($config->disablePhpInUploads) {
            $lines[] = '<IfModule mod_rewrite.c>';
            $lines[] = '    RewriteEngine On';
            $lines[] = '    RewriteCond %{REQUEST_URI} /wp-content/uploads/ [NC]';
            $lines[] = '    RewriteRule \.php$ - [F,L]';
            $lines[] = '</IfModule>';
        }

        // Protect system files.
        if ($config->protectSystemFiles) {
            $lines[] = '<FilesMatch "^(wp-config\.php|\.htaccess|\.htpasswd|readme\.html|readme\.txt|license\.txt|debug\.log)$">';
            $lines[] = '    Require all denied';
            $lines[] = '    Order deny,allow';
            $lines[] = '    Deny from all';
            $lines[] = '</FilesMatch>';
        }

        // XML-RPC off at server level.
        if ($config->xmlrpcMode === HardeningConfig::XMLRPC_OFF) {
            $lines[] = '<Files "xmlrpc.php">';
            $lines[] = '    Require all denied';
            $lines[] = '    Order deny,allow';
            $lines[] = '    Deny from all';
            $lines[] = '</Files>';
        }

        // IP/range bans (capped to avoid unbounded .htaccess growth).
        // BLOCKER 3: before emitting any deny rule, filter out CIDRs that would
        // lock out the operator or control plane:
        //   (a) All-address CIDRs (0.0.0.0/0, ::/0, IPv4 prefix < /8, IPv6 prefix < /16)
        //       are dropped entirely — they would deny every request including the
        //       signed command that would undo the ban.
        //   (b) CIDRs that overlap private/loopback ranges are skipped at render time
        //       (WAF mu-plugin also enforces this, belt-and-braces here).
        //   (c) The configured allow_cidrs are read from wpmgr_security_config so
        //       that a ban can never override an operator-level exemption.
        // Safety filtering is delegated to WafCidrGuard so the same rules apply
        // here and in HardeningModule::syncWafDenyCidrs() (single source of truth).
        $rawIpBans  = array_slice($config->ipRangeBans(), 0, self::MAX_SERVER_BAN_RULES);
        $allowCidrs = $this->readAllowCidrs();
        $safeDenies = [];
        foreach ($rawIpBans as $cidr) {
            $cidr = trim($cidr);
            if (WafCidrGuard::isUnsafe($cidr, $allowCidrs)) {
                continue;
            }
            // Belt-and-braces BLOCKER 2: skip values containing control chars.
            if (preg_match('/[\x00-\x1F\x7F]/', $cidr) === 1) {
                continue;
            }
            $safeDenies[] = $cidr;
        }

        if ($safeDenies !== []) {
            $lines[] = '<IfModule mod_authz_core.c>';
            $lines[] = '    <RequireAll>';
            $lines[] = '        Require all granted';
            foreach ($safeDenies as $cidr) {
                $lines[] = '        Require not ip ' . $cidr;
            }
            $lines[] = '    </RequireAll>';
            $lines[] = '</IfModule>';
            // Legacy Apache 2.2 fallback.
            $lines[] = '<IfModule !mod_authz_core.c>';
            $lines[] = '    Order allow,deny';
            $lines[] = '    Allow from all';
            foreach ($safeDenies as $cidr) {
                $lines[] = '    Deny from ' . $cidr;
            }
            $lines[] = '</IfModule>';
        }

        // User-agent bans.
        // BLOCKER 1 FIX: emit one positive RewriteCond per ban pattern, OR-combined,
        // with the LAST condition NOT carrying [OR]. The RewriteRule ^ - [F,L]
        // then fires only when the UA MATCHES at least one banned pattern.
        //
        // The old negated form (!pattern [NC] AND-chained) was inverted: Apache
        // AND-combines all RewriteConds, so the rule fired when the UA matched
        // NONE of the ban patterns — blocking every legitimate visitor and letting
        // the banned bots through. This is the corrected, positive-match OR-chain.
        $uaBans = $config->userAgentBans();
        if ($uaBans !== []) {
            // Pre-filter: drop empty values and values with control chars
            // (belt-and-braces BLOCKER 2 guard at the render layer).
            $safeUaBans = [];
            foreach ($uaBans as $ua) {
                $ua = trim($ua);
                if ($ua !== '' && preg_match('/[\x00-\x1F\x7F]/', $ua) !== 1) {
                    $safeUaBans[] = $ua;
                }
            }

            if ($safeUaBans !== []) {
                $lastIdx = count($safeUaBans) - 1;
                $lines[] = '<IfModule mod_rewrite.c>';
                $lines[] = '    RewriteEngine On';
                // GH #529: exempt the recovery surfaces BEFORE the ban fires.
                // Apache answers before PHP loads, so HardeningModule's
                // isRecoverySurface() can never run for a request this block
                // rejects — the exemption has to exist here too or the .htaccess
                // layer locks the owner out of the door the PHP layer opens.
                foreach ($this->recoveryExemptionConds() as $cond) {
                    $lines[] = '    ' . $cond;
                }
                foreach ($safeUaBans as $idx => $ua) {
                    $escaped = preg_quote($ua, '!');
                    // Positive match per pattern. [OR] on every condition except the
                    // last so Apache OR-combines them: the block fires when ANY single
                    // pattern matches (banned UA detected).
                    $flag    = ($idx < $lastIdx) ? ' [NC,OR]' : ' [NC]';
                    $lines[] = '    RewriteCond %{HTTP_USER_AGENT} ' . $escaped . $flag;
                }
                $lines[] = '    RewriteRule ^ - [F,L]';
                $lines[] = '</IfModule>';
            }
        }

        if ($lines === []) {
            return '';
        }

        return implode("\n", $lines);
    }

    /**
     * The AND-combined RewriteCond lines that keep the user-agent ban off the
     * two surfaces an administrator uses to get back into their own site
     * (GH #529). Mirrors HardeningModule::isRecoverySurface() exactly: the login
     * screen, and the agent's own autologin route in both permalink shapes.
     *
     * WHY THIS HAS TO EXIST HERE AND NOT ONLY IN PHP. .htaccess is the primary
     * enforcement point on Apache and LiteSpeed, and `RewriteRule ^ - [F,L]`
     * matches every path including wp-login.php. Apache answers that 403 before
     * wp-settings.php runs, so the PHP-layer exemption is unreachable for
     * exactly the request it exists to allow. A recovery exemption that only
     * exists in PHP is not a recovery exemption on the servers that matter.
     *
     * HOW APACHE GROUPS THESE. [OR] binds tighter than the implicit AND, so a
     * condition carrying [OR] is OR-combined with the one after it and the
     * resulting groups are AND-combined. The four lines below therefore read:
     *
     *   NOT login
     *     AND NOT autologin-path
     *     AND ( NOT site-root OR NOT rest_route=autologin )
     *
     * The third group is De Morgan applied deliberately: what it means is
     * NOT (site-root AND rest_route=autologin), i.e. the plain-permalink form
     * /?rest_route=/wpmgr/v1/autologin is exempt only when it is actually
     * addressed to the site root, never when the query is bolted onto an
     * arbitrary path. Two AND-combined negations could not express that; a pair
     * of negations inside one OR-group can.
     *
     * The ban's own OR-chain follows and forms its own group, so the emitted
     * rule fires on (recovery-exempt = false) AND (any banned UA matched).
     *
     * ANCHORING. Every path pattern is anchored at ^ against %{REQUEST_URI},
     * which in per-directory context is the full URL path including any
     * subdirectory prefix — so the site's own home path is prepended and
     * regex-escaped. `/xmlrpc.php/wp-json/wpmgr/v1/autologin` does not match a
     * pattern anchored at the start, which is the same smuggling class the PHP
     * layer refuses.
     *
     * @return array<int,string> RewriteCond lines, without indentation.
     */
    private function recoveryExemptionConds(): array
    {
        $home  = preg_quote($this->homePath(), '#');
        $route = HardeningModule::AGENT_REST_NAMESPACE . HardeningModule::AGENT_AUTOLOGIN_PATH;

        $restPrefix = 'wp-json';
        if (function_exists('rest_get_url_prefix')) {
            $candidate = rest_get_url_prefix();
            if (is_string($candidate) && $candidate !== '') {
                $restPrefix = $candidate;
            }
        }

        $autologinPath = preg_quote('/' . trim($restPrefix, '/') . '/' . $route, '#');
        $restRouteArg  = preg_quote('rest_route=/' . $route, '#');

        return [
            // 1. The login screen. ($|/) mirrors core's $pagenow regex, which
            //    absorbs PATH_INFO, so /wp-login.php/foo is the login screen to
            //    the PHP layer and must be the login screen to this one.
            'RewriteCond %{REQUEST_URI} !^' . $home . '/wp-login\.php($|/) [NC]',
            // 2. Pretty-permalink autologin, exact path.
            'RewriteCond %{REQUEST_URI} !^' . $home . $autologinPath . '/?$ [NC]',
            // 3+4. Plain-permalink autologin: site root AND the rest_route arg.
            'RewriteCond %{REQUEST_URI} !^' . $home . '/(index\.php)?$ [NC,OR]',
            'RewriteCond %{QUERY_STRING} !(^|&)' . $restRouteArg . '/?($|&) [NC]',
        ];
    }

    /**
     * The site's home URL path, with any trailing slash removed — '' for a root
     * install, '/blog' for a subdirectory one.
     *
     * %{REQUEST_URI} carries the full request path in per-directory context, so
     * every anchored pattern in {@see recoveryExemptionConds()} needs this
     * prefix or it silently stops matching on subdirectory installs.
     *
     * Returns '' when WordPress is not loaded (the pure-transform test path).
     * '' is the correct root-install prefix here, not a missing-value fallback:
     * these strings are regex anchors for an exemption, never a filesystem path.
     *
     * @return string
     */
    private function homePath(): string
    {
        if (!function_exists('home_url') || !function_exists('wp_parse_url')) {
            return '';
        }
        $path = wp_parse_url(home_url('/'), PHP_URL_PATH);
        if (!is_string($path)) {
            return '';
        }
        return rtrim($path, '/');
    }

    // -------------------------------------------------------------------------
    // Block splice helpers (mirror HtaccessManager pattern)
    // -------------------------------------------------------------------------

    /**
     * Strip the managed security block (BEGIN..END inclusive).
     *
     * @param string $content .htaccess content.
     * @return string
     */
    private function stripBlock(string $content): string
    {
        $pattern = '/' . preg_quote(self::BEGIN, '/') . '.*?' . preg_quote(self::END, '/') . "\n?/s";
        $result  = preg_replace($pattern, '', $content);
        return $result ?? $content;
    }

    /**
     * Splice the fresh block BEFORE the WordPress block (if present) or prepend
     * it to the existing content. Strips any prior copy first.
     *
     * @param string $content Existing .htaccess content.
     * @param string $block   Fully-formed BEGIN...END block.
     * @return string
     */
    private function spliceBlock(string $content, string $block): string
    {
        $stripped = $this->stripBlock($content);
        if (trim($stripped) === '') {
            return $block . "\n";
        }
        return $block . "\n\n" . ltrim($stripped, "\n");
    }

    /**
     * Resolve the site-root .htaccess path.
     *
     * @return string Absolute path, or '' when unresolvable.
     */
    private function htaccessPath(): string
    {
        $root = '';
        if (function_exists('get_home_path')) {
            $candidate = get_home_path();
            if (is_string($candidate) && $candidate !== '') {
                $root = $candidate;
            }
        }
        if ($root === '' && defined('ABSPATH')) {
            $root = (string) constant('ABSPATH');
        }
        if ($root === '') {
            return '';
        }
        return rtrim($root, '/\\') . DIRECTORY_SEPARATOR . '.htaccess';
    }

    /**
     * Read the operator-configured allow_cidrs from wpmgr_security_config.
     * Used at render time to exempt allow-listed CIDRs from deny rules.
     *
     * Returns an empty array when the option is absent or unreadable.
     *
     * @return array<int,string>
     */
    private function readAllowCidrs(): array
    {
        if (!function_exists('get_option')) {
            return [];
        }
        $raw = get_option('wpmgr_security_config', '');
        if (!is_string($raw) || $raw === '') {
            return [];
        }
        $cfg = json_decode($raw, true);
        if (!is_array($cfg)) {
            return [];
        }
        $allow = isset($cfg['allow_cidrs']) && is_array($cfg['allow_cidrs']) ? $cfg['allow_cidrs'] : [];
        return array_values(array_filter($allow, 'is_string'));
    }
}
