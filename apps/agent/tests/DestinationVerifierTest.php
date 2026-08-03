<?php
/**
 * DestinationVerifierTest - the restore-skip's own backstop (GitHub issue #328).
 *
 * REAL TEMPORARY DIRECTORIES, no mocks: this class is filesystem behaviour, and
 * a mocked filesystem would only prove that the mock agrees with the code.
 *
 * The verdict is three-valued and the whole safety argument rests on the
 * asymmetry: only `true` may skip a restore, and `true` requires the POSITIVE
 * header read. Every test below is therefore either "this is provably wrong"
 * (false), "this host cannot answer" (null) or the single narrow case that is
 * allowed to skip.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\DestinationVerifier;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\DestinationVerifier
 */
final class DestinationVerifierTest extends TestCase
{
    /** Absolute path to this test's own scratch tree (payload copies). */
    private string $root = '';

    /** Absolute path to the plugins root this process uses. */
    private string $pluginsRoot = '';

    /** @var array<int,string> Fixture directories to remove in tear-down. */
    private array $created = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root = sys_get_temp_dir() . '/wpmgr-destverify-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0777, true);

        // WP_PLUGIN_DIR is a process-global CONSTANT and this suite is not
        // isolated, so whichever test file runs first pins it. ADOPT whatever
        // is already there (creating it when needed) rather than fighting for
        // it, exactly as SnapshotManagerTest does, and give every fixture a
        // unique folder name so files never collide across tests.
        $this->pluginsRoot = $this->ensurePluginRoot();
        $this->created     = [];
    }

    protected function tear_down(): void
    {
        foreach ($this->created as $dir) {
            $this->deleteTree($dir);
        }
        $this->deleteTree($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Adopt (or establish) this process's plugins root.
     *
     * @return string Absolute path without a trailing slash.
     */
    private function ensurePluginRoot(): string
    {
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir((string) constant('WP_CONTENT_DIR'))) {
            mkdir((string) constant('WP_CONTENT_DIR'), 0777, true);
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins');
        }
        $root = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');
        if (!is_dir($root)) {
            mkdir($root, 0777, true);
        }

        return $root;
    }

    /**
     * A collision-proof fixture folder name.
     *
     * @param string $label Human label.
     * @return string
     */
    private function uniqueFolder(string $label): string
    {
        return 'wpmgr-dv-' . $label . '-' . bin2hex(random_bytes(4));
    }

    /**
     * Recursively remove a scratch tree.
     *
     * @param string $dir Directory to remove.
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
                @chmod($path, 0666);
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @chmod($dir, 0777);
        rmdir($dir);
    }

    /**
     * Write a plugin directory with a parsable header.
     *
     * @param string $folder  Plugin folder name.
     * @param string $version Version in the header.
     * @param string $main    Main file name.
     * @return string Absolute plugin directory path.
     */
    private function makePlugin(string $folder, string $version, string $main = ''): string
    {
        $dir = $this->pluginsRoot . '/' . $folder;
        mkdir($dir, 0777, true);
        $this->created[] = $dir;
        $main            = $main !== '' ? $main : $folder . '.php';
        file_put_contents(
            $dir . '/' . $main,
            "<?php\n/**\n * Plugin Name: Test " . $folder . "\n * Version: " . $version . "\n */\n"
        );
        file_put_contents($dir . '/readme.txt', "=== Test ===\n");

        return $dir;
    }

    /**
     * Copy a directory, the way SnapshotManager's payload capture does.
     *
     * @param string $from Source directory.
     * @param string $to   Destination directory.
     * @return void
     */
    private function copyTree(string $from, string $to): void
    {
        mkdir($to, 0777, true);
        foreach (array_diff((array) scandir($from), ['.', '..']) as $entry) {
            $src = $from . '/' . $entry;
            $dst = $to . '/' . $entry;
            if (is_dir($src)) {
                $this->copyTree($src, $dst);
            } else {
                copy($src, $dst);
            }
        }
    }

    /**
     * Run the verifier against this test's plugins root.
     *
     * @param string $slug        Plugin slug.
     * @param string $fromVersion Pre-update version.
     * @param string $payloadDir  Payload directory or ''.
     * @return array{verdict:bool|null,detail:string,signals:array<int,string>}
     */
    private function verifyPlugin(string $slug, string $fromVersion, string $payloadDir = ''): array
    {
        return DestinationVerifier::verify('plugin', $slug, $fromVersion, $payloadDir);
    }

    // ---- the only true verdict ------------------------------------------

    public function test_an_intact_directory_at_the_pre_update_version_verifies_true(): void
    {
        $folder  = $this->uniqueFolder('intact');
        $dir     = $this->makePlugin($folder, '1.2.3');
        $payload = $this->root . '/payload';
        $this->copyTree($dir, $payload);

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.2.3', $payload);

        $this->assertTrue($out['verdict'], $out['detail']);
        $this->assertContains('R4-header-matches', $out['signals']);
    }

    // ---- provably wrong: false ------------------------------------------

    /** R1, the destroyed-destination catch. */
    public function test_an_absent_directory_under_a_listable_root_is_false(): void
    {
        $folder = $this->uniqueFolder('absent');

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.2.3');

        $this->assertFalse($out['verdict']);
        $this->assertContains('R1-absent', $out['signals']);
    }

    /** R2. */
    public function test_an_empty_directory_is_false(): void
    {
        $folder = $this->uniqueFolder('empty');
        $dir    = $this->pluginsRoot . '/' . $folder;
        mkdir($dir, 0777, true);
        $this->created[] = $dir;

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.2.3');

        $this->assertFalse($out['verdict']);
        $this->assertContains('R2-empty', $out['signals']);
    }

    /** R4, the truncated half-write. */
    public function test_a_main_file_whose_header_does_not_parse_is_false(): void
    {
        $folder = $this->uniqueFolder('truncated');
        $dir    = $this->makePlugin($folder, '1.2.3');
        file_put_contents($dir . '/' . $folder . '.php', "<?php\n/**\n * Plug");

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.2.3');

        $this->assertFalse($out['verdict']);
        $this->assertContains('R4-header-unparseable', $out['signals']);
    }

    /** R4, the completed-swap catch: any version movement at all condemns. */
    public function test_a_version_that_already_moved_is_false(): void
    {
        $folder = $this->uniqueFolder('moved');
        $this->makePlugin($folder, '2.0.0');

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.2.3');

        $this->assertFalse($out['verdict']);
        $this->assertContains('R4-version-moved', $out['signals']);
    }

    /** R5: a top-level entry that existed before the update is gone now. */
    public function test_a_missing_payload_entry_is_false(): void
    {
        $folder  = $this->uniqueFolder('missing');
        $dir     = $this->makePlugin($folder, '1.0.0');
        $payload = $this->root . '/payload';
        $this->copyTree($dir, $payload);
        unlink($dir . '/readme.txt'); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture mutation

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0', $payload);

        $this->assertFalse($out['verdict']);
        $this->assertContains('R5-entry-missing', $out['signals']);
    }

    /**
     * An EXTRA live entry is not evidence of anything: core has no mechanism
     * that adds a single top-level file, while a plugin writing its own cache
     * file between the capture and this check is completely normal. Without
     * this asymmetry the verifier would report every such plugin as modified.
     */
    public function test_an_extra_live_entry_is_not_evidence(): void
    {
        $folder  = $this->uniqueFolder('extra');
        $dir     = $this->makePlugin($folder, '1.0.0');
        $payload = $this->root . '/payload';
        $this->copyTree($dir, $payload);
        file_put_contents($dir . '/cache.json', '{}');

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0', $payload);

        $this->assertTrue($out['verdict'], $out['detail']);
    }

    /** R6, size only. */
    public function test_a_main_file_whose_size_changed_is_false(): void
    {
        $folder  = $this->uniqueFolder('resized');
        $dir     = $this->makePlugin($folder, '1.0.0');
        $payload = $this->root . '/payload';
        $this->copyTree($dir, $payload);
        file_put_contents(
            $dir . '/' . $folder . '.php',
            "<?php\n/**\n * Plugin Name: Test\n * Version: 1.0.0\n */\n// padding padding padding\n"
        );

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0', $payload);

        $this->assertFalse($out['verdict']);
        $this->assertContains('R6-size-differs', $out['signals']);
    }

    /**
     * MTIME MUST NEVER BE COMPARED. The snapshot payload is built with plain
     * file copies, which do not preserve mtimes, so an mtime comparison would
     * report every single capture as modified.
     */
    public function test_mtime_is_never_compared(): void
    {
        $folder  = $this->uniqueFolder('mtime');
        $dir     = $this->makePlugin($folder, '1.0.0');
        $payload = $this->root . '/payload';
        $this->copyTree($dir, $payload);
        touch($payload . '/' . $folder . '.php', time() - 86400);
        touch($dir . '/' . $folder . '.php', time());

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0', $payload);

        $this->assertTrue($out['verdict'], 'an mtime difference must not affect the verdict');
    }

    // ---- cannot answer: null --------------------------------------------

    /**
     * R0: an unlistable ROOT is the open_basedir class this verifier exists to
     * be gentle with, and it must never condemn. Once the root DOES list, a
     * missing child is genuinely missing, because open_basedir is prefix-based
     * and cannot hide one entry of a directory it lets you read.
     */
    public function test_an_unlistable_root_is_null_not_false(): void
    {
        $folder = $this->uniqueFolder('rootless');
        $this->skipWhenChmodDoesNotBlockReads();

        chmod($this->pluginsRoot, 0000);
        try {
            $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0');
        } finally {
            chmod($this->pluginsRoot, 0777);
        }

        $this->assertNull($out['verdict']);
        $this->assertContains('R0-root-unlistable', $out['signals']);
    }

    /**
     * R2: presence in the parent listing is positive evidence of existence, so
     * a child we cannot read is inconclusive, never destroyed.
     */
    public function test_a_present_but_unlistable_directory_is_null_not_false(): void
    {
        $this->skipWhenChmodDoesNotBlockReads();

        $folder = $this->uniqueFolder('locked');
        $dir    = $this->makePlugin($folder, '1.0.0');

        chmod($dir, 0000);
        try {
            $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0');
        } finally {
            chmod($dir, 0777);
        }

        $this->assertNull($out['verdict']);
        $this->assertContains('R2-unlistable', $out['signals']);
    }

    /** R3: a guessed main file that misses must be null, never false. */
    public function test_an_unresolvable_main_file_is_null_not_false(): void
    {
        $folder = $this->uniqueFolder('ambiguous');
        $dir    = $this->pluginsRoot . '/' . $folder;
        mkdir($dir, 0777, true);
        $this->created[] = $dir;
        file_put_contents($dir . '/one.php', "<?php\n");
        file_put_contents($dir . '/two.php', "<?php\n");

        $out = $this->verifyPlugin($folder, '1.0.0');

        $this->assertNull($out['verdict']);
        $this->assertContains('R3-unresolved', $out['signals']);
    }

    /**
     * An unconventional main file name IS resolvable when a snapshot exists,
     * because the payload is a copy of the pre-update directory.
     */
    public function test_an_unconventional_main_file_is_resolved_from_the_snapshot(): void
    {
        $folder  = $this->uniqueFolder('bedrock');
        $dir     = $this->makePlugin($folder, '3.1.0', 'loader.php');
        $payload = $this->root . '/payload';
        $this->copyTree($dir, $payload);

        $out = $this->verifyPlugin($folder, '3.1.0', $payload);

        $this->assertTrue($out['verdict'], $out['detail']);
    }

    /** R4: without a pre-update version there is nothing to match against. */
    public function test_an_empty_from_version_can_never_verify_true(): void
    {
        $folder = $this->uniqueFolder('noversion');
        $this->makePlugin($folder, '1.0.0');

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '');

        $this->assertNull($out['verdict']);
        $this->assertContains('R4-no-from-version', $out['signals']);
    }

    /**
     * R6 must return NULL, not FALSE, when the payload has no copy of the main
     * file's relative path. Snapshot capture skips symlinks, and on a Bedrock
     * or Composer layout a plain size inequality against a missing payload file
     * would be a spurious restore on every such host.
     */
    public function test_a_payload_without_a_copy_of_the_main_file_skips_the_size_check(): void
    {
        $folder  = $this->uniqueFolder('nopayloadcopy');
        $dir     = $this->makePlugin($folder, '1.0.0');
        $payload = $this->root . '/payload';
        mkdir($payload, 0777, true);

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0', $payload);

        $this->assertTrue($out['verdict'], $out['detail']);
        $this->assertContains('R6-skipped-no-payload-copy', $out['signals']);
    }

    /**
     * THE VERIFICATION-FAILS CASE. Anything thrown inside must produce null,
     * never false and never an escaped exception: the caller is already on a
     * failure path and a throw here would lose the whole item result.
     */
    public function test_a_throwing_header_reader_is_null_and_never_propagates(): void
    {
        $folder = $this->uniqueFolder('throwing');
        $this->makePlugin($folder, '1.0.0');

        Functions\when('get_file_data')->alias(function () {
            throw new \RuntimeException('filesystem exploded');
        });

        $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0');

        $this->assertNull($out['verdict']);
    }

    /**
     * R4: a main file that exists but cannot be READ is inconclusive, never
     * destroyed. A permissions problem is not evidence about content.
     */
    public function test_an_unreadable_main_file_is_null_not_false(): void
    {
        $this->skipWhenChmodDoesNotBlockReads();

        $folder = $this->uniqueFolder('unreadable');
        $dir    = $this->makePlugin($folder, '1.0.0');
        $main   = $dir . '/' . $folder . '.php';

        chmod($main, 0000);
        try {
            $out = $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0');
        } finally {
            chmod($main, 0666);
        }

        $this->assertNull($out['verdict']);
        $this->assertContains('R4-unreadable', $out['signals']);
    }

    /** A type this class does not understand answers null, never a guess. */
    public function test_an_unsupported_type_is_null(): void
    {
        $out = DestinationVerifier::verify('core', 'core', '6.0', '');

        $this->assertNull($out['verdict']);
    }

    /**
     * THE FALSE-TRUE THIS BACKSTOP MUST NEVER PRODUCE. get_plugins() is a
     * cached, full-directory scan with no clearstatcache(), so it can return
     * the PRE-apply version for a directory that has since been wiped. The
     * verifier must read the header off the file itself.
     */
    public function test_the_verifier_never_consults_get_plugins(): void
    {
        $folder = $this->uniqueFolder('nocache');
        $this->makePlugin($folder, '1.0.0');

        $called = false;
        Functions\when('get_plugins')->alias(function () use (&$called) {
            $called = true;
            return [];
        });

        $this->verifyPlugin($folder . '/' . $folder . '.php', '1.0.0');

        $this->assertFalse($called, 'the verifier must never read a cached plugin listing');
    }

    /**
     * chmod is advisory for root, so the two unlistable-directory cases are
     * skipped with an explicit message rather than silently passing when the
     * suite happens to run as root (a container default).
     *
     * @return void
     */
    private function skipWhenChmodDoesNotBlockReads(): void
    {
        $probe = $this->root . '/chmod-probe';
        mkdir($probe, 0777, true);
        chmod($probe, 0000);
        $blocked = @scandir($probe) === false;
        chmod($probe, 0777);
        rmdir($probe);

        if (!$blocked) {
            $this->markTestSkipped('this process can read a 0000 directory (running as root), so R0/R2 cannot be exercised');
        }
    }
}
