<?php
/**
 * WafGateHardeningTest — exercises the real WAF gate decision function
 * (wpmgr_waf_should_deny) extracted from the mu-plugin, and the
 * HardeningModule::syncWafDenyCidrs() broad/private CIDR filter.
 *
 * FIX A (MEDIUM): the test now calls the REAL wpmgr_waf_should_deny() defined
 * in a-wpmgr-waf.php rather than a hand-copied replica. Any reordering of the
 * five gate layers inside the mu-plugin will immediately break these tests,
 * making them a genuine regression guard.
 *
 * The mu-plugin file defines functions and ends with a WPMGR_WAF_TESTING-gated
 * wpmgr_waf_gate() call. Defining the constant before require_once ensures we
 * load the function definitions without triggering the top-level gate() call
 * (which would fail in the test environment where $wpdb is not available).
 *
 * FIX B (LOW-1): test that syncWafDenyCidrs() drops broad/private CIDRs before
 * persisting into hardening_deny_cidrs, while letting valid public CIDRs through.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\HardeningConfig;
use WPMgr\Agent\Security\HardeningModule;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

// Guard: define the testing constant BEFORE requiring the mu-plugin so the
// top-level wpmgr_waf_gate() call at the bottom of the file is skipped.
// The constant must be defined at file-load time (before any test class is
// instantiated) because require_once runs once per process.
if (!defined('WPMGR_WAF_TESTING')) {
    define('WPMGR_WAF_TESTING', true);
}

// Load the real mu-plugin — defines wpmgr_waf_should_deny() and its helpers.
// function_exists() guard makes this idempotent if another test already loaded it.
if (!function_exists('wpmgr_waf_should_deny')) {
    require_once dirname(__DIR__) . '/mu-plugin-loader/a-wpmgr-waf.php';
}

/**
 * PROCESS ISOLATION IS LOAD-BEARING HERE (GH #529).
 *
 * wpmgr_waf_should_deny() now returns false whenever WPMGR_DISABLE_SITE_2FA is
 * defined — the operator's documented recovery constant, honoured at the IP
 * gate so a locked-out admin has a way back in. A constant cannot be undefined,
 * and Site2faModuleTest defines this one in the SHARED test process
 * (tests/Security/Site2faModuleTest.php, test_safety_disable_constant_makes_
 * install_noop). PHPUnit's file order puts that before this file, so without
 * isolation every "must block" assertion below silently stopped testing the
 * gate and started testing the escape hatch — four tests passing green while
 * asserting nothing about what they name.
 *
 * `@runTestsInSeparateProcesses` is the CLASS-level form (the per-test spelling
 * is `@runInSeparateProcess` and is silently ignored on a class), applied here rather
 * than to the four affected tests so a test added later inherits the guarantee
 * instead of quietly rotting the moment someone else's suite defines the
 * constant first.
 *
 * @covers wpmgr_waf_should_deny
 * @covers \WPMgr\Agent\Security\HardeningModule::syncWafDenyCidrs
 *
 * @runTestsInSeparateProcesses
 * @preserveGlobalState disabled
 */
final class WafGateHardeningTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $optionStore = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->optionStore = [];

        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->optionStore[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('wp_json_encode')->alias(fn ($v) => json_encode($v));
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helper: call the REAL gate decision function (not a replica)
    // -------------------------------------------------------------------------

    /**
     * Thin helper so test bodies stay readable. Delegates entirely to the real
     * wpmgr_waf_should_deny() loaded from a-wpmgr-waf.php above.
     *
     * @param array<string,mixed> $config  wpmgr_security_config structure.
     * @param string              $ip      Client IP address to test.
     * @return bool True → blocked, false → passed.
     */
    private function decide(array $config, string $ip): bool
    {
        $mode = isset($config['mode']) && is_string($config['mode']) ? $config['mode'] : 'protect';
        return wpmgr_waf_should_deny($config, $ip, $mode);
    }

    // -------------------------------------------------------------------------
    // FIX A: ITEM 5 — hardening bans enforce regardless of mode
    // -------------------------------------------------------------------------

    /**
     * ITEM 5: a public IP that matches hardening_deny_cidrs must be blocked
     * even when login-protection mode is 'disabled'.
     */
    public function test_hardening_ban_blocks_in_disabled_mode(): void
    {
        $config = [
            'mode'                 => 'disabled',
            'hardening_deny_cidrs' => ['203.0.113.0/24'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => [],
        ];
        $this->assertTrue(
            $this->decide($config, '203.0.113.50'),
            'Hardening ban must block in disabled mode'
        );
    }

    /**
     * ITEM 5: a public IP that matches hardening_deny_cidrs must be blocked
     * even when login-protection mode is 'audit'.
     */
    public function test_hardening_ban_blocks_in_audit_mode(): void
    {
        $config = [
            'mode'                 => 'audit',
            'hardening_deny_cidrs' => ['198.51.100.0/24'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => [],
        ];
        $this->assertTrue(
            $this->decide($config, '198.51.100.1'),
            'Hardening ban must block in audit mode'
        );
    }

    /**
     * ITEM 5: a public IP that matches hardening_deny_cidrs must also block in
     * protect mode (consistent across all modes).
     */
    public function test_hardening_ban_blocks_in_protect_mode(): void
    {
        $config = [
            'mode'                 => 'protect',
            'hardening_deny_cidrs' => ['203.0.113.0/24'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => [],
        ];
        $this->assertTrue(
            $this->decide($config, '203.0.113.99'),
            'Hardening ban must block in protect mode'
        );
    }

    /**
     * A non-banned public IP must NOT be blocked by the hardening layer.
     */
    public function test_non_banned_ip_is_not_blocked_by_hardening(): void
    {
        $config = [
            'mode'                 => 'disabled',
            'hardening_deny_cidrs' => ['203.0.113.0/24'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => [],
        ];
        $this->assertFalse(
            $this->decide($config, '192.0.2.1'),
            'Non-banned public IP must pass through in disabled mode'
        );
    }

    // -------------------------------------------------------------------------
    // Safety rails: allow_cidrs and private/loopback bypass in all modes
    // -------------------------------------------------------------------------

    /**
     * An IP in allow_cidrs must bypass the hardening ban in protect mode.
     * allow_cidrs always wins over any deny list (layer 1 of the gate).
     */
    public function test_allow_cidr_bypasses_hardening_ban(): void
    {
        $config = [
            'mode'                 => 'protect',
            'hardening_deny_cidrs' => ['203.0.113.0/24'],
            'deny_cidrs'           => ['203.0.113.0/24'],
            'allow_cidrs'          => ['203.0.113.50/32'],
        ];
        $this->assertFalse(
            $this->decide($config, '203.0.113.50'),
            'allow_cidrs must bypass hardening ban'
        );
    }

    /**
     * An IP in allow_cidrs must bypass the hardening ban in disabled mode too.
     */
    public function test_allow_cidr_bypasses_hardening_ban_in_disabled_mode(): void
    {
        $config = [
            'mode'                 => 'disabled',
            'hardening_deny_cidrs' => ['203.0.113.0/24'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => ['203.0.113.50/32'],
        ];
        $this->assertFalse(
            $this->decide($config, '203.0.113.50'),
            'allow_cidrs must bypass hardening ban in disabled mode'
        );
    }

    /**
     * A private (RFC-1918) IP must bypass the hardening ban regardless of mode.
     * LAN admin must never be locked out (gate layer 2).
     */
    public function test_private_ip_bypasses_hardening_ban_in_disabled_mode(): void
    {
        $config = [
            'mode'                 => 'disabled',
            'hardening_deny_cidrs' => ['10.0.0.0/8'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => [],
        ];
        $this->assertFalse(
            $this->decide($config, '10.1.2.3'),
            'Private IP must bypass hardening ban in disabled mode'
        );
    }

    /**
     * Loopback (127.0.0.1) must bypass hardening ban in all modes.
     */
    public function test_loopback_bypasses_hardening_ban(): void
    {
        $config = [
            'mode'                 => 'protect',
            'hardening_deny_cidrs' => ['127.0.0.0/8'],
            'deny_cidrs'           => [],
            'allow_cidrs'          => [],
        ];
        $this->assertFalse(
            $this->decide($config, '127.0.0.1'),
            'Loopback must bypass hardening ban'
        );
    }

    // -------------------------------------------------------------------------
    // Brute-force protect gate: unchanged behaviour
    // -------------------------------------------------------------------------

    /**
     * deny_cidrs (brute-force path) must still block in protect mode.
     */
    public function test_deny_cidrs_blocks_in_protect_mode(): void
    {
        $config = [
            'mode'                 => 'protect',
            'hardening_deny_cidrs' => [],
            'deny_cidrs'           => ['203.0.113.0/24'],
            'allow_cidrs'          => [],
        ];
        $this->assertTrue(
            $this->decide($config, '203.0.113.7'),
            'deny_cidrs must still block in protect mode'
        );
    }

    /**
     * deny_cidrs must NOT block in disabled mode — it is mode-gated.
     * Only hardening_deny_cidrs is mode-independent (gate layer 3 vs 4-5).
     */
    public function test_deny_cidrs_does_not_block_in_disabled_mode(): void
    {
        $config = [
            'mode'                 => 'disabled',
            'hardening_deny_cidrs' => [],
            'deny_cidrs'           => ['203.0.113.0/24'],
            'allow_cidrs'          => [],
        ];
        $this->assertFalse(
            $this->decide($config, '203.0.113.7'),
            'deny_cidrs must not block in disabled mode (brute-force gate is mode-gated)'
        );
    }

    // -------------------------------------------------------------------------
    // FIX B: syncWafDenyCidrs() broad/private CIDR filter
    // -------------------------------------------------------------------------

    /**
     * A broad CIDR (IPv4 prefix < /8) handed to syncWafDenyCidrs() must NOT
     * appear in hardening_deny_cidrs after sync.
     */
    public function test_sync_waf_deny_cidrs_drops_broad_ipv4_cidr(): void
    {
        $this->optionStore['wpmgr_security_config'] = (string) json_encode([
            'mode'        => 'protect',
            'deny_cidrs'  => [],
            'allow_cidrs' => [],
        ]);

        $config = HardeningConfig::fromArray([
            'bans' => [
                ['id' => 'b1', 'type' => 'range', 'value' => '0.0.0.0/0', 'comment' => ''],
                ['id' => 'b2', 'type' => 'range', 'value' => '1.0.0.0/7', 'comment' => ''],
            ],
        ]);

        $module = new HardeningModule();
        $module->syncWafDenyCidrs($config);

        $wafDecoded = json_decode($this->optionStore['wpmgr_security_config'] ?? '{}', true);
        $hardeningCidrs = $wafDecoded['hardening_deny_cidrs'] ?? [];

        $this->assertNotContains('0.0.0.0/0', $hardeningCidrs, '0.0.0.0/0 (all-address) must be dropped');
        $this->assertNotContains('1.0.0.0/7',  $hardeningCidrs, '/7 prefix (< /8) must be dropped');
        $this->assertSame([], $hardeningCidrs, 'All broad CIDRs must be filtered out; result must be empty');
    }

    /**
     * A broad CIDR (IPv6 prefix < /16) must be dropped before persisting.
     */
    public function test_sync_waf_deny_cidrs_drops_broad_ipv6_cidr(): void
    {
        $this->optionStore['wpmgr_security_config'] = (string) json_encode([
            'mode'        => 'protect',
            'deny_cidrs'  => [],
            'allow_cidrs' => [],
        ]);

        $config = HardeningConfig::fromArray([
            'bans' => [
                ['id' => 'b1', 'type' => 'range', 'value' => '::/0',   'comment' => ''],
                ['id' => 'b2', 'type' => 'range', 'value' => '2001::/15', 'comment' => ''],
            ],
        ]);

        $module = new HardeningModule();
        $module->syncWafDenyCidrs($config);

        $wafDecoded = json_decode($this->optionStore['wpmgr_security_config'] ?? '{}', true);
        $hardeningCidrs = $wafDecoded['hardening_deny_cidrs'] ?? [];

        $this->assertNotContains('::/0',      $hardeningCidrs, '::/0 (all-address IPv6) must be dropped');
        $this->assertNotContains('2001::/15', $hardeningCidrs, '/15 prefix (< /16) must be dropped');
        $this->assertSame([], $hardeningCidrs, 'All broad IPv6 CIDRs must be filtered out; result must be empty');
    }

    /**
     * A private (RFC-1918) CIDR must be dropped before persisting — even though
     * the runtime WAF also enforces the private bypass, the agent must not store it.
     */
    public function test_sync_waf_deny_cidrs_drops_private_cidr(): void
    {
        $this->optionStore['wpmgr_security_config'] = (string) json_encode([
            'mode'        => 'protect',
            'deny_cidrs'  => [],
            'allow_cidrs' => [],
        ]);

        $config = HardeningConfig::fromArray([
            'bans' => [
                ['id' => 'b1', 'type' => 'range', 'value' => '10.0.0.0/8',     'comment' => ''],
                ['id' => 'b2', 'type' => 'range', 'value' => '192.168.0.0/16', 'comment' => ''],
                ['id' => 'b3', 'type' => 'range', 'value' => '::1/128',        'comment' => ''],
            ],
        ]);

        $module = new HardeningModule();
        $module->syncWafDenyCidrs($config);

        $wafDecoded = json_decode($this->optionStore['wpmgr_security_config'] ?? '{}', true);
        $hardeningCidrs = $wafDecoded['hardening_deny_cidrs'] ?? [];

        $this->assertNotContains('10.0.0.0/8',     $hardeningCidrs, 'RFC-1918 10/8 must be dropped');
        $this->assertNotContains('192.168.0.0/16',  $hardeningCidrs, 'RFC-1918 192.168/16 must be dropped');
        $this->assertNotContains('::1/128',          $hardeningCidrs, 'IPv6 loopback must be dropped');
        $this->assertSame([], $hardeningCidrs, 'All private/loopback CIDRs must be filtered out');
    }

    /**
     * A valid public CIDR must pass through syncWafDenyCidrs() and appear in
     * hardening_deny_cidrs after sync. Broad/private filtering must not drop it.
     */
    public function test_sync_waf_deny_cidrs_keeps_valid_public_cidr(): void
    {
        $this->optionStore['wpmgr_security_config'] = (string) json_encode([
            'mode'        => 'protect',
            'deny_cidrs'  => [],
            'allow_cidrs' => [],
        ]);

        $config = HardeningConfig::fromArray([
            'bans' => [
                ['id' => 'b1', 'type' => 'ip',    'value' => '203.0.113.10',   'comment' => ''],
                ['id' => 'b2', 'type' => 'range',  'value' => '198.51.100.0/24', 'comment' => ''],
            ],
        ]);

        $module = new HardeningModule();
        $module->syncWafDenyCidrs($config);

        $wafDecoded = json_decode($this->optionStore['wpmgr_security_config'] ?? '{}', true);
        $hardeningCidrs = $wafDecoded['hardening_deny_cidrs'] ?? [];

        $this->assertContains('203.0.113.10',    $hardeningCidrs, 'Valid public IP must survive the filter');
        $this->assertContains('198.51.100.0/24', $hardeningCidrs, 'Valid public range must survive the filter');
    }

    /**
     * Mixed list: broad/private CIDRs are dropped while valid public ones survive.
     */
    public function test_sync_waf_deny_cidrs_mixed_list_drops_bad_keeps_good(): void
    {
        $this->optionStore['wpmgr_security_config'] = (string) json_encode([
            'mode'        => 'protect',
            'deny_cidrs'  => [],
            'allow_cidrs' => [],
        ]);

        $config = HardeningConfig::fromArray([
            'bans' => [
                ['id' => 'b1', 'type' => 'range', 'value' => '0.0.0.0/0',       'comment' => ''],  // broad — drop
                ['id' => 'b2', 'type' => 'range', 'value' => '10.0.0.0/8',      'comment' => ''],  // private — drop
                ['id' => 'b3', 'type' => 'ip',    'value' => '203.0.113.5',      'comment' => ''],  // public — keep
                ['id' => 'b4', 'type' => 'range', 'value' => '198.51.100.0/24',  'comment' => ''],  // public — keep
                ['id' => 'b5', 'type' => 'range', 'value' => '::1/128',          'comment' => ''],  // private — drop
            ],
        ]);

        $module = new HardeningModule();
        $module->syncWafDenyCidrs($config);

        $wafDecoded = json_decode($this->optionStore['wpmgr_security_config'] ?? '{}', true);
        $hardeningCidrs = $wafDecoded['hardening_deny_cidrs'] ?? [];

        $this->assertNotContains('0.0.0.0/0',      $hardeningCidrs, 'Broad all-address must be dropped');
        $this->assertNotContains('10.0.0.0/8',     $hardeningCidrs, 'Private 10/8 must be dropped');
        $this->assertNotContains('::1/128',          $hardeningCidrs, 'IPv6 loopback must be dropped');
        $this->assertContains('203.0.113.5',        $hardeningCidrs, 'Public IP must be kept');
        $this->assertContains('198.51.100.0/24',    $hardeningCidrs, 'Public range must be kept');
        $this->assertCount(2, $hardeningCidrs, 'Only the 2 public entries must survive');
    }
}
