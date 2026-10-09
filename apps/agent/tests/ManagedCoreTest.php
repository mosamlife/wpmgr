<?php
/**
 * ManagedCoreTest - the detector that keeps a WordPress-side core update off a
 * Composer-owned or file-change-locked install (GitHub issue #367).
 *
 * Half of this file proves each signal fires; the other half proves the
 * detector stays quiet on ordinary sites, because a guard that reddens a
 * normal core update gets switched off. Every tree is a real directory on
 * disk, built per test under a private scratch root.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\ManagedCore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\ManagedCore
 */
final class ManagedCoreTest extends TestCase
{
    /** Private scratch root for this test's trees. */
    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root = sys_get_temp_dir() . '/wpmgr-managed-core-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0777, true);
    }

    protected function tear_down(): void
    {
        $this->deleteTree($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * @param string $dir Directory to remove recursively.
     * @return void
     */
    private function deleteTree(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        foreach (array_diff((array) scandir($dir), ['.', '..']) as $entry) {
            $path = $dir . '/' . $entry;
            if (is_dir($path)) {
                $this->deleteTree($path);
            } else {
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        rmdir($dir);
    }

    /**
     * Write a file under the scratch root, creating its directories.
     *
     * @param string $relative Path relative to the scratch root.
     * @param string $content  File content.
     * @return void
     */
    private function put(string $relative, string $content): void
    {
        $path = $this->root . '/' . $relative;
        if (!is_dir(dirname($path))) {
            mkdir(dirname($path), 0777, true);
        }
        file_put_contents($path, $content);
    }

    /**
     * A composer.json body requiring the given packages.
     *
     * @param array<string,string> $require    `require` section.
     * @param array<string,string> $requireDev `require-dev` section.
     * @return string
     */
    private function manifest(array $require, array $requireDev = []): string
    {
        $body = ['name' => 'example/site', 'type' => 'project', 'require' => $require];
        if ($requireDev !== []) {
            $body['require-dev'] = $requireDev;
        }

        return (string) json_encode($body, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES);
    }

    /**
     * A detector over a WordPress directory under the scratch root, with file
     * changes allowed unless a test says otherwise.
     *
     * @param string $wpDir         WordPress directory, relative to the scratch root.
     * @param bool   $fileModsAllowed Answer to the file-changes question.
     * @return ManagedCore
     */
    private function detector(string $wpDir, bool $fileModsAllowed = true): ManagedCore
    {
        if (!is_dir($this->root . '/' . $wpDir)) {
            mkdir($this->root . '/' . $wpDir, 0777, true);
        }

        return new ManagedCore($this->root . '/' . $wpDir . '/', static function () use ($fileModsAllowed): bool {
            return $fileModsAllowed;
        });
    }

    // =====================================================================
    // Signals that must fire
    // =====================================================================

    /** The reported layout: a Bedrock project, core at web/wp, composer.json two levels up. */
    public function test_a_bedrock_project_manifest_marks_core_as_composer_managed(): void
    {
        $this->put('composer.json', $this->manifest(['php' => '>=8.1', 'roots/wordpress' => '^7.0']));

        $verdict = $this->detector('web/wp')->detect();

        $this->assertSame(ManagedCore::REASON_COMPOSER, $verdict['reason']);
        $this->assertStringContainsString('requires roots/wordpress', $verdict['evidence']);
    }

    /**
     * @return array<string,array{string}>
     */
    public static function corePackages(): array
    {
        $rows = [];
        foreach (ManagedCore::CORE_PACKAGES as $package) {
            $rows[$package] = [$package];
        }

        return $rows;
    }

    /**
     * Every listed core package counts, compared case-insensitively the way
     * Composer compares package names.
     *
     * @dataProvider corePackages
     *
     * @param string $package Core package name.
     */
    public function test_every_core_package_marks_core_as_composer_managed(string $package): void
    {
        $this->put('composer.json', $this->manifest([strtoupper($package) => '*']));

        $verdict = $this->detector('wordpress')->detect();

        $this->assertSame(ManagedCore::REASON_COMPOSER, $verdict['reason'], $package);
    }

    /** A core package installed by Composer carries its own manifest in the WordPress directory. */
    public function test_a_core_package_manifest_inside_the_wordpress_directory_counts(): void
    {
        $this->put(
            'public/wp/composer.json',
            (string) json_encode(['name' => 'johnpbloch/wordpress-core', 'type' => 'wordpress-core', 'require' => ['php' => '>=7.2']])
        );

        $verdict = $this->detector('public/wp')->detect();

        $this->assertSame(ManagedCore::REASON_COMPOSER, $verdict['reason']);
        $this->assertStringContainsString('Composer-installed core package', $verdict['evidence']);
    }

    /** A Bedrock-style wp-config.php one level above the WordPress directory, as wp-load.php finds it. */
    public function test_a_framework_managed_wp_config_marks_core_as_composer_managed(): void
    {
        $this->put(
            'web/wp-config.php',
            "<?php\nrequire_once dirname(__DIR__) . '/vendor/autoload.php';\nrequire_once dirname(__DIR__) . '/config/application.php';\nrequire_once ABSPATH . 'wp-settings.php';\n"
        );

        $verdict = $this->detector('web/wp')->detect();

        $this->assertSame(ManagedCore::REASON_COMPOSER, $verdict['reason']);
        $this->assertStringContainsString('framework-managed config', $verdict['evidence']);
    }

    /** File changes disallowed, on a site with no Composer signal at all. */
    public function test_disallowed_file_changes_mark_core_as_not_updatable(): void
    {
        $verdict = $this->detector('htdocs', false)->detect();

        $this->assertSame(ManagedCore::REASON_FILE_MODS, $verdict['reason']);
    }

    /**
     * Bedrock sets DISALLOW_FILE_MODS by default, so both signals fire there.
     * Composer wins, because its remedy is the one that updates core.
     */
    public function test_composer_wins_when_both_signals_fire(): void
    {
        $this->put('composer.json', $this->manifest(['roots/wordpress' => '^7.0']));

        $verdict = $this->detector('web/wp', false)->detect();

        $this->assertSame(ManagedCore::REASON_COMPOSER, $verdict['reason']);
    }

    /**
     * The production route for the file-changes question is WordPress's own
     * capability question, with the context core uses for `update_core`.
     */
    public function test_the_default_file_changes_question_is_the_one_core_asks_for_update_core(): void
    {
        Functions\expect('wp_is_file_mod_allowed')
            ->once()
            ->with('capability_update_core')
            ->andReturn(false);

        mkdir($this->root . '/plain', 0777, true);
        $verdict = (new ManagedCore($this->root . '/plain/'))->detect();

        $this->assertSame(ManagedCore::REASON_FILE_MODS, $verdict['reason']);
        $this->assertSame('the file_mod_allowed filter disallows file changes', $verdict['evidence']);
    }

    // =====================================================================
    // Ordinary sites: the detector must stay quiet
    // =====================================================================

    /** A Composer project that manages plugins only leaves core to WordPress. */
    public function test_a_manifest_without_a_core_package_is_not_managed(): void
    {
        $this->put('composer.json', $this->manifest([
            'wpackagist-plugin/akismet' => '^5.0',
            'composer/installers'       => '^2.0',
            'roots/wp-password-bcrypt'  => '^1.1',
        ]));

        $this->assertSame('', $this->detector('web/wp')->detect()['reason']);
    }

    /** A core package used only for development or tests does not serve the site. */
    public function test_a_core_package_under_require_dev_is_not_managed(): void
    {
        $this->put('composer.json', $this->manifest(['php' => '>=8.1'], ['johnpbloch/wordpress' => '*']));

        $this->assertSame('', $this->detector('wordpress')->detect()['reason']);
    }

    /** No manifest, plain wp-config.php, file changes allowed: an ordinary site. */
    public function test_an_ordinary_site_is_not_managed(): void
    {
        $this->put('htdocs/wp-config.php', "<?php\ndefine('DB_NAME', 'wp');\nrequire_once ABSPATH . 'wp-settings.php';\n");

        $verdict = $this->detector('htdocs')->detect();

        $this->assertSame(['reason' => '', 'evidence' => ''], $verdict);
    }

    /** A manifest beyond the search bound belongs to something else. */
    public function test_a_manifest_above_the_search_bound_is_ignored(): void
    {
        $this->put('composer.json', $this->manifest(['roots/wordpress' => '^7.0']));

        // Four levels below the manifest: one more than PARENT_LEVELS.
        $this->assertSame('', $this->detector('a/b/c/wp')->detect()['reason']);
        // Three levels below it: inside the bound, so the same manifest counts.
        $this->assertSame(ManagedCore::REASON_COMPOSER, $this->detector('a/b/wp')->detect()['reason']);
    }

    /** A broken or oversized manifest is no evidence, never an error. */
    public function test_an_unparseable_manifest_is_not_managed(): void
    {
        $this->put('composer.json', '{"require": {"roots/wordpress": ');

        $this->assertSame('', $this->detector('web/wp')->detect()['reason']);
    }

    /** wp-load.php ignores a parent wp-config.php that sits beside another install's wp-settings.php. */
    public function test_a_parent_wp_config_belonging_to_another_install_is_not_read(): void
    {
        $this->put('site/wp-config.php', "<?php\nrequire_once __DIR__ . '/config/application.php';\n");
        $this->put('site/wp-settings.php', "<?php\n");

        $this->assertSame('', $this->detector('site/blog')->detect()['reason']);
    }

    /** An empty or relative WordPress directory is never searched from. */
    public function test_an_unusable_wordpress_directory_is_never_searched(): void
    {
        $allowed = static function (): bool {
            return true;
        };

        $this->assertSame('', (new ManagedCore('', $allowed))->detect()['reason']);
        $this->assertSame('', (new ManagedCore('/', $allowed))->detect()['reason']);
        $this->assertSame('', (new ManagedCore('relative/wp/', $allowed))->detect()['reason']);
    }

    /** A filter that throws is no answer: fall back to the constant, which is unset here. */
    public function test_a_throwing_file_changes_question_falls_back_to_the_constant(): void
    {
        Functions\when('wp_is_file_mod_allowed')->alias(static function (): bool {
            throw new \RuntimeException('a third-party filter threw');
        });

        mkdir($this->root . '/plain', 0777, true);

        $this->assertSame('', (new ManagedCore($this->root . '/plain/'))->detect()['reason']);
    }

    // =====================================================================
    // open_basedir
    // =====================================================================

    /** Probes outside open_basedir are skipped, matched on a path-segment boundary. */
    public function test_open_basedir_matching_is_anchored(): void
    {
        $this->assertTrue(ManagedCore::pathAllowedBy('/srv/site/web', ''));
        $this->assertTrue(ManagedCore::pathAllowedBy('/srv/site/web', '/srv/site'));
        $this->assertTrue(ManagedCore::pathAllowedBy('/srv/site', '/srv/site/'));
        $this->assertTrue(ManagedCore::pathAllowedBy('/srv/site/web', '/tmp' . PATH_SEPARATOR . '/srv/site'));

        $this->assertFalse(ManagedCore::pathAllowedBy('/srv/site2', '/srv/site'), 'a sibling sharing a prefix is outside');
        $this->assertFalse(ManagedCore::pathAllowedBy('/srv', '/srv/site'), 'a parent of the allowed tree is outside');
        $this->assertFalse(ManagedCore::pathAllowedBy('/srv/site', '.'), 'the working directory entry is never trusted');
    }
}
