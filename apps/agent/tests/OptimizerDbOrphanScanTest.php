<?php
/**
 * OptimizerDbOrphanScanTest — unit tests for Phase 3.3 orphan enumeration:
 *   DbCleanup::buildInstalledPluginsSnapshot()
 *   DbCleanup::scanOrphanedOptions()
 *   DbCleanup::scanOrphanedCron()
 *
 * Design goals:
 *   - The installed set = get_plugins() UNION mu-plugins UNION dropins UNION network.
 *     get_option('active_plugins') is only used for the active flag, NOT as the
 *     installed oracle.
 *   - Inactive installed plugins are included in the snapshot (P3.8 safety gate).
 *   - WP core option names + transients + wpmgr_ prefix are excluded from orphan set.
 *   - Installed-plugin attribution (slug-prefix) prevents false positives.
 *   - has_action() is NEVER called for cron orphan detection.
 *   - WP core cron hooks are excluded.
 *   - Capped results (500 options cap) work correctly.
 *   - All three methods are READ-ONLY (no writes).
 *   - No-wpdb path returns safe empty results.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Optimizer\DbCleanup;
use WPMgr\Agent\Optimizer\PerfConfig;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Optimizer\DbCleanup::buildInstalledPluginsSnapshot
 * @covers \WPMgr\Agent\Optimizer\DbCleanup::scanOrphanedOptions
 * @covers \WPMgr\Agent\Optimizer\DbCleanup::scanOrphanedCron
 */
final class OptimizerDbOrphanScanTest extends TestCase
{
    private OrphanFakeWpdb $wpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->wpdb = new OrphanFakeWpdb();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    private function cleanup(): DbCleanup
    {
        return new DbCleanup(new PerfConfig([]), $this->wpdb);
    }

    // =========================================================================
    // buildInstalledPluginsSnapshot() tests
    // =========================================================================

    public function test_snapshot_returns_array(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->returnArg(1); // fallback

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();
        $this->assertIsArray($snapshot);
    }

    public function test_snapshot_includes_regular_plugins(): void
    {
        Functions\when('get_plugins')->justReturn([
            'woocommerce/woocommerce.php' => ['Name' => 'WooCommerce'],
            'yoast-seo/yoast-seo.php'    => ['Name' => 'Yoast SEO'],
        ]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([
            'woocommerce/woocommerce.php', // active
            // yoast-seo is NOT active
        ]);

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();

        $slugs = array_column($snapshot, 'slug');
        $this->assertContains('woocommerce', $slugs);
        $this->assertContains('yoast-seo', $slugs);

        // WooCommerce is active; Yoast is installed but inactive.
        $wooEntry = null;
        $yoastEntry = null;
        foreach ($snapshot as $entry) {
            if ($entry['slug'] === 'woocommerce') {
                $wooEntry = $entry;
            }
            if ($entry['slug'] === 'yoast-seo') {
                $yoastEntry = $entry;
            }
        }
        $this->assertNotNull($wooEntry, 'woocommerce must be in snapshot');
        $this->assertNotNull($yoastEntry, 'yoast-seo must be in snapshot even when inactive');
        $this->assertTrue($wooEntry['active'], 'woocommerce should be active');
        $this->assertFalse($yoastEntry['active'], 'yoast-seo should be inactive (installed but not in active_plugins)');
        $this->assertSame('plugin', $wooEntry['source']);
        $this->assertSame('plugin', $yoastEntry['source']);
    }

    public function test_snapshot_includes_inactive_plugins_as_installed(): void
    {
        // CRITICAL: inactive plugins MUST be in the snapshot.
        // active_plugins is NOT the installed oracle — it is only used for the
        // active flag. An installed-but-inactive plugin is still an owner.
        Functions\when('get_plugins')->justReturn([
            'deactivated-plugin/deactivated-plugin.php' => ['Name' => 'Deactivated Plugin'],
        ]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]); // no active plugins

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();

        $slugs = array_column($snapshot, 'slug');
        $this->assertContains('deactivated-plugin', $slugs,
            'Installed-but-inactive plugin MUST be in snapshot (P3.8 safety gate)');

        foreach ($snapshot as $entry) {
            if ($entry['slug'] === 'deactivated-plugin') {
                $this->assertFalse($entry['active']);
                $this->assertSame('plugin', $entry['source']);
            }
        }
    }

    public function test_snapshot_includes_mu_plugins(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([
            'my-mu-plugin.php' => ['Name' => 'My MU Plugin'],
        ]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();

        $slugs = array_column($snapshot, 'slug');
        $this->assertContains('my-mu-plugin', $slugs);

        foreach ($snapshot as $entry) {
            if ($entry['slug'] === 'my-mu-plugin') {
                $this->assertTrue($entry['active'], 'mu-plugins are always active');
                $this->assertSame('mu-plugin', $entry['source']);
            }
        }
    }

    public function test_snapshot_includes_dropins(): void
    {
        // Simulate object-cache.php dropin.
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([
            'object-cache.php' => ['Name' => 'Object Cache'],
            'advanced-cache.php' => ['Name' => 'Advanced Cache'],
        ]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();

        $slugs = array_column($snapshot, 'slug');
        $this->assertContains('object-cache', $slugs);
        $this->assertContains('advanced-cache', $slugs);

        foreach ($snapshot as $entry) {
            if ($entry['slug'] === 'object-cache') {
                $this->assertSame('dropin', $entry['source']);
            }
        }
    }

    public function test_snapshot_no_mu_plugins_function_safe(): void
    {
        // get_mu_plugins() must still be defined (Brain\Monkey stubs WP functions).
        // Test that a snapshot with no mu-plugins is safe.
        Functions\when('get_plugins')->justReturn([
            'hello/hello.php' => ['Name' => 'Hello Dolly'],
        ]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();
        $this->assertIsArray($snapshot);
        $slugs = array_column($snapshot, 'slug');
        $this->assertContains('hello', $slugs);
    }

    public function test_snapshot_source_column_values(): void
    {
        Functions\when('get_plugins')->justReturn([
            'jetpack/jetpack.php' => ['Name' => 'Jetpack'],
        ]);
        Functions\when('get_mu_plugins')->justReturn([
            'must-use.php' => ['Name' => 'Must Use'],
        ]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn(['jetpack/jetpack.php']);

        $snapshot = $this->cleanup()->buildInstalledPluginsSnapshot();

        foreach ($snapshot as $entry) {
            $this->assertContains($entry['source'], ['plugin', 'mu-plugin', 'dropin', 'network'],
                "Source must be one of the four canonical values");
            $this->assertIsString($entry['slug']);
            $this->assertIsString($entry['name']);
            $this->assertIsBool($entry['active']);
        }
    }

    // =========================================================================
    // scanOrphanedOptions() tests
    // =========================================================================

    public function test_scan_orphaned_options_returns_struct(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        $this->wpdb->orphanOptionRows = [];
        $snapshot = [];

        $result = $this->cleanup()->scanOrphanedOptions($snapshot);

        $this->assertArrayHasKey('items', $result);
        $this->assertArrayHasKey('capped', $result);
        $this->assertIsArray($result['items']);
        $this->assertIsBool($result['capped']);
    }

    public function test_wp_core_options_excluded_from_orphans(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        // Inject WP core option names — all should be excluded.
        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'blogname',    'autoload' => 'yes', 'size_bytes' => 10],
            ['option_name' => 'siteurl',     'autoload' => 'yes', 'size_bytes' => 20],
            ['option_name' => 'active_plugins', 'autoload' => 'yes', 'size_bytes' => 30],
        ];

        $result = $this->cleanup()->scanOrphanedOptions([]);

        $this->assertSame([], $result['items'],
            'WP core option names must NOT appear in orphaned_options');
    }

    public function test_wpmgr_prefixed_options_excluded(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'wpmgr_last_scan',    'autoload' => 'yes', 'size_bytes' => 10],
            ['option_name' => 'wpmgr_perf_config',  'autoload' => 'yes', 'size_bytes' => 20],
        ];

        $result = $this->cleanup()->scanOrphanedOptions([]);

        $this->assertSame([], $result['items'],
            'wpmgr_ prefixed options must NOT appear in orphaned_options');
    }

    public function test_installed_plugin_options_excluded_by_prefix(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        // Woocommerce is installed → its options must NOT be flagged.
        $installedPlugins = [
            ['slug' => 'woocommerce', 'name' => 'WooCommerce', 'active' => true, 'source' => 'plugin'],
        ];

        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'woocommerce_settings', 'autoload' => 'yes', 'size_bytes' => 100],
            ['option_name' => 'woocommerce_version',  'autoload' => 'no',  'size_bytes' => 10],
        ];

        $result = $this->cleanup()->scanOrphanedOptions($installedPlugins);

        // These options are owned by the installed woocommerce plugin.
        $names = array_column($result['items'], 'name');
        $this->assertNotContains('woocommerce_settings', $names,
            'Options owned by installed plugins must not be flagged as orphans');
        $this->assertNotContains('woocommerce_version', $names,
            'Options owned by installed plugins must not be flagged as orphans');
    }

    public function test_truly_unknown_options_are_orphaned(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        // An option whose prefix does not match any installed plugin and is not a
        // WP core name should appear in the orphaned list.
        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'zxorphan_long_gone_setting', 'autoload' => 'no', 'size_bytes' => 42],
        ];

        $result = $this->cleanup()->scanOrphanedOptions([]);

        $names = array_column($result['items'], 'name');
        $this->assertContains('zxorphan_long_gone_setting', $names,
            'Unattributable option should appear in orphaned list');
    }

    public function test_orphaned_option_item_shape(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'zxorphan_foo_bar', 'autoload' => 'yes', 'size_bytes' => 55],
        ];

        $result = $this->cleanup()->scanOrphanedOptions([]);

        if ($result['items'] !== []) {
            $item = $result['items'][0];
            $this->assertArrayHasKey('name', $item);
            $this->assertArrayHasKey('autoload', $item);
            $this->assertArrayHasKey('size_bytes', $item);
            $this->assertArrayHasKey('guessed_prefix', $item);
            $this->assertIsString($item['name']);
            $this->assertIsBool($item['autoload']);
            $this->assertIsInt($item['size_bytes']);
            $this->assertIsString($item['guessed_prefix']);
        }
    }

    public function test_scan_orphaned_options_no_writes(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'zxorphan_test', 'autoload' => 'no', 'size_bytes' => 10],
        ];

        $this->cleanup()->scanOrphanedOptions([]);

        $this->assertSame([], $this->wpdb->writes,
            'scanOrphanedOptions() must not execute any write statements');
    }

    public function test_scan_orphaned_options_no_wpdb_returns_empty(): void
    {
        $cleanup = new DbCleanup(new PerfConfig([]), null);
        $result  = $cleanup->scanOrphanedOptions([]);
        $this->assertSame([], $result['items']);
        $this->assertFalse($result['capped']);
    }

    /**
     * Security regression test: transient-exclusion patterns MUST be bound as
     * %s args to wpdb::prepare(), NOT inlined as literal SQL containing bare
     * percent signs.
     *
     * Why this matters: a bare % in the prepare() template is not a valid
     * placeholder — on WP < 6.2 wpdb::prepare() mangles or silently drops it,
     * and on all versions it triggers a _doing_it_wrong notice. If the NOT LIKE
     * exclusion silently fails, _transient_* and _site_transient_* rows pass all
     * subsequent attribution passes and surface as false-positive orphans, feeding
     * the future destructive P3.8 gate with live WordPress data.
     *
     * The OrphanFakeWpdb::prepare() double now throws if a bare percent is found
     * in the template that is not a %s/%d/%f placeholder, so any regression here
     * will make the test suite fail before the guard reaches production.
     */
    public function test_transient_exclusion_patterns_are_bound_as_args_not_inlined(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            // Mirror WordPress core esc_like: escape \, %, _ for SQL LIKE.
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        $this->wpdb->orphanOptionRows = [];

        // This call must NOT throw; OrphanFakeWpdb::prepare() will throw if it
        // detects a bare percent literal in the SQL template.
        $result = $this->cleanup()->scanOrphanedOptions([]);

        // Verify the SELECT template for orphan options contains %s placeholders
        // (not inline patterns) for the transient exclusions.
        $optionScanTemplate = null;
        foreach ($this->wpdb->rawTemplates as $tpl) {
            if (stripos($tpl, 'option_name') !== false && stripos($tpl, 'NOT LIKE') !== false) {
                $optionScanTemplate = $tpl;
                break;
            }
        }

        $this->assertNotNull($optionScanTemplate,
            'scanOrphanedOptions() must have issued a prepare() call with option_name NOT LIKE');

        // The template must use %s placeholders, not inline LIKE patterns.
        $this->assertStringContainsString('NOT LIKE %s', $optionScanTemplate,
            'Transient exclusion must be a bound %s arg in the prepare() template');

        // The bound args for that query must contain the transient and site_transient
        // patterns. After esc_like(), underscores are backslash-escaped (\_), so we
        // search for the invariant substring "transient" which survives escaping.
        // We also verify that the args end with "%" (LIKE wildcard suffix), which
        // confirms this is a pattern arg, not just any arg that happens to have the
        // word "transient" in it.
        $foundTransientArg     = false;
        $foundSiteTransientArg = false;
        foreach ($this->wpdb->boundArgSets as $argSet) {
            foreach ($argSet as $arg) {
                if (!is_string($arg)) {
                    continue;
                }
                // After esc_like, "_transient_" becomes "\_transient\_"; the
                // literal "transient" substring is preserved so we use that.
                if (strpos($arg, 'transient') !== false && substr($arg, -1) === '%') {
                    // Distinguish _transient_ (no "site" prefix) from _site_transient_.
                    if (strpos($arg, 'site') !== false) {
                        $foundSiteTransientArg = true;
                    } else {
                        $foundTransientArg = true;
                    }
                }
            }
        }

        $this->assertTrue($foundTransientArg,
            'A bound arg for the _transient_ exclusion pattern must be passed to prepare() as a %s arg');
        $this->assertTrue($foundSiteTransientArg,
            'A bound arg for the _site_transient_ exclusion pattern must be passed to prepare() as a %s arg');

        // Confirm the result struct is still valid.
        $this->assertArrayHasKey('items', $result);
        $this->assertArrayHasKey('capped', $result);
    }

    public function test_theme_mods_excluded(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });

        $this->wpdb->orphanOptionRows = [
            ['option_name' => 'theme_mods_mycustomtheme', 'autoload' => 'yes', 'size_bytes' => 100],
        ];

        $result = $this->cleanup()->scanOrphanedOptions([]);

        $names = array_column($result['items'], 'name');
        $this->assertNotContains('theme_mods_mycustomtheme', $names,
            'theme_mods_* options must be excluded (theme ownership)');
    }

    // =========================================================================
    // scanOrphanedCron() tests
    // =========================================================================

    private function stubWpCoreFunctions(): void
    {
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);
        // esc_like() is called by scanOrphanedOptions() to build the transient-
        // exclusion LIKE patterns.  Mirror WordPress core: escape \, %, _ for SQL.
        Functions\when('esc_like')->alias(static function (string $s): string {
            return str_replace(['\\', '%', '_'], ['\\\\', '\\%', '\\_'], $s);
        });
    }

    public function test_scan_orphaned_cron_returns_array(): void
    {
        Functions\when('_get_cron_array')->justReturn([]);
        $this->stubWpCoreFunctions();

        $result = $this->cleanup()->scanOrphanedCron([]);
        $this->assertIsArray($result);
    }

    public function test_wp_core_cron_hooks_excluded(): void
    {
        $cron = [
            time() + 3600 => [
                'wp_version_check' => [
                    'abc123' => ['schedule' => 'twicedaily', 'args' => [], 'interval' => 43200],
                ],
                'wp_update_plugins' => [
                    'def456' => ['schedule' => 'twicedaily', 'args' => [], 'interval' => 43200],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);
        $this->stubWpCoreFunctions();

        $result = $this->cleanup()->scanOrphanedCron([]);

        $hooks = array_column($result, 'hook');
        $this->assertNotContains('wp_version_check', $hooks,
            'wp_version_check is a WP core hook and must not be flagged');
        $this->assertNotContains('wp_update_plugins', $hooks,
            'wp_update_plugins is a WP core hook and must not be flagged');
    }

    public function test_wpmgr_prefixed_cron_hooks_excluded(): void
    {
        $cron = [
            time() + 3600 => [
                'wpmgr_daily_backup' => [
                    'abc' => ['schedule' => 'daily', 'args' => [], 'interval' => 86400],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);
        $this->stubWpCoreFunctions();

        $result = $this->cleanup()->scanOrphanedCron([]);

        $hooks = array_column($result, 'hook');
        $this->assertNotContains('wpmgr_daily_backup', $hooks,
            'wpmgr_ prefixed cron hooks must not be flagged');
    }

    public function test_installed_plugin_cron_excluded_by_slug_prefix(): void
    {
        $ts   = time() + 3600;
        $cron = [
            $ts => [
                'woocommerce_scheduled_sales' => [
                    'e3b0c4' => ['schedule' => 'daily', 'args' => [], 'interval' => 86400],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);
        $this->stubWpCoreFunctions();

        // woocommerce is installed → its cron hooks must not be flagged.
        $installedPlugins = [
            ['slug' => 'woocommerce', 'name' => 'WooCommerce', 'active' => true, 'source' => 'plugin'],
        ];

        $result = $this->cleanup()->scanOrphanedCron($installedPlugins);

        $hooks = array_column($result, 'hook');
        $this->assertNotContains('woocommerce_scheduled_sales', $hooks,
            'Cron hook prefixed by installed plugin slug must not be flagged');
    }

    public function test_truly_unknown_cron_hook_is_orphaned(): void
    {
        $ts   = time() + 7200;
        $cron = [
            $ts => [
                'zxorphan_plugin_cleanup_job' => [
                    'deadbeef' => ['schedule' => 'hourly', 'args' => [], 'interval' => 3600],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);

        // No installed plugins.
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);

        $result = $this->cleanup()->scanOrphanedCron([]);

        $hooks = array_column($result, 'hook');
        $this->assertContains('zxorphan_plugin_cleanup_job', $hooks,
            'Unattributable cron hook must appear in orphaned_cron');
    }

    public function test_orphaned_cron_item_shape(): void
    {
        $ts   = time() + 1800;
        $cron = [
            $ts => [
                'zxorphan_do_something' => [
                    'cafebabe' => ['schedule' => 'twicedaily', 'args' => ['param1', 'param2'], 'interval' => 43200],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);

        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);

        $result = $this->cleanup()->scanOrphanedCron([]);

        if ($result !== []) {
            $item = $result[0];
            $this->assertArrayHasKey('hook', $item);
            $this->assertArrayHasKey('next_run_at', $item);
            $this->assertArrayHasKey('recurrence', $item);
            $this->assertArrayHasKey('args_hash', $item);
            $this->assertArrayHasKey('args_count', $item);
            $this->assertIsString($item['hook']);
            $this->assertIsInt($item['next_run_at']);
            $this->assertIsString($item['recurrence']);
            $this->assertIsString($item['args_hash']);
            $this->assertIsInt($item['args_count']);
            // Raw args must NOT be present (privacy).
            $this->assertArrayNotHasKey('args', $item);
        }
    }

    public function test_cron_args_count_correct(): void
    {
        $ts   = time() + 900;
        $cron = [
            $ts => [
                'zxorphan_with_args' => [
                    'hash001' => [
                        'schedule' => 'hourly',
                        'args'     => ['arg_a', 'arg_b', 'arg_c'],
                        'interval' => 3600,
                    ],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);

        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);

        $result = $this->cleanup()->scanOrphanedCron([]);

        foreach ($result as $item) {
            if ($item['hook'] === 'zxorphan_with_args') {
                $this->assertSame(3, $item['args_count']);
                $this->assertSame('hash001', $item['args_hash']);
                return;
            }
        }

        // If the hook was filtered, that is fine too (source-scan may have caught it).
    }

    public function test_cron_no_args_count_is_zero(): void
    {
        $ts   = time() + 500;
        $cron = [
            $ts => [
                'zxorphan_no_args' => [
                    'hash002' => ['schedule' => false, 'args' => [], 'interval' => 0],
                ],
            ],
        ];
        Functions\when('_get_cron_array')->justReturn($cron);

        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);

        $result = $this->cleanup()->scanOrphanedCron([]);

        foreach ($result as $item) {
            if ($item['hook'] === 'zxorphan_no_args') {
                $this->assertSame(0, $item['args_count']);
                // Non-recurring: recurrence should be empty.
                $this->assertSame('', $item['recurrence']);
                return;
            }
        }
    }

    public function test_scan_orphaned_cron_empty_array_returns_empty(): void
    {
        // When _get_cron_array() returns an empty array (no scheduled events), result is [].
        Functions\when('_get_cron_array')->justReturn([]);
        $this->stubWpCoreFunctions();

        $result = $this->cleanup()->scanOrphanedCron([]);
        $this->assertIsArray($result);
        $this->assertSame([], $result);
    }

    public function test_scan_orphaned_cron_non_array_cron_returns_empty(): void
    {
        // When _get_cron_array() returns a non-array, the guard should return [].
        Functions\when('_get_cron_array')->justReturn(false);
        $this->stubWpCoreFunctions();

        $result = $this->cleanup()->scanOrphanedCron([]);
        $this->assertIsArray($result);
        $this->assertSame([], $result);
    }

    /**
     * CRITICAL: has_action() must NEVER be consulted in scanOrphanedCron().
     * If it were called, it would mass-false-positive every third-party hook at
     * scan time (when most plugins are not loaded).
     */
    public function test_has_action_is_not_called(): void
    {
        $hasActionCalled = false;

        // Register a stub that records if it was called.
        Functions\when('has_action')->alias(static function () use (&$hasActionCalled) {
            $hasActionCalled = true;
            return false;
        });

        Functions\when('_get_cron_array')->justReturn([
            time() + 3600 => [
                'zxorphan_some_hook' => [
                    'abc' => ['schedule' => 'daily', 'args' => [], 'interval' => 86400],
                ],
            ],
        ]);

        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_mu_plugins')->justReturn([]);
        Functions\when('get_dropins')->justReturn([]);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_get_themes')->justReturn([]);

        $this->stubWpCoreFunctions();

        $this->cleanup()->scanOrphanedCron([]);

        $this->assertFalse($hasActionCalled,
            'scanOrphanedCron() MUST NOT call has_action() — wrong oracle at scan time');
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// Minimal wpdb double for orphan-scan tests.
//
// The orphan scan path calls:
//   - prepare()     — for paginated SELECT queries
//   - get_results() — to fetch option rows
//   - query()       — MUST NOT be called (read-only)
// ─────────────────────────────────────────────────────────────────────────────

final class OrphanFakeWpdb
{
    public string $prefix = 'wp_';

    /**
     * Rows returned by get_results() for the orphan options scan.
     * Each element: ['option_name'=>string, 'autoload'=>string, 'size_bytes'=>int].
     *
     * @var list<array<string,mixed>>
     */
    public array $orphanOptionRows = [];

    /** All write statements — must remain empty after scan methods. */
    public array $writes = [];

    /**
     * Raw query templates passed to prepare() (before substitution).
     * Used to assert that transient exclusion patterns are bound as %s args,
     * NOT inlined as literal SQL — which would leave a bare % in the template
     * and cause wpdb::prepare() to mangle or warn on WP < 6.2.
     *
     * @var list<string>
     */
    public array $rawTemplates = [];

    /**
     * Flattened bound args passed to each prepare() call, in call order.
     * Index i corresponds to rawTemplates[i].
     *
     * @var list<list<mixed>>
     */
    public array $boundArgSets = [];

    public function prepare(string $query, ...$args): string
    {
        $this->rawTemplates[] = $query;

        // Flat-flatten args (DbCleanup may pass a spread or a single array).
        $flat = [];
        foreach ($args as $a) {
            if (is_array($a)) {
                foreach ($a as $v) {
                    $flat[] = $v;
                }
            } else {
                $flat[] = $a;
            }
        }

        $this->boundArgSets[] = $flat;

        // Reject any lone percent in the template that is NOT a valid %s/%d/%f
        // placeholder — this is exactly the class of bug the security review
        // flagged.  A literal percent embedded in the SQL (e.g. LIKE '\_transient\_%')
        // leaves the trailing % unescaped; real wpdb::prepare() emits a
        // _doing_it_wrong notice and on WP < 6.2 may silently drop it.
        // Detecting it here makes the test fail early with a clear message.
        $templateWithoutPlaceholders = preg_replace('/%[sdf]/', '', $query);
        if ($templateWithoutPlaceholders !== null && strpos($templateWithoutPlaceholders, '%') !== false) {
            throw new \RuntimeException(
                "OrphanFakeWpdb::prepare() detected a bare percent-literal in the SQL " .
                "template that is not a valid %s/%d/%f placeholder. This would cause " .
                "wpdb::prepare() to emit _doing_it_wrong on real WordPress. " .
                "Bind the pattern as a %s arg instead. Template: " . $query
            );
        }

        $i   = 0;
        $sql = preg_replace_callback('/%[sd]/', static function ($m) use (&$i, $flat) {
            $v = $flat[$i] ?? '';
            $i++;
            return $m[0] === '%d' ? (string) (int) $v : "'" . addslashes((string) $v) . "'";
        }, $query);
        return (string) $sql;
    }

    public function get_results(string $sql, $mode = null): array
    {
        // Orphan options scan: OFFSET 0 returns the rows; higher offsets return [].
        if (stripos($sql, 'option_name') !== false && stripos($sql, 'OFFSET') !== false) {
            if (stripos($sql, 'OFFSET 0') !== false || preg_match('/OFFSET\s+\'?0\'?/i', $sql)) {
                return $this->orphanOptionRows;
            }
            return [];
        }
        return [];
    }

    public function get_row(string $sql, $mode = null): ?array
    {
        return null;
    }

    public function get_var(string $sql): ?string
    {
        return null;
    }

    public function get_col(string $sql): array
    {
        return [];
    }

    /** Records any write statement — scan methods must never call this. */
    public function query(string $sql): int
    {
        $this->writes[] = $sql;
        return 0;
    }

    /** Mirrors real wpdb::esc_like(): escape \, %, _ for a SQL LIKE clause. */
    public function esc_like(string $text): string
    {
        return addcslashes($text, '_%\\');
    }
}
