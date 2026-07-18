<?php
/**
 * AdminBarPurge: "Manage in WPMgr" deep-link URL generation.
 *
 * GH #243 (Gap C) — dashboardCacheUrl() built `{cp_base}/sites/{siteId}/performance`,
 * a dashboard route that has never existed (the per-site tab is `/cache`; the
 * `/performance` slug is a fleet-level page, not a per-site one). This test pins
 * the exact generated URL for a known cp_url + siteId so a regression to the
 * wrong slug (or a change to the CP web app's route shape) is caught here
 * instead of shipping a dead admin-bar link again.
 *
 * @package WPMgr\Agent\Tests\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Cache;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\AdminBarPurge;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Settings;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\AdminBarPurge
 */
final class AdminBarPurgeTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('get_site_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('is_multisite')->justReturn(false);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Invoke the private dashboardCacheUrl() via reflection — it is the exact
     * unit that shipped the wrong route in v0.18.0, and it is not observable
     * through addBarNodes() without stubbing WP_Admin_Bar.
     */
    private function dashboardCacheUrl(AdminBarPurge $abp): string
    {
        $ref = new \ReflectionMethod(AdminBarPurge::class, 'dashboardCacheUrl');

        return (string) $ref->invoke($abp);
    }

    public function test_dashboard_cache_url_points_at_the_per_site_cache_tab(): void
    {
        $settings = new Settings();
        $settings->setControlPlaneUrl('https://manage.wpmgr.app');
        $settings->setEnrollment('a1b2c3d4-1111-2222-3333-444455556666', 'tenant-x');

        $abp = new AdminBarPurge(new CacheManager(), $settings);

        $this->assertSame(
            'https://manage.wpmgr.app/sites/a1b2c3d4-1111-2222-3333-444455556666/cache',
            $this->dashboardCacheUrl($abp)
        );
    }

    public function test_dashboard_cache_url_strips_a_trailing_slash_on_the_cp_base(): void
    {
        $settings = new Settings();
        $settings->setControlPlaneUrl('https://manage.wpmgr.app/');
        $settings->setEnrollment('a1b2c3d4-1111-2222-3333-444455556666', 'tenant-x');

        $abp = new AdminBarPurge(new CacheManager(), $settings);

        $this->assertSame(
            'https://manage.wpmgr.app/sites/a1b2c3d4-1111-2222-3333-444455556666/cache',
            $this->dashboardCacheUrl($abp)
        );
    }

    public function test_dashboard_cache_url_rawurlencodes_the_site_id(): void
    {
        $settings = new Settings();
        $settings->setControlPlaneUrl('https://manage.wpmgr.app');
        // Not a real site_id shape, but proves the segment is encoded rather
        // than interpolated raw.
        $settings->setEnrollment('site/with spaces', 'tenant-x');

        $abp = new AdminBarPurge(new CacheManager(), $settings);

        $this->assertSame(
            'https://manage.wpmgr.app/sites/site%2Fwith%20spaces/cache',
            $this->dashboardCacheUrl($abp)
        );
    }

    public function test_dashboard_cache_url_is_empty_when_not_enrolled(): void
    {
        $settings = new Settings();
        $settings->setControlPlaneUrl('https://manage.wpmgr.app');
        // No setEnrollment() call — site_id stays unset.

        $abp = new AdminBarPurge(new CacheManager(), $settings);

        $this->assertSame('', $this->dashboardCacheUrl($abp));
    }
}
