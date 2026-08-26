<?php
/**
 * GH #529, the third enforcement point: LoginProtection's event-backed lockout
 * must honour the operator's documented recovery constant — and must not
 * honour it anywhere else.
 *
 * `define('WPMGR_DISABLE_SITE_2FA', true)` in wp-config.php is this plugin's
 * last-resort escape hatch for an administrator it has locked out.
 * Site2faModule, PasswordPolicyModule, HardeningModule's auth appliers and the
 * WAF mu-plugin gate all honoured it. LoginProtection::onAuthenticate() did
 * not — so the single most likely lockout of all stayed shut. An admin
 * mistypes a password past temp_block_limit, gets the 403, edits wp-config.php
 * over SFTP, reloads, and is blocked again by the same escalating tiers, now
 * believing the documented remedy has been tried and failed.
 *
 * WHAT THE CONSTANT RELEASES HERE, AND WHAT IT MUST NOT
 * ----------------------------------------------------
 * Released — every deny this class can issue, all of them brute-force-owned:
 *   - step 4, deny_cidrs. Auto-populated from failed attempts and owned solely
 *     by the login-protection subsystem, despite the bare name.
 *   - steps 7, 8 and 9 — ALL_BLOCKED, TEMP_BLOCK, CAPTCHA_BLOCK — counted live
 *     out of {prefix}wpmgr_login_events, rows this plugin wrote itself.
 * All four are automatic, transient, self-inflicted, and aimed at whoever is
 * currently typing.
 *
 * NOT released: hardening_deny_cidrs, the operator's explicit IP ban list. This
 * class never reads that key at all — test_login_protection_never_consults_
 * hardening_deny_cidrs below pins that, so the blanket release above provably
 * cannot reach it. It is enforced one layer earlier by the WAF mu-plugin, whose
 * own recovery check sits BELOW the hardening layer;
 * WafRecoveryConstantTest::test_recovery_constant_never_releases_an_explicit_
 * hardening_ban is the proof that it still blocks with the constant set.
 *
 * WHY deny_cidrs RELEASES HERE, having first been scoped not to. With the gate
 * under step 4 the WAF released deny_cidrs before WordPress booted and this
 * method blocked the same list in PHP: the hatch opened the outer door and
 * locked the inner one. The names are the trap — the plainer key is the
 * automatic list and the qualified key is the deliberate one.
 *
 * WHY THESE RUN IN SEPARATE PROCESSES. A constant cannot be undefined. Defining
 * WPMGR_DISABLE_SITE_2FA in the shared test process would release the tiers for
 * every later test in the run, and PHPUnit's file order would decide whether
 * the suite passed. `@runTestsInSeparateProcesses` at CLASS level (the per-test
 * spelling is `@runInSeparateProcess` and is silently ignored on a class)
 * contains each define to one child process, which is also what lets the
 * over-fire tests below assert on a genuinely undefined constant.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\LoginProtection;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\LoginProtection::onAuthenticate
 *
 * @runTestsInSeparateProcesses
 * @preserveGlobalState disabled
 */
final class LoginProtectionRecoveryConstantTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb'], $_SERVER['REMOTE_ADDR']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Self-contained harness (deliberately not shared with LoginProtectionTest:
    // these run in child processes and must not depend on anything the parent
    // process set up).
    // -------------------------------------------------------------------------

    /**
     * Seed get_option() so loadConfig() returns a validated config.
     *
     * @param string            $mode
     * @param array<string,int> $thresholds
     * @param list<string>      $denyCidrs
     */
    private function configure(
        string $mode,
        array $thresholds = [],
        array $denyCidrs = [],
        array $hardeningDenyCidrs = []
    ): void {
        $config = [
            'mode'                 => $mode,
            'thresholds'           => $thresholds,
            'ip_header'            => 'REMOTE_ADDR',
            'allow_cidrs'          => [],
            'deny_cidrs'           => $denyCidrs,
            // Present in the real wp_options row. This class must ignore it.
            'hardening_deny_cidrs' => $hardeningDenyCidrs,
        ];
        Functions\when('get_option')->justReturn((string) json_encode($config));
    }

    /**
     * wpdb double returning scripted sliding-window counts, keyed on
     * (status, per-IP vs global) rather than the time()-derived SQL text.
     */
    private function scriptedWpdb(int $successPerIp = 0, int $failureGlobal = 0, int $failurePerIp = 0): object
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
                    return '0'; // enforceRowCap()'s bare COUNT(*) probe.
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

    /** Standard escalating tiers, stated explicitly so a default change cannot move the goalposts. */
    private const TIERS = ['captcha_limit' => 3, 'temp_block_limit' => 10, 'block_all_limit' => 100];

    // =========================================================================
    // GREEN: the constant releases each of the three event-backed tiers.
    // =========================================================================

    /**
     * The headline case. Admin mistypes past temp_block_limit, sets the
     * constant, and gets in.
     */
    public function test_recovery_constant_releases_the_per_ip_temp_block_tier(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        $user   = new \stdClass();
        $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');

        $this->assertSame(
            $user,
            $result,
            'with the recovery constant set, the temp-block tier must pass the login through unchanged'
        );
    }

    /**
     * The soft tier releases too. An admin three failures in is the commonest
     * version of this lockout, not the rarest.
     */
    public function test_recovery_constant_releases_the_per_ip_captcha_tier(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 3);

        $user   = new \stdClass();
        $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');

        $this->assertSame($user, $result, 'the captcha tier must release under the recovery constant');
    }

    /**
     * The site-wide tier releases too. It is the tier most likely to be
     * standing when an admin finally reaches for wp-config.php, because it
     * fires on everyone else's failures as well as their own.
     */
    public function test_recovery_constant_releases_the_global_all_blocked_tier(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 100, failurePerIp: 1);

        $user   = new \stdClass();
        $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');

        $this->assertSame($user, $result, 'the all-blocked tier must release under the recovery constant');
    }

    /**
     * PROTECT mode is the mode that actually terminates the request with a
     * wp_die() 403; AUDIT only returns a WP_Error. The release must reach the
     * mode that can strand an administrator, not just the mode that logs.
     */
    public function test_recovery_constant_releases_the_lockout_in_protect_mode(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $this->configure(LoginProtection::MODE_PROTECT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        // Both stubbed so that a REGRESSION here fails as a readable assertion
        // rather than as "undefined function wp_kses" from inside terminate().
        Functions\when('wp_die')->alias(static function ($message, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called');
        });
        Functions\when('wp_kses')->alias(static fn($html, $allowed) => $html);

        $user   = new \stdClass();
        $died   = false;
        $result = null;
        try {
            $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');
        } catch (\RuntimeException $e) {
            $died = $e->getMessage() === 'wp_die called';
        }

        $this->assertFalse(
            $died,
            'PROTECT mode must not wp_die() an administrator who has set the recovery constant'
        );
        $this->assertSame($user, $result, 'the login must pass through unchanged instead');
    }

    /**
     * THE INNER DOOR. The WAF mu-plugin already releases deny_cidrs before
     * WordPress boots. With this method still blocking the same list, the
     * recovery constant opened the outer door and locked the inner one, and the
     * admin it was written for met "Your IP address is blocked from logging in"
     * anyway — an escape hatch that does not release what it claims to.
     *
     * Zero failures on record here, so deny_cidrs is the ONLY thing that can
     * block; if this passes for any other reason the scripted counts are wrong.
     */
    public function test_recovery_constant_releases_a_deny_cidr_hit(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS, ['203.0.113.9/32']);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 0, failurePerIp: 0);

        $user   = new \stdClass();
        $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');

        $this->assertNotInstanceOf(
            \WP_Error::class,
            $result,
            'with the recovery constant set, a deny_cidrs hit must not return wpmgr_ip_blocked'
        );
        $this->assertSame($user, $result, 'the login must pass through unchanged');
    }

    /**
     * Same release in PROTECT mode, where the block is a wp_die() rather than a
     * filter return. PROTECT is the mode that actually strands an
     * administrator.
     */
    public function test_recovery_constant_releases_a_deny_cidr_hit_in_protect_mode(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $this->configure(LoginProtection::MODE_PROTECT, self::TIERS, ['203.0.113.9/32']);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 0, failurePerIp: 0);

        Functions\when('wp_die')->alias(static function ($message, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called');
        });
        Functions\when('wp_kses')->alias(static fn($html, $allowed) => $html);

        $user   = new \stdClass();
        $died   = false;
        $result = null;
        try {
            $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');
        } catch (\RuntimeException $e) {
            $died = $e->getMessage() === 'wp_die called';
        }

        $this->assertFalse($died, 'PROTECT mode must not wp_die() a deny_cidrs hit under the recovery constant');
        $this->assertSame($user, $result, 'the login must pass through unchanged');
    }

    // =========================================================================
    // THE BOUNDARY: hardening_deny_cidrs, the operator's explicit ban list, is
    // never released — and cannot be, because this class never reads it.
    // =========================================================================

    /**
     * THE BLAST-RADIUS PIN. The release above is deliberately broad: it sits
     * above every deny this class can issue. That is only safe while the ONLY
     * denies this class can issue are brute-force-owned.
     *
     * So this asserts the blindness directly, with the constant ABSENT and
     * enforcement fully live: an IP that matches hardening_deny_cidrs and
     * nothing else must sail straight through, because LoginProtection does not
     * consult that key. hardening_deny_cidrs is enforced by the WAF mu-plugin,
     * and WafRecoveryConstantTest::test_recovery_constant_never_releases_an_
     * explicit_hardening_ban proves it still blocks there with the constant set.
     *
     * If someone ever wires hardening_deny_cidrs into this class, this test goes
     * red — and that is the signal that the step 3b gate must move BELOW the new
     * check before the feature ships, or the recovery constant silently becomes
     * a kill switch for the operator's IP ban list.
     */
    public function test_login_protection_never_consults_hardening_deny_cidrs(): void
    {
        $this->assertConstantAbsent();

        $this->configure(
            LoginProtection::MODE_PROTECT,
            self::TIERS,
            denyCidrs: [],
            hardeningDenyCidrs: ['203.0.113.9/32']
        );
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 0, failurePerIp: 0);

        Functions\when('wp_die')->alias(static function ($message, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called');
        });
        Functions\when('wp_kses')->alias(static fn($html, $allowed) => $html);

        $user   = new \stdClass();
        $result = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'correct-horse');

        $this->assertSame(
            $user,
            $result,
            'LoginProtection must not enforce hardening_deny_cidrs; if it now does, the step 3b '
            . 'recovery gate must move below that check before this test is updated'
        );
    }

    // =========================================================================
    // OVER-FIRE: with the constant UNDEFINED, brute-force protection must fire
    // exactly as before — same thresholds, same tiers, same categories.
    //
    // Every test in this class runs in its own process, so the constant is
    // genuinely absent here; the guard below fails loudly rather than skipping
    // if that ever stops being true, because a silently-skipped over-fire proof
    // is how a weakened rate limiter ships.
    // =========================================================================

    private function assertConstantAbsent(): void
    {
        $this->assertFalse(
            defined('WPMGR_DISABLE_SITE_2FA'),
            'over-fire proof is void unless WPMGR_DISABLE_SITE_2FA is undefined in this process'
        );
    }

    /**
     * deny_cidrs is the half the release was WIDENED to cover, so it is the half
     * most at risk of having been widened into a permanent hole. With the
     * constant absent it must block exactly as it always did.
     */
    public function test_without_the_constant_a_deny_cidr_still_blocks(): void
    {
        $this->assertConstantAbsent();

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS, ['203.0.113.9/32']);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 0, failurePerIp: 0);

        $result = (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'deny_cidrs must still block without the constant');
        $this->assertSame('wpmgr_ip_blocked', $result->get_error_code());
    }

    /**
     * And it still terminates the request in PROTECT mode.
     */
    public function test_without_the_constant_a_deny_cidr_still_terminates_in_protect_mode(): void
    {
        $this->assertConstantAbsent();

        $this->configure(LoginProtection::MODE_PROTECT, self::TIERS, ['203.0.113.9/32']);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 0, failurePerIp: 0);

        $died = false;
        Functions\when('wp_die')->alias(static function ($message, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called');
        });
        Functions\when('wp_kses')->alias(static fn($html, $allowed) => $html);

        try {
            (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');
        } catch (\RuntimeException $e) {
            $died = $e->getMessage() === 'wp_die called';
        }

        $this->assertTrue($died, 'a deny_cidrs hit must still terminate in PROTECT mode without the constant');
    }

    public function test_without_the_constant_the_temp_block_tier_still_fires(): void
    {
        $this->assertConstantAbsent();

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        $result = (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'temp-block tier must still fire without the constant');
        $this->assertSame('wpmgr_temp_blocked', $result->get_error_code());
    }

    public function test_without_the_constant_the_captcha_tier_still_fires(): void
    {
        $this->assertConstantAbsent();

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 3);

        $result = (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'captcha tier must still fire without the constant');
        $this->assertSame('wpmgr_captcha_block', $result->get_error_code());
    }

    public function test_without_the_constant_the_all_blocked_tier_still_fires(): void
    {
        $this->assertConstantAbsent();

        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 100, failurePerIp: 1);

        $result = (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');

        $this->assertInstanceOf(\WP_Error::class, $result, 'all-blocked tier must still fire without the constant');
        $this->assertSame('wpmgr_all_blocked', $result->get_error_code());
    }

    /**
     * THE THRESHOLD ITSELF MUST NOT MOVE. One failure below temp_block_limit is
     * still the captcha tier and not the hard block; one below captcha_limit is
     * still a clean pass. A fix that quietly shifted a boundary by one would be
     * invisible to the three tests above, which all sit exactly on a limit.
     */
    public function test_without_the_constant_the_tier_boundaries_are_unchanged(): void
    {
        $this->assertConstantAbsent();

        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';

        // 9 failures: one below temp_block_limit -> captcha tier, not temp block.
        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $GLOBALS['wpdb'] = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 9);
        $result          = (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');
        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame('wpmgr_captcha_block', $result->get_error_code(), '9 failures must still be the captcha tier');

        // 2 failures: one below captcha_limit -> no block at all.
        $this->configure(LoginProtection::MODE_AUDIT, self::TIERS);
        $GLOBALS['wpdb'] = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 2);
        $user            = new \stdClass();
        $result          = (new LoginProtection(null))->onAuthenticate($user, 'admin', 'wrongpass');
        $this->assertSame($user, $result, '2 failures must still pass through untouched');
    }

    /**
     * And PROTECT mode still terminates the request without the constant. This
     * is the assertion that would catch a release accidentally left
     * unconditional.
     */
    public function test_without_the_constant_protect_mode_still_terminates(): void
    {
        $this->assertConstantAbsent();

        $this->configure(LoginProtection::MODE_PROTECT, self::TIERS);
        $_SERVER['REMOTE_ADDR'] = '203.0.113.9';
        $GLOBALS['wpdb']        = $this->scriptedWpdb(successPerIp: 0, failureGlobal: 5, failurePerIp: 10);

        $died = false;
        Functions\when('wp_die')->alias(static function ($message, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called');
        });
        Functions\when('wp_kses')->alias(static fn($html, $allowed) => $html);

        try {
            (new LoginProtection(null))->onAuthenticate(new \stdClass(), 'admin', 'wrongpass');
        } catch (\RuntimeException $e) {
            $died = $e->getMessage() === 'wp_die called';
        }

        $this->assertTrue($died, 'PROTECT mode must still wp_die() a brute-force block when the constant is absent');
    }
}
