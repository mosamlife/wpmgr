<?php
/**
 * GH #964: switching 2FA method on the challenge screen works.
 *
 * The "Or use:" alternatives used to be plain GET links, and the verify
 * handler reads the session id, user id and token from POST only, so every
 * switch ended on "Invalid 2FA session." The alternatives are now submit
 * buttons inside the signed form, honoured only after the full session and
 * signature check, re-rendering the chosen method without counting a code
 * attempt.
 *
 * Also covered: only a method the screen offered this user is accepted; one
 * 2FA session sends at most three email codes; and a stray GET on the verify
 * action lands back on the login form with a notice instead of a dead end.
 *
 * Reached through handleVerifySubmit() and maybeResumeInterstitial(), the
 * public entry points core reaches via login_init. wp_kses() is a throwing
 * spy so every render is captured before its exit.
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
final class Site2faMethodSwitchTest extends TestCase
{
    private const USER_ID = 810;

    /** @var array<int,array<string,mixed>> */
    private array $userMeta = [];

    /** @var list<string> */
    private array $rendered = [];

    /** @var list<string> Recipients of every wp_mail() call. */
    private array $mailSent = [];

    private bool $authCookieIssued = false;

    private const SERVER_KEYS = ['REQUEST_URI', 'HTTP_REFERER', 'HTTP_HOST', 'REQUEST_METHOD'];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->userMeta         = [];
        $this->rendered         = [];
        $this->mailSent         = [];
        $this->authCookieIssued = false;
        $_POST = [];
        $_GET  = [];
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

        Functions\when('get_option')->justReturn('');
        Functions\when('esc_url_raw')->alias(fn ($u) => (string) $u);
        Functions\when('esc_url')->alias(fn ($u) => (string) $u);
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_html')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr')->alias(fn ($t) => htmlspecialchars((string) $t, ENT_QUOTES));
        Functions\when('esc_attr__')->alias(fn ($t, $d = '') => $t);
        Functions\when('__')->alias(fn ($t, $d = '') => $t);
        Functions\when('is_ssl')->justReturn(true);
        Functions\when('admin_url')->justReturn('/wp-admin/');
        Functions\when('wp_login_url')->justReturn('https://example.test/wp-login.php');
        Functions\when('add_query_arg')->alias(fn ($args, $url = '') => $url . '?' . http_build_query($args));
        Functions\when('login_header')->justReturn(null);
        Functions\when('login_footer')->justReturn(null);
        Functions\when('wp_salt')->justReturn('test-salt-value');
        Functions\when('get_bloginfo')->justReturn('Test Site');
        Functions\when('wp_hash')->alias(fn (string $data) => hash('sha256', $data));
        Functions\when('wp_hash_password')->alias(fn ($p) => password_hash((string) $p, PASSWORD_BCRYPT));
        Functions\when('wp_check_password')->alias(fn ($p, $h, $uid = 0) => password_verify((string) $p, (string) $h));
        Functions\when('wp_mail')->alias(function ($to, $subject = '', $message = '') {
            $this->mailSent[] = (string) $to;
            return true;
        });
        Functions\when('wp_destroy_all_sessions')->justReturn(null);
        Functions\when('wp_clear_auth_cookie')->justReturn(null);
        Functions\when('wp_allowed_protocols')->justReturn(['http', 'https', 'mailto']);
        Functions\when('do_action')->justReturn(null);
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
        foreach (self::SERVER_KEYS as $key) {
            unset($_SERVER[$key]);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function makeUser(int $id): \WP_User
    {
        $u             = new \WP_User();
        $u->ID         = $id;
        $u->roles      = ['administrator'];
        $u->user_login = 'user' . $id;
        $u->user_email = 'user' . $id . '@example.com';
        return $u;
    }

    /**
     * @param list<string> $methods
     */
    private function makeModule(array $methods = ['totp', 'email', 'backup']): Site2faModule
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
                'two_factor_methods'        => $methods,
            ],
        ]);

        return new Site2faModule($policy, [new TotpProvider($ageIdentity), new EmailCodeProvider(), new BackupCodesProvider()]);
    }

    private function enrollTotp(): void
    {
        $this->userMeta[self::USER_ID][TotpProvider::META_SECRET] = base64_encode('JBSWY3DPEHPK3PXP');
    }

    /**
     * @return list<string> The plaintext backup codes.
     */
    private function enrollBackupCodes(): array
    {
        return (new BackupCodesProvider())->generateAndStore($this->makeUser(self::USER_ID));
    }

    /**
     * Mint a 2FA session the way interceptIfRequired() does and put its signed
     * fields in $_POST, as the challenge form would.
     *
     * @param array<string,string> $extra Further POST fields.
     */
    private function postSignedForm(Site2faModule $module, array $extra = [], ?string $token = null): void
    {
        $create  = new \ReflectionMethod($module, 'createSession');
        $session = $create->invoke($module, self::USER_ID, '/wp-admin/', false, '2fa');
        $hmac    = (new \ReflectionMethod($module, 'computeSessionHmac'))
            ->invoke($module, self::USER_ID, $session['id'], $session['created_at'], $session['uuid']);

        $_SERVER['REQUEST_METHOD'] = 'POST';
        $_POST = array_merge([
            'wpmgr_2fa_user_id'    => (string) self::USER_ID,
            'wpmgr_2fa_session_id' => $session['id'],
            'wpmgr_2fa_token'      => $token ?? $hmac,
            'wpmgr_2fa_provider'   => 'totp',
        ], $extra);
    }

    /**
     * Run one verify submit and say how it ended.
     *
     * @return array{html:?string,redirect:?string,die:?string}
     */
    private function submit(Site2faModule $module): array
    {
        $before = count($this->rendered);
        $result = ['html' => null, 'redirect' => null, 'die' => null];
        try {
            $module->handleVerifySubmit();
        } catch (\RuntimeException $e) {
            $message = $e->getMessage();
            if (str_starts_with($message, 'marker:redirect:')) {
                $result['redirect'] = substr($message, strlen('marker:redirect:'));
            } elseif (str_starts_with($message, 'wp_die called: ')) {
                $result['die'] = $message;
            } elseif ($message !== 'marker:wp_kses_called') {
                throw $e;
            }
        }
        if (count($this->rendered) > $before) {
            $result['html'] = $this->rendered[count($this->rendered) - 1];
        }
        return $result;
    }

    private function xpath(string $html): \DOMXPath
    {
        $doc = new \DOMDocument();
        libxml_use_internal_errors(true);
        $doc->loadHTML('<!DOCTYPE html><html><body>' . $html . '</body></html>', LIBXML_NOERROR | LIBXML_NOWARNING);
        libxml_clear_errors();
        return new \DOMXPath($doc);
    }

    private function activeProviderIn(string $html): string
    {
        $node = $this->xpath($html)->query('//input[@name="wpmgr_2fa_provider"]')->item(0);
        $this->assertInstanceOf(\DOMElement::class, $node, 'the challenge must carry the active provider');
        return $node->getAttribute('value');
    }

    /**
     * @return array<string,mixed>
     */
    private function storedSession(): array
    {
        $session = $this->userMeta[self::USER_ID][Site2faModule::META_SESSION] ?? null;
        $this->assertIsArray($session, 'the 2FA session must still exist');
        return $session;
    }

    // -------------------------------------------------------------------------
    // The alternatives are submit buttons inside the signed form
    // -------------------------------------------------------------------------

    public function test_alternatives_render_as_switch_buttons_inside_the_signed_form(): void
    {
        $this->enrollTotp();
        $this->enrollBackupCodes();
        $module = $this->makeModule();
        $user   = $this->makeUser(self::USER_ID);

        $module->onWpLogin($user->user_login, $user);

        $this->assertCount(1, $this->rendered, 'the challenge must render');
        $xpath = $this->xpath($this->rendered[0]);

        $buttons = $xpath->query('//form//button[@name="wpmgr_2fa_switch"]');
        $values  = [];
        foreach ($buttons as $button) {
            $this->assertInstanceOf(\DOMElement::class, $button);
            $this->assertSame('submit', $button->getAttribute('type'));
            $this->assertTrue($button->hasAttribute('formnovalidate'), '#964: a switch must not be blocked by the empty, required code field');
            $values[] = $button->getAttribute('value');
        }
        $this->assertSame(['email', 'backup'], $values, '#964: every other offered method is a switch button inside the form; the active one is not');

        $this->assertSame(
            0,
            $xpath->query('//a[contains(@href, "wpmgr_2fa")]')->length,
            '#964: no alternative may be a link: a GET carries none of the signed session fields'
        );

        // Enter in the code field submits the first submit button in the form,
        // so Verify must come before every switch button.
        $order = [];
        foreach ($xpath->query('//form//*[@type="submit"]') as $node) {
            $this->assertInstanceOf(\DOMElement::class, $node);
            $order[] = $node->getAttribute('name') === 'wpmgr_2fa_switch' ? 'switch' : $node->getAttribute('id');
        }
        $this->assertSame('wp-submit', $order[0] ?? '', '#964: Verify must be the form\'s first submit button');
    }

    public function test_switch_submit_renders_the_chosen_provider_without_counting_an_attempt(): void
    {
        $this->enrollTotp();
        $this->enrollBackupCodes();
        $module = $this->makeModule();

        $this->postSignedForm($module, ['wpmgr_2fa_switch' => 'backup', 'wpmgr_totp_code' => '']);
        $result = $this->submit($module);

        $this->assertNull($result['die'], '#964: a signed switch must not end on "Invalid 2FA session"');
        $this->assertNotNull($result['html'], '#964: a switch must re-render the challenge');
        $this->assertSame('backup', $this->activeProviderIn((string) $result['html']));
        $this->assertSame(
            1,
            $this->xpath((string) $result['html'])->query('//input[@name="wpmgr_backup_code"]')->length,
            '#964: the chosen method\'s own form must be shown'
        );

        $session = $this->storedSession();
        $this->assertSame(0, (int) ($session['attempts'] ?? 0), '#964: a switch is not a code attempt');
        $this->assertSame('backup', $session['provider'] ?? null, 'the chosen method must stick for the next render');
        $this->assertArrayNotHasKey(Site2faModule::META_ATTEMPT_COUNT, $this->userMeta[self::USER_ID], '#964: a switch must not touch the cross-request counter');
        $this->assertFalse($this->authCookieIssued);
    }

    public function test_code_entered_after_a_switch_completes_the_login(): void
    {
        $this->enrollTotp();
        $codes  = $this->enrollBackupCodes();
        $module = $this->makeModule();

        $this->postSignedForm($module, ['wpmgr_2fa_switch' => 'backup']);
        $this->submit($module);

        // The same signed session, now answering the backup-code form.
        $session = $this->storedSession();
        $hmac    = (new \ReflectionMethod($module, 'computeSessionHmac'))
            ->invoke($module, self::USER_ID, $session['id'], $session['created_at'], $session['uuid']);
        $_POST = [
            'wpmgr_2fa_user_id'    => (string) self::USER_ID,
            'wpmgr_2fa_session_id' => $session['id'],
            'wpmgr_2fa_token'      => $hmac,
            'wpmgr_2fa_provider'   => 'backup',
            'wpmgr_backup_code'    => $codes[0],
        ];
        $result = $this->submit($module);

        $this->assertSame('/wp-admin/', $result['redirect'], 'a valid backup code after the switch must complete the login');
        $this->assertTrue($this->authCookieIssued);
    }

    // -------------------------------------------------------------------------
    // Only a method the screen offered this user
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideUnofferedSwitches(): array
    {
        return [
            'allowed but not enrolled'      => ['not_enrolled'],
            'enrolled but not allowed'      => ['not_allowed'],
            'no such method'                => ['unknown'],
            'empty value'                   => ['empty'],
        ];
    }

    /**
     * @dataProvider provideUnofferedSwitches
     */
    public function test_switch_to_a_method_the_screen_did_not_offer_is_refused(string $case): void
    {
        $this->enrollTotp();
        switch ($case) {
            case 'not_enrolled':
                $module = $this->makeModule(['totp', 'email', 'backup']);
                $target = 'backup';
                break;
            case 'not_allowed':
                $this->enrollBackupCodes();
                $module = $this->makeModule(['totp', 'email']);
                $target = 'backup';
                break;
            case 'unknown':
                $module = $this->makeModule();
                $target = 'sms';
                break;
            default:
                $module = $this->makeModule();
                $target = '';
        }

        $this->postSignedForm($module, ['wpmgr_2fa_switch' => $target]);
        $result = $this->submit($module);

        $this->assertNotNull($result['html'], 'a refused switch re-renders the challenge');
        $this->assertStringContainsString('That verification method is not available for your account.', (string) $result['html']);
        $this->assertSame('totp', $this->activeProviderIn((string) $result['html']), 'a refused switch must leave the offered method active');

        $session = $this->storedSession();
        $this->assertNotSame($target, $session['provider'] ?? null);
        $this->assertSame(0, (int) ($session['attempts'] ?? 0), 'a refused switch is not a code attempt either');
        $this->assertFalse($this->authCookieIssued);
    }

    public function test_switch_is_honoured_only_after_the_session_signature_check(): void
    {
        $this->enrollTotp();
        $module = $this->makeModule();

        $this->postSignedForm($module, ['wpmgr_2fa_switch' => 'email'], 'not-the-real-token');
        $result = $this->submit($module);

        $this->assertNotNull($result['die']);
        $this->assertStringContainsString('Session expired or invalid', (string) $result['die']);
        $this->assertNull($result['html'], 'nothing may render for an unsigned switch');
        $this->assertSame([], $this->mailSent, 'an unsigned switch to email must not send a code');
    }

    // -------------------------------------------------------------------------
    // At most three email codes per 2FA session
    // -------------------------------------------------------------------------

    public function test_a_fourth_email_send_in_one_session_is_refused(): void
    {
        $this->enrollTotp();
        $module = $this->makeModule();

        $this->postSignedForm($module, ['wpmgr_2fa_switch' => 'email']);
        $signed = $_POST;

        for ($i = 1; $i <= 3; $i++) {
            $_POST  = $signed;
            $result = $this->submit($module);
            $this->assertNotNull($result['html']);
            $this->assertSame('email', $this->activeProviderIn((string) $result['html']));
            $this->assertCount($i, $this->mailSent, 'switch ' . $i . ' to email sends code ' . $i);
        }

        $_POST  = $signed;
        $result = $this->submit($module);

        $this->assertCount(3, $this->mailSent, '#964: the fourth email send in one 2FA session must be refused');
        $this->assertNotNull($result['html']);
        $this->assertSame('email', $this->activeProviderIn((string) $result['html']), 'the email form stays usable for a code already sent');
        $this->assertStringContainsString('No new code was sent', (string) $result['html']);
        $this->assertSame(3, (int) ($this->storedSession()['email_sends'] ?? 0));

        // The cap covers every render, not only switches: a wrong code re-renders without sending.
        $_POST  = array_merge($signed, ['wpmgr_2fa_provider' => 'email', 'wpmgr_email_code' => '00000000']);
        unset($_POST['wpmgr_2fa_switch']);
        $result = $this->submit($module);
        $this->assertNotNull($result['html']);
        $this->assertCount(3, $this->mailSent, 'a re-render after a wrong code must not exceed the cap either');
        $this->assertSame(1, (int) ($this->storedSession()['attempts'] ?? 0), 'the wrong code itself still counts as an attempt');
    }

    // -------------------------------------------------------------------------
    // A stray GET on the verify action
    // -------------------------------------------------------------------------

    public function test_stray_get_on_the_verify_action_redirects_to_the_login_form_with_a_notice(): void
    {
        $module = $this->makeModule();

        $_SERVER['REQUEST_METHOD'] = 'GET';
        $_SERVER['REQUEST_URI']    = '/my-login?action=wpmgr_2fa_verify&wpmgr_2fa_provider=email';
        $_GET = ['action' => 'wpmgr_2fa_verify', 'wpmgr_2fa_provider' => 'email'];

        $redirect = null;
        $died     = null;
        try {
            $module->maybeResumeInterstitial();
        } catch (\RuntimeException $e) {
            if (str_starts_with($e->getMessage(), 'marker:redirect:')) {
                $redirect = substr($e->getMessage(), strlen('marker:redirect:'));
            } elseif (str_starts_with($e->getMessage(), 'wp_die called: ')) {
                $died = $e->getMessage();
            } else {
                throw $e;
            }
        }

        $this->assertNull($died, '#964: a stray GET must not end on a wp_die() page');
        $this->assertSame('/my-login?wpmgr_2fa_notice=interrupted', $redirect, '#964: a stray GET goes back to the login form it came from, with the notice flag');
    }

    public function test_login_form_shows_the_notice_only_for_its_flag(): void
    {
        $module = $this->makeModule();

        $this->assertSame('<p>core</p>', $module->filterLoginMessage('<p>core</p>'), 'without the flag the login message is untouched');

        $_GET = ['wpmgr_2fa_notice' => 'something-else'];
        $this->assertSame('', $module->filterLoginMessage(''), 'an unknown flag value shows nothing');

        $_GET    = ['wpmgr_2fa_notice' => 'interrupted'];
        $message = $module->filterLoginMessage('<p>core</p>');
        $this->assertIsString($message);
        $this->assertStringStartsWith('<p>core</p>', $message, 'an existing login message is kept');
        $this->assertStringContainsString('Your two-factor verification was interrupted. Please log in again.', $message);
    }

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_login_notice_filter_is_registered_with_the_login_hooks(): void
    {
        $registered = [];
        $recorder   = function ($hook, $callback = null, $priority = 10, $args = 1) use (&$registered): bool {
            $registered[] = [(string) $hook, is_array($callback) ? (string) ($callback[1] ?? '') : ''];
            return true;
        };
        Functions\when('add_action')->alias($recorder);
        Functions\when('add_filter')->alias($recorder);

        $this->makeModule()->install();

        $this->assertContains(['login_message', 'filterLoginMessage'], $registered);
        $this->assertContains(['login_init', 'maybeResumeInterstitial'], $registered, 'sanity: the recorder saw the login hooks');
    }
}
