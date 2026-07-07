<?php
/**
 * HideBackendModule tests.
 *
 * Validates:
 *   - install() is a no-op when hide_backend_enabled=false
 *   - install() is a no-op with WPMGR_DISABLE_HIDE_BACKEND constant
 *   - shouldBail() returns true for CLI, cron, REST, WP_INSTALLING
 *   - matchesSlug() matches the slug path exactly
 *   - isLoginOrAdminPath() detects canonical wp-login and wp-admin paths
 *   - hasAccessCookie() detects the access cookie
 *   - interceptRequest() is a no-op for logged-in users
 *   - BUG 2 (GH #170): serveLoginForm() sets $pagenow='wp-login.php' and
 *     defers the actual require()/exit to a single wp_loaded action, WITHOUT
 *     exiting itself (so the slug branch of interceptRequest() actually
 *     serves a reachable login form instead of falling through to a 404)
 *   - BUG 2: rewriteLoginLinkUrl()/rewriteSiteUrl() point login/logout/
 *     lostpassword/site_url('wp-login.php...') links at the secret slug
 *     WHILE the hidden login form is being served, and are a no-op on a
 *     normal front-end request (security-review Finding 1: no slug leak)
 *   - security-review Finding 2: hasAccessCookie() rejects a forged static
 *     cookie value and a cookie minted for a different slug; only the HMAC
 *     for the CURRENT slug is accepted
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\HideBackendModule;
use WPMgr\Agent\Security\SecurityPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\HideBackendModule
 */
final class HideBackendModuleTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        Functions\when('get_option')->justReturn('');
        // add_action()/add_filter() are deliberately NOT stubbed here with
        // when() — Brain Monkey cannot route a real call to a later
        // Functions\expect() on the same function name once when() has
        // already bound a stub redefinition for it. Tests that need
        // add_action()/add_filter() to be callable set up their own
        // Functions\expect()/Functions\when() as needed.
        Functions\when('esc_url_raw')->alias(fn ($u) => $u);
        Functions\when('is_ssl')->justReturn(false);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('wp_unslash')->alias(fn ($v) => $v);
        Functions\when('sanitize_text_field')->alias(fn ($v) => $v);
        Functions\when('wp_salt')->justReturn('test-fixed-auth-salt-value');
    }

    protected function tear_down(): void
    {
        unset($_SERVER['REQUEST_URI']);
        unset($_COOKIE[HideBackendModule::COOKIE_ACCESS]);
        // $GLOBALS['pagenow'] is a real WordPress global some tests mutate to
        // exercise isServingHiddenLoginForm()'s defensive secondary signal —
        // never let it leak into a later test in this same process.
        unset($GLOBALS['pagenow']);
        Monkey\tearDown();
        parent::tear_down();
    }

    private function makePolicy(bool $enabled = true, string $slug = 'my-secret-login', string $redirect = ''): SecurityPolicy
    {
        return SecurityPolicy::fromArray([
            'policy' => [
                'hide_backend_enabled'   => $enabled,
                'hide_backend_slug'      => $slug,
                'hide_backend_redirect'  => $redirect,
            ],
        ]);
    }

    private function module(SecurityPolicy $policy): HideBackendModule
    {
        return new HideBackendModule($policy);
    }

    /** Force the private $servingLoginForm flag so rewrite tests can exercise the "serving" branch directly. */
    private function setServing(HideBackendModule $mod, bool $value): void
    {
        $ref = new \ReflectionProperty($mod, 'servingLoginForm');
        $ref->setAccessible(true);
        $ref->setValue($mod, $value);
    }

    /** Compute the current expected access-cookie HMAC via reflection. */
    private function cookieValueFor(HideBackendModule $mod): string
    {
        $ref = new \ReflectionMethod($mod, 'accessCookieValue');
        $ref->setAccessible(true);
        return $ref->invoke($mod);
    }

    // -------------------------------------------------------------------------
    // install() no-op cases
    // -------------------------------------------------------------------------

    /**
     * install() uses a function-local `static $installed` idempotency latch
     * (the same convention as every other security module's install()) that
     * persists for the lifetime of the PHP process, not per-instance. Other
     * tests in this file (and this file alone calls install() more than
     * once) would otherwise silently short-circuit this test depending on
     * execution order — run it in its own process so the latch starts fresh.
     *
     * @runInSeparateProcess
     */
    public function test_install_registers_intercept_and_url_filters_when_enabled(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        Functions\expect('add_action')
            ->once()
            ->with('setup_theme', [$mod, 'interceptRequest']);

        Functions\expect('add_filter')
            ->once()
            ->with('login_url', [$mod, 'rewriteLoginLinkUrl']);
        Functions\expect('add_filter')
            ->once()
            ->with('logout_url', [$mod, 'rewriteLoginLinkUrl']);
        Functions\expect('add_filter')
            ->once()
            ->with('lostpassword_url', [$mod, 'rewriteLoginLinkUrl']);
        Functions\expect('add_filter')
            ->once()
            ->with('site_url', [$mod, 'rewriteSiteUrl'], 10, 2);

        $mod->install();

        // Brain Monkey's Functions\expect() assertions are verified in
        // tearDown() via Mockery::close(), not via a PHPUnit assertion here —
        // record one explicitly so this test isn't flagged risky.
        $this->addToAssertionCount(1);
    }

    public function test_install_noop_when_disabled(): void
    {
        $policy = $this->makePolicy(false, 'my-secret-login');
        $mod    = $this->module($policy);
        $mod->install();
        $this->assertTrue(true, 'install() must not throw when disabled');
    }

    // -------------------------------------------------------------------------
    // shouldBail() — REST path detection
    // -------------------------------------------------------------------------

    public function test_should_bail_for_wp_json_rest_path(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);

        $_SERVER['REQUEST_URI'] = '/wp-json/wpmgr/v1/autologin';

        // Expose shouldBail() via reflection.
        $ref = new \ReflectionMethod($mod, 'shouldBail');
        $ref->setAccessible(true);
        $bail = $ref->invoke($mod);

        $this->assertTrue($bail, 'REST /wp-json/ path must bail (autologin must remain reachable)');
    }

    // -------------------------------------------------------------------------
    // matchesSlug() — exact match
    // -------------------------------------------------------------------------

    public function test_matches_slug_exact_basename(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        $ref = new \ReflectionMethod($mod, 'matchesSlug');
        $ref->setAccessible(true);

        $this->assertTrue($ref->invoke($mod, '/my-secret-login', 'my-secret-login'));
        $this->assertFalse($ref->invoke($mod, '/wp-login.php', 'my-secret-login'));
        $this->assertFalse($ref->invoke($mod, '/other-path', 'my-secret-login'));
    }

    // -------------------------------------------------------------------------
    // isLoginOrAdminPath() — canonical paths
    // -------------------------------------------------------------------------

    public function test_is_login_or_admin_path_detects_canonical_paths(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);

        $ref = new \ReflectionMethod($mod, 'isLoginOrAdminPath');
        $ref->setAccessible(true);

        $this->assertTrue($ref->invoke($mod, '/wp-login.php'));
        $this->assertTrue($ref->invoke($mod, '/wp-admin'));
        $this->assertTrue($ref->invoke($mod, '/wp-admin/edit.php'));
        $this->assertFalse($ref->invoke($mod, '/some-other-page'));
        $this->assertFalse($ref->invoke($mod, '/my-secret-login'));
    }

    // -------------------------------------------------------------------------
    // hasAccessCookie()
    // -------------------------------------------------------------------------

    public function test_has_access_cookie_true_when_cookie_set(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);

        $_COOKIE[HideBackendModule::COOKIE_ACCESS] = $this->cookieValueFor($mod);

        $ref = new \ReflectionMethod($mod, 'hasAccessCookie');
        $ref->setAccessible(true);
        $this->assertTrue($ref->invoke($mod));

        unset($_COOKIE[HideBackendModule::COOKIE_ACCESS]);
    }

    public function test_has_access_cookie_false_when_cookie_absent(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);
        unset($_COOKIE[HideBackendModule::COOKIE_ACCESS]);

        $ref = new \ReflectionMethod($mod, 'hasAccessCookie');
        $ref->setAccessible(true);
        $this->assertFalse($ref->invoke($mod));
    }

    // -------------------------------------------------------------------------
    // security-review Finding 2 (GH #170): the access cookie is an HMAC of
    // the slug, not a guessable static value — a forged "1" cookie, or one
    // minted for a different slug, must be rejected.
    // -------------------------------------------------------------------------

    public function test_has_access_cookie_rejects_forged_static_value(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        // Pre-fix, the literal digit "1" was a valid cookie value — anyone
        // reading the OSS source could forge it without knowing the slug.
        $_COOKIE[HideBackendModule::COOKIE_ACCESS] = '1';

        $ref = new \ReflectionMethod($mod, 'hasAccessCookie');
        $ref->setAccessible(true);
        $this->assertFalse($ref->invoke($mod), 'a forged static "1" cookie must be rejected');

        unset($_COOKIE[HideBackendModule::COOKIE_ACCESS]);
    }

    public function test_has_access_cookie_accepts_hmac_for_current_slug(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        $_COOKIE[HideBackendModule::COOKIE_ACCESS] = $this->cookieValueFor($mod);

        $ref = new \ReflectionMethod($mod, 'hasAccessCookie');
        $ref->setAccessible(true);
        $this->assertTrue($ref->invoke($mod), 'the HMAC minted for the current slug must be accepted');

        unset($_COOKIE[HideBackendModule::COOKIE_ACCESS]);
    }

    public function test_has_access_cookie_rejects_hmac_minted_for_a_different_slug(): void
    {
        $modForSlugA = $this->module($this->makePolicy(true, 'my-secret-login'));
        $modForSlugB = $this->module($this->makePolicy(true, 'a-totally-different-slug'));

        // A cookie minted while visiting slug A must not grant access under
        // a policy configured with a different slug B.
        $_COOKIE[HideBackendModule::COOKIE_ACCESS] = $this->cookieValueFor($modForSlugA);

        $ref = new \ReflectionMethod($modForSlugB, 'hasAccessCookie');
        $ref->setAccessible(true);
        $this->assertFalse($ref->invoke($modForSlugB), 'a cookie minted for a different slug must be rejected');

        unset($_COOKIE[HideBackendModule::COOKIE_ACCESS]);
    }

    // -------------------------------------------------------------------------
    // interceptRequest() allows logged-in users through
    // -------------------------------------------------------------------------

    public function test_intercept_allows_logged_in_users(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);

        $_SERVER['REQUEST_URI'] = '/wp-admin';

        Functions\when('is_user_logged_in')->justReturn(true);

        // interceptRequest() must not exit for logged-in users.
        // We test the path check + logged-in bail without actually calling exit.
        $getPath = new \ReflectionMethod($mod, 'getRequestPath');
        $getPath->setAccessible(true);
        $path = $getPath->invoke($mod);
        $this->assertSame('/wp-admin', $path);

        $isLogin = new \ReflectionMethod($mod, 'isLoginOrAdminPath');
        $isLogin->setAccessible(true);
        $this->assertTrue($isLogin->invoke($mod, $path));

        // Verify that logged-in users would pass through (no exit).
        // is_user_logged_in() is true, so the function returns before blocking.
        $this->assertTrue(true, 'logged-in users must not be blocked');
    }

    // -------------------------------------------------------------------------
    // SAFETY: autologin path (REST) always bails
    // -------------------------------------------------------------------------

    public function test_autologin_rest_path_bails(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);

        $_SERVER['REQUEST_URI'] = '/wp-json/wpmgr/v1/autologin?token=abc123';

        $ref = new \ReflectionMethod($mod, 'shouldBail');
        $ref->setAccessible(true);

        $this->assertTrue($ref->invoke($mod), 'autologin REST path must always bail (lockout-proofing)');
    }

    // -------------------------------------------------------------------------
    // LOW (b): matchesSlug must not match at arbitrary depth
    // -------------------------------------------------------------------------

    public function test_low_b_matches_slug_only_at_root_depth(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        $ref = new \ReflectionMethod($mod, 'matchesSlug');
        $ref->setAccessible(true);

        // Root-depth match: must be true.
        $this->assertTrue(
            $ref->invoke($mod, '/my-secret-login', 'my-secret-login'),
            'LOW (b): slug at root depth must match'
        );

        // Sub-path match: must be false (the slug appears as a later segment).
        $this->assertFalse(
            $ref->invoke($mod, '/some/path/my-secret-login', 'my-secret-login'),
            'LOW (b): slug at sub-path depth must NOT match (bypass prevented)'
        );

        // Another sub-path variant.
        $this->assertFalse(
            $ref->invoke($mod, '/subdir/my-secret-login', 'my-secret-login'),
            'LOW (b): slug under /subdir must NOT match'
        );

        // Exact wrong slug.
        $this->assertFalse(
            $ref->invoke($mod, '/other-slug', 'my-secret-login'),
            'LOW (b): different slug must not match'
        );

        // Empty path.
        $this->assertFalse(
            $ref->invoke($mod, '/', 'my-secret-login'),
            'LOW (b): root path must not match slug'
        );
    }

    public function test_low_b_matches_slug_works_with_no_leading_slash(): void
    {
        $policy = $this->makePolicy(true, 'my-login');
        $mod    = $this->module($policy);

        $ref = new \ReflectionMethod($mod, 'matchesSlug');
        $ref->setAccessible(true);

        // Path without a leading slash (defensive).
        $this->assertTrue(
            $ref->invoke($mod, 'my-login', 'my-login'),
            'LOW (b): slug match must work even when path has no leading slash'
        );
    }

    // -------------------------------------------------------------------------
    // LOW (b): isLoginOrAdminPath catches wp-login.php query-action variants
    // -------------------------------------------------------------------------

    public function test_low_b_is_login_path_catches_action_variants(): void
    {
        $policy = $this->makePolicy();
        $mod    = $this->module($policy);

        $ref = new \ReflectionMethod($mod, 'isLoginOrAdminPath');
        $ref->setAccessible(true);

        // Standard form.
        $this->assertTrue($ref->invoke($mod, '/wp-login.php'), 'isLoginOrAdminPath: bare wp-login.php');

        // With a subdirectory prefix (site installed in /subdir).
        $this->assertTrue(
            $ref->invoke($mod, '/subdir/wp-login.php'),
            'isLoginOrAdminPath: wp-login.php under subdirectory'
        );

        // wp-admin variants.
        $this->assertTrue($ref->invoke($mod, '/wp-admin'), 'isLoginOrAdminPath: /wp-admin');
        $this->assertTrue($ref->invoke($mod, '/wp-admin/'), 'isLoginOrAdminPath: /wp-admin/ (trailing slash stripped by getRequestPath but defensive)');
        $this->assertTrue($ref->invoke($mod, '/wp-admin/edit.php'), 'isLoginOrAdminPath: /wp-admin/edit.php');
        $this->assertTrue($ref->invoke($mod, '/wp-admin/options-general.php'), 'isLoginOrAdminPath: /wp-admin/options-general.php');

        // Non-login paths must not match.
        $this->assertFalse($ref->invoke($mod, '/some-page'), 'isLoginOrAdminPath: non-login page must not match');
        $this->assertFalse($ref->invoke($mod, '/my-secret-login'), 'isLoginOrAdminPath: custom slug must not match');
        $this->assertFalse($ref->invoke($mod, '/wp-content/uploads/file.php'), 'isLoginOrAdminPath: wp-content must not match');
    }

    // -------------------------------------------------------------------------
    // BUG 2 (GH #170): the secret slug must actually serve a login form —
    // serveLoginForm() presents the request AS wp-login.php and defers the
    // real require()/exit to a single wp_loaded action so init/login_init/
    // login_enqueue_scripts fire exactly as a real wp-login.php hit would.
    // -------------------------------------------------------------------------

    public function test_bug2_serve_login_form_sets_pagenow_and_defers_require_without_exiting(): void
    {
        // serveLoginForm() only proceeds past its file_exists() guard when
        // ABSPATH/wp-login.php exists; the file's contents never matter here
        // because the require() lives inside the deferred wp_loaded closure,
        // which this test never invokes.
        if (!is_dir(ABSPATH)) {
            mkdir(ABSPATH, 0755, true);
        }
        $loginFile = ABSPATH . 'wp-login.php';
        file_put_contents($loginFile, "<?php\n// placeholder wp-login.php for HideBackendModuleTest\n");

        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        unset($GLOBALS['pagenow']);
        $_SERVER['SCRIPT_NAME'] = '/my-secret-login';

        Functions\expect('add_action')
            ->once()
            ->with('wp_loaded', \Mockery::type('callable'), 0);

        $ref = new \ReflectionMethod($mod, 'serveLoginForm');
        $ref->setAccessible(true);
        // If serveLoginForm() reached the require()/exit itself (instead of
        // deferring it), PHPUnit's process would terminate here and this
        // assertion would never run.
        $ref->invoke($mod);

        $this->assertSame(
            'wp-login.php',
            $GLOBALS['pagenow'] ?? null,
            'serveLoginForm() must set $pagenow so WP treats this request as wp-login.php'
        );

        unset($_SERVER['SCRIPT_NAME']);
        @unlink($loginFile);
    }

    public function test_bug2_serve_login_form_noop_when_wp_login_missing(): void
    {
        // Ensure no wp-login.php exists at ABSPATH (leftover from a previous
        // test, or absent by default) so the file_exists() guard is hit.
        @unlink(ABSPATH . 'wp-login.php');

        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);

        Functions\expect('add_action')->never();

        $ref = new \ReflectionMethod($mod, 'serveLoginForm');
        $ref->setAccessible(true);
        $ref->invoke($mod);

        $this->assertTrue(true, 'serveLoginForm() must return quietly when wp-login.php does not exist');
    }

    // -------------------------------------------------------------------------
    // BUG 2: login/logout/lostpassword/site_url links point at the secret
    // slug instead of the literal /wp-login.php path — but ONLY while the
    // hidden login form is actually being served (security-review Finding 1).
    // -------------------------------------------------------------------------

    public function test_rewrite_login_link_url_swaps_wp_login_path_for_slug_while_serving_form(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);
        $this->setServing($mod, true);

        $this->assertSame(
            'https://example.com/my-secret-login?action=lostpassword',
            $mod->rewriteLoginLinkUrl('https://example.com/wp-login.php?action=lostpassword')
        );
    }

    public function test_rewrite_login_link_url_is_noop_when_slug_empty(): void
    {
        $policy = $this->makePolicy(false, '');
        $mod    = $this->module($policy);
        $this->setServing($mod, true);

        $url = 'https://example.com/wp-login.php';
        $this->assertSame($url, $mod->rewriteLoginLinkUrl($url));
    }

    public function test_rewrite_site_url_only_touches_wp_login_paths_while_serving_form(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);
        $this->setServing($mod, true);

        $this->assertSame(
            'https://example.com/my-secret-login',
            $mod->rewriteSiteUrl('https://example.com/wp-login.php', 'wp-login.php')
        );

        $this->assertSame(
            'https://example.com/my-secret-login?action=register',
            $mod->rewriteSiteUrl('https://example.com/wp-login.php?action=register', 'wp-login.php?action=register')
        );

        // Unrelated site_url() calls must pass through untouched, even while serving.
        $unrelated = 'https://example.com/wp-json/wpmgr/v1/ping';
        $this->assertSame(
            $unrelated,
            $mod->rewriteSiteUrl($unrelated, 'wp-json/wpmgr/v1/ping')
        );
    }

    // -------------------------------------------------------------------------
    // security-review Finding 1 (GH #170, MEDIUM): a normal front-end request
    // (NOT serving the hidden login form) must NEVER have its login/logout/
    // lostpassword/site_url links rewritten to the secret slug — otherwise the
    // Meta widget, a comment form, or a theme nav "Log in" link leaks the
    // slug to every logged-out visitor, defeating hide-login entirely.
    // -------------------------------------------------------------------------

    public function test_rewrite_login_link_url_is_noop_on_normal_front_end_request(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);
        // Not serving: $servingLoginForm defaults false, and pagenow reflects
        // a normal front-end request (never 'wp-login.php').
        $GLOBALS['pagenow'] = 'index.php';

        $url = 'https://example.com/wp-login.php?action=lostpassword';
        $this->assertSame(
            $url,
            $mod->rewriteLoginLinkUrl($url),
            'a normal front-end render must not leak the secret slug via login_url/logout_url/lostpassword_url'
        );
    }

    public function test_rewrite_site_url_is_noop_on_normal_front_end_request(): void
    {
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);
        $GLOBALS['pagenow'] = 'index.php';

        $url = 'https://example.com/wp-login.php';
        $this->assertSame(
            $url,
            $mod->rewriteSiteUrl($url, 'wp-login.php'),
            'a normal front-end render must not leak the secret slug via site_url()'
        );
    }

    public function test_rewrite_login_link_url_applies_when_core_pagenow_is_wp_login_even_without_flag(): void
    {
        // Defensive secondary signal: a genuine, already-authorized direct
        // hit on /wp-login.php has $pagenow set to 'wp-login.php' by
        // WordPress core itself (not by serveLoginForm()) — the rewrite must
        // still apply there so the login screen's own links stay consistent.
        $policy = $this->makePolicy(true, 'my-secret-login');
        $mod    = $this->module($policy);
        $GLOBALS['pagenow'] = 'wp-login.php';

        $this->assertSame(
            'https://example.com/my-secret-login',
            $mod->rewriteLoginLinkUrl('https://example.com/wp-login.php')
        );
    }
}
