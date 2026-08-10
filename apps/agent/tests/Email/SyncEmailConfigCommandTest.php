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

    /**
     * The carry-forward is a read-modify-write over an option with replace-all
     * semantics, so a read-back that comes up empty must not be written. The
     * keystore answers '' both for "nothing stored" and for "stored but it
     * would not decrypt this request", and writing the resulting empty map
     * deletes the ciphertext outright: the exact loss the carry-forward exists
     * to prevent.
     */
    public function test_a_carry_forward_push_that_reads_back_nothing_writes_nothing(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        // The keystore reports no readable secret for 'relay'.
        $keystore = new FakeKeystore();

        $cmd    = new SyncEmailConfigCommand($keystore);
        $result = $cmd->execute([], [
            'provider'    => 'smtp',
            'connections' => [
                'relay' => ['provider' => 'smtp', 'config' => ['host' => 'relay.example.com']],
            ],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame(
            [],
            $keystore->stored_conn_secrets,
            'a push carrying no connection secret must not overwrite the stored map with an empty one'
        );
    }

    public function test_an_emptied_registry_still_clears_the_connection_secrets(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        $keystore                        = new FakeKeystore();
        $keystore->conn_secrets['relay'] = 'relay-password';

        $cmd    = new SyncEmailConfigCommand($keystore);
        $result = $cmd->execute([], ['provider' => 'smtp', 'connections' => []]);

        $this->assertTrue($result['ok']);
        $this->assertSame(
            [[]],
            $keystore->stored_conn_secrets,
            'dropping every connection is a deliberate replace-all, not a failed read-back'
        );
        $this->assertSame('', $keystore->get_connection_secret('relay'));
    }

    /**
     * A connections map whose entries are all unreadable is not an emptied
     * registry. Both arrive as "connections is present", but one is an operator
     * decision and the other is a payload we cannot read, and the registry has
     * replace-all semantics: reading the unreadable one as a decision deletes
     * every stored connection credential.
     */
    public function test_a_connections_map_of_unreadable_entries_is_refused(): void
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

        $keystore                         = new FakeKeystore('the-working-password');
        $keystore->conn_secrets['relay']  = 'relay-password';
        $keystore->conn_secrets['backup'] = 'backup-password';

        $cmd    = new SyncEmailConfigCommand($keystore);
        $result = $cmd->execute([], [
            'provider'    => 'smtp',
            'secret'      => 'a-rotated-password',
            'connections' => [
                'relay'  => 'not-an-object',
                'backup' => 42,
            ],
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('connections', $result['detail']);
        $this->assertSame(
            [],
            $keystore->stored_conn_secrets,
            'an unreadable registry must never be written through as an empty one'
        );
        $this->assertSame('relay-password', $keystore->get_connection_secret('relay'));
        $this->assertSame('backup-password', $keystore->get_connection_secret('backup'));
        $this->assertSame([], $keystore->stored, 'the refusal must land before anything is settled');
        $this->assertSame('the-working-password', $keystore->get_email_secret());
        $this->assertNull($stored_option, 'no settings may be written from a payload we could not read');
    }

    /**
     * One unreadable entry is enough. In a replace-all registry an entry we
     * cannot read is an entry we would silently drop, and a dropped connection
     * legitimately loses its secret — so a single unreadable entry can cost a
     * credential the operator never retired.
     */
    public function test_a_single_unreadable_connection_entry_refuses_the_whole_push(): void
    {
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? [] : false);
        Functions\when('update_option')->justReturn(true);

        $keystore                         = new FakeKeystore();
        $keystore->conn_secrets['relay']  = 'relay-password';
        $keystore->conn_secrets['backup'] = 'backup-password';

        $cmd    = new SyncEmailConfigCommand($keystore);
        $result = $cmd->execute([], [
            'provider'    => 'smtp',
            'connections' => [
                'relay'  => ['provider' => 'smtp', 'config' => ['host' => 'relay.example.com']],
                'backup' => 'not-an-object',
            ],
        ]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('connections', $result['detail']);
        $this->assertSame([], $keystore->stored_conn_secrets);
        $this->assertSame('relay-password', $keystore->get_connection_secret('relay'));
        $this->assertSame('backup-password', $keystore->get_connection_secret('backup'));
    }

    /**
     * The settings write is the second half of a two-store push whose first
     * half has already committed. When it does not land, the keystore holds the
     * new credential while the option still names the old endpoint, and the
     * next send offers the new credential to the old server. Report it.
     */
    public function test_a_settings_write_that_does_not_land_is_reported_as_a_failure(): void
    {
        $old = (new EmailConfig([
            'provider' => 'smtp',
            'config'   => ['host' => 'smtp.old.example', 'port' => 587],
        ]))->to_array();

        // The option store refuses the write and keeps answering with the old value.
        Functions\when('get_option')->alias(fn($key) => $key === EmailConfig::OPTION ? $old : false);
        Functions\when('update_option')->justReturn(false);

        $keystore = new FakeKeystore('the-old-password');
        $cmd      = new SyncEmailConfigCommand($keystore);
        $result   = $cmd->execute([], [
            'provider' => 'smtp',
            'config'   => ['host' => 'smtp.new.example', 'port' => 587],
            'secret'   => 'the-new-password',
        ]);

        $this->assertFalse($result['ok'], 'a settings write that did not land must not report success');
        $this->assertStringContainsString('not saved', $result['detail']);
        // The hazard this reports: the credential is already rotated while the
        // endpoint the option names is still the old one.
        $this->assertSame('the-new-password', $keystore->get_email_secret());
    }

    /**
     * update_option() answers false for two unrelated reasons, and the second
     * one is the ordinary case here: rotating only a password changes no
     * setting, so the value handed to update_option equals the stored one and
     * the write is skipped. Reading that as a failure would break every routine
     * rotation, which is worse than the bug above.
     */
    public function test_a_rotation_that_changes_no_setting_still_reports_success(): void
    {
        // false is what WordPress answers for an option that is not set, so it
        // doubles as the initial state here.
        $store = false;

        Functions\when('get_option')->alias(
            function (string $key) use (&$store) {
                return $key === EmailConfig::OPTION ? $store : false;
            }
        );
        // Mirrors WP: false when the new value equals the stored one, and no write.
        Functions\when('update_option')->alias(
            function (string $key, $value) use (&$store) {
                if ($key !== EmailConfig::OPTION) {
                    return true;
                }
                if ($store === $value) {
                    return false;
                }
                $store = $value;
                return true;
            }
        );

        $payload = [
            'provider' => 'smtp',
            'config'   => ['host' => 'smtp.example.com', 'port' => 587, 'auth' => true, 'username' => 'postmaster'],
        ];

        $keystore = new FakeKeystore('the-old-password');
        $cmd      = new SyncEmailConfigCommand($keystore);

        // First push settles the settings.
        $this->assertTrue($cmd->execute([], $payload + ['secret' => 'the-old-password'])['ok']);

        // Second push rotates only the password: identical settings, so
        // update_option writes nothing and answers false.
        $result = $cmd->execute([], $payload + ['secret' => 'the-new-password']);

        $this->assertTrue($result['ok'], 'an unchanged-settings rotation is a successful push');
        $this->assertSame('the-new-password', $keystore->get_email_secret());
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
