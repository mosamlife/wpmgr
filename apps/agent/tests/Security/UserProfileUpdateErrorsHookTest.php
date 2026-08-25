<?php
/**
 * GH #521 regression: callbacks on `user_profile_update_errors` must accept the
 * object WordPress core ACTUALLY passes, and must not lose role scoping doing it.
 *
 * WHY THIS FILE EXISTS SEPARATELY FROM PasswordPolicyModuleTest
 * -------------------------------------------------------------
 * The existing suite is the reason this shipped. Its makeUser() helper returns
 * a \WP_User, so every test handed the chain exactly the type the chain
 * declared, and not one test entered through validateOnProfileUpdate(). The
 * fatal lives at the hook boundary, one level ABOVE where that suite starts.
 *
 * Core does not pass a WP_User. wp-admin/includes/user.php builds a plain
 * stdClass, gives it an ID only on update, and only sometimes gives it a role:
 *
 *     $user = new stdClass();
 *     if ( $user_id ) { $user->ID = $user_id; ... }
 *     ...
 *     $user->role = $new_role;   // only when the actor can promote_users
 *     do_action_ref_array( 'user_profile_update_errors', array( &$errors, $update, &$user ) );
 *
 * Every test below therefore constructs that object — a bare stdClass carrying
 * only the properties core sets — and never a \WP_User, except where a
 * legitimate \WP_User is deliberately being proven still to work.
 *
 * Two failure modes are covered, because fixing only the first produces the
 * second:
 *
 *   1. TypeError at argument binding (the reported fatal). Thrown BEFORE any
 *      early return inside the callback body, so no guard in the method can
 *      save it, and it is not caught by validatePassword()'s catch(\Throwable).
 *
 *   2. Silent under-enforcement. The stdClass has no `roles` property, so
 *      merely widening the type hints would make every role-scoped rule read
 *      an empty role list and fall back to the site default — no crash, no
 *      log, no enforcement. The role-scoped tests here fail loudly on that.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\CpUrlProvider;
use WPMgr\Agent\Security\HardeningConfig;
use WPMgr\Agent\Security\HardeningModule;
use WPMgr\Agent\Security\PasswordPolicyModule;
use WPMgr\Agent\Security\ProfileUpdateUser;
use WPMgr\Agent\Security\RequestSigner;
use WPMgr\Agent\Security\SecurityPolicy;
use WPMgr\Agent\Security\Site2faModule;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\ProfileUpdateUser
 * @covers \WPMgr\Agent\Security\PasswordPolicyModule
 * @covers \WPMgr\Agent\Security\HardeningModule
 * @covers \WPMgr\Agent\Security\SecurityPolicy
 */
final class UserProfileUpdateErrorsHookTest extends TestCase
{
    /** A password zxcvbn scores 0 against these user inputs. */
    private const WEAK_PASSWORD = 'password1';

    /** A password zxcvbn scores 4 against these user inputs. */
    private const STRONG_PASSWORD = '7Kq!vZ3m#Xw9Lp2R@nT5';

    /** @var array<int,\WP_User> Fake user store backing get_userdata(). */
    private array $users = [];

    /** @var array<string,mixed> Fake option store. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->users   = [];
        $this->options = [];

        Functions\when('get_userdata')->alias(fn ($id) => $this->users[(int) $id] ?? false);
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->options[$k] ?? $d);
        Functions\when('get_user_meta')->justReturn('');
        Functions\when('update_user_meta')->justReturn(true);
        Functions\when('get_bloginfo')->justReturn('Test Site');
        Functions\when('wp_unslash')->alias(fn ($v) => $v);
        Functions\when('sanitize_text_field')->alias(fn ($v) => $v);
        Functions\when('esc_html')->alias(fn ($t) => $t);
        Functions\when('esc_html__')->alias(fn ($t, $d = '') => $t);
        Functions\when('__')->alias(fn ($t, $d = '') => $t);
        Functions\when('esc_url_raw')->alias(fn ($u) => $u);

        unset($_POST['pass1'], $_POST['nickname']);
    }

    protected function tear_down(): void
    {
        unset($_POST['pass1'], $_POST['nickname']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Fixtures
    // -------------------------------------------------------------------------

    /**
     * The object core actually passes on a user UPDATE: an ID, the stored
     * login, and whatever the form submitted. No `roles`. No `caps`.
     */
    private function coreUpdateObject(int $id = 7): \stdClass
    {
        $u             = new \stdClass();
        $u->ID         = $id;
        $u->user_login = 'admin';
        $u->user_email = 'admin@example.com';
        return $u;
    }

    /**
     * The object core actually passes on a user CREATE: no ID at all — core
     * assigns ID only inside `if ( $user_id )`.
     */
    private function coreCreateObject(): \stdClass
    {
        $u             = new \stdClass();
        $u->user_login = 'newbie';
        $u->user_email = 'newbie@example.com';
        return $u;
    }

    /** Register a stored user for get_userdata() to return. */
    private function storeUser(int $id, array $roles, string $login = 'admin', string $email = 'admin@example.com'): \WP_User
    {
        $u               = new \WP_User();
        $u->ID           = $id;
        $u->roles        = $roles;
        $u->user_login   = $login;
        $u->user_email   = $email;
        $u->display_name = 'Admin';
        $this->users[$id] = $u;
        return $u;
    }

    /**
     * A policy whose strength rule applies ONLY to administrators. A fix that
     * loses role resolution reads an empty role list, concludes the rule does
     * not apply, and enforces nothing — which these tests catch.
     */
    private function adminScopedStrengthPolicy(): SecurityPolicy
    {
        return SecurityPolicy::fromArray([
            'policy' => [
                'password_min_zxcvbn_score' => 4,
                'password_min_zxcvbn_roles' => ['administrator'],
            ],
        ]);
    }

    private function passwordModule(SecurityPolicy $policy): PasswordPolicyModule
    {
        $settings = new class implements CpUrlProvider {
            public function controlPlaneUrl(): string
            {
                return '';
            }
        };
        $signer = new class implements RequestSigner {
            /** @return array<string,string> */
            public function signHeaders(string $method, string $path, string $body): array
            {
                return [];
            }
        };
        return new PasswordPolicyModule($policy, $settings, $signer);
    }

    // =========================================================================
    // Site A — PasswordPolicyModule::validateOnProfileUpdate
    // =========================================================================

    /**
     * The reported fatal. Before the fix this dies with
     *   TypeError: ...validateOnProfileUpdate(): Argument #3 ($user) must be of
     *   type WP_User, stdClass given
     * at argument binding, on every profile.php / user-edit.php save.
     *
     * It also pins Trap 1: the administrator-scoped rule must still fire, which
     * it cannot do if the fix simply widens the hint and reads roles off the
     * role-less stdClass.
     */
    public function test_profile_update_on_update_enforces_role_scoped_rule(): void
    {
        $this->storeUser(7, ['administrator']);
        $_POST['pass1'] = self::WEAK_PASSWORD;

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, true, $this->coreUpdateObject(7));

        $this->assertArrayHasKey(
            'wpmgr_password_strength',
            $errors->errors,
            'an administrator-scoped strength rule must reject a weak password when core passes its stdClass'
        );
    }

    /**
     * The create path. Core assigns no ID here at all, and puts the submitted
     * role on the object. The rule must be evaluated against the role the new
     * account is about to receive.
     */
    public function test_profile_update_on_create_uses_submitted_role(): void
    {
        $_POST['pass1'] = self::WEAK_PASSWORD;

        $raw       = $this->coreCreateObject();
        $raw->role = 'administrator';

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, false, $raw);

        $this->assertArrayHasKey(
            'wpmgr_password_strength',
            $errors->errors,
            'on create, the submitted role must drive the role-scoped rule'
        );
        $this->assertObjectNotHasProperty(
            'ID',
            $raw,
            'guard against this fixture drifting: core assigns no ID on the create path'
        );
    }

    /**
     * Create path where the actor could not promote_users, so core set no role.
     * wp_insert_user() will apply the site default_role, so that is the role
     * the rule must be evaluated against — never an empty list.
     */
    public function test_profile_update_on_create_without_role_uses_site_default_role(): void
    {
        $_POST['pass1']                = self::WEAK_PASSWORD;
        $this->options['default_role'] = 'administrator';

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, false, $this->coreCreateObject());

        $this->assertArrayHasKey(
            'wpmgr_password_strength',
            $errors->errors,
            'with no submitted role, the site default_role is the role the new user will get'
        );
    }

    /**
     * Promotion: the stored role is out of scope but the submitted role is in
     * scope. The union must apply the stricter rule.
     */
    public function test_profile_update_promotion_applies_the_target_role_rule(): void
    {
        $this->storeUser(9, ['subscriber'], 'sub', 'sub@example.com');
        $_POST['pass1'] = self::WEAK_PASSWORD;

        $raw       = $this->coreUpdateObject(9);
        $raw->role = 'administrator';

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, true, $raw);

        $this->assertArrayHasKey(
            'wpmgr_password_strength',
            $errors->errors,
            'promoting a subscriber to administrator must apply the administrator rule to the new password'
        );
    }

    // -------------------------------------------------------------------------
    // Site A — over-fire checks
    // -------------------------------------------------------------------------

    /** A password that satisfies the rule must still be accepted. */
    public function test_profile_update_accepts_a_strong_password(): void
    {
        $this->storeUser(7, ['administrator']);
        $_POST['pass1'] = self::STRONG_PASSWORD;

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, true, $this->coreUpdateObject(7));

        $this->assertSame([], $errors->errors, 'a strong password must not be rejected');
    }

    /**
     * A user whose role is OUT of the rule's scope must not be enforced against.
     * This is the honest case the fix must not block: without it, "resolve a
     * role" could degenerate into "enforce on everybody".
     */
    public function test_profile_update_does_not_enforce_on_out_of_scope_role(): void
    {
        $this->storeUser(8, ['subscriber'], 'sub', 'sub@example.com');
        $_POST['pass1'] = self::WEAK_PASSWORD;

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, true, $this->coreUpdateObject(8));

        $this->assertSame(
            [],
            $errors->errors,
            'a subscriber must not be held to an administrator-scoped rule'
        );
    }

    /** A caller that legitimately passes a real WP_User must keep working. */
    public function test_profile_update_still_accepts_a_real_wp_user(): void
    {
        $user           = $this->storeUser(7, ['administrator']);
        $_POST['pass1'] = self::WEAK_PASSWORD;

        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, true, $user);

        $this->assertArrayHasKey(
            'wpmgr_password_strength',
            $errors->errors,
            'a genuine WP_User must still be evaluated with its own roles'
        );
    }

    /** No password typed is still the cheap early return — no user load at all. */
    public function test_profile_update_without_a_password_adds_no_error(): void
    {
        $mod    = $this->passwordModule($this->adminScopedStrengthPolicy());
        $errors = new \WP_Error();

        $mod->validateOnProfileUpdate($errors, true, $this->coreUpdateObject(7));

        $this->assertSame([], $errors->errors, 'saving a profile without touching the password must add no error');
    }

    // =========================================================================
    // Site B — HardeningModule force_unique_nickname closure
    // =========================================================================

    /**
     * B is an independent toggle behind a different setting, so a site with no
     * password policy at all still hits the identical crash. Capture the
     * closure install() registers and invoke it with core's stdClass.
     *
     * Every caller of this helper runs in its own process. Site2faModuleTest
     * defines WPMGR_DISABLE_SITE_2FA process-globally, and a constant cannot be
     * undefined; applyForceUniqueNickname() now honours that constant, so in a
     * whole-suite run these tests would register no closure, assert nothing and
     * pass green — a regression test for a production fatal that silently stops
     * running is worth less than no test. Verified: without isolation this
     * helper returns null under `phpunit tests/Security/`, and the three
     * callers fail on "null is of type callable".
     */
    private function nicknameClosure(): callable
    {
        $this->assertFalse(
            defined('WPMGR_DISABLE_SITE_2FA'),
            'these tests need the recovery constant unset; process isolation has failed'
        );
        $captured = null;
        Functions\expect('add_action')
            ->once()
            ->with('user_profile_update_errors', \Mockery::type('callable'), 10, 3)
            ->andReturnUsing(function ($hook, $cb, $priority, $args) use (&$captured) {
                $captured = $cb;
                return true;
            });

        // No setAccessible(): it has been a no-op since PHP 8.1 and is
        // deprecated as of 8.5, which phpunit reports as an issue.
        $mod = new HardeningModule();
        $ref = new \ReflectionMethod($mod, 'applyForceUniqueNickname');
        $ref->invoke($mod, HardeningConfig::fromArray(['config' => ['force_unique_nickname' => true]]));

        $this->assertIsCallable($captured, 'force_unique_nickname must register a user_profile_update_errors callback');
        return $captured;
    }

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_force_unique_nickname_accepts_core_stdclass(): void
    {
        $this->storeUser(7, ['administrator']);
        $_POST['nickname'] = 'admin';

        $cb     = $this->nicknameClosure();
        $errors = new \WP_Error();

        $cb($errors, true, $this->coreUpdateObject(7));

        $this->assertArrayHasKey(
            'wpmgr_nickname_conflict',
            $errors->errors,
            'a nickname equal to the login must still be rejected when core passes its stdClass'
        );
    }

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_force_unique_nickname_allows_a_distinct_nickname(): void
    {
        $this->storeUser(7, ['administrator']);
        $_POST['nickname'] = 'Site Owner';

        $cb     = $this->nicknameClosure();
        $errors = new \WP_Error();

        $cb($errors, true, $this->coreUpdateObject(7));

        $this->assertSame([], $errors->errors, 'a nickname unlike the login must be accepted');
    }

    /**
     * Core sets user_login on the stdClass from the stored user, but a caller
     * that omits it must not silently disable the check — the login is
     * recovered from the stored user instead.
     */
    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_force_unique_nickname_recovers_login_when_absent_from_the_object(): void
    {
        $this->storeUser(7, ['administrator'], 'admin');
        $_POST['nickname'] = 'admin';

        $raw     = new \stdClass();
        $raw->ID = 7;

        $cb     = $this->nicknameClosure();
        $errors = new \WP_Error();

        $cb($errors, true, $raw);

        $this->assertArrayHasKey(
            'wpmgr_nickname_conflict',
            $errors->errors,
            'a missing user_login on the hook object must not silently disable the check'
        );
    }

    // =========================================================================
    // The recovery constant must actually release the auth policy
    // =========================================================================

    /**
     * `define('WPMGR_DISABLE_SITE_2FA', true)` is the documented escape hatch
     * for an admin locked out by this plugin's auth policy. Site2faModule and
     * PasswordPolicyModule honoured it; HardeningModule did not — so an
     * operator who set it silenced the password-policy crash and was still
     * refused by force_unique_nickname. An escape hatch that does not release
     * everything it claims to is worse than none.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_releases_the_nickname_rule(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        Functions\expect('add_action')->never();

        $mod = new HardeningModule();
        $ref = new \ReflectionMethod($mod, 'applyForceUniqueNickname');
        $ref->invoke($mod, HardeningConfig::fromArray(['config' => ['force_unique_nickname' => true]]));

        $this->addToAssertionCount(1);
    }

    /**
     * The same hatch must release the login-identifier restriction, the other
     * HardeningModule rule that can keep an administrator out.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_releases_the_login_identifier_restriction(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        Functions\expect('add_action')->never();

        $mod = new HardeningModule();
        $ref = new \ReflectionMethod($mod, 'applyLoginIdentifier');
        $ref->invoke($mod, HardeningConfig::fromArray(['config' => ['restrict_login_identifier' => 'username']]));

        $this->addToAssertionCount(1);
    }

    /**
     * Over-fire guard on the hatch. It is scoped to auth policy on purpose:
     * recovering a login must not silently drop the site's IP bans. If someone
     * later widens authPolicyDisabled() to gate every applier, this reddens.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_recovery_constant_does_not_drop_ip_bans(): void
    {
        define('WPMGR_DISABLE_SITE_2FA', true);

        $captured = null;
        Functions\expect('add_action')
            ->once()
            ->andReturnUsing(function ($hook, $cb, $priority = 10, $args = 1) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $mod    = new HardeningModule();
        $config = HardeningConfig::fromArray([
            'bans' => [['id' => 'b1', 'type' => 'user_agent', 'value' => 'EvilBot']],
        ]);
        $ref = new \ReflectionMethod($mod, 'applyBanFilters');
        $ref->invoke($mod, $config);

        $this->assertIsCallable(
            $captured,
            'the auth-policy recovery constant must not disable IP/user-agent bans'
        );
    }

    // =========================================================================
    // Site C — Site2faModule and the xmlrpc_login_error hook
    // =========================================================================

    /**
     * `xmlrpc_login_error` fires ONLY inside core's `if ( is_wp_error( $user ) )`
     * branch (wp-includes/class-wp-xmlrpc-server.php), so its second argument is
     * always a WP_Error and never a WP_User. A callback hinting \WP_User there
     * could only ever fatal, and its body was `return $error;` — it never
     * blocked anything. It is removed rather than widened.
     */
    public function test_xmlrpc_login_error_callback_is_gone(): void
    {
        $this->assertFalse(
            method_exists(Site2faModule::class, 'blockXmlrpcFor2faUser'),
            'the xmlrpc_login_error callback fires only on already-failed auth; it cannot block anything and must not exist'
        );
    }

    /**
     * The functional XML-RPC block lives on `authenticate`, which core calls
     * with the resolved user. It must survive C's removal, and it must tolerate
     * the WP_Error core hands it when authentication already failed.
     */
    public function test_xmlrpc_authenticate_block_tolerates_a_wp_error(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'         => true,
                'block_xmlrpc_for_2fa_users' => true,
            ],
        ]);
        $mod = new Site2faModule($policy, []);

        $error  = new \WP_Error('incorrect_password', 'nope');
        $result = $mod->interceptXmlrpc2fa($error, 'someone', 'secret');

        $this->assertSame($error, $result, 'a failed authentication must pass straight through untouched');
    }

    // =========================================================================
    // The chain — SecurityPolicy role resolution
    // =========================================================================

    /**
     * Last line of defence for Trap 1. If a future caller bypasses the resolver
     * and hands SecurityPolicy the raw core object, role scoping must still
     * resolve from the submitted role rather than silently emptying out.
     */
    public function test_security_policy_resolves_roles_from_a_bare_core_object(): void
    {
        $policy = $this->adminScopedStrengthPolicy();

        $raw       = new \stdClass();
        $raw->role = 'administrator';

        $this->assertSame(
            4,
            $policy->effectiveMinZxcvbnScore($raw),
            'the submitted role must drive the role-scoped score, not the empty-role default'
        );

        $other       = new \stdClass();
        $other->role = 'subscriber';

        $this->assertSame(
            0,
            $policy->effectiveMinZxcvbnScore($other),
            'an out-of-scope role must not pick up the administrator rule'
        );
    }

    public function test_security_policy_block_compromised_resolves_group_role(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => ['password_block_compromised' => false],
            'groups' => [
                ['role' => 'administrator', 'block_compromised' => true],
            ],
        ]);

        $raw       = new \stdClass();
        $raw->role = 'administrator';

        $this->assertTrue(
            $policy->blockCompromisedFor($raw),
            'a group rule keyed on the submitted role must still apply'
        );
    }

    // =========================================================================
    // The resolver itself
    // =========================================================================

    public function test_resolver_returns_a_real_wp_user_untouched(): void
    {
        $user = $this->storeUser(7, ['administrator']);
        $this->assertSame($user, ProfileUpdateUser::resolve($user));
    }

    /**
     * get_userdata() returns the request-cached WP_User. Writing a unioned role
     * list onto it would corrupt that cache for the rest of the request, so the
     * resolver must always hand back a separate object.
     */
    public function test_resolver_never_mutates_the_cached_user(): void
    {
        $stored = $this->storeUser(9, ['subscriber'], 'sub', 'sub@example.com');

        $raw       = $this->coreUpdateObject(9);
        $raw->role = 'administrator';

        $resolved = ProfileUpdateUser::resolve($raw);

        $this->assertNotSame($stored, $resolved, 'the resolver must not hand back the cached instance');
        $this->assertSame(['subscriber'], $stored->roles, 'the cached user must be left exactly as it was');
        $this->assertSame(['subscriber', 'administrator'], $resolved->roles);
        $this->assertSame(9, $resolved->ID);
    }

    /** Junk on a hook boundary must degrade, never throw. */
    public function test_resolver_tolerates_junk(): void
    {
        foreach ([null, 'a string', 42, [], new \stdClass()] as $junk) {
            $resolved = ProfileUpdateUser::resolve($junk);
            $this->assertInstanceOf(\WP_User::class, $resolved);
            $this->assertSame(0, $resolved->ID);
        }
    }

    /** A stored user that no longer exists must not resurrect an empty role set silently. */
    public function test_resolver_keeps_submitted_role_when_the_stored_user_is_gone(): void
    {
        $raw       = $this->coreUpdateObject(404);
        $raw->role = 'editor';

        $resolved = ProfileUpdateUser::resolve($raw);

        $this->assertSame(['editor'], $resolved->roles);
        $this->assertSame(404, $resolved->ID);
    }
}
