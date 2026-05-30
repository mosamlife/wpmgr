<?php
/**
 * Unit tests for S2 — IpUtils CIDR matching, LoginProtection decision tree,
 * SyncSecurityConfigCommand, and UnblockIpCommand.
 *
 * These tests exercise only pure PHP behaviour; they do not touch a live DB or
 * WordPress install. Brain Monkey stubs the WP functions the classes reference
 * (get_option, update_option, delete_transient, wp_die, esc_html, __).
 *
 * Coverage targets:
 *   - IpUtils::cidrMatch(): IPv4 and IPv6 positive/negative cases, /0, /32, /128.
 *   - IpUtils::isPrivate(): loopback, RFC-1918, link-local, ULA, public.
 *   - LoginProtection::loadConfig(): safe defaults, corrupt JSON, invalid mode.
 *   - LoginProtection::applyConfig(): writes option, clears cache.
 *   - SyncSecurityConfigCommand::execute(): missing mode, invalid mode, type checks,
 *     success path.
 *   - UnblockIpCommand::execute(): missing ip, empty ip, invalid ip, success path.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\SyncSecurityConfigCommand;
use WPMgr\Agent\Commands\UnblockIpCommand;
use WPMgr\Agent\Support\IpUtils;
use WPMgr\Agent\Support\LoginProtection;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\IpUtils
 * @covers \WPMgr\Agent\Support\LoginProtection
 * @covers \WPMgr\Agent\Commands\SyncSecurityConfigCommand
 * @covers \WPMgr\Agent\Commands\UnblockIpCommand
 */
final class LoginProtectionTest extends TestCase
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

    private function makeProtection(): LoginProtection
    {
        return new LoginProtection(null);
    }

    // =========================================================================
    // IpUtils::cidrMatch() — IPv4
    // =========================================================================

    public function test_cidr_match_ipv4_positive(): void
    {
        $this->assertTrue(IpUtils::cidrMatch('192.168.1.50', '192.168.1.0/24'));
    }

    public function test_cidr_match_ipv4_negative(): void
    {
        $this->assertFalse(IpUtils::cidrMatch('192.168.2.1', '192.168.1.0/24'));
    }

    public function test_cidr_match_ipv4_slash32(): void
    {
        $this->assertTrue(IpUtils::cidrMatch('203.0.113.5', '203.0.113.5/32'));
        $this->assertFalse(IpUtils::cidrMatch('203.0.113.6', '203.0.113.5/32'));
    }

    public function test_cidr_match_ipv4_slash0_matches_any(): void
    {
        $this->assertTrue(IpUtils::cidrMatch('1.2.3.4', '0.0.0.0/0'));
    }

    public function test_cidr_match_ipv4_host_boundary(): void
    {
        // 10.0.0.255 is the last address of 10.0.0.0/24.
        $this->assertTrue(IpUtils::cidrMatch('10.0.0.255', '10.0.0.0/24'));
        // 10.0.1.0 is the first address of the NEXT /24.
        $this->assertFalse(IpUtils::cidrMatch('10.0.1.0', '10.0.0.0/24'));
    }

    // =========================================================================
    // IpUtils::cidrMatch() — IPv6
    // =========================================================================

    public function test_cidr_match_ipv6_positive(): void
    {
        $this->assertTrue(IpUtils::cidrMatch('2001:db8::1', '2001:db8::/32'));
    }

    public function test_cidr_match_ipv6_negative(): void
    {
        $this->assertFalse(IpUtils::cidrMatch('2001:db9::1', '2001:db8::/32'));
    }

    public function test_cidr_match_ipv6_slash128(): void
    {
        $this->assertTrue(IpUtils::cidrMatch('::1', '::1/128'));
        $this->assertFalse(IpUtils::cidrMatch('::2', '::1/128'));
    }

    public function test_cidr_match_ipv6_slash0_matches_any(): void
    {
        $this->assertTrue(IpUtils::cidrMatch('2001:db8::1', '::/0'));
    }

    // =========================================================================
    // IpUtils::cidrMatch() — invalid / mixed inputs
    // =========================================================================

    public function test_cidr_match_returns_false_for_empty_ip(): void
    {
        $this->assertFalse(IpUtils::cidrMatch('', '192.168.1.0/24'));
    }

    public function test_cidr_match_returns_false_for_empty_cidr(): void
    {
        $this->assertFalse(IpUtils::cidrMatch('192.168.1.1', ''));
    }

    public function test_cidr_match_returns_false_for_cidr_without_prefix(): void
    {
        $this->assertFalse(IpUtils::cidrMatch('192.168.1.1', '192.168.1.0'));
    }

    public function test_cidr_match_returns_false_for_mixed_families(): void
    {
        // IPv4 address against IPv6 CIDR (or vice-versa) — no match.
        $this->assertFalse(IpUtils::cidrMatch('192.168.1.1', '2001:db8::/32'));
        $this->assertFalse(IpUtils::cidrMatch('2001:db8::1', '192.168.1.0/24'));
    }

    // =========================================================================
    // IpUtils::isPrivate()
    // =========================================================================

    public function test_is_private_loopback_ipv4(): void
    {
        $this->assertTrue(IpUtils::isPrivate('127.0.0.1'));
    }

    public function test_is_private_rfc1918_10(): void
    {
        $this->assertTrue(IpUtils::isPrivate('10.0.0.1'));
    }

    public function test_is_private_rfc1918_172(): void
    {
        $this->assertTrue(IpUtils::isPrivate('172.16.0.1'));
        $this->assertTrue(IpUtils::isPrivate('172.31.255.255'));
    }

    public function test_is_private_rfc1918_192(): void
    {
        $this->assertTrue(IpUtils::isPrivate('192.168.0.1'));
    }

    public function test_is_private_link_local_ipv4(): void
    {
        $this->assertTrue(IpUtils::isPrivate('169.254.0.1'));
    }

    public function test_is_private_public_ipv4(): void
    {
        $this->assertFalse(IpUtils::isPrivate('203.0.113.1'));
        $this->assertFalse(IpUtils::isPrivate('8.8.8.8'));
    }

    public function test_is_private_loopback_ipv6(): void
    {
        $this->assertTrue(IpUtils::isPrivate('::1'));
    }

    public function test_is_private_ula_ipv6(): void
    {
        $this->assertTrue(IpUtils::isPrivate('fc00::1'));
        $this->assertTrue(IpUtils::isPrivate('fd12:3456:789a::1'));
    }

    public function test_is_private_link_local_ipv6(): void
    {
        $this->assertTrue(IpUtils::isPrivate('fe80::1'));
    }

    public function test_is_private_public_ipv6(): void
    {
        $this->assertFalse(IpUtils::isPrivate('2001:db8::1'));
    }

    public function test_is_private_empty_string(): void
    {
        $this->assertTrue(IpUtils::isPrivate(''));
    }

    // =========================================================================
    // LoginProtection::loadConfig() — defaults
    // =========================================================================

    public function test_load_config_returns_defaults_when_option_absent(): void
    {
        Functions\when('get_option')->justReturn(null);

        $lp     = $this->makeProtection();
        $config = $lp->loadConfig();

        // Inert by default: with no CP-pushed config the agent does nothing
        // until the operator enables protection from the dashboard.
        $this->assertSame(LoginProtection::MODE_DISABLED, $config['mode']);
        $this->assertSame('REMOTE_ADDR', $config['ip_header']);
        $this->assertSame([], $config['allow_cidrs']);
        $this->assertSame([], $config['deny_cidrs']);
        $this->assertIsArray($config['thresholds']);
        $this->assertSame(3, $config['thresholds']['captcha_limit']);
        $this->assertSame(10, $config['thresholds']['temp_block_limit']);
    }

    public function test_load_config_falls_back_on_corrupt_json(): void
    {
        Functions\when('get_option')->justReturn('{{not-json}}');

        $lp     = $this->makeProtection();
        $config = $lp->loadConfig();

        // Corrupt JSON → treated as no config → inert (disabled).
        $this->assertSame(LoginProtection::MODE_DISABLED, $config['mode']);
    }

    public function test_load_config_replaces_invalid_mode_with_disabled(): void
    {
        $stored = (string) json_encode([
            'mode'       => 'delete_everything',
            'thresholds' => [],
            'ip_header'  => 'REMOTE_ADDR',
            'allow_cidrs'=> [],
            'deny_cidrs' => [],
        ]);
        Functions\when('get_option')->justReturn($stored);

        $lp     = $this->makeProtection();
        $config = $lp->loadConfig();

        // An unrecognised mode falls back to the safe inert default.
        $this->assertSame(LoginProtection::MODE_DISABLED, $config['mode']);
    }

    public function test_load_config_accepts_disabled_mode(): void
    {
        $stored = (string) json_encode([
            'mode'       => 'disabled',
            'thresholds' => [],
            'ip_header'  => 'REMOTE_ADDR',
            'allow_cidrs'=> [],
            'deny_cidrs' => [],
        ]);
        Functions\when('get_option')->justReturn($stored);

        $lp     = $this->makeProtection();
        $config = $lp->loadConfig();

        $this->assertSame(LoginProtection::MODE_DISABLED, $config['mode']);
    }

    public function test_load_config_drops_invalid_cidr_entries(): void
    {
        $stored = (string) json_encode([
            'mode'        => 'audit',
            'thresholds'  => [],
            'ip_header'   => 'REMOTE_ADDR',
            'allow_cidrs' => ['192.168.1.0/24', 'not-a-cidr', ''],
            'deny_cidrs'  => ['10.0.0.0/8'],
        ]);
        Functions\when('get_option')->justReturn($stored);

        $lp     = $this->makeProtection();
        $config = $lp->loadConfig();

        // 'not-a-cidr' and '' must be dropped; valid CIDRs survive.
        $this->assertSame(['192.168.1.0/24'], $config['allow_cidrs']);
        $this->assertSame(['10.0.0.0/8'], $config['deny_cidrs']);
    }

    // =========================================================================
    // LoginProtection::applyConfig()
    // =========================================================================

    public function test_apply_config_persists_and_clears_cache(): void
    {
        Functions\when('get_option')->justReturn(null);
        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value): bool {
                $this->assertSame(LoginProtection::OPTION_CONFIG, $key);
                $decoded = json_decode($value, true);
                $this->assertSame('audit', $decoded['mode']);
                return true;
            });

        $lp = $this->makeProtection();
        $lp->applyConfig(['mode' => 'audit', 'thresholds' => [], 'ip_header' => 'REMOTE_ADDR', 'allow_cidrs' => [], 'deny_cidrs' => []]);

        // Config cache is cleared: next loadConfig() call would re-read the option.
        // We verify this by stubbing get_option again and checking the mode.
        $stored2 = (string) json_encode(['mode' => 'disabled', 'thresholds' => [], 'ip_header' => 'REMOTE_ADDR', 'allow_cidrs' => [], 'deny_cidrs' => []]);
        Functions\when('get_option')->justReturn($stored2);

        $config = $lp->loadConfig();
        $this->assertSame('disabled', $config['mode']);
    }

    // =========================================================================
    // SyncSecurityConfigCommand — name()
    // =========================================================================

    public function test_sync_security_config_command_name(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $this->assertSame('sync_security_config', $cmd->name());
    }

    // =========================================================================
    // SyncSecurityConfigCommand — validation rejections
    // =========================================================================

    public function test_sync_security_config_rejects_missing_mode(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], []);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('mode', $res['detail']);
    }

    public function test_sync_security_config_rejects_non_string_mode(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 42]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('string', $res['detail']);
    }

    public function test_sync_security_config_rejects_invalid_mode_value(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'superprotect']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('disabled', $res['detail']);
    }

    public function test_sync_security_config_rejects_non_array_thresholds(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'protect', 'thresholds' => 'bad']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('thresholds', $res['detail']);
    }

    public function test_sync_security_config_rejects_non_array_allow_cidrs(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'protect', 'allow_cidrs' => 'not-an-array']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('allow_cidrs', $res['detail']);
    }

    public function test_sync_security_config_rejects_non_array_deny_cidrs(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'protect', 'deny_cidrs' => 'not-an-array']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('deny_cidrs', $res['detail']);
    }

    public function test_sync_security_config_rejects_empty_ip_header(): void
    {
        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'protect', 'ip_header' => '   ']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('ip_header', $res['detail']);
    }

    // =========================================================================
    // SyncSecurityConfigCommand — success paths
    // =========================================================================

    public function test_sync_security_config_success_protect(): void
    {
        Functions\when('get_option')->justReturn(null);
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], [
            'mode'        => 'protect',
            'thresholds'  => ['captcha_limit' => 5, 'temp_block_limit' => 15, 'block_all_limit' => 200, 'failed_login_gap' => 3600, 'success_login_gap' => 3600, 'all_blocked_gap' => 3600],
            'ip_header'   => 'HTTP_CF_CONNECTING_IP',
            'allow_cidrs' => ['203.0.113.0/24'],
            'deny_cidrs'  => ['198.51.100.0/24'],
        ]);

        $this->assertTrue($res['ok']);
        $this->assertSame('security config applied', $res['detail']);
    }

    public function test_sync_security_config_success_disabled(): void
    {
        Functions\when('get_option')->justReturn(null);
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'disabled']);

        $this->assertTrue($res['ok']);
    }

    public function test_sync_security_config_success_audit(): void
    {
        Functions\when('get_option')->justReturn(null);
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncSecurityConfigCommand($this->makeProtection());
        $res = $cmd->execute([], ['mode' => 'audit']);

        $this->assertTrue($res['ok']);
    }

    // =========================================================================
    // UnblockIpCommand — name()
    // =========================================================================

    public function test_unblock_ip_command_name(): void
    {
        $cmd = new UnblockIpCommand($this->makeProtection());
        $this->assertSame('unblock_ip', $cmd->name());
    }

    // =========================================================================
    // UnblockIpCommand — validation rejections
    // =========================================================================

    public function test_unblock_ip_rejects_missing_ip(): void
    {
        $cmd = new UnblockIpCommand($this->makeProtection());
        $res = $cmd->execute([], []);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('ip', $res['detail']);
    }

    public function test_unblock_ip_rejects_non_string_ip(): void
    {
        $cmd = new UnblockIpCommand($this->makeProtection());
        $res = $cmd->execute([], ['ip' => 12345]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('string', $res['detail']);
    }

    public function test_unblock_ip_rejects_empty_ip(): void
    {
        $cmd = new UnblockIpCommand($this->makeProtection());
        $res = $cmd->execute([], ['ip' => '   ']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('empty', $res['detail']);
    }

    public function test_unblock_ip_rejects_invalid_ip_string(): void
    {
        $cmd = new UnblockIpCommand($this->makeProtection());
        $res = $cmd->execute([], ['ip' => 'not-an-ip-address']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('valid', $res['detail']);
    }

    // =========================================================================
    // UnblockIpCommand — success paths
    // =========================================================================

    public function test_unblock_ip_success_ipv4(): void
    {
        // No wpdb present — unblockIp() returns early after the transient delete.
        // delete_transient must be stubbed.
        Functions\when('delete_transient')->justReturn(true);

        $cmd = new UnblockIpCommand($this->makeProtection());
        $res = $cmd->execute([], ['ip' => '203.0.113.5']);

        $this->assertTrue($res['ok']);
        $this->assertStringContainsString('203.0.113.5', $res['detail']);
    }

    public function test_unblock_ip_success_ipv6(): void
    {
        Functions\when('delete_transient')->justReturn(true);

        $cmd = new UnblockIpCommand($this->makeProtection());
        $res = $cmd->execute([], ['ip' => '2001:db8::1']);

        $this->assertTrue($res['ok']);
        $this->assertStringContainsString('2001:db8::1', $res['detail']);
    }

    // =========================================================================
    // LoginProtection — getLoginCount() guards
    // =========================================================================

    public function test_get_login_count_returns_zero_without_wpdb(): void
    {
        Functions\when('get_option')->justReturn(null);

        $lp    = $this->makeProtection();
        $count = $lp->getLoginCount(LoginProtection::STATUS_FAILURE, '1.2.3.4', time(), 1800);

        $this->assertSame(0, $count);
    }

    public function test_get_login_count_uses_wpdb(): void
    {
        Functions\when('get_option')->justReturn(null);

        $wpdbSpy = new class {
            public string $prefix = 'wp_';

            /** @return string */
            public function prepare(string $q, mixed ...$args): string
            {
                return $q;
            }

            /** @return string */
            public function get_var(string $q): string
            {
                return '7';
            }
        };
        $GLOBALS['wpdb'] = $wpdbSpy;

        $lp    = $this->makeProtection();
        $count = $lp->getLoginCount(LoginProtection::STATUS_FAILURE, '1.2.3.4', time(), 1800);

        $this->assertSame(7, $count);
        unset($GLOBALS['wpdb']);
    }
}
