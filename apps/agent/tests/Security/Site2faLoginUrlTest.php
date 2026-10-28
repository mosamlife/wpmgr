<?php
/**
 * GH #965: every 2FA form posts back to the login URL the visitor is on.
 *
 * On a site that renames its login page, a form posting to wp_login_url() is
 * redirected (losing its body) or answered with a 404, so a user who must pass
 * 2FA can never get past the second step. The 2FA challenge, the 2FA setup
 * screens and the forced password change must all post to the path and query
 * this login request is running on.
 *
 * Two shapes of login renamer are simulated:
 *  - one that serves the login form at its own slug, so the login POST arrives
 *    at that slug (REQUEST_URI is the slug);
 *  - one that serves the form at wp-login.php?<slug>, posts it to the bare
 *    wp-login.php and keeps the slug only in the page the visitor came from,
 *    so the slug survives only in the Referer.
 *
 * The screens are reached through onWpLogin(), the hook production fires after
 * the password step. wp_kses() is a throwing spy, so the assembled form is
 * captured at the escape-at-output boundary and the render's exit is never
 * reached. esc_url_raw() and esc_url() delete the same bytes core deletes,
 * because that deletion is how "/\evil.test" turns into "//evil.test".
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
    /** @var array<int,array<string,mixed>> */
    private array $userMeta = [];

    /** @var list<string> HTML handed to wp_kses() by each render. */
    private array $rendered = [];

    private const SERVER_KEYS = ['REQUEST_URI', 'HTTP_REFERER', 'HTTP_HOST', 'REQUEST_METHOD'];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->userMeta = [];
        $this->rendered = [];
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

        $stripUrl = static function ($url): string {
            // The two steps of core's esc_url() that matter here: every byte
            // outside its allow-list is deleted, and anything that is not a
            // path, query or fragment gets http:// prepended.
            $url = str_replace(' ', '%20', ltrim((string) $url));
            $url = (string) preg_replace('|[^a-z0-9-~+_.?#=!&;,/:%@$\|*\'()\[\]\x80-\xff]|i', '', $url);
            if ($url === '') {
                return '';
            }
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

    private function makeModule(): Site2faModule
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

        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
                'two_factor_methods'        => ['totp', 'email', 'backup'],
                'two_factor_grace_logins'   => 0,
            ],
        ]);

        return new Site2faModule($policy, [new TotpProvider($ageIdentity), new EmailCodeProvider(), new BackupCodesProvider()]);
    }

    /**
     * Log a user in through onWpLogin() so that the named screen renders, and
     * return the form it assembled.
     */
    private function loginScreen(string $screen): string
    {
        $userId = 700;
        $user   = $this->makeUser($userId);

        switch ($screen) {
            case 'challenge':
                // Enrolled administrator: the standard 2FA challenge.
                $this->userMeta[$userId][TotpProvider::META_SECRET] = base64_encode('JBSWY3DPEHPK3PXP');
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

        // onWpLogin() swallows every Throwable, the wp_kses() marker included,
        // exactly as it would swallow an engine failure in production.
        $this->makeModule()->onWpLogin($user->user_login, $user);

        $this->assertCount(1, $this->rendered, 'the ' . $screen . ' screen must render exactly one form');
        return $this->rendered[0];
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
            '/my-login?action=wpmgr_2fa_verify',
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
        $_SERVER['HTTP_HOST']      = 'example.test';
        $_SERVER['HTTP_REFERER']   = 'https://example.test/wp-login.php?backend&redirect=false';

        $this->assertSame(
            '/wp-login.php?backend&redirect=false&action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen($screen)),
            '#965: when the login posts to the bare wp-login.php, the slug kept in a same-origin, same-path Referer must travel with the ' . $screen . ' form'
        );
    }

    public function test_query_style_slug_and_subdirectory_paths_are_kept(): void
    {
        $_SERVER['REQUEST_URI'] = '/?my-login';
        $this->assertSame('/?my-login&action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));

        $this->rendered         = [];
        $_SERVER['REQUEST_URI'] = '/blog/my-login/';
        $this->assertSame('/blog/my-login/?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    public function test_action_and_wpmgr_2fa_keys_are_stripped_from_the_current_query(): void
    {
        $_SERVER['REQUEST_URI'] = '/my-login?action=lostpassword&wpmgr_2fa_user_id=7&wpmgr_2fa_provider=email&wp_lang=fr_FR&ACTION=x&wpmgr.2fa_token=y';

        $this->assertSame(
            '/my-login?wp_lang=fr_FR&action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen('challenge')),
            '#965: action and every wpmgr_2fa_* key (as PHP would name it) are dropped; everything else is kept'
        );
    }

    public function test_without_a_request_uri_the_form_falls_back_to_the_login_url_path(): void
    {
        $this->assertSame('/wp-login.php?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    public function test_bind_cookie_is_scoped_to_the_login_path_in_use(): void
    {
        $_SERVER['REQUEST_URI'] = '/my-login?x=1';
        $ref = new \ReflectionMethod(Site2faModule::class, 'loginPath');
        $this->assertSame('/my-login', $ref->invoke($this->makeModule()));
    }

    // -------------------------------------------------------------------------
    // The Referer is trusted only on this request's own origin and path
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string,1:string,2:string}>
     */
    public static function provideIgnoredReferers(): array
    {
        return [
            'another host'                 => ['/wp-login.php', 'example.test', 'https://evil.test/wp-login.php?backend'],
            'another scheme'               => ['/wp-login.php', 'example.test', 'http://example.test/wp-login.php?backend'],
            'another port'                 => ['/wp-login.php', 'example.test', 'https://example.test:8443/wp-login.php?backend'],
            'a sub-domain'                 => ['/wp-login.php', 'example.test', 'https://login.example.test/wp-login.php?backend'],
            'another path'                 => ['/wp-login.php', 'example.test', 'https://example.test/other-page/?backend'],
            'another install depth'        => ['/wp-login.php', 'example.test', 'https://example.test/blog/wp-login.php?backend'],
            'no Host header'               => ['/wp-login.php', '', 'https://example.test/wp-login.php?backend'],
        ];
    }

    /**
     * @dataProvider provideIgnoredReferers
     */
    public function test_referer_query_is_ignored_unless_origin_and_path_match(string $uri, string $host, string $referer): void
    {
        $_SERVER['REQUEST_URI']  = $uri;
        $_SERVER['HTTP_REFERER'] = $referer;
        if ($host !== '') {
            $_SERVER['HTTP_HOST'] = $host;
        }

        $this->assertSame(
            '/wp-login.php?action=wpmgr_2fa_verify',
            $this->formAction($this->loginScreen('challenge')),
            '#965: a Referer on another origin or path must contribute nothing to the form target'
        );
    }

    public function test_referer_is_consulted_only_for_the_bare_core_login_script(): void
    {
        $_SERVER['HTTP_HOST']    = 'example.test';
        $_SERVER['REQUEST_URI']  = '/my-login';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/my-login?injected=1';
        $this->assertSame('/my-login?action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));

        $this->rendered          = [];
        $_SERVER['REQUEST_URI']  = '/wp-login.php?reauth=1';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/wp-login.php?backend';
        $this->assertSame('/wp-login.php?reauth=1&action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    public function test_log_pwd_and_redirect_to_are_never_copied_from_the_referer(): void
    {
        $_SERVER['REQUEST_URI']  = '/wp-login.php';
        $_SERVER['HTTP_HOST']    = 'example.test';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/wp-login.php?backend&log=admin&pwd=hunter2'
            . '&redirect_to=https%3A%2F%2Fevil.test%2F&PWD=x&redirect.to=y&log[]=z';

        $action = $this->formAction($this->loginScreen('challenge'));

        $this->assertSame('/wp-login.php?backend&action=wpmgr_2fa_verify', $action);
        $this->assertStringNotContainsString('hunter2', $action, '#965: a password in the Referer must never reach the page');
        $this->assertStringNotContainsString('evil.test', $action, '#965: redirect_to must never be taken from the Referer');
    }

    public function test_action_and_wpmgr_2fa_keys_are_stripped_from_the_referer(): void
    {
        $_SERVER['REQUEST_URI']  = '/wp-login.php';
        $_SERVER['HTTP_HOST']    = 'example.test';
        $_SERVER['HTTP_REFERER'] = 'https://example.test/wp-login.php?backend&action=register'
            . '&wpmgr_2fa_switch=email&act%69on=x&wpmgr+2fa_token=y&action[]=z';

        $this->assertSame('/wp-login.php?backend&action=wpmgr_2fa_verify', $this->formAction($this->loginScreen('challenge')));
    }

    // -------------------------------------------------------------------------
    // No open redirect: the target is path-absolute on the login's own origin
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provideHostileRequestTargets(): array
    {
        return [
            'network-path reference'  => ['//evil.test/my-login', '/evil.test/my-login?action=wpmgr_2fa_verify'],
            'three slashes'           => ['///evil.test/my-login', '/evil.test/my-login?action=wpmgr_2fa_verify'],
            'backslash after slash'   => ['/\\evil.test/my-login', '/evil.test/my-login?action=wpmgr_2fa_verify'],
            'tab between the slashes' => ["/\t/evil.test/my-login", '/evil.test/my-login?action=wpmgr_2fa_verify'],
            'absolute-form target'    => ['https://evil.test/my-login', '/my-login?action=wpmgr_2fa_verify'],
            'fragment'                => ['/my-login?x=1#frag', '/my-login?x=1&action=wpmgr_2fa_verify'],
            'markup in the query'     => ['/my-login?a="><script>', '/my-login?a=script&action=wpmgr_2fa_verify'],
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
        $this->assertMatchesRegularExpression(
            '#^/(?![/\\\\])#',
            $action,
            '#965: the form target must be a path-absolute reference, never "//host" or "/\\host"'
        );
    }
}
