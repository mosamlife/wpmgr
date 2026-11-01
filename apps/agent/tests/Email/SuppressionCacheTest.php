<?php
/**
 * SuppressionCacheTest — validates delta pull, hash storage, cursor advancement,
 * and the is_suppressed() lookup, all without real network I/O.
 *
 * Stubs: wp_remote_get (Brain Monkey), get_option / update_option (Brain Monkey),
 *        Settings / Signer (real instances backed by a temp-file Keystore,
 *        same pattern as EmailLogReporterTest).
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\SuppressionCache;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;

/**
 * @covers \WPMgr\Agent\Email\SuppressionCache
 */
class SuppressionCacheTest extends TestCase
{
    /** @var string */
    private string $keyFile = '';

    /** @var array<string,mixed> */
    private array $optionStore = [];

    private Settings $settings;
    private Signer   $signer;

    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();

        $this->optionStore = [];

        Functions\when('get_option')->alias(
            fn ($k, $d = false) => $this->optionStore[$k] ?? $d
        );
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('delete_option')->alias(function ($k) {
            unset($this->optionStore[$k]);
            return true;
        });
        Functions\when('get_site_option')->justReturn('__wpmgr_settings_missing__');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_json_encode')->alias(static fn ($v) => json_encode($v));

        // Build real Signer from a temp-file Keystore (same as EmailLogReporterTest).
        $this->keyFile = sys_get_temp_dir() . '/wpmgr-suppression-test-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test-only setup, no WP_Filesystem context
        file_put_contents((string) constant('WPMGR_AGENT_KEY_FILE'), random_bytes(32));

        $keystore = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $this->signer = new Signer($keystore);

        $this->optionStore[Settings::OPTION_SITE_ID] = 'test-site-uuid';
        $this->optionStore[Settings::OPTION_CP_URL]  = 'https://cp.example.com';
        $this->settings = new Settings();
    }

    protected function tearDown(): void
    {
        if ($this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only cleanup
        }
        Monkey\tearDown();
        parent::tearDown();
    }

    // -------------------------------------------------------------------------
    // Option key and hook name constants
    // -------------------------------------------------------------------------

    public function test_option_cursor_key_is_correct(): void
    {
        $this->assertSame('wpmgr_email_suppression_cursor', SuppressionCache::OPTION_CURSOR);
    }

    public function test_option_hashes_key_is_correct(): void
    {
        $this->assertSame('wpmgr_email_suppression_hashes', SuppressionCache::OPTION_HASHES);
    }

    public function test_hook_pull_name_is_correct(): void
    {
        $this->assertSame('wpmgr_email_suppression_pull', SuppressionCache::HOOK_PULL);
    }

    // -------------------------------------------------------------------------
    // is_suppressed() without a pull (empty cache)
    // -------------------------------------------------------------------------

    public function test_is_suppressed_returns_false_when_cache_empty(): void
    {
        $cache = new SuppressionCache($this->settings, $this->signer);
        $this->assertFalse($cache->is_suppressed('anyone@example.com'));
    }

    public function test_is_suppressed_returns_false_on_empty_string(): void
    {
        $cache = new SuppressionCache($this->settings, $this->signer);
        $this->assertFalse($cache->is_suppressed(''));
    }

    // -------------------------------------------------------------------------
    // pull() — delta ingestion
    // -------------------------------------------------------------------------

    /**
     * CP returns one active entry; after the pull is_suppressed() matches.
     */
    public function test_pull_stores_active_hash_and_advances_cursor(): void
    {
        $suppressed_email = 'bounce@example.com';
        $hash             = hash('sha256', strtolower($suppressed_email));

        $pull_time = 1700000100;

        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200],
            'body'     => json_encode([
                'entries'   => [
                    ['email_hash' => $hash, 'active' => true],
                ],
                'pull_time' => $pull_time,
            ]),
        ]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->alias(
            fn ($r) => $r['body'] ?? ''
        );

        $cache = new SuppressionCache($this->settings, $this->signer);
        $cache->pull();

        // Hash should be in the option.
        $this->assertTrue($cache->is_suppressed($suppressed_email), 'suppressed email must be flagged');
        $this->assertFalse($cache->is_suppressed('safe@example.com'), 'unlisted email must not be flagged');

        // Cursor must be advanced to pull_time.
        $this->assertSame($pull_time, $this->optionStore[SuppressionCache::OPTION_CURSOR] ?? null);
    }

    /**
     * CP returns an entry with active=false; it must be removed from the cache.
     */
    public function test_pull_removes_hash_when_active_false(): void
    {
        $email = 'previously-suppressed@example.com';
        $hash  = hash('sha256', strtolower($email));

        // Pre-populate the cache with this hash.
        $this->optionStore[SuppressionCache::OPTION_HASHES] = json_encode([$hash]);

        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200],
            'body'     => json_encode([
                'entries'   => [
                    ['email_hash' => $hash, 'active' => false],
                ],
                'pull_time' => 1700000200,
            ]),
        ]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->alias(fn ($r) => $r['body'] ?? '');

        $cache = new SuppressionCache($this->settings, $this->signer);

        // Before pull the email is suppressed.
        $this->assertTrue($cache->is_suppressed($email));

        $cache->pull();

        // After pull (active=false) the email must be cleared.
        // Re-create the cache to force a fresh load_hashes() from the updated option.
        $freshCache = new SuppressionCache($this->settings, $this->signer);
        $this->assertFalse($freshCache->is_suppressed($email), 'removed suppression must not be flagged');
    }

    /**
     * When the CP returns a non-2xx response the cursor must not be advanced.
     */
    public function test_pull_does_not_advance_cursor_on_http_error(): void
    {
        Functions\when('wp_remote_get')->justReturn(['response' => ['code' => 503], 'body' => '']);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(503);

        $cache = new SuppressionCache($this->settings, $this->signer);
        $cache->pull();

        $this->assertArrayNotHasKey(
            SuppressionCache::OPTION_CURSOR,
            $this->optionStore,
            'cursor must not be written on a failed pull'
        );
    }

    /**
     * pull() is a no-op when the site is not enrolled.
     */
    public function test_pull_noop_when_not_enrolled(): void
    {
        Functions\expect('wp_remote_get')->never();

        unset($this->optionStore[Settings::OPTION_SITE_ID]);
        $unenrolled = new Settings();
        $cache      = new SuppressionCache($unenrolled, $this->signer);
        $cache->pull();

        $this->assertArrayNotHasKey(SuppressionCache::OPTION_CURSOR, $this->optionStore);
    }

    /**
     * Entries with invalid hashes (wrong length / non-hex) must be silently skipped.
     */
    public function test_pull_ignores_invalid_hash_entries(): void
    {
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200],
            'body'     => json_encode([
                'entries'   => [
                    ['email_hash' => 'not-a-valid-hash', 'active' => true],
                    ['email_hash' => '', 'active' => true],
                    ['email_hash' => str_repeat('g', 64), 'active' => true], // non-hex
                ],
                'pull_time' => 1700000300,
            ]),
        ]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->alias(fn ($r) => $r['body'] ?? '');

        $cache = new SuppressionCache($this->settings, $this->signer);
        $cache->pull();

        // No hashes stored (all invalid); cursor still advanced.
        $stored = $this->optionStore[SuppressionCache::OPTION_HASHES] ?? '[]';
        $hashes = json_decode((string) $stored, true);
        $this->assertIsArray($hashes);
        $this->assertCount(0, $hashes, 'invalid hashes must not be stored');
        $this->assertSame(1700000300, $this->optionStore[SuppressionCache::OPTION_CURSOR] ?? null);
    }

    // -------------------------------------------------------------------------
    // is_suppressed() — case-insensitive matching
    // -------------------------------------------------------------------------

    public function test_is_suppressed_is_case_insensitive(): void
    {
        $email = 'Bounce@Example.Com';
        $hash  = hash('sha256', strtolower($email)); // bounce@example.com

        $this->optionStore[SuppressionCache::OPTION_HASHES] = json_encode([$hash]);

        $cache = new SuppressionCache($this->settings, $this->signer);

        $this->assertTrue($cache->is_suppressed('bounce@example.com'));
        $this->assertTrue($cache->is_suppressed('BOUNCE@EXAMPLE.COM'));
        $this->assertTrue($cache->is_suppressed($email));
    }
}
