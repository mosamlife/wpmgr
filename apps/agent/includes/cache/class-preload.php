<?php
/**
 * Preload — the cache warmer.
 *
 * Enqueues same-host URLs into WPMgr's own custom-table preload queue
 * (PreloadQueue) and exposes a single-URL warm method that the queue's runner
 * invokes per task. Each URL is warmed for a desktop and (when mobile caching is
 * enabled) a mobile user-agent so both cache buckets are populated; the device
 * fan-out happens at ENQUEUE time (one row per (url, device)).
 *
 * The warm request carries the site's own token in the `x-wpmgr-preload` header
 * (see PreloadToken), by which the serving drop-in recognises a preload: it
 * skips the disk cache and lets WordPress render fresh HTML that the
 * CacheWriter then stores. A 429 / empty / 5xx response THROWS so the queue's
 * retry/backoff machinery defers the URL rather than hammering a struggling
 * origin.
 *
 * Standard WordPress page-cache preloader technique.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

// PerfReporter + PreloadQueue are in the same namespace.

/**
 * Cache warmer: enqueues URLs into the queue table and warms one (url, device)
 * per task.
 */
final class Preload
{
    /** WP cron hook the fallback-drain warmer runs under. */
    public const HOOK = 'wpmgr_cache_preload';

    /**
     * Header that marks a request as a preload. Its value is the site's own
     * token ({@see PreloadToken}); without it the drop-in treats the request
     * as an ordinary visitor request.
     */
    public const PRELOAD_HEADER = 'x-wpmgr-preload';

    /** Desktop UA used when warming. */
    public const DESKTOP_UA = 'Mozilla/5.0 (compatible; WPMgr-Preload/1.0; +https://wpmgr.app)';

    /** Mobile UA used when warming the mobile bucket. */
    public const MOBILE_UA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) WPMgr-Preload/1.0';

    /** Priority for a targeted/single-page preload (higher urgency). */
    public const PRIORITY_TARGETED = 11;

    /** Priority for a full-site enumeration preload. */
    public const PRIORITY_FULLSITE = 20;

    /** Whether to also warm a mobile UA pass. */
    private bool $warmMobile;

    /** Lazily-built queue (shares the same group/callback for all instances). */
    private ?PreloadQueue $queue = null;

    /**
     * @param bool $warmMobile Warm a mobile UA pass in addition to desktop.
     */
    public function __construct(bool $warmMobile = false)
    {
        $this->warmMobile = $warmMobile;
    }

    /**
     * Queue a set of URLs for background warming. SSRF-filters to same-host,
     * de-dups, enqueues a (url, desktop) row (plus a (url, mobile) row when
     * mobile caching is enabled), then starts draining via the loopback runners.
     *
     * SSRF guard (confused-deputy): a `cache_preload` command is signed by the
     * control plane but carries an OPERATOR-supplied `urls[]`. Every URL is
     * filtered to THIS site's own host before it is queued; off-host URLs are
     * dropped (and logged) and never reach the warming HTTP request. The warmer
     * re-validates same-host at WARM time too (defence-in-depth).
     *
     * @param list<string> $urls     URLs to warm.
     * @param int          $priority Ascending-urgency (lower = warmed first).
     * @return int Count of URLs now pending (pending + processing).
     */
    public function queue(array $urls, int $priority = self::PRIORITY_FULLSITE): int
    {
        $urls = $this->filterOnHost(array_values(array_unique(array_filter(
            array_map('strval', $urls),
            static fn (string $u): bool => $u !== ''
        ))));
        if ($urls === []) {
            return 0;
        }

        $queue = $this->getQueue();
        foreach ($urls as $url) {
            $queue->addTask($url, 'desktop', $priority);
            if ($this->warmMobile) {
                $queue->addTask($url, 'mobile', $priority);
            }
        }

        // Persist the total so the dashboard knows the denominator, then start
        // draining via the loopback runners (replaces the old single cron self-
        // reschedule).
        $pending = $queue->pendingCount();
        PerfReporter::persistPreloadTotal($pending);
        $queue->dispatchRunners();

        return $pending;
    }

    /**
     * Fallback cron drain entry (Preload::HOOK). Loopback runners are the primary
     * drain; this keeps a wp-cron path so cron and loopback share the table. Drains
     * via claimNext()/complete()/fail() within a bounded URL budget, defers when
     * the server is overloaded, and re-kicks the loopback runners when work remains.
     *
     * @param int               $max      Maximum URLs to warm this pass.
     * @param PerfReporter|null $reporter Optional reporter (injected for tests).
     * @return int URLs warmed this pass.
     */
    public function run(int $max = 50, ?PerfReporter $reporter = null): int
    {
        $queue = $this->getQueue();

        // Load-gate: defer (warm nothing) while the host is overloaded.
        if ($queue->isServerOverloaded()) {
            return 0;
        }

        $max    = max(1, $max);
        $warmed = 0;
        for ($i = 0; $i < $max; $i++) {
            $task = $queue->claimNext();
            if ($task === null) {
                break;
            }
            $device = (string) ($task['device'] ?? 'desktop');
            $url    = (string) ($task['url'] ?? '');
            try {
                $this->warmOne($url, $device);
                $queue->complete($task);
                $warmed++;
            } catch (\Throwable $e) {
                $queue->fail($task, $e->getMessage());
            }
        }

        $pending = $queue->pendingCount();

        if ($reporter !== null) {
            try {
                $preloadTotal = (int) (function_exists('get_option')
                    ? get_option(PerfReporter::OPTION_PRELOAD_TOTAL, $pending)
                    : $pending);
                $lastPreloadAt = null;
                if ($pending === 0 && $preloadTotal > 0) {
                    PerfReporter::persistLastPreloadAt(time());
                    $lastPreloadAt = time();
                }
                $reporter->reportStats($pending, $preloadTotal, $lastPreloadAt);
            } catch (\Throwable $e) {
                // Fire-and-forget.
            }
        }

        // Hand any remaining work back to the loopback runners.
        if ($pending > 0) {
            $queue->dispatchRunners();
        }

        return $warmed;
    }

    /**
     * Warm a single (url, device): re-runs the same-host SSRF guard, picks the UA
     * for the device, issues ONE warming request, and THROWS on a backoff response
     * (WP_Error / empty / 429 / 5xx) so the queue parks the task for retry. Every
     * other status (2xx/3xx/4xx except 429) is treated as success.
     *
     * @param string $url    URL to warm.
     * @param string $device 'desktop' | 'mobile'.
     * @return void
     * @throws \RuntimeException On a throttle/backoff response.
     */
    public function warmOne(string $url, string $device): void
    {
        // Re-validate same-host at WARM time (defence-in-depth): a row enqueued
        // before a home_url() change MUST be re-checked. Off-host is a silent skip
        // (not a retryable failure) — the row should not endlessly retry.
        if (!$this->isOnHost($url)) {
            $this->logOffHost($url);
            return;
        }

        $userAgent = strtolower(trim($device)) === 'mobile' ? self::MOBILE_UA : self::DESKTOP_UA;
        $this->fetch($url, $userAgent);
    }

    /**
     * Warm a single URL (desktop, then mobile if enabled). Retained for callers
     * that warm both buckets inline; the queue path uses warmOne() per device.
     *
     * @param string $url URL to warm.
     * @return void
     * @throws \RuntimeException On a throttle/backoff response.
     */
    public function warm(string $url): void
    {
        $this->warmOne($url, 'desktop');
        if ($this->warmMobile) {
            $this->warmOne($url, 'mobile');
        }
    }

    /**
     * Pending (pending + processing) count from the queue table. Kept for the
     * PerfReporter denominator and back-compat callers.
     *
     * @return int
     */
    public function pendingCount(): int
    {
        return $this->getQueue()->pendingCount();
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * The shared queue, built from the persisted PerfConfig throttle values.
     *
     * @return PreloadQueue
     */
    private function getQueue(): PreloadQueue
    {
        if ($this->queue === null) {
            $this->queue = PreloadQueue::fromConfig();
        }
        return $this->queue;
    }

    /**
     * Perform the warming HTTP request via wp_remote_get.
     *
     * @param string $url       URL.
     * @param string $userAgent UA to present.
     * @return void
     * @throws \RuntimeException On a WP_Error / empty / 429 / 5xx response.
     */
    private function fetch(string $url, string $userAgent): void
    {
        if (!function_exists('wp_remote_get')) {
            return;
        }

        $response = wp_remote_get($url, [
            'timeout'           => 10,
            'redirection'       => 2,
            'blocking'          => true,
            'sslverify'         => true,
            // Engage WP's SSRF guard (blocks loopback/private-IP/non-standard
            // port) even for on-host URLs — defence-in-depth.
            'reject_unsafe_urls' => true,
            'user-agent'        => $userAgent,
            'headers'           => [self::PRELOAD_HEADER => PreloadToken::get()],
        ]);

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            // Transport failure: treat as a 500 so the queue backs off + retries.
            throw new \RuntimeException('preload transport error');
        }

        $code = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 200;

        // An empty/zero code is an unusable response — treat as 500.
        if ($code === 0) {
            throw new \RuntimeException('preload empty response');
        }

        if ($code === 429 || $code >= 500) {
            throw new \RuntimeException('preload backoff: HTTP ' . esc_html((string) $code));
        }
    }

    /**
     * Drop every URL whose host is not this site's own host. The surviving list
     * is safe to warm; off-host URLs are logged and discarded.
     *
     * @param list<string> $urls Candidate URLs.
     * @return list<string> On-host URLs only.
     */
    private function filterOnHost(array $urls): array
    {
        $kept = [];
        foreach ($urls as $url) {
            if ($this->isOnHost($url)) {
                $kept[] = $url;
            } else {
                $this->logOffHost($url);
            }
        }
        return $kept;
    }

    /**
     * Whether $url's host equals this site's own host (case-insensitive).
     *
     * A URL with no host, a non-http(s) scheme, or a host that differs from the
     * site host is rejected. This is the key SSRF control on the command-driven
     * preload path: it blocks loopback (127.0.0.1), link-local cloud-metadata
     * (169.254.169.254) and any other off-host target an operator could supply.
     *
     * @param string $url Candidate URL.
     * @return bool
     */
    private function isOnHost(string $url): bool
    {
        $siteHost = $this->siteHost();
        if ($siteHost === '') {
            // Cannot resolve our own host — fail closed (warm nothing).
            return false;
        }

        $scheme = strtolower((string) (wp_parse_url($url, PHP_URL_SCHEME) ?? ''));
        if ($scheme !== 'http' && $scheme !== 'https') {
            return false;
        }

        $host = wp_parse_url($url, PHP_URL_HOST);
        if (!is_string($host) || $host === '') {
            return false;
        }

        return hash_equals($siteHost, strtolower($host));
    }

    /**
     * This site's own host (lower-case), derived from home_url() — never from
     * request input.
     *
     * @return string Lower-case host, or '' when unresolvable.
     */
    private function siteHost(): string
    {
        if (!function_exists('home_url')) {
            return '';
        }
        $host = wp_parse_url((string) home_url('/'), PHP_URL_HOST);
        return is_string($host) ? strtolower($host) : '';
    }

    /**
     * Log a dropped off-host preload URL (no secrets; short operational line).
     *
     * @param string $url The rejected URL.
     * @return void
     */
    private function logOffHost(string $url): void
    {
        $host = wp_parse_url($url, PHP_URL_HOST);
        \WPMgr\Agent\Support\DebugLog::write('wpmgr-agent: preload rejected off-host url (host=' . (is_string($host) ? $host : '?') . ')');
    }
}
