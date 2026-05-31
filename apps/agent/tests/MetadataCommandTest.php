<?php
/**
 * Tests for the metadata collector command.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MetadataCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\MetadataCommand
 */
final class MetadataCommandTest extends TestCase
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

    public function test_collect_returns_expected_fields(): void
    {
        Functions\when('get_bloginfo')->alias(static fn ($k) => $k === 'version' ? '6.5.2' : '');
        Functions\when('is_multisite')->justReturn(true);
        Functions\when('get_option')->alias(static function ($name) {
            return $name === 'active_plugins' ? ['akismet/akismet.php'] : false;
        });
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Name' => 'Akismet', 'Version' => '5.3'],
            'hello/hello.php'     => ['Name' => 'Hello Dolly', 'Version' => '1.7.2'],
        ]);
        // Empty update transients + no core update keep this test focused on
        // the inventory shape; dedicated tests cover the populated cases.
        Functions\when('get_site_transient')->justReturn(false);
        Functions\when('get_core_updates')->justReturn([]);

        $activeTheme = new class {
            /** @param string $k Field. @return string */
            public function get($k): string
            {
                return match ($k) {
                    'Name'    => 'Twenty Twenty-Four',
                    'Version' => '1.0',
                    default   => '',
                };
            }
            public function get_template(): string
            {
                return 'twentytwentyfour';
            }
            public function get_stylesheet(): string
            {
                return 'twentytwentyfour';
            }
        };
        Functions\when('wp_get_theme')->justReturn($activeTheme);

        $themeObj = new class {
            /** @param string $k Field. @return string */
            public function get($k): string
            {
                return match ($k) {
                    'Name'    => 'Twenty Twenty-Four',
                    'Version' => '1.0',
                    default   => '',
                };
            }
        };
        Functions\when('wp_get_themes')->justReturn([
            'twentytwentyfour' => $themeObj,
            'twentytwentythree' => $themeObj,
        ]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');

        $_SERVER['SERVER_SOFTWARE'] = 'nginx/1.25.3';

        $data = (new MetadataCommand())->collect();

        $this->assertSame('6.5.2', $data['wp_version']);
        $this->assertSame(PHP_VERSION, $data['php_version']);
        $this->assertSame('nginx/1.25.3', $data['server_info']);
        $this->assertTrue($data['multisite']);

        // active_theme is a STRING (stylesheet slug) per the contract.
        $this->assertSame('twentytwentyfour', $data['active_theme']);

        // Plugins: both installed, with the active flag set correctly.
        // Contract shape since ADR-037 Sprint 1, 1C (sparse-metadata expansion):
        // {slug,name,version,active,available_update,plugin_uri,update_uri,
        //  author_uri,network}. The four 1C fields are sourced from the plugin
        // header (empty-string / false defaults when the header omits them) and
        // the CP tolerantly decodes them.
        $this->assertCount(2, $data['plugins']);
        $byFile = [];
        foreach ($data['plugins'] as $p) {
            $this->assertSame(
                ['slug', 'name', 'version', 'active', 'available_update', 'plugin_uri', 'update_uri', 'author_uri', 'network'],
                array_keys($p)
            );
            $byFile[$p['slug']] = $p;
        }
        $this->assertSame('akismet/akismet.php', $byFile['akismet/akismet.php']['slug']);
        $this->assertSame('Akismet', $byFile['akismet/akismet.php']['name']);
        $this->assertSame('5.3', $byFile['akismet/akismet.php']['version']);
        $this->assertTrue($byFile['akismet/akismet.php']['active']);
        $this->assertNull($byFile['akismet/akismet.php']['available_update']);
        $this->assertSame('hello/hello.php', $byFile['hello/hello.php']['slug']);
        $this->assertFalse($byFile['hello/hello.php']['active']);
        $this->assertNull($byFile['hello/hello.php']['available_update']);

        // 1C sparse fields default to '' / false when the header omits them
        // (the test plugin metadata carries only Name + Version).
        $this->assertSame('', $byFile['akismet/akismet.php']['plugin_uri']);
        $this->assertSame('', $byFile['akismet/akismet.php']['update_uri']);
        $this->assertSame('', $byFile['akismet/akismet.php']['author_uri']);
        $this->assertFalse($byFile['akismet/akismet.php']['network']);

        // Themes inventory: v0.9.0 contract shape
        // {slug,name,version,active,available_update}.
        $this->assertCount(2, $data['themes']);
        $byStylesheet = [];
        foreach ($data['themes'] as $t) {
            $this->assertSame(['slug', 'name', 'version', 'active', 'available_update'], array_keys($t));
            $byStylesheet[$t['slug']] = $t;
        }
        $this->assertSame('twentytwentyfour', $byStylesheet['twentytwentyfour']['slug']);
        $this->assertSame('1.0', $byStylesheet['twentytwentyfour']['version']);
        $this->assertTrue($byStylesheet['twentytwentyfour']['active']);
        $this->assertNull($byStylesheet['twentytwentyfour']['available_update']);
        $this->assertFalse($byStylesheet['twentytwentythree']['active']);

        // core_update is ALWAYS present; null when no upgrade offered.
        $this->assertArrayHasKey('core_update', $data);
        $this->assertNull($data['core_update']);
    }

    public function test_execute_delegates_to_collect(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_theme')->justReturn(new class {
            /** @param string $k Field. @return string */
            public function get($k): string
            {
                return '';
            }
            public function get_template(): string
            {
                return 't';
            }
            public function get_stylesheet(): string
            {
                return 's';
            }
        });
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('s');
        Functions\when('get_site_transient')->justReturn(false);
        Functions\when('get_core_updates')->justReturn([]);

        $cmd  = new MetadataCommand();
        $data = $cmd->execute([], []);

        $this->assertSame('metadata', $cmd->name());
        $this->assertArrayHasKey('wp_version', $data);
        $this->assertArrayHasKey('plugins', $data);
        $this->assertArrayHasKey('themes', $data);
        $this->assertArrayHasKey('core_update', $data);
    }

    public function test_plugins_include_available_update_when_transient_has_entry(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([
            'wp-rocket/wp-rocket.php' => ['Name' => 'WP Rocket', 'Version' => '3.16.1'],
            'akismet/akismet.php'     => ['Name' => 'Akismet', 'Version' => '5.3.1'],
        ]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_core_updates')->justReturn([]);

        // update_plugins transient: one pending update keyed by basename.
        $pluginTransient = new \stdClass();
        $pluginEntry = new \stdClass();
        $pluginEntry->new_version  = '3.16.2';
        $pluginEntry->package      = 'https://wp-rocket.example/v3.16.2.zip';
        $pluginEntry->tested       = '6.5';
        $pluginEntry->requires_php = '7.4';
        $pluginTransient->response = ['wp-rocket/wp-rocket.php' => $pluginEntry];

        Functions\when('get_site_transient')->alias(static function ($key) use ($pluginTransient) {
            return $key === 'update_plugins' ? $pluginTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $byFile = [];
        foreach ($data['plugins'] as $p) {
            $byFile[$p['slug']] = $p;
        }

        $this->assertSame([
            'new_version'  => '3.16.2',
            'package'      => 'https://wp-rocket.example/v3.16.2.zip',
            'tested'       => '6.5',
            'requires_php' => '7.4',
        ], $byFile['wp-rocket/wp-rocket.php']['available_update']);

        // Akismet has no entry in the transient response map -> null.
        $this->assertNull($byFile['akismet/akismet.php']['available_update']);
    }

    public function test_plugins_available_update_is_null_when_transient_empty(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Name' => 'Akismet', 'Version' => '5.3.1'],
        ]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_site_transient')->justReturn(false);
        Functions\when('get_core_updates')->justReturn([]);

        $data = (new MetadataCommand())->collect();

        $this->assertCount(1, $data['plugins']);
        $this->assertNull($data['plugins'][0]['available_update']);
    }

    public function test_themes_include_available_update_when_transient_has_entry(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        $themeObj = new class {
            /** @param string $k Field. @return string */
            public function get($k): string
            {
                return match ($k) {
                    'Name'    => 'Twenty Twenty-Four',
                    'Version' => '1.0',
                    default   => '',
                };
            }
        };
        Functions\when('wp_get_themes')->justReturn([
            'twentytwentyfour' => $themeObj,
        ]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_core_updates')->justReturn([]);

        // Theme transient entries are ARRAYS, not objects (per WP core).
        $themeTransient = new \stdClass();
        $themeTransient->response = [
            'twentytwentyfour' => [
                'theme'        => 'twentytwentyfour',
                'new_version'  => '1.1',
                'package'      => 'https://wp.org/twentytwentyfour.1.1.zip',
                'requires_php' => '7.0',
            ],
        ];
        Functions\when('get_site_transient')->alias(static function ($key) use ($themeTransient) {
            return $key === 'update_themes' ? $themeTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $this->assertCount(1, $data['themes']);
        $this->assertSame([
            'new_version'  => '1.1',
            'package'      => 'https://wp.org/twentytwentyfour.1.1.zip',
            'tested'       => null,
            'requires_php' => '7.0',
        ], $data['themes'][0]['available_update']);
    }

    public function test_core_update_present_when_get_core_updates_returns_upgrade(): void
    {
        Functions\when('get_bloginfo')->alias(static fn ($k) => $k === 'version' ? '6.4.3' : '');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_site_transient')->justReturn(false);

        $core = new \stdClass();
        $core->response = 'upgrade';
        $core->version  = '6.5.2';
        Functions\when('get_core_updates')->justReturn([$core]);

        $data = (new MetadataCommand())->collect();

        $this->assertSame([
            'new_version'     => '6.5.2',
            'current_version' => '6.4.3',
        ], $data['core_update']);
    }

    public function test_core_update_null_when_no_upgrade_offered(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_site_transient')->justReturn(false);

        // get_core_updates returns a `latest` response (not `upgrade`).
        $core = new \stdClass();
        $core->response = 'latest';
        $core->version  = '6.5.2';
        Functions\when('get_core_updates')->justReturn([$core]);

        $data = (new MetadataCommand())->collect();

        $this->assertNull($data['core_update']);
    }
}
