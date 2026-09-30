<?php
/**
 * Varnish — purge the local Varnish reverse cache over loopback.
 *
 * Many managed stacks (and self-managed VPSes) run Varnish in front of PHP.
 * Varnish has no shared API, so the universally-supported technique is to send
 * an HTTP request with a custom method to the daemon and have its VCL turn that
 * into a purge:
 *   - per-URL: `PURGE` the exact path,
 *   - everything: `BAN` (regex-wide) — with `URLPURGE` as the request method some
 *     VCLs use for the same intent.
 *
 * We talk ONLY to loopback (127.0.0.1, host/port configurable) and set the
 * Host header to the site host so the VCL keys the right object. The loopback
 * target is why we intentionally pass reject_unsafe_urls=false — this is an
 * internal, operator-trusted address, never an attacker-influenced URL.
 *
 * Detection: presence of the Varnish marker headers on the inbound request
 * (X-Varnish / Via: varnish) — set by Varnish itself, the host's own signal.
 *
 * Standard Varnish PURGE/BAN-over-HTTP idiom.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * Loopback Varnish purger.
 */
final class Varnish extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'varnish';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Shared: how far a purge reaches is set by the server\'s own configuration.';

    /** Hard cap on per-call URL purges (loopback flood guard). */
    private const MAX_URLS = 200;

    /**
     * Detect Varnish from the request markers it injects.
     *
     * @return bool
     */
    protected function detect(): bool
    {
        if (isset($_SERVER['HTTP_X_VARNISH'])) {
            // A `varnishpass` application marker means Varnish is bypassing the
            // request, so there is nothing cached to purge.
            $app = isset($_SERVER['HTTP_X_APPLICATION'])
                ? strtolower( sanitize_text_field( wp_unslash( (string) $_SERVER['HTTP_X_APPLICATION'] ) ) )
                : '';
            return $app !== 'varnishpass';
        }
        $via = isset($_SERVER['HTTP_VIA'])
            ? strtolower( sanitize_text_field( wp_unslash( (string) $_SERVER['HTTP_VIA'] ) ) )
            : '';
        return str_contains($via, 'varnish');
    }

    /**
     * BAN the whole site from the local Varnish.
     *
     * @return void
     */
    protected function purgeAll(): void
    {
        $this->request('BAN', '/.*', ['X-Ban-Host' => $this->host(), 'X-Ban-Url' => '/.*']);
    }

    /**
     * PURGE each URL's path from the local Varnish.
     *
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        $count = 0;
        foreach ($urls as $url) {
            if ($count++ >= self::MAX_URLS) {
                break;
            }
            $path = (string) (wp_parse_url($url, PHP_URL_PATH) ?? '/');
            if ($path === '') {
                $path = '/';
            }
            $this->request('PURGE', $path);
        }
    }

    /**
     * Fire a single loopback request against Varnish.
     *
     * @param string                $method  HTTP method (PURGE / BAN).
     * @param string                $path    Request path.
     * @param array<string,string>  $extra   Extra request headers.
     * @return void
     */
    private function request(string $method, string $path, array $extra = []): void
    {
        if (!function_exists('wp_remote_request')) {
            return;
        }

        [$host, $port] = $this->endpoint();
        $url = 'http://' . $host . ($port !== 80 ? ':' . $port : '') . $path;

        \wp_remote_request($url, [
            'method'            => $method,
            'timeout'           => 2,
            'blocking'          => false,
            'sslverify'         => false,
            // Intentional: the target is loopback, not a user-controlled URL.
            'reject_unsafe_urls' => false,
            'headers'           => array_merge(['Host' => $this->host()], $extra),
        ]);
    }

    /**
     * Resolve the configurable loopback endpoint (host, port).
     *
     * @return array{0:string,1:int}
     */
    private function endpoint(): array
    {
        $host = defined('WPMGR_VARNISH_HOST') ? (string) \WPMGR_VARNISH_HOST : '127.0.0.1';
        $port = defined('WPMGR_VARNISH_PORT') ? (int) \WPMGR_VARNISH_PORT : 80;
        if ($host === '') {
            $host = '127.0.0.1';
        }
        if ($port <= 0 || $port > 65535) {
            $port = 80;
        }
        return [$host, $port];
    }

    /**
     * The site host the VCL keys cached objects by.
     *
     * @return string
     */
    private function host(): string
    {
        if (isset($_SERVER['HTTP_HOST']) && is_string($_SERVER['HTTP_HOST']) && $_SERVER['HTTP_HOST'] !== '') {
            return sanitize_text_field( wp_unslash( (string) $_SERVER['HTTP_HOST'] ) );
        }
        if (isset($_SERVER['SERVER_NAME']) && is_string($_SERVER['SERVER_NAME'])) {
            return sanitize_text_field( wp_unslash( (string) $_SERVER['SERVER_NAME'] ) );
        }
        if (function_exists('home_url')) {
            return (string) (wp_parse_url((string) \home_url('/'), PHP_URL_HOST) ?? '');
        }
        return '';
    }
}
