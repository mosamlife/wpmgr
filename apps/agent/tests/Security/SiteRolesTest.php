<?php
/**
 * SiteRoles / role-discovery tests (GH #350).
 *
 * The site security policy is written in ROLE SLUGS and the agent has always
 * enforced it against whatever slugs a user really holds. What was missing was
 * DISCOVERY: the dashboard offered a hardcoded list of the five default
 * WordPress roles, so a WooCommerce shop manager, or any membership-plugin
 * role, could not be given a password policy at all.
 *
 * These tests pin the reporting side of that fix. Every one of them FAILS
 * against the pre-change code, where SiteRoles did not exist and the metadata
 * payload carried no `roles` key at all.
 *
 * Validates:
 *   - T1: a custom role (shop_manager / "Gestore negozio") is reported with
 *         both its slug and its display name
 *   - T2: default role names are reported LOCALIZED (translate_user_role), not
 *         as hardcoded English
 *   - the unfiltered wp_roles() registry is the source, NOT get_editable_roles()
 *     (which plugins filter to hide roles from user-management screens)
 *   - C4: a site with a huge number of roles is bounded, not unbounded
 *   - the metadata READ path carries the roles, so they are known before an
 *     operator saves any policy
 *   - an unreadable registry reports an empty list rather than fataling
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Security\SiteRoles;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\SiteRoles
 */
final class SiteRolesTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Install a fake wp_roles() registry.
     *
     * The fake is a bare anonymous object exposing the two public properties
     * WP_Roles exposes. The collector is duck-typed rather than
     * instanceof-checked, so a site running a replacement roles object still
     * reports; this fake pins that.
     *
     * Self-contained by design: Patchwork redefinitions leak across tests in
     * this suite, so nothing here reaches for shared state.
     *
     * @param array<string,array<string,mixed>> $roles Slug-keyed role map.
     * @return void
     */
    private function fakeRegistry(array $roles): void
    {
        $registry              = new \stdClass();
        $registry->roles       = $roles;
        $registry->role_names  = array_map(
            static fn ($r) => isset($r['name']) ? $r['name'] : '',
            $roles
        );

        Functions\when('wp_roles')->justReturn($registry);
    }

    // -------------------------------------------------------------------------
    // T1: a custom role is reported with slug AND display name
    // -------------------------------------------------------------------------

    public function test_reports_a_custom_role_with_slug_and_display_name(): void
    {
        $this->fakeRegistry([
            'administrator' => ['name' => 'Administrator', 'capabilities' => []],
            'shop_manager'  => ['name' => 'Gestore negozio', 'capabilities' => []],
            'customer'      => ['name' => 'Cliente', 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $roles = SiteRoles::collect();

        $this->assertContains(
            ['slug' => 'shop_manager', 'name' => 'Gestore negozio'],
            $roles,
            'T1: the WooCommerce shop manager must be reported so a policy can name it'
        );
        $this->assertContains(
            ['slug' => 'customer', 'name' => 'Cliente'],
            $roles
        );
        $this->assertCount(3, $roles);
    }

    public function test_role_slug_is_reported_verbatim_not_derived_from_the_name(): void
    {
        // The slug is the ONLY part the policy enforces against. A display name
        // that looks nothing like it must not change what is stored.
        $this->fakeRegistry([
            'shop_manager' => ['name' => 'Gestore negozio', 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $roles = SiteRoles::collect();

        $this->assertSame('shop_manager', $roles[0]['slug']);
    }

    // -------------------------------------------------------------------------
    // T2: default role names are localized, not hardcoded English
    // -------------------------------------------------------------------------

    public function test_default_role_names_are_localized_not_hardcoded_english(): void
    {
        // WordPress stores core role names in English and translates them at
        // display time. An Italian site shows "Amministratore"; an operator who
        // sees that in wp-admin must see the same thing here.
        $this->fakeRegistry([
            'administrator' => ['name' => 'Administrator', 'capabilities' => []],
            'editor'        => ['name' => 'Editor', 'capabilities' => []],
        ]);

        $italian = [
            'Administrator' => 'Amministratore',
            'Editor'        => 'Editore',
        ];
        Functions\when('translate_user_role')->alias(
            static fn ($name) => $italian[$name] ?? $name
        );

        $roles = SiteRoles::collect();

        $this->assertSame(
            [
                ['slug' => 'administrator', 'name' => 'Amministratore'],
                ['slug' => 'editor', 'name' => 'Editore'],
            ],
            $roles,
            'T2: names must come from translate_user_role, the same call core makes for its own role dropdown'
        );
    }

    public function test_translation_that_returns_blank_falls_back_to_the_stored_name(): void
    {
        $this->fakeRegistry([
            'administrator' => ['name' => 'Administrator', 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->justReturn('   ');

        $roles = SiteRoles::collect();

        $this->assertSame('Administrator', $roles[0]['name']);
    }

    // -------------------------------------------------------------------------
    // Source of truth: wp_roles(), NOT get_editable_roles()
    // -------------------------------------------------------------------------

    public function test_uses_the_unfiltered_registry_and_never_calls_get_editable_roles(): void
    {
        // get_editable_roles() applies the `editable_roles` filter, which
        // membership and role-editor plugins use to HIDE roles from the user
        // screens. Those roles still exist and their users still log in, so
        // honouring that filter here would recreate the very gap being fixed.
        $this->fakeRegistry([
            'administrator' => ['name' => 'Administrator', 'capabilities' => []],
            'hidden_staff'  => ['name' => 'Calandrini Staff', 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);
        Functions\expect('get_editable_roles')->never();

        $slugs = array_column(SiteRoles::collect(), 'slug');

        $this->assertContains(
            'hidden_staff',
            $slugs,
            'a role hidden from the user editor still authenticates and must remain selectable'
        );
    }

    public function test_falls_back_to_role_names_when_only_that_property_exists(): void
    {
        $registry             = new \stdClass();
        $registry->role_names = ['shop_manager' => 'Gestore negozio'];
        Functions\when('wp_roles')->justReturn($registry);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $this->assertSame(
            [['slug' => 'shop_manager', 'name' => 'Gestore negozio']],
            SiteRoles::collect()
        );
    }

    // -------------------------------------------------------------------------
    // C4: a site with a huge number of roles stays bounded
    // -------------------------------------------------------------------------

    public function test_a_huge_role_registry_is_capped(): void
    {
        $many = [];
        for ($i = 0; $i < 500; $i++) {
            $many['membership_tier_' . $i] = ['name' => 'Tier ' . $i, 'capabilities' => []];
        }
        $this->fakeRegistry($many);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $roles = SiteRoles::collect();

        $this->assertCount(SiteRoles::MAX_ROLES, $roles, 'C4: the payload must be bounded');
        $this->assertSame('membership_tier_0', $roles[0]['slug'], 'the cap keeps the first roles, it does not reorder');
    }

    public function test_a_very_long_display_name_is_truncated_but_the_role_is_kept(): void
    {
        $this->fakeRegistry([
            'shop_manager' => ['name' => str_repeat('a', 400), 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $roles = SiteRoles::collect();

        $this->assertCount(1, $roles, 'a long NAME is presentational; the role must still be selectable');
        $this->assertSame(SiteRoles::MAX_NAME_LENGTH, strlen($roles[0]['name']));
        $this->assertSame('shop_manager', $roles[0]['slug']);
    }

    public function test_an_absurdly_long_slug_is_dropped_rather_than_truncated(): void
    {
        // A truncated slug matches nothing the agent enforces against, so
        // reporting one would be worse than reporting nothing.
        $this->fakeRegistry([
            str_repeat('x', 300) => ['name' => 'Broken', 'capabilities' => []],
            'shop_manager'       => ['name' => 'Gestore negozio', 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $this->assertSame(
            [['slug' => 'shop_manager', 'name' => 'Gestore negozio']],
            SiteRoles::collect()
        );
    }

    public function test_a_role_with_no_name_falls_back_to_its_slug(): void
    {
        $this->fakeRegistry([
            'orphan_role' => ['capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        $this->assertSame(
            [['slug' => 'orphan_role', 'name' => 'orphan_role']],
            SiteRoles::collect()
        );
    }

    // -------------------------------------------------------------------------
    // Never fatal
    // -------------------------------------------------------------------------

    public function test_an_unreadable_registry_reports_an_empty_list(): void
    {
        Functions\when('wp_roles')->justReturn(null);

        $this->assertSame([], SiteRoles::collect());
    }

    public function test_a_registry_that_throws_reports_an_empty_list(): void
    {
        Functions\when('wp_roles')->alias(static function () {
            throw new \RuntimeException('roles table is gone');
        });

        $this->assertSame([], SiteRoles::collect());
    }

    // -------------------------------------------------------------------------
    // The READ path: metadata carries the roles
    // -------------------------------------------------------------------------

    public function test_metadata_read_path_reports_the_sites_real_roles(): void
    {
        // Metadata is collected on the agent's periodic push AND on the control
        // plane's synchronous re-check pull, so the roles are known BEFORE an
        // operator saves any policy. That ordering is the whole point: a write
        // path alone cannot answer "which roles may I pick" on first load.
        $this->fakeRegistry([
            'administrator' => ['name' => 'Administrator', 'capabilities' => []],
            'shop_manager'  => ['name' => 'Gestore negozio', 'capabilities' => []],
        ]);
        Functions\when('translate_user_role')->alias(static fn ($n) => $n);

        // Everything else MetadataCommand::collect() touches, stubbed flat.
        Functions\when('get_bloginfo')->justReturn('6.8');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_site_transient')->justReturn(false);
        Functions\when('get_core_updates')->justReturn([]);
        Functions\when('get_users')->justReturn([]);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => sys_get_temp_dir()]);
        Functions\when('sanitize_text_field')->alias(static fn ($v) => $v);
        Functions\when('wp_unslash')->alias(static fn ($v) => $v);

        $payload = (new MetadataCommand())->collect();

        $this->assertArrayHasKey('roles', $payload, 'the metadata read path must carry the role registry');
        $this->assertContains(
            ['slug' => 'shop_manager', 'name' => 'Gestore negozio'],
            $payload['roles']
        );
    }

    public function test_metadata_always_carries_a_roles_key_even_when_unreadable(): void
    {
        // Always present, never absent: "[] roles" and "no roles key" would be
        // indistinguishable to the control plane otherwise, and it needs to be
        // able to say "this site has not reported its roles" out loud.
        Functions\when('wp_roles')->justReturn(null);
        Functions\when('get_bloginfo')->justReturn('6.8');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_site_transient')->justReturn(false);
        Functions\when('get_core_updates')->justReturn([]);
        Functions\when('get_users')->justReturn([]);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => sys_get_temp_dir()]);
        Functions\when('sanitize_text_field')->alias(static fn ($v) => $v);
        Functions\when('wp_unslash')->alias(static fn ($v) => $v);

        $payload = (new MetadataCommand())->collect();

        $this->assertArrayHasKey('roles', $payload);
        $this->assertSame([], $payload['roles']);
    }
}
