<?php
/**
 * RUM injector + PerfConfig coercion tests.
 *
 * GH #154: the RUM collector used to be injected only inside the optimizer's
 * cache-write output buffer (Optimizer stage 11), which never ran when WPmgr
 * page caching was off (the norm when a third-party page cache serves the
 * site) or on a third-party cache HIT — so RUM silently collected zero data.
 * RUM is now injected by RumInjector::renderHead(), bound to
 * `wp_enqueue_scripts` independent of the optimizer/cache pipeline. These
 * tests cover both the injector behaviour and the two core regression
 * proofs: (1) a RUM-only config no longer activates the optimizer buffer,
 * and (2) the beacon still enqueues when the optimizer/page-cache is fully
 * disabled.
 *
 * 2026-07 wp.org review fix: RumInjector used to `echo` raw <script> tags
 * from a `wp_head` priority-99 callback (with two now-deleted
 * `NonEnqueuedScript` phpcs:ignore suppressions and a docblock claiming
 * WP's enqueue API was "inapplicable at this late priority" -- which was
 * incorrect: `wp_enqueue_scripts` itself fires from inside `wp_head` at
 * priority 1, before any priority-99 callback). It now calls
 * wp_enqueue_script() + wp_add_inline_script() instead. These tests mock
 * both functions (plus wp_version/add_filter for the WP<6.3 async fallback)
 * and assert on the CALLS made rather than on echoed markup.
 *
 * Covered invariants:
 *   1. rumEnabled defaults to false.
 *   2. rumSampleRate defaults to 1.0 and clamps to [0,1].
 *   3. rumBeaconKey round-trips through constructor and toArray().
 *   4. rumIngestUrl is derived from the CP URL option when not pushed.
 *   5. rumIngestUrl is used as-is when pushed by the CP.
 *   6. anyHtmlTransformEnabled() returns FALSE when ONLY rumEnabled is on
 *      (RUM no longer rides the optimizer buffer — core regression proof).
 *   7. anyHtmlTransformEnabled() returns false when rumEnabled is off and all others off.
 *   8. PerfConfig round-trips RUM fields through toArray() -> constructor.
 *   9. RumInjector::renderHead(): flag OFF => nothing enqueued.
 *  10. RumInjector::renderHead(): flag ON, empty key => nothing enqueued.
 *  11. RumInjector::renderHead(): flag ON, empty url => nothing enqueued.
 *  12. RumInjector::renderHead(): valid config, anon/GET/200 => enqueues the
 *      collector script + inline config, in that dependency order.
 *  13. RumInjector::renderHead(): enqueues EXACTLY ONCE across two invocations
 *      (static once-per-request guard).
 *  14. RumInjector::renderHead(): sample_rate is clamped to [0,1] in the JSON config.
 *  15. RumInjector::renderHead(): CSP with nonce and without unsafe-inline => skip.
 *  16. RumInjector::renderHead(): CSP with unsafe-inline => allow.
 *  17. RumInjector::renderHead(): is_user_logged_in() true => skip (anonymous-only guard).
 *  18. RumInjector::renderHead(): REQUEST_METHOD POST => skip (GET-only guard).
 *  19. RumInjector::renderHead(): is_404() true => skip (200-only guard).
 *  20. RumInjector::renderHead(): post_password_required() true => skip.
 *  21. RumInjector::renderHead(): still enqueues when the optimizer/page-cache is
 *      fully disabled (core regression proof — RUM no longer requires
 *      CacheConfig->enabled or PerfConfig->anyHtmlTransformEnabled()).
 *  22. Optimizer::isActive() is false when ONLY rumEnabled is on.
 *  23. Optimizer::run() never emits the RUM marker even when rumEnabled is on
 *      (proves the removed stage 11 is gone; RumInjector is not called from
 *      the pipeline at all).
 *  24. RumInjector::renderHead(): a hostile key/url containing `</script>` and
 *      `<!--<script>` is JSON_HEX_*-escaped so the emitted inline script config
 *      can never be broken out of (script-sink hardening regression guard).
 *  25. On WP >= 6.3, wp_enqueue_script() is called with the array
 *      in_footer/strategy=async form and no script_loader_tag filter is
 *      registered.
 *  26. On WP < 6.3 (or unknown), wp_enqueue_script() is called with a plain
 *      bool(false) in_footer and the script_loader_tag async fallback filter
 *      IS registered.
 *  27. RumInjector::filterAsyncScriptTag() adds `async` to this plugin's own
 *      handle's tag, leaves other handles' tags untouched, and does not
 *      double up `async` if already present.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Optimizer\Optimizer;
use WPMgr\Agent\Optimizer\PerfConfig;
use WPMgr\Agent\Optimizer\RumInjector;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Optimizer\PerfConfig
 * @covers \WPMgr\Agent\Optimizer\RumInjector
 * @covers \WPMgr\Agent\Optimizer\Optimizer
 */
final class OptimizerRumTest extends TestCase
{
    private const BASIC_DOC = '<!DOCTYPE html><html><head></head><body><p>Hello</p></body></html>';

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<string,mixed> Saved $_SERVER to restore in tear_down(). */
    private array $savedServer = [];

    /** @var list<array{handle:string,src:string,deps:mixed,ver:mixed,args:mixed}> */
    private array $enqueuedScripts = [];

    /** @var list<array{handle:string,data:string,position:string}> */
    private array $inlineScripts = [];

    /** @var list<array{hook:string,callback:mixed,priority:int}> */
    private array $registeredFilters = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->savedServer = $_SERVER;
        $_SERVER['REQUEST_METHOD'] = 'GET';

        $this->enqueuedScripts   = [];
        $this->inlineScripts     = [];
        $this->registeredFilters = [];

        $this->options = [];
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->options[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->options[$k] = $v;
            return true;
        });
        Functions\when('plugins_url')->alias(function (string $path, string $file): string {
            return 'https://example.com/wp-content/plugins/wpmgr-agent/' . ltrim($path, '/');
        });
        // Real wp_json_encode($data, $options, $depth) forwards $options to
        // json_encode() verbatim — the mock must do the same so tests actually
        // exercise the JSON_HEX_* hardening flags RumInjector passes.
        Functions\when('wp_json_encode')->alias(fn ($v, $options = 0, $depth = 512) => (string) json_encode($v, $options, $depth));
        Functions\when('esc_url')->alias(fn ($u) => $u);
        Functions\when('headers_list')->justReturn([]);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('is_singular')->justReturn(false);
        Functions\when('is_404')->justReturn(false);
        Functions\when('post_password_required')->justReturn(false);
        Functions\when('site_url')->justReturn('https://example.com');
        Functions\when('home_url')->justReturn('https://example.com');
        Functions\when('get_site_option')->alias(fn ($k, $d = false) => $d);

        Functions\when('wp_enqueue_script')->alias(function ($handle, $src = '', $deps = [], $ver = false, $args = false) {
            $this->enqueuedScripts[] = ['handle' => $handle, 'src' => $src, 'deps' => $deps, 'ver' => $ver, 'args' => $args];
            return true;
        });
        Functions\when('wp_add_inline_script')->alias(function ($handle, $data, $position = 'after') {
            $this->inlineScripts[] = ['handle' => $handle, 'data' => $data, 'position' => $position];
            return true;
        });
        Functions\when('add_filter')->alias(function ($hook, $callback, $priority = 10, $acceptedArgs = 1) {
            $this->registeredFilters[] = ['hook' => $hook, 'callback' => $callback, 'priority' => $priority];
            return true;
        });

        // Unset by default: coreSupportsScriptStrategy() must treat an
        // unknown version as WP<6.3 (the safe default).
        unset($GLOBALS['wp_version']);

        // The once-per-request static guard must not leak between tests.
        $this->resetRumEmitted();

        if (!defined('WPMGR_AGENT_FILE')) {
            define('WPMGR_AGENT_FILE', '/path/to/wpmgr-agent.php');
        }
    }

    protected function tear_down(): void
    {
        $_SERVER = $this->savedServer;
        unset($GLOBALS['wp_version']);
        $this->resetRumEmitted();
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Reset RumInjector's private static once-per-request guard via
     * reflection (matches the codebase's established static-reset pattern;
     * see ObjectCacheCooldownTest).
     *
     * @return void
     */
    private function resetRumEmitted(): void
    {
        $ref  = new \ReflectionClass(RumInjector::class);
        $prop = $ref->getProperty('emitted');
        $prop->setValue(null, false);
    }

    private function makeConfig(array $overrides = []): PerfConfig
    {
        return new PerfConfig(array_merge([
            'rum_enabled'     => true,
            'rum_sample_rate' => 1.0,
            'rum_beacon_key'  => 'TESTBEACONKEY',
            'rum_ingest_url'  => 'https://cp.example.com/rum/ingest',
        ], $overrides));
    }

    private function render(PerfConfig $config): void
    {
        (new RumInjector($config))->renderHead();
    }

    /**
     * @return array{handle:string,src:string,deps:mixed,ver:mixed,args:mixed}|null
     */
    private function enqueuedRumScript(): ?array
    {
        foreach ($this->enqueuedScripts as $call) {
            if ($call['handle'] === RumInjector::SCRIPT_HANDLE) {
                return $call;
            }
        }
        return null;
    }

    /**
     * @return array{handle:string,data:string,position:string}|null
     */
    private function inlineRumScript(): ?array
    {
        foreach ($this->inlineScripts as $call) {
            if ($call['handle'] === RumInjector::SCRIPT_HANDLE) {
                return $call;
            }
        }
        return null;
    }

    // ---- PerfConfig coercion tests ----

    public function test_rum_enabled_defaults_false(): void
    {
        $c = new PerfConfig([]);
        $this->assertFalse($c->rumEnabled);
    }

    public function test_rum_sample_rate_defaults_to_one(): void
    {
        $c = new PerfConfig([]);
        $this->assertSame(1.0, $c->rumSampleRate);
    }

    public function test_rum_sample_rate_clamped_below_zero(): void
    {
        $c = new PerfConfig(['rum_sample_rate' => -0.5]);
        $this->assertSame(0.0, $c->rumSampleRate);
    }

    public function test_rum_sample_rate_clamped_above_one(): void
    {
        $c = new PerfConfig(['rum_sample_rate' => 2.5]);
        $this->assertSame(1.0, $c->rumSampleRate);
    }

    public function test_rum_beacon_key_round_trips(): void
    {
        $c = new PerfConfig(['rum_beacon_key' => 'TESTKEY123456']);
        $this->assertSame('TESTKEY123456', $c->rumBeaconKey);
    }

    public function test_rum_beacon_key_defaults_empty(): void
    {
        $c = new PerfConfig([]);
        $this->assertSame('', $c->rumBeaconKey);
    }

    public function test_rum_ingest_url_uses_pushed_value(): void
    {
        $c = new PerfConfig(['rum_ingest_url' => 'https://cp.example.com/rum/ingest']);
        $this->assertSame('https://cp.example.com/rum/ingest', $c->rumIngestUrl);
    }

    public function test_rum_ingest_url_derived_from_cp_option(): void
    {
        $this->options['wpmgr_agent_cp_url'] = 'https://manage.wpmgr.app';
        $c = new PerfConfig([]);
        $this->assertSame('https://manage.wpmgr.app/rum/ingest', $c->rumIngestUrl);
    }

    public function test_rum_ingest_url_empty_when_no_cp_url(): void
    {
        $c = new PerfConfig([]);
        $this->assertSame('', $c->rumIngestUrl);
    }

    public function test_any_html_transform_disabled_when_only_rum_on(): void
    {
        // Core regression proof: RUM no longer rides the optimizer buffer, so
        // a RUM-only config must NOT report any HTML transform as enabled.
        $c = new PerfConfig(['rum_enabled' => true, 'rum_beacon_key' => 'key']);
        $this->assertFalse($c->anyHtmlTransformEnabled());
    }

    public function test_any_html_transform_disabled_when_rum_off_and_all_others_off(): void
    {
        $c = new PerfConfig([]);
        $this->assertFalse($c->anyHtmlTransformEnabled());
    }

    public function test_perf_config_rum_round_trips_via_to_array(): void
    {
        $data = [
            'rum_enabled'     => true,
            'rum_sample_rate' => 0.5,
            'rum_beacon_key'  => 'MYROUNDTRIPKEY',
            'rum_ingest_url'  => 'https://cp.example.com/rum/ingest',
        ];
        $c1   = new PerfConfig($data);
        $c2   = new PerfConfig($c1->toArray());

        $this->assertTrue($c2->rumEnabled);
        $this->assertSame(0.5, $c2->rumSampleRate);
        $this->assertSame('MYROUNDTRIPKEY', $c2->rumBeaconKey);
        $this->assertSame('https://cp.example.com/rum/ingest', $c2->rumIngestUrl);
    }

    // ---- RumInjector::renderHead() tests ----

    public function test_render_noop_when_rum_disabled(): void
    {
        $c = new PerfConfig(['rum_enabled' => false]);
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts);
        $this->assertSame([], $this->inlineScripts);
    }

    public function test_render_noop_when_key_empty(): void
    {
        $c = $this->makeConfig(['rum_beacon_key' => '']);
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts);
    }

    public function test_render_noop_when_url_empty(): void
    {
        $c = $this->makeConfig(['rum_ingest_url' => '']);
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts);
    }

    public function test_render_enqueues_collector_script_and_inline_config(): void
    {
        $c = $this->makeConfig();
        $this->render($c);

        $script = $this->enqueuedRumScript();
        $this->assertNotNull($script, 'the collector script must be enqueued');
        $this->assertStringContainsString('wpmgr-rum.min.js', $script['src']);

        $inline = $this->inlineRumScript();
        $this->assertNotNull($inline, 'the config must be added as an inline script');
        $this->assertSame('before', $inline['position'], 'config must print BEFORE the collector script');
        $this->assertStringContainsString('window.__WPMGR_RUM__', $inline['data']);
        $this->assertStringContainsString('"TESTBEACONKEY"', $inline['data']);
        $this->assertStringContainsString('cp.example.com', $inline['data']);
    }

    public function test_render_enqueues_exactly_once_across_two_invocations(): void
    {
        $c = $this->makeConfig();

        $this->render($c);
        $this->render($c);

        $this->assertCount(1, $this->enqueuedScripts, 'a second wp_enqueue_scripts invocation in the same request must be a no-op');
        $this->assertCount(1, $this->inlineScripts);
    }

    public function test_render_sample_rate_clamped_in_json(): void
    {
        $c = $this->makeConfig(['rum_sample_rate' => 2.0]);
        $this->render($c);

        $inline = $this->inlineRumScript();
        $this->assertNotNull($inline);
        $this->assertStringContainsString('"rate":1', $inline['data']);
    }

    /**
     * Script-sink hardening regression guard. $key/$url are CP-controlled
     * today (not reachable by end-user input), but the config JSON must apply
     * JSON_HEX_TAG|JSON_HEX_AMP|JSON_HEX_APOS|JSON_HEX_QUOT regardless, as
     * defense-in-depth for an inline <script> sink (wp_add_inline_script()
     * does not itself escape its $data argument).
     *
     * The hostile payload deliberately contains NO forward slash, so this
     * test exercises the JSON_HEX_* flags specifically rather than PHP's
     * unrelated default json_encode() slash-escaping (which would otherwise
     * mask a missing JSON_HEX_TAG for a `</script>`-shaped payload).
     */
    public function test_render_escapes_hostile_key_and_url_against_script_breakout(): void
    {
        $hostileKey = 'K<!--<script>alert(1)';
        $hostileUrl = 'https://cp.example.com/rum<script>alert(2)';

        $c = $this->makeConfig([
            'rum_beacon_key' => $hostileKey,
            'rum_ingest_url' => $hostileUrl,
        ]);
        $this->render($c);

        $inline = $this->inlineRumScript();
        $this->assertNotNull($inline);
        $out = $inline['data'];

        // No raw "<!--" or "<script" from the hostile payload may survive
        // into the emitted inline script -- the hostile bytes come only
        // from $key/$url, never from a static PHP string literal.
        $this->assertStringNotContainsString('<!--', $out, 'Hostile key/url must not inject a raw HTML comment open');
        $this->assertStringNotContainsString('<script>alert', $out, 'Hostile key/url must not inject a raw second <script> tag');

        // json_encode() with JSON_HEX_TAG emits the 6-character escape
        // sequences backslash-u-0-0-3-C and backslash-u-0-0-3-E in place of
        // the raw "<" / ">" characters. Build the expected needles from
        // chr(92) (backslash) so this source file never contains a literal
        // "<"/">" token that a tool/editor could silently decode.
        $bs      = chr(92);
        $escLt   = $bs . 'u003C';
        $escGt   = $bs . 'u003E';
        $this->assertStringContainsString($escLt, $out, 'Hex-escaped "<" must be present in the JSON payload');
        $this->assertStringContainsString($escGt, $out, 'Hex-escaped ">" must be present in the JSON payload');
    }

    public function test_render_skips_when_strict_nonce_csp_present(): void
    {
        Functions\when('headers_list')->justReturn([
            "Content-Security-Policy: default-src 'self'; script-src 'nonce-abc123'",
        ]);

        $c = $this->makeConfig();
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts, 'Should skip injection on strict nonce CSP');
    }

    public function test_render_allows_when_csp_has_unsafe_inline(): void
    {
        Functions\when('headers_list')->justReturn([
            "Content-Security-Policy: script-src 'nonce-abc123' 'unsafe-inline'",
        ]);

        $c = $this->makeConfig();
        $this->render($c);
        $this->assertNotNull($this->enqueuedRumScript());
    }

    public function test_render_skips_when_user_logged_in(): void
    {
        Functions\when('is_user_logged_in')->justReturn(true);

        $c = $this->makeConfig();
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts, 'Anonymous-only guard: must not beacon a logged-in visitor');
    }

    public function test_render_skips_on_post_request(): void
    {
        $_SERVER['REQUEST_METHOD'] = 'POST';

        $c = $this->makeConfig();
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts, 'GET-only guard: firing during a POST must not beacon');
    }

    public function test_render_skips_on_404(): void
    {
        Functions\when('is_404')->justReturn(true);

        $c = $this->makeConfig();
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts, '200-only guard: must not beacon a 404 response');
    }

    public function test_render_skips_on_password_protected(): void
    {
        Functions\when('post_password_required')->justReturn(true);

        $c = $this->makeConfig();
        $this->render($c);
        $this->assertSame([], $this->enqueuedScripts, 'Password-protected content must not beacon');
    }

    /**
     * Core regression proof: the beacon must enqueue even when the optimizer
     * (and therefore any WPMgr page-cache buffer) is completely disabled.
     * This is the GH #154 fix — RUM no longer requires
     * PerfConfig::anyHtmlTransformEnabled() / CacheConfig->enabled to be true.
     */
    public function test_render_works_with_optimizer_and_page_cache_disabled(): void
    {
        $c = $this->makeConfig();

        // Prove the optimizer pipeline is completely inert for this config —
        // i.e. no WPMgr cache/optimizer buffer would ever have opened.
        $this->assertFalse($c->anyHtmlTransformEnabled(), 'Optimizer must be inert; RUM must not depend on it');
        $opt = new Optimizer($c);
        $this->assertFalse($opt->isActive());

        // Yet the wp_enqueue_scripts-bound RUM injector still enqueues.
        $this->render($c);
        $this->assertNotNull($this->enqueuedRumScript());
        $this->assertNotNull($this->inlineRumScript());
    }

    // ---- WP<6.3 async fallback tests (2026-07 wp.org review fix) ----

    public function test_render_on_wp63_plus_uses_strategy_array_and_registers_no_fallback_filter(): void
    {
        $GLOBALS['wp_version'] = '6.4-alpha';

        $c = $this->makeConfig();
        $this->render($c);

        $script = $this->enqueuedRumScript();
        $this->assertNotNull($script);
        $this->assertIsArray($script['args'], 'WP 6.3+ must use the array in_footer/strategy form');
        $this->assertSame(false, $script['args']['in_footer'] ?? null);
        $this->assertSame('async', $script['args']['strategy'] ?? null);

        $this->assertSame(
            [],
            array_filter($this->registeredFilters, static fn ($f) => $f['hook'] === 'script_loader_tag'),
            'WP 6.3+ must not need the script_loader_tag async fallback'
        );
    }

    public function test_render_on_wp62_uses_bool_infooter_and_registers_fallback_filter(): void
    {
        $GLOBALS['wp_version'] = '6.2.2';

        $c = $this->makeConfig();
        $this->render($c);

        $script = $this->enqueuedRumScript();
        $this->assertNotNull($script);
        $this->assertSame(
            false,
            $script['args'],
            'WP<6.3 must pass a plain bool(false) in_footer, never an array (which PHP would coerce to true)'
        );

        $fallback = array_values(array_filter(
            $this->registeredFilters,
            static fn ($f) => $f['hook'] === 'script_loader_tag'
        ));
        $this->assertNotEmpty($fallback, 'WP<6.3 must register the script_loader_tag async fallback filter');
        $this->assertSame([RumInjector::class, 'filterAsyncScriptTag'], $fallback[0]['callback']);
    }

    public function test_render_with_unknown_wp_version_defaults_to_wp62_safe_path(): void
    {
        // $GLOBALS['wp_version'] is unset in set_up(); coreSupportsScriptStrategy()
        // must default to the safe (bool + filter) path rather than risk the
        // footer-forcing array coercion on an unrecognized/older core.
        $c = $this->makeConfig();
        $this->render($c);

        $script = $this->enqueuedRumScript();
        $this->assertNotNull($script);
        $this->assertSame(false, $script['args']);
    }

    public function test_filter_async_script_tag_adds_async_to_own_handle(): void
    {
        $tag = '<script src="https://example.com/wp-content/plugins/wpmgr-agent/assets/wpmgr-rum.min.js?ver=1.0.0" id="wpmgr-rum-js"></script>';

        $out = RumInjector::filterAsyncScriptTag($tag, RumInjector::SCRIPT_HANDLE);

        $this->assertStringContainsString('async src=', $out);
    }

    public function test_filter_async_script_tag_leaves_other_handles_untouched(): void
    {
        $tag = '<script src="https://example.com/some-other-plugin.js" id="some-other-plugin-js"></script>';

        $out = RumInjector::filterAsyncScriptTag($tag, 'some-other-plugin');

        $this->assertSame($tag, $out);
    }

    public function test_filter_async_script_tag_does_not_double_up_async(): void
    {
        $tag = '<script async src="https://example.com/wp-content/plugins/wpmgr-agent/assets/wpmgr-rum.min.js" id="wpmgr-rum-js"></script>';

        $out = RumInjector::filterAsyncScriptTag($tag, RumInjector::SCRIPT_HANDLE);

        $this->assertSame($tag, $out, 'must not double up async if already present (e.g. a future core also adding it)');
        $this->assertSame(1, substr_count($out, 'async'));
    }

    // ---- Optimizer pipeline tests (stage 11 removal) ----

    public function test_optimizer_is_not_active_when_only_rum_enabled(): void
    {
        $config = new PerfConfig([
            'rum_enabled'     => true,
            'rum_sample_rate' => 1.0,
            'rum_beacon_key'  => 'PIPELINEKEY',
            'rum_ingest_url'  => 'https://cp.example.com/rum/ingest',
        ]);

        $opt = new Optimizer($config);
        $this->assertFalse($opt->isActive(), 'A RUM-only config must not activate the optimizer buffer');
    }

    public function test_optimizer_run_never_emits_rum_marker_even_when_rum_enabled(): void
    {
        // Even if some OTHER transform is also on (so the pipeline actually
        // runs), the RUM marker must never appear in the optimizer's output —
        // stage 11 is gone; RumInjector is reached only via wp_enqueue_scripts now.
        $config = new PerfConfig([
            'cache_link_prefetch' => true,
            'rum_enabled'         => true,
            'rum_sample_rate'     => 1.0,
            'rum_beacon_key'      => 'PIPELINEKEY',
            'rum_ingest_url'      => 'https://cp.example.com/rum/ingest',
        ]);

        $opt = new Optimizer($config);
        $this->assertTrue($opt->isActive());

        $out = $opt->run(self::BASIC_DOC);
        $this->assertStringNotContainsString('data-wpmgr-rum-config', $out);
        $this->assertStringNotContainsString('wpmgr-rum.min.js', $out);
    }

    public function test_optimizer_noop_rum_when_disabled(): void
    {
        $config = new PerfConfig([]);
        $opt    = new Optimizer($config);
        $this->assertFalse($opt->isActive());
        $out = $opt->run(self::BASIC_DOC);
        $this->assertStringNotContainsString('data-wpmgr-rum-config', $out);
    }
}
