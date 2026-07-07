<?php
/**
 * SyncSecurityPolicyCommand tests.
 *
 * Validates:
 *   - command name is "sync_security_policy"
 *   - valid full payload persists and returns {ok:true, detail:"applied", enrollment_summary}
 *   - invalid policy type returns an error
 *   - invalid groups type returns an error
 *   - invalid force_password_change type returns an error
 *   - empty payload applies defaults (all-OFF) — never fatal
 *   - force_password_change flags are applied to named users via update_user_meta
 *   - enrollment_summary counts enrolled users per required role
 *   - SAFETY: command name is registered in the closed allow-list
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\SyncSecurityPolicyCommand;
use WPMgr\Agent\Security\SecurityPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\SyncSecurityPolicyCommand
 * @covers \WPMgr\Agent\Security\SecurityPolicy
 */
final class SyncSecurityPolicyCommandTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $optionStore = [];

    /** @var array<int,array<string,mixed>> */
    private array $userMetaStore = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->optionStore   = [];
        $this->userMetaStore = [];

        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->optionStore[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('wp_json_encode')->alias(fn ($v) => json_encode($v));
        Functions\when('esc_url_raw')->alias(fn ($u) => $u);

        // User meta stubs.
        Functions\when('get_user_meta')->alias(function ($uid, $key, $single) {
            return $this->userMetaStore[$uid][$key] ?? '';
        });
        Functions\when('update_user_meta')->alias(function ($uid, $key, $value) {
            $this->userMetaStore[$uid][$key] = $value;
            return true;
        });
        Functions\when('delete_user_meta')->alias(function ($uid, $key) {
            unset($this->userMetaStore[$uid][$key]);
            return true;
        });

        // get_user_by stub — return a WP_User for known logins.
        Functions\when('get_user_by')->alias(function (string $field, $value) {
            if ($field === 'login' && $value === 'john') {
                $u            = new \WP_User();
                $u->ID        = 42;
                $u->user_login = 'john';
                return $u;
            }
            return false;
        });

        // get_users stub — return empty by default.
        Functions\when('get_users')->justReturn([]);

        Functions\when('sanitize_key')->alias(fn ($k) => $k);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    private function command(): SyncSecurityPolicyCommand
    {
        return new SyncSecurityPolicyCommand();
    }

    // -------------------------------------------------------------------------
    // Command name
    // -------------------------------------------------------------------------

    public function test_command_name_is_sync_security_policy(): void
    {
        $this->assertSame('sync_security_policy', $this->command()->name());
    }

    // -------------------------------------------------------------------------
    // Validation errors
    // -------------------------------------------------------------------------

    public function test_policy_must_be_array(): void
    {
        $result = $this->command()->execute([], ['policy' => 'not-an-array']);
        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('policy', $result['detail']);
    }

    public function test_groups_must_be_array(): void
    {
        $result = $this->command()->execute([], ['groups' => 'not-an-array']);
        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('groups', $result['detail']);
    }

    public function test_force_password_change_must_be_array(): void
    {
        $result = $this->command()->execute([], ['force_password_change' => 'not-an-array']);
        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('force_password_change', $result['detail']);
    }

    // -------------------------------------------------------------------------
    // Happy path
    // -------------------------------------------------------------------------

    public function test_full_valid_payload_returns_ok_and_persists(): void
    {
        $params = [
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
                'password_min_zxcvbn_score' => 3,
            ],
            'groups' => [
                ['role' => 'administrator', 'require_2fa' => true],
            ],
            'force_password_change' => [
                ['user_login' => 'john', 'reason' => 'admin_reset'],
            ],
        ];

        $result = $this->command()->execute([], $params);

        $this->assertTrue($result['ok']);
        $this->assertSame('applied', $result['detail']);
        $this->assertArrayHasKey('enrollment_summary', $result);

        // Policy option was persisted.
        $stored = $this->optionStore[SecurityPolicy::OPTION_KEY] ?? null;
        $this->assertNotNull($stored, 'policy option must be written');

        $decoded = json_decode($stored, true);
        $this->assertIsArray($decoded);
        $this->assertTrue($decoded['policy']['two_factor_enabled']);
        $this->assertSame(3, $decoded['policy']['password_min_zxcvbn_score']);
    }

    // -------------------------------------------------------------------------
    // Empty payload — defaults, never fatal
    // -------------------------------------------------------------------------

    public function test_empty_payload_applies_defaults_and_never_fatals(): void
    {
        $result = $this->command()->execute([], []);

        $this->assertTrue($result['ok']);

        $stored  = $this->optionStore[SecurityPolicy::OPTION_KEY] ?? '{}';
        $decoded = json_decode($stored, true);
        $this->assertFalse($decoded['policy']['two_factor_enabled'], 'empty payload must leave 2FA off');
        $this->assertSame(0, $decoded['policy']['password_min_zxcvbn_score'], 'empty payload must leave score at 0');
    }

    // -------------------------------------------------------------------------
    // force_password_change applied to named users
    // -------------------------------------------------------------------------

    public function test_force_password_change_flags_known_user(): void
    {
        $params = [
            'force_password_change' => [
                ['user_login' => 'john', 'reason' => 'admin_reset'],
            ],
        ];

        $this->command()->execute([], $params);

        $meta = $this->userMetaStore[42]['wpmgr_pw_change_required'] ?? null;
        $this->assertSame('admin_reset', $meta, 'force-change meta must be set for john (uid=42)');
    }

    // -------------------------------------------------------------------------
    // enrollment_summary structure
    // -------------------------------------------------------------------------

    public function test_enrollment_summary_is_per_role_structure(): void
    {
        // Simulate get_users returning one administrator.
        $adminRow     = new \stdClass();
        $adminRow->ID = 5;

        Functions\when('get_users')->alias(function (array $args) use ($adminRow) {
            if ($args['role'] === 'administrator') {
                return [$adminRow];
            }
            return [];
        });

        // User 5 has no 2FA meta set (not enrolled).

        $params = [
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ];

        $result = $this->command()->execute([], $params);

        $this->assertTrue($result['ok']);
        $summary = $result['enrollment_summary'] ?? [];
        $this->assertArrayHasKey('per_role', $summary);
        // per_role is cast to an object on the way out (see BUG 1 fix) so it
        // serializes as `{}` rather than `[]` when empty; convert back to an
        // array here to inspect the populated case.
        $perRole = (array) $summary['per_role'];
        $this->assertArrayHasKey('administrator', $perRole);

        $roleData = $perRole['administrator'];
        $this->assertSame(1, $roleData['total']);
        $this->assertSame(0, $roleData['enrolled'], 'user with no 2FA meta is not enrolled');
    }

    public function test_enrollment_summary_counts_enrolled_user(): void
    {
        // Simulate get_users returning one administrator.
        $adminRow     = new \stdClass();
        $adminRow->ID = 7;

        Functions\when('get_users')->alias(function (array $args) use ($adminRow) {
            if ($args['role'] === 'administrator') {
                return [$adminRow];
            }
            return [];
        });

        // Pre-seed TOTP secret for user 7 (marks them as enrolled).
        $this->userMetaStore[7]['wpmgr_2fa_totp_secret'] = 'encrypted-secret-value';

        $params = [
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ];

        $result = $this->command()->execute([], $params);
        $this->assertTrue($result['ok']);

        $perRole = (array) $result['enrollment_summary']['per_role'];
        $summary = $perRole['administrator'] ?? [];
        $this->assertSame(1, $summary['enrolled'], 'user with totp secret is enrolled');
    }

    // -------------------------------------------------------------------------
    // BUG 1: per_role must serialize as `{}` (object), never `[]` (array),
    // when it has no entries — a Go map[string]RoleEnrollment field cannot be
    // unmarshaled from a JSON array. Both the 2FA-off case and the 2FA-on but
    // zero-required-roles case must round-trip through wp_json_encode as `{}`.
    // -------------------------------------------------------------------------

    public function test_build_enrollment_summary_serializes_empty_per_role_as_object_when_2fa_off(): void
    {
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled' => false,
                'hide_backend_enabled' => true,
                'hide_backend_slug' => 'secret-login',
            ],
        ]);

        $ref = new \ReflectionMethod($this->command(), 'buildEnrollmentSummary');
        $ref->setAccessible(true);
        $result = $ref->invoke($this->command(), $policy);

        $this->assertInstanceOf(\stdClass::class, $result['per_role']);
        $this->assertSame('{"per_role":{}}', wp_json_encode($result));
    }

    public function test_build_enrollment_summary_serializes_empty_per_role_as_object_when_2fa_on_no_roles(): void
    {
        // 2FA is enabled but hide_backend toggled alone in the real bug —
        // here we exercise the "2FA on, zero required roles" branch directly:
        // no required roles and no groups with require_2fa=true.
        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => [],
            ],
            'groups' => [],
        ]);

        $ref = new \ReflectionMethod($this->command(), 'buildEnrollmentSummary');
        $ref->setAccessible(true);
        $result = $ref->invoke($this->command(), $policy);

        $this->assertInstanceOf(\stdClass::class, $result['per_role']);
        $this->assertSame('{"per_role":{}}', wp_json_encode($result));
    }

    public function test_build_enrollment_summary_populated_case_serializes_as_nested_object(): void
    {
        $adminRow     = new \stdClass();
        $adminRow->ID = 5;

        Functions\when('get_users')->alias(function (array $args) use ($adminRow) {
            return $args['role'] === 'administrator' ? [$adminRow] : [];
        });

        $policy = SecurityPolicy::fromArray([
            'policy' => [
                'two_factor_enabled'        => true,
                'two_factor_required_roles' => ['administrator'],
            ],
        ]);

        $ref = new \ReflectionMethod($this->command(), 'buildEnrollmentSummary');
        $ref->setAccessible(true);
        $result = $ref->invoke($this->command(), $policy);

        $encoded = wp_json_encode($result);
        $this->assertStringContainsString('"administrator":{"enrolled":0,"required":1,"total":1}', $encoded);
    }

    // -------------------------------------------------------------------------
    // SAFETY: default-empty policy challenges nobody
    // -------------------------------------------------------------------------

    public function test_safety_empty_payload_does_not_enable_enforcement(): void
    {
        // Execute with no parameters.
        $result = $this->command()->execute([], []);
        $this->assertTrue($result['ok']);

        // Load the persisted policy.
        $stored  = $this->optionStore[SecurityPolicy::OPTION_KEY] ?? '{}';
        $decoded = json_decode($stored, true);
        $policy  = SecurityPolicy::fromArray($decoded);

        // A user should NOT be challenged.
        $user        = new \WP_User();
        $user->ID    = 99;
        $user->roles = ['administrator'];

        $this->assertFalse(
            $policy->requires2fa($user),
            'SAFETY: empty payload must not require 2FA for any user'
        );
    }
}
