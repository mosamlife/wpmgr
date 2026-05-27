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
        $this->assertSame('nginx/1.25.3', $data['server_software']);
        $this->assertTrue($data['is_multisite']);

        $this->assertSame('Twenty Twenty-Four', $data['active_theme']['name']);
        $this->assertSame('twentytwentyfour', $data['active_theme']['template']);
        $this->assertSame('twentytwentyfour', $data['active_theme']['stylesheet']);

        // Plugins: both installed, with the active flag set correctly.
        // Each plugin object must use the contract shape {slug,name,version,active}.
        $this->assertCount(2, $data['plugins']);
        $byFile = [];
        foreach ($data['plugins'] as $p) {
            $this->assertSame(['slug', 'name', 'version', 'active'], array_keys($p));
            $byFile[$p['slug']] = $p;
        }
        $this->assertSame('akismet/akismet.php', $byFile['akismet/akismet.php']['slug']);
        $this->assertSame('Akismet', $byFile['akismet/akismet.php']['name']);
        $this->assertSame('5.3', $byFile['akismet/akismet.php']['version']);
        $this->assertTrue($byFile['akismet/akismet.php']['active']);
        $this->assertSame('hello/hello.php', $byFile['hello/hello.php']['slug']);
        $this->assertFalse($byFile['hello/hello.php']['active']);

        // Themes inventory: contract shape {slug,name,version,active}.
        $this->assertCount(2, $data['themes']);
        $byStylesheet = [];
        foreach ($data['themes'] as $t) {
            $this->assertSame(['slug', 'name', 'version', 'active'], array_keys($t));
            $byStylesheet[$t['slug']] = $t;
        }
        $this->assertSame('twentytwentyfour', $byStylesheet['twentytwentyfour']['slug']);
        $this->assertSame('1.0', $byStylesheet['twentytwentyfour']['version']);
        $this->assertTrue($byStylesheet['twentytwentyfour']['active']);
        $this->assertFalse($byStylesheet['twentytwentythree']['active']);
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

        $cmd  = new MetadataCommand();
        $data = $cmd->execute([], []);

        $this->assertSame('metadata', $cmd->name());
        $this->assertArrayHasKey('wp_version', $data);
        $this->assertArrayHasKey('plugins', $data);
        $this->assertArrayHasKey('themes', $data);
    }
}
