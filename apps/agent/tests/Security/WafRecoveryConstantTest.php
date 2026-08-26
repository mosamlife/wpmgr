<?php
/**
 * GH #529, layer (0): the WAF mu-plugin's IP gate must honour the operator's
 * documented recovery constant.
 *
 * `define('WPMGR_DISABLE_SITE_2FA', true)` in wp-config.php is this plugin's
 * last-resort escape hatch for an administrator it has locked out. Site2faModule,
 * PasswordPolicyModule and HardeningModule's auth appliers all honoured it; the
 * WAF gate did not. An admin whose ordinary public IP had landed in deny_cidrs
 * or hardening_deny_cidrs therefore still met a 403 before WordPress booted,
 * with the escape hatch set and nothing to tell them it had done nothing.
 *
 * The constant is readable at mu-plugin time: wp-config.php runs to completion —
 * it is what requires wp-settings.php — so every define() in it exists before
 * wp-settings.php line 1, and the mu-plugin include loop is line 498 of the
 * WordPress 7.0.4 tree.
 *
 * WHY THESE RUN IN SEPARATE PROCESSES. A constant cannot be undefined. Defining
 * WPMGR_DISABLE_SITE_2FA in the shared test process would switch the gate off
 * for every later test in the run — including WafGateHardeningTest, whose whole
 * job is to prove the gate denies — and PHPUnit's file order would decide
 * whether the suite passed. @runInSeparateProcess with @preserveGlobalState
 * disabled contains the define to one child process. Site2faModuleTest already
 * defines the same constant in the shared process, which is exactly why the
 * "still denies" assertions below must never share a process with anything.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers wpmgr_waf_should_deny
 */
final class WafRecoveryConstantTest extends TestCase
{
    /**
     * A config with both deny lists populated: one brute-force entry (mode-gated)
     * and one explicit operator hardening ban (mode-independent).
     *
     * @return array<string,mixed>
     */
    private static function config(): array
    {
        return [
            'deny_cidrs'           => ['203.0.113.10/32'],
            'hardening_deny_cidrs' => ['198.51.100.0/24'],
            'allow_cidrs'          => [],
        ];
    }

    /**
     * Load the real mu-plugin's function definitions without firing its
     * top-level gate() call (which needs $wpdb).
     */
    private static function loadWaf(): void
    {
        if (!defined('WPMGR_WAF_TESTING')) {
            define('WPMGR_WAF_TESTING', true);
        }
        if (!function_exists('wpmgr_waf_should_deny')) {
            require_once dirname(__DIR__, 2) . '/mu-plugin-loader/a-wpmgr-waf.php';
        }
    }

    /**
     * The recovery constant releases the gate — both deny layers, both modes.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_releases_the_ip_gate(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);
        self::loadWaf();

        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '203.0.113.10', 'protect'),
            'with the recovery constant set, a brute-force deny_cidrs hit must pass'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '198.51.100.7', 'off'),
            'with the recovery constant set, an operator hardening ban must pass'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '198.51.100.7', 'protect'),
            'with the recovery constant set, no layer may deny'
        );
    }

    /**
     * OVER-FIRE GUARD, and the load-bearing half. Without the constant — which
     * is every ordinary site, because setting it needs write access to
     * wp-config.php — the gate denies exactly as before.
     *
     * The first assertion is specifically brute-force protection on the login
     * page: deny_cidrs in protect mode is what the login-protection subsystem
     * populates after repeated failed logins. GH #529 exempts wp-login.php from
     * the USER-AGENT ban only; it deliberately does NOT exempt it from the IP
     * gate, because blocking repeated login attempts by IP is that gate's entire
     * job and a path exemption would delete the protection it exists to provide.
     * If this assertion ever flips, the fix has become the bug.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_without_the_constant_the_gate_still_denies(): void
    {
        self::loadWaf();

        $this->assertFalse(
            defined('WPMGR_DISABLE_SITE_2FA'),
            'this test is only meaningful in a process where the constant is absent'
        );

        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '203.0.113.10', 'protect'),
            'brute-force protection must still deny a banned IP — including on wp-login.php'
        );
        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '198.51.100.7', 'off'),
            'an explicit operator hardening ban must still deny in every mode'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '192.0.2.55', 'protect'),
            'an unrelated public IP must still pass'
        );
    }

    /**
     * The allow-list and the private/loopback bypass keep working underneath the
     * new layer, so the ordering of layers (1) and (2) is unchanged.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_existing_bypasses_are_unchanged(): void
    {
        self::loadWaf();

        $config                = self::config();
        $config['allow_cidrs'] = ['203.0.113.0/24'];

        $this->assertFalse(
            wpmgr_waf_should_deny($config, '203.0.113.10', 'protect'),
            'allow_cidrs must still win over deny_cidrs'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '10.0.0.5', 'protect'),
            'private addresses must still bypass the gate'
        );
    }
}
