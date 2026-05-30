<?php
/**
 * Unit tests for LoginBrand::loadConfig() validation and
 * LoginBrand::applyConfig() URL-scheme rejection.
 *
 * These tests exercise only the pure PHP behaviour of LoginBrand and
 * SyncLoginBrandCommand; they do not touch the DB and do not require a live
 * WordPress install. Brain Monkey stubs the handful of WP functions the classes
 * reference (get_option, update_option, esc_url_raw, esc_url, wp_kses).
 *
 * Coverage targets:
 *   - loadConfig(): safe defaults when option is absent / corrupt JSON /
 *     missing keys; per-instance cache is cleared by applyConfig().
 *   - applyConfig(): valid http/https URLs are accepted; non-http(s) schemes
 *     (javascript:, data:, ftp:) are rejected (coerced to empty); message is
 *     length-capped and kses-sanitized.
 *   - SyncLoginBrandCommand::execute(): command name; wrong-type fields rejected;
 *     all-absent body accepted; delegates to LoginBrand::applyConfig().
 *
 * NOTE: To run these tests, install dev dependencies first:
 *   composer install --dev
 * then run:
 *   vendor/bin/phpunit tests/LoginBrandTest.php
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\SyncLoginBrandCommand;
use WPMgr\Agent\Support\LoginBrand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\LoginBrand
 * @covers \WPMgr\Agent\Commands\SyncLoginBrandCommand
 */
final class LoginBrandTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helper
    // -------------------------------------------------------------------------

    private function makeBrand(): LoginBrand
    {
        return new LoginBrand();
    }

    // -------------------------------------------------------------------------
    // loadConfig() — defaults when option is absent
    // -------------------------------------------------------------------------

    public function test_loadConfig_returns_empty_defaults_when_option_absent(): void
    {
        Functions\when('get_option')->justReturn(null);

        $brand  = $this->makeBrand();
        $config = $brand->loadConfig();

        $this->assertSame('', $config['logo_url']);
        $this->assertSame('', $config['logo_link']);
        $this->assertSame('', $config['message']);
    }

    // -------------------------------------------------------------------------
    // loadConfig() — corrupt JSON falls back to defaults
    // -------------------------------------------------------------------------

    public function test_loadConfig_falls_back_on_corrupt_json(): void
    {
        Functions\when('get_option')->justReturn('not-valid-json{{{');

        $brand  = $this->makeBrand();
        $config = $brand->loadConfig();

        $this->assertSame('', $config['logo_url']);
        $this->assertSame('', $config['logo_link']);
        $this->assertSame('', $config['message']);
    }

    // -------------------------------------------------------------------------
    // loadConfig() — missing keys fall back to empty strings
    // -------------------------------------------------------------------------

    public function test_loadConfig_tolerates_partial_option(): void
    {
        // Only logo_url present; logo_link and message should default to "".
        $encoded = (string) json_encode(['logo_url' => 'https://example.com/logo.png']);
        Functions\when('get_option')->justReturn($encoded);

        $brand  = $this->makeBrand();
        $config = $brand->loadConfig();

        $this->assertSame('https://example.com/logo.png', $config['logo_url']);
        $this->assertSame('', $config['logo_link']);
        $this->assertSame('', $config['message']);
    }

    // -------------------------------------------------------------------------
    // loadConfig() — per-instance cache is cleared by applyConfig()
    // -------------------------------------------------------------------------

    public function test_loadConfig_cache_invalidated_by_applyConfig(): void
    {
        // First get_option returns empty; after applyConfig the option is updated.
        Functions\when('get_option')->justReturn(null);
        Functions\when('update_option')->justReturn(true);
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_kses')->returnArg();

        $brand = $this->makeBrand();

        // First load: all empty (from null option).
        $first = $brand->loadConfig();
        $this->assertSame('', $first['logo_url']);

        // applyConfig clears cache; next loadConfig() re-reads wp-options.
        // Stub get_option to return the new value from this point on.
        $encoded = (string) json_encode([
            'logo_url'  => 'https://example.com/logo.png',
            'logo_link' => 'https://example.com',
            'message'   => '',
        ]);
        Functions\when('get_option')->justReturn($encoded);

        $brand->applyConfig('https://example.com/logo.png', 'https://example.com', '');

        $second = $brand->loadConfig();
        $this->assertSame('https://example.com/logo.png', $second['logo_url']);
    }

    // -------------------------------------------------------------------------
    // applyConfig() — valid http URLs are accepted
    // -------------------------------------------------------------------------

    public function test_applyConfig_accepts_valid_https_urls(): void
    {
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_kses')->returnArg();

        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value): bool {
                $decoded = json_decode($value, true);
                \PHPUnit\Framework\TestCase::assertSame('https://cdn.example.com/logo.png', $decoded['logo_url']);
                \PHPUnit\Framework\TestCase::assertSame('https://example.com', $decoded['logo_link']);
                return true;
            });

        $brand = $this->makeBrand();
        $brand->applyConfig('https://cdn.example.com/logo.png', 'https://example.com', '');
    }

    // -------------------------------------------------------------------------
    // applyConfig() — non-http(s) scheme for logo_url is rejected
    // -------------------------------------------------------------------------

    public function test_applyConfig_rejects_javascript_scheme_in_logo_url(): void
    {
        // esc_url_raw with allowed ['http','https'] would return '' for javascript:
        // We simulate that behaviour here.
        Functions\when('esc_url_raw')->justReturn('');
        Functions\when('wp_kses')->returnArg();

        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value): bool {
                $decoded = json_decode($value, true);
                // logo_url must be coerced to empty string.
                \PHPUnit\Framework\TestCase::assertSame('', $decoded['logo_url']);
                return true;
            });

        $brand = $this->makeBrand();
        $brand->applyConfig('javascript:alert(1)', 'https://example.com', '');
    }

    // -------------------------------------------------------------------------
    // applyConfig() — data: scheme for logo_link is rejected
    // -------------------------------------------------------------------------

    public function test_applyConfig_rejects_data_scheme_in_logo_link(): void
    {
        // esc_url_raw returns empty for data: URIs when only http/https allowed.
        Functions\when('esc_url_raw')->justReturn('');
        Functions\when('wp_kses')->returnArg();

        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value): bool {
                $decoded = json_decode($value, true);
                \PHPUnit\Framework\TestCase::assertSame('', $decoded['logo_link']);
                return true;
            });

        $brand = $this->makeBrand();
        $brand->applyConfig('', 'data:text/html,<script>alert(1)</script>', '');
    }

    // -------------------------------------------------------------------------
    // applyConfig() — ftp: scheme is rejected
    // -------------------------------------------------------------------------

    public function test_applyConfig_rejects_ftp_scheme(): void
    {
        Functions\when('esc_url_raw')->justReturn('');
        Functions\when('wp_kses')->returnArg();

        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value): bool {
                $decoded = json_decode($value, true);
                \PHPUnit\Framework\TestCase::assertSame('', $decoded['logo_url']);
                return true;
            });

        $brand = $this->makeBrand();
        $brand->applyConfig('ftp://files.example.com/logo.png', '', '');
    }

    // -------------------------------------------------------------------------
    // applyConfig() — message is length-capped and kses'd
    // -------------------------------------------------------------------------

    public function test_applyConfig_truncates_long_message(): void
    {
        Functions\when('esc_url_raw')->returnArg();

        $longMessage = str_repeat('A', 3000); // Over the 2000-char limit.

        Functions\when('wp_kses')->alias(function (string $data): string {
            // Verify the message was truncated to 2000 before kses.
            \PHPUnit\Framework\TestCase::assertSame(2000, strlen($data));
            return $data;
        });

        Functions\when('update_option')->justReturn(true);

        $brand = $this->makeBrand();
        $brand->applyConfig('', '', $longMessage);

        $this->addToAssertionCount(1);
    }

    // -------------------------------------------------------------------------
    // applyConfig() — empty strings are stored as-is (clear the brand)
    // -------------------------------------------------------------------------

    public function test_applyConfig_stores_all_empty_to_clear_brand(): void
    {
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_kses')->returnArg();

        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value): bool {
                $decoded = json_decode($value, true);
                \PHPUnit\Framework\TestCase::assertSame('', $decoded['logo_url']);
                \PHPUnit\Framework\TestCase::assertSame('', $decoded['logo_link']);
                \PHPUnit\Framework\TestCase::assertSame('', $decoded['message']);
                return true;
            });

        $brand = $this->makeBrand();
        $brand->applyConfig('', '', '');
    }

    // -------------------------------------------------------------------------
    // SyncLoginBrandCommand — name()
    // -------------------------------------------------------------------------

    public function test_sync_login_brand_command_name(): void
    {
        $cmd = new SyncLoginBrandCommand($this->makeBrand());
        $this->assertSame('sync_login_brand', $cmd->name());
    }

    // -------------------------------------------------------------------------
    // SyncLoginBrandCommand — wrong type for logo_url
    // -------------------------------------------------------------------------

    public function test_sync_login_brand_rejects_non_string_logo_url(): void
    {
        $cmd = new SyncLoginBrandCommand($this->makeBrand());
        $res = $cmd->execute([], ['logo_url' => 12345]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('logo_url', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncLoginBrandCommand — wrong type for logo_link
    // -------------------------------------------------------------------------

    public function test_sync_login_brand_rejects_non_string_logo_link(): void
    {
        $cmd = new SyncLoginBrandCommand($this->makeBrand());
        $res = $cmd->execute([], ['logo_link' => true]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('logo_link', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncLoginBrandCommand — wrong type for message
    // -------------------------------------------------------------------------

    public function test_sync_login_brand_rejects_non_string_message(): void
    {
        $cmd = new SyncLoginBrandCommand($this->makeBrand());
        $res = $cmd->execute([], ['message' => ['array', 'not', 'allowed']]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('message', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncLoginBrandCommand — all fields absent (empty body) is valid
    // -------------------------------------------------------------------------

    public function test_sync_login_brand_accepts_empty_body(): void
    {
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_kses')->returnArg();
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncLoginBrandCommand($this->makeBrand());
        $res = $cmd->execute([], []);

        $this->assertTrue($res['ok']);
        $this->assertSame('login brand applied', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncLoginBrandCommand — success path with all fields
    // -------------------------------------------------------------------------

    public function test_sync_login_brand_success_with_all_fields(): void
    {
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_kses')->returnArg();
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncLoginBrandCommand($this->makeBrand());
        $res = $cmd->execute([], [
            'logo_url'  => 'https://cdn.example.com/logo.png',
            'logo_link' => 'https://example.com',
            'message'   => '<strong>Welcome</strong> to our site.',
        ]);

        $this->assertTrue($res['ok']);
        $this->assertSame('login brand applied', $res['detail']);
    }
}
