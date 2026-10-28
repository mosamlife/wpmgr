<?php
/**
 * GH #966: allowing email codes does not challenge everyone.
 *
 * The email provider reports itself configured for any user with an address,
 * so with email in the allowed methods every user had a "configured" provider
 * and the old skip (`!required && no providers`) never fired: users outside
 * the enforced roles who never enrolled anything got an email-code challenge
 * on every login.
 *
 * The rule: a user for whom 2FA is not required is challenged only when they
 * enrolled a non-email method themselves. For a user who is required, email
 * stays an allowed method and the fallback.
 *
 * Reached through onWpLogin(), the hook production fires after the password
 * step. login_header() records that a screen started rendering and then
 * throws, so a render never reaches its exit; onWpLogin() swallows the throw
 * exactly as it swallows an engine failure in production.
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
final class Site2faOptionalEmailChallengeTest extends TestCase
{
    /** @var array<int,array<string,mixed>> */
    private array $userMeta = [];

    /** @var list<string> Titles passed to login_header(), one per screen started. */
    private array $screens = [];

    /** @var list<string> Recipients of every wp_mail() call. */
    private array $mailSent = [];

    private int $sessionsDestroyed = 0;

    /** @var list<string> HTML handed to wp_kses(). */
    private array $rendered = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->userMeta          = [];
        $this->screens           = [];
        $this->mailSent          = [];
        $this->sessionsDestroyed = 0;
        $this->rendered          = [];
        $_POST = [];
        unset($_SERVER['REQUEST_URI'], $_SERVER['HTTP_REFERER'], $_SERVER['HTTP_HOST']);

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
        Functions\when('wp_salt')->justReturn('test-salt-value');
        Functions\when('get_bloginfo')->justReturn('Test Site');
        Functions\when('wp_hash')->alias(fn (string $data) => hash('sha256', $data));
        Functions\when('wp_hash_password')->alias(fn ($p) => password_hash((string) $p, PASSWORD_BCRYPT));
        Functions\when('wp_allowed_protocols')->justReturn(['http', 'https', 'mailto']);
        Functions\when('wp_mail')->alias(function ($to, $subject = '', $message = '') {
            $this->mailSent[] = (string) $to;
            return true;
        });
        Functions\when('wp_destroy_all_sessions')->alias(function () {
            $this->sessionsDestroyed++;
        });
        Functions\when('wp_clear_auth_cookie')->justReturn(null);
        Functions\when('wp_kses')->alias(function ($html, $allowed = [], $protocols = []) {
            $this->rendered[] = (string) $html;
            return (string) $html;
        });
        Functions\when('login_footer')->justReturn(null);
        Functions\when('login_header')->alias(function ($title = '') {
            $this->screens[] = (string) $title;
            throw new \RuntimeException('marker:screen_started');
        });
        Functions\when('wp_die')->alias(function ($msg, $title = '', $args = []) {
            throw new \RuntimeException('wp_die called: ' . (string) $msg);
        });
    }

    protected function tear_down(): void
    {
        $_POST = [];
        Monkey\tearDown();
        parent::tear_down();
    }

    private function makeUser(int $id, string $role): \WP_User
    {
        $u             = new \WP_User();
        $u->ID         = $id;
        $u->roles      = [$role];
        $u->user_login = $role . $id;
        $u->user_email = $role . $id . '@example.com';
        return $u;
    }

    /**
     * 2FA enforced for administrators only, with email among the allowed methods.
     *
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
                'two_factor_grace_logins'   => 0,
            ],
        ]);

        return new Site2faModule($policy, [new TotpProvider($ageIdentity), new EmailCodeProvider(), new BackupCodesProvider()]);
    }

    private function logIn(Site2faModule $module, \WP_User $user): void
    {
        $module->onWpLogin($user->user_login, $user);
    }

    /**
     * @return array<string,array{0:list<string>}>
     */
    public static function provideMethodSetsWithEmail(): array
    {
        return [
            'totp, email and backup' => [['totp', 'email', 'backup']],
            'email and backup'       => [['email', 'backup']],
            'email only'             => [['email']],
        ];
    }

    /**
     * @dataProvider provideMethodSetsWithEmail
     *
     * @param list<string> $methods
     */
    public function test_editor_with_nothing_enrolled_is_not_challenged_when_2fa_is_enforced_only_for_administrators(array $methods): void
    {
        $editor = $this->makeUser(900, 'editor');

        $this->logIn($this->makeModule($methods), $editor);

        $this->assertSame([], $this->screens, '#966: an editor outside the enforced roles who never enrolled must not see a 2FA screen');
        $this->assertSame([], $this->mailSent, '#966: and must not be sent an email code');
        $this->assertSame(0, $this->sessionsDestroyed, '#966: and must keep the session the password step just issued');
        $this->assertArrayNotHasKey(Site2faModule::META_SESSION, $this->userMeta[900] ?? [], 'no 2FA session may be minted');
    }

    public function test_editor_who_enrolled_totp_is_challenged(): void
    {
        $editor = $this->makeUser(901, 'editor');
        $this->userMeta[901][TotpProvider::META_SECRET] = base64_encode('JBSWY3DPEHPK3PXP');

        $this->logIn($this->makeModule(), $editor);

        $this->assertSame(['Two-Factor Authentication'], $this->screens, '#966: an editor who enrolled TOTP themselves is challenged');
        $this->assertSame(1, $this->sessionsDestroyed, 'the half-issued session is destroyed before the challenge');
        $this->assertSame('2fa', $this->userMeta[901][Site2faModule::META_SESSION]['type'] ?? null);
        $this->assertSame([], $this->mailSent, 'TOTP is offered first, so no email is sent');
    }

    public function test_editor_who_enrolled_backup_codes_is_challenged(): void
    {
        $editor = $this->makeUser(902, 'editor');
        (new BackupCodesProvider())->generateAndStore($editor);

        $this->logIn($this->makeModule(), $editor);

        $this->assertSame(['Two-Factor Authentication'], $this->screens, 'backup codes are a non-email method the user enrolled');
    }

    public function test_administrator_with_nothing_enrolled_is_routed_to_setup(): void
    {
        $admin = $this->makeUser(903, 'administrator');

        $this->logIn($this->makeModule(), $admin);

        $this->assertSame(['Two-Factor Setup'], $this->screens, '#966 must not relax enforcement: a required administrator is still intercepted');
        $this->assertSame(Site2faModule::SESSION_TYPE_2FA_SETUP, $this->userMeta[903][Site2faModule::META_SESSION]['type'] ?? null);
    }

    public function test_administrator_keeps_email_as_the_allowed_fallback(): void
    {
        $admin = $this->makeUser(904, 'administrator');

        // TOTP not allowed and grace exhausted: the required user's challenge is the email code.
        $this->logIn($this->makeModule(['email', 'backup']), $admin);

        $this->assertSame(['Two-Factor Authentication'], $this->screens, 'a required administrator is challenged');
        $this->assertSame(['administrator904@example.com'], $this->mailSent, 'for a required user email stays the allowed method and fallback');
        $this->assertSame('2fa', $this->userMeta[904][Site2faModule::META_SESSION]['type'] ?? null);
    }
}
