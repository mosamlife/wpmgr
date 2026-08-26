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
use WPMgr\Agent\Security\ServerConfigWriter;
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

    /** @var array<int,string> Temporary site roots to remove in tear_down. */
    private array $tempRoots = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('sanitize_text_field')->alias([self::class, 'coreSanitizeTextField']);
        Functions\when('wp_unslash')->alias(fn ($v) => $v);
        Functions\when('headers_sent')->justReturn(false);

        // A root install served by the front controller — the ordinary case.
        // Individual tests override these to model a subdirectory install or a
        // request some other PHP file answered.
        $this->stubSite('https://example.test', '/index.php');
    }

    protected function tear_down(): void
    {
        unset(
            $_SERVER['HTTP_USER_AGENT'],
            $_SERVER['REQUEST_URI'],
            $_SERVER['SCRIPT_NAME'],
            $_SERVER['SERVER_SOFTWARE'],
            $GLOBALS['pagenow']
        );

        foreach ($this->tempRoots as $root) {
            $htaccess = $root . '/.htaccess';
            if (is_file($htaccess)) {
                unlink($htaccess);
            }
            if (is_dir($root)) {
                rmdir($root);
            }
        }
        $this->tempRoots = [];

        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Point the site at $homeUrl and make $scriptName the script the server
     * resolved for this request.
     *
     * home_url() and rest_get_url_prefix() are stubbed for EVERY test in this
     * class, never only the ones that care. Brain Monkey cannot un-define a
     * function once defined, so a stub introduced by one test would otherwise
     * leak into the next as a function that exists but has no behaviour, and
     * isRecoverySurface() branches on function_exists().
     */
    private function stubSite(string $homeUrl, string $scriptName): void
    {
        $base = rtrim($homeUrl, '/');
        Functions\when('home_url')->alias(
            static fn (string $path = '') => $base . '/' . ltrim($path, '/')
        );
        Functions\when('rest_get_url_prefix')->justReturn('wp-json');
        $_SERVER['SCRIPT_NAME'] = $scriptName;
    }

    /**
     * WordPress core's sanitize_text_field(), reimplemented rather than stubbed
     * to identity.
     *
     * The identity stub hid a live behaviour: _sanitize_text_fields()
     * (wp-includes/formatting.php) DELETES percent-encoded octets rather than
     * decoding them, so "%3f" vanishes and "/wp-admin/options.php%3f/wp-json/…"
     * collapses into a string that matched the old suffix test. Every crafted
     * URI in provideNonAutologinUris() depends on this being faithful; with the
     * identity stub none of them proved anything.
     */
    public static function coreSanitizeTextField(string $str): string
    {
        $filtered = strip_tags($str);
        $filtered = (string) preg_replace('/[\r\n\t ]+/', ' ', $filtered);
        $filtered = trim($filtered);

        $found = false;
        while (preg_match('/%[a-f0-9]{2}/i', $filtered)) {
            $filtered = (string) preg_replace('/%[a-f0-9]{2}/i', '', $filtered);
            $found    = true;
        }
        if ($found) {
            $filtered = trim((string) preg_replace('/ +/', ' ', $filtered));
        }

        return $filtered;
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
            // Every one of these was ACCEPTED while Chrome and Firefox were
            // refused, so each reproduced the #529 lockout for one browser's
            // users. A denylist that names only the four desktop brands is not
            // a genericity test, it is a list of the browsers someone thought of.
            'Chrome on iOS'          => ['CriOS'],
            'Firefox on iOS'         => ['FxiOS'],
            'Samsung Internet'       => ['SamsungBrowser'],
            'Opera'                  => ['OPR/'],
            'Vivaldi'                => ['Vivaldi'],
            'Edge on Android'        => ['EdgA/'],
            'Edge on iOS'            => ['EdgiOS'],
            'Edge on desktop'        => ['Edg/'],
            'Samsung device token'   => ['SAMSUNG'],
            'iPhone'                 => ['iPhone'],
            'Android'                => ['Android'],
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
        // The pattern below is longer than any COMMON_BROWSER_AGENTS entry and is
        // not a substring of one, so it MUST survive validation and MUST bind a
        // callback. Branching on `$cb !== null` here would let this whole test
        // pass green the day applyBanFilters() or the validator regressed and
        // stopped binding anything — a guard that finds nothing has to go red.
        $cb = $this->captureInitCallback($this->configWithUaBan('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) x'));
        $this->assertInstanceOf(
            \Closure::class,
            $cb,
            'an accepted pattern must still bind the ban callback — no callback means the ban stopped working, not that the test passed'
        );

        $GLOBALS['pagenow']         = 'wp-login.php';
        $_SERVER['REQUEST_URI']     = '/wp-login.php';
        $_SERVER['HTTP_USER_AGENT'] = self::ADMIN_UA;
        $this->assertFalse($this->callbackDenies($cb));
    }

    /**
     * @dataProvider provideAutologinUris
     */
    public function test_autologin_route_is_exempt(string $uri, string $homeUrl, string $scriptName): void
    {
        $this->stubSite($homeUrl, $scriptName);

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
     * @return array<string,array{0:string,1:string,2:string}>
     */
    public static function provideAutologinUris(): array
    {
        return [
            'pretty permalink'      => ['/wp-json/wpmgr/v1/autologin', 'https://example.test', '/index.php'],
            'with a query string'   => ['/wp-json/wpmgr/v1/autologin?token=abc123', 'https://example.test', '/index.php'],
            // A real autologin token is base64 and arrives percent-encoded. The
            // path/query split has to happen before sanitizing or the fail-closed
            // rule in (c) would refuse every genuine one-click login.
            'percent-encoded token' => ['/wp-json/wpmgr/v1/autologin?token=YWJj%3D%3D', 'https://example.test', '/index.php'],
            'trailing slash'        => ['/wp-json/wpmgr/v1/autologin/', 'https://example.test', '/index.php'],
            'subdirectory install'  => ['/blog/wp-json/wpmgr/v1/autologin?token=abc123', 'https://example.test/blog', '/blog/index.php'],
            'plain permalink'       => ['/?rest_route=/wpmgr/v1/autologin', 'https://example.test', '/index.php'],
            'plain, trailing slash' => ['/index.php?rest_route=/wpmgr/v1/autologin/', 'https://example.test', '/index.php'],
        ];
    }

    /**
     * OVER-FIRE GUARD. The autologin exemption is matched anchored, so a request
     * cannot smuggle the route into an unrelated URL and buy itself a pass.
     *
     * The str_ends_with() form this replaced handed the exemption to every
     * "crafted" case below. The xmlrpc one is the reason it mattered: under
     * Apache mod_php and the standard nginx fastcgi_split_path_info,
     * /xmlrpc.php/wp-json/wpmgr/v1/autologin executes xmlrpc.php normally, with
     * system.multicall reachable and the user-agent ban skipped.
     *
     * @dataProvider provideNonAutologinUris
     */
    public function test_autologin_exemption_is_anchored_and_cannot_be_smuggled(
        string $uri,
        string $scriptName
    ): void {
        $this->stubSite('https://example.test', $scriptName);

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
     * @return array<string,array{0:string,1:string}>
     */
    public static function provideNonAutologinUris(): array
    {
        return [
            'route in a query value'  => ['/?x=/wp-json/wpmgr/v1/autologin', '/index.php'],
            'route as a path prefix'  => ['/wp-json/wpmgr/v1/autologin/../../wp-admin/', '/index.php'],
            'lookalike suffix'        => ['/wp-json/wpmgr/v1/autologin-not-really', '/index.php'],
            'different namespace'     => ['/wp-json/other/v1/autologin', '/index.php'],
            'not under wp-json'       => ['/evil/wpmgr/v1/autologin', '/index.php'],
            'rest_route near-miss'    => ['/?rest_route=/wpmgr/v1/autologin-x', '/index.php'],
            // Unconstrained prefix — the whole class the suffix match let through.
            'arbitrary path prefix'   => ['/anything/wp-json/wpmgr/v1/autologin', '/index.php'],
            'xmlrpc PATH_INFO'        => ['/xmlrpc.php/wp-json/wpmgr/v1/autologin', '/xmlrpc.php'],
            'comments-post PATH_INFO' => ['/wp-comments-post.php/wp-json/wpmgr/v1/autologin', '/wp-comments-post.php'],
            'admin script PATH_INFO'  => ['/wp-admin/options.php%3f/wp-json/wpmgr/v1/autologin', '/wp-admin/options.php'],
            'percent-encoded suffix'  => ['/wp-json/wpmgr/v1/autologin%00', '/index.php'],
            'protocol-relative slash' => ['//wp-json/wpmgr/v1/autologin', '/index.php'],
            // The query form smuggled onto a path that is not the site root.
            'rest_route off-root'     => ['/wp-admin/?rest_route=/wpmgr/v1/autologin', '/wp-admin/index.php'],
            'rest_route on xmlrpc'    => ['/xmlrpc.php?rest_route=/wpmgr/v1/autologin', '/xmlrpc.php'],
            // Right path, wrong install: a root-install site must not hand the
            // exemption to a subdirectory site's URL.
            'foreign subdirectory'    => ['/blog/wp-json/wpmgr/v1/autologin', '/index.php'],
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

    // =========================================================================
    // 5. The .htaccess layer — the PRIMARY enforcement point on Apache and
    //    LiteSpeed, which answers before PHP loads
    // =========================================================================

    /**
     * Render the managed block for a config carrying $uaBans.
     *
     * @param array<int,string> $uaBans
     */
    private function renderBlock(array $uaBans, string $homeUrl = 'https://example.test'): string
    {
        $this->stubSite($homeUrl, '/index.php');
        Functions\when('get_option')->justReturn('');
        unset($_SERVER['SERVER_SOFTWARE']); // Apache, not nginx.

        $bans = [];
        foreach ($uaBans as $i => $value) {
            $bans[] = ['id' => 'ua-' . $i, 'type' => 'user_agent', 'value' => $value];
        }

        return (new ServerConfigWriter())->renderInto('', HardeningConfig::fromArray(['bans' => $bans]));
    }

    /**
     * Evaluate the rendered block's RewriteConds the way Apache does, and report
     * whether the `RewriteRule ^ - [F,L]` would fire for this request.
     *
     * Asserting that a string is present in a config file proves the string is
     * present. The bug in #529 was never a missing string — it was a rule whose
     * MEANING excluded no path, so this models the semantics that decide the
     * 403: `!` negates, [NC] is case-insensitive, and [OR] binds tighter than
     * the implicit AND, so conditions form OR-groups that are then AND-combined.
     */
    private function blockDenies(string $block, string $uri, string $userAgent): bool
    {
        $parts       = explode('?', $uri, 2);
        $requestUri  = $parts[0];
        $queryString = $parts[1] ?? '';

        $vars = [
            'REQUEST_URI'     => $requestUri,
            'QUERY_STRING'    => $queryString,
            'HTTP_USER_AGENT' => $userAgent,
        ];

        // Collect the conditions guarding the ban rule.
        $conds = [];
        foreach (explode("\n", $block) as $line) {
            $line = trim($line);
            if (str_starts_with($line, 'RewriteCond ')) {
                $conds[] = $line;
                continue;
            }
            if ($line === 'RewriteRule ^ - [F,L]') {
                break;
            }
        }
        $this->assertNotSame([], $conds, 'the ban rule must be guarded by conditions');

        // Group by [OR] (OR binds tighter than AND), then AND the groups.
        $groups  = [];
        $current = [];
        foreach ($conds as $cond) {
            $ok = preg_match(
                '/^RewriteCond %\{([A-Z_]+)\} (!?)(\S+?)(?: \[([A-Z,]+)\])?$/',
                $cond,
                $m
            );
            $this->assertSame(1, $ok, sprintf('unparseable RewriteCond: %s', $cond));

            $value    = $vars[$m[1]] ?? '';
            $negate   = $m[2] === '!';
            $flags    = explode(',', $m[4] ?? '');
            $delim    = in_array('NC', $flags, true) ? '#i' : '#';
            $matched  = preg_match('#' . $m[3] . $delim, $value) === 1;
            $current[] = $negate ? !$matched : $matched;

            if (!in_array('OR', $flags, true)) {
                $groups[] = in_array(true, $current, true);
                $current  = [];
            }
        }

        foreach ($groups as $group) {
            if ($group === false) {
                return false;
            }
        }
        return true;
    }

    /**
     * THE #529 BUG AT THE LAYER THAT ACTUALLY SERVES IT. `RewriteRule ^ - [F,L]`
     * matches every path, and Apache answers it before wp-settings.php runs, so
     * HardeningModule::isRecoverySurface() is unreachable for exactly the
     * request it exists to allow. A recovery exemption that lives only in PHP is
     * not a recovery exemption on Apache or LiteSpeed.
     *
     * @dataProvider provideServerBlockRecoveryUris
     */
    public function test_rendered_server_block_exempts_the_recovery_surfaces(string $uri): void
    {
        $block = $this->renderBlock(['EvilBot']);

        $this->assertFalse(
            $this->blockDenies($block, $uri, 'EvilBot/1.0'),
            sprintf('%s is a recovery surface and the .htaccess block must not 403 it', $uri)
        );
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideServerBlockRecoveryUris(): array
    {
        return [
            'login page'              => ['/wp-login.php'],
            'login with a query'      => ['/wp-login.php?action=lostpassword'],
            'login with PATH_INFO'    => ['/wp-login.php/'],
            'autologin, pretty'       => ['/wp-json/wpmgr/v1/autologin'],
            'autologin with a token'  => ['/wp-json/wpmgr/v1/autologin?token=abc123'],
            'autologin, trailing /'   => ['/wp-json/wpmgr/v1/autologin/'],
            'autologin, plain'        => ['/?rest_route=/wpmgr/v1/autologin'],
            'autologin, plain index'  => ['/index.php?rest_route=/wpmgr/v1/autologin'],
        ];
    }

    /**
     * OVER-FIRE GUARD, and the load-bearing half of B1. The exemption must not
     * turn the ban off: ordinary traffic from a banned agent is still 403'd at
     * Apache, which is the entire point of writing the rule.
     *
     * @dataProvider provideServerBlockBannedUris
     */
    public function test_rendered_server_block_still_bans_ordinary_traffic(string $uri): void
    {
        $block = $this->renderBlock(['EvilBot']);

        $this->assertTrue(
            $this->blockDenies($block, $uri, 'Mozilla/5.0 (compatible; EvilBot/1.0)'),
            sprintf('%s is ordinary traffic from a banned agent and must still be 403d', $uri)
        );
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideServerBlockBannedUris(): array
    {
        return [
            'front page'             => ['/'],
            'a post'                 => ['/2026/08/hello-world/'],
            'wp-admin'               => ['/wp-admin/'],
            'xmlrpc'                 => ['/xmlrpc.php'],
            'another REST route'     => ['/wp-json/wp/v2/posts'],
            'autologin lookalike'    => ['/wp-json/wpmgr/v1/autologin-not-really'],
            'smuggled autologin'     => ['/xmlrpc.php/wp-json/wpmgr/v1/autologin'],
            'login lookalike'        => ['/wp-login.php.bak'],
            'rest_route off-root'    => ['/wp-admin/?rest_route=/wpmgr/v1/autologin'],
        ];
    }

    public function test_rendered_server_block_leaves_an_unbanned_visitor_alone(): void
    {
        $block = $this->renderBlock(['EvilBot']);

        $this->assertFalse(
            $this->blockDenies($block, '/', self::ADMIN_UA),
            'a real visitor must not be 403d by an unrelated ban'
        );
    }

    /**
     * %{REQUEST_URI} carries the full path in per-directory context, so an
     * exemption anchored at ^ silently stops matching on a subdirectory install
     * unless the home path is prepended.
     */
    public function test_rendered_server_block_anchors_the_exemption_to_the_site_home_path(): void
    {
        $block = $this->renderBlock(['EvilBot'], 'https://example.test/blog');

        $this->assertFalse(
            $this->blockDenies($block, '/blog/wp-login.php', 'EvilBot/1.0'),
            'the subdirectory install\'s own login page must be exempt'
        );
        $this->assertFalse(
            $this->blockDenies($block, '/blog/wp-json/wpmgr/v1/autologin', 'EvilBot/1.0'),
            'the subdirectory install\'s own autologin route must be exempt'
        );
        $this->assertTrue(
            $this->blockDenies($block, '/wp-login.php', 'EvilBot/1.0'),
            'a path outside this install is not this install\'s login page'
        );
    }

    public function test_a_block_with_no_user_agent_ban_carries_no_exemption(): void
    {
        $this->stubSite('https://example.test', '/index.php');
        Functions\when('get_option')->justReturn('');
        unset($_SERVER['SERVER_SOFTWARE']);

        $block = (new ServerConfigWriter())->renderInto('', HardeningConfig::fromArray([
            'config' => ['disable_directory_browsing' => true],
        ]));

        $this->assertStringNotContainsString(
            'wp-login',
            $block,
            'the exemption belongs to the user-agent ban and must not appear without one'
        );
    }

    // =========================================================================
    // 6. The upgrade path — a stale block is the reason the fix did not land
    // =========================================================================

    /**
     * A pre-fix block: a bare generic ban, no exemption. This is what is sitting
     * in .htaccess on every site that reported #529.
     */
    private const STALE_BLOCK = "# BEGIN WPMgr Security\n"
        . "<IfModule mod_rewrite.c>\n"
        . "    RewriteEngine On\n"
        . "    RewriteCond %{HTTP_USER_AGENT} Chrome [NC]\n"
        . "    RewriteRule ^ - [F,L]\n"
        . "</IfModule>\n"
        . "# END WPMgr Security\n";

    /**
     * Point the writer at a real temporary site root and stub the option store.
     *
     * @param array<string,mixed> $options
     * @return array{0:string,1:string} [site root, .htaccess path]
     */
    private function stubSiteRoot(array &$options): array
    {
        // Must exist BEFORE the refresh runs: refreshServerConfigIfStale() has
        // nothing to compare against without it and returns early.
        self::agentVersion();

        $root = sys_get_temp_dir() . '/wpmgr-529-' . bin2hex(random_bytes(8));
        mkdir($root, 0777, true);
        $this->tempRoots[] = $root;

        Functions\when('get_home_path')->justReturn($root . '/');
        Functions\when('get_option')->alias(
            function ($key, $default = false) use (&$options) {
                return $options[$key] ?? $default;
            }
        );
        Functions\when('update_option')->alias(
            function ($key, $value) use (&$options) {
                $options[$key] = $value;
                return true;
            }
        );
        unset($_SERVER['SERVER_SOFTWARE']); // Apache.

        return [$root, $root . '/.htaccess'];
    }

    /**
     * THE HALF THAT REACHES THE PEOPLE WHO REPORTED IT. Every path that wrote
     * .htaccess ran from the sync command, so a site already 403ing on a
     * persisted `Chrome` rule stayed locked out after upgrading: the validator
     * drops the pattern from the option and the PHP filter exempts the login
     * page, but Apache reads the FILE, and nothing rewrote it.
     *
     * The re-render deliberately does not need the control plane to reach the
     * site — on a locked-out site an inbound sync is exactly what the 403 is
     * eating.
     */
    public function test_an_upgrade_rerenders_a_block_left_behind_by_an_older_version(): void
    {
        $options = [HardeningModule::OPTION_SERVER_REV => '0.60.0-stale'];
        [, $htaccess] = $this->stubSiteRoot($options);
        file_put_contents($htaccess, self::STALE_BLOCK);

        $config = HardeningConfig::fromArray([
            'bans' => [['id' => 'a', 'type' => 'user_agent', 'value' => 'Chrome']],
        ]);
        $this->assertSame([], $config->userAgentBans(), 'the generic pattern is refused at the boundary');

        $ref = new \ReflectionMethod(HardeningModule::class, 'refreshServerConfigIfStale');
        $ref->invoke(new HardeningModule(), $config);

        $after = (string) file_get_contents($htaccess);

        $this->assertStringNotContainsString(
            'RewriteCond %{HTTP_USER_AGENT} Chrome',
            $after,
            'the rule that 403d the administrator must be gone from the file Apache reads'
        );
        $this->assertSame(
            self::agentVersion(),
            $options[HardeningModule::OPTION_SERVER_REV] ?? null,
            'the block on disk must be stamped with the version that wrote it'
        );
    }

    /**
     * A site whose ban survives validation keeps the ban AND gains the
     * exemption — the upgrade repairs the block rather than emptying it.
     */
    public function test_an_upgrade_adds_the_exemption_without_dropping_a_valid_ban(): void
    {
        $options = [HardeningModule::OPTION_SERVER_REV => '0.60.0-stale'];
        [, $htaccess] = $this->stubSiteRoot($options);
        file_put_contents($htaccess, self::STALE_BLOCK);

        $ref = new \ReflectionMethod(HardeningModule::class, 'refreshServerConfigIfStale');
        $ref->invoke(new HardeningModule(), $this->configWithUaBan('EvilBot'));

        $after = (string) file_get_contents($htaccess);

        $this->assertTrue(
            $this->blockDenies($after, '/', 'EvilBot/1.0'),
            'the operator\'s valid ban must survive the re-render'
        );
        $this->assertFalse(
            $this->blockDenies($after, '/wp-login.php', 'EvilBot/1.0'),
            'and the login page must now be exempt in the file Apache reads'
        );
    }

    /**
     * OVER-FIRE GUARD. This runs on plugins_loaded, on every request. A site
     * already on the current version must not rewrite .htaccess on every hit.
     */
    public function test_a_current_stamp_does_not_touch_the_file(): void
    {
        $options = [HardeningModule::OPTION_SERVER_REV => self::agentVersion()];
        [, $htaccess] = $this->stubSiteRoot($options);
        file_put_contents($htaccess, self::STALE_BLOCK);

        $ref = new \ReflectionMethod(HardeningModule::class, 'refreshServerConfigIfStale');
        $ref->invoke(new HardeningModule(), $this->configWithUaBan('EvilBot'));

        $this->assertSame(
            self::STALE_BLOCK,
            (string) file_get_contents($htaccess),
            'an up-to-date stamp must short-circuit before any write'
        );
    }

    /**
     * THE WIRING, not the mechanism. Every other test in this section reaches
     * refreshServerConfigIfStale() by reflection, and every one of them would
     * still pass if install() never called it — which is the exact shape of the
     * bug: applyConfig() refreshed the block, install() did not, and no test
     * looked at install() because its `static $installed` latch makes it awkward
     * to call twice in one process. That awkwardness is why the gap survived, so
     * this test pays for a separate process rather than inherit it.
     *
     * SERVER_SOFTWARE is nginx so writeServerConfig() returns before any disk
     * write; the stamp is then the only observable effect, and it can only be
     * written by the call under test.
     *
     * `@runInSeparateProcess` — install()'s latch is process-wide.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_install_reaches_the_server_config_refresh(): void
    {
        self::agentVersion();

        $options = [HardeningModule::OPTION_SERVER_REV => '0.60.0-stale'];
        Functions\when('get_option')->alias(
            function ($key, $default = false) use (&$options) {
                return $options[$key] ?? $default;
            }
        );
        Functions\when('update_option')->alias(
            function ($key, $value) use (&$options) {
                $options[$key] = $value;
                return true;
            }
        );
        Functions\when('add_filter')->justReturn(true);
        Functions\when('add_action')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        $_SERVER['SERVER_SOFTWARE'] = 'nginx/1.24.0';

        (new HardeningModule())->install();

        $this->assertSame(
            self::agentVersion(),
            $options[HardeningModule::OPTION_SERVER_REV] ?? null,
            'install() must reach refreshServerConfigIfStale() — a stale block is repaired on boot, not on the next sync'
        );
    }

    /**
     * THE STAMP MUST FOLLOW THE WRITE. Stamping a failed write marks the repair
     * done and permanently strands the site on the stale generic rule: the only
     * things that would try again are the next version bump and the next sync,
     * and the sync is the inbound request the stale 403 is eating.
     *
     * A read-only site root is the transient case that matters — a full disk, a
     * deploy that flips the tree read-only for a minute, a lock held elsewhere.
     */
    public function test_a_failed_rewrite_is_not_stamped_as_current(): void
    {
        $options = [HardeningModule::OPTION_SERVER_REV => '0.60.0-stale'];
        [$root, $htaccess] = $this->stubSiteRoot($options);
        file_put_contents($htaccess, self::STALE_BLOCK);

        // Make the write fail the way a real read-only tree does.
        chmod($htaccess, 0444);
        chmod($root, 0555);

        $ref = new \ReflectionMethod(HardeningModule::class, 'refreshServerConfigIfStale');
        $ref->invoke(new HardeningModule(), $this->configWithUaBan('EvilBot'));

        chmod($root, 0755);
        chmod($htaccess, 0644);

        $this->assertSame(
            self::STALE_BLOCK,
            (string) file_get_contents($htaccess),
            'the write really did fail — otherwise this test proves nothing about the stamp'
        );
        $this->assertSame(
            '0.60.0-stale',
            $options[HardeningModule::OPTION_SERVER_REV] ?? null,
            'a failed rewrite must leave the stamp stale so the next boot retries the repair'
        );
    }

    /**
     * With hide-backend on, wp-login.php is NOT the door — HideBackendModule
     * intercepts the secret slug at setup_theme and serves the login form
     * there. Apache reaches the slug before PHP does, so a server block that
     * exempts only wp-login.php 403s the one door that works, on exactly the
     * sites that hardened it hardest.
     */
    public function test_rendered_server_block_exempts_the_hidden_login_slug(): void
    {
        $this->stubSite('https://example.test', '/index.php');
        Functions\when('get_option')->alias(
            static function ($key, $default = false) {
                if ($key === 'wpmgr_security_policy') {
                    return (string) json_encode([
                        'policy' => [
                            'hide_backend_enabled' => true,
                            'hide_backend_slug'    => 'my-secret-door',
                        ],
                    ]);
                }
                return $default;
            }
        );
        unset($_SERVER['SERVER_SOFTWARE']);

        $block = (new ServerConfigWriter())->renderInto('', HardeningConfig::fromArray([
            'bans' => [['id' => 'ua-0', 'type' => 'user_agent', 'value' => 'EvilBot']],
        ]));

        $this->assertFalse(
            $this->blockDenies($block, '/my-secret-door', 'EvilBot/1.0'),
            'the hidden login slug is the recovery surface when hide-backend is on'
        );
        $this->assertTrue(
            $this->blockDenies($block, '/', 'EvilBot/1.0'),
            'and the ban must still fire on ordinary traffic'
        );
        $this->assertTrue(
            $this->blockDenies($block, '/not-the-door', 'EvilBot/1.0'),
            'only the configured slug is exempt'
        );
    }

    /**
     * OVER-FIRE GUARD. With hide-backend off there is no slug to exempt, and an
     * empty or unset slug must never widen into a bare `!^/($|/)`.
     */
    public function test_no_slug_exemption_when_hide_backend_is_off(): void
    {
        $block = $this->renderBlock(['EvilBot']);

        $this->assertSame(
            4,
            substr_count($block, 'RewriteCond %{REQUEST_URI} !') + substr_count($block, 'RewriteCond %{QUERY_STRING} !'),
            'exactly the four standing exemptions, no slug condition'
        );
        $this->assertTrue(
            $this->blockDenies($block, '/anything', 'EvilBot/1.0'),
            'an absent slug must not exempt an arbitrary first segment'
        );
    }

    /**
     * The agent version this test process is running as. Read, never asserted:
     * another test in the same process may have defined the constant first.
     */
    private static function agentVersion(): string
    {
        if (!defined('WPMGR_AGENT_VERSION')) {
            define('WPMGR_AGENT_VERSION', '0.61.145-test');
        }
        return (string) constant('WPMGR_AGENT_VERSION');
    }
}
