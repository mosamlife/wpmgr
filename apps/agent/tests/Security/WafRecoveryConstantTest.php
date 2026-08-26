<?php
/**
 * GH #529: the WAF mu-plugin's gate must NOT honour the recovery constant, and
 * these tests hold that shut from both directions.
 *
 * A recovery layer was added to wpmgr_waf_should_deny() and has since been
 * removed. It was added on the belief that deny_cidrs was auto-populated by
 * brute-force protection. It is not — it arrives from the control plane through
 * SyncSecurityConfigCommand as "always-block ranges", and nothing in this plugin
 * ever writes to it. Both lists this gate enforces (deny_cidrs and
 * hardening_deny_cidrs) are therefore operator policy.
 *
 * While the layer existed, `define('WPMGR_DISABLE_SITE_2FA', true)` in
 * wp-config.php silently disabled the operator's always-block ranges on every
 * request before WordPress booted. On a managed site the party who can edit
 * wp-config.php is frequently NOT the operator — an agency's client, a host's
 * tenant — so that let the managed party override the manager. A constant in
 * wp-config.php proves local file access, never operator authority.
 *
 * The constant's legitimate job — releasing the escalating lockout this plugin
 * computes out of its own {prefix}wpmgr_login_events rows — cannot be done here
 * at all: this file has no access to those rows. It is done in
 * LoginProtection::onAuthenticate() step 6b, proved by
 * LoginProtectionRecoveryConstantTest.
 *
 * The constant is readable at mu-plugin time: wp-config.php runs to completion —
 * it is what requires wp-settings.php — so every define() in it exists before
 * wp-settings.php line 1, and the mu-plugin include loop is line 498 of the
 * WordPress 7.0.4 tree. Readability was never the issue; scope was.
 *
 * WHY THESE RUN IN SEPARATE PROCESSES. A constant cannot be undefined. Defining
 * WPMGR_DISABLE_SITE_2FA in the shared test process would switch the gate off
 * for every later test in the run — including WafGateHardeningTest, whose whole
 * job is to prove the gate denies — and PHPUnit's file order would decide
 * whether the suite passed. `@runInSeparateProcess` with `@preserveGlobalState`
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
     * THE TENANCY GUARD, and the reason the recovery layer was removed from this
     * gate entirely.
     *
     * deny_cidrs is NOT auto-populated by brute-force protection. It arrives
     * from the control plane through SyncSecurityConfigCommand as "always-block
     * ranges" — the operator's policy, pushed to a site whose local
     * administrator may be a different party (an agency's client, a host's
     * tenant). A constant in wp-config.php proves local file access, never
     * operator authority.
     *
     * While a recovery layer existed above this check, setting the constant
     * silently disabled those ranges on every request before WordPress booted,
     * letting the managed party override the manager. If this assertion ever
     * flips, that hole is back.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_never_releases_control_plane_deny_cidrs(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);
        self::loadWaf();

        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '203.0.113.10', 'protect'),
            'the recovery constant must NOT release a control-plane deny_cidrs range'
        );
    }

    /**
     * The corollary: this gate has no recovery bypass at all, because it
     * enforces only operator policy. The event-backed lockout the constant DOES
     * release is counted from {prefix}wpmgr_login_events, which this file cannot
     * even read — that release lives in LoginProtection::onAuthenticate() step
     * 6b and is proved by LoginProtectionRecoveryConstantTest.
     *
     * Asserted as behaviour rather than by grepping the source: with the
     * constant set, every layer decides exactly as it does without it.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_changes_no_decision_in_this_gate(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);
        self::loadWaf();

        // Same four probes as test_without_the_constant_the_gate_still_denies().
        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '203.0.113.10', 'protect'),
            'deny_cidrs in protect mode: unchanged by the constant'
        );
        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '198.51.100.7', 'off'),
            'hardening ban in any mode: unchanged by the constant'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '192.0.2.55', 'protect'),
            'unrelated public IP: unchanged by the constant'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '203.0.113.10', 'off'),
            'deny_cidrs outside protect mode: unchanged by the constant'
        );
    }

    /**
     * THE BLAST-RADIUS GUARD. Placed at layer (0) the constant returned false
     * before any list was consulted, so a constant named DISABLE_SITE_2FA also
     * deleted the operator's explicit IP ban list, in every mode, with no
     * signal. On nginx those bans have no other enforcement path at all.
     *
     * hardening_deny_cidrs is the operator's standing instruction about traffic,
     * not a lock on the owner's own door, and it is removed by removing it. This
     * is the same boundary HardeningModule::authPolicyDisabled() draws: the
     * constant gates who may log in, never the explicit bans.
     *
     * If either assertion flips, the recovery lever has become a kill switch.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_never_releases_an_explicit_hardening_ban(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);
        self::loadWaf();

        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '198.51.100.7', 'off'),
            'the recovery constant must NOT release an explicit operator hardening ban'
        );
        $this->assertTrue(
            wpmgr_waf_should_deny(self::config(), '198.51.100.7', 'protect'),
            'the recovery constant must NOT release a hardening ban in protect mode either'
        );
    }

    /**
     * The safety bypasses still outrank the hardening bans with the constant
     * set, so narrowing the gate did not reorder layers (1) and (2) under it.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_allow_list_and_private_bypass_still_outrank_hardening_bans(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);
        self::loadWaf();

        $config                = self::config();
        $config['allow_cidrs'] = ['198.51.100.0/24'];

        $this->assertFalse(
            wpmgr_waf_should_deny($config, '198.51.100.7', 'off'),
            'allow_cidrs must still win over a hardening ban'
        );
        $this->assertFalse(
            wpmgr_waf_should_deny(self::config(), '10.0.0.5', 'off'),
            'private addresses must still bypass a hardening ban'
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
