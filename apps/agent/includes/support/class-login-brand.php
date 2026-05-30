<?php
/**
 * LoginBrand: cosmetic login-page branding pushed from the control plane.
 *
 * Stores a logo URL, logo link, and a short message in the wp-option
 * `wpmgr_login_brand` and applies them to wp-login.php via standard WP hooks.
 *
 * Wire contract (wp-option):
 *   wpmgr_login_brand JSON: { "logo_url": string, "logo_link": string, "message": string }
 *   All fields are optional. Empty string = "no override / WP default".
 *   DEFAULT when absent: all empty (stock WP login page — no change).
 *
 * Security posture:
 *   - All three fields are treated as UNTRUSTED input from the control plane.
 *   - Validated on store (applyConfig): URLs checked with esc_url_raw + scheme
 *     in {http, https} only — javascript:, data:, ftp:, etc. are rejected.
 *   - Escaped on output (install hooks): esc_url() for URLs, wp_kses() with a
 *     narrow allowlist for message, esc_url() again inside the <style> block.
 *   - A missing or corrupt option NEVER fatals the login page — every path is
 *     guarded by try/catch and function_exists checks.
 *   - Hooks are bound ONLY when at least one field is non-empty, so a
 *     freshly-installed agent with no brand config adds zero overhead.
 *
 * Allowed HTML for the message (wp_kses allowlist):
 *   a[href,title,target], strong, em, br, p, span
 *   No script, no style, no iframe, no on* attributes.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Reads / writes the login-brand option and registers the login-page hooks.
 */
final class LoginBrand
{
    /** wp-options key. JSON: { "logo_url": string, "logo_link": string, "message": string } */
    public const OPTION = 'wpmgr_login_brand';

    /** Maximum number of characters accepted for the message field. */
    private const MESSAGE_MAX_LEN = 2000;

    /**
     * Allowed HTML tags and attributes for the message field.
     * This list is intentionally narrow — no script, style, iframe, or event
     * attributes. Only inline presentational + link elements are permitted.
     *
     * @var array<string,array<string,bool>>
     */
    private const MESSAGE_KSES_TAGS = [
        'a'      => ['href' => true, 'title' => true, 'target' => true],
        'strong' => [],
        'em'     => [],
        'br'     => [],
        'p'      => [],
        'span'   => [],
    ];

    /**
     * Per-instance cache populated on the first loadConfig() call and
     * invalidated by applyConfig(). Mirrors the ErrorMonitor pattern so that
     * within a single PHP worker request the config is read from wp-options
     * exactly once.
     *
     * @var array{logo_url:string,logo_link:string,message:string}|null
     */
    private ?array $configCache = null;

    // -------------------------------------------------------------------------
    // Config loading
    // -------------------------------------------------------------------------

    /**
     * Load the login-brand config from wp-options. Cached per-instance.
     *
     * Returns safe empty defaults when the option is absent, not valid JSON,
     * or missing expected keys. Never throws — this runs on the login page.
     *
     * @return array{logo_url:string,logo_link:string,message:string}
     */
    public function loadConfig(): array
    {
        if ($this->configCache !== null) {
            return $this->configCache;
        }

        $this->configCache = $this->readOption();
        return $this->configCache;
    }

    /**
     * Read and parse the wp-option. Returns empty defaults on any failure.
     *
     * @return array{logo_url:string,logo_link:string,message:string}
     */
    private function readOption(): array
    {
        $defaults = ['logo_url' => '', 'logo_link' => '', 'message' => ''];

        try {
            if (!function_exists('get_option')) {
                return $defaults;
            }

            $raw = get_option(self::OPTION, null);
            if (!is_string($raw) || $raw === '') {
                return $defaults;
            }

            $decoded = json_decode($raw, true);
            if (!is_array($decoded)) {
                return $defaults;
            }

            return [
                'logo_url'  => isset($decoded['logo_url'])  && is_string($decoded['logo_url'])  ? $decoded['logo_url']  : '',
                'logo_link' => isset($decoded['logo_link']) && is_string($decoded['logo_link']) ? $decoded['logo_link'] : '',
                'message'   => isset($decoded['message'])   && is_string($decoded['message'])   ? $decoded['message']   : '',
            ];
        } catch (\Throwable $e) {
            // Never propagate — we are in the login-page boot path.
            return $defaults;
        }
    }

    // -------------------------------------------------------------------------
    // Hook installation
    // -------------------------------------------------------------------------

    /**
     * Register the wp-login.php hooks when at least one brand field is set.
     *
     * Idempotent (static guard). Called from Plugin::registerHooks() on every
     * boot. When all three fields are empty the method returns immediately and
     * adds zero hooks, keeping stock WP login behaviour intact.
     *
     * Every callback is wrapped in try/catch so a bug here can never fatal the
     * login page.
     *
     * @return void
     */
    public function install(): void
    {
        static $installed = false;
        if ($installed) {
            return;
        }
        $installed = true;

        // Self-gate: add hooks only when there is something to apply.
        $config = $this->loadConfig();
        if ($config['logo_url'] === '' && $config['logo_link'] === '' && $config['message'] === '') {
            return;
        }

        if (!function_exists('add_action') || !function_exists('add_filter')) {
            return;
        }

        // --- logo_head: custom logo via inline <style> -----------------------
        if ($config['logo_url'] !== '') {
            add_action('login_head', function () use ($config): void {
                $this->renderLogoStyle($config['logo_url']);
            });
        }

        // --- login_headerurl: replace the "WordPress" link -------------------
        if ($config['logo_link'] !== '') {
            add_filter('login_headerurl', function ($url) use ($config) {
                return $this->filterHeaderUrl($url, $config['logo_link']);
            });
        }

        // --- login_message: prepend the brand message ------------------------
        if ($config['message'] !== '') {
            add_filter('login_message', function ($message) use ($config) {
                return $this->filterLoginMessage($message, $config['message']);
            });
        }
    }

    /**
     * Emit the login-logo <style> block. Called inside the login_head action.
     *
     * Validates the URL (http/https only, esc_url_raw + parse_url scheme check)
     * before writing anything. If the URL is invalid, outputs nothing — the WP
     * default logo is preserved.
     *
     * @param string $logoUrl Raw logo_url from the stored config.
     * @return void
     */
    private function renderLogoStyle(string $logoUrl): void
    {
        try {
            if (!$this->isValidHttpUrl($logoUrl)) {
                return;
            }

            // Double-escape: esc_url() for the context of a CSS url() value.
            // esc_url() encodes quotes, parentheses, and dangerous characters.
            if (!function_exists('esc_url')) {
                return;
            }
            $safeUrl = esc_url($logoUrl);
            if ($safeUrl === '') {
                return;
            }

            // Output a minimal <style> block. We do NOT use wp_add_inline_style
            // here because the login page loads styles differently depending on
            // the WP version and the login_head hook fires after wp_enqueue is
            // no longer reliable. A direct <style> echo is safe for login_head.
            echo '<style>.login h1 a { background-image: url(\'' . $safeUrl . '\'); background-size: contain; width: auto; }</style>' . "\n";
        } catch (\Throwable $e) {
            // Never fatal the login page.
        }
    }

    /**
     * login_headerurl filter callback. Returns the sanitized logo_link when it
     * is a valid http(s) URL, otherwise returns the WP default unchanged.
     *
     * @param mixed  $url       Current header URL (WordPress default).
     * @param string $logoLink  Raw logo_link from the stored config.
     * @return string
     */
    private function filterHeaderUrl($url, string $logoLink): string
    {
        try {
            if (!$this->isValidHttpUrl($logoLink)) {
                return is_string($url) ? $url : '';
            }
            if (!function_exists('esc_url')) {
                return is_string($url) ? $url : '';
            }
            $safe = esc_url($logoLink);
            return $safe !== '' ? $safe : (is_string($url) ? $url : '');
        } catch (\Throwable $e) {
            return is_string($url) ? $url : '';
        }
    }

    /**
     * login_message filter callback. Prepends the sanitized brand message and
     * preserves any existing message from other plugins.
     *
     * @param mixed  $message   Existing login message from WP or other plugins.
     * @param string $rawMessage Raw message from the stored config.
     * @return string
     */
    private function filterLoginMessage($message, string $rawMessage): string
    {
        try {
            if ($rawMessage === '') {
                return is_string($message) ? $message : '';
            }
            if (!function_exists('wp_kses')) {
                return is_string($message) ? $message : '';
            }

            $safe = wp_kses($rawMessage, self::MESSAGE_KSES_TAGS);
            if ($safe === '') {
                return is_string($message) ? $message : '';
            }

            $existing = is_string($message) ? $message : '';
            return '<div class="wpmgr-login-message">' . $safe . '</div>' . $existing;
        } catch (\Throwable $e) {
            return is_string($message) ? $message : '';
        }
    }

    // -------------------------------------------------------------------------
    // Config persistence
    // -------------------------------------------------------------------------

    /**
     * Validate and persist a CP-pushed brand config. Called by
     * SyncLoginBrandCommand after the JWT has been verified.
     *
     * Validation:
     *   - logo_url  / logo_link: must be empty OR a valid http(s) URL
     *     (esc_url_raw + parse_url scheme). Non-http(s) schemes (javascript:,
     *     data:, ftp:, etc.) are silently coerced to empty string.
     *   - message: truncated to MESSAGE_MAX_LEN, then run through wp_kses with
     *     MESSAGE_KSES_TAGS before storage (so the stored value is already safe
     *     for output, and the output path's wp_kses call is defence-in-depth).
     *
     * Never throws — errors are silently absorbed and reflected in the return
     * value of the command.
     *
     * @param string $logoUrl  Raw logo URL from the CP payload.
     * @param string $logoLink Raw logo link URL from the CP payload.
     * @param string $message  Raw message HTML from the CP payload.
     * @return void
     */
    public function applyConfig(string $logoUrl, string $logoLink, string $message): void
    {
        try {
            // Validate URLs: accept only http/https. Empty = "no override".
            $cleanLogoUrl  = $this->sanitizeUrl($logoUrl);
            $cleanLogoLink = $this->sanitizeUrl($logoLink);

            // Sanitize message: truncate then kses.
            $cleanMessage  = $this->sanitizeMessage($message);

            $encoded = (string) json_encode([
                'logo_url'  => $cleanLogoUrl,
                'logo_link' => $cleanLogoLink,
                'message'   => $cleanMessage,
            ]);

            if (function_exists('update_option')) {
                update_option(self::OPTION, $encoded, false);
            }

            // Invalidate the per-instance cache so subsequent loadConfig() calls
            // in the same request (e.g. unit tests) pick up the new values.
            $this->configCache = null;
        } catch (\Throwable $e) {
            // Never propagate — the command response will still return ok:false
            // if the caller detects a problem, but we must not throw here.
        }
    }

    // -------------------------------------------------------------------------
    // Internal validators
    // -------------------------------------------------------------------------

    /**
     * Sanitize a URL field for storage. Returns the URL as sanitised by
     * esc_url_raw if it passes the http/https scheme check, or empty string
     * otherwise. Never throws.
     *
     * Rejects: javascript:, data:, ftp:, tel:, file:, and everything else that
     * is not explicitly http or https.
     *
     * @param string $url Raw URL string.
     * @return string Sanitized URL or empty string.
     */
    private function sanitizeUrl(string $url): string
    {
        if ($url === '') {
            return '';
        }

        // esc_url_raw removes dangerous characters and normalises the URL.
        if (function_exists('esc_url_raw')) {
            $cleaned = esc_url_raw($url, ['http', 'https']);
        } else {
            // Fallback when WP is not fully loaded (should not happen in practice
            // since we are called from a REST command after WP boot, but defensive).
            $cleaned = filter_var($url, FILTER_SANITIZE_URL);
            if (!is_string($cleaned)) {
                return '';
            }
        }

        if ($cleaned === '') {
            return '';
        }

        // Verify scheme is http or https only.
        if (!$this->isValidHttpUrl($cleaned)) {
            return '';
        }

        return $cleaned;
    }

    /**
     * Sanitize the message field for storage. Truncates to MESSAGE_MAX_LEN and
     * runs wp_kses with the narrow allowlist. Returns an empty string when the
     * result is blank after sanitization. Never throws.
     *
     * @param string $message Raw message HTML.
     * @return string Sanitized message.
     */
    private function sanitizeMessage(string $message): string
    {
        if ($message === '') {
            return '';
        }

        // Hard length cap before kses to avoid processing multi-MB payloads.
        if (strlen($message) > self::MESSAGE_MAX_LEN) {
            $message = substr($message, 0, self::MESSAGE_MAX_LEN);
        }

        if (!function_exists('wp_kses')) {
            // WP not available — strip all tags as a conservative fallback.
            return strip_tags($message);
        }

        return wp_kses($message, self::MESSAGE_KSES_TAGS);
    }

    /**
     * Return true only when $url is an http or https URL.
     *
     * Uses parse_url() to extract the scheme and compares with hash_equals to
     * avoid any timing-oracle concern on the comparison (the string is not
     * secret here, but hash_equals is the house style for all string compares).
     *
     * @param string $url URL to check.
     * @return bool
     */
    private function isValidHttpUrl(string $url): bool
    {
        if ($url === '') {
            return false;
        }

        try {
            $parts  = parse_url($url);
            $scheme = isset($parts['scheme']) && is_string($parts['scheme'])
                ? strtolower($parts['scheme'])
                : '';

            if ($scheme === '') {
                return false;
            }

            return hash_equals('http', $scheme) || hash_equals('https', $scheme);
        } catch (\Throwable $e) {
            return false;
        }
    }
}
