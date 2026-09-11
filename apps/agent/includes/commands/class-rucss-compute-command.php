<?php
/**
 * RucssComputeCommand — triggers on-demand RUCSS (Remove Unused CSS) computation
 * for a list of URLs by issuing same-host self-fetch requests that force a fresh
 * cache MISS render. The existing request-path Optimizer's RUCSS stage intercepts
 * the render and posts the page HTML to the CP's /agent/v1/rucss endpoint, which
 * enqueues a compute job on the CP worker.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/rucss_compute
 *   Authorization: Bearer <Ed25519 JWT, cmd="rucss_compute", aud=<siteId>>
 *   Body: { "urls": string[]? }  // empty ⇒ home_url('/')
 *
 * Response: { "ok": <bool>, "detail": "<text>", "queued": <int> }
 *
 * SSRF guard: only same-host URLs are fetched. Any URL whose host differs from
 * home_url() is rejected silently (counted in the detail). Reuses the same SSRF
 * guard pattern that CachePreload uses.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\Preload;
use WPMgr\Agent\Optimizer\PerfConfig;

/**
 * Triggers on-demand RUCSS computation for given URLs.
 */
final class RucssComputeCommand implements CommandInterface
{
    /** Hard cap on URLs accepted in one push. */
    private const MAX_URLS = 50;

    /** Header that marks a request as a preload (skips disk cache). */
    private const RUCSS_HEADER = 'x-wpmgr-rucss-compute';

    /**
     * Header marking a CP-initiated post-compute re-warm self-fetch. The optimizer
     * forwards it (as meta.reheat) to the CP so a re-miss (structure_hash drift)
     * does not trigger yet another reheat — the loop terminates.
     */
    private const REHEAT_HEADER = 'x-wpmgr-rucss-reheat';

    /** Desktop self-fetch UA (non-mobile → desktop cache bucket). */
    private const DESKTOP_UA = 'Mozilla/5.0 (compatible; WPMgr-RUCSS/1.0; +https://wpmgr.app)';

    /** Page-cache orchestrator, used to purge a URL before its self-fetch. */
    private ?CacheManager $cache;

    /**
     * @param CacheManager|null $cache Page-cache orchestrator. When provided, the
     *        target URL's cached page is deleted immediately before the self-fetch
     *        so the request falls through to PHP. This is required because the
     *        request-header cache bypass (x-wpmgr-preload) is only honoured by the
     *        PHP drop-in — the nginx/Apache static fast-path serves the on-disk
     *        .html.gz BEFORE PHP boots, so without a real purge the optimizer's
     *        RUCSS stage never runs and no bundle is posted to the CP.
     */
    public function __construct(?CacheManager $cache = null)
    {
        $this->cache = $cache;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'rucss_compute';
    }

    /**
     * Effect: purges each target URL's cache entry, then forces a fresh self-fetch render so the optimizer's
     * RUCSS stage runs, and is a Write -- driving every subsequent visitor to a freshly computed,
     * RUCSS-optimized page is the whole reason this command exists. Only derived cache state changes, and the
     * site would rebuild it on the next real visit regardless; that does not decide the label -- purpose does.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: a second call re-queues the same URLs.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Repeatable;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params { urls?: string[] }.
     * @return array{ok:bool,detail:string,queued:int}
     */
    public function execute(array $claims, array $params): array
    {
        // Gate: RUCSS must be enabled for the fetch to do anything meaningful.
        $config = PerfConfig::load();
        if (!$config->cssRucss) {
            return ['ok' => true, 'detail' => 'RUCSS is disabled', 'queued' => 0];
        }

        // Resolve URLs.
        $rawUrls = [];
        if (isset($params['urls'])) {
            if (!is_array($params['urls'])) {
                return ['ok' => false, 'detail' => 'urls must be an array', 'queued' => 0];
            }
            foreach ($params['urls'] as $url) {
                if (is_string($url) && $url !== '') {
                    $rawUrls[] = $url;
                }
                if (count($rawUrls) >= self::MAX_URLS) {
                    break;
                }
            }
        }

        if ($rawUrls === [] && function_exists('home_url')) {
            $rawUrls = [(string) home_url('/')];
        }

        // SSRF guard: keep only same-host URLs.
        $siteHost = $this->siteHost();
        $safeUrls = [];
        foreach ($rawUrls as $url) {
            if ($this->isOnHost($url, $siteHost)) {
                $safeUrls[] = $url;
            }
        }

        if ($safeUrls === []) {
            return [
                'ok'     => false,
                'detail' => 'no valid same-host URLs supplied',
                'queued' => 0,
            ];
        }

        // Fetch each URL as a logged-out request with the preload bypass header
        // (so the disk cache is bypassed and WordPress renders fresh HTML) PLUS
        // our RUCSS-compute header (so the optimizer's RUCSS stage knows to post
        // to the CP). Fire-and-forget per URL; failures are tolerated.
        // A CP-initiated post-compute re-warm sets reheat=true so the self-fetch
        // carries the reheat marker (loop guard, see REHEAT_HEADER).
        $reheat = !empty($params['reheat']);

        $queued = 0;
        foreach ($safeUrls as $url) {
            try {
                // Force a guaranteed cache MISS: delete this URL's cached page so
                // the web server's static fast-path (nginx try_files / Apache
                // mod_rewrite) cannot serve a stale .html.gz ahead of PHP. The
                // request-header bypass alone is insufficient because those rules
                // match the on-disk file before PHP — and thus the optimizer's
                // RUCSS stage — ever runs.
                if ($this->cache !== null) {
                    $this->cache->purge($url);
                }
                if ($this->fetch($url, $reheat)) {
                    $queued++;
                }
            } catch (\Throwable $e) {
                // Non-fatal: skip this URL.
            }
        }

        $detail = $queued === count($safeUrls)
            ? 'RUCSS compute triggered for ' . $queued . ' URL(s)'
            : 'RUCSS compute triggered for ' . $queued . '/' . count($safeUrls) . ' URL(s)';

        return ['ok' => true, 'detail' => $detail, 'queued' => $queued];
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Perform the self-fetch that forces a fresh render through the optimizer
     * RUCSS stage. Returns true on a 2xx/3xx response, false on error.
     *
     * @param string $url    URL to fetch.
     * @param bool   $reheat Whether this is a CP post-compute re-warm self-fetch.
     * @return bool
     */
    private function fetch(string $url, bool $reheat = false): bool
    {
        if (!function_exists('wp_remote_get')) {
            return false;
        }

        // Desktop pass: the WPMgr-RUCSS UA is non-mobile, so it lands in the
        // desktop cache bucket (index.html.gz).
        $ok = $this->fetchUA($url, self::DESKTOP_UA, $reheat);

        // Mobile pass: when the site caches a separate mobile bucket, warm it too —
        // otherwise mobile visitors never receive the optimized (or any) cached
        // variant. structure_hash is UA-independent so the CP returns the SAME
        // used-CSS for both; only the on-disk filename differs (-mobile segment),
        // and a mobile UA (Preload::MOBILE_UA, an iPhone string) routes there.
        if ($this->cache !== null && $this->cache->config()->cacheMobile) {
            $okMobile = $this->fetchUA($url, Preload::MOBILE_UA, $reheat);
            $ok       = $ok || $okMobile;
        }

        return $ok;
    }

    /**
     * Issue one same-host self-fetch with the given User-Agent (which selects the
     * desktop vs mobile cache bucket) and the preload/compute bypass headers.
     *
     * @param string $url       URL to fetch.
     * @param string $userAgent UA selecting the cache bucket.
     * @param bool   $reheat    Whether this is a CP post-compute re-warm.
     * @return bool True on a 2xx/3xx response.
     */
    private function fetchUA(string $url, string $userAgent, bool $reheat): bool
    {
        $headers = [
            // Bypass the disk cache so the optimizer's RUCSS stage runs on
            // a guaranteed fresh render (same mechanism as the preload warmer).
            Preload::PRELOAD_HEADER => '1',
            // Signal to the Optimizer that this is an RUCSS-compute pass so
            // it can short-circuit other expensive transforms if needed.
            self::RUCSS_HEADER     => '1',
        ];
        if ($reheat) {
            // Mark the post-compute re-warm so the CP terminates the loop on a
            // structure_hash re-miss (the optimizer forwards this as meta.reheat).
            $headers[self::REHEAT_HEADER] = '1';
        }

        $response = wp_remote_get($url, [
            'timeout'            => 15,
            'redirection'        => 2,
            'blocking'           => true,
            'sslverify'          => true,
            // Engage WP's SSRF guard (blocks loopback / private-IP / non-standard
            // port) — defence-in-depth even for on-host URLs.
            'reject_unsafe_urls' => true,
            'user-agent'         => $userAgent,
            'headers'            => $headers,
        ]);

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return false;
        }

        $code = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        return $code >= 200 && $code < 400;
    }

    /**
     * Whether $url's host equals the site's own host (case-insensitive).
     *
     * @param string $url      Candidate URL.
     * @param string $siteHost Lower-case site host (empty → fail closed).
     * @return bool
     */
    private function isOnHost(string $url, string $siteHost): bool
    {
        if ($siteHost === '') {
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
     * Resolve the site's own host (lower-case) from home_url().
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
}
