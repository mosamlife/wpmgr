<?php
/**
 * GH #529 regression guard: a user-agent ban must never 403 the administrator's
 * own way back into the site, and must still ban.
 *
 * The defect. HardeningModule::applyBanFilters() registered a closure on `init`
 * priority 1; `init` fires on wp-login.php (wp-settings.php line 771 of the
 * WordPress 7.0.4 tree, reached because wp-login.php requires wp-load.php); the
 * match was an unbounded case-insensitive substring, stripos($ua, $pattern);
 * HardeningConfig imposed no minimum length and no genericity check on the
 * pattern; and a match ended in exit('Access denied.') behind a 403. A ban
 * pattern of "Mozilla", "Chrome", "Safari", "AppleWebKit" or "Gecko" — each a
 * substring of essentially every real browser's user-agent — therefore turned
 * the site's own login page into a 403 for the administrator, with no recovery
 * path, whether the pattern was typed by an operator or pushed by the control
 * plane.
 *
 * What is guarded here, in three layers, and the fourth thing that must NOT
 * change:
 *
 *   1. Config boundary. HardeningConfig refuses a user_agent pattern that is
 *      too short, or that appears inside ordinary browser user-agents, and
 *      records the refusal instead of dropping it silently. This is the single
 *      boundary both enforcement layers read from, so the .htaccess renderer
 *      inherits the same guarantee as the PHP filter.
 *
 *   2. Recovery surfaces. The `init` callback exempts wp-login.php and the
 *      agent's /wpmgr/v1/autologin route, and the autologin match is ANCHORED
 *      so a crafted query string cannot claim the exemption.
 *
 *   3. Visibility. A refused ban rides back to the control plane on the
 *      sync_security_hardening `detail` string. Silently discarding an
 *      operator's explicit instruction is its own fail-open: they would believe
 *      a ban is live when it is not.
 *
 *   4. THE BAN STILL BANS. A genuinely hostile user-agent is still denied on
 *      ordinary traffic, and every hostile pattern an operator would realistically
 *      type still survives validation. A fix that disables the ban is worse than
 *      the bug it fixes, so the over-fire cases below are the load-bearing half
 *      of this file, not an afterthought.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\HardeningConfig;
use WPMgr\Agent\Security\HardeningModule;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\HardeningConfig::userAgentPatternRejection
 * @covers \WPMgr\Agent\Security\HardeningModule
 */
final class HardeningBanLockoutTest extends TestCase
{
    /** A stock desktop Chrome user-agent — what an administrator's browser sends. */
    private const ADMIN_UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

    /** A genuinely hostile scanner user-agent. */
    private const HOSTILE_UA = 'sqlmap/1.7.2#stable (https://sqlmap.org)';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('sanitize_text_field')->alias(fn ($v) => $v);
        Functions\when('wp_unslash')->alias(fn ($v) => $v);
        Functions\when('headers_sent')->justReturn(false);
    }

    protected function tear_down(): void
    {
        unset(
            $_SERVER['HTTP_USER_AGENT'],
            $_SERVER['REQUEST_URI'],
            $GLOBALS['pagenow']
        );
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Build a HardeningConfig carrying exactly one user_agent ban.
     */
    private function configWithUaBan(string $pattern): HardeningConfig
    {
        return HardeningConfig::fromArray([
            'config' => [],
            'bans'   => [
                ['id' => 'ban-1', 'type' => 'user_agent', 'value' => $pattern, 'comment' => ''],
            ],
        ]);
    }

    /**
     * Register applyBanFilters() for $config and return the closure it bound to
     * `init`, or null when it registered nothing.
     */
    private function captureInitCallback(HardeningConfig $config): ?\Closure
    {
        $captured = null;
        Functions\when('add_action')->alias(
            function ($hook, $cb, $priority = 10) use (&$captured) {
                if ($hook === 'init' && $priority === 1) {
                    $captured = $cb;
                }
                return true;
            }
        );

        $ref = new \ReflectionMethod(HardeningModule::class, 'applyBanFilters');
        $ref->invoke(new HardeningModule(), $config);

        return $captured instanceof \Closure ? $captured : null;
    }

    /**
     * Run the captured `init` callback and report whether it reached the 403
     * block. exit() cannot be intercepted, so the last header() call made on
     * the block path throws a marker and reaching it is the proof.
     */
    private function callbackDenies(\Closure $cb): bool
    {
        Functions\when('http_response_code')->justReturn(true);
        Functions\when('header')->alias(function (string $h) {
            if (strpos($h, 'Cache-Control') === 0) {
                throw new \RuntimeException('marker:ua_blocked');
            }
            return null;
        });

        try {
            $cb();
        } catch (\RuntimeException $e) {
            return $e->getMessage() === 'marker:ua_blocked';
        }
        return false;
    }

    // =========================================================================
    // 1. Config boundary — the generic pattern never becomes a live ban
    // =========================================================================

    /**
     * @dataProvider provideLockoutPatterns
     */
    public function test_generic_browser_pattern_is_refused_at_the_config_boundary(string $pattern): void
    {
        $config = $this->configWithUaBan($pattern);

        $this->assertSame(
            [],
            $config->userAgentBans(),
            sprintf('a ban pattern of "%s" matches ordinary browsers and must never become a live ban', $pattern)
        );
        $this->assertCount(
            1,
            $config->rejectedBans(),
            'the refusal must be recorded, not silently dropped'
        );
        $this->assertSame($pattern, $config->rejectedBans()[0]['value']);
        $this->assertNotSame('', $config->rejectedBans()[0]['reason'], 'a refusal must carry a reason');
    }

    /**
     * Every one of these is a substring of a real browser's user-agent, so each
     * would have 403'd the administrator's own login page.
     *
     * @return array<string,array{0:string}>
     */
    public static function provideLockoutPatterns(): array
    {
        return [
            'Mozilla'                => ['Mozilla'],
            'Chrome'                 => ['Chrome'],
            'Safari'                 => ['Safari'],
            'AppleWebKit'            => ['AppleWebKit'],
            'Gecko'                  => ['Gecko'],
            'KHTML'                  => ['KHTML'],
            'Mobile'                 => ['Mobile'],
            'Firefox'                => ['Firefox'],
            'Windows NT'             => ['Windows NT'],
            'Macintosh'              => ['Macintosh'],
            'lowercase mozilla'      => ['mozilla'],
            'uppercase CHROME'       => ['CHROME'],
            'Mozilla/5.0 prefix'     => ['Mozilla/5.0'],
            'too short: NT'          => ['NT'],
            'too short: rv:'         => ['rv:'],
            'too short: 64'          => ['64'],
        ];
    }

    /**
     * OVER-FIRE GUARD. The patterns an operator actually bans must still pass
     * validation untouched. If this fails the feature has been broken, not
     * fixed.
     *
     * @dataProvider provideHostilePatterns
     */
    public function test_hostile_pattern_still_survives_validation(string $pattern): void
    {
        $config = $this->configWithUaBan($pattern);

        $this->assertSame(
            [$pattern],
            $config->userAgentBans(),
            sprintf('"%s" is a legitimate ban and must still be applied', $pattern)
        );
        $this->assertSame([], $config->rejectedBans(), 'a legitimate ban must not be reported as refused');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideHostilePatterns(): array
    {
        return [
            'sqlmap'                    => ['sqlmap'],
            'nikto'                     => ['Nikto'],
            'curl'                      => ['curl'],
            'wget'                      => ['Wget'],
            'python-requests'           => ['python-requests/2.31'],
            'Go http client'            => ['Go-http-client'],
            'masscan'                   => ['masscan'],
            'zgrab'                     => ['zgrab'],
            'browser-spoofing bot'      => ['Mozilla/5.0 (compatible; EvilBot/1.0)'],
            'a crawler is a valid ban'  => ['Googlebot'],
            'AhrefsBot'                 => ['AhrefsBot'],
            'SemrushBot'                => ['SemrushBot'],
            'a specific old Chrome'     => ['Chrome/58.0.3029.110 Safari/537.36 Presto'],
        ];
    }

    public function test_a_refused_pattern_registers_no_hook_at_all(): void
    {
        Functions\expect('add_action')->never();

        $ref = new \ReflectionMethod(HardeningModule::class, 'applyBanFilters');
        $ref->invoke(new HardeningModule(), $this->configWithUaBan('Chrome'));

        $this->addToAssertionCount(1);
    }

    public function test_a_mixed_push_keeps_the_good_bans_and_refuses_only_the_bad(): void
    {
        $config = HardeningConfig::fromArray([
            'bans' => [
                ['id' => 'a', 'type' => 'user_agent', 'value' => 'sqlmap'],
                ['id' => 'b', 'type' => 'user_agent', 'value' => 'Chrome'],
                ['id' => 'c', 'type' => 'user_agent', 'value' => 'Nikto'],
                ['id' => 'd', 'type' => 'ip',         'value' => '203.0.113.9'],
            ],
        ]);

        $this->assertSame(['sqlmap', 'Nikto'], $config->userAgentBans());
        $this->assertSame(['203.0.113.9'], $config->ipRangeBans(), 'IP bans are untouched by GH #529');
        $this->assertCount(1, $config->rejectedBans());
        $this->assertSame('Chrome', $config->rejectedBans()[0]['value']);
    }

    // =========================================================================
    // 2. Recovery surfaces — the login page and the autologin route
    // =========================================================================

    public function test_login_page_is_exempt_so_the_administrator_is_never_locked_out(): void
    {
        // 'EvilBot' is an accepted pattern, so this proves the EXEMPTION rather
        // than the config-boundary refusal — the two layers are independent.
        $cb = $this->captureInitCallback($this->configWithUaBan('EvilBot'));
        $this->assertInstanceOf(\Closure::class, $cb);

        // wp-includes/vars.php derives $pagenow from the resolved script path;
        // HideBackendModule assigns the same value at setup_theme when serving
        // its secret slug. Both are before `init`.
        $GLOBALS['pagenow']         = 'wp-login.php';
        $_SERVER['REQUEST_URI']     = '/wp-login.php';
        $_SERVER['HTTP_USER_AGENT'] = 'EvilBot/1.0';

        $this->assertFalse(
            $this->callbackDenies($cb),
            'a user-agent ban must never 403 wp-login.php — that is the door the owner uses to get back in'
        );
    }

    public function test_the_administrators_own_browser_is_never_403d_on_the_login_page(): void
    {
        // The exact reported lockout, driven end to end: a generic pattern is
        // refused at the boundary, so no callback exists to 403 anybody.
        $config = $this->configWithUaBan('Chrome');
        $this->assertSame([], $config->userAgentBans());

        // And even a pattern that IS accepted cannot reach the admin's login page.
        $cb = $this->captureInitCallback($this->configWithUaBan('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) x'));
        if ($cb !== null) {
            $GLOBALS['pagenow']         = 'wp-login.php';
            $_SERVER['REQUEST_URI']     = '/wp-login.php';
            $_SERVER['HTTP_USER_AGENT'] = self::ADMIN_UA;
            $this->assertFalse($this->callbackDenies($cb));
        }
        $this->addToAssertionCount(1);
    }

    /**
     * @dataProvider provideAutologinUris
     */
    public function test_autologin_route_is_exempt(string $uri): void
    {
        $cb = $this->captureInitCallback($this->configWithUaBan('EvilBot'));
        $this->assertInstanceOf(\Closure::class, $cb);

        $GLOBALS['pagenow']         = 'index.php';
        $_SERVER['REQUEST_URI']     = $uri;
        $_SERVER['HTTP_USER_AGENT'] = 'EvilBot/1.0';

        $this->assertFalse(
            $this->callbackDenies($cb),
            sprintf('%s is the operator one-click-login route and must not be banned', $uri)
        );
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideAutologinUris(): array
    {
        return [
            'pretty permalink'       => ['/wp-json/wpmgr/v1/autologin'],
            'with a query string'    => ['/wp-json/wpmgr/v1/autologin?token=abc123'],
            'trailing slash'         => ['/wp-json/wpmgr/v1/autologin/'],
            'subdirectory install'   => ['/blog/wp-json/wpmgr/v1/autologin?token=abc123'],
            'plain permalink'        => ['/?rest_route=/wpmgr/v1/autologin'],
            'plain, trailing slash'  => ['/index.php?rest_route=/wpmgr/v1/autologin/'],
        ];
    }

    /**
     * OVER-FIRE GUARD. The autologin exemption is matched anchored, so a request
     * cannot smuggle the route into an unrelated URL and buy itself a pass.
     *
     * @dataProvider provideNonAutologinUris
     */
    public function test_autologin_exemption_is_anchored_and_cannot_be_smuggled(string $uri): void
    {
        $cb = $this->captureInitCallback($this->configWithUaBan('EvilBot'));
        $this->assertInstanceOf(\Closure::class, $cb);

        $GLOBALS['pagenow']         = 'index.php';
        $_SERVER['REQUEST_URI']     = $uri;
        $_SERVER['HTTP_USER_AGENT'] = 'EvilBot/1.0';

        $this->assertTrue(
            $this->callbackDenies($cb),
            sprintf('%s is not the autologin route and must still be banned', $uri)
        );
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideNonAutologinUris(): array
    {
        return [
            'route in a query value'   => ['/?x=/wp-json/wpmgr/v1/autologin'],
            'route as a path prefix'   => ['/wp-json/wpmgr/v1/autologin/../../wp-admin/'],
            'lookalike suffix'         => ['/wp-json/wpmgr/v1/autologin-not-really'],
            'different namespace'      => ['/wp-json/other/v1/autologin'],
            'not under wp-json'        => ['/evil/wpmgr/v1/autologin'],
            'rest_route near-miss'     => ['/?rest_route=/wpmgr/v1/autologin-x'],
        ];
    }

    // =========================================================================
    // 3. THE BAN STILL BANS — the load-bearing over-fire proofs
    // =========================================================================

    public function test_hostile_user_agent_is_still_denied_on_ordinary_front_end_traffic(): void
    {
        $config = $this->configWithUaBan('sqlmap');
        $this->assertSame(['sqlmap'], $config->userAgentBans(), 'the ban must have survived validation');

        $cb = $this->captureInitCallback($config);
        $this->assertInstanceOf(\Closure::class, $cb);

        $GLOBALS['pagenow']         = 'index.php';
        $_SERVER['REQUEST_URI']     = '/';
        $_SERVER['HTTP_USER_AGENT'] = self::HOSTILE_UA;

        $this->assertTrue(
            $this->callbackDenies($cb),
            'a genuinely hostile user-agent must still be denied — a fix that disables the ban is worse than the bug'
        );
    }

    public function test_hostile_user_agent_is_still_denied_in_wp_admin(): void
    {
        $cb = $this->captureInitCallback($this->configWithUaBan('sqlmap'));
        $this->assertInstanceOf(\Closure::class, $cb);

        $GLOBALS['pagenow']         = 'index.php';
        $_SERVER['REQUEST_URI']     = '/wp-admin/';
        $_SERVER['HTTP_USER_AGENT'] = self::HOSTILE_UA;

        $this->assertTrue(
            $this->callbackDenies($cb),
            'the exemption covers the login page and the autologin route, and nothing else'
        );
    }

    public function test_an_ordinary_visitor_is_not_denied_by_an_unrelated_ban(): void
    {
        $cb = $this->captureInitCallback($this->configWithUaBan('sqlmap'));
        $this->assertInstanceOf(\Closure::class, $cb);

        $GLOBALS['pagenow']         = 'index.php';
        $_SERVER['REQUEST_URI']     = '/';
        $_SERVER['HTTP_USER_AGENT'] = self::ADMIN_UA;

        $this->assertFalse($this->callbackDenies($cb), 'an unrelated ban must not touch a real visitor');
    }

    // =========================================================================
    // 4. Visibility — a refusal must reach whoever configured the ban
    // =========================================================================

    /**
     * Stub the option store and force the nginx branch so applyConfig() never
     * reaches ServerConfigWriter's filesystem write.
     *
     * @return \WPMgr\Agent\Commands\SyncSecurityHardeningCommand
     */
    private function commandWithStubbedStore(): \WPMgr\Agent\Commands\SyncSecurityHardeningCommand
    {
        $store = [];
        Functions\when('get_option')->alias(
            function ($k, $d = false) use (&$store) {
                return $store[$k] ?? $d;
            }
        );
        Functions\when('update_option')->alias(
            function ($k, $v) use (&$store) {
                $store[$k] = $v;
                return true;
            }
        );
        Functions\when('wp_json_encode')->alias(fn ($v) => json_encode($v));
        Functions\when('add_filter')->justReturn(true);
        Functions\when('add_action')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);

        $_SERVER['SERVER_SOFTWARE'] = 'nginx/1.24.0';

        return new \WPMgr\Agent\Commands\SyncSecurityHardeningCommand(new HardeningModule());
    }

    public function test_a_refused_ban_is_reported_back_to_the_control_plane(): void
    {
        $result = $this->commandWithStubbedStore()->execute([], [
            'bans' => [
                ['id' => 'a', 'type' => 'user_agent', 'value' => 'sqlmap'],
                ['id' => 'b', 'type' => 'user_agent', 'value' => 'Chrome'],
            ],
        ]);

        $this->assertTrue($result['ok'], 'one bad pattern must not discard the operator\'s good bans');
        $this->assertStringContainsString(
            'REFUSED',
            $result['detail'],
            'a silently dropped ban is its own fail-open — the operator would believe it is live'
        );
        $this->assertStringContainsString('Chrome', $result['detail'], 'the refusal must name the pattern');
        $this->assertStringNotContainsString(
            'sqlmap',
            $result['detail'],
            'the accepted ban must not be reported as refused'
        );
    }

    public function test_a_clean_push_says_nothing_about_refusals(): void
    {
        $result = $this->commandWithStubbedStore()->execute([], [
            'bans' => [['id' => 'a', 'type' => 'user_agent', 'value' => 'sqlmap']],
        ]);

        $this->assertTrue($result['ok']);
        $this->assertSame('applied', $result['detail'], 'a clean push must keep its existing detail exactly');
    }

    public function test_the_refusal_count_is_computed_not_asserted(): void
    {
        $bans = [];
        foreach (['Chrome', 'Safari', 'Gecko', 'Mozilla', 'KHTML', 'Firefox', 'Mobile'] as $i => $pattern) {
            $bans[] = ['id' => 'b' . $i, 'type' => 'user_agent', 'value' => $pattern];
        }

        $result = $this->commandWithStubbedStore()->execute([], ['bans' => $bans]);

        $this->assertTrue($result['ok']);
        $this->assertStringContainsString(
            count($bans) . ' ban(s) REFUSED',
            $result['detail'],
            'the count must be the real number refused'
        );
        $this->assertStringContainsString('(+2 more)', $result['detail'], 'the quoted list is capped, the count is not');
    }
}
