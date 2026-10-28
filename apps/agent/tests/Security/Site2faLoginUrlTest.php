<?php
/**
 * GH #965: every 2FA form posts back to the login URL the visitor is on.
 *
 * On a site that renames its login page, a form posting to wp_login_url() is
 * redirected (losing its body) or answered with a 404, so a user who must pass
 * 2FA can never get past the second step. On the login screen the 2FA
 * challenge, the 2FA setup screens and the forced password change all post to
 * this request's path and query, as an absolute URL on the site's own origin.
 *
 * Two shapes of login renamer are simulated:
 *  - one that serves the login form at its own slug, so the login POST arrives
 *    at that slug (REQUEST_URI is the slug);
 *  - one that serves the form at wp-login.php?<slug>, posts it to the bare
 *    wp-login.php and keeps the slug only in the page the visitor came from,
 *    so the slug survives only in the Referer.
 *
 * The form's action attribute goes through wp_kses(), which strips anything
 * before the first ":" that is not an allowed scheme. The contract that keeps
 * kses from rewriting it: an absolute http(s) URL on the site's origin whose
 * only raw ":" is the scheme's (and an explicit port's). wp_kses() is a
 * throwing spy here, so that property is asserted on the assembled action;
 * the real-kses fuzz run lives outside the suite.
 *
 * The screens are reached through onWpLogin(), the hook production fires after
 * the password step, with login_init fired first (the login screen) unless a
 * test simulates a password step on a front-end page. esc_url_raw() and
 * esc_url() delete the bytes core deletes. $_SERVER values that carry a
 * backslash are given slashed, as wp_magic_quotes() delivers them.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\BackupCodesProvider;
use WPMgr\Agent\Security\EmailCodeProvider;
use WPMgr\Agent\Security\PasswordPolicyModule;
use WPMgr\Agent\Security\SecurityPolicy;
use WPMgr\Agent\Security\Site2faModule;
use WPMgr\Agent\Security\TotpProvider;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\Site2faModule
 */
final class Site2faLoginUrlTest extends TestCase
{
    private const SECRET = 'JBSWY3DPEHPK3PXP';

    /** @var array<int,array<string,mixed>> */
    private array $userMeta = [];

    /** @var list<string> HTML handed to wp_kses() by each render. */
    private array $rendered = [];

    /** Whether loginScreen() simulates the login screen (login_init fired). */
    private bool $onLoginScreen = true;

    private bool $authCookieIssued = false;

    private const SERVER_KEYS = ['REQUEST_URI', 'HTTP_REFERER', 'HTTP_HOST', 'REQUEST_METHOD'];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->userMeta         = [];
        $this->rendered         = [];
        $this->onLoginScreen    = true;
        $this->authCookieIssued = false;
        $_POST = [];
        $_GET  = [];
        unset($_COOKIE[Site2faModule::COOKIE_BIND]);
        foreach (self::SERVER_KEYS as $key) {
            unset($_SERVER[$key]);
        }

        Functions\when('get_user_meta')->alias(function ($uid, $key, $single = false) {
            return $this->userMeta[$uid][$key] ?? '';
        });
        Functions\when('update_user_meta')->alias(function ($uid, $key, $value) {
            $this->userMeta[$uid][$key] = $value;
            return true;
        });
        Functions\when('delete_user_meta')->alias(function ($uid, $key) {
            unset($this->userMeta[$uid][$key]);
            return true;
        });
        Functions\when('get_userdata')->alias(fn ($id) => $this->makeUser((int) $id));

        $stripUrl = static function ($url): string {
            // The steps of core's esc_url() that matter here: every byte
            // outside its allow-list is deleted, ";//" becomes "://", and
            // anything that is not a path, query or fragment gets http://.
            $url = str_replace(' ', '%20', ltrim((string) $url));
            $url = (string) preg_replace('|[^a-z0-9-~+_.?#=!&;,/:%@$\|*\'()\[\]\x80-\xff]|i', '', $url);
            if ($url === '') {
                return '';
            }
            $url = str_replace(';//', '://', $url);
            if (!str_contains($url, ':') && !in_array($url[0], ['/', '#', '?'], true)
                && preg_match('/^[a-z0-9-]+?\.php/i', $url) !== 1
            ) {
                $url = 'http://' . $url;
            }
            return $url;
        };
        Functions\when('esc_url_raw')->alias($stripUrl);
        Functions\when('esc_url')->alias(static function ($url) use ($stripUrl): string {
            return str_replace(['&', "'"], ['&#038;', '&#039;'], $stripUrl($url));
        });

        Functions\when('get_option')->justReturn('');
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_html')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr__')->alias(fn ($t, $d = '') => $t);
        Functions\when('__')->alias(fn ($t, $d = '') => $t);
        Functions\when('is_ssl')->justReturn(true);
        Functions\when('admin_url')->justReturn('https://example.test/wp-admin/');
        Functions\when('wp_login_url')->justReturn('https://example.test/wp-login.php');
        Functions\when('add_query_arg')->alias(fn ($args, $url = '') => $url . '?' . http_build_query($args));
        Functions\when('login_header')->justReturn(null);
        Functions\when('login_footer')->justReturn(null);
        Functions\when('wp_salt')->justReturn('test-salt-value');
        Functions\when('get_bloginfo')->justReturn('Test Site');
        Functions\when('wp_hash')->alias(fn (string $data) => hash('sha256', $data));
        Functions\when('wp_mail')->justReturn(true);
        Functions\when('wp_destroy_all_sessions')->justReturn(null);
        Functions\when('wp_clear_auth_cookie')->justReturn(null);
        Functions\when('wp_allowed_protocols')->justReturn(['http', 'https', 'mailto']);
        Functions\when('wp_set_auth_cookie')->alias(function () {
            $this->authCookieIssued = true;
        });
        Functions\when('wp_safe_redirect')->alias(function ($url) {
            throw new \RuntimeException('marker:redirect:' . (string) $url);
        });
        Functions\when('wp_die')->alias(function ($msg, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called: ' . (string) $msg);
        });
        Functions\when('wp_kses')->alias(function ($html, $allowed = [], $protocols = []) {
            $this->rendered[] = (string) $html;
            throw new \RuntimeException('marker:wp_kses_called');
        });
    }

    protected function tear_down(): void
    {
        $_POST = [];
        $_GET  = [];
        unset($_COOKIE[Site2faModule::COOKIE_BIND]);
        foreach (self::SERVER_KEYS as $key) {
            unset($_SERVER[$key]);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function makeUser(int $id, array $roles = ['administrator']): \WP_User
    {
        $u             = new \WP_User();
        $u->ID         = $id;
        $u->roles      = $roles;
        $u->user_login = 'user' . $id;
        $u->user_email = 'user' . $id . '@example.com';
        return $u;
    }

    private function makeTotp(): TotpProvider
    {
        $ageIdentity = new class extends AgeIdentity {
            public function __construct()
            {
                // Skip parent -- no keystore needed in tests.
            }

            public function encryptChunk(string $plaintext): string
            {
                return $plaintext;
            }

            public function decryptChunk(string $ciphertext): string
            {
                return $ciphertext;
            }
        };
        return new TotpProvider($ageIdentity);
    }

    private function makeModule(): Site2faModule
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
                'two_factor_methods'        => ['totp', 'email', 'backup'],
                'two_factor_grace_logins'   => 0,
            ],
        ]);

        return new Site2faModule($policy, [$this->makeTotp(), new EmailCodeProvider(), new BackupCodesProvider()]);
    }

    /**
     * Log a user in through onWpLogin() so that the named screen renders, and
     * return the form it assembled.
     */
    private function loginScreen(string $screen, ?Site2faModule $module = null): string
    {
        $userId = 700;
        $user   = $this->makeUser($userId);

        switch ($screen) {
            case 'challenge':
                // Enrolled administrator: the standard 2FA challenge.
                $this->userMeta[$userId][TotpProvider::META_SECRET] = base64_encode(self::SECRET);
                break;
            case 'setup':
                // Required, nothing enrolled, TOTP allowed: the setup screens.
                break;
            case 'forced_change':
                $this->userMeta[$userId][PasswordPolicyModule::META_FORCE_CHANGE] = 'expiry';
                break;
            default:
                $this->fail('unknown screen ' . $screen);
        }

        if ($this->onLoginScreen) {
            // Brain Monkey records it, so did_action('login_init') is 1.
            do_action('login_init');
        }

        $before = count($this->rendered);
        // onWpLogin() swallows every Throwable, the wp_kses() marker included,
        // exactly as it would swallow an engine failure in production.
        ($module ?? $this->makeModule())->onWpLogin($user->user_login, $user);

        $this->assertCount($before + 1, $this->rendered, 'the ' . $screen . ' screen must render exactly one form');
        return $this->rendered[count($this->rendered) - 1];
    }

    private function formAction(string $html): string
    {
        $doc = new \DOMDocument();
        libxml_use_internal_errors(true);
        $doc->loadHTML('<!DOCTYPE html><html><body>' . $html . '</body></html>', LIBXML_NOERROR | LIBXML_NOWARNING);
        libxml_clear_errors();

        $form = $doc->getElementsByTagName('form')->item(0);
        $this->assertInstanceOf(\DOMElement::class, $form, 'the screen must contain a form');
        return $form->getAttribute('action');
    }

    /**
     * The contract that keeps wp_kses() from rewriting the action: an absolute
     * URL with an allowed scheme on the site's origin, and no raw ":" after
     * that origin.
     */
    private function assertActionIsOnSiteOrigin(string $action, string $originPrefix, string $context): void
    {
        $this->assertStringStartsWith($originPrefix . '/', $action, $context . ': the action must be an absolute URL on the site origin ' . $originPrefix);

        $parts = parse_url($action);
        $this->assertIsArray($parts, $context . ': the action must parse');
        $this->assertContains($parts['scheme'] ?? '', ['http', 'https'], $context . ': allowed scheme');
        $expected = parse_url($originPrefix . '/');
        $this->assertSame($expected['host'] ?? null, $parts['host'] ?? null, $context . ': host');
        $this->assertSame($expected['port'] ?? null, $parts['port'] ?? null, $context . ': port');

        $this->assertStringNotContainsString(
            ':',
            substr($action, strlen($originPrefix)),
            $context . ': no raw ":" may follow the origin, or wp_kses() can read a scheme out of it'
        );
        $this->assertDoesNotMatchRegularExpression('/[\s"\'<>`\\\\]/', $action, $context . ': no raw unsafe byte');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideScreens(): array
    {
        return [
            '2FA challenge'          => ['challenge'],
            '2FA setup'              => ['setup'],
            'forced password change' => ['forced_change'],
        ];
    }

    // -------------------------------------------------------------------------
    // The renamed login page itself
    // -------------------------------------------------------------------------

    /**
     * @dataProvider provideScreens
     */
    public function test_form_posts_back_to_a_renamed_login_slug(string $screen): void
    {
        $_SERVER['REQUEST_METHOD'] = 'POST';
        $_SERVER['REQUEST_URI']    = '/my-login';

        $this->assertSame(
            'https://example.test/my-login?action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen($screen)),
            '#965: the ' . $screen . ' form must post to the renamed login URL the visitor is on, not wp_login_url()'
        );
    }

    /**
     * @dataProvider provideScreens
     */
    public function test_bare_core_login_post_takes_the_slug_from_a_same_origin_same_path_referer(string $screen): void
    {
        $_SERVER['REQUEST_METHOD'] = 'POST';
        $_SERVER['REQUEST_URI']    = '/wp-login.php';
        $_SERVER['HTTP_REFERER']   = 'https://example.test/wp-login.php?backend&redirect=false';

        $this->assertSame(
            'https://example.test/wp-login.php?backend&redirect=false&action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen($screen)),
            '#965: when the login posts to the bare wp-login.php, the slug kept in a same-origin, same-path Referer must travel with the ' . $screen . ' form'
        );
    }

    public function test_query_style_slug_and_subdirectory_paths_are_kept(): void
    {
        $_SERVER['REQUEST_URI'] = '/?my-login';
        $this->assertSame('https://example.test/?my-login&action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));

        $_SERVER['REQUEST_URI'] = '/blog/my-login/';
        $this->assertSame('https://example.test/blog/my-login/?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    public function test_action_wpmgr_2fa_and_credential_keys_are_stripped_from_the_current_query(): void
    {
        $_SERVER['REQUEST_URI'] = '/my-login?action=lostpassword&wpmgr_2fa_user_id=7&wpmgr_2fa_provider=email&wp_lang=fr_FR'
            . '&ACTION=x&wpmgr.2fa_token=y&redirect_to=%2Fwp-admin%2F&pwd=secret&log=admin';

        $this->assertSame(
            'https://example.test/my-login?wp_lang=fr_FR&action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen('challenge')),
            '#965: action, credentials, redirect_to and every wpmgr_2fa_* key (as PHP would name it) are dropped; the rest is kept'
        );
    }

    public function test_without_a_request_uri_the_form_falls_back_to_the_login_url_path(): void
    {
        $this->assertSame('https://example.test/wp-login.php?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    public function test_host_header_never_names_the_form_origin(): void
    {
        $_SERVER['HTTP_HOST']   = 'evil.test';
        $_SERVER['REQUEST_URI'] = '/my-login';
        $this->assertSame('https://example.test/my-login?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));

        // A Referer on the Host header's origin is not the site's origin either.
        $_SERVER['REQUEST_URI']  = '/wp-login.php';
        $_SERVER['HTTP_REFERER'] = 'https://evil.test/wp-login.php?backend';
        $this->assertSame('https://example.test/wp-login.php?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    // -------------------------------------------------------------------------
    // The Referer is trusted only on the site's origin and this request's path
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideIgnoredReferers(): array
    {
        return [
            'another host'          => ['https://evil.test/wp-login.php?backend'],
            'another scheme'        => ['http://example.test/wp-login.php?backend'],
            'another port'          => ['https://example.test:8443/wp-login.php?backend'],
            'a sub-domain'          => ['https://login.example.test/wp-login.php?backend'],
            'no scheme'             => ['//example.test/wp-login.php?backend'],
            'another path'          => ['https://example.test/other-page/?backend'],
            'another install depth' => ['https://example.test/blog/wp-login.php?backend'],
        ];
    }

    /**
     * @dataProvider provideIgnoredReferers
     */
    public function test_referer_query_is_ignored_unless_origin_and_path_match(string $referer): void
    {
        $_SERVER['REQUEST_URI']  = '/wp-login.php';
        $_SERVER['HTTP_REFERER'] = $referer;

        $this->assertSame(
            'https://example.test/wp-login.php?action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen('challenge')),
            '#965: a Referer on another origin or path must contribute nothing to the form target'
        );
    }

    public function test_referer_is_consulted_only_for_the_bare_core_login_script(): void
    {
        $_SERVER['REQUEST_URI']  = '/my-login';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/my-login?injected=1';
        $this->assertSame('https://example.test/my-login?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));

        $_SERVER['REQUEST_URI']  = '/wp-login.php?reauth=1';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/wp-login.php?backend';
        $this->assertSame('https://example.test/wp-login.php?reauth=1&action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    public function test_log_pwd_and_redirect_to_are_never_copied_from_the_referer(): void
    {
        $_SERVER['REQUEST_URI']  = '/wp-login.php';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/wp-login.php?backend&log=admin&pwd=hunter2'
            . '&redirect_to=https%3A%2F%2Fevil.test%2F&PWD=x&redirect.to=y&log[]=z';

        $action = $this->formAction($this->loginScreen('challenge'));

        $this->assertSame('https://example.test/wp-login.php?backend&action=wpmgr_2fa_verify', $action);
        $this->assertStringNotContainsString('hunter2', $action, '#965: a password in the Referer must never reach the page');
        $this->assertStringNotContainsString('evil.test', $action, '#965: redirect_to must never be taken from the Referer');
    }

    public function test_action_and_wpmgr_2fa_keys_are_stripped_from_the_referer(): void
    {
        $_SERVER['REQUEST_URI']  = '/wp-login.php';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/wp-login.php?backend&action=register'
            . '&wpmgr_2fa_switch=email&act%69on=x&wpmgr+2fa_token=y&action[]=z';

        $this->assertSame('https://example.test/wp-login.php?backend&action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    // -------------------------------------------------------------------------
    // The action survives wp_kses(): absolute, site origin, no carried raw ":"
    // -------------------------------------------------------------------------

    /**
     * Site layouts: [wp_login_url(), install path prefix, expected origin prefix].
     *
     * @return array<string,array{0:string,1:string,2:string}>
     */
    private static function siteLayouts(): array
    {
        return [
            'root install'           => ['https://example.test/wp-login.php', '', 'https://example.test'],
            'subdirectory install'   => ['https://example.test/blog/wp-login.php', '/blog', 'https://example.test'],
            'multisite sub-domain'   => ['https://site2.example.test/wp-login.php', '', 'https://site2.example.test'],
            'multisite subdirectory' => ['https://example.test/site2/wp-login.php', '/site2', 'https://example.test'],
            'non-default port'       => ['https://example.test:8443/wp-login.php', '', 'https://example.test:8443'],
        ];
    }

    /**
     * Carried inputs: [REQUEST_URI suffix after the install prefix, Referer suffix or null].
     *
     * @return array<string,array{0:string,1:?string}>
     */
    private static function hostileCarriedInputs(): array
    {
        return [
            'colon and slashes in a query value'   => ['/my-login?next=x://evil.test/', null],
            'colon in a query key'                 => ['/my-login?x://evil.test=1', null],
            'encoded colon in the query'           => ['/my-login?next=x%3A%2F%2Fevil.test%2F', null],
            'colon and doubled slashes in the path' => ['/a:b//evil.test/my-login', null],
            'encoded colon in the path'            => ['/my%3Alogin', null],
            'semicolon before slashes in the path' => ['/a;//evil.test', null],
            'backslashes and a colon'              => [addslashes('/\\evil.test\\x:y/my-login'), null],
            'doubled leading slashes and a port'   => ['//evil.test:443/x', null],
            'colons in the Referer query'          => ['/wp-login.php', '/wp-login.php?a=x://evil.test/&b:c=d&javascript:alert(1)'],
            'encoded colon in the Referer query'   => ['/wp-login.php', '/wp-login.php?a=x%3A//evil.test/'],
        ];
    }

    /**
     * @return array<string,array{0:string,1:string,2:string,3:?string,4:string}>
     */
    public static function provideLayoutsAndHostileInputs(): array
    {
        $cases = [];
        foreach (self::siteLayouts() as $layout => [$loginUrl, $prefix, $origin]) {
            foreach (self::hostileCarriedInputs() as $input => [$uriSuffix, $refererSuffix]) {
                $referer = $refererSuffix === null ? null : $origin . $prefix . $refererSuffix;
                $cases[$layout . ', ' . $input] = [$loginUrl, $prefix . $uriSuffix, $origin, $referer, $prefix];
            }
        }
        return $cases;
    }

    /**
     * @dataProvider provideLayoutsAndHostileInputs
     */
    public function test_action_is_absolute_on_the_site_origin_whatever_the_request_carries(
        string $loginUrl,
        string $requestUri,
        string $origin,
        ?string $referer,
        string $prefix
    ): void {
        Functions\when('wp_login_url')->justReturn($loginUrl);
        $_SERVER['HTTP_HOST']   = 'evil.test';
        $_SERVER['REQUEST_URI'] = $requestUri;
        if ($referer !== null) {
            $_SERVER['HTTP_REFERER'] = $referer;
        }

        foreach (['challenge', 'setup', 'forced_change'] as $screen) {
            $this->assertActionIsOnSiteOrigin(
                $this->formAction($this->loginScreen($screen)),
                $origin,
                $screen . ' for ' . $requestUri
            );
        }
    }

    public function test_carried_values_are_percent_encoded(): void
    {
        $_SERVER['REQUEST_URI'] = '/my-login?next=x://evil.test/&b:c=d';
        $this->assertSame(
            'https://example.test/my-login?next=x%3A%2F%2Fevil.test%2F&b%3Ac=d&action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen('challenge'))
        );

        $_SERVER['REQUEST_URI'] = '/a:b//evil.test/my-login';
        $this->assertSame(
            'https://example.test/a%3Ab//evil.test/my-login?action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen('challenge')),
            'a path ":" is encoded; doubled slashes after the origin stay on it'
        );
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provideHostileRequestTargets(): array
    {
        return [
            'network-path reference'  => ['//evil.test/my-login', 'https://example.test/evil.test/my-login?action=wpmgr_2fa_verify'],
            'three slashes'           => ['///evil.test/my-login', 'https://example.test/evil.test/my-login?action=wpmgr_2fa_verify'],
            'backslash after slash'   => [addslashes('/\\evil.test/my-login'), 'https://example.test/evil.test/my-login?action=wpmgr_2fa_verify'],
            'tab between the slashes' => ["/\t/evil.test/my-login", 'https://example.test/evil.test/my-login?action=wpmgr_2fa_verify'],
            'absolute-form target'    => ['https://evil.test/my-login', 'https://example.test/my-login?action=wpmgr_2fa_verify'],
            'fragment'                => ['/my-login?x=1#frag', 'https://example.test/my-login?x=1&action=wpmgr_2fa_verify'],
            'markup in the query'     => ['/my-login?a="><script>', 'https://example.test/my-login?a=script&action=wpmgr_2fa_verify'],
        ];
    }

    /**
     * @dataProvider provideHostileRequestTargets
     */
    public function test_form_target_never_leaves_the_login_origin(string $requestUri, string $expected): void
    {
        $_SERVER['REQUEST_URI'] = $requestUri;

        $action = $this->formAction($this->loginScreen('challenge'));

        $this->assertSame($expected, $action);
        $this->assertActionIsOnSiteOrigin($action, 'https://example.test', $requestUri);
    }

    // -------------------------------------------------------------------------
    // A password step outside the login screen (#965 review, F2)
    // -------------------------------------------------------------------------

    public function test_password_step_outside_the_login_screen_posts_to_wp_login_url_and_completes_there(): void
    {
        $module = $this->makeModule();

        // A front-end page (an account page calling wp_signon()): login_init never fires.
        $this->onLoginScreen       = false;
        $_SERVER['REQUEST_METHOD'] = 'POST';
        $_SERVER['REQUEST_URI']    = '/my-account/';
        $action = $this->formAction($this->loginScreen('challenge', $module));

        $this->assertSame(
            'https://example.test/wp-login.php?action=wpmgr_2fa_verify',
            $action,
            'outside the login screen the form must post to wp_login_url(), where the verify handler runs'
        );

        // The browser posts the challenge to that URL: the login screen, login_init.
        $session = $this->userMeta[700][Site2faModule::META_SESSION] ?? null;
        $this->assertIsArray($session, 'the front-end password step must mint a 2FA session');
        $token = bin2hex(random_bytes(32));
        $this->userMeta[700][Site2faModule::META_SESSION]['bind_hash'] = hash('sha256', $token);
        $_COOKIE[Site2faModule::COOKIE_BIND] = $token;

        $hmac = (new \ReflectionMethod($module, 'computeSessionHmac'))
            ->invoke($module, 700, $session['id'], $session['created_at'], $session['uuid']);
        $totp = $this->makeTotp();
        $code = (new \ReflectionMethod($totp, 'generateCode'))->invoke($totp, self::SECRET, (int) (time() / 30));

        do_action('login_init');
        $_SERVER['REQUEST_URI'] = '/wp-login.php?action=wpmgr_2fa_verify';
        $_GET  = ['action' => 'wpmgr_2fa_verify'];
        $_POST = [
            'wpmgr_2fa_user_id'    => '700',
            'wpmgr_2fa_session_id' => $session['id'],
            'wpmgr_2fa_token'      => $hmac,
            'wpmgr_2fa_provider'   => 'totp',
            'wpmgr_totp_code'      => $code,
        ];

        $redirect = null;
        try {
            $module->maybeResumeInterstitial();
        } catch (\RuntimeException $e) {
            if (!str_starts_with($e->getMessage(), 'marker:redirect:')) {
                throw $e;
            }
            $redirect = substr($e->getMessage(), strlen('marker:redirect:'));
        }

        $this->assertSame('https://example.test/wp-admin/', $redirect, 'the login_init handler must complete the login');
        $this->assertTrue($this->authCookieIssued);
    }

    // -------------------------------------------------------------------------
    // The bind cookie's scope
    // -------------------------------------------------------------------------

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_bind_cookie_is_scoped_like_the_wordpress_test_cookie(): void
    {
        define('COOKIEPATH', '/');
        define('SITECOOKIEPATH', '/wp/');

        $paths = (new \ReflectionMethod(Site2faModule::class, 'bindCookiePaths'))->invoke(null);
        $this->assertSame(['/', '/wp/'], $paths, 'COOKIEPATH, plus SITECOOKIEPATH when it differs');

        $legacy = (new \ReflectionMethod(Site2faModule::class, 'loginPath'))->invoke($this->makeModule());
        $this->assertSame('/wp-login.php', $legacy, 'the path earlier releases used, so writeBindCookie() can expire it');
    }

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_bind_cookie_uses_one_path_when_the_site_and_home_paths_match(): void
    {
        define('COOKIEPATH', '/blog/');
        define('SITECOOKIEPATH', '/blog/');

        $paths = (new \ReflectionMethod(Site2faModule::class, 'bindCookiePaths'))->invoke(null);
        $this->assertSame(['/blog/'], $paths);
    }
}
