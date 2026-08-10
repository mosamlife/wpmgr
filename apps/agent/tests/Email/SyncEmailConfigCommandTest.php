<?php
/**
 * Tests for SyncEmailConfigCommand: config persistence + keystore secret storage.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Commands\SyncEmailConfigCommand;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Tests\Email\FakeKeystore;

/**
 * @covers \WPMgr\Agent\Commands\SyncEmailConfigCommand
 */
class SyncEmailConfigCommandTest extends TestCase
{
    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    private function make_keystore(): FakeKeystore
    {
        return new FakeKeystore();
    }

    public function test_name_is_correct(): void
    {
        $cmd = new SyncEmailConfigCommand($this->make_keystore());
        $this->assertSame('sync_email_config', $cmd->name());
    }

    public function test_rejects_invalid_provider(): void
    {
        $cmd    = new SyncEmailConfigCommand($this->make_keystore());
        $result = $cmd->execute([], ['provider' => 'not_a_provider']);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('provider', $result['detail']);
    }

    public function test_rejects_non_string_provider(): void
    {
        $cmd    = new SyncEmailConfigCommand($this->make_keystore());
        $result = $cmd->execute([], ['provider' => 42]);

        $this->assertFalse($result['ok']);
    }

    public function test_rejects_non_array_config(): void
    {
        $cmd    = new SyncEmailConfigCommand($this->make_keystore());
        $result = $cmd->execute([], ['provider' => 'smtp', 'config' => 'bad']);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('config', $result['detail']);
    }

    public function test_rejects_non_array_mappings(): void
    {
        $cmd    = new SyncEmailConfigCommand($this->make_keystore());
        $result = $cmd->execute([], ['provider' => 'smtp', 'mappings' => 'bad']);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('mappings', $result['detail']);
    }

    public function test_rejects_non_string_secret(): void
    {
        $cmd    = new SyncEmailConfigCommand($this->make_keystore());
        $result = $cmd->execute([], ['provider' => 'smtp', 'secret' => 42]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('secret', $result['detail']);
    }

    public function test_stores_config_and_secret_on_valid_payload(): void
    {
        $stored_option = null;

        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->alias(
            function (string $key, $value) use (&$stored_option) {
                if ($key === EmailConfig::OPTION) {
                    $stored_option = $value;
                }
                return true;
            }
        );
        // sanitize_email and sanitize_text_field are already stubbed in bootstrap.php.

        $keystore = new FakeKeystore();
        $cmd      = new SyncEmailConfigCommand($keystore);
        $result   = $cmd->execute([], [
            'provider'    => 'sendgrid',
            'from_address' => 'hello@example.com',
            'from_name'   => 'My Site',
            'log_emails'  => true,
            'secret'      => 'my-api-key',
        ]);

        $this->assertTrue($result['ok']);
        $this->assertStringContainsString('saved', $result['detail']);
        $this->assertIsArray($stored_option);
        $this->assertSame('sendgrid', $stored_option['provider'] ?? null);
        // Assert that the secret was stored in the fake keystore.
        $this->assertContains('my-api-key', $keystore->stored);
    }

    public function test_explicit_clear_secret_clears_keystore_entry(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        $keystore = new FakeKeystore('existing-secret');
        $cmd      = new SyncEmailConfigCommand($keystore);
        $result   = $cmd->execute([], ['provider' => 'smtp', 'secret' => '', 'clear_secret' => true]);

        $this->assertTrue($result['ok']);
        $this->assertStringContainsString('cleared', $result['detail']);
        $this->assertContains('', $keystore->stored);
    }

    public function test_succeeds_when_secret_field_absent(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        $keystore = new FakeKeystore();
        $cmd      = new SyncEmailConfigCommand($keystore);
        $result   = $cmd->execute([], ['provider' => 'postmark']);

        $this->assertTrue($result['ok']);
        // GH #380: an absent 'secret' must not touch the keystore at all. This
        // test previously asserted the opposite, which is how the defect shipped.
        $this->assertSame([], $keystore->stored);
    }

    public function test_connection_secret_survives_a_push_that_omits_it(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        $keystore                        = new FakeKeystore();
        $keystore->conn_secrets['relay'] = 'relay-password';

        $cmd    = new SyncEmailConfigCommand($keystore);
        $result = $cmd->execute([], [
            'provider'    => 'smtp',
            'connections' => [
                'relay' => ['provider' => 'smtp', 'config' => ['host' => 'relay.example.com']],
            ],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame('relay-password', $keystore->get_connection_secret('relay'));
    }

    public function test_connection_secret_is_dropped_on_explicit_clear(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        $keystore                        = new FakeKeystore();
        $keystore->conn_secrets['relay'] = 'relay-password';

        $cmd    = new SyncEmailConfigCommand($keystore);
        $result = $cmd->execute([], [
            'provider'    => 'smtp',
            'connections' => [
                'relay' => ['provider' => 'smtp', 'config' => [], 'clear_secret' => true],
            ],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame('', $keystore->get_connection_secret('relay'));
    }
}
