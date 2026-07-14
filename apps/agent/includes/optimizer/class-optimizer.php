<?php
/**
 * Optimizer — the ob_start orchestrator for the optimization layer (Phase 4).
 *
 * Runs INSIDE the Phase-3 cache-writer's MISS path: on a cacheable miss the
 * fully-rendered HTML is passed through run() BEFORE the writer gzip-compresses
 * and writes it to disk, so the optimized bytes are what get cached AND what the
 * live visitor receives. The transform order is fixed (the same pipeline order
 * the reference notes documents as standard):
 *
 *   1. Font          display-swap + Google self-host + preload
 *   2. CSS minify    + self-host third-party CSS
 *   3. CSS RUCSS     remove-unused-css via the CP (GRACEFUL skip on any failure)
 *   4. JS minify     + self-host third-party JS
 *   5. Images        width/height + lazy + fetchpriority (+ srcset preserved)
 *   6. IFrame        YouTube facade
 *   7. Gravatar      self-host avatars
 *   8. JS delay      data-src rewrite + inject the delay runtime
 *   9. Speculation   prefetch rules
 *  10. CDN rewrite   asset URLs -> CDN host
 *
 * RUM (Real User Monitoring) beacon injection is NOT a stage in this pipeline
 * (GH #154): it used to run here as a final stage, but that meant the collector
 * was only ever injected inside THIS cache-writer output buffer — so a site
 * with WPMgr page caching off (the norm when a third-party page cache serves
 * the site), or a page served from a third-party cache HIT, never received the
 * collector at all and RUM silently collected zero data. RUM is now enqueued
 * by RumInjector::renderHead(), bound directly to the `wp_enqueue_scripts`
 * action (Plugin::registerRumHooks()), independent of this optimizer/cache
 * pipeline entirely.
 *
 * Every transform is config-gated and a no-op when its flag is off. The whole
 * run() is wrapped so a transform failure can never corrupt or drop the page —
 * on any \Throwable it returns the HTML as it was before that stage. RUCSS in
 * particular is guaranteed never to throw to this path (see RucssClient).
 *
 * Only full HTML documents are optimized; non-HTML buffers pass through
 * untouched. Logged-in responses are skipped (personalised; not safe to cache-
 * optimize the anonymous shape).
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

use WPMgr\Agent\Cache\PerfReporter;
use WPMgr\Agent\Cache\WooFragmentsRuntime;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\DebugLog;

/**
 * Orchestrates the request-path HTML optimization pipeline.
 */
final class Optimizer
{
    private PerfConfig $config;

    /** @var Signer|null Lazily built for the RUCSS client. */
    private ?Signer $signer;

    /** @var Settings|null Lazily built for the RUCSS client. */
    private ?Settings $settings;

    /**
     * Whether the LAST run()'s RUCSS stage saw a genuine "processing" cache miss
     * (used-CSS is being computed and WILL land later). The cache writer reads
     * this via {@see rucssPending()} to DEFER caching this optimization-incomplete
     * render — so the static fast-path never serves the un-optimized page until a
     * purge. Only set when css_rucss is on AND the CP returned a 202 processing.
     */
    private bool $rucssPending = false;

    /**
     * @param PerfConfig|null $config   Optimization config (default: loaded).
     * @param Signer|null     $signer   Request signer for RUCSS (default: built).
     * @param Settings|null   $settings Enrollment source for RUCSS (default: built).
     */
    public function __construct(?PerfConfig $config = null, ?Signer $signer = null, ?Settings $settings = null)
    {
        $this->config   = $config ?? PerfConfig::load();
        $this->signer   = $signer;
        $this->settings = $settings;
    }

    /**
     * Whether the optimizer has any request-path work to do. The cache writer
     * checks this to skip the whole pipeline (and its DOM scans) on an inert
     * site.
     *
     * @return bool
     */
    public function isActive(): bool
    {
        return $this->config->anyHtmlTransformEnabled();
    }

    /**
     * Run the full pipeline over a rendered page.
     *
     * @param string $html The fully-rendered HTML document.
     * @return string Optimized HTML (or the input on a non-HTML / inert / failed pass).
     */
    public function run(string $html): string
    {
        $this->rucssPending = false;
        if (!$this->config->anyHtmlTransformEnabled()) {
            return $html;
        }
        // Only operate on full HTML documents (mirrors the cacheability gate).
        if (!preg_match('/<!DOCTYPE\s+html|<html[\s>]/i', $html)) {
            return $html;
        }
        // Skip personalised (logged-in) responses — those keep their full,
        // un-deferred CSS/JS so admin UI never breaks.
        if (function_exists('is_user_logged_in') && is_user_logged_in()) {
            return $html;
        }
        // Per-page optimization opt-out via post meta.
        if (function_exists('is_singular') && is_singular()
            && function_exists('get_queried_object_id') && function_exists('get_post_meta')
        ) {
            if (get_post_meta((int) get_queried_object_id(), '_wpmgr_no_optimize', true) === '1') {
                return $html;
            }
        }

        // 1. Fonts.
        if ($this->config->fontsDisplaySwap || $this->config->fontsOptimizeGoogle || $this->config->fontsPreload) {
            $html = $this->stage($html, fn (string $h): string => (new Font($this->config))->process($h));
        }

        // 2. CSS minify + self-host.
        if ($this->config->cssJsMinify) {
            $html = $this->stage($html, fn (string $h): string => (new CssMinify())->process($h));
        }
        if ($this->config->selfHostThirdParty) {
            $html = $this->stage($html, fn (string $h): string => $this->selfHostCss($h));
        }

        // 3. CSS RUCSS — graceful: the client itself never throws, but we still
        //    wrap the stage so even a constructor issue can't break render.
        if ($this->config->cssRucss) {
            $html = $this->stage($html, fn (string $h): string => $this->runRucss($h));
        }

        // 4. JS minify + self-host.
        if ($this->config->cssJsMinify) {
            $html = $this->stage($html, fn (string $h): string => (new JsMinify())->process($h));
        }
        if ($this->config->selfHostThirdParty) {
            $html = $this->stage($html, fn (string $h): string => $this->selfHostJs($h));
        }

        // 5. Images.
        if ($this->config->lazyLoad || $this->config->properlySizeImages) {
            $html = $this->stage($html, fn (string $h): string => (new ImagesHtml($this->config))->process($h));
        }

        // 6. IFrame YouTube facade.
        if ($this->config->youtubePlaceholder) {
            $html = $this->stage($html, fn (string $h): string => (new IFrame($this->config))->process($h));
        }

        // 7. Gravatar self-host.
        if ($this->config->selfHostGravatars) {
            $html = $this->stage($html, fn (string $h): string => (new Gravatar($this->config))->process($h));
        }

        // 8. JS delay + runtime.
        if ($this->config->jsDelay) {
            $html = $this->stage($html, fn (string $h): string => (new JsDelay($this->config->jsDelayMethod, $this->config->jsDelayExcludes))->process($h));
        }

        // 8b. WooCommerce cart-fragments JS-delay compatibility shim.
        // Injected only when BOTH the woo_cacheable_session flag is on AND the
        // agent's own probe has confirmed fragment support, AND the JS-delay method
        // is 'interaction' or 'idle' (those two methods re-sequence jQuery events
        // in a way that prevents the native cart-fragments script from firing; the
        // shim replays the ready/load events so it fires correctly). The 'defer'
        // method uses native browser deferral and does not need the shim.
        if ($this->config->wooCacheableSession && $this->config->jsDelay) {
            $wooSupported = (bool) (function_exists('get_option')
                ? get_option(PerfReporter::OPTION_WOO_FRAGMENTS_SUPPORTED, false)
                : false);
            $runtime = new WooFragmentsRuntime($this->config->wooCacheableSession && $wooSupported, $this->config->jsDelayMethod);
            $html = $this->stage($html, fn (string $h): string => $runtime->maybeInject($h));
        }

        // 9. Speculation rules.
        if ($this->config->cacheLinkPrefetch) {
            $html = $this->stage($html, fn (string $h): string => (new SpeculationRules($this->config))->process($h));
        }

        // 10. CDN rewrite (last in the asset pipeline — catches the new local asset URLs).
        if ($this->config->cdn && $this->config->cdnUrl !== '') {
            $html = $this->stage($html, fn (string $h): string => (new CdnRewrite($this->config))->process($h));
        }

        // RUM beacon injection is deliberately NOT a stage here — see the class
        // docblock (GH #154). It is injected via a wp_head action instead, so it
        // no longer requires this optimizer buffer to be active.

        return $html;
    }

    /**
     * Run one transform stage with a hard failure guard: any \Throwable returns
     * the HTML as it entered the stage, so a single broken transform degrades to
     * a no-op instead of breaking the page.
     *
     * @param string                 $html  Input HTML.
     * @param callable(string):string $stage Transform.
     * @return string
     */
    private function stage(string $html, callable $stage): string
    {
        try {
            $out = $stage($html);
            return is_string($out) && $out !== '' ? $out : $html;
        } catch (\Throwable $e) {
            DebugLog::write('wpmgr-agent: optimizer stage degraded (' . $e->getMessage() . ')');
            return $html;
        }
    }

    /**
     * Self-host external stylesheets only.
     *
     * @param string $html HTML.
     * @return string
     */
    private function selfHostCss(string $html): string
    {
        // SelfHost::process does both families; gate so JS is handled in stage 4.
        $selfHost = new SelfHost();
        return $this->selfHostFamily($selfHost, $html, 'css');
    }

    /**
     * Self-host external scripts only.
     *
     * @param string $html HTML.
     * @return string
     */
    private function selfHostJs(string $html): string
    {
        $selfHost = new SelfHost();
        return $this->selfHostFamily($selfHost, $html, 'js');
    }

    /**
     * Invoke SelfHost for a single asset family via its public process() (which
     * handles both); CSS runs in stage 2 and JS in stage 4 of the documented
     * order. Calling process() twice is safe — the second pass finds the first
     * family already localised and skips it.
     *
     * @param SelfHost $selfHost Self-host engine.
     * @param string   $html     HTML.
     * @param string   $family   'css' or 'js' (documentation only).
     * @return string
     */
    private function selfHostFamily(SelfHost $selfHost, string $html, string $family): string
    {
        return $selfHost->process($html);
    }

    /**
     * Run the RUCSS client (which itself guarantees graceful degradation).
     *
     * @param string $html HTML.
     * @return string
     */
    private function runRucss(string $html): string
    {
        try {
            $signer   = $this->signer ?? new Signer(new Keystore());
            $settings = $this->settings ?? new Settings();
        } catch (\Throwable $e) {
            // Could not even build the client — serve full CSS.
            return $html;
        }
        $client = new RucssClient($signer, $settings, $this->config->rucssIncludeSelectors);
        $out    = $client->optimize($html);
        // Record whether used-CSS is still being computed so the cache writer can
        // defer caching this optimization-incomplete render.
        $this->rucssPending = $client->wasPending();
        return $out;
    }

    /**
     * Whether the last run() deferred RUCSS because the CP is still computing the
     * used-CSS (HTTP 202 processing). The cache writer skips persisting such a
     * render so the un-optimized page is never cached; the CP's post-compute
     * re-warm (or the next visit, once used-CSS lands) produces the render that
     * DOES get cached.
     *
     * @return bool
     */
    public function rucssPending(): bool
    {
        return $this->rucssPending;
    }
}
