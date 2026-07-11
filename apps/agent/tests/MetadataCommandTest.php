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

        // MetadataCommand calls wp_upload_dir() under a function_exists() guard.
        // Once any test in the suite defines wp_upload_dir as a Brain Monkey stub,
        // function_exists('wp_upload_dir') returns true for all subsequent tests in
        // the PHP process. Stub it here unconditionally so the guard triggers a
        // predictable result rather than an "unmocked function" error.
        Functions\when('wp_upload_dir')->justReturn([
            'basedir' => '/var/www/html/wp-content/uploads',
            'baseurl' => 'https://example.com/wp-content/uploads',
        ]);
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

    /**
     * GH #211: WordPress's own `update_plugins` transient can carry a
     * response entry whose `new_version` merely EQUALS the installed
     * version (a stale/duplicate offer) — that must not surface as an
     * "available update".
     */
    public function test_plugins_available_update_is_null_when_same_version(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Name' => 'Akismet', 'Version' => '5.3.1'],
        ]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_core_updates')->justReturn([]);

        $pluginTransient = new \stdClass();
        $pluginEntry = new \stdClass();
        $pluginEntry->new_version = '5.3.1';
        $pluginTransient->response = ['akismet/akismet.php' => $pluginEntry];
        Functions\when('get_site_transient')->alias(static function ($key) use ($pluginTransient) {
            return $key === 'update_plugins' ? $pluginTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $this->assertCount(1, $data['plugins']);
        $this->assertNull($data['plugins'][0]['available_update']);
    }

    /**
     * GH #211 normalization: a leading `v`/`V` and incidental whitespace on
     * either side of the comparison must not defeat the same-version guard
     * (`v1.5.1` from the transient vs. ` 1.5.1 ` from the plugin header).
     */
    public function test_plugins_available_update_normalizes_leading_v_and_whitespace(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([
            'foo/foo.php' => ['Name' => 'Foo', 'Version' => ' 1.5.1 '],
        ]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_core_updates')->justReturn([]);

        $pluginTransient = new \stdClass();
        $pluginEntry = new \stdClass();
        $pluginEntry->new_version = 'v1.5.1';
        $pluginTransient->response = ['foo/foo.php' => $pluginEntry];
        Functions\when('get_site_transient')->alias(static function ($key) use ($pluginTransient) {
            return $key === 'update_plugins' ? $pluginTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $this->assertNull($data['plugins'][0]['available_update']);
    }

    /**
     * Regression guard: the #211 same-version guard must NOT suppress a
     * genuinely newer offer — a real update must still surface even when
     * both versions carry a leading `v`.
     */
    public function test_plugins_available_update_still_surfaces_when_genuinely_newer(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([
            'foo/foo.php' => ['Name' => 'Foo', 'Version' => 'v1.5.1'],
        ]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_core_updates')->justReturn([]);

        $pluginTransient = new \stdClass();
        $pluginEntry = new \stdClass();
        $pluginEntry->new_version = '1.6.0';
        $pluginTransient->response = ['foo/foo.php' => $pluginEntry];
        Functions\when('get_site_transient')->alias(static function ($key) use ($pluginTransient) {
            return $key === 'update_plugins' ? $pluginTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $this->assertNotNull($data['plugins'][0]['available_update']);
        $this->assertSame('1.6.0', $data['plugins'][0]['available_update']['new_version']);
    }

    /**
     * Fail-open guard: when the installed version cannot be determined
     * (get_plugins() omitted the Version header, so plugins() falls back to
     * the 'unknown' sentinel), the same-version comparison must be skipped
     * entirely and the offered update kept — hiding a real update because
     * the installed version was unreadable would be worse than an occasional
     * over-report.
     */
    public function test_plugins_available_update_present_when_installed_version_unknown(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_option')->justReturn([]);
        Functions\when('get_plugins')->justReturn([
            'foo/foo.php' => ['Name' => 'Foo'],
        ]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_core_updates')->justReturn([]);

        $pluginTransient = new \stdClass();
        $pluginEntry = new \stdClass();
        $pluginEntry->new_version = '1.0.0';
        $pluginTransient->response = ['foo/foo.php' => $pluginEntry];
        Functions\when('get_site_transient')->alias(static function ($key) use ($pluginTransient) {
            return $key === 'update_plugins' ? $pluginTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $this->assertSame('unknown', $data['plugins'][0]['version']);
        $this->assertNotNull($data['plugins'][0]['available_update']);
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

    /**
     * GH #211 theme-side sibling of test_plugins_available_update_is_null_when_same_version():
     * the `update_themes` transient's `new_version` merely EQUALS the
     * installed stylesheet version — must not surface as an "available
     * update".
     */
    public function test_themes_available_update_is_null_when_same_version(): void
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
                    'Version' => '1.5.1',
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
                'theme'       => 'twentytwentyfour',
                'new_version' => '1.5.1',
            ],
        ];
        Functions\when('get_site_transient')->alias(static function ($key) use ($themeTransient) {
            return $key === 'update_themes' ? $themeTransient : false;
        });

        $data = (new MetadataCommand())->collect();

        $this->assertCount(1, $data['themes']);
        $this->assertNull($data['themes'][0]['available_update']);
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

    /**
     * GH #211 core-side sibling: an `update_core` transient entry whose
     * `response` is 'upgrade' but whose offered `version` merely EQUALS the
     * currently-running core version (a stale/duplicate entry) must not
     * surface as a core update.
     */
    public function test_core_update_null_when_new_version_not_newer(): void
    {
        Functions\when('get_bloginfo')->alias(static fn ($k) => $k === 'version' ? '6.5.2' : '');
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

        $this->assertNull($data['core_update']);
    }

    // ---- open_basedir log-flood guard (GitHub issue #131) -----------------

    /**
     * Reproduces the reported log flood: on a locked-down managed host with
     * `open_basedir` restricting the PHP process to the webroot,
     * `is_dir('/var/lib/runcloud')` — an absolute, out-of-webroot probe — used
     * to raise an unsilenced E_WARNING on every single agent request. Fixed
     * by `@`-guarding that one call site (class-metadata-command.php).
     *
     * `open_basedir` can only ever be TIGHTENED at runtime — PHP refuses to
     * widen it back — so genuinely reproducing the restriction cannot be done
     * inline: it would permanently break every later filesystem-touching test
     * in this (single, non-isolated) PHPUnit process. Instead this spawns a
     * disposable `php -d open_basedir=...` child process whose entire
     * lifetime is the probe: it loads only the two source files hostFlags()
     * needs (no WordPress dependency — the method is pure PHP), invokes it
     * under a custom error handler, and reports what it saw as JSON. The
     * restriction, and its irreversibility, are fully contained to that
     * throwaway process.
     */
    public function test_host_flags_runcloud_probe_raises_no_warning_under_open_basedir(): void
    {
        if (!function_exists('exec')) {
            $this->markTestSkipped('exec() is unavailable in this PHP configuration.');
        }

        $includesDir = dirname(__DIR__) . '/includes';
        $tmpDir      = sys_get_temp_dir();

        $probeScript = (string) tempnam($tmpDir, 'wpmgr-hostflags-');
        file_put_contents($probeScript, self::hostFlagsProbeSource());

        try {
            $cmd = escapeshellarg(PHP_BINARY)
                . ' -d ' . escapeshellarg('open_basedir=' . $includesDir . PATH_SEPARATOR . $tmpDir)
                . ' -f ' . escapeshellarg($probeScript)
                . ' -- ' . escapeshellarg($includesDir);

            exec($cmd . ' 2>&1', $outputLines, $exitCode); // phpcs:ignore WordPress.PHP.DiscouragedFunctions.exec -- test-only: spawns a disposable subprocess to safely reproduce a runtime-irreversible open_basedir restriction; all inputs are internally generated absolute paths, none are request-derived
        } finally {
            @unlink($probeScript); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }

        $rawOutput = implode("\n", $outputLines);
        $this->assertSame(0, $exitCode, 'probe subprocess must exit cleanly: ' . $rawOutput);

        $result = json_decode($rawOutput, true);
        $this->assertIsArray($result, 'probe subprocess must emit parseable JSON: ' . $rawOutput);
        $this->assertSame(
            [],
            $result['warnings'] ?? ['<missing warnings key>'],
            'hostFlags() must not leak an unsuppressed warning from the RunCloud filesystem probe under open_basedir'
        );
        $this->assertTrue($result['is_runcloud_is_bool'] ?? false, 'is_runcloud must remain a bool');
    }

    /**
     * Source for the isolated open_basedir probe subprocess spawned by
     * test_host_flags_runcloud_probe_raises_no_warning_under_open_basedir().
     *
     * Deliberately a standalone script with no Composer/WordPress bootstrap:
     * hostFlags() is pure PHP (defined()/is_dir()/$_SERVER only), so loading
     * just the command interface + class under test is sufficient. Takes the
     * includes/ directory as argv[1] so it never hard-codes an absolute path
     * that could drift from this test file's own location.
     */
    private static function hostFlagsProbeSource(): string
    {
        return <<<'PHP'
<?php
declare(strict_types=1);

$includesDir = $argv[1] ?? '';
require_once $includesDir . '/commands/interface-command-interface.php';
require_once $includesDir . '/commands/class-metadata-command.php';

$warnings = [];
// Respect the standard @-operator convention: PHP still invokes a custom
// error handler for a silenced expression, but the handler's
// error_reporting() reads a reduced mask that excludes the silenced level —
// AND-ing it against $errno is how a well-behaved handler tells "silenced"
// apart from "a warning that actually escaped" (checking `=== 0` is NOT
// reliable: PHP 8's @ mask still includes the always-fatal levels).
set_error_handler(static function (int $errno, string $errstr) use (&$warnings): bool {
    if ((error_reporting() & $errno) === 0) {
        return true;
    }
    $warnings[] = $errstr;

    return true;
});

$ref = new ReflectionMethod(\WPMgr\Agent\Commands\MetadataCommand::class, 'hostFlags');
$flags = $ref->invoke(new \WPMgr\Agent\Commands\MetadataCommand());

restore_error_handler();

echo json_encode([
    'warnings'            => $warnings,
    'is_runcloud_is_bool' => is_bool($flags['is_runcloud']),
]);
PHP;
    }

    /**
     * Durable, environment-independent regression guard: no matter what a
     * future edit does to hostFlags() (or adds beside it), an absolute
     * out-of-webroot filesystem probe like `/var/lib/runcloud` must never be
     * called unsilenced. A static source check catches this even in an
     * environment (or a future PHP) where the open_basedir-restriction test
     * above cannot be exercised.
     */
    public function test_metadata_command_source_has_no_unsuppressed_var_lib_probe(): void
    {
        $source = (string) file_get_contents(dirname(__DIR__) . '/includes/commands/class-metadata-command.php');

        $this->assertMatchesRegularExpression(
            "#@is_dir\\('/var/lib/runcloud'\\)#",
            $source,
            'the RunCloud probe must remain @-guarded'
        );
        $this->assertDoesNotMatchRegularExpression(
            "#(?<!@)is_dir\\('/var/#",
            $source,
            'no unsuppressed is_dir() call against an absolute /var/... path may be reintroduced'
        );
    }
}
