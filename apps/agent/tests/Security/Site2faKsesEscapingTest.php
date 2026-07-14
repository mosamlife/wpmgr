<?php
/**
 * Regression tests for the 2026-07 wp.org review escape-late fix:
 * Site2faModule's assembled-form echo sinks (renderForcedChangeForm,
 * renderInterstitial, renderSetupScreen, renderProfileSection) now run
 * through `wp_kses($html, self::allowedFormKses(), self::allowedFormProtocols())`
 * instead of a raw `echo $formHtml; // phpcs:ignore ...OutputNotEscaped`.
 *
 * wp_kses_post() is deliberately NOT used: its default $allowedposttags
 * omits <form>/<input>/<button>, which every one of these screens needs to
 * render (and submit) at all. These tests prove:
 *
 *   1. Every one of the four echo sinks actually calls wp_kses() (not a bare
 *      echo) with the module's own explicit allowlist + protocol list.
 *   2. allowedFormKses() allows form/input/button/svg/rect (the exact set
 *      wp_kses_post() would strip) plus every other tag/attribute the module
 *      emits.
 *   3. allowedFormProtocols() extends WP's default protocol list with
 *      `data:` (needed for the backup-codes step's client-side download
 *      link) without dropping the defaults.
 *   4. Every tag/attribute actually present in each render path's raw
 *      output is covered by allowedFormKses() -- a structural regression
 *      guard so a future form change can't silently fall outside the
 *      allowlist and get its markup stripped (or worse, someone "fixes" the
 *      resulting breakage by reaching for wp_kses_post()).
 *
 * We invoke each private render*() method directly via reflection rather
 * than through its public routing entrypoint (unlike most of this module's
 * other tests) because renderForcedChangeForm()/renderInterstitial()/
 * renderSetupScreen() all end with a literal `exit;` -- calling wp_kses()
 * as a throwing spy (recording its args before throwing) lets us observe
 * the call without ever reaching that exit() and killing the PHPUnit
 * process.
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
final class Site2faKsesEscapingTest extends TestCase
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

        Functions\when('get_option')->justReturn('');
        Functions\when('update_option')->justReturn(true);
        Functions\when('wp_json_encode')->alias(fn ($v) => json_encode($v));
        Functions\when('esc_url_raw')->alias(fn ($u) => $u);
        Functions\when('esc_url')->alias(fn ($u) => $u);
        Functions\when('add_action')->justReturn(true);
        Functions\when('add_filter')->justReturn(true);
        Functions\when('is_ssl')->justReturn(false);
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_html')->alias(fn ($t) => $t);
        Functions\when('esc_attr')->alias(fn ($t) => $t);
        Functions\when('esc_attr__')->alias(fn ($t, $d = '') => $t);
        Functions\when('__')->alias(fn ($t, $d = '') => $t);
        Functions\when('wp_login_url')->justReturn('/wp-login.php');
        Functions\when('add_query_arg')->alias(fn ($args, $url = '') => $url . '?' . http_build_query($args));
        Functions\when('login_header')->justReturn(null);
        Functions\when('login_footer')->justReturn(null);
        Functions\when('wp_salt')->justReturn('test-salt-value');
        Functions\when('get_bloginfo')->justReturn('Test Site');
        Functions\when('wp_hash')->alias(fn (string $data) => hash('sha256', $data));
        Functions\when('wp_mail')->justReturn(true);
        Functions\when('sanitize_text_field')->alias(fn ($t) => $t);
        Functions\when('sanitize_file_name')->alias(fn ($t) => preg_replace('/[^A-Za-z0-9._-]/', '-', (string) $t));
        Functions\when('wp_create_nonce')->justReturn('test-nonce');
        // Real WP default: no 'data' scheme. allowedFormProtocols() must add it.
        Functions\when('wp_allowed_protocols')->justReturn(['http', 'https', 'mailto']);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    private function makeUser(int $id, array $roles = ['administrator']): \WP_User
    {
        $u             = new \WP_User();
        $u->ID         = $id;
        $u->roles      = $roles;
        $u->user_login = 'user' . $id;
        $u->user_email = 'user' . $id . '@example.com';
        return $u;
    }

    private function makeProviders(): array
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

        return [
            new TotpProvider($ageIdentity),
            new EmailCodeProvider(),
            new BackupCodesProvider(),
        ];
    }

    private function makeModule(SecurityPolicy $policy): Site2faModule
    {
        return new Site2faModule($policy, $this->makeProviders());
    }

    /**
     * @return array<string,array<string,bool>>
     */
    private function allowedFormKses(): array
    {
        $ref = new \ReflectionMethod(Site2faModule::class, 'allowedFormKses');
        return $ref->invoke(null);
    }

    /**
     * @return string[]
     */
    private function allowedFormProtocols(): array
    {
        $ref = new \ReflectionMethod(Site2faModule::class, 'allowedFormProtocols');
        return $ref->invoke(null);
    }

    /**
     * Install a throwing wp_kses() spy that records every call's args, then
     * asserts exactly one call happened using the module's own allowlist +
     * protocol helpers, and returns the captured HTML (pre-filtering, since
     * the spy never reaches real wp_kses filtering -- that is intentional;
     * see the class docblock).
     *
     * @param callable $invoke Callback that triggers the render path.
     * @return string Captured $html argument from the single wp_kses() call.
     */
    private function captureWpKsesCall(callable $invoke): string
    {
        $captured = [];
        Functions\when('wp_kses')->alias(function ($html, $allowed = [], $protocols = []) use (&$captured) {
            $captured[] = ['html' => $html, 'allowed' => $allowed, 'protocols' => $protocols];
            throw new \RuntimeException('marker:wp_kses_called');
        });

        $threw = false;
        try {
            $invoke();
        } catch (\RuntimeException $e) {
            if ($e->getMessage() === 'marker:wp_kses_called') {
                $threw = true;
            } else {
                throw $e;
            }
        }

        $this->assertTrue($threw, 'render path must reach an escape-at-output-boundary wp_kses() call');
        $this->assertCount(1, $captured, 'render path must call wp_kses() exactly once');

        $this->assertSame(
            $this->allowedFormKses(),
            $captured[0]['allowed'],
            'render path must pass the module\'s own allowedFormKses() allowlist, not an ad-hoc or wp_kses_post() shape'
        );
        $this->assertSame(
            $this->allowedFormProtocols(),
            $captured[0]['protocols'],
            'render path must pass the module\'s own allowedFormProtocols() (default protocols + data:)'
        );

        return (string) $captured[0]['html'];
    }

    // -------------------------------------------------------------------------
    // 1) allowedFormKses() / allowedFormProtocols() structural assertions
    // -------------------------------------------------------------------------

    public function test_allowed_form_kses_permits_form_input_button_unlike_wp_kses_post(): void
    {
        $allowed = $this->allowedFormKses();

        // The whole reason wp_kses_post() cannot be used for these screens.
        $this->assertArrayHasKey('form', $allowed);
        $this->assertArrayHasKey('input', $allowed);
        $this->assertArrayHasKey('button', $allowed);

        // The inline TOTP QR code (QrEncoder::toSvg()).
        $this->assertArrayHasKey('svg', $allowed);
        $this->assertArrayHasKey('rect', $allowed);

        $this->assertArrayHasKey('action', $allowed['form']);
        $this->assertArrayHasKey('method', $allowed['form']);
        $this->assertArrayHasKey('type', $allowed['input']);
        $this->assertArrayHasKey('required', $allowed['input']);
        $this->assertArrayHasKey('viewbox', $allowed['svg']);
    }

    public function test_allowed_form_protocols_extends_defaults_with_data_scheme(): void
    {
        $protocols = $this->allowedFormProtocols();

        $this->assertContains('data', $protocols, 'the backup-codes download link is a data: URI');
        // Must extend, not replace, WP's own defaults.
        $this->assertContains('http', $protocols);
        $this->assertContains('https', $protocols);
    }

    // -------------------------------------------------------------------------
    // 2) Every echo sink actually calls wp_kses() with the module's allowlist
    // -------------------------------------------------------------------------

    public function test_render_forced_change_form_escapes_via_wp_kses(): void
    {
        $policy = SecurityPolicy::fromArray(['policy' => ['two_factor_enabled' => true]]);
        $module = $this->makeModule($policy);
        $userId = 501;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, 'forced_change');

        $renderRef = new \ReflectionMethod($module, 'renderForcedChangeForm');

        $html = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, 'expiry', '')
        );

        $this->assertStringContainsString('<form', $html);
        $this->assertStringContainsString('wpmgr_fc_pass1', $html);
    }

    public function test_render_interstitial_escapes_via_wp_kses(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 502;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, '2fa');

        $renderRef = new \ReflectionMethod($module, 'renderInterstitial');

        $html = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertStringContainsString('<form', $html);
        $this->assertStringContainsString('wpmgr_2fa_token', $html);
    }

    public function test_render_setup_screen_totp_step_escapes_via_wp_kses_and_includes_svg(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 503;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, Site2faModule::SESSION_TYPE_2FA_SETUP);
        $session['setup_step'] = Site2faModule::SETUP_STEP_TOTP;

        $renderRef = new \ReflectionMethod($module, 'renderSetupScreen');

        $html = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertStringContainsString('<form', $html);
        // The QR code -- the reason svg/rect are in the allowlist at all.
        $this->assertStringContainsString('<svg', $html);
        $this->assertStringContainsString('<rect', $html);
    }

    public function test_render_setup_screen_backup_step_escapes_via_wp_kses_and_keeps_data_uri_download(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 504;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, Site2faModule::SESSION_TYPE_2FA_SETUP);
        $session['setup_step']    = Site2faModule::SETUP_STEP_BACKUP;
        $session['backup_codes']  = ['1111111111', '2222222222'];

        $renderRef = new \ReflectionMethod($module, 'renderSetupScreen');

        $html = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertStringContainsString('<ol', $html);
        $this->assertStringContainsString('1111111111', $html);
        // The client-side download link -- proves allowedFormProtocols()'s
        // data: addition is actually needed by real emitted markup.
        $this->assertStringContainsString('href="data:text/plain', $html);
        $this->assertStringContainsString('download=', $html);
    }

    public function test_render_profile_section_escapes_via_wp_kses(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $user   = $this->makeUser(505);

        $html = $this->captureWpKsesCall(
            fn () => $module->renderProfileSection($user)
        );

        $this->assertStringContainsString('<table', $html);
        $this->assertStringContainsString('wpmgr_2fa_profile_nonce', $html);
    }

    // -------------------------------------------------------------------------
    // 3) Structural coverage: every tag/attribute actually emitted is allowed
    // -------------------------------------------------------------------------

    /**
     * Parse a raw HTML fragment (the pre-kses string the module built) via
     * DOMDocument (which correctly respects quoting -- unlike a naive regex,
     * it will not mistake `charset=` inside a quoted `data:text/plain;
     * charset=utf-8,...` attribute VALUE for a second attribute NAME) and
     * assert every element/attribute pair is a key in allowedFormKses() --
     * catching a future form edit that adds a tag/attribute without updating
     * the allowlist (which would otherwise only surface as silently-stripped
     * markup in production).
     */
    private function assertEveryTagAndAttributeIsAllowlisted(string $html): void
    {
        $allowed = $this->allowedFormKses();

        $this->assertNotSame('', $html, 'sanity: captured HTML must not be empty');

        $doc = new \DOMDocument();
        libxml_use_internal_errors(true);
        $doc->loadHTML(
            '<!DOCTYPE html><html><body>' . $html . '</body></html>',
            LIBXML_NOERROR | LIBXML_NOWARNING
        );
        libxml_clear_errors();

        $body = $doc->getElementsByTagName('body')->item(0);
        $this->assertNotNull($body, 'sanity: DOMDocument must parse the fragment');

        $seenAnyTag = false;
        $walker      = function (\DOMNode $node) use (&$walker, &$seenAnyTag, $allowed): void {
            if ($node instanceof \DOMElement) {
                $tag = strtolower($node->tagName);
                $seenAnyTag = true;
                $this->assertArrayHasKey(
                    $tag,
                    $allowed,
                    "tag <$tag> is emitted by the module but missing from allowedFormKses()"
                );
                foreach ($node->attributes as $attr) {
                    $attrLow = strtolower($attr->name);
                    $this->assertArrayHasKey(
                        $attrLow,
                        $allowed[$tag],
                        "attribute \"$attrLow\" on <$tag> is emitted by the module but missing from allowedFormKses()['$tag']"
                    );
                }
            }
            foreach ($node->childNodes as $child) {
                $walker($child);
            }
        };
        foreach ($body->childNodes as $child) {
            $walker($child);
        }

        $this->assertTrue($seenAnyTag, 'sanity: at least one tag must be present');
    }

    public function test_forced_change_form_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray(['policy' => ['two_factor_enabled' => true]]);
        $module = $this->makeModule($policy);
        $userId = 511;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, 'forced_change');

        $renderRef = new \ReflectionMethod($module, 'renderForcedChangeForm');
        $html      = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, 'expiry', 'Some error')
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }

    public function test_setup_totp_step_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 512;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, Site2faModule::SESSION_TYPE_2FA_SETUP);
        $session['setup_step'] = Site2faModule::SETUP_STEP_TOTP;

        $renderRef = new \ReflectionMethod($module, 'renderSetupScreen');
        $html      = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }

    public function test_setup_choose_step_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 513;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, Site2faModule::SESSION_TYPE_2FA_SETUP);
        $session['setup_step'] = Site2faModule::SETUP_STEP_CHOOSE;

        $renderRef = new \ReflectionMethod($module, 'renderSetupScreen');
        $html      = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }

    public function test_setup_backup_step_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 514;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, Site2faModule::SESSION_TYPE_2FA_SETUP);
        $session['setup_step']   = Site2faModule::SETUP_STEP_BACKUP;
        $session['backup_codes'] = ['3333333333', '4444444444'];

        $renderRef = new \ReflectionMethod($module, 'renderSetupScreen');
        $html      = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }

    public function test_setup_done_step_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 515;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, Site2faModule::SESSION_TYPE_2FA_SETUP);
        $session['setup_step'] = Site2faModule::SETUP_STEP_DONE;

        $renderRef = new \ReflectionMethod($module, 'renderSetupScreen');
        $html      = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, '')
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }

    public function test_interstitial_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $userId = 516;
        $user   = $this->makeUser($userId);

        $createRef = new \ReflectionMethod($module, 'createSession');
        $session   = $createRef->invoke($module, $userId, '/wp-admin/', false, '2fa');

        $renderRef = new \ReflectionMethod($module, 'renderInterstitial');
        $html      = $this->captureWpKsesCall(
            fn () => $renderRef->invoke($module, $user, $session, 'Invalid code')
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }

    public function test_profile_section_output_is_fully_covered_by_allowlist(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $module = $this->makeModule($policy);
        $user   = $this->makeUser(517);

        $html = $this->captureWpKsesCall(
            fn () => $module->renderProfileSection($user)
        );

        $this->assertEveryTagAndAttributeIsAllowlisted($html);
    }
}
