<?php
/**
 * The agent loads its own translations (GH #967).
 *
 * Every user-visible string uses the `wpmgr-agent` text domain, but until this
 * change nothing ever loaded a catalogue for it: the main file had no
 * `Domain Path` header and no load_plugin_textdomain() call, so a
 * wpmgr-agent-de_DE.mo dropped into the plugin's languages/ folder was never
 * read.
 *
 * What this file locks down:
 *
 *   - Plugin::boot() binds the loader to `init`, the hook WordPress documents
 *     for loading a plugin's text domain.
 *   - The loader points load_plugin_textdomain() at the plugin's own languages/
 *     folder, relative to the plugins directory, derived from the main file.
 *   - The main file header names that folder in `Domain Path`, and the folder
 *     exists (a `Domain Path` that points nowhere is a Plugin Check finding in
 *     the wp.org build).
 *   - The wp.org build (WPMGR_WPORG_BUILD defined true, as `make
 *     agent-zip-wporg` stamps the staged main file) neither binds the hook nor
 *     calls load_plugin_textdomain(): wordpress.org serves language packs for
 *     its own slug and Plugin Check discourages the call there.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Support\TextDomain;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\TextDomain
 * @covers \WPMgr\Agent\Plugin
 */
final class TextDomainTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** @var list<array{0:string,1:mixed,2:int}> Hook bindings, in order (hook, callback, priority). */
    private array $actions = [];

    private string $uploadsDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->uploadsDir = sys_get_temp_dir() . '/wpmgr-textdomain-' . bin2hex(random_bytes(6));
        mkdir($this->uploadsDir, 0755, true);
        $this->options = [];
        $this->actions = [];

        // The minimal stub set the other Plugin::boot() tests use.
        foreach (['add_filter', 'register_activation_hook', 'register_deactivation_hook',
                  'wp_schedule_single_event', 'spawn_cron', 'wp_get_scheduled_event',
                  'wp_next_scheduled', 'wp_schedule_event', 'wp_clear_scheduled_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('add_action')->alias(function ($hook, $callback, $priority = 10) {
            $this->actions[] = [(string) $hook, $callback, (int) $priority];

            return true;
        });
        Functions\when('is_admin')->justReturn(false);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('plugin_basename')->returnArg();
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);

            return true;
        });
        Functions\when('get_site_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('update_site_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);

        // Every test below needs a main-file path; the value is process-global
        // and set by whichever test file defines it first, so nothing here
        // depends on what it is.
        if (!defined('WPMGR_AGENT_FILE')) {
            define('WPMGR_AGENT_FILE', '/path/to/wpmgr-agent/wpmgr-agent.php');
        }
    }

    protected function tear_down(): void
    {
        Plugin::resetForTesting();
        if (is_dir($this->uploadsDir)) {
            @rmdir($this->uploadsDir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Booting the plugin binds the loader to `init`, once.
     */
    public function test_boot_binds_the_text_domain_loader_to_init(): void
    {
        Plugin::boot();

        $bound = array_values(array_filter(
            $this->actions,
            static fn (array $a): bool => $a[0] === 'init' && $a[1] === [TextDomain::class, 'load']
        ));

        $this->assertCount(
            1,
            $bound,
            'Plugin::boot() must bind [TextDomain::class, "load"] to the init action exactly once, or the plugin never loads its translations'
        );
    }

    /**
     * What the bound loader does: load the `wpmgr-agent` domain from the
     * plugin's own languages/ folder, as a path relative to the plugins
     * directory, derived from the main file the same way WordPress derives
     * the plugin basename.
     */
    public function test_the_loader_points_load_plugin_textdomain_at_the_plugin_languages_folder(): void
    {
        $seenMainFile = null;
        Functions\when('plugin_basename')->alias(function ($file) use (&$seenMainFile): string {
            $seenMainFile = $file;

            return 'wpmgr-agent/wpmgr-agent.php';
        });
        Functions\expect('load_plugin_textdomain')
            ->once()
            ->with('wpmgr-agent', false, 'wpmgr-agent/languages')
            ->andReturn(true);

        $this->assertTrue(TextDomain::load());
        $this->assertSame(
            constant('WPMGR_AGENT_FILE'),
            $seenMainFile,
            'the folder must be derived from the real main file, not guessed'
        );
    }

    /**
     * The folder follows the main file, so the wp.org build (renamed main
     * file, different folder) and a renamed self-hosted install both resolve
     * their own folder.
     */
    public function test_the_languages_path_follows_the_plugin_folder(): void
    {
        Functions\when('plugin_basename')->alias(
            static fn (string $file): string => 'fleet-agent-site-manager/fleet-agent-site-manager.php'
        );

        $this->assertSame(
            'fleet-agent-site-manager/languages',
            TextDomain::languagesPath('/srv/wp-content/plugins/fleet-agent-site-manager/fleet-agent-site-manager.php')
        );
    }

    /**
     * The main file header names the folder, and the folder exists.
     */
    public function test_the_main_file_header_names_the_domain_path_and_the_folder_exists(): void
    {
        $agentDir = dirname(__DIR__);
        $main     = (string) file_get_contents($agentDir . '/wpmgr-agent.php');
        $this->assertNotSame('', $main, 'the main plugin file could not be read');

        $this->assertMatchesRegularExpression('/^ \* Text Domain:\s+wpmgr-agent\s*$/m', $main);
        $this->assertMatchesRegularExpression(
            '/^ \* Domain Path:\s+\/languages\s*$/m',
            $main,
            'the header must carry "Domain Path: /languages" so WordPress knows where the catalogues live'
        );
        $this->assertDirectoryExists(
            $agentDir . '/languages',
            'the folder named by Domain Path must exist; Plugin Check warns on a Domain Path that points nowhere'
        );
    }

    /**
     * The wp.org build: no hook, no call.
     *
     * Separate process: WPMGR_WPORG_BUILD is a real constant, and once defined
     * it can never be undefined for the rest of the PHPUnit process (see
     * SizeProbeWporgGuardTest for the same idiom).
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_the_wporg_build_neither_binds_nor_calls_the_loader(): void
    {
        if (!defined('WPMGR_WPORG_BUILD')) {
            define('WPMGR_WPORG_BUILD', true);
        }

        $bound  = [];
        $loaded = [];
        Functions\when('add_action')->alias(function (...$args) use (&$bound): bool {
            $bound[] = $args;

            return true;
        });
        Functions\when('load_plugin_textdomain')->alias(function (...$args) use (&$loaded): bool {
            $loaded[] = $args;

            return true;
        });

        TextDomain::register();
        $result = TextDomain::load();

        $this->assertSame([], $bound, 'the wp.org build must not bind any hook for the text domain');
        $this->assertSame([], $loaded, 'the wp.org build must never call load_plugin_textdomain(); wordpress.org serves the language packs');
        $this->assertFalse($result, 'load() reports that nothing was loaded');
    }
}
