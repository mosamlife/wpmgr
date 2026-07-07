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
        // Defensive: a test that throws mid-assertion (e.g. the wp_die marker
        // exception) must never leak $wpdb or REMOTE_ADDR into a later test.
        unset($GLOBALS['wpdb'], $_SERVER['REMOTE_ADDR']);
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

    // =========================================================================
    // P0 gap #3 (outcome-test-debt audit, GH #170 Wave 1):
    //
    // Everything above this point tests the CIDR/config/plumbing primitives in
    // isolation; LoginProtection::onAuthenticate() -- the actual enforcement
    // gate that returns a WP_Error to block a login -- was never invoked. A
    // stub that always `return $user` (never blocks an attacker) would pass
    // every test above; a broken bypass that drops the allow-CIDR/private/
    // known-good short-circuits (locking out legitimate admins) would too.
    // The tests below drive onAuthenticate() directly with a scripted wpdb
    // double standing in for the sliding-window event counts.
    // =========================================================================

    /**
     * Seed get_option() so loadConfig() returns a fully-defaulted config with
     * the given mode/thresholds/CIDR lists. Any threshold key omitted from
     * $thresholds falls back to LoginProtection's own class defaults via
     * buildConfig() -- exactly as a partial CP push would behave in production.
     *
     * @param string              $mode
     * @param array<string,int>   $thresholds
     * @param list<string>        $allowCidrs
     * @param list<string>        $denyCidrs
     * @return void
     */
    private function configureProtection(
        string $mode,
        array $thresholds = [],
        array $allowCidrs = [],
        array $denyCidrs = []
    ): void {
        $config = [
            'mode'        => $mode,
            'thresholds'  => $thresholds,
            'ip_header'   => 'REMOTE_ADDR',
            'allow_cidrs' => $allowCidrs,
            'deny_cidrs'  => $denyCidrs,
        ];
        Functions\when('get_option')->justReturn((string) json_encode($config));
    }

    /**
     * Build a wpdb double whose get_var() returns a SCRIPTED count depending
     * on which getLoginCount() call is in flight -- distinguished by
     * (status, per-IP vs global) rather than by the literal SQL text (which
     * embeds a time()-derived cutoff we don't want to hardcode). Mirrors the
     * three query shapes onAuthenticate() actually issues:
     *   - status=STATUS_SUCCESS, per-IP        -> known-good bypass count
     *   - status=STATUS_FAILURE, global (no ip) -> all_blocked count
     *   - status=STATUS_FAILURE, per-IP         -> temp_block / captcha count
     *
     * A bare `SELECT COUNT(*) FROM {table}` (no WHERE -- enforceRowCap()'s
     * row-cap probe, fired after every insert()) always answers '0' so the
     * row-cap eviction path is never spuriously triggered by a scripted count.
     *
     * @param int $successPerIp Recent STATUS_SUCCESS count for this IP.
     * @param int $failureGlobal Site-wide STATUS_FAILURE count.
     * @param int $failurePerIp Per-IP STATUS_FAILURE count.
     * @return object
     */
    private function makeScriptedLoginEventsWpdb(int $successPerIp = 0, int $failureGlobal = 0, int $failurePerIp = 0): object
    {
        return new class ($successPerIp, $failureGlobal, $failurePerIp) {
            public string $prefix = 'wp_';

            /** @var array<int,mixed> */
            private array $lastArgs = [];

            public function __construct(
                private readonly int $successPerIp,
                private readonly int $failureGlobal,
                private readonly int $failurePerIp
            ) {
            }

            /** @return string */
            public function prepare(string $query, mixed ...$args): string
            {
                $this->lastArgs = $args;
                return $query;
            }

            /** @return string */
            public function get_var(string $query): string
            {
                if (!str_contains($query, 'WHERE')) {
                    // enforceRowCap()'s bare COUNT(*) probe.
                    return '0';
                }

                $status  = (int) ($this->lastArgs[0] ?? 0);
                $isPerIp = str_contains($query, 'AND ip = %s');

                if ($status === LoginProtection::STATUS_SUCCESS && $isPerIp) {
                    return (string) $this->successPerIp;
                }
                if ($status === LoginProtection::STATUS_FAILURE && !$isPerIp) {
                    return (string) $this->failureGlobal;
                }
                if ($status === LoginProtection::STATUS_FAILURE && $isPerIp) {
                    return (string) $this->failurePerIp;
                }
                return '0';
            }

            /** @return int */
            public function insert(string $table, array $data, array $format = []): int
            {
                return 1;
            }

            /** @return int */
            public function query(string $query): int
            {
                return 0;
            }
        };
    }

    // -------------------------------------------------------------------------
    // Escalating block tiers (each triggered independently)
    // -------------------------------------------------------------------------

    public function test_onauthenticate_blocks_when_perip_failures_at_temp_block_limit(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        $lp           = $this->makeProtection();
        $originalUser = new \stdClass();
        $result       = $lp->onAuthenticate($originalUser, 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'temp-block tier must return a WP_Error');
        $this->assertSame('wpmgr_temp_blocked', $result->get_error_code());
    }

    public function test_onauthenticate_blocks_with_captcha_when_perip_failures_at_captcha_limit_below_temp(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 3);

        $lp     = $this->makeProtection();
        $result = $lp->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'captcha tier must return a WP_Error');
        $this->assertSame('wpmgr_captcha_block', $result->get_error_code());
    }

    public function test_onauthenticate_blocks_all_when_global_failures_at_block_all_limit(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        // Global count already at the limit; per-IP count would only reach
        // captcha tier, proving the GLOBAL check is evaluated first (most
        // severe tier wins) regardless of the per-IP count.
        $GLOBALS['wpdb'] = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 100, failurePerIp: 1);

        $lp     = $this->makeProtection();
        $result = $lp->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'all-blocked tier must return a WP_Error');
        $this->assertSame('wpmgr_all_blocked', $result->get_error_code());
    }

    public function test_onauthenticate_blocks_ip_on_deny_cidrs_match(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            [],
            [],
            ['203.0.113.0/24']
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb();

        $lp     = $this->makeProtection();
        $result = $lp->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'a deny_cidrs match must return a WP_Error');
        $this->assertSame('wpmgr_ip_blocked', $result->get_error_code());
    }

    // -------------------------------------------------------------------------
    // Pass-through paths (must NEVER lock out a legitimate admin)
    // -------------------------------------------------------------------------

    public function test_onauthenticate_passes_through_unchanged_for_allow_cidrs_match(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100],
            ['203.0.113.0/24']
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        // Even with failure counts far past every threshold, the allow-CIDR
        // bypass must short-circuit before any count is even consulted.
        $GLOBALS['wpdb'] = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 999, failurePerIp: 999);

        $lp           = $this->makeProtection();
        $originalUser = new \stdClass();
        $result       = $lp->onAuthenticate($originalUser, 'admin', 'anything');

        $this->assertSame($originalUser, $result, 'allow_cidrs match must return $user unchanged, never a WP_Error');
    }

    public function test_onauthenticate_passes_through_unchanged_for_private_ip(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '10.0.0.5';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 999, failurePerIp: 999);

        $lp           = $this->makeProtection();
        $originalUser = new \stdClass();
        $result       = $lp->onAuthenticate($originalUser, 'admin', 'anything');

        $this->assertSame($originalUser, $result, 'a private/LAN IP must never be blocked, regardless of failure counts');
    }

    public function test_onauthenticate_passes_through_unchanged_for_recent_known_good_success(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        // A recent successful login from this IP bypasses the check even
        // though the failure counts alone would otherwise trigger every tier.
        $GLOBALS['wpdb'] = $this->makeScriptedLoginEventsWpdb(successPerIp: 1, failureGlobal: 999, failurePerIp: 999);

        $lp           = $this->makeProtection();
        $originalUser = new \stdClass();
        $result       = $lp->onAuthenticate($originalUser, 'admin', 'anything');

        $this->assertSame($originalUser, $result, 'a recent known-good success from this IP must bypass all block tiers');
    }

    public function test_onauthenticate_passes_through_unchanged_when_counts_are_below_every_threshold(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 1, failurePerIp: 1);

        $lp           = $this->makeProtection();
        $originalUser = new \stdClass();
        $result       = $lp->onAuthenticate($originalUser, 'admin', 'anything');

        $this->assertSame($originalUser, $result, 'sub-threshold counts must return $user unchanged');
    }

    // -------------------------------------------------------------------------
    // PROTECT mode terminates the request; AUDIT mode only records + returns.
    // -------------------------------------------------------------------------

    public function test_onauthenticate_protect_mode_calls_wp_die_with_403_on_block(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_PROTECT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        $wpDieArgs = null;
        Functions\when('wp_die')->alias(function ($message, $title = '', $args = []) use (&$wpDieArgs) {
            $wpDieArgs = $args;
            throw new \RuntimeException('wp_die called');
        });

        $lp = $this->makeProtection();

        $threw = false;
        try {
            $lp->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');
        } catch (\RuntimeException $e) {
            $threw = $e->getMessage() === 'wp_die called';
        }

        $this->assertTrue($threw, 'PROTECT mode must call wp_die() on a block');
        $this->assertIsArray($wpDieArgs);
        $this->assertSame(403, $wpDieArgs['response'] ?? null, 'PROTECT mode must terminate with a 403 response');
    }

    public function test_onauthenticate_audit_mode_never_calls_wp_die_on_block(): void
    {
        $this->configureProtection(
            LoginProtection::MODE_AUDIT,
            ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100]
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->makeScriptedLoginEventsWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        $wpDieCalled = false;
        Functions\when('wp_die')->alias(function ($message, $title = '', $args = []) use (&$wpDieCalled) {
            $wpDieCalled = true;
            throw new \RuntimeException('wp_die called');
        });

        $lp     = $this->makeProtection();
        $result = $lp->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertFalse($wpDieCalled, 'AUDIT mode must never call wp_die()');
        $this->assertInstanceOf(\WP_Error::class, $result, 'AUDIT mode must still return a WP_Error as a safety net');
        $this->assertSame('wpmgr_temp_blocked', $result->get_error_code());
    }
}
