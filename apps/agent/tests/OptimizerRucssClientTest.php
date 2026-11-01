<?php
/**
 * RucssClient contract + graceful-degradation tests.
 *
 * Contract: the client POSTs multipart/form-data (meta + html + css) to
 * {cp_base}/agent/v1/rucss, signed with the Ed25519 Connector. On 200 the body IS
 * the used CSS (inline it + defer originals); on 202 the CP is still processing a
 * cache miss (serve the page with FULL CSS, unchanged). On ANY failure (not
 * enrolled, signing throws, CP unreachable, non-200/202, empty body) it MUST
 * return the original HTML UNCHANGED, with the full CSS intact, and MUST NEVER
 * throw to the render path. These tests force each path and assert exactly that.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Optimizer\RucssClient;
use WPMgr\Agent\Optimizer\UrlHelper;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Optimizer\RucssClient
 */
final class OptimizerRucssClientTest extends TestCase
{
    private const HTML = '<!DOCTYPE html><html><head><link rel="stylesheet" href="/a.css"></head><body>hi</body></html>';

    /** @var array<string,mixed> */
    private array $options = [];

    private string $keyFile = '';

    /** @var array<string,mixed>|null Args captured from the last wp_remote_post. */
    private ?array $lastPostArgs = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile      = sys_get_temp_dir() . '/wpmgr-rucss-' . bin2hex(random_bytes(8)) . '.key';
        $this->lastPostArgs = null;
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        // Enrolled by default (so failures past the enrollment gate are exercised).
        $this->options = [
            'wpmgr_agent_site_id' => 'site-uuid',
            'wpmgr_agent_cp_url'  => 'https://cp.example.com',
        ];
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->options[$k] ?? $d);
        // Settings::get() consults get_site_option first; return the supplied
        // default (its missing-sentinel) so it falls through to get_option.
        Functions\when('get_site_option')->alias(fn ($k, $d = false) => $this->options[$k] ?? $d);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->options[$k] = $v;
            return true;
        });
        Functions\when('home_url')->justReturn('https://example.com/');
        Functions\when('wp_json_encode')->alias(static fn ($v) => json_encode($v));
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');
        Functions\when('esc_url_raw')->returnArg();
    }

    /**
     * Capture the args wp_remote_post was called with and return $response.
     *
     * @param mixed $response Canned response.
     */
    private function capturingPost($response): void
    {
        Functions\when('wp_remote_post')->alias(function ($url, $args = []) use ($response) {
            $this->lastPostArgs = is_array($args) ? $args : [];
            $this->lastPostArgs['__url'] = (string) $url;
            return $response;
        });
    }

    /**
     * A UrlHelper that never resolves any href to a local file (so the test does
     * not depend on the filesystem — the css part is simply empty).
     */
    private function urlHelper(): UrlHelper
    {
        return new UrlHelper('https://example.com', '/nonexistent-doc-root');
    }

    protected function tear_down(): void
    {
        if ($this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * A Settings double reporting the enrolled URL/site id from the option store.
     */
    private function settings(): Settings
    {
        return new Settings();
    }

    /**
     * A real Signer backed by a provisioned keypair (signHeaders succeeds).
     * Signer is final, so we provision the underlying keystore rather than stub.
     */
    private function workingSigner(): Signer
    {
        $keystore = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        return new Signer($keystore);
    }

    /**
     * A real Signer with NO keypair: signHeaders() throws "keypair not
     * provisioned" — the worst case mid-render, which optimize() must absorb.
     */
    private function throwingSigner(): Signer
    {
        // Fresh keystore, key file removed so getSiteKeypair() returns null and
        // signHeaders() throws.
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        return new Signer(new Keystore());
    }

    public function test_returns_unchanged_when_not_enrolled(): void
    {
        $this->options = []; // wipe enrollment
        $this->capturingPost([]);

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $this->assertSame(self::HTML, $client->optimize(self::HTML));
    }

    public function test_returns_unchanged_and_never_throws_when_signer_throws(): void
    {
        $this->capturingPost([]);

        $client = new RucssClient($this->throwingSigner(), $this->settings(), [], $this->urlHelper());
        $out = $client->optimize(self::HTML);

        // The render path is intact: full CSS present, no exception escaped.
        $this->assertSame(self::HTML, $out);
        $this->assertStringContainsString('<link rel="stylesheet" href="/a.css">', $out);
    }

    public function test_returns_unchanged_when_cp_unreachable(): void
    {
        // Network error: is_wp_error true.
        Functions\when('is_wp_error')->justReturn(true);
        $this->capturingPost(new \WP_Error('http', 'down'));

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $this->assertSame(self::HTML, $client->optimize(self::HTML));
    }

    public function test_returns_unchanged_on_non_2xx(): void
    {
        $this->capturingPost([]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(502);

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $this->assertSame(self::HTML, $client->optimize(self::HTML));
    }

    public function test_returns_full_css_unchanged_on_202_cache_miss(): void
    {
        // 202 = cache miss / still processing: serve this render UN-optimized.
        $this->capturingPost([]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('body{color:red}');

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $out = $client->optimize(self::HTML);

        $this->assertSame(self::HTML, $out);
        $this->assertStringContainsString('<link rel="stylesheet" href="/a.css">', $out);
        $this->assertStringNotContainsString('wpmgr-used-css', $out);
    }

    public function test_returns_unchanged_on_empty_body(): void
    {
        $this->capturingPost([]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $this->assertSame(self::HTML, $client->optimize(self::HTML));
    }

    public function test_inlines_used_css_and_defers_stylesheets_on_200(): void
    {
        // 200 body IS the used CSS (Content-Type: text/css).
        $this->capturingPost([]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('body{color:red}');

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $out = $client->optimize(self::HTML);

        // Used CSS inlined.
        $this->assertStringContainsString('<style id="wpmgr-used-css">body{color:red}</style>', $out);
        // Original stylesheet deferred (href renamed) — full CSS still loads, non-blocking.
        $this->assertStringContainsString('data-wpmgr-href="/a.css"', $out);
        $this->assertStringNotContainsString(' href="/a.css"', $out);
    }

    public function test_gzip_encoded_used_css_is_decoded_on_200(): void
    {
        $this->capturingPost([]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn((string) gzencode('body{color:green}'));
        Functions\when('wp_remote_retrieve_header')->alias(
            static fn ($r, $h) => strtolower((string) $h) === 'content-encoding' ? 'gzip' : ''
        );

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $out = $client->optimize(self::HTML);

        $this->assertStringContainsString('<style id="wpmgr-used-css">body{color:green}</style>', $out);
    }

    public function test_posts_multipart_to_agent_v1_rucss_with_signature(): void
    {
        $this->capturingPost([]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $client->optimize(self::HTML);

        $this->assertIsArray($this->lastPostArgs);
        // Hit the real CP endpoint.
        $this->assertSame('https://cp.example.com/agent/v1/rucss', $this->lastPostArgs['__url']);

        $headers = $this->lastPostArgs['headers'] ?? [];
        $this->assertIsArray($headers);
        // multipart content type with a boundary.
        $this->assertStringStartsWith('multipart/form-data; boundary=', (string) ($headers['Content-Type'] ?? ''));
        // Ed25519 signature headers present.
        $this->assertArrayHasKey(Signer::HEADER_SIGNATURE, $headers);
        $this->assertArrayHasKey(Signer::HEADER_KEY, $headers);

        // Body carries the three named parts + the JSON meta keys.
        $body = (string) ($this->lastPostArgs['body'] ?? '');
        $this->assertStringContainsString('name="meta"', $body);
        $this->assertStringContainsString('name="html"', $body);
        $this->assertStringContainsString('name="css"', $body);
        $this->assertStringContainsString('"site_id"', $body);
        $this->assertStringContainsString('"structure_hash"', $body);
        $this->assertStringContainsString('"url"', $body);
    }

    public function test_optimize_never_throws_even_if_remote_post_throws(): void
    {
        // wp_remote_post itself blows up — the catch-all must still hold.
        Functions\when('wp_remote_post')->alias(static function () {
            throw new \RuntimeException('boom');
        });

        $client = new RucssClient($this->workingSigner(), $this->settings(), [], $this->urlHelper());
        $out = $client->optimize(self::HTML); // must not throw
        $this->assertSame(self::HTML, $out);
    }
}
