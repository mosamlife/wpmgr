<?php
/**
 * EmailCodeProvider tests.
 *
 * Validates:
 *   - key() returns 'email'
 *   - isConfiguredFor() requires a non-empty user_email
 *   - sendCode() stores wp_hash(code) with TTL and calls wp_mail
 *   - validate() accepts a valid, non-expired code
 *   - validate() burns ALL codes on success (single-use)
 *   - validate() rejects an expired code
 *   - validate() rejects a wrong code
 *   - validate() rejects wrong-length code
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\EmailCodeProvider;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\EmailCodeProvider
 */
final class EmailCodeProviderTest extends TestCase
{
    /** @var array<int,array<string,mixed>> */
    private array $userMeta = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->userMeta = [];

        Functions\when('get_user_meta')->alias(function ($uid, $key, $single) {
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

        Functions\when('wp_hash')->alias(fn ($v) => md5($v));
        Functions\when('wp_mail')->justReturn(true);
        Functions\when('get_bloginfo')->justReturn('Test Site');
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_html')->alias(fn ($t) => $t);
        Functions\when('esc_attr')->alias(fn ($t) => $t);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    private function makeUser(int $id = 1, string $email = 'user@example.com'): \WP_User
    {
        $u              = new \WP_User();
        $u->ID          = $id;
        $u->user_email  = $email;
        $u->user_login  = 'testuser';
        return $u;
    }

    private function provider(): EmailCodeProvider
    {
        return new EmailCodeProvider();
    }

    // -------------------------------------------------------------------------
    // Key and configuration
    // -------------------------------------------------------------------------

    public function test_key_is_email(): void
    {
        $this->assertSame('email', $this->provider()->key());
    }

    public function test_is_configured_true_when_email_set(): void
    {
        $user = $this->makeUser(1, 'test@example.com');
        $this->assertTrue($this->provider()->isConfiguredFor($user));
    }

    public function test_is_configured_false_when_email_empty(): void
    {
        $user = $this->makeUser(1, '');
        // get_userdata not called by isConfiguredFor directly — it checks user_email property.
        Functions\when('get_userdata')->justReturn(false);
        $this->assertFalse($this->provider()->isConfiguredFor($user));
    }

    // -------------------------------------------------------------------------
    // sendCode()
    // -------------------------------------------------------------------------

    public function test_send_code_stores_token_and_calls_wp_mail(): void
    {
        $user     = $this->makeUser(1);
        $provider = $this->provider();

        $sent = $provider->sendCode($user);
        $this->assertTrue($sent);

        // Token must be stored in user-meta.
        $tokens = $this->userMeta[1][EmailCodeProvider::META_TOKENS] ?? null;
        $this->assertIsArray($tokens);
        $this->assertCount(1, $tokens);
        $this->assertArrayHasKey('hash', $tokens[0]);
        $this->assertArrayHasKey('expires', $tokens[0]);
        $this->assertGreaterThan(time(), $tokens[0]['expires']);
    }

    // -------------------------------------------------------------------------
    // validate() — valid code
    // -------------------------------------------------------------------------

    public function test_validate_accepts_valid_non_expired_code(): void
    {
        $user     = $this->makeUser(1);
        $provider = $this->provider();
        $code     = '12345678';

        $this->userMeta[1][EmailCodeProvider::META_TOKENS] = [
            ['hash' => md5($code), 'expires' => time() + 900],
        ];

        $result = $provider->validate($user, ['wpmgr_email_code' => $code]);
        $this->assertTrue($result);
    }

    // -------------------------------------------------------------------------
    // validate() — burns ALL codes on success
    // -------------------------------------------------------------------------

    public function test_validate_burns_all_codes_on_success(): void
    {
        $user     = $this->makeUser(1);
        $provider = $this->provider();
        $code     = '87654321';

        $this->userMeta[1][EmailCodeProvider::META_TOKENS] = [
            ['hash' => md5($code), 'expires' => time() + 900],
            ['hash' => md5('99999999'), 'expires' => time() + 900],
        ];

        $provider->validate($user, ['wpmgr_email_code' => $code]);

        // All tokens must have been deleted.
        $this->assertArrayNotHasKey(
            EmailCodeProvider::META_TOKENS,
            $this->userMeta[1] ?? [],
            'all tokens must be burned after successful validation'
        );
    }

    // -------------------------------------------------------------------------
    // validate() — expired code rejected
    // -------------------------------------------------------------------------

    public function test_validate_rejects_expired_code(): void
    {
        $user = $this->makeUser(1);
        $code = '11223344';

        $this->userMeta[1][EmailCodeProvider::META_TOKENS] = [
            ['hash' => md5($code), 'expires' => time() - 1],  // already expired
        ];

        $result = $this->provider()->validate($user, ['wpmgr_email_code' => $code]);
        $this->assertFalse($result, 'expired code must be rejected');
    }

    // -------------------------------------------------------------------------
    // validate() — wrong code rejected
    // -------------------------------------------------------------------------

    public function test_validate_rejects_wrong_code(): void
    {
        $user = $this->makeUser(1);

        $this->userMeta[1][EmailCodeProvider::META_TOKENS] = [
            ['hash' => md5('00000000'), 'expires' => time() + 900],
        ];

        $result = $this->provider()->validate($user, ['wpmgr_email_code' => '99999999']);
        $this->assertFalse($result, 'wrong code must be rejected');
    }

    // -------------------------------------------------------------------------
    // validate() — wrong-length code rejected
    // -------------------------------------------------------------------------

    public function test_validate_rejects_wrong_length_code(): void
    {
        $user = $this->makeUser(1);
        $this->userMeta[1][EmailCodeProvider::META_TOKENS] = [
            ['hash' => md5('1234567'), 'expires' => time() + 900],
        ];

        $result = $this->provider()->validate($user, ['wpmgr_email_code' => '1234567']); // 7 digits
        $this->assertFalse($result, '7-digit code must be rejected (needs 8)');
    }

    // -------------------------------------------------------------------------
    // Translation (GH #967): the label, the placeholder and the email pass
    // through gettext, and the email keeps its placeholders when translated.
    // -------------------------------------------------------------------------

    public function test_label_placeholder_and_verification_email_are_translated(): void
    {
        // A catalogue that marks every string it is asked for, so a literal
        // that bypasses gettext shows up without the marker.
        $mark = static fn (string $text, string $domain = 'default'): string => '[' . $domain . '] ' . $text;
        Functions\when('__')->alias($mark);
        Functions\when('esc_attr__')->alias($mark);
        Functions\when('esc_html__')->alias($mark);

        $mail = null;
        Functions\when('wp_mail')->alias(function ($to, $subject, $message) use (&$mail): bool {
            $mail = [$to, $subject, $message];

            return true;
        });

        $user     = $this->makeUser(1);
        $provider = $this->provider();

        $this->assertTrue($provider->sendCode($user));
        $this->assertIsArray($mail);
        $this->assertSame('[wpmgr-agent] [Test Site] Two-factor verification code', $mail[1], 'the subject is translated and keeps the site name');
        $this->assertMatchesRegularExpression(
            '/^\[wpmgr-agent\] Your two-factor verification code is: \d{8}\n\nThis code expires in 15 minutes\.$/',
            (string) $mail[2],
            'the body is translated and keeps the generated code'
        );

        $this->assertSame('[wpmgr-agent] Email Code', $provider->label());
        $this->assertStringContainsString('placeholder="[wpmgr-agent] 8-digit code"', $provider->renderForm($user));
    }
}
