<?php
/**
 * Site2faModule hook-REGISTRATION tests (issue #523).
 *
 * WHY THIS FILE EXISTS, SEPARATELY FROM Site2faModuleTest:
 * Site2faModuleTest covers what the app-password callback DOES when you call
 * it directly. That is exactly the coverage shape that let the "H1 fix" sit
 * dead in the tree: the callback was added to
 * `wp_authenticate_application_password`, which is a core FUNCTION
 * (wp-includes/user.php, registered by core on `authenticate` at priority 20
 * in wp-includes/default-filters.php) and NOT a hook anybody ever fires.
 * add_filter() against a name that is never fired stores the callback and
 * warns about nothing, so every body-level test stayed green while the control
 * had never executed on a single site.
 *
 * The tests here therefore assert the REGISTRATION and then DISPATCH the hook
 * the way WordPress core dispatches it -- same hook name, same argument list,
 * sliced to the arity the module actually declared -- and assert the observable
 * result. A wrong hook name, a wrong arity or a wrong argument order all fail
 * here, and none of them fail in a body-level test.
 *
 * PROCESS ISOLATION IS LOAD BEARING:
 * install() guards itself with a `static $installed` local, and
 * Site2faModuleTest defines WPMGR_DISABLE_SITE_2FA process-globally. Both are
 * per-process state that would make install() a silent no-op here and turn
 * every assertion below vacuous. @runInSeparateProcess +
 * @preserveGlobalState disabled is what keeps them honest.
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
final class Site2faHookRegistrationTest extends TestCase
{
    /**
     * The core action the app-password block must be registered on.
     *
     * Fired by wp_authenticate_application_password() in wp-includes/user.php
     * as do_action( 'wp_authenticate_application_password_errors', $error,
     * $user, $item, $password ) -- after the supplied password has matched a
     * stored application-password hash and before core records its use. Core
     * documents it as the place "for plugins to add additional constraints to
     * prevent an application password from being used": populate the passed
     * WP_Error and core abandons the authentication.
     */
    private const APP_PASSWORD_HOOK = 'wp_authenticate_application_password_errors';

    /**
     * Every add_action()/add_filter() call made during install(), in order.
     *
     * @var list<array{hook:string,callback:mixed,priority:int,accepted_args:int}>
     */
    private array $registered = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->registered = [];

        // Record registrations instead of discarding them. Brain Monkey's own
        // add_action/add_filter interception is not used here because we need
        // the priority and arity, and we need to replay the callback later.
        $recorder = function ($hook, $callback = null, $priority = 10, $acceptedArgs = 1): bool {
            $this->registered[] = [
                'hook'          => (string) $hook,
                'callback'      => $callback,
                'priority'      => (int) $priority,
                'accepted_args' => (int) $acceptedArgs,
            ];
            return true;
        };
        Functions\when('add_action')->alias($recorder);
        Functions\when('add_filter')->alias($recorder);

        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_html')->alias(fn ($t) => $t);
        Functions\when('__')->alias(fn ($t, $d = '') => $t);
        Functions\when('sanitize_text_field')->alias(fn ($t) => $t);
        Functions\when('wp_unslash')->alias(fn ($t) => $t);
        Functions\when('get_user_meta')->justReturn('');
        Functions\when('update_user_meta')->justReturn(true);
        Functions\when('delete_user_meta')->justReturn(true);
        Functions\when('get_option')->justReturn('');
    }

    protected function tear_down(): void
    {
        unset($_SERVER['REQUEST_URI']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * @param list<string> $roles
     */
    private function makeUser(int $id = 1, array $roles = ['administrator']): \WP_User
    {
        $u             = new \WP_User();
        $u->ID         = $id;
        $u->roles      = $roles;
        $u->user_login = 'user' . $id;
        $u->user_email = 'user' . $id . '@example.com';
        return $u;
    }

    /**
     * @return list<\WPMgr\Agent\Security\SiteTwoFactorProvider>
     */
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

    /**
     * Prove the recorder is wired up before any "nothing was registered"
     * assertion leans on it. A recorder that silently captured nothing would
     * make every negative assertion in this file pass for the wrong reason.
     *
     * @return void
     */
    private function assertRecorderIsLive(): void
    {
        $before = count($this->registered);
        add_action('wpmgr_recorder_self_check', 'wpmgr_recorder_self_check_cb', 7, 2);
        $this->assertCount(
            $before + 1,
            $this->registered,
            'instrument self-check: the add_action recorder must capture registrations'
        );
        array_pop($this->registered);
    }

    /**
     * Find the single recorded registration for a hook name.
     *
     * @param string $hook
     * @return array{hook:string,callback:mixed,priority:int,accepted_args:int}|null
     */
    private function findRegistration(string $hook): ?array
    {
        foreach ($this->registered as $entry) {
            if ($entry['hook'] === $hook) {
                return $entry;
            }
        }
        return null;
    }

    /**
     * Replay a recorded registration exactly as WordPress core would: core
     * slices the fired argument list down to the callback's declared
     * accepted_args before invoking it (WP_Hook::apply_filters). Declaring the
     * wrong arity therefore changes what the callback actually receives, which
     * is why the slice is reproduced here rather than splatting all four.
     *
     * @param array{hook:string,callback:mixed,priority:int,accepted_args:int} $registration
     * @param list<mixed>                                                      $args
     * @return mixed
     */
    private function fireAsCore(array $registration, array $args): mixed
    {
        $sliced = array_slice($args, 0, $registration['accepted_args']);
        return call_user_func_array($registration['callback'], $sliced);
    }

    private function policyRequiringAdmins(): SecurityPolicy
    {
        return SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
    }

    // -------------------------------------------------------------------------
    // #523: the app-password block must be registered on a hook core FIRES
    // -------------------------------------------------------------------------

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_app_password_block_is_registered_on_a_hook_core_actually_fires(): void
    {
        $module = new Site2faModule($this->policyRequiringAdmins(), $this->makeProviders());
        $module->install();

        $names = array_column($this->registered, 'hook');

        $this->assertContains(
            self::APP_PASSWORD_HOOK,
            $names,
            '#523: the app-password block must be registered on ' . self::APP_PASSWORD_HOOK
            . ', the action core fires. Registered instead: ' . implode(', ', $names)
        );

        $registration = $this->findRegistration(self::APP_PASSWORD_HOOK);
        $this->assertNotNull($registration);
        $this->assertSame(
            4,
            $registration['accepted_args'],
            '#523: core fires ' . self::APP_PASSWORD_HOOK . ' with ($error, $user, $item, $password)'
            . ' -- a callback declaring any other arity is handed the wrong arguments'
        );
    }

    /**
     * The registration must not have been "fixed" by moving the block onto the
     * `authenticate` filter, which every wp-login.php form post also runs
     * through. That would put the app-password refusal in the path of ordinary
     * interactive logins, which are supposed to reach the 2FA interstitial
     * rather than be refused outright.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_app_password_block_is_not_registered_on_the_shared_authenticate_filter(): void
    {
        $module = new Site2faModule($this->policyRequiringAdmins(), $this->makeProviders());
        $module->install();

        foreach ($this->registered as $entry) {
            if ($entry['hook'] !== 'authenticate') {
                continue;
            }
            $callbackName = is_array($entry['callback']) ? (string) ($entry['callback'][1] ?? '') : '';
            $this->assertNotSame(
                'blockAppPasswordFor2faUser',
                $callbackName,
                'the app-password block must not sit on `authenticate`: that filter also runs for'
                . ' ordinary wp-login.php form logins'
            );
        }
    }

    /**
     * FIRES: dispatch the recorded callback with core's own argument list and
     * assert the credential is refused for a user whose role requires 2FA.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_dispatching_the_core_hook_blocks_a_2fa_required_user(): void
    {
        $module = new Site2faModule($this->policyRequiringAdmins(), $this->makeProviders());
        $module->install();

        $registration = $this->findRegistration(self::APP_PASSWORD_HOOK);
        $this->assertNotNull(
            $registration,
            '#523: nothing is registered on ' . self::APP_PASSWORD_HOOK . ', so the control never runs'
        );

        // Exactly what wp_authenticate_application_password() passes.
        $error = new \WP_Error();
        $user  = $this->makeUser(1, ['administrator']);
        $item  = ['uuid' => '11111111-2222-3333-4444-555555555555', 'name' => 'ci'];

        $this->fireAsCore($registration, [$error, $user, $item, 'abcd efgh ijkl mnop']);

        $this->assertSame(
            'wpmgr_2fa_app_password_blocked',
            $error->get_error_code(),
            '#523: firing the core hook must populate the WP_Error core inspects,'
            . ' which is how core abandons the application-password authentication'
        );
        $this->assertNotSame('', $error->get_error_message(), 'the refusal must carry a message for the caller');
    }

    /**
     * FIRES: a user with TOTP deliberately enrolled is refused even though the
     * policy does not require 2FA for their role.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_dispatching_the_core_hook_blocks_a_user_with_totp_enrolled(): void
    {
        $userId = 3;
        Functions\when('get_user_meta')->alias(function ($uid, $key, $single = false) use ($userId) {
            if ($uid === $userId && $key === TotpProvider::META_SECRET) {
                return base64_encode('FAKEBASE32SECRET');
            }
            return '';
        });

        $module = new Site2faModule($this->policyRequiringAdmins(), $this->makeProviders());
        $module->install();

        $registration = $this->findRegistration(self::APP_PASSWORD_HOOK);
        $this->assertNotNull($registration);

        $error = new \WP_Error();
        $this->fireAsCore($registration, [$error, $this->makeUser($userId, ['subscriber']), [], 'app-pw']);

        $this->assertSame(
            'wpmgr_2fa_app_password_blocked',
            $error->get_error_code(),
            'a subscriber with TOTP enrolled must be refused an application password'
        );
    }

    // -------------------------------------------------------------------------
    // OVER-FIRE: application passwords must keep working for everyone else
    // -------------------------------------------------------------------------

    /**
     * DOES NOT OVER-FIRE: a user who is neither role-required nor enrolled must
     * keep authenticating with an application password. A block that refused
     * everybody would break every integration on the site silently -- core
     * returns the WP_Error we populate, and nothing else reports it.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_dispatching_the_core_hook_leaves_a_non_2fa_user_authenticated(): void
    {
        $module = new Site2faModule($this->policyRequiringAdmins(), $this->makeProviders());
        $module->install();

        $registration = $this->findRegistration(self::APP_PASSWORD_HOOK);
        $this->assertNotNull(
            $registration,
            'the callback must be registered, or this test passes for the wrong reason'
        );

        $error = new \WP_Error();
        // Subscriber: not role-required, nothing enrolled. EmailCodeProvider is
        // "configured" for anyone with an email address and must not count.
        $this->fireAsCore($registration, [$error, $this->makeUser(2, ['subscriber']), [], 'app-pw']);

        $this->assertSame(
            [],
            $error->errors,
            'over-fire: application-password auth must still succeed for a user without 2FA'
        );
    }

    /**
     * DOES NOT OVER-FIRE: the agent's own /wpmgr/v1 REST channel is exempt even
     * for a 2FA-required administrator. Those routes authenticate with an
     * Ed25519 signature at the permission callback.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_dispatching_the_core_hook_exempts_the_agent_rest_channel(): void
    {
        $module = new Site2faModule($this->policyRequiringAdmins(), $this->makeProviders());
        $module->install();

        $registration = $this->findRegistration(self::APP_PASSWORD_HOOK);
        $this->assertNotNull($registration);

        $_SERVER['REQUEST_URI'] = '/wp-json/wpmgr/v1/command/sync_security_policy';

        $error = new \WP_Error();
        $this->fireAsCore($registration, [$error, $this->makeUser(1, ['administrator']), [], 'app-pw']);

        $this->assertSame(
            [],
            $error->errors,
            'over-fire: the agent /wpmgr/v1 channel must never be refused by the app-password block'
        );
    }

    /**
     * DOES NOT OVER-FIRE: with 2FA switched off the callback is not registered
     * at all, so application passwords are untouched on the overwhelming
     * majority of sites.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_app_password_block_is_not_registered_when_two_factor_is_off(): void
    {
        $this->assertRecorderIsLive();

        $module = new Site2faModule(SecurityPolicy::defaults(), $this->makeProviders());
        $module->install();

        $this->assertNull(
            $this->findRegistration(self::APP_PASSWORD_HOOK),
            'with two_factor_enabled=false the app-password block must not be registered'
        );
    }

    // -------------------------------------------------------------------------
    // Recovery constant: install() must register NOTHING
    // -------------------------------------------------------------------------

    /**
     * The WPMGR_DISABLE_SITE_2FA escape hatch is the fleet's way out of a
     * lockout, so its contract is not "install() does not crash" -- it is
     * "install() leaves no hook behind". Asserted against a live recorder so
     * that an empty result means the module registered nothing, not that the
     * instrument was broken.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_disable_constant_makes_install_register_no_hooks(): void
    {
        $this->assertRecorderIsLive();

        if (!defined('WPMGR_DISABLE_SITE_2FA')) {
            define('WPMGR_DISABLE_SITE_2FA', true);
        }
        $this->assertTrue(
            defined('WPMGR_DISABLE_SITE_2FA') && (bool) WPMGR_DISABLE_SITE_2FA,
            'the escape hatch must be defined and true in this process'
        );

        // A policy that would otherwise register every hook the module owns.
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'          => true,
                'two_factor_required_roles'   => ['administrator'],
                'block_xmlrpc_for_2fa_users'  => true,
                'password_max_age_days'       => 30,
            ],
        ]);

        $module = new Site2faModule($policy, $this->makeProviders());
        $module->install();

        $this->assertSame(
            [],
            array_column($this->registered, 'hook'),
            'WPMGR_DISABLE_SITE_2FA must leave install() with zero hook registrations'
        );
    }
}
