<?php
/**
 * Every 2FA submit is bound to the browser that passed the password step.
 *
 * createSession() mints a bind token, keeps only its hash in the session and
 * hands the raw token to the browser in an HttpOnly cookie. handleVerifySubmit()
 * requires that cookie for every submit that carries the session fields: a
 * code, a method switch, a setup step and a forced password change. The right
 * cookie lets the submit proceed; no cookie, or the cookie of another session,
 * restarts the login with the interrupted notice and changes nothing.
 *
 * setcookie() never reaches $_COOKIE in a test, so each session is given a
 * known bind token and the test presents (or withholds) it.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\BackupCodesProvider;
use WPMgr\Agent\Security\EmailCodeProvider;
use WPMgr\Agent\Security\SecurityPolicy;
use WPMgr\Agent\Security\Site2faModule;
use WPMgr\Agent\Security\TotpProvider;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\Site2faModule
 */
final class Site2faSessionBindingTest extends TestCase
{
    private const USER_ID  = 840;
    private const OTHER_ID = 841;
    private const SECRET   = 'JBSWY3DPEHPK3PXP';

    /** @var array<int,array<string,mixed>> */
    private array $userMeta = [];

    /** @var list<string> */
    private array $rendered = [];

    private bool $authCookieIssued = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // The verify handler runs on the login screen.
        do_action('login_init');

        $this->userMeta         = [];
        $this->rendered         = [];
        $this->authCookieIssued = false;
        $_POST = [];
        $_GET  = [];
        unset($_COOKIE[Site2faModule::COOKIE_BIND], $_SERVER['HTTP_REFERER'], $_SERVER['HTTP_HOST']);
        $_SERVER['REQUEST_METHOD'] = 'POST';
        $_SERVER['REQUEST_URI']    = '/wp-login.php?action=wpmgr_2fa_verify';

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
        Functions\when('get_userdata')->alias(function ($id) {
            $u             = new \WP_User();
            $u->ID         = (int) $id;
            $u->roles      = ['administrator'];
            $u->user_login = 'user' . $id;
            $u->user_email = 'user' . $id . '@example.com';
            return $u;
        });

        Functions\when('get_option')->justReturn('');
        Functions\when('esc_url_raw')->alias(fn ($u) => (string) $u);
        Functions\when('esc_url')->alias(fn ($u) => (string) $u);
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_html')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr__')->alias(fn ($t, $d = '') => $t);
        Functions\when('__')->alias(fn ($t, $d = '') => $t);
        Functions\when('is_ssl')->justReturn(true);
        Functions\when('admin_url')->justReturn('https://example.test/wp-admin/');
        Functions\when('site_url')->alias(fn ($path = '', $scheme = null) => 'https://example.test' . ($path === '' ? '' : '/' . ltrim((string) $path, '/')));
        Functions\when('home_url')->alias(fn ($path = '', $scheme = null) => 'https://example.test/' . ltrim((string) $path, '/'));
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
        Functions\when('wp_set_password')->justReturn(null);
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
        unset($_COOKIE[Site2faModule::COOKIE_BIND], $_SERVER['REQUEST_METHOD'], $_SERVER['REQUEST_URI']);
        Monkey\tearDown();
        parent::tear_down();
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
     * Mint a session of the given type for $userId with a known bind token.
     *
     * @return array{0:array<string,mixed>,1:string} The session and its raw bind token.
     */
    private function mintSession(Site2faModule $module, int $userId, string $type): array
    {
        $session = (new \ReflectionMethod($module, 'createSession'))->invoke($module, $userId, '/wp-admin/', false, $type);
        $token   = bin2hex(random_bytes(32));
        $this->userMeta[$userId][Site2faModule::META_SESSION]['bind_hash'] = hash('sha256', $token);
        if ($type === Site2faModule::SESSION_TYPE_2FA_SETUP) {
            $this->userMeta[$userId][Site2faModule::META_SESSION]['setup_step']      = Site2faModule::SETUP_STEP_INTRO;
            $this->userMeta[$userId][Site2faModule::META_SESSION]['grace_remaining'] = false;
        }
        return [$session, $token];
    }

    /**
     * The POST fields of one kind of submit, signed for $session.
     *
     * @param array<string,mixed> $session
     * @return array<string,string>
     */
    private function signedPost(Site2faModule $module, array $session, string $kind): array
    {
        $hmac = (new \ReflectionMethod($module, 'computeSessionHmac'))
            ->invoke($module, self::USER_ID, $session['id'], $session['created_at'], $session['uuid']);
        $post = [
            'wpmgr_2fa_user_id'    => (string) self::USER_ID,
            'wpmgr_2fa_session_id' => $session['id'],
            'wpmgr_2fa_token'      => $hmac,
            'wpmgr_2fa_provider'   => 'totp',
        ];

        switch ($kind) {
            case 'code':
                $ref = new \ReflectionMethod(TotpProvider::class, 'generateCode');
                $totp = (new \ReflectionClass(TotpProvider::class))->newInstanceWithoutConstructor();
                $post['wpmgr_totp_code'] = $ref->invoke($totp, self::SECRET, (int) (time() / 30));
                break;
            case 'switch':
                $post['wpmgr_2fa_switch'] = 'email';
                break;
            case 'setup step':
                $post['wpmgr_2fa_provider'] = 'setup';
                break;
            case 'forced change':
                $post['wpmgr_2fa_provider'] = 'forced_change';
                $post['wpmgr_fc_pass1']     = 'one new password';
                $post['wpmgr_fc_pass2']     = 'a different one';
                break;
        }
        return $post;
    }

    /**
     * @return array{html:?string,redirect:?string,die:?string}
     */
    private function submit(Site2faModule $module): array
    {
        $result = ['html' => null, 'redirect' => null, 'die' => null];
        try {
            $module->handleVerifySubmit();
        } catch (\RuntimeException $e) {
            $message = $e->getMessage();
            if (str_starts_with($message, 'marker:redirect:')) {
                $result['redirect'] = substr($message, strlen('marker:redirect:'));
            } elseif (str_starts_with($message, 'wp_die called: ')) {
                $result['die'] = $message;
            } elseif ($message === 'marker:wp_kses_called') {
                $result['html'] = $this->rendered[count($this->rendered) - 1];
            } else {
                throw $e;
            }
        }
        return $result;
    }

    /**
     * @return array<string,array{0:string,1:string}> [kind, session type]
     */
    public static function provideSubmitKinds(): array
    {
        return [
            'code'          => ['code', '2fa'],
            'method switch' => ['switch', '2fa'],
            'setup step'    => ['setup step', Site2faModule::SESSION_TYPE_2FA_SETUP],
            'forced change' => ['forced change', 'forced_change'],
        ];
    }

    /**
     * @dataProvider provideSubmitKinds
     */
    public function test_the_browser_holding_the_bind_cookie_proceeds(string $kind, string $type): void
    {
        $module = $this->makeModule();
        $this->userMeta[self::USER_ID][TotpProvider::META_SECRET] = base64_encode(self::SECRET);
        [$session, $token] = $this->mintSession($module, self::USER_ID, $type);

        $_COOKIE[Site2faModule::COOKIE_BIND] = $token;
        $_POST  = $this->signedPost($module, $session, $kind);
        $result = $this->submit($module);

        $this->assertNull($result['die']);
        switch ($kind) {
            case 'code':
                $this->assertSame('/wp-admin/', $result['redirect'], 'a valid code from the bound browser completes the login');
                $this->assertTrue($this->authCookieIssued);
                break;
            case 'switch':
                $this->assertStringContainsString('name="wpmgr_email_code"', (string) $result['html'], 'the switch re-renders the chosen method');
                break;
            case 'setup step':
                $this->assertSame(Site2faModule::SETUP_STEP_CHOOSE, $this->userMeta[self::USER_ID][Site2faModule::META_SESSION]['setup_step'] ?? null, 'the setup step advances');
                break;
            case 'forced change':
                $this->assertStringContainsString('Passwords do not match.', (string) $result['html'], 'the forced-change handler ran');
                break;
        }
    }

    /**
     * @return array<string,array{0:string,1:string,2:string}> [kind, session type, cookie]
     */
    public static function provideRefusedSubmits(): array
    {
        $cases = [];
        foreach (self::provideSubmitKinds() as $name => [$kind, $type]) {
            $cases[$name . ', no cookie']                 = [$kind, $type, 'none'];
            $cases[$name . ', another session\'s cookie'] = [$kind, $type, 'other'];
        }
        return $cases;
    }

    /**
     * @dataProvider provideRefusedSubmits
     */
    public function test_a_submit_without_this_sessions_bind_cookie_restarts_the_login(string $kind, string $type, string $cookie): void
    {
        $module = $this->makeModule();
        $this->userMeta[self::USER_ID][TotpProvider::META_SECRET] = base64_encode(self::SECRET);
        [, $otherToken]   = $this->mintSession($module, self::OTHER_ID, $type);
        [$session, ]      = $this->mintSession($module, self::USER_ID, $type);
        $before           = $this->userMeta[self::USER_ID][Site2faModule::META_SESSION];

        if ($cookie === 'other') {
            $_COOKIE[Site2faModule::COOKIE_BIND] = $otherToken;
        }
        $_POST  = $this->signedPost($module, $session, $kind);
        $result = $this->submit($module);

        $this->assertSame(
            'https://example.test/wp-login.php?wpmgr_2fa_notice=interrupted',
            $result['redirect'],
            'a submit without this session\'s bind cookie restarts the login with the interrupted notice'
        );
        $this->assertNull($result['html'], 'nothing renders');
        $this->assertNull($result['die']);
        $this->assertFalse($this->authCookieIssued, 'no login');
        $this->assertSame($before, $this->userMeta[self::USER_ID][Site2faModule::META_SESSION] ?? null, 'the pending session is left exactly as it was');
        $this->assertArrayNotHasKey(Site2faModule::META_ATTEMPT_COUNT, $this->userMeta[self::USER_ID], 'nothing is counted');
    }
}
