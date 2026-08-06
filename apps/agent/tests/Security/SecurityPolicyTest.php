<?php
/**
 * SecurityPolicy value-object tests.
 *
 * Validates:
 *   - fromArray() produces correct values from a full payload
 *   - fromArray() applies safe defaults for missing/invalid fields
 *   - defaults() returns all-OFF policy
 *   - requires2fa() correctly resolves role membership (site-level + group)
 *   - allowedMethodsFor() returns the correct intersection
 *   - effectiveMinZxcvbnScore() resolves group overrides
 *   - effectiveMaxAgeDays() resolves the strictest group override
 *   - hide_backend_slug validation (valid/invalid/reserved)
 *   - coerceGroups drops bad entries
 *   - toArray() round-trips through fromArray()
 *   - SAFETY: defaults() challenges nobody (two_factor_enabled=false)
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\SecurityPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\SecurityPolicy
 */
final class SecurityPolicyTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('get_option')->justReturn('');
        Functions\when('esc_url_raw')->alias(fn ($u) => $u);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // defaults()
    // -------------------------------------------------------------------------

    public function test_defaults_produces_all_off_policy(): void
    {
        $policy = SecurityPolicy::defaults();

        $this->assertFalse($policy->twoFactorEnabled, 'default: 2FA off');
        $this->assertSame(0, $policy->passwordMinZxcvbnScore, 'default: no strength requirement');
        $this->assertFalse($policy->passwordBlockCompromised, 'default: no HIBP check');
        $this->assertSame(0, $policy->passwordReuseBlockCount, 'default: no reuse block');
        $this->assertSame(0, $policy->passwordMaxAgeDays, 'default: no expiry');
        $this->assertFalse($policy->hideBackendEnabled, 'default: hide-backend off');
        $this->assertSame([], $policy->twoFactorRequiredRoles, 'default: no required roles');
        $this->assertSame([], $policy->groups, 'default: no groups');
        $this->assertSame([], $policy->forcePasswordChange, 'default: no force-list');
    }

    // -------------------------------------------------------------------------
    // fromArray() — full valid payload
    // -------------------------------------------------------------------------

    public function test_from_array_full_valid_payload(): void
    {
        $raw = [
            'policy' => [
                'two_factor_enabled'              => true,
                'two_factor_methods'              => ['totp', 'backup'],
                'two_factor_required_roles'       => ['administrator', 'editor'],
                'two_factor_grace_logins'         => 5,
                'two_factor_remember_device_days' => 14,
                'block_xmlrpc_for_2fa_users'      => false,
                'password_min_zxcvbn_score'       => 3,
                'password_min_zxcvbn_roles'       => ['administrator'],
                'password_block_compromised'      => true,
                'password_reuse_block_count'      => 5,
                'password_max_age_days'           => 90,
                'password_expiry_roles'           => ['administrator'],
                'hide_backend_enabled'            => true,
                'hide_backend_slug'               => 'my-login-page',
                'hide_backend_redirect'           => 'https://example.com',
            ],
            'groups' => [
                [
                    'role'              => 'administrator',
                    'require_2fa'       => true,
                    'allowed_methods'   => ['totp', 'backup'],
                    'min_zxcvbn_score'  => 4,
                    'block_compromised' => true,
                    'max_age_days'      => 60,
                ],
            ],
            'force_password_change' => [
                ['user_login' => 'john', 'reason' => 'admin_reset'],
            ],
        ];

        $policy = SecurityPolicy::fromArray($raw);

        $this->assertTrue($policy->twoFactorEnabled);
        $this->assertSame(['totp', 'backup'], $policy->twoFactorMethods);
        $this->assertSame(['administrator', 'editor'], $policy->twoFactorRequiredRoles);
        $this->assertSame(5, $policy->twoFactorGraceLogins);
        $this->assertSame(14, $policy->twoFactorRememberDeviceDays);
        $this->assertFalse($policy->blockXmlrpcFor2faUsers);
        $this->assertSame(3, $policy->passwordMinZxcvbnScore);
        $this->assertSame(['administrator'], $policy->passwordMinZxcvbnRoles);
        $this->assertTrue($policy->passwordBlockCompromised);
        $this->assertSame(5, $policy->passwordReuseBlockCount);
        $this->assertSame(90, $policy->passwordMaxAgeDays);
        $this->assertTrue($policy->hideBackendEnabled);
        $this->assertSame('my-login-page', $policy->hideBackendSlug);
        $this->assertCount(1, $policy->groups);
        $this->assertSame('administrator', $policy->groups[0]['role']);
        $this->assertSame(4, $policy->groups[0]['min_zxcvbn_score']);
        $this->assertCount(1, $policy->forcePasswordChange);
        $this->assertSame('john', $policy->forcePasswordChange[0]['user_login']);
        $this->assertSame('admin_reset', $policy->forcePasswordChange[0]['reason']);
    }

    // -------------------------------------------------------------------------
    // fromArray() — missing/invalid fields
    // -------------------------------------------------------------------------

    public function test_from_array_empty_payload_produces_defaults(): void
    {
        $policy = SecurityPolicy::fromArray([]);
        $this->assertFalse($policy->twoFactorEnabled);
        $this->assertSame(0, $policy->passwordMinZxcvbnScore);
        $this->assertFalse($policy->hideBackendEnabled);
    }

    public function test_from_array_invalid_zxcvbn_score_clamped(): void
    {
        $p = SecurityPolicy::fromArray(['policy' => ['password_min_zxcvbn_score' => 99]]);
        $this->assertSame(4, $p->passwordMinZxcvbnScore);

        $p2 = SecurityPolicy::fromArray(['policy' => ['password_min_zxcvbn_score' => -5]]);
        $this->assertSame(0, $p2->passwordMinZxcvbnScore);
    }

    public function test_from_array_invalid_hide_backend_slug_is_rejected(): void
    {
        // Too short.
        $p = SecurityPolicy::fromArray(['policy' => ['hide_backend_enabled' => true, 'hide_backend_slug' => 'ab']]);
        $this->assertFalse($p->hideBackendEnabled, 'hide-backend disabled when slug is invalid');

        // Reserved path.
        $p2 = SecurityPolicy::fromArray(['policy' => ['hide_backend_enabled' => true, 'hide_backend_slug' => 'wp-login']]);
        $this->assertFalse($p2->hideBackendEnabled, 'hide-backend disabled for reserved slug');

        // Invalid characters.
        $p3 = SecurityPolicy::fromArray(['policy' => ['hide_backend_enabled' => true, 'hide_backend_slug' => 'MY SLUG']]);
        $this->assertFalse($p3->hideBackendEnabled);
    }

    public function test_from_array_valid_hide_backend_slug(): void
    {
        $p = SecurityPolicy::fromArray(['policy' => ['hide_backend_enabled' => true, 'hide_backend_slug' => 'my-secret-login']]);
        $this->assertTrue($p->hideBackendEnabled);
        $this->assertSame('my-secret-login', $p->hideBackendSlug);
    }

    public function test_from_array_control_chars_in_role_are_dropped(): void
    {
        $p = SecurityPolicy::fromArray(['policy' => ['two_factor_required_roles' => ["administ\x00rator", 'editor']]]);
        $this->assertSame(['editor'], $p->twoFactorRequiredRoles);
    }

    public function test_from_array_invalid_groups_dropped(): void
    {
        $p = SecurityPolicy::fromArray([
            'groups' => [
                ['role' => ''],                           // empty role
                ['role' => "admin\nstrator"],             // control char
                ['role' => str_repeat('x', 65)],         // too long
                ['role' => 'administrator', 'require_2fa' => true],  // valid
            ],
        ]);
        $this->assertCount(1, $p->groups);
        $this->assertSame('administrator', $p->groups[0]['role']);
    }

    public function test_from_array_force_list_drops_empty_logins(): void
    {
        $p = SecurityPolicy::fromArray([
            'force_password_change' => [
                ['user_login' => '', 'reason' => 'reset'],
                ['user_login' => 'jane', 'reason' => 'expiry'],
            ],
        ]);
        $this->assertCount(1, $p->forcePasswordChange);
        $this->assertSame('jane', $p->forcePasswordChange[0]['user_login']);
    }

    // -------------------------------------------------------------------------
    // toArray() round-trip
    // -------------------------------------------------------------------------

    public function test_to_array_round_trips(): void
    {
        $raw = [
            'policy' => [
                'two_factor_enabled'              => true,
                'two_factor_methods'              => ['totp'],
                'two_factor_required_roles'       => ['administrator'],
                'two_factor_grace_logins'         => 2,
                'two_factor_remember_device_days' => 7,
                'block_xmlrpc_for_2fa_users'      => true,
                'password_min_zxcvbn_score'       => 2,
                'password_min_zxcvbn_roles'       => [],
                'password_block_compromised'      => false,
                'password_reuse_block_count'      => 3,
                'password_max_age_days'           => 180,
                'password_expiry_roles'           => [],
                'hide_backend_enabled'            => false,
                'hide_backend_slug'               => '',
                'hide_backend_redirect'           => '',
            ],
            'groups'               => [],
            'force_password_change' => [],
        ];

        $policy = SecurityPolicy::fromArray($raw);
        $rebuilt = SecurityPolicy::fromArray($policy->toArray());

        $this->assertTrue($rebuilt->twoFactorEnabled);
        $this->assertSame(['totp'], $rebuilt->twoFactorMethods);
        $this->assertSame(['administrator'], $rebuilt->twoFactorRequiredRoles);
        $this->assertSame(3, $rebuilt->passwordReuseBlockCount);
    }

    // -------------------------------------------------------------------------
    // requires2fa() — role-based resolution
    // -------------------------------------------------------------------------

    private function makeUser(int $id = 1, array $roles = []): \WP_User
    {
        $u         = new \WP_User();
        $u->ID     = $id;
        $u->roles  = $roles;
        return $u;
    }

    public function test_requires2fa_false_when_2fa_disabled(): void
    {
        $policy = SecurityPolicy::fromArray(['policy' => ['two_factor_enabled' => false]]);
        $user   = $this->makeUser(1, ['administrator']);
        $this->assertFalse($policy->requires2fa($user));
    }

    public function test_requires2fa_true_when_role_is_in_required_roles(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);
        $admin  = $this->makeUser(1, ['administrator']);
        $editor = $this->makeUser(2, ['editor']);

        $this->assertTrue($policy->requires2fa($admin));
        $this->assertFalse($policy->requires2fa($editor), 'editor is not in required roles');
    }

    public function test_requires2fa_true_when_group_requires_it(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => ['two_factor_enabled' => true, 'two_factor_required_roles' => []],
            'groups' => [['role' => 'editor', 'require_2fa' => true]],
        ]);
        $editor = $this->makeUser(2, ['editor']);
        $this->assertTrue($policy->requires2fa($editor));
    }

    public function test_requires2fa_false_for_non_matching_group(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => ['two_factor_enabled' => true, 'two_factor_required_roles' => []],
            'groups' => [['role' => 'administrator', 'require_2fa' => true]],
        ]);
        $subscriber = $this->makeUser(3, ['subscriber']);
        $this->assertFalse($policy->requires2fa($subscriber));
    }

    // -------------------------------------------------------------------------
    // effectiveMinZxcvbnScore() — group override
    // -------------------------------------------------------------------------

    public function test_effective_min_zxcvbn_score_group_override(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'password_min_zxcvbn_score' => 2,
                'password_min_zxcvbn_roles' => [],
            ],
            'groups' => [['role' => 'administrator', 'min_zxcvbn_score' => 4]],
        ]);
        $admin  = $this->makeUser(1, ['administrator']);
        $editor = $this->makeUser(2, ['editor']);

        $this->assertSame(4, $policy->effectiveMinZxcvbnScore($admin), 'group overrides to 4');
        $this->assertSame(2, $policy->effectiveMinZxcvbnScore($editor), 'editor uses site-level 2');
    }

    // -------------------------------------------------------------------------
    // effectiveMaxAgeDays() — shortest-rotation wins
    // -------------------------------------------------------------------------

    public function test_effective_max_age_days_shortest_wins(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'password_max_age_days' => 180,
                'password_expiry_roles' => [],
            ],
            'groups' => [['role' => 'administrator', 'max_age_days' => 60]],
        ]);
        $admin  = $this->makeUser(1, ['administrator']);
        $editor = $this->makeUser(2, ['editor']);

        $this->assertSame(60, $policy->effectiveMaxAgeDays($admin), 'group 60 < site 180');
        $this->assertSame(180, $policy->effectiveMaxAgeDays($editor), 'editor uses site-level');
    }

    // -------------------------------------------------------------------------
    // GH #350 — enforcement against CUSTOM role slugs is unchanged
    //
    // Role discovery was the gap, not enforcement: the agent has always matched
    // a policy against whatever slugs a user really holds. These pin that the
    // discovery work did not narrow it back to the five default roles.
    // -------------------------------------------------------------------------

    public function test_custom_role_slug_is_gated_by_the_site_level_strength_rule(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'password_min_zxcvbn_score' => 3,
                'password_min_zxcvbn_roles' => ['shop_manager'],
            ],
        ]);
        $shopManager = $this->makeUser(1, ['shop_manager']);
        $subscriber  = $this->makeUser(2, ['subscriber']);

        $this->assertSame(3, $policy->effectiveMinZxcvbnScore($shopManager), 'a named custom role is gated');
        $this->assertSame(0, $policy->effectiveMinZxcvbnScore($subscriber), 'an unnamed role is not');
    }

    public function test_custom_role_slug_is_gated_by_expiry_and_2fa_and_group_overrides(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['shop_manager'],
                'password_max_age_days'     => 90,
                'password_expiry_roles'     => ['shop_manager'],
            ],
            'groups' => [
                ['role' => 'membership_tier_3', 'min_zxcvbn_score' => 4, 'block_compromised' => true],
            ],
        ]);
        $shopManager = $this->makeUser(1, ['shop_manager']);
        $tier3       = $this->makeUser(2, ['membership_tier_3']);
        $editor      = $this->makeUser(3, ['editor']);

        $this->assertTrue($policy->requires2fa($shopManager));
        $this->assertSame(90, $policy->effectiveMaxAgeDays($shopManager));
        $this->assertSame(4, $policy->effectiveMinZxcvbnScore($tier3), 'per-role group overrides use the same slug vocabulary');
        $this->assertTrue($policy->blockCompromisedFor($tier3));
        $this->assertFalse($policy->requires2fa($editor));
        $this->assertSame(0, $policy->effectiveMaxAgeDays($editor));
    }

    public function test_a_policy_naming_a_role_the_site_no_longer_has_round_trips_and_gates_nobody(): void
    {
        // T3, agent half: an operator can keep a stale role in the stored policy
        // (the plugin that created it was deactivated) without it being dropped
        // on the way through, and without it silently gating anyone else.
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'password_min_zxcvbn_score' => 3,
                'password_min_zxcvbn_roles' => ['deactivated_plugin_role'],
            ],
        ]);

        $stored = $policy->toArray();
        $this->assertSame(
            ['deactivated_plugin_role'],
            $stored['policy']['password_min_zxcvbn_roles'],
            'the slug survives the round trip so the operator can still see and remove it'
        );

        $admin = $this->makeUser(1, ['administrator']);
        $this->assertSame(0, $policy->effectiveMinZxcvbnScore($admin), 'a role nobody holds gates nobody');
    }

    // -------------------------------------------------------------------------
    // SAFETY: default policy challenges nobody
    // -------------------------------------------------------------------------

    public function test_safety_default_policy_challenges_nobody(): void
    {
        $policy = SecurityPolicy::defaults();
        $admin  = $this->makeUser(1, ['administrator']);

        $this->assertFalse($policy->requires2fa($admin), 'SAFETY: defaults must never require 2FA');
        $this->assertSame(0, $policy->effectiveMinZxcvbnScore($admin), 'SAFETY: no strength requirement by default');
        $this->assertSame(0, $policy->effectiveMaxAgeDays($admin), 'SAFETY: no expiry by default');
    }
}
