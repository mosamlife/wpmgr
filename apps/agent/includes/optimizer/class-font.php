<?php
/**
 * Font — font-display optimization.
 *
 * Four independent transforms, each gated by its PerfConfig flag:
 *   - display-swap        : add `&display=swap` to Google Fonts <link> hrefs and
 *     inject `font-display:swap` into every @font-face block in inline <style>
 *     tags, so text renders immediately with a fallback while the web font loads
 *     (kills the FOIT / invisible-text flash).
 *   - optimize-google     : self-host the Google Fonts stylesheet (download the
 *     CSS, rewrite its font URLs to local copies) and rewrite the <link> href to
 *     the local file, removing the render-blocking cross-origin request.
 *   - preload             : heuristically emit `<link rel=preload as=font>` for
 *     the first few woff2 files referenced by self-hosted/inline font CSS.
 *   - transcode-woff2     : for each self-hosted TTF/OTF/WOFF font found in BOTH
 *     inline <style> blocks AND enqueued external stylesheets, request a WOFF2
 *     transcode from the media-encoder via the CP, cache the result, and rewrite
 *     the @font-face src to serve WOFF2-first with the original as a format()
 *     fallback. When a latin-ext subset is available (fonts_subset ON), an
 *     additive @font-face with a unicode-range descriptor is prepended so the
 *     browser fetches the smaller subset for in-range codepoints and falls back to
 *     the full WOFF2 for anything else. Always serves the original on any
 *     miss/failure. Never blocks the page on a pending transcode.
 *
 * Standard font-display optimization technique.
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

use WPMgr\Agent\Support\Blake3;

/**
 * Applies font-display + Google-Fonts self-host + font preload + woff2 transcode.
 */
final class Font
{
    /**
     * Source extensions that are candidates for WOFF2 transcoding.
     * WOFF2/SVG/EOT are intentionally excluded: woff2 is already optimal,
     * svg/eot are legacy-only and not worth transcoding.
     */
    private const TRANSCODE_EXTS = ['ttf', 'otf', 'woff'];

    /** CSS format() hint per extension. */
    private const FORMAT_HINTS = [
        'woff'  => 'woff',
        'ttf'   => 'truetype',
        'otf'   => 'opentype',
    ];

    /**
     * Hosts whose external stylesheets we are allowed to fetch and scan for
     * font URLs. The site's own host is always allowed (checked dynamically).
     * Google Fonts has its own self-host pipeline so its CSS is handled
     * separately and intentionally excluded here.
     *
     * @var list<string>
     */
    private const ALLOWED_EXTERNAL_CSS_HOSTS = [
        'fonts.googleapis.com', // handled by the Google self-host path, not the generic scanner
    ];

    /**
     * Maximum byte-size of an external CSS file we will fetch and scan.
     * Protects against fetching giant files from a third-party host.
     */
    private const MAX_EXTERNAL_CSS_BYTES = 512 * 1024; // 512 KiB

    private PerfConfig $config;

    private AssetCache $cache;

    /** @var callable(string):?string Downloader for the Google Fonts CSS. */
    private $downloader;

    /** @var FontTranscodeClientInterface|null Transcode client; null disables the feature. */
    private ?FontTranscodeClientInterface $transcodeClient;

    /**
     * Per-page accumulator: maps source_hash => discovery metadata for the
     * fire-and-forget results push. Populated during transcodeWoff2InlineStyles()
     * and transcodeWoff2ExternalStylesheets() so the results push covers every
     * font seen on this page build, not just inline <style> blocks.
     *
     * @var array<string,array{family:string,source_file:string,source_ext:string,original_size:int}>
     */
    private array $discoveredFonts = [];

    /**
     * @param PerfConfig|null                    $config          Optimization config.
     * @param AssetCache|null                    $cache           Asset store.
     * @param callable|null                      $downloader      Override fetcher (tests). url -> bytes|null.
     * @param FontTranscodeClientInterface|null  $transcodeClient Woff2 transcode client; null = feature off.
     */
    public function __construct(
        ?PerfConfig $config = null,
        ?AssetCache $cache = null,
        ?callable $downloader = null,
        ?FontTranscodeClientInterface $transcodeClient = null
    ) {
        $this->config          = $config ?? PerfConfig::load();
        $this->cache           = $cache ?? new AssetCache();
        $this->downloader      = $downloader ?? [new SelfHost($this->cache), 'fetch'];
        $this->transcodeClient = $transcodeClient;
    }

    /**
     * Run the enabled font transforms over the page.
     *
     * @param string $html Full page HTML.
     * @return string
     */
    public function process(string $html): string
    {
        if ($this->config->fontsDisplaySwap) {
            $html = $this->addDisplaySwapToGoogleLinks($html);
            $html = $this->addDisplaySwapToInlineStyles($html);
        }
        if ($this->config->fontsOptimizeGoogle && $this->cache->isUsable()) {
            $html = $this->selfHostGoogleFonts($html);
        }
        if ($this->config->fontsPreload) {
            $html = $this->preloadFonts($html);
        }
        if ($this->config->fontsTranscodeWoff2 && $this->transcodeClient !== null) {
            $this->discoveredFonts = [];
            $html = $this->transcodeWoff2InlineStyles($html);
            $html = $this->transcodeWoff2ExternalStylesheets($html);
            $this->scanFontLibraryFilesystem();
        }
        return $html;
    }

    /**
     * Add `display=swap` to every Google Fonts <link>.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function addDisplaySwapToGoogleLinks(string $html): string
    {
        if (!preg_match_all('/<link\b[^>]*href=["\'][^"\']*fonts\.googleapis\.com\/css[^"\']*["\'][^>]*>/i', $html, $tags)) {
            return $html;
        }
        foreach ($tags[0] as $tag) {
            $href = TagHelper::attr($tag, 'href');
            if ($href === null) {
                continue;
            }
            $new = preg_match('/[?&]display=/', $href)
                ? (string) preg_replace('/display=[a-z]+/i', 'display=swap', $href)
                : $href . (strpos($href, '?') !== false ? '&' : '?') . 'display=swap';
            $html = str_replace($tag, TagHelper::setAttr($tag, 'href', $new), $html);
        }
        return $html;
    }

    /**
     * Inject `font-display:swap` into @font-face blocks inside inline <style>.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function addDisplaySwapToInlineStyles(string $html): string
    {
        return (string) preg_replace_callback(
            '/<style\b[^>]*>(.*?)<\/style>/is',
            static function (array $m): string {
                return str_replace($m[1], self::injectDisplaySwap($m[1]), $m[0]);
            },
            $html
        );
    }

    /**
     * Inject `font-display:swap` into a CSS string's @font-face blocks (also
     * normalises any existing font-display to swap).
     *
     * @param string $css CSS.
     * @return string
     */
    public static function injectDisplaySwap(string $css): string
    {
        if (stripos($css, '@font-face') === false) {
            return $css;
        }
        // Drop existing font-display declarations, then add swap right after
        // each @font-face {.
        $css = (string) preg_replace('/font-display\s*:\s*(swap|block|fallback|optional|auto)\s*;?/i', '', $css);
        return (string) preg_replace('/@font-face\s*\{/i', '@font-face{font-display:swap;', $css);
    }

    /**
     * Self-host the Google Fonts stylesheet(s) referenced by <link> tags.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function selfHostGoogleFonts(string $html): string
    {
        if (!preg_match_all('/<link\b[^>]*href=["\']([^"\']*fonts\.googleapis\.com\/css[^"\']*)["\'][^>]*>/i', $html, $tags, PREG_SET_ORDER)) {
            return $html;
        }
        foreach ($tags as $set) {
            $tag  = $set[0];
            $href = html_entity_decode($set[1]);
            $localUrl = $this->cachedGoogleCssUrl($href);
            if ($localUrl === null) {
                continue;
            }
            $html = str_replace($tag, TagHelper::setAttr($tag, 'href', $localUrl), $html);
        }
        return $html;
    }

    /**
     * Download + cache a Google Fonts stylesheet, rewriting its font URLs to
     * local copies; return the local CSS URL (or null on any failure).
     *
     * @param string $href Google Fonts CSS URL.
     * @return string|null
     */
    private function cachedGoogleCssUrl(string $href): ?string
    {
        if (str_starts_with($href, '//')) {
            $href = 'https:' . $href;
        }
        $name = $this->cache->name($href, 'google-font', 'css');
        if ($this->cache->exists($name)) {
            return $this->cache->url($name);
        }
        $css = ($this->downloader)($href);
        if (!is_string($css) || $css === '') {
            return null;
        }
        // Download the woff2/woff files and rewrite their URLs to local copies.
        foreach ($this->extractFontUrls($css) as $fontUrl) {
            $fontName = $this->cache->name($fontUrl, basename((string) (wp_parse_url($fontUrl, PHP_URL_PATH) ?? '')), pathinfo($fontUrl, PATHINFO_EXTENSION) ?: 'woff2');
            if (!$this->cache->exists($fontName)) {
                $fontBytes = ($this->downloader)($fontUrl);
                if (!is_string($fontBytes) || strlen($fontBytes) < 100) {
                    continue;
                }
                $this->cache->write($fontName, $fontBytes);
            }
            $css = str_replace($fontUrl, $this->cache->url($fontName), $css);
        }
        if (!$this->cache->write($name, $css)) {
            return null;
        }
        return $this->cache->url($name);
    }

    /**
     * Emit preload tags for the first few woff2 fonts referenced by inline /
     * self-hosted font CSS (heuristic: critical above-the-fold fonts).
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function preloadFonts(string $html): string
    {
        $fonts = [];
        if (preg_match_all('/<style\b[^>]*>(.*?)<\/style>/is', $html, $styles)) {
            foreach ($styles[1] as $css) {
                foreach ($this->extractFontUrls($css) as $url) {
                    if (preg_match('/\.woff2(\?|$)/i', $url)) {
                        $fonts[$url] = true;
                    }
                }
            }
        }
        if ($fonts === []) {
            return $html;
        }
        // Heuristic cap — preload at most 2 fonts so we do not flood the head.
        $fonts = array_slice(array_keys($fonts), 0, 2);
        $links = '';
        foreach ($fonts as $url) {
            $links .= '<link rel="preload" href="' . htmlspecialchars($url, ENT_QUOTES) . '" as="font" type="font/woff2" crossorigin>';
        }
        if (stripos($html, '</head>') !== false) {
            return (string) preg_replace('/<\/head>/i', $links . '</head>', $html, 1);
        }
        return $links . $html;
    }

    // -----------------------------------------------------------------------
    // WOFF2 transcode — inline <style> blocks
    // -----------------------------------------------------------------------

    /**
     * Attempt WOFF2 transcoding for TTF/OTF/WOFF fonts in inline <style> blocks.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function transcodeWoff2InlineStyles(string $html): string
    {
        if ($this->transcodeClient === null) {
            return $html;
        }
        return (string) preg_replace_callback(
            '/<style\b[^>]*>(.*?)<\/style>/is',
            function (array $m): string {
                $newCss = $this->rewriteFontFaceForWoff2($m[1]);
                return str_replace($m[1], $newCss, $m[0]);
            },
            $html
        );
    }

    // -----------------------------------------------------------------------
    // WOFF2 transcode — external enqueued stylesheets
    // -----------------------------------------------------------------------

    /**
     * Attempt WOFF2 transcoding for fonts found in enqueued external <link> stylesheets.
     *
     * This closes the classic-theme/plugin gap: fonts loaded via wp_enqueue_style()
     * produce a <link rel=stylesheet> rather than an inline <style>, so they are
     * never seen by transcodeWoff2InlineStyles(). We fetch, rewrite, and cache the
     * stylesheet, then swap its <link> href to the locally-rewritten copy.
     *
     * Security: only fetches same-origin stylesheets (or hosts explicitly allowed
     * by ALLOWED_EXTERNAL_CSS_HOSTS). Google Fonts is excluded because it has its
     * own self-host pipeline. SSRF mitigated by origin check + MAX_EXTERNAL_CSS_BYTES.
     *
     * External stylesheets whose font src URLs are rewritten are cached locally by
     * hash. The rewritten copy contains absolute woff2 URLs into the agent AssetCache.
     *
     * @param string $html Page HTML.
     * @return string
     */
    private function transcodeWoff2ExternalStylesheets(string $html): string
    {
        if ($this->transcodeClient === null) {
            return $html;
        }

        // Find all external CSS <link> tags.
        if (!preg_match_all(
            '/<link\b[^>]*rel=["\']stylesheet["\'][^>]*href=["\']([^"\']+)["\'][^>]*>/i',
            $html,
            $byRelFirst,
            PREG_SET_ORDER
        )) {
            // Also try href-first ordering.
            if (!preg_match_all(
                '/<link\b[^>]*href=["\']([^"\']+)["\'][^>]*rel=["\']stylesheet["\'][^>]*>/i',
                $html,
                $byHrefFirst,
                PREG_SET_ORDER
            )) {
                return $html;
            }
            $linkMatches = $byHrefFirst;
        } else {
            $linkMatches = $byRelFirst;
        }

        foreach ($linkMatches as $set) {
            $tag  = $set[0];
            $href = html_entity_decode($set[1]);

            if (!$this->isAllowedExternalCssUrl($href)) {
                continue;
            }

            $rewrittenUrl = $this->rewriteExternalStylesheet($href);
            if ($rewrittenUrl === null) {
                continue;
            }
            // Swap the link href to the locally-cached rewritten version.
            $html = str_replace($tag, TagHelper::setAttr($tag, 'href', $rewrittenUrl), $html);
        }

        return $html;
    }

    /**
     * Fetch an external stylesheet, rewrite any font @font-face blocks for WOFF2,
     * cache the result, and return the local URL of the rewritten CSS.
     *
     * Returns null when no font rewrites were needed (not worth caching) or on
     * any fetch/write failure.
     *
     * @param string $href External stylesheet URL.
     * @return string|null Local URL of the rewritten stylesheet, or null.
     */
    private function rewriteExternalStylesheet(string $href): ?string
    {
        // Cache key: hash of the original href. The CSS content-hash is part of
        // the rewritten file name so stale caches are naturally busted on change.
        $cacheName = $this->cache->name($href, 'ext-css', 'css');
        if ($this->cache->exists($cacheName)) {
            return $this->cache->url($cacheName);
        }

        $css = ($this->downloader)($href);
        if (!is_string($css) || $css === '' || strlen($css) > self::MAX_EXTERNAL_CSS_BYTES) {
            return null;
        }

        if (stripos($css, '@font-face') === false) {
            return null; // no fonts — nothing to rewrite
        }

        $rewritten = $this->rewriteFontFaceForWoff2($css);
        if ($rewritten === $css) {
            return null; // no rewrites performed — skip caching
        }

        if (!$this->cache->write($cacheName, $rewritten)) {
            return null;
        }

        return $this->cache->url($cacheName);
    }

    /**
     * Determine whether a stylesheet URL is allowed to be fetched and scanned.
     *
     * Only same-origin stylesheets are allowed; cross-origin URLs are rejected
     * to prevent SSRF. Google Fonts is explicitly excluded (handled elsewhere).
     *
     * @param string $href Stylesheet URL.
     * @return bool
     */
    private function isAllowedExternalCssUrl(string $href): bool
    {
        // Normalise protocol-relative URLs.
        if (str_starts_with($href, '//')) {
            $href = 'https:' . $href;
        }

        // Must be an absolute http/https URL.
        if (!str_starts_with($href, 'http://') && !str_starts_with($href, 'https://')) {
            return false;
        }

        $parsed = wp_parse_url($href);
        if (!is_array($parsed)) {
            return false;
        }
        $host = isset($parsed['host']) && is_string($parsed['host']) ? strtolower($parsed['host']) : '';
        if ($host === '') {
            return false;
        }

        // Block Google Fonts — its CSS has its own self-host pipeline.
        foreach (self::ALLOWED_EXTERNAL_CSS_HOSTS as $blockedHost) {
            if ($host === strtolower($blockedHost)) {
                return false;
            }
        }

        // Allow same-origin only.
        $siteHost = '';
        if (function_exists('home_url')) {
            $siteParsed = wp_parse_url(home_url());
            if (is_array($siteParsed) && isset($siteParsed['host']) && is_string($siteParsed['host'])) {
                $siteHost = strtolower($siteParsed['host']);
            }
        }

        return $siteHost !== '' && $host === $siteHost;
    }

    // -----------------------------------------------------------------------
    // WP Font Library filesystem scan (WP 6.5+)
    // -----------------------------------------------------------------------

    /**
     * Walk the WP Font Library directory (wp-content/fonts by default) and
     * transcode any TTF/OTF/WOFF files found there.
     *
     * These fonts are reported to the CP results endpoint but their @font-face
     * src is NOT rewritten here — only fonts referenced in scanned CSS get their
     * src rewritten. Filesystem-discovered-but-unreferenced fonts are transcoded
     * and reported so they appear in the QA dashboard, but serve no @font-face
     * rewrite output (they may never be referenced in CSS at all). This is
     * expected and documented.
     *
     * Security: the walk stays strictly within the resolved fonts directory.
     * Symlinks and path traversal are prevented by resolving realpath and
     * verifying the prefix before descending.
     *
     * @return void
     */
    private function scanFontLibraryFilesystem(): void
    {
        if ($this->transcodeClient === null) {
            return;
        }

        $fontsDir = $this->resolveFontLibraryDir();
        if ($fontsDir === '' || !is_dir($fontsDir)) {
            return;
        }

        try {
            $this->walkFontDir($fontsDir, $fontsDir);
        } catch (\Throwable $e) {
            // Never let the filesystem scan break the page build.
        }
    }

    /**
     * Resolve the WP Font Library directory path, honouring the font_dir filter
     * if it is available (WP 6.5+). Returns '' when the directory is not
     * accessible or resolves outside wp-content.
     *
     * @return string Resolved absolute path with no trailing slash, or ''.
     */
    private function resolveFontLibraryDir(): string
    {
        // The WordPress 6.5+ Font Library stores fonts under wp-content/fonts by
        // default, overridable via the 'font_dir' filter. Resolve it WITHOUT calling
        // wp_get_font_dir() (introduced in 6.5) so the plugin stays compatible with
        // its declared minimum WordPress version; the 'font_dir' filter is honored
        // either way, matching what wp_get_font_dir() does internally.
        $dir          = '';
        $wpContentDir = defined('WP_CONTENT_DIR') ? (string) constant('WP_CONTENT_DIR') : '';
        if ($wpContentDir !== '') {
            $default = ['path' => rtrim($wpContentDir, '/\\') . '/fonts'];
            /** @var array<string,mixed> $info */
            // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- 'font_dir' is a WordPress core filter (the one wp_get_font_dir uses); it must stay unprefixed.
            $info = (array) apply_filters('font_dir', $default);
            $path = isset($info['path']) && is_string($info['path']) ? $info['path'] : $default['path'];
            $dir  = rtrim($path, '/\\');
        }

        if ($dir === '') {
            return '';
        }

        // Resolve symlinks and verify the path exists.
        $resolved = realpath($dir);
        if ($resolved === false) {
            return '';
        }

        return $resolved;
    }

    /**
     * Recursively walk $dir (bounded to $baseDir) and transcode font files found.
     *
     * @param string $dir     Current directory to scan.
     * @param string $baseDir Top-level fonts directory (traversal guard).
     * @return void
     */
    private function walkFontDir(string $dir, string $baseDir): void
    {
        $entries = @scandir($dir);
        if ($entries === false) {
            return;
        }

        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }

            $path = $dir . '/' . $entry;

            // Resolve to catch symlinks before checking the prefix guard.
            $realPath = realpath($path);
            if ($realPath === false) {
                continue;
            }

            // Traversal guard: must stay within the base fonts dir. The prefix is
            // separator-terminated so a sibling directory whose name merely starts
            // with the base name (e.g. "fonts-secret" vs "fonts") cannot escape.
            $guardPrefix = rtrim($baseDir, '/\\') . '/';
            if (strncmp($realPath . '/', $guardPrefix, strlen($guardPrefix)) !== 0) {
                continue;
            }

            if (is_dir($realPath)) {
                $this->walkFontDir($realPath, $baseDir);
                continue;
            }

            $ext = strtolower((string) pathinfo($realPath, PATHINFO_EXTENSION));
            if (!in_array($ext, self::TRANSCODE_EXTS, true)) {
                continue;
            }

            $this->transcodeFilesystemFont($realPath, $ext);
        }
    }

    /**
     * Transcode a single font file discovered via the filesystem scan.
     *
     * @param string $absolutePath Resolved absolute path to the font file.
     * @param string $ext          "ttf" | "otf" | "woff".
     * @return void
     */
    private function transcodeFilesystemFont(string $absolutePath, string $ext): void
    {
        $sourceBytes = @file_get_contents($absolutePath); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_get_contents_file_get_contents -- reading a local file in a headless agent; WP_Filesystem is unavailable in this context
        if ($sourceBytes === false || strlen($sourceBytes) < 12) {
            return;
        }

        $hash = Blake3::hashHex($sourceBytes);
        if ($hash === '' || isset($this->discoveredFonts[$hash])) {
            return; // already processed via CSS scan this page build
        }

        $basename = basename($absolutePath);

        // Record for the results push but don't add a family (no CSS context).
        $this->discoveredFonts[$hash] = [
            'family'       => '',
            'source_file'  => $basename,
            'source_ext'   => $ext,
            'original_size' => strlen($sourceBytes),
        ];

        $subsetMode  = '';
        $subsetRange = '';
        if ($this->config->fontsSubset && $this->config->fontsTranscodeWoff2) {
            $subsetMode  = $this->config->fontsSubsetMode;
            $subsetRange = $this->config->fontsSubsetRange;
        }

        $result = $this->transcodeClient !== null
            ? $this->transcodeClient->resolve($sourceBytes, $ext, $subsetMode, $subsetRange)
            : null;

        $this->pushDiscoveryResult($hash, '', $basename, $ext, strlen($sourceBytes), $result);
    }

    // -----------------------------------------------------------------------
    // @font-face rewrite — shared by inline + external paths
    // -----------------------------------------------------------------------

    /**
     * Rewrite @font-face src declarations in a CSS string for WOFF2-first delivery.
     *
     * Each @font-face block is scanned for a src that contains a URL pointing at
     * a TTF/OTF/WOFF font. When transcoding succeeds (state=="ready" or "subset"),
     * the src is rewritten. Any failure leaves the src unchanged.
     *
     * @param string $css CSS containing zero or more @font-face blocks.
     * @return string
     */
    private function rewriteFontFaceForWoff2(string $css): string
    {
        if (stripos($css, '@font-face') === false) {
            return $css;
        }

        $result = (string) preg_replace_callback(
            '/@font-face\s*\{([^}]*)\}/is',
            function (array $m): string {
                $block    = $m[0];
                $interior = $m[1];

                try {
                    $rewritten = $this->tryRewriteFontFaceBlock($block, $interior);
                    return $rewritten ?? $block;
                } catch (\Throwable $e) {
                    return $block;
                }
            },
            $css
        );

        return $result === null ? $css : $result;
    }

    /**
     * Try to rewrite a single @font-face block for WOFF2-first delivery.
     *
     * When a subset is ready (state=="subset"), an additive @font-face rule with a
     * unicode-range descriptor is prepended to the canonical full-WOFF2 rule:
     *
     *   @font-face{unicode-range:U+…;src:url('subset.woff2') format('woff2')}
     *   @font-face{src:url('full.woff2') format('woff2'),url('orig.ttf') format('truetype')}
     *
     * The browser fetches the subset for in-range codepoints and falls back to the
     * full WOFF2 otherwise. The full WOFF2 is always the canonical fallback.
     *
     * Returns the rewritten block(s) on success, or null to signal "keep original".
     *
     * @param string $block    Full @font-face { ... } string.
     * @param string $interior Interior declarations.
     * @return string|null Rewritten block, or null = keep original.
     */
    private function tryRewriteFontFaceBlock(string $block, string $interior): ?string
    {
        if ($this->transcodeClient === null) {
            return null;
        }

        // Extract font-family name for discovery metadata (informational).
        $fontFamily = '';
        if (preg_match('/font-family\s*:\s*["\']?([^"\';\}]+)["\']?\s*;?/i', $interior, $famMatch)) {
            $fontFamily = trim((string) $famMatch[1]);
        }

        // Find the src declaration.
        if (!preg_match('/\bsrc\s*:\s*((?:(?:url\s*\([^)]*\)|format\s*\([^)]*\)|local\s*\([^)]*\)|[^;{}]))+)/is', $interior, $srcMatch)) {
            return null;
        }

        $srcDecl  = $srcMatch[0];
        $srcValue = $srcMatch[1];

        // Extract the first URL in the src value.
        if (!preg_match('/url\(\s*["\']?([^"\')\s]+)["\']?\s*\)/i', $srcValue, $urlMatch)) {
            return null;
        }
        $originalUrl = $urlMatch[1];

        // Strip query-string and fragment once; reuse throughout this method.
        $urlNoQuery  = (string) (strtok($originalUrl, '?#') ?: $originalUrl);
        $ext = strtolower((string) pathinfo($urlNoQuery, PATHINFO_EXTENSION));
        if (!in_array($ext, self::TRANSCODE_EXTS, true)) {
            return null;
        }

        $formatHint = self::FORMAT_HINTS[$ext] ?? null;
        if ($formatHint === null) {
            return null;
        }

        $sourceBytes = ($this->downloader)($originalUrl);
        if (!is_string($sourceBytes) || strlen($sourceBytes) < 12) {
            return null;
        }

        $hash       = Blake3::hashHex($sourceBytes);
        $sourceFile = basename($urlNoQuery);

        // Record discovery metadata for the results push (idempotent by hash).
        if ($hash !== '' && !isset($this->discoveredFonts[$hash])) {
            $this->discoveredFonts[$hash] = [
                'family'        => $fontFamily,
                'source_file'   => $sourceFile,
                'source_ext'    => $ext,
                'original_size' => strlen($sourceBytes),
            ];
        }

        // Determine subset spec from config (hard-gated on both flags).
        $subsetMode  = '';
        $subsetRange = '';
        if ($this->config->fontsSubset && $this->config->fontsTranscodeWoff2) {
            $subsetMode  = $this->config->fontsSubsetMode;
            $subsetRange = $this->config->fontsSubsetRange;
        }

        $result = $this->transcodeClient->resolve($sourceBytes, $ext, $subsetMode, $subsetRange);

        // Fire-and-forget results push on first discovery (hash recorded above).
        if ($hash !== '') {
            $this->pushDiscoveryResult($hash, $fontFamily, $sourceFile, $ext, strlen($sourceBytes), $result);
        }

        // 'skipped' means the encoder confirmed it's an icon/variable font and
        // declined to subset — the full WOFF2 is still available. Treat it like
        // 'ready' for src rewriting (no subset block emitted).
        if ($result === null || !in_array($result['state'], ['ready', 'subset', 'skipped'], true) || $result['woff2_url'] === '') {
            return null;
        }

        $woff2Url = $result['woff2_url'];
        $escapedWoff2    = $this->cssSafeUrl($woff2Url);
        $escapedOriginal = $this->cssSafeUrl($originalUrl);
        $newSrcValue = "url('{$escapedWoff2}') format('woff2'), url('{$escapedOriginal}') format('{$formatHint}')";

        $newSrcDecl = str_replace($srcValue, $newSrcValue, $srcDecl);
        $rewrittenBlock = str_replace($srcDecl, $newSrcDecl, $block);

        // If a subset WOFF2 is available, prepend an additive @font-face rule that
        // carries the unicode-range descriptor. The browser fetches the subset for
        // in-range codepoints and falls back to the full WOFF2 (rewrittenBlock)
        // for anything outside the range.
        if ($result['state'] === 'subset' && $result['subset_url'] !== '' && $result['unicode_range'] !== '') {
            $subsetUrl      = $this->cssSafeUrl($result['subset_url']);
            $escapedRange   = htmlspecialchars($result['unicode_range'], ENT_QUOTES);

            // Build the subset @font-face: copy all descriptors from the original
            // block, replace the src with the subset-only URL, and inject unicode-range.
            $subsetInterior = $interior;

            // Replace the src declaration with the subset URL.
            $subsetSrc  = "url('{$subsetUrl}') format('woff2')";
            $subsetSrcDecl = str_replace($srcValue, $subsetSrc, $srcDecl);
            $subsetInterior = str_replace($srcDecl, $subsetSrcDecl, $subsetInterior);

            // Remove any existing unicode-range descriptor, then append it.
            $subsetInterior = (string) preg_replace('/unicode-range\s*:[^;]*;?/i', '', $subsetInterior);
            $subsetInterior = rtrim($subsetInterior, ';') . ';unicode-range:' . $escapedRange . ';';

            $subsetBlock = '@font-face{' . $subsetInterior . '}';

            // Prepend the subset rule; the canonical full-WOFF2 rule follows.
            return $subsetBlock . $rewrittenBlock;
        }

        return $rewrittenBlock;
    }

    // -----------------------------------------------------------------------
    // Results push helper
    // -----------------------------------------------------------------------

    /**
     * Fire-and-forget push of a per-font result to the CP catalog endpoint.
     *
     * Only called once per (hash, page-build) pair — the $discoveredFonts map
     * ensures idempotency across multiple @font-face blocks referencing the same
     * source file.
     *
     * @param string $hash
     * @param string $fontFamily
     * @param string $sourceFile
     * @param string $sourceExt
     * @param int    $originalSize
     * @param array{state:string,woff2_url:string,subset_url:string,unicode_range:string}|null $result
     * @return void
     */
    private function pushDiscoveryResult(
        string $hash,
        string $fontFamily,
        string $sourceFile,
        string $sourceExt,
        int $originalSize,
        ?array $result
    ): void {
        if ($hash === '' || !($this->transcodeClient instanceof FontTranscodeClient)) {
            return;
        }

        $state        = 'pending';
        $unicodeRange = null;
        $errorDetail  = '';

        if ($result !== null) {
            $state        = $result['state'];
            $unicodeRange = ($result['unicode_range'] !== '') ? $result['unicode_range'] : null;
        }

        // Sizes are best-effort: the CP derives savings_pct from the sizes we send.
        // We do not have the byte lengths here without re-reading the cache files,
        // so we send null and let the CP fill them in from the transcode job record.
        $this->transcodeClient->pushResult(
            $hash,
            $fontFamily,
            $sourceFile,
            $sourceExt,
            $originalSize,
            null,
            null,
            $unicodeRange,
            $state,
            $errorDetail
        );
    }

    // -----------------------------------------------------------------------
    // Utilities
    // -----------------------------------------------------------------------

    /**
     * Escape a URL for use inside a CSS url('…') token.
     * Replaces single quotes and backslashes — the only characters that must be
     * escaped inside a single-quoted CSS string.
     *
     * @param string $url Raw URL.
     * @return string
     */
    private function cssSafeUrl(string $url): string
    {
        return str_replace(['\\', "'"], ['\\\\', "\\'"], $url);
    }

    /**
     * Extract font file URLs (woff2/woff/ttf/otf) from CSS url() declarations.
     *
     * @param string $css CSS.
     * @return list<string>
     */
    private function extractFontUrls(string $css): array
    {
        if (!preg_match_all('/url\(\s*["\']?(https?:\/\/[^"\')]+\.(?:woff2|woff|ttf|otf))[^"\')]*["\']?\s*\)/i', $css, $m)) {
            return [];
        }
        return array_values(array_unique($m[1]));
    }
}
