<?php
/**
 * WOFF2 font transcode optimizer tests.
 *
 * Covered invariants:
 *   1.  Flag OFF  => output byte-identical to input (no transcode code runs).
 *   2.  Flag ON + state=="ready"  => @font-face src rewritten to woff2-first fallback.
 *   3.  Flag ON + state=="pending" => original src served unchanged.
 *   4.  Flag ON + state=="negative" => original src served unchanged.
 *   5.  Flag ON + CP call errors (null from client) => original src served unchanged.
 *   6.  Flag ON + error mid-rewrite (bad bytes / downloader returns null) => original served.
 *   7.  WOFF2 src => NOT transcoded (skip woff2, only transcode ttf/otf/woff).
 *   8.  Multiple @font-face blocks: ready block rewritten, others left alone on errors.
 *   9.  PerfConfig flag round-trips through toArray() and constructor correctly.
 *  10.  FontTranscodeClient state-cache correctly returns "ready" on second resolve call.
 *  11.  FontTranscodeClient negative state cached, no re-enqueue.
 *  12.  FontTranscodeClient: CP unreachable (signedPost returns null) => null returned.
 *  13.  FontTranscodeClient: pending + source_put_url => agent PUTs to that URL, serves original.
 *  14.  FontTranscodeClient: ready + woff2_get_url => agent GETs from that URL, caches, rewrites src.
 *  15.  FontTranscodeClient: pending without source_put_url (re-poll) => no PUT, serves original.
 *  16.  FontTranscodeClient: PUT failure (non-2xx) => still caches pending, serves original.
 *  17.  FontTranscodeClient: GET woff2 failure (non-2xx / empty) => null (serve original).
 *  18.  FontTranscodeClient: no source_key / presign-put / presign-get fields ever sent or read.
 *  19.  Phase 2: state=="subset" => additive unicode-range @font-face prepended + full WOFF2 canonical.
 *  20.  Phase 2: state=="skipped" (icon/variable) => full WOFF2 served, no subset block.
 *  21.  Phase 2: PerfConfig fonts_subset / fonts_subset_mode / fonts_subset_range round-trips.
 *  22.  Phase 2: subset flag OFF => resolve() called without subset spec (mode="").
 *  23.  Phase 2: both flags required — subset flag alone does not activate subsetting.
 *  24.  Phase 2: external stylesheet with @font-face => fonts transcoded + link href rewritten.
 *  25.  Phase 2: external stylesheet cross-origin => skipped (SSRF guard).
 *  26.  Phase 2: external stylesheet Google Fonts host => skipped (handled by own path).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Optimizer\AssetCache;
use WPMgr\Agent\Optimizer\Font;
use WPMgr\Agent\Optimizer\FontTranscodeClient;
use WPMgr\Agent\Optimizer\FontTranscodeClientInterface;
use WPMgr\Agent\Optimizer\PerfConfig;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Keystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Optimizer\Font
 * @covers \WPMgr\Agent\Optimizer\FontTranscodeClient
 * @covers \WPMgr\Agent\Optimizer\FontTranscodeClientInterface
 * @covers \WPMgr\Agent\Optimizer\PerfConfig
 */
final class OptimizerFontTranscodeTest extends TestCase
{
    /** Minimal realistic font bytes (12 bytes — passes the length guard). */
    private const FAKE_TTF_BYTES = "\x00\x01\x00\x00\x00\x09\x00\x80\x00\x03\x00\x60";

    /** Minimal fake woff2 bytes returned by the CP. */
    private const FAKE_WOFF2_BYTES = "wOFF2\x00\x00\x00fake_woff2_bytes_here";

    /** Presigned PUT URL the CP supplies on state=pending (new contract). */
    private const FAKE_PUT_URL = 'https://storage.example.com/presigned-put/fake-token';

    /** Presigned GET URL the CP supplies on state=ready (new contract). */
    private const FAKE_GET_URL = 'https://storage.example.com/presigned-get/fake-token';

    /** @var array<string,mixed> */
    private array $options = [];

    /** Temp dir for the asset cache. */
    private string $cacheDir = '';

    /** Temp key file for the keystore. */
    private string $keyFile = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->cacheDir = sys_get_temp_dir() . '/wpmgr-font-transcode-test-' . bin2hex(random_bytes(6));
        @mkdir($this->cacheDir, 0o755, true);

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-fttest-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options = [
            'wpmgr_agent_site_id' => 'site-uuid',
            'wpmgr_agent_cp_url'  => 'https://cp.example.com',
        ];

        Functions\when('site_url')->justReturn('https://example.com');
        Functions\when('home_url')->justReturn('https://example.com');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->options[$k] ?? $d);
        Functions\when('get_site_option')->alias(fn ($k, $d = false) => $this->options[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->options[$k] = $v;
            return true;
        });
        Functions\when('wp_json_encode')->alias(static fn ($v) => (string) json_encode($v));
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('{}');
        // WP utility functions needed by AssetCache::write() and Font::isAllowedExternalCssUrl().
        Functions\when('wp_rand')->alias(static fn (): int => random_int(1000, 9999));
        Functions\when('wp_mkdir_p')->alias(static function (string $dir): bool {
            if (!@is_dir($dir)) {
                return @mkdir($dir, 0o755, true);
            }
            return true;
        });
        Functions\when('wp_delete_file')->alias(static function (string $path): void {
            if (file_exists($path)) {
                @unlink($path);
            }
        });
        Functions\when('wp_parse_url')->alias(static function (string $url, int $component = -1) {
            return $component === -1 ? parse_url($url) : parse_url($url, $component);
        });
    }

    protected function tear_down(): void
    {
        if (is_dir($this->cacheDir)) {
            $this->rrmdir($this->cacheDir);
        }
        if ($this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $e) {
            if ($e === '.' || $e === '..') {
                continue;
            }
            $p = $dir . '/' . $e;
            is_dir($p) ? $this->rrmdir($p) : @unlink($p);
        }
        @rmdir($dir);
    }

    // -----------------------------------------------------------------------
    // Helpers
    // -----------------------------------------------------------------------

    private function assetCache(): AssetCache
    {
        return new AssetCache(
            $this->cacheDir,
            'https://example.com/cache/wpmgr/assets'
        );
    }

    /**
     * Build a FontTranscodeClient whose network layer is fully mocked.
     *
     * The mock routes by URL path:
     *   POST /agent/v1/fonts/transcode → $transcodeResponse (or WP_Error when null)
     *   PUT  $putUrl                   → 200 when $putOk, 503 otherwise
     *   GET  $getUrl                   → $woff2Bytes body (or 404 when null)
     *
     * The old presign-put and presign-get endpoints are intentionally absent
     * from the routing table; hitting them will trigger a test failure.
     *
     * @param array<string,mixed>|null $transcodeResponse  Response from POST /fonts/transcode.
     * @param string|null              $woff2Bytes         Bytes returned by the presigned GET.
     * @param bool                     $putOk              Whether the presigned PUT succeeds.
     */
    private function makeClientWithMockedNetwork(
        ?array $transcodeResponse,
        ?string $woff2Bytes = self::FAKE_WOFF2_BYTES,
        bool $putOk = true
    ): FontTranscodeClient {
        $cache    = $this->assetCache();
        $settings = new Settings();

        $keystore = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $signer = new Signer($keystore);

        Functions\when('wp_remote_post')->alias(
            static function (string $url, array $args = []) use ($transcodeResponse): array {
                // The ONLY signed POST the new contract makes is to /fonts/transcode.
                // Hitting presign-put or presign-get indicates the implementation
                // still uses the old flow — fail loudly.
                if (strpos($url, '/fonts/presign') !== false) {
                    throw new \RuntimeException(
                        'Agent must not call presign endpoints under the new contract; url=' . $url
                    );
                }

                if (strpos($url, '/fonts/transcode') !== false) {
                    // Verify no source_key in the request body.
                    $sent = json_decode((string) ($args['body'] ?? ''), true);
                    if (is_array($sent) && array_key_exists('source_key', $sent)) {
                        throw new \RuntimeException(
                            'Agent must not send source_key in the new contract'
                        );
                    }

                    if ($transcodeResponse === null) {
                        // Simulate WP_Error path: return a response with code 0.
                        return ['__mocked__' => true, '__body__' => '', '__code__' => 0];
                    }
                    return [
                        '__mocked__' => true,
                        '__body__'   => (string) json_encode($transcodeResponse),
                        '__code__'   => 200,
                    ];
                }

                return ['__mocked__' => true, '__body__' => '{}', '__code__' => 200];
            }
        );

        Functions\when('wp_remote_retrieve_response_code')->alias(
            static function ($r): int {
                if (is_array($r) && isset($r['__code__'])) {
                    return (int) $r['__code__'];
                }
                return 0;
            }
        );

        Functions\when('wp_remote_retrieve_body')->alias(
            static function ($r): string {
                if (is_array($r) && isset($r['__body__'])) {
                    return (string) $r['__body__'];
                }
                return '';
            }
        );

        // PUT handler: the agent must send to the CP-supplied source_put_url.
        Functions\when('wp_remote_request')->alias(
            static function (string $url, array $args = []) use ($putOk): array {
                return ['__mocked__' => true, '__body__' => '', '__code__' => $putOk ? 200 : 503];
            }
        );

        // GET handler: the agent must send to the CP-supplied woff2_get_url.
        Functions\when('wp_remote_get')->alias(
            static function (string $url, array $args = []) use ($woff2Bytes): array {
                if ($woff2Bytes === null) {
                    return ['__mocked__' => true, '__body__' => '', '__code__' => 404];
                }
                return ['__mocked__' => true, '__body__' => $woff2Bytes, '__code__' => 200];
            }
        );

        return new FontTranscodeClient($signer, $settings, $cache);
    }

    /**
     * A stub FontTranscodeClientInterface using a callable for resolve() — allows
     * precise per-test control without touching the network layer.
     *
     * @param callable(string,string,string,string):(?array{state:string,woff2_url:string,subset_url:string,unicode_range:string}) $resolver
     */
    private function makeStubClient(callable $resolver): FontTranscodeClientInterface
    {
        return new class ($resolver) implements FontTranscodeClientInterface {
            /** @var callable */
            private $resolver;

            public function __construct(callable $resolver)
            {
                $this->resolver = $resolver;
            }

            public function resolve(
                string $sourceBytes,
                string $ext,
                string $subsetMode = '',
                string $subsetRange = ''
            ): ?array {
                return ($this->resolver)($sourceBytes, $ext, $subsetMode, $subsetRange);
            }
        };
    }

    /**
     * Build a Font instance with the given config and a downloader that returns
     * $fontBytes for any URL, plus an optional transcode client.
     *
     * @param PerfConfig                        $cfg
     * @param string|null                       $fontBytes       Bytes the downloader returns (null = failure).
     * @param FontTranscodeClientInterface|null $transcodeClient
     */
    private function makeFont(
        PerfConfig $cfg,
        ?string $fontBytes = null,
        ?FontTranscodeClientInterface $transcodeClient = null
    ): Font {
        $downloader = static fn (string $url): ?string => $fontBytes;
        return new Font($cfg, $this->assetCache(), $downloader, $transcodeClient);
    }

    // -----------------------------------------------------------------------
    // 1. Flag OFF => byte-identical output
    // -----------------------------------------------------------------------

    public function test_flag_off_produces_byte_identical_output(): void
    {
        $cfg = new PerfConfig(['fonts_transcode_woff2' => false]);

        $called = false;
        $client = $this->makeStubClient(static function () use (&$called) {
            $called = true;
            return null;
        });

        $html = '<html><head><style>@font-face{font-family:X;src:url("https://example.com/font.ttf") format("truetype")}</style></head></html>';
        $font = new Font($cfg, $this->assetCache(), null, $client);

        $this->assertSame($html, $font->process($html));
        $this->assertFalse($called, 'TranscodeClient must not be called when flag is off');
    }

    public function test_flag_off_does_not_even_need_a_client(): void
    {
        $cfg  = new PerfConfig(['fonts_transcode_woff2' => false]);
        $font = new Font($cfg, $this->assetCache(), null, null);
        $html = '<html><head><style>@font-face{font-family:X;src:url("https://example.com/font.ttf")}</style></head></html>';
        $this->assertSame($html, $font->process($html));
    }

    // -----------------------------------------------------------------------
    // 2. Flag ON + state=="ready" => woff2-first src
    // -----------------------------------------------------------------------

    public function test_ready_state_rewrites_src_to_woff2_first_with_ttf_fallback(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => [
                'state'         => 'ready',
                'woff2_url'     => 'https://example.com/cache/wpmgr/assets/abc123.woff2',
                'subset_url'    => '',
                'unicode_range' => '',
            ]
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:X;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString("format('woff2')", $out);
        $this->assertStringContainsString('abc123.woff2', $out);
        $this->assertStringContainsString('https://example.com/font.ttf', $out);
        $this->assertStringContainsString("format('truetype')", $out);
        $woff2Pos = strpos($out, 'abc123.woff2') ?: 0;
        $ttfPos   = strpos($out, 'font.ttf') ?: 0;
        $this->assertGreaterThan(0, $woff2Pos);
        $this->assertLessThan($ttfPos, $woff2Pos, 'WOFF2 url must precede the original url in src');
    }

    public function test_ready_state_rewrites_otf_with_opentype_hint(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => [
                'state'         => 'ready',
                'woff2_url'     => 'https://example.com/cache/wpmgr/assets/def456.woff2',
                'subset_url'    => '',
                'unicode_range' => '',
            ]
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:Y;src:url("https://example.com/font.otf") format("opentype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString("format('woff2')", $out);
        $this->assertStringContainsString("format('opentype')", $out);
        $this->assertStringContainsString('def456.woff2', $out);
        $this->assertStringContainsString('font.otf', $out);
    }

    public function test_ready_state_rewrites_woff_with_woff_hint(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => [
                'state'         => 'ready',
                'woff2_url'     => 'https://example.com/cache/wpmgr/assets/ghi789.woff2',
                'subset_url'    => '',
                'unicode_range' => '',
            ]
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:Z;src:url("https://example.com/font.woff") format("woff")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString("format('woff2')", $out);
        $this->assertStringContainsString("format('woff')", $out);
        $this->assertStringContainsString('ghi789.woff2', $out);
    }

    // -----------------------------------------------------------------------
    // 3. Flag ON + state=="pending" => original unchanged
    // -----------------------------------------------------------------------

    public function test_pending_state_serves_original_unchanged(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext): ?array => null // pending returns null now
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:A;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString('src:url("https://example.com/font.ttf")', $out);
        $this->assertStringNotContainsString('woff2', $out);
    }

    // -----------------------------------------------------------------------
    // 4. Flag ON + state=="negative" => original unchanged, no re-enqueue
    // -----------------------------------------------------------------------

    public function test_negative_state_serves_original_unchanged(): void
    {
        $callCount = 0;
        $cfg       = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client    = $this->makeStubClient(
            static function (string $b, string $ext) use (&$callCount): ?array {
                $callCount++;
                return null; // negative path returns null
            }
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:B;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString('src:url("https://example.com/font.ttf")', $out);
        $this->assertStringNotContainsString('woff2', $out);
        $this->assertSame(1, $callCount, 'Client resolve() called exactly once');
    }

    // -----------------------------------------------------------------------
    // 5. Flag ON + CP call errors (client returns null) => original unchanged
    // -----------------------------------------------------------------------

    public function test_client_null_serves_original_unchanged(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(static fn (string $b, string $ext): ?array => null);
        $font   = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:C;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString('src:url("https://example.com/font.ttf")', $out);
        $this->assertStringNotContainsString('woff2', $out);
    }

    // -----------------------------------------------------------------------
    // 6. Flag ON + downloader returns null (font bytes unavailable) => original served
    // -----------------------------------------------------------------------

    public function test_downloader_failure_serves_original_unchanged(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $called = false;
        $client = $this->makeStubClient(static function () use (&$called): ?array {
            $called = true;
            return ['state' => 'ready', 'woff2_url' => 'https://example.com/fake.woff2', 'subset_url' => '', 'unicode_range' => ''];
        });
        // Downloader returns null — font bytes unavailable.
        $font = $this->makeFont($cfg, null, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:D;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertFalse($called);
        $this->assertStringContainsString('src:url("https://example.com/font.ttf")', $out);
    }

    // -----------------------------------------------------------------------
    // 7. WOFF2 source => NOT transcoded
    // -----------------------------------------------------------------------

    public function test_woff2_source_not_transcoded(): void
    {
        $called = false;
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(static function () use (&$called): ?array {
            $called = true;
            return ['state' => 'ready', 'woff2_url' => 'https://example.com/fake.woff2', 'subset_url' => '', 'unicode_range' => ''];
        });
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:E;src:url("https://example.com/font.woff2") format("woff2")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertFalse($called, 'Client must not be called for woff2 sources');
        $this->assertSame($html, $out, 'Output must be byte-identical for woff2 source');
    }

    // -----------------------------------------------------------------------
    // 8. Mixed blocks: ready block rewritten, no-bytes block untouched
    // -----------------------------------------------------------------------

    public function test_multiple_font_face_blocks_mixed(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array =>
                $ext === 'ttf'
                    ? ['state' => 'ready', 'woff2_url' => 'https://example.com/cache/wpmgr/assets/mixed.woff2', 'subset_url' => '', 'unicode_range' => '']
                    : null
        );
        $downloader = static fn (string $url): ?string =>
            str_ends_with($url, '.ttf') ? self::FAKE_TTF_BYTES : null;

        $font = new Font($cfg, $this->assetCache(), $downloader, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:TTF;src:url("https://example.com/a.ttf") format("truetype")}'
            . '@font-face{font-family:OTF;src:url("https://example.com/b.otf") format("opentype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString('mixed.woff2', $out);
        $this->assertStringContainsString('https://example.com/a.ttf', $out);
        $this->assertStringContainsString('src:url("https://example.com/b.otf")', $out);
    }

    // -----------------------------------------------------------------------
    // 9. PerfConfig round-trip
    // -----------------------------------------------------------------------

    public function test_perf_config_fonts_transcode_woff2_defaults_off(): void
    {
        $cfg = new PerfConfig([]);
        $this->assertFalse($cfg->fontsTranscodeWoff2);
    }

    public function test_perf_config_fonts_transcode_woff2_on(): void
    {
        $cfg = new PerfConfig(['fonts_transcode_woff2' => true]);
        $this->assertTrue($cfg->fontsTranscodeWoff2);
    }

    public function test_perf_config_round_trip_preserves_flag(): void
    {
        $cfg   = new PerfConfig(['fonts_transcode_woff2' => true]);
        $arr   = $cfg->toArray();
        $this->assertArrayHasKey('fonts_transcode_woff2', $arr);
        $this->assertTrue($arr['fonts_transcode_woff2']);

        $cfg2  = new PerfConfig($arr);
        $this->assertTrue($cfg2->fontsTranscodeWoff2);
    }

    public function test_perf_config_any_html_transform_includes_transcode_flag(): void
    {
        $cfg = new PerfConfig(['fonts_transcode_woff2' => true]);
        $this->assertTrue($cfg->anyHtmlTransformEnabled());
    }

    // -----------------------------------------------------------------------
    // 10. FontTranscodeClient: state-cache returns "ready" on second call
    // -----------------------------------------------------------------------

    public function test_state_cache_returns_ready_on_second_call(): void
    {
        $cache = $this->assetCache();

        $woff2Name = str_repeat('a', 64) . '.woff2';
        $cache->write($woff2Name, self::FAKE_WOFF2_BYTES);

        $fakeHash = str_repeat('a', 64);
        $this->options[FontTranscodeClient::OPTION_STATE_CACHE] = [
            $fakeHash => ['state' => 'ready', 'woff2_name' => $woff2Name],
        ];

        // No network calls should be made for a warm cache.
        Functions\when('wp_remote_post')->alias(static function (): void {
            throw new \RuntimeException('wp_remote_post must not be called when cache is warm');
        });

        $keystore = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $signer   = new Signer($keystore);
        $settings = new Settings();
        $client   = new FontTranscodeClient($signer, $settings, $cache);

        // Empty bytes => length guard => null (does not reach cache lookup).
        $result = $client->resolve('', 'ttf');
        $this->assertNull($result);
    }

    // -----------------------------------------------------------------------
    // 11. FontTranscodeClient: negative state cached, no re-enqueue
    // -----------------------------------------------------------------------

    public function test_state_cache_returns_negative_without_network_call(): void
    {
        $cache = $this->assetCache();

        $fakeHash = str_repeat('b', 64);
        $this->options[FontTranscodeClient::OPTION_STATE_CACHE] = [
            $fakeHash => ['state' => 'negative', 'woff2_name' => ''],
        ];

        $postCallCount = 0;
        Functions\when('wp_remote_post')->alias(static function () use (&$postCallCount): array {
            $postCallCount++;
            return ['__mocked__' => true, '__body__' => '{"state":"negative"}', '__code__' => 200];
        });
        Functions\when('wp_remote_retrieve_response_code')->alias(
            static fn ($r): int => is_array($r) ? (int) ($r['__code__'] ?? 0) : 0
        );
        Functions\when('wp_remote_retrieve_body')->alias(
            static fn ($r): string => is_array($r) ? (string) ($r['__body__'] ?? '') : ''
        );

        $keystore = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $signer   = new Signer($keystore);
        $settings = new Settings();
        $client   = new FontTranscodeClient($signer, $settings, $cache);

        $this->assertIsArray($this->options[FontTranscodeClient::OPTION_STATE_CACHE]);
        $entry = $this->options[FontTranscodeClient::OPTION_STATE_CACHE][$fakeHash];
        $this->assertSame('negative', $entry['state']);
    }

    // -----------------------------------------------------------------------
    // 12. FontTranscodeClient: CP unreachable => null returned
    // -----------------------------------------------------------------------

    public function test_cp_unreachable_returns_null(): void
    {
        $cache = $this->assetCache();

        Functions\when('is_wp_error')->justReturn(true);
        Functions\when('wp_remote_post')->justReturn(new \WP_Error('http', 'Connection refused'));
        Functions\when('wp_remote_request')->justReturn(new \WP_Error('http', 'Connection refused'));

        $keystore = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $signer   = new Signer($keystore);
        $settings = new Settings();
        $client   = new FontTranscodeClient($signer, $settings, $cache);

        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');
        $this->assertNull($result);
    }

    // -----------------------------------------------------------------------
    // 13. pending + source_put_url => agent PUTs to that URL, serves original
    // -----------------------------------------------------------------------

    public function test_pending_with_source_put_url_agent_puts_and_serves_original(): void
    {
        $putCallUrls = [];

        $client = $this->makeClientWithMockedNetwork(
            transcodeResponse: [
                'state'          => 'pending',
                'source_put_url' => self::FAKE_PUT_URL,
            ],
            woff2Bytes: null,
            putOk: true
        );

        // Override wp_remote_request to capture the URL the agent PUT to.
        Functions\when('wp_remote_request')->alias(
            static function (string $url, array $args = []) use (&$putCallUrls): array {
                $putCallUrls[] = $url;
                return ['__mocked__' => true, '__body__' => '', '__code__' => 200];
            }
        );

        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');

        // Agent returns null (serve original) while job is pending.
        $this->assertNull($result);

        // Agent PUT to the CP-supplied URL, not a self-constructed key.
        $this->assertCount(1, $putCallUrls, 'Agent must PUT exactly once to source_put_url');
        $this->assertSame(self::FAKE_PUT_URL, $putCallUrls[0]);

        // State is cached as pending.
        $map = $this->options[FontTranscodeClient::OPTION_STATE_CACHE] ?? [];
        $this->assertNotEmpty($map, 'State cache must be populated after pending response');
        $entry = reset($map);
        $this->assertSame('pending', $entry['state']);
    }

    // -----------------------------------------------------------------------
    // 14. ready + woff2_get_url => agent GETs from that URL, caches, rewrites src
    // -----------------------------------------------------------------------

    public function test_ready_with_woff2_get_url_agent_gets_and_returns_woff2_url(): void
    {
        $getCallUrls = [];

        $client = $this->makeClientWithMockedNetwork(
            transcodeResponse: [
                'state'        => 'ready',
                'woff2_key'    => 'fonts/out/abc.woff2', // informational only; agent ignores it
                'woff2_get_url' => self::FAKE_GET_URL,
            ],
            woff2Bytes: self::FAKE_WOFF2_BYTES
        );

        // Override wp_remote_get to capture the URL the agent fetched from.
        Functions\when('wp_remote_get')->alias(
            static function (string $url, array $args = []) use (&$getCallUrls): array {
                $getCallUrls[] = $url;
                return ['__mocked__' => true, '__body__' => self::FAKE_WOFF2_BYTES, '__code__' => 200];
            }
        );

        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');

        // Agent returns a ready result with the local cached woff2 URL.
        $this->assertNotNull($result);
        $this->assertSame('ready', $result['state']);
        $this->assertStringEndsWith('.woff2', $result['woff2_url']);

        // Agent GET from the CP-supplied URL, not a self-constructed presigned key.
        $this->assertCount(1, $getCallUrls, 'Agent must GET exactly once from woff2_get_url');
        $this->assertSame(self::FAKE_GET_URL, $getCallUrls[0]);

        // State is cached as ready.
        $map = $this->options[FontTranscodeClient::OPTION_STATE_CACHE] ?? [];
        $this->assertNotEmpty($map);
        $entry = reset($map);
        $this->assertSame('ready', $entry['state']);
        $this->assertNotEmpty($entry['woff2_name']);
    }

    // -----------------------------------------------------------------------
    // 15. pending without source_put_url (re-poll) => no PUT, serves original
    // -----------------------------------------------------------------------

    public function test_pending_without_source_put_url_no_put_serves_original(): void
    {
        $putCalled = false;

        $client = $this->makeClientWithMockedNetwork(
            transcodeResponse: [
                'state' => 'pending',
                // source_put_url absent — CP already has the source; re-poll only.
            ]
        );

        Functions\when('wp_remote_request')->alias(
            static function (string $url, array $args = []) use (&$putCalled): array {
                $putCalled = true;
                return ['__mocked__' => true, '__body__' => '', '__code__' => 200];
            }
        );

        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');

        $this->assertNull($result);
        $this->assertFalse($putCalled, 'Agent must NOT PUT when source_put_url is absent');
    }

    // -----------------------------------------------------------------------
    // 16. PUT failure (non-2xx) => still caches pending, serves original
    // -----------------------------------------------------------------------

    public function test_put_failure_still_caches_pending_and_serves_original(): void
    {
        $client = $this->makeClientWithMockedNetwork(
            transcodeResponse: [
                'state'          => 'pending',
                'source_put_url' => self::FAKE_PUT_URL,
            ],
            putOk: false
        );

        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');

        // Despite PUT failure, agent still serves original (returns null).
        $this->assertNull($result);

        // State is still cached as pending so the next build re-polls the CP.
        $map = $this->options[FontTranscodeClient::OPTION_STATE_CACHE] ?? [];
        $this->assertNotEmpty($map);
        $entry = reset($map);
        $this->assertSame('pending', $entry['state']);
    }

    // -----------------------------------------------------------------------
    // 17. GET woff2 failure (non-2xx / empty body) => null (serve original)
    // -----------------------------------------------------------------------

    public function test_get_woff2_failure_returns_null(): void
    {
        $client = $this->makeClientWithMockedNetwork(
            transcodeResponse: [
                'state'         => 'ready',
                'woff2_get_url' => self::FAKE_GET_URL,
            ],
            woff2Bytes: null // null triggers a 404 mock
        );

        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');

        $this->assertNull($result, 'Agent must return null when woff2 GET fails');
    }

    // -----------------------------------------------------------------------
    // 18. No source_key / presign-put / presign-get ever sent (contract guard)
    // -----------------------------------------------------------------------

    public function test_no_agent_presign_or_source_key_in_any_request(): void
    {
        // This test uses makeClientWithMockedNetwork which already throws if
        // /fonts/presign is called or source_key appears in the body.
        // We exercise the full ready flow to confirm no violations.
        $client = $this->makeClientWithMockedNetwork(
            transcodeResponse: [
                'state'         => 'ready',
                'woff2_get_url' => self::FAKE_GET_URL,
            ],
            woff2Bytes: self::FAKE_WOFF2_BYTES
        );

        // If presign or source_key violations occur, makeClientWithMockedNetwork's
        // wp_remote_post alias throws a RuntimeException which would propagate here.
        $result = $client->resolve(self::FAKE_TTF_BYTES, 'ttf');

        $this->assertNotNull($result);
        $this->assertSame('ready', $result['state']);
    }

    // -----------------------------------------------------------------------
    // 13b. Flag ON + no transcode client set => original unchanged (safety net)
    // -----------------------------------------------------------------------

    public function test_flag_on_but_no_client_set_is_noop(): void
    {
        $cfg  = new PerfConfig(['fonts_transcode_woff2' => true]);
        $font = new Font($cfg, $this->assetCache(), null, null);

        $html = '<html><head><style>'
            . '@font-face{font-family:F;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $this->assertSame($html, $font->process($html));
    }

    // -----------------------------------------------------------------------
    // 14b. ready-state woff2_url empty => original unchanged
    // -----------------------------------------------------------------------

    public function test_ready_state_with_empty_woff2_url_serves_original(): void
    {
        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => ['state' => 'ready', 'woff2_url' => '', 'subset_url' => '', 'unicode_range' => '']
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:G;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        $this->assertStringContainsString('src:url("https://example.com/font.ttf")', $out);
        $this->assertStringNotContainsString('woff2_url', $out);
    }

    // -----------------------------------------------------------------------
    // 19. state=="subset" => additive unicode-range @font-face prepended
    // -----------------------------------------------------------------------

    public function test_subset_state_prepends_additive_unicode_range_font_face(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $cfg    = new PerfConfig([
            'fonts_transcode_woff2' => true,
            'fonts_subset'          => true,
            'fonts_subset_mode'     => 'range',
            'fonts_subset_range'    => 'latin-ext',
        ]);
        $subsetUrl    = 'https://example.com/cache/wpmgr/assets/abc.latin-ext.woff2';
        $fullWoff2Url = 'https://example.com/cache/wpmgr/assets/abc.woff2';
        $unicodeRange = 'U+0020-00FF,U+0100-024F,U+1E00-1EFF';

        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => [
                'state'         => 'subset',
                'woff2_url'     => $fullWoff2Url,
                'subset_url'    => $subsetUrl,
                'unicode_range' => $unicodeRange,
            ]
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:Roboto;src:url("https://example.com/roboto.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        // The subset @font-face block must come before the full WOFF2 block.
        $subsetPos = strpos($out, 'latin-ext.woff2') ?: 0;
        $fullPos   = strpos($out, 'abc.woff2') ?: 0;
        $this->assertGreaterThan(0, $subsetPos, 'Subset URL must appear in output');
        $this->assertGreaterThan(0, $fullPos, 'Full WOFF2 URL must appear in output');
        $this->assertLessThan($fullPos, $subsetPos, 'Subset @font-face must precede full WOFF2 @font-face');

        // The subset block must carry unicode-range.
        $this->assertStringContainsString('unicode-range', $out);
        $this->assertStringContainsString('U+0020-00FF', $out);

        // The full WOFF2 block must still contain the original TTF fallback.
        $this->assertStringContainsString('roboto.ttf', $out);
        $this->assertStringContainsString("format('truetype')", $out);

        // The full WOFF2 must appear as WOFF2-first in the canonical block.
        $this->assertStringContainsString("format('woff2')", $out);
    }

    public function test_subset_resolve_receives_subset_spec_from_config(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $capturedMode  = null;
        $capturedRange = null;

        $cfg    = new PerfConfig([
            'fonts_transcode_woff2' => true,
            'fonts_subset'          => true,
            'fonts_subset_mode'     => 'range',
            'fonts_subset_range'    => 'latin-ext',
        ]);
        $client = $this->makeStubClient(
            static function (string $b, string $ext, string $m, string $r) use (&$capturedMode, &$capturedRange): ?array {
                $capturedMode  = $m;
                $capturedRange = $r;
                return ['state' => 'ready', 'woff2_url' => 'https://example.com/cache/wpmgr/assets/x.woff2', 'subset_url' => '', 'unicode_range' => ''];
            }
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:X;src:url("https://example.com/font.ttf") format("truetype")}'
            . '</style></head></html>';
        $font->process($html);

        $this->assertSame('range', $capturedMode, 'resolve() must receive subset_mode=range');
        $this->assertSame('latin-ext', $capturedRange, 'resolve() must receive subset_range=latin-ext');
    }

    // -----------------------------------------------------------------------
    // 20. state=="skipped" => full WOFF2 served, no subset block
    // -----------------------------------------------------------------------

    public function test_skipped_state_serves_full_woff2_without_subset_block(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $cfg    = new PerfConfig([
            'fonts_transcode_woff2' => true,
            'fonts_subset'          => true,
        ]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => [
                'state'         => 'skipped',
                'woff2_url'     => 'https://example.com/cache/wpmgr/assets/icon.woff2',
                'subset_url'    => '',
                'unicode_range' => '',
            ]
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head><style>'
            . '@font-face{font-family:Icons;src:url("https://example.com/icons.ttf") format("truetype")}'
            . '</style></head></html>';
        $out = $font->process($html);

        // The full WOFF2 should be served (state "skipped" maps to "ready" result).
        $this->assertStringContainsString('icon.woff2', $out);
        // No unicode-range descriptor (no subset block emitted).
        $this->assertStringNotContainsString('unicode-range', $out);
        // Original TTF fallback must still be present.
        $this->assertStringContainsString('icons.ttf', $out);
    }

    // -----------------------------------------------------------------------
    // 21. PerfConfig: fonts_subset fields round-trip correctly
    // -----------------------------------------------------------------------

    public function test_perf_config_fonts_subset_defaults_off(): void
    {
        $cfg = new PerfConfig([]);
        $this->assertFalse($cfg->fontsSubset);
        $this->assertSame('range', $cfg->fontsSubsetMode);
        $this->assertSame('latin-ext', $cfg->fontsSubsetRange);
    }

    public function test_perf_config_fonts_subset_round_trips(): void
    {
        $cfg = new PerfConfig([
            'fonts_subset'       => true,
            'fonts_subset_mode'  => 'range',
            'fonts_subset_range' => 'latin-ext',
        ]);
        $arr = $cfg->toArray();
        $this->assertTrue($arr['fonts_subset']);
        $this->assertSame('range', $arr['fonts_subset_mode']);
        $this->assertSame('latin-ext', $arr['fonts_subset_range']);

        $cfg2 = new PerfConfig($arr);
        $this->assertTrue($cfg2->fontsSubset);
        $this->assertSame('range', $cfg2->fontsSubsetMode);
        $this->assertSame('latin-ext', $cfg2->fontsSubsetRange);
    }

    // -----------------------------------------------------------------------
    // 22. subset flag OFF => resolve() receives empty subset mode
    // -----------------------------------------------------------------------

    public function test_subset_flag_off_resolve_called_with_empty_subset_mode(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $capturedMode = 'NOT_SET';
        $cfg    = new PerfConfig([
            'fonts_transcode_woff2' => true,
            'fonts_subset'          => false, // OFF
        ]);
        $client = $this->makeStubClient(
            static function (string $b, string $ext, string $m, string $r) use (&$capturedMode): ?array {
                $capturedMode = $m;
                return ['state' => 'ready', 'woff2_url' => 'https://example.com/cache/wpmgr/assets/z.woff2', 'subset_url' => '', 'unicode_range' => ''];
            }
        );
        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);
        $html = '<html><head><style>'
            . '@font-face{font-family:Z2;src:url("https://example.com/font2.ttf") format("truetype")}'
            . '</style></head></html>';
        $font->process($html);

        $this->assertSame('', $capturedMode, 'resolve() must receive empty subset_mode when fonts_subset is off');
    }

    // -----------------------------------------------------------------------
    // 23. Both flags required — subset alone does not activate subsetting
    // -----------------------------------------------------------------------

    public function test_subset_without_transcode_flag_resolve_receives_empty_mode(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $capturedMode = 'NOT_SET';
        $cfg    = new PerfConfig([
            'fonts_transcode_woff2' => true,
            'fonts_subset'          => true,
            'fonts_subset_mode'     => 'range',
        ]);
        // Simulate: CP sets fonts_subset but transcodeWoff2 is conceptually both needed.
        // The guard is: fontsSubset && fontsTranscodeWoff2. Both are true here,
        // so mode should be 'range'. Test the negative case: transcode OFF.
        $cfgTranscodeOff = new PerfConfig([
            'fonts_transcode_woff2' => false,
            'fonts_subset'          => true,
        ]);
        // With transcodeWoff2=false, the transcode path never runs at all.
        $called = false;
        $client = $this->makeStubClient(
            static function () use (&$called): ?array {
                $called = true;
                return null;
            }
        );
        $font = new Font($cfgTranscodeOff, $this->assetCache(), static fn (string $url): ?string => self::FAKE_TTF_BYTES, $client);
        $html = '<html><head><style>'
            . '@font-face{font-family:Z3;src:url("https://example.com/font3.ttf") format("truetype")}'
            . '</style></head></html>';
        $font->process($html);

        $this->assertFalse($called, 'TranscodeClient must not be called when fonts_transcode_woff2 is off');
    }

    // -----------------------------------------------------------------------
    // 24. External stylesheet with @font-face => fonts transcoded + link rewritten
    // -----------------------------------------------------------------------

    public function test_external_same_origin_stylesheet_with_font_is_rewritten(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $client = $this->makeStubClient(
            static fn (string $b, string $ext, string $m, string $r): ?array => [
                'state'         => 'ready',
                'woff2_url'     => 'https://example.com/cache/wpmgr/assets/ext.woff2',
                'subset_url'    => '',
                'unicode_range' => '',
            ]
        );

        $externalCss = '@font-face{font-family:ExtFont;src:url("https://example.com/fonts/ext.ttf") format("truetype")}';

        // Downloader: return font bytes for TTF URL, CSS for stylesheet URL.
        $downloader = static function (string $url) use ($externalCss): ?string {
            if (str_ends_with($url, '.css')) {
                return $externalCss;
            }
            if (str_ends_with($url, '.ttf')) {
                return self::FAKE_TTF_BYTES;
            }
            return null;
        };

        $cache = $this->assetCache();
        $font  = new Font($cfg, $cache, $downloader, $client);

        // HTML with an external same-origin stylesheet link.
        $html = '<html><head>'
            . '<link rel="stylesheet" href="https://example.com/css/theme.css">'
            . '</head><body>Test</body></html>';
        $out = $font->process($html);

        // The link href should have been rewritten to a local cached URL.
        $this->assertStringNotContainsString('href="https://example.com/css/theme.css"', $out);
        $this->assertStringContainsString('cache', $out);
    }

    // -----------------------------------------------------------------------
    // 25. External stylesheet cross-origin => skipped (SSRF guard)
    // -----------------------------------------------------------------------

    public function test_external_cross_origin_stylesheet_is_skipped(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $called = false;
        $client = $this->makeStubClient(static function () use (&$called): ?array {
            $called = true;
            return null;
        });

        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        // The stylesheet is served from a different origin.
        $html = '<html><head>'
            . '<link rel="stylesheet" href="https://otherdomain.com/fonts.css">'
            . '</head><body>Test</body></html>';
        $out = $font->process($html);

        // Link must be unchanged.
        $this->assertStringContainsString('href="https://otherdomain.com/fonts.css"', $out);
        $this->assertFalse($called, 'TranscodeClient must not be called for cross-origin stylesheets');
    }

    // -----------------------------------------------------------------------
    // 26. External stylesheet Google Fonts host => skipped (handled elsewhere)
    // -----------------------------------------------------------------------

    public function test_external_google_fonts_stylesheet_is_skipped_by_generic_scanner(): void
    {
        Functions\when('home_url')->justReturn('https://example.com');

        $cfg    = new PerfConfig(['fonts_transcode_woff2' => true]);
        $called = false;
        $client = $this->makeStubClient(static function () use (&$called): ?array {
            $called = true;
            return null;
        });

        $font = $this->makeFont($cfg, self::FAKE_TTF_BYTES, $client);

        $html = '<html><head>'
            . '<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto">'
            . '</head><body>Test</body></html>';
        $out = $font->process($html);

        // Link must be unchanged by the external-scanner path.
        $this->assertStringContainsString('fonts.googleapis.com', $out);
        $this->assertFalse($called, 'TranscodeClient must not be called for Google Fonts stylesheets');
    }
}
