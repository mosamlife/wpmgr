<?php
/**
 * RucssClient — Remove Unused CSS via the control plane.
 *
 * WPMgr does not compute used-CSS on the WordPress host (too CPU-heavy). Instead
 * the agent POSTs the rendered page HTML + the concatenated stylesheet CSS to the
 * control-plane RUCSS endpoint (signed with the site's Ed25519 key, exactly like
 * every other agent->CP call), keyed by a STRUCTURE HASH so the CP can dedup
 * identical page shapes.
 *
 * WIRE CONTRACT (matches the control plane)
 * -----------------------------------------
 *   POST {cp_base}/agent/v1/rucss
 *   Content-Type: multipart/form-data; boundary=...
 *   X-WPMgr-* Ed25519 signature headers (signed over the raw multipart body)
 *   parts:
 *     meta  application/json  {"site_id","url","structure_hash"}
 *     html  text/html         the rendered page HTML
 *     css   text/css          the concatenated stylesheet CSS
 *
 *   Responses:
 *     200  body IS the used CSS (Content-Type: text/css; possibly
 *          Content-Encoding: gzip — decode). Inline it + defer the originals.
 *     202  cache miss / still processing — serve this render UN-optimized
 *          (return the HTML UNCHANGED with full CSS).
 *     any other / timeout / exception — return the HTML UNCHANGED.
 *
 * CRITICAL GRACEFUL-DEGRADATION CONTRACT
 * --------------------------------------
 * RUCSS is an OPTIMIZATION, never a correctness requirement. This client MUST
 * NEVER throw to, or block, the render path:
 *   - Not enrolled / no CP URL / signing failure / network error / non-200 /
 *     202 / malformed response / empty used-css / ANY \Throwable
 *       => return the ORIGINAL HTML UNCHANGED (full CSS intact), log a short
 *          operational line, and move on.
 * The single public entry point optimize() wraps everything in try/catch and is
 * guaranteed side-effect-free on failure. The orchestrator can call it without
 * its own guard and the page is always served with working CSS.
 *
 * Standard control-plane RUCSS delegation technique.
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\DebugLog;

/**
 * Fetches used-CSS from the control plane and inlines it (degrades gracefully).
 */
final class RucssClient
{
    /** CP path the agent POSTs the page HTML+CSS to (multipart). */
    public const CP_PATH = '/agent/v1/rucss';

    /** Hard request timeout (seconds). RUCSS must never stall a render. */
    public const TIMEOUT = 8;

    /**
     * Built-in safelist (substring tokens) ALWAYS merged into the operator's
     * include-selectors. RUCSS purges against the SERVER-rendered DOM, so it is
     * structurally blind to the elements + state classes that JS libraries inject
     * at RUNTIME (a slider's slides, .is-initialized/.is-active reveal rules,
     * clones, lightbox markup). Without these their rules get stripped and the
     * widget breaks (e.g. a slider stuck at .splide{visibility:hidden} because its
     * paired `.splide.is-initialized{visibility:visible}` was purged). Substring-
     * matched by the CP engine, so each token covers all its variants. Kept tight
     * to runtime-injected library/state classes so it barely dents the reduction.
     */
    public const DEFAULT_SAFELIST = [
        // Slider / carousel libraries (DOM + state built at runtime).
        'splide', 'swiper', 'slick', 'flickity', 'glide__', 'tns-', 'owl-',
        // Lightbox / gallery libraries.
        'glightbox', 'fancybox', 'pswp',
        // Generic runtime STATE classes (sliders, tabs, accordions, menus, sticky).
        'is-active', 'is-initialized', 'is-rendered', 'is-visible', 'is-loaded',
        'is-selected', 'is-open', 'is-expanded', 'is-sticky', 'is-stuck',
        '--clone', '--active',
        // Scroll-animation libraries.
        'aos-animate', 'animate__animated',
    ];

    private Signer $signer;

    private Settings $settings;

    /** @var list<string> Extra selectors the CP must always keep. */
    private array $includeSelectors;

    private UrlHelper $urls;

    /**
     * Whether the LAST optimize() saw a genuine "processing" cache miss (HTTP 202
     * status=processing) — i.e. the used-CSS is being computed and WILL become
     * available on a later render. The cache writer reads this to DEFER caching an
     * optimization-incomplete page (so the static fast-path never serves the
     * un-optimized render indefinitely). A hard error / "unavailable" does NOT set
     * this (those won't resolve, so the page should cache normally with full CSS).
     */
    private bool $lastPending = false;

    /**
     * @param Signer        $signer           Ed25519 request signer.
     * @param Settings      $settings         Enrollment + CP URL source.
     * @param list<string>  $includeSelectors RUCSS safelist selectors.
     * @param UrlHelper|null $urls            Asset->path resolver (tests).
     */
    public function __construct(
        Signer $signer,
        Settings $settings,
        array $includeSelectors = [],
        ?UrlHelper $urls = null
    ) {
        $this->signer           = $signer;
        $this->settings         = $settings;
        // ALWAYS merge the built-in default safelist (slider/lightbox/runtime-state
        // classes RUCSS is structurally blind to) ahead of the operator's list, then
        // de-dup. This both protects the keep-set AND folds into structure_hash (the
        // selectors flow into StructureHash::compute), so a fleet on the old hash
        // recomputes once with the safety net instead of shipping broken sliders.
        $this->includeSelectors = array_values(array_unique(
            array_merge(self::DEFAULT_SAFELIST, $includeSelectors)
        ));
        $this->urls             = $urls ?? new UrlHelper();
    }

    /**
     * Replace the page's render-blocking CSS with the CP-computed used-CSS.
     *
     * GUARANTEE: returns the input HTML UNCHANGED (with full CSS) on ANY failure
     * (including a 202 cache-miss) and never throws. This is the load-bearing
     * graceful-degradation contract.
     *
     * @param string $html Full page HTML.
     * @return string Optimized HTML on a 200 hit; the original HTML otherwise.
     */
    public function optimize(string $html): string
    {
        $this->lastPending = false;
        try {
            if (!$this->settings->isEnrolled()) {
                return $html;
            }
            $usedCss = $this->fetchUsedCss($html);
            if ($usedCss === null || $usedCss === '') {
                // Cache miss (202) / no usable result — serve full CSS untouched.
                return $html;
            }
            return $this->applyUsedCss($html, $usedCss);
        } catch (\Throwable $e) {
            // The whole point: a RUCSS failure must NEVER break the page. Log a
            // short operational line (no secrets, no CSS) and return the input.
            DebugLog::write('wpmgr-agent: rucss degraded (' . $e->getMessage() . ')');
            return $html;
        }
    }

    /**
     * Whether the last optimize() saw a genuine "processing" cache miss (the
     * used-CSS is being computed and will become available on a later render).
     * The cache writer uses this to defer caching an optimization-incomplete page.
     *
     * @return bool
     */
    public function wasPending(): bool
    {
        return $this->lastPending;
    }

    /**
     * POST the HTML + concatenated CSS + structure hash to the CP as multipart;
     * return the used CSS on a 200 hit, or null on a 202 (cache miss) / any other
     * status. Never throws past optimize()'s catch, but is itself defensive.
     *
     * @param string $html Page HTML.
     * @return string|null Used CSS on a 200 hit, or null when unavailable.
     */
    private function fetchUsedCss(string $html): ?string
    {
        if (!function_exists('wp_remote_post')) {
            return null;
        }
        $base = $this->settings->controlPlaneUrl();
        if ($base === '') {
            return null;
        }

        $metaArr = [
            'site_id'        => $this->settings->siteId(),
            'url'            => $this->currentUrl(),
            'structure_hash' => StructureHash::compute($html, $this->includeSelectors),
        ];
        // Hand the engine the safelist so it can FORCE-KEEP these selectors before
        // any static-DOM match. Without this the CP runs the purge with an empty
        // list (meta.safelist nil) and the whole feature is a no-op — runtime-only
        // slider/widget rules get stripped. Always populated (defaults are merged in
        // the constructor), so the gate is just future-proofing against an empty set.
        if ($this->includeSelectors !== []) {
            $metaArr['safelist'] = $this->includeSelectors;
        }
        // Propagate the CP-initiated reheat marker: when this render is a
        // post-compute re-warm self-fetch (the rucss_compute command set the
        // x-wpmgr-rucss-reheat header), tell the CP so a re-miss (structure_hash
        // drift) does not trigger yet another reheat — the loop terminates.
        if (isset($_SERVER['HTTP_X_WPMGR_RUCSS_REHEAT'])) {
            $metaArr['reheat'] = true;
        }
        $meta = function_exists('wp_json_encode') ? wp_json_encode($metaArr) : null;
        if (!is_string($meta)) {
            return null;
        }

        $css      = $this->concatStylesheets($html);
        $boundary = 'wpmgr' . bin2hex(random_bytes(16));
        $body     = $this->buildMultipart($boundary, $meta, $html, $css);

        // Sign the EXACT multipart body bytes (same primitive as every other
        // agent->CP call): METHOD\nPATH\nTS\nNONCE\nhex(sha256(body)).
        $headers = $this->signer->signHeaders('POST', self::CP_PATH, $body);

        $response = wp_remote_post(
            $base . self::CP_PATH,
            [
                'timeout' => self::TIMEOUT,
                'headers' => array_merge(
                    [
                        'Content-Type' => 'multipart/form-data; boundary=' . $boundary,
                        'Accept'       => 'text/css',
                    ],
                    $headers
                ),
                'body'    => $body,
            ]
        );

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return null;
        }
        $code = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        // 202 => cache miss. Distinguish a genuine "processing" 202 (used-CSS is
        // being computed and WILL land — defer caching this render) from
        // "unavailable" / any other status (RUCSS won't resolve — cache full CSS
        // normally). Only "processing" marks the render pending.
        if ($code !== 200) {
            if ($code === 202) {
                $body = function_exists('wp_remote_retrieve_body')
                    ? (string) wp_remote_retrieve_body($response)
                    : '';
                $decoded = json_decode($body, true);
                $status  = is_array($decoded) ? (string) ($decoded['status'] ?? '') : '';
                $this->lastPending = ($status === 'processing');
            }
            return null;
        }

        $rawBody = function_exists('wp_remote_retrieve_body')
            ? (string) wp_remote_retrieve_body($response)
            : '';
        if ($rawBody === '') {
            return null;
        }

        $usedCss = $this->maybeGunzip($response, $rawBody);
        return $usedCss !== '' ? $usedCss : null;
    }

    /**
     * Build the multipart/form-data body for the meta/html/css parts.
     *
     * @param string $boundary Multipart boundary token.
     * @param string $meta     JSON meta part.
     * @param string $html     Page HTML part.
     * @param string $css      Concatenated CSS part.
     * @return string Raw multipart body.
     */
    private function buildMultipart(string $boundary, string $meta, string $html, string $css): string
    {
        $eol   = "\r\n";
        $parts = [
            ['name' => 'meta', 'type' => 'application/json', 'data' => $meta],
            ['name' => 'html', 'type' => 'text/html',        'data' => $html],
            ['name' => 'css',  'type' => 'text/css',         'data' => $css],
        ];

        $body = '';
        foreach ($parts as $part) {
            $body .= '--' . $boundary . $eol;
            $body .= 'Content-Disposition: form-data; name="' . $part['name'] . '"' . $eol;
            $body .= 'Content-Type: ' . $part['type'] . $eol . $eol;
            $body .= $part['data'] . $eol;
        }
        $body .= '--' . $boundary . '--' . $eol;

        return $body;
    }

    /**
     * Concatenate the CSS of the page's local stylesheets (best-effort). External
     * sheets that cannot be resolved to a local file are skipped; the CP still
     * gets whatever local CSS we could read. Never throws.
     *
     * @param string $html Page HTML.
     * @return string Concatenated CSS (possibly '').
     */
    private function concatStylesheets(string $html): string
    {
        $css = '';
        if (!preg_match_all('/<link\b[^>]*\brel=["\']stylesheet["\'][^>]*>/i', $html, $links)) {
            return $css;
        }
        foreach ($links[0] as $link) {
            $href = TagHelper::attr($link, 'href');
            if ($href === null || $href === '') {
                continue;
            }
            $path = $this->urls->localPath($href);
            if ($path === null || !is_file($path) || !is_readable($path)) {
                continue;
            }
            $contents = @file_get_contents($path);
            if (is_string($contents) && $contents !== '') {
                // Rebase relative url() to ABSOLUTE against THIS stylesheet's own
                // location before sending to the CP. The CP returns used-CSS that we
                // inline into the page <head>; a relative url() like
                // "../../fonts/x.woff2" (valid from the stylesheet at
                // /wp-content/themes/.../css/libs/) would otherwise resolve against
                // the PAGE path once inlined (→ /fonts/x.woff2, 404, broken fonts
                // and background images). Resolving here keeps every asset URL valid.
                $css .= $this->rebaseCssUrls($contents, $href) . "\n";
            }
        }
        return $css;
    }

    /**
     * Rewrite every url(...) in a stylesheet to an ABSOLUTE URL resolved against
     * that stylesheet's own location ($baseHref). data:/absolute/protocol-relative
     * refs are left untouched. Handles single/double/unquoted url() and ./ ../.
     *
     * @param string $css      Raw stylesheet CSS.
     * @param string $baseHref The stylesheet href (may be root-/protocol-relative).
     * @return string
     */
    private function rebaseCssUrls(string $css, string $baseHref): string
    {
        $base = $this->absolutizeHref($baseHref);
        if ($base === '') {
            return $css;
        }
        $out = preg_replace_callback(
            '/url\(\s*("[^"]*"|\'[^\']*\'|[^)]*)\s*\)/i',
            function (array $m) use ($base): string {
                $raw = trim($m[1]);
                $q   = '';
                if (strlen($raw) >= 2 && ($raw[0] === '"' || $raw[0] === "'") && $raw[strlen($raw) - 1] === $raw[0]) {
                    $q   = $raw[0];
                    $raw = substr($raw, 1, -1);
                }
                return 'url(' . $q . $this->resolveUrl($base, $raw) . $q . ')';
            },
            $css
        );
        return is_string($out) ? $out : $css;
    }

    /**
     * Make a stylesheet href absolute (scheme+host) so it can serve as a resolution
     * base: protocol-relative → https:; root-relative → site URL + path.
     *
     * @param string $href Stylesheet href.
     * @return string Absolute base URL, or '' when no site host is known.
     */
    private function absolutizeHref(string $href): string
    {
        $href = trim($href);
        if ($href === '') {
            return '';
        }
        if (str_starts_with($href, '//')) {
            return 'https:' . $href;
        }
        if (preg_match('#^[a-z][a-z0-9+.\-]*://#i', $href) === 1) {
            return $href; // already absolute
        }
        $site = $this->urls->siteUrl();
        if ($site === '') {
            return '';
        }
        return $site . '/' . ltrim($href, '/');
    }

    /**
     * Resolve a CSS url() reference against an absolute base URL (the stylesheet's
     * own URL). data:/absolute/protocol-relative/fragment refs pass through.
     *
     * @param string $base Absolute stylesheet URL.
     * @param string $ref  The url() target.
     * @return string Absolute URL (or the ref unchanged when it cannot be resolved).
     */
    private function resolveUrl(string $base, string $ref): string
    {
        $ref = trim($ref);
        if ($ref === ''
            || str_starts_with($ref, 'data:')
            || str_starts_with($ref, '#')
            || str_starts_with($ref, '//')
            || preg_match('#^[a-z][a-z0-9+.\-]*:#i', $ref) === 1
        ) {
            return $ref;
        }

        $scheme = wp_parse_url($base, PHP_URL_SCHEME) ?: 'https';
        $host   = wp_parse_url($base, PHP_URL_HOST);
        if (!is_string($host) || $host === '') {
            return $ref;
        }
        $port      = wp_parse_url($base, PHP_URL_PORT);
        $authority = $scheme . '://' . $host . ($port ? ':' . $port : '');

        if ($ref[0] === '/') {
            return $authority . self::removeDotSegments($ref);
        }

        $basePath = wp_parse_url((string) (preg_replace('/[?#].*$/', '', $base) ?? $base), PHP_URL_PATH);
        $basePath = is_string($basePath) && $basePath !== '' ? $basePath : '/';
        $dir      = substr($basePath, 0, (int) strrpos($basePath, '/') + 1);
        if ($dir === '') {
            $dir = '/';
        }
        return $authority . self::removeDotSegments($dir . $ref);
    }

    /**
     * Collapse "." and ".." segments in an absolute path (RFC 3986
     * remove_dot_segments, simplified). Always returns a leading-slash path.
     *
     * @param string $path Path possibly containing ./ and ../.
     * @return string
     */
    private static function removeDotSegments(string $path): string
    {
        $out = [];
        foreach (explode('/', $path) as $seg) {
            if ($seg === '..') {
                array_pop($out);
            } elseif ($seg !== '.' && $seg !== '') {
                $out[] = $seg;
            }
        }
        $trail = (substr($path, -1) === '/' && $path !== '/') ? '/' : '';
        return '/' . implode('/', $out) . ($out ? $trail : '');
    }

    /**
     * Gunzip the response body when the CP marked it Content-Encoding: gzip.
     * WP's HTTP API usually decodes transport gzip itself, but the CP may set the
     * header explicitly on the used-CSS payload; decode defensively. Falls back to
     * the raw body when decoding is unavailable or fails.
     *
     * @param mixed  $response The wp_remote_post response.
     * @param string $rawBody  Raw response body.
     * @return string Decoded CSS.
     */
    private function maybeGunzip($response, string $rawBody): string
    {
        $encoding = '';
        if (function_exists('wp_remote_retrieve_header')) {
            $header = wp_remote_retrieve_header($response, 'content-encoding');
            // WP may return an array of header values; collapse to a string.
            if (is_array($header)) {
                $header = implode(',', array_map('strval', $header));
            }
            $encoding = strtolower(is_string($header) ? $header : '');
        }
        if (strpos($encoding, 'gzip') === false) {
            return $rawBody;
        }
        if (!function_exists('gzdecode')) {
            return $rawBody;
        }
        $decoded = @gzdecode($rawBody);
        return is_string($decoded) && $decoded !== '' ? $decoded : $rawBody;
    }

    /**
     * The URL of the page currently being optimized (own host). Derived from the
     * request URI joined to home_url(); never trusted as an outbound target — it
     * is metadata only.
     *
     * @return string
     */
    private function currentUrl(): string
    {
        $home = function_exists('home_url') ? (string) home_url('/') : '';
        $base = rtrim($home, '/');
        $uri  = isset($_SERVER['REQUEST_URI']) && is_string($_SERVER['REQUEST_URI'])
            ? sanitize_text_field(wp_unslash($_SERVER['REQUEST_URI']))
            : '/';
        return $base . '/' . ltrim($uri, '/');
    }

    /**
     * Inline the used-CSS in <head> and defer the original stylesheets to load
     * on interaction (so the full CSS still arrives, just non-blocking).
     *
     * @param string $html    Page HTML.
     * @param string $usedCss Minimal used CSS.
     * @return string
     */
    private function applyUsedCss(string $html, string $usedCss): string
    {
        // Belt-and-suspenders break-out guard: $usedCss is computed by a
        // trusted first-party compute service, but a stray literal "</style"
        // inside it (e.g. from an unusual selector/content value) would
        // otherwise close the tag early and let the remainder of $usedCss
        // render as raw HTML. Neutralize any case-insensitive "</style"
        // sequence before inlining.
        $usedCss = str_ireplace('</style', '<\/style', $usedCss);
        // phpcs:ignore WordPress.WP.EnqueuedResources.NonEnqueuedStylesheet -- this class only runs from Optimizer::run(), called by CacheWriter's ob_start() callback (class-cache-writer.php) on the fully-rendered HTML string of a cacheable MISS, well after wp_head has already printed; the RUCSS transform rewrites a <style>/<link href> already in the buffered string, so it can only operate as a post-hoc string rewrite -- WP's enqueue API has no queue left to append to at this point
        $styleTag = '<style id="wpmgr-used-css">' . $usedCss . '</style>';
        if (stripos($html, '</head>') !== false) {
            $html = (string) preg_replace('/<\/head>/i', $styleTag . '</head>', $html, 1);
        } else {
            $html = $styleTag . $html;
        }

        // Defer the original render-blocking stylesheets: rename href -> the
        // delay attribute so the delay runtime swaps them post-interaction.
        if (preg_match_all('/<link\b[^>]*\brel=["\']stylesheet["\'][^>]*>/i', $html, $links)) {
            foreach ($links[0] as $link) {
                // Keep print stylesheets as-is.
                if (strtolower((string) (TagHelper::attr($link, 'media') ?? '')) === 'print') {
                    continue;
                }
                if (TagHelper::attr($link, 'href') === null) {
                    continue;
                }
                $deferred = TagHelper::renameAttr($link, 'href', 'data-wpmgr-href');
                $html = str_replace($link, $deferred, $html);
            }
        }
        return $html;
    }
}
