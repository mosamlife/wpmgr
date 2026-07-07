<?php
/**
 * RUM injector + PerfConfig coercion tests.
 *
 * GH #154: the RUM collector used to be injected only inside the optimizer's
 * cache-write output buffer (Optimizer stage 11), which never ran when WPmgr
 * page caching was off (the norm when a third-party page cache serves the
 * site) or on a third-party cache HIT — so RUM silently collected zero data.
 * RUM is now injected by RumInjector::renderHead(), bound to `wp_head`
 * independent of the optimizer/cache pipeline. These tests cover both the
 * new injector behaviour and the two core regression proofs: (1) a RUM-only
 * config no longer activates the optimizer buffer, and (2) the beacon still
 * renders when the optimizer/page-cache is fully disabled.
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
 *   9. RumInjector::renderHead(): flag OFF => nothing echoed.
 *  10. RumInjector::renderHead(): flag ON, empty key => nothing echoed.
 *  11. RumInjector::renderHead(): flag ON, empty url => nothing echoed.
 *  12. RumInjector::renderHead(): valid config, anon/GET/200 => echoes inline
 *      config + async external script.
 *  13. RumInjector::renderHead(): emits EXACTLY ONCE across two invocations
 *      (static once-per-request guard).
 *  14. RumInjector::renderHead(): sample_rate is clamped to [0,1] in the JSON config.
 *  15. RumInjector::renderHead(): CSP with nonce and without unsafe-inline => skip.
 *  16. RumInjector::renderHead(): CSP with unsafe-inline => allow.
 *  17. RumInjector::renderHead(): is_user_logged_in() true => skip (anonymous-only guard).
 *  18. RumInjector::renderHead(): REQUEST_METHOD POST => skip (GET-only guard).
 *  19. RumInjector::renderHead(): is_404() true => skip (200-only guard).
 *  20. RumInjector::renderHead(): post_password_required() true => skip.
 *  21. RumInjector::renderHead(): still emits when the optimizer/page-cache is
 *      fully disabled (core regression proof — RUM no longer requires
 *      CacheConfig->enabled or PerfConfig->anyHtmlTransformEnabled()).
 *  22. Optimizer::isActive() is false when ONLY rumEnabled is on.
 *  23. Optimizer::run() never emits the RUM marker even when rumEnabled is on
 *      (proves the removed stage 11 is gone; RumInjector is not called from
 *      the pipeline at all).
 *  24. RumInjector::renderHead(): a hostile key/url containing `</script>` and
 *      `<!--<script>` is JSON_HEX_*-escaped so the emitted inline script can
 *      never be broken out of (script-sink hardening regression guard).
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

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->savedServer = $_SERVER;
        $_SERVER['REQUEST_METHOD'] = 'GET';

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

        // The once-per-request static guard must not leak between tests.
        $this->resetRumEmitted();

        if (!defined('WPMGR_AGENT_FILE')) {
            define('WPMGR_AGENT_FILE', '/path/to/wpmgr-agent.php');
        }
    }

    protected function tear_down(): void
    {
        $_SERVER = $this->savedServer;
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

    /**
     * @return string Captured echo output of a renderHead() call.
     */
    private function render(PerfConfig $config): string
    {
        ob_start();
        (new RumInjector($config))->renderHead();
        return (string) ob_get_clean();
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
        $c   = new PerfConfig(['rum_enabled' => false]);
        $out = $this->render($c);
        $this->assertSame('', $out);
    }

    public function test_render_noop_when_key_empty(): void
    {
        $c   = $this->makeConfig(['rum_beacon_key' => '']);
        $out = $this->render($c);
        $this->assertSame('', $out);
    }

    public function test_render_noop_when_url_empty(): void
    {
        $c   = $this->makeConfig(['rum_ingest_url' => '']);
        $out = $this->render($c);
        $this->assertSame('', $out);
    }

    public function test_render_emits_inline_config_and_external_script(): void
    {
        $c   = $this->makeConfig();
        $out = $this->render($c);

        $this->assertStringContainsString('data-wpmgr-rum-config', $out);
        $this->assertStringContainsString('window.__WPMGR_RUM__', $out);
        $this->assertStringContainsString('"TESTBEACONKEY"', $out);
        $this->assertStringContainsString('cp.example.com', $out);

        $this->assertStringContainsString('wpmgr-rum.min.js', $out);
        $this->assertStringContainsString('async', $out);
        $this->assertStringNotContainsString('defer', $out);
    }

    public function test_render_emits_exactly_once_across_two_invocations(): void
    {
        $c = $this->makeConfig();

        $out1 = $this->render($c);
        $out2 = $this->render($c);

        $this->assertStringContainsString('data-wpmgr-rum-config', $out1, 'First wp_head invocation must emit the beacon');
        $this->assertSame('', $out2, 'Second wp_head invocation in the same request must be a no-op');
    }

    public function test_render_sample_rate_clamped_in_json(): void
    {
        $c   = $this->makeConfig(['rum_sample_rate' => 2.0]);
        $out = $this->render($c);
        $this->assertStringContainsString('"rate":1', $out);
    }

    /**
     * Script-sink hardening regression guard. $key/$url are CP-controlled
     * today (not reachable by end-user input), but buildSnippet() must apply
     * JSON_HEX_TAG|JSON_HEX_AMP|JSON_HEX_APOS|JSON_HEX_QUOT to the config
     * JSON regardless, as defense-in-depth for an inline <script> sink.
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

        $c   = $this->makeConfig([
            'rum_beacon_key' => $hostileKey,
            'rum_ingest_url' => $hostileUrl,
        ]);
        $out = $this->render($c);

        $this->assertNotSame('', $out);

        // No raw "<!--" or "<script" from the hostile payload may survive
        // into the emitted HTML — both legitimate <script> tags we construct
        // come from static PHP string literals, never from $key/$url.
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

        $c   = $this->makeConfig();
        $out = $this->render($c);
        $this->assertSame('', $out, 'Should skip injection on strict nonce CSP');
    }

    public function test_render_allows_when_csp_has_unsafe_inline(): void
    {
        Functions\when('headers_list')->justReturn([
            "Content-Security-Policy: script-src 'nonce-abc123' 'unsafe-inline'",
        ]);

        $c   = $this->makeConfig();
        $out = $this->render($c);
        $this->assertStringContainsString('data-wpmgr-rum-config', $out);
    }

    public function test_render_skips_when_user_logged_in(): void
    {
        Functions\when('is_user_logged_in')->justReturn(true);

        $c   = $this->makeConfig();
        $out = $this->render($c);
        $this->assertSame('', $out, 'Anonymous-only guard: must not beacon a logged-in visitor');
    }

    public function test_render_skips_on_post_request(): void
    {
        $_SERVER['REQUEST_METHOD'] = 'POST';

        $c   = $this->makeConfig();
        $out = $this->render($c);
        $this->assertSame('', $out, 'GET-only guard: wp_head firing during a POST must not beacon');
    }

    public function test_render_skips_on_404(): void
    {
        Functions\when('is_404')->justReturn(true);

        $c   = $this->makeConfig();
        $out = $this->render($c);
        $this->assertSame('', $out, '200-only guard: must not beacon a 404 response');
    }

    public function test_render_skips_on_password_protected(): void
    {
        Functions\when('post_password_required')->justReturn(true);

        $c   = $this->makeConfig();
        $out = $this->render($c);
        $this->assertSame('', $out, 'Password-protected content must not beacon');
    }

    /**
     * Core regression proof: the beacon must render even when the optimizer
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

        // Yet the wp_head-bound RUM injector still emits.
        $out = $this->render($c);
        $this->assertStringContainsString('data-wpmgr-rum-config', $out);
        $this->assertStringContainsString('wpmgr-rum.min.js', $out);
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
        // stage 11 is gone; RumInjector is reached only via wp_head now.
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
