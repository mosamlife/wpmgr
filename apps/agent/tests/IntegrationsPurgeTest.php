<?php
/**
 * Host/edge-cache integration tests.
 *
 * Three guarantees:
 *   1. The loader registers every integration on the purge actions on boot.
 *   2. Each integration NO-OPS cleanly when its host is NOT detected — no PHP
 *      error and, critically, no outbound HTTP / host call.
 *   3. For two representative hosts (loopback Varnish + WP Engine) the right
 *      purge mechanism IS invoked with the expected URLs/methods when the host
 *      is stubbed present.
 *
 * Integrations detect their host via the host's own class/function/global, so
 * each test stubs (or withholds) that signal and inspects the recorded calls.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Integrations\Cloudflare;
use WPMgr\Agent\Integrations\CloudPanel;
use WPMgr\Agent\Integrations\Cloudways;
use WPMgr\Agent\Integrations\GridPane;
use WPMgr\Agent\Integrations\Integrations;
use WPMgr\Agent\Integrations\Kinsta;
use WPMgr\Agent\Integrations\RocketNet;
use WPMgr\Agent\Integrations\RunCloud;
use WPMgr\Agent\Integrations\SiteGround;
use WPMgr\Agent\Integrations\SpinupWP;
use WPMgr\Agent\Integrations\Varnish;
use WPMgr\Agent\Integrations\WPCloud;
use WPMgr\Agent\Integrations\WPEngine;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Integrations\Integrations
 * @covers \WPMgr\Agent\Integrations\Integration
 * @covers \WPMgr\Agent\Integrations\CloudPanel
 * @covers \WPMgr\Agent\Integrations\Varnish
 * @covers \WPMgr\Agent\Integrations\WPEngine
 */
final class IntegrationsPurgeTest extends TestCase
{
    /** @var array<string,list<callable>> Captured add_action(hook => callbacks). */
    private array $hooks = [];

    /** @var list<array{string,string,array<string,mixed>}> wp_remote_request calls (url, method, args). */
    private array $http = [];

    /** @var string CloudPanel temp root for this test class. */
    private static string $cloudpanelTempRoot = '';

    /** @var string CloudPanel settings override path for this test class. */
    private static string $cloudpanelSettingsPath = '';

    /** @var string CloudPanel PageSpeed override base path for this test class. */
    private static string $cloudpanelPageSpeedPath = '';

    public static function set_up_before_class(): void
    {
        parent::set_up_before_class();

        self::$cloudpanelTempRoot     = rtrim(sys_get_temp_dir(), '\\/') . '/wpmgr_cloudpanel_' . uniqid('', true);
        self::$cloudpanelSettingsPath = self::$cloudpanelTempRoot . '/settings.json';
        self::$cloudpanelPageSpeedPath = self::$cloudpanelTempRoot . '/pagespeed';

        if (!is_dir(self::$cloudpanelTempRoot)) {
            mkdir(self::$cloudpanelTempRoot, 0755, true);
        }

        if (!defined('WPMGR_CLOUDPANEL_SETTINGS_PATH')) {
            define('WPMGR_CLOUDPANEL_SETTINGS_PATH', self::$cloudpanelSettingsPath);
        }
        if (!defined('WPMGR_CLOUDPANEL_PAGESPEED_PATH')) {
            define('WPMGR_CLOUDPANEL_PAGESPEED_PATH', self::$cloudpanelPageSpeedPath);
        }
    }

    public static function tear_down_after_class(): void
    {
        self::removeCloudPanelPath(self::$cloudpanelTempRoot);
        parent::tear_down_after_class();
    }

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->hooks = [];
        $this->http  = [];
        $this->cleanCloudPanelState();

        // Capture hook registrations so we can both assert them and dispatch.
        Functions\when('add_action')->alias(function (string $hook, $cb, $prio = 10, $args = 1) {
            $this->hooks[$hook][] = $cb;
            return true;
        });

        Functions\when('wp_remote_request')->alias(function ($url, $args = []) {
            $this->http[] = [
                (string) $url,
                (string) ($args['method'] ?? 'GET'),
                is_array($args) ? $args : [],
            ];
            return [];
        });
        Functions\when('wp_remote_post')->alias(function ($url, $args = []) {
            $this->http[] = [(string) $url, 'POST', is_array($args) ? $args : []];
            return [];
        });
        Functions\when('home_url')->alias(static function ($path = '/') {
            $path = is_string($path) ? $path : '/';
            if ($path === '') {
                return 'https://shop.example';
            }
            return 'https://shop.example' . ($path[0] === '/' ? $path : '/' . $path);
        });
        // 2026-07 wp.org review fix: CloudPanel's $_SERVER['SERVER_NAME']/['HOME']
        // reads are now sanitize_text_field(wp_unslash(...)) inline at the read
        // site. Mock both realistically (not a passthrough) so the fallback
        // branches are exercised deterministically regardless of what earlier
        // test files may have already Patchwork-defined process-wide.
        Functions\when('wp_unslash')->alias(static fn ($v) => is_string($v) ? stripslashes($v) : $v);
        // Mirrors real WP core sanitize_text_field(): collapses \r\n\t\space
        // RUNS into a single space (does NOT delete the surrounding text) --
        // the real function's job is to make raw newline/CR bytes unable to
        // start a new HTTP header line, not to redact arbitrary substrings.
        Functions\when('sanitize_text_field')->alias(static function ($v) {
            $v = (string) $v;
            $v = preg_replace('/[\r\n\t ]+/', ' ', $v) ?? '';
            return trim($v);
        });

        unset($GLOBALS['kinsta_cache'], $GLOBALS['cloudflareHooks']);
        unset(
            $_SERVER['HTTP_X_VARNISH'],
            $_SERVER['HTTP_X_APPLICATION'],
            $_SERVER['HTTP_VIA'],
            $_SERVER['HTTP_HOST'],
            $_SERVER['SERVER_NAME']
        );
    }

    protected function tear_down(): void
    {
        $this->cleanCloudPanelState();

        unset($GLOBALS['kinsta_cache'], $GLOBALS['cloudflareHooks']);
        unset(
            $_SERVER['HTTP_X_VARNISH'],
            $_SERVER['HTTP_X_APPLICATION'],
            $_SERVER['HTTP_VIA'],
            $_SERVER['HTTP_HOST'],
            $_SERVER['SERVER_NAME']
        );
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Dispatch every callback registered on a hook with the given args. */
    private function dispatch(string $hook, mixed ...$args): void
    {
        foreach ($this->hooks[$hook] ?? [] as $cb) {
            $cb(...$args);
        }
    }

    /** Remove CloudPanel temp state between tests. */
    private function cleanCloudPanelState(): void
    {
        if (self::$cloudpanelSettingsPath !== '') {
            self::removeCloudPanelPath(self::$cloudpanelSettingsPath);
        }
        if (self::$cloudpanelPageSpeedPath !== '') {
            self::removeCloudPanelPath(self::$cloudpanelPageSpeedPath);
        }
    }

    /** Write a CloudPanel settings.json for the current test. */
    private function writeCloudPanelSettings(array $data): void
    {
        $dir = dirname(self::$cloudpanelSettingsPath);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents(self::$cloudpanelSettingsPath, json_encode($data, JSON_THROW_ON_ERROR));
    }

    /** Build the path to a host's PageSpeed cache directory. */
    private function cloudpanelPageSpeedHostPath(string $host): string
    {
        return self::$cloudpanelPageSpeedPath . '/pagespeed_cache/v3/' . $host;
    }

    /** Create a nested file under a host's PageSpeed cache directory. */
    private function writeCloudPanelPageSpeedFile(string $host, string $relativePath, string $contents): void
    {
        $path = $this->cloudpanelPageSpeedHostPath($host) . '/' . ltrim($relativePath, '/');
        $dir  = dirname($path);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($path, $contents);
    }

    /** Recursively delete a file or directory. */
    private static function removeCloudPanelPath(string $path): void
    {
        if (!file_exists($path)) {
            return;
        }

        if (is_dir($path) && !is_link($path)) {
            $items = new \FilesystemIterator($path, \FilesystemIterator::SKIP_DOTS);
            foreach ($items as $item) {
                self::removeCloudPanelPath($item->getPathname());
            }
            @rmdir($path);
            return;
        }

        @unlink($path);
    }

    // ------------------------------------------------------------------ loader

    public function test_loader_registers_all_integrations_on_purge_actions(): void
    {
        (new Integrations())->boot();

        // 12 integrations each register the three purge:before hooks.
        $this->assertCount(12, $this->hooks['wpmgr_purge_urls:before'] ?? []);
        $this->assertCount(12, $this->hooks['wpmgr_purge_pages:before'] ?? []);
        $this->assertCount(12, $this->hooks['wpmgr_purge_everything:before'] ?? []);
    }

    public function test_loader_boot_is_idempotent(): void
    {
        $loader = new Integrations();
        $loader->boot();
        $loader->boot();

        // Still one registration per integration despite the double boot.
        $this->assertCount(12, $this->hooks['wpmgr_purge_everything:before'] ?? []);
    }

    // -------------------------------------------------- no-op when host absent

    /**
     * With NO host signals present, booting + firing every purge action must be
     * completely silent: no host call, no outbound HTTP.
     *
     * @dataProvider purgeActions
     */
    public function test_every_integration_noops_when_host_absent(string $hook, array $args): void
    {
        (new Integrations())->boot();
        $this->dispatch($hook, ...$args);

        $this->assertSame([], $this->http, 'no integration may make an outbound call when its host is absent');
    }

    /** @return array<string,array{0:string,1:array<mixed>}> */
    public static function purgeActions(): array
    {
        return [
            'purge urls'       => ['wpmgr_purge_urls:before', [['https://example.com/a/', 'https://example.com/b/']]],
            'purge pages'      => ['wpmgr_purge_pages:before', [['https://example.com/']]],
            'purge everything' => ['wpmgr_purge_everything:before', []],
        ];
    }

    /**
     * Each integration instantiated directly also no-ops on both handlers when
     * its host is absent (defence regardless of how it is dispatched).
     *
     * @dataProvider allIntegrations
     */
    public function test_integration_class_noops_when_host_absent(string $class): void
    {
        if ($class === WPEngine::class && class_exists('\WpeCommon')) {
            $this->markTestSkipped('WpeCommon double already defined this process — host not absent.');
        }

        /** @var \WPMgr\Agent\Integrations\Integration $integration */
        $integration = new $class();
        $integration->onPurgeUrls(['https://example.com/x/']);
        $integration->onPurgeEverything();

        $this->assertSame([], $this->http, $class . ' must not call out when its host is absent');
    }

    /**
     * Define a recording WpeCommon double in the global namespace (once per
     * process). WP Engine ships this class; we mimic its static purge surface.
     */
    private function defineWpeCommon(): void
    {
        if (class_exists('\WpeCommon')) {
            return;
        }
        eval(
            'class WpeCommon {'
            . ' public static array $calls = [];'
            . ' public static function purge_varnish_cache() { self::$calls[] = "purge_varnish_cache"; }'
            . ' public static function purge_memcached() { self::$calls[] = "purge_memcached"; }'
            . ' public static function clear_maxcdn_cache() { self::$calls[] = "clear_maxcdn_cache"; }'
            . '}'
        );
    }

    /** @return array<string,array{0:class-string}> */
    public static function allIntegrations(): array
    {
        return [
            'Varnish'    => [Varnish::class],
            'CloudPanel' => [CloudPanel::class],
            'Cloudflare' => [Cloudflare::class],
            'Kinsta'     => [Kinsta::class],
            'SiteGround' => [SiteGround::class],
            'WPEngine'   => [WPEngine::class],
            'Cloudways'  => [Cloudways::class],
            'RunCloud'   => [RunCloud::class],
            'GridPane'   => [GridPane::class],
            'SpinupWP'   => [SpinupWP::class],
            'RocketNet'  => [RocketNet::class],
            'WPCloud'    => [WPCloud::class],
        ];
    }

    // ----------------------------------------------------------- Varnish (host)

    public function test_varnish_detected_purges_each_url_over_loopback(): void
    {
        // Varnish marks the request with X-Varnish; Host header keys the object.
        $_SERVER['HTTP_X_VARNISH'] = '12345';
        $_SERVER['HTTP_HOST']      = 'shop.example';

        (new Varnish())->onPurgeUrls(['https://shop.example/cart/', 'https://shop.example/checkout/']);

        $this->assertCount(2, $this->http);

        [$url1, $method1, $args1] = $this->http[0];
        $this->assertSame('PURGE', $method1);
        $this->assertSame('http://127.0.0.1/cart/', $url1);
        $this->assertSame('shop.example', $args1['headers']['Host']);
        // Loopback target: reject_unsafe_urls is intentionally disabled.
        $this->assertFalse($args1['reject_unsafe_urls']);

        [$url2, $method2] = $this->http[1];
        $this->assertSame('PURGE', $method2);
        $this->assertSame('http://127.0.0.1/checkout/', $url2);
    }

    public function test_varnish_detected_bans_everything(): void
    {
        $_SERVER['HTTP_X_VARNISH'] = '1';
        $_SERVER['HTTP_HOST']      = 'shop.example';

        (new Varnish())->onPurgeEverything();

        $this->assertCount(1, $this->http);
        [$url, $method, $args] = $this->http[0];
        $this->assertSame('BAN', $method);
        $this->assertSame('http://127.0.0.1/.*', $url);
        $this->assertSame('shop.example', $args['headers']['X-Ban-Host']);
    }

    public function test_varnish_noops_on_varnishpass_application(): void
    {
        // X-Varnish present but the app is bypassing Varnish → nothing cached.
        $_SERVER['HTTP_X_VARNISH']     = '1';
        $_SERVER['HTTP_X_APPLICATION'] = 'varnishpass';
        $_SERVER['HTTP_HOST']          = 'shop.example';

        (new Varnish())->onPurgeEverything();
        $this->assertSame([], $this->http);
    }

    // --------------------------------------------------------- WP Engine (host)

    public function test_wpengine_detected_flushes_each_cache_layer(): void
    {
        $this->defineWpeCommon();
        \WpeCommon::$calls = [];
        (new WPEngine())->onPurgeEverything();

        // Every available WpeCommon purge method was invoked exactly once.
        $this->assertSame(
            ['purge_varnish_cache', 'purge_memcached', 'clear_maxcdn_cache'],
            \WpeCommon::$calls
        );
    }

    public function test_wpengine_url_purge_falls_back_to_full_flush(): void
    {
        $this->defineWpeCommon();
        \WpeCommon::$calls = [];
        (new WPEngine())->onPurgeUrls(['https://example.com/page/']);

        // No per-URL WPE purge exists → full flush.
        $this->assertContains('purge_varnish_cache', \WpeCommon::$calls);
    }

    // --------------------------------------------------------- CloudPanel (host)

    public function test_cloudpanel_full_purge_sends_host_and_tag_purges(): void
    {
        $this->writeCloudPanelSettings([
            'enabled'        => true,
            'server'         => '127.0.0.1:6081',
            'cacheTagPrefix' => 'testtag',
        ]);
        $_SERVER['HTTP_HOST'] = 'attacker.example';

        (new CloudPanel())->onPurgeEverything();

        $this->assertCount(2, $this->http);

        [$url1, $method1, $args1] = $this->http[0];
        $this->assertSame('PURGE', $method1);
        $this->assertSame('http://127.0.0.1:6081/', $url1);
        $this->assertSame('shop.example', $args1['headers']['Host']);
        $this->assertFalse($args1['blocking']);
        $this->assertFalse($args1['sslverify']);
        $this->assertFalse($args1['reject_unsafe_urls']);

        [$url2, $method2, $args2] = $this->http[1];
        $this->assertSame('PURGE', $method2);
        $this->assertSame('http://127.0.0.1:6081/', $url2);
        $this->assertSame('testtag', $args2['headers']['X-Cache-Tags']);
        $this->assertFalse($args2['blocking']);
        $this->assertFalse($args2['sslverify']);
        $this->assertFalse($args2['reject_unsafe_urls']);
    }

    public function test_cloudpanel_noops_for_public_varnish_endpoint(): void
    {
        $this->writeCloudPanelSettings([
            'enabled'        => true,
            'server'         => 'https://example.com:9000',
            'cacheTagPrefix' => 'testtag',
        ]);

        $_SERVER['HTTP_HOST'] = 'shop.example';
        $this->writeCloudPanelPageSpeedFile('shop.example', 'keep.txt', 'keep');

        new CloudPanel();
        $this->dispatch('wpmgr_purge_everything:before');

        $this->assertSame([], $this->http, 'no HTTP calls when CloudPanel endpoint is public');
        $this->assertFileExists($this->cloudpanelPageSpeedHostPath('shop.example') . '/keep.txt');
    }

    public function test_cloudpanel_url_purge_preserves_path_query_and_skips_offsite_hosts(): void
    {
        $this->writeCloudPanelSettings([
            'enabled'        => true,
            'server'         => '127.0.0.1:6081',
            'cacheTagPrefix' => 'testtag',
        ]);

        (new CloudPanel())->onPurgeUrls([
            'https://shop.example/cart/?id=1',
            'https://cdn.example/about/us/',
        ]);

        $this->assertCount(1, $this->http);

        [$url1, $method1, $args1] = $this->http[0];
        $this->assertSame('PURGE', $method1);
        $this->assertSame('http://127.0.0.1:6081/cart/?id=1', $url1);
        $this->assertSame('shop.example', $args1['headers']['Host']);
    }

    /**
     * 2026-07 wp.org review fix regression: siteHost() falls back to
     * $_SERVER['SERVER_NAME'] when home_url() cannot resolve a host, and must
     * sanitize it inline (sanitize_text_field(wp_unslash(...))) rather than
     * trusting the raw superglobal.
     */
    public function test_cloudpanel_site_host_falls_back_to_sanitized_server_name(): void
    {
        Functions\when('home_url')->justReturn('');
        $_SERVER['SERVER_NAME'] = 'fallback.example';

        $this->writeCloudPanelSettings([
            'enabled'        => true,
            'server'         => '127.0.0.1:6081',
            'cacheTagPrefix' => 'testtag',
        ]);

        (new CloudPanel())->onPurgeEverything();

        $this->assertNotEmpty($this->http);
        [, , $args1] = $this->http[0];
        $this->assertSame('fallback.example', $args1['headers']['Host']);

        unset($_SERVER['SERVER_NAME']);
    }

    /**
     * A hostile SERVER_NAME (e.g. a header-injection attempt riding a
     * misconfigured proxy) must have raw CR/LF bytes neutralized by the
     * inline sanitize_text_field(wp_unslash(...)) pass before it is ever used
     * to build a Host header -- sanitize_text_field() collapses \r\n runs to
     * a single space (the same behaviour real WP core applies), which is
     * what makes header-splitting (injecting a SECOND header line) impossible;
     * it is not a general substring redaction.
     */
    public function test_cloudpanel_site_host_strips_control_characters_from_server_name(): void
    {
        Functions\when('home_url')->justReturn('');
        $_SERVER['SERVER_NAME'] = "evil.example\r\nX-Injected: 1";

        $ref = new \ReflectionMethod(CloudPanel::class, 'siteHost');
        $host = $ref->invoke(new CloudPanel());

        $this->assertStringNotContainsString("\r", $host, 'no raw CR may survive sanitization');
        $this->assertStringNotContainsString("\n", $host, 'no raw LF may survive sanitization -- this is what prevents HTTP header-splitting');
        $this->assertSame('evil.example X-Injected: 1', $host, 'CRLF run collapses to a single space, exactly matching real sanitize_text_field()');

        unset($_SERVER['SERVER_NAME']);
    }

    /**
     * 2026-07 wp.org review fix regression: homeDirectories() reads
     * $_SERVER['HOME'] through the same inline sanitize_text_field(wp_unslash(...))
     * pass, neutralizing raw CR/LF/TAB bytes (collapsed to a single space,
     * matching real sanitize_text_field()) before the value is used to build
     * a filesystem path.
     */
    public function test_cloudpanel_home_directories_sanitizes_server_home(): void
    {
        $savedHome = $_SERVER['HOME'] ?? null;
        $_SERVER['HOME'] = "/home/evil\r\n\t/sneaky";

        $ref = new \ReflectionMethod(CloudPanel::class, 'homeDirectories');
        $homes = $ref->invoke(new CloudPanel());

        foreach ($homes as $home) {
            $this->assertStringNotContainsString("\r", $home, 'no raw CR may survive sanitization');
            $this->assertStringNotContainsString("\n", $home, 'no raw LF may survive sanitization');
            $this->assertStringNotContainsString("\t", $home, 'no raw TAB may survive sanitization');
        }
        $this->assertContains(
            '/home/evil /sneaky',
            $homes,
            'CRLF/TAB run collapses to a single space, exactly matching real sanitize_text_field()'
        );

        if ($savedHome === null) {
            unset($_SERVER['HOME']);
        } else {
            $_SERVER['HOME'] = $savedHome;
        }
    }

    public function test_cloudpanel_full_purge_wipes_pagespeed_host_cache(): void
    {
        $this->writeCloudPanelSettings([
            'enabled'        => true,
            'server'         => '127.0.0.1:6081',
            'cacheTagPrefix' => 'testtag',
        ]);
        $_SERVER['HTTP_HOST'] = 'attacker.example';

        $hostDir = $this->cloudpanelPageSpeedHostPath('shop.example');
        $this->writeCloudPanelPageSpeedFile('shop.example', 'page.html', 'cached');
        $this->writeCloudPanelPageSpeedFile('shop.example', 'nested/inner.txt', 'cached');

        $siblingDir = $this->cloudpanelPageSpeedHostPath('other.example');
        mkdir($siblingDir, 0755, true);
        file_put_contents($siblingDir . '/sibling.txt', 'cached');

        (new CloudPanel())->onPurgeEverything();

        $this->assertDirectoryExists($hostDir);
        $this->assertFileDoesNotExist($hostDir . '/page.html');
        $this->assertFileDoesNotExist($hostDir . '/nested/inner.txt');
        $this->assertDirectoryDoesNotExist($hostDir . '/nested');
        $this->assertFileExists($siblingDir . '/sibling.txt');
    }

    /**
     * @return array<string,array{0:array<string,mixed>|null}>
     */
    public static function cloudpanelInvalidSettings(): array
    {
        return [
            'missing settings file' => [null],
            'disabled'              => [['enabled' => false, 'server' => '127.0.0.1:6081', 'cacheTagPrefix' => 'testtag']],
            'empty server'          => [['enabled' => true, 'server' => '', 'cacheTagPrefix' => 'testtag']],
            'empty cacheTagPrefix'  => [['enabled' => true, 'server' => '127.0.0.1:6081', 'cacheTagPrefix' => '']],
        ];
    }

    /**
     * @dataProvider cloudpanelInvalidSettings
     * @param array<string,mixed>|null $settings
     */
    public function test_cloudpanel_noops_for_invalid_settings(?array $settings): void
    {
        if ($settings !== null) {
            $this->writeCloudPanelSettings($settings);
        }

        $_SERVER['HTTP_HOST'] = 'shop.example';
        $this->writeCloudPanelPageSpeedFile('shop.example', 'keep.txt', 'keep');

        (new CloudPanel())->onPurgeEverything();

        $this->assertSame([], $this->http, 'no HTTP calls when CloudPanel settings do not qualify');
        $this->assertFileExists($this->cloudpanelPageSpeedHostPath('shop.example') . '/keep.txt');
    }
}
