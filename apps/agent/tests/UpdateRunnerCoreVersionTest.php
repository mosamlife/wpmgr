<?php
/**
 * GitHub issue #415: a core update reports the version that is installed on
 * disk, not the version WordPress loaded into memory when the request began.
 *
 * WordPress loads $wp_version once per request. A core update that runs inside
 * that same request does not replace the global (update_core() reads the new
 * version file in function scope only), so get_bloginfo('version') keeps
 * answering with the version the site was updated FROM. These tests keep the
 * global and get_bloginfo('version') at the old version throughout, exactly as
 * WordPress does, and change only the file on disk.
 *
 * Every version read goes through the REAL UpdateRunner. The only thing
 * replaced is the upgrader call itself, by a subclass that does to
 * wp-includes/version.php what Core_Upgrader does to it.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateRunner
 * @covers \WPMgr\Agent\Commands\UpdateCommand
 */
final class UpdateRunnerCoreVersionTest extends TestCase
{
    /** The version WordPress loaded into memory when the request began. */
    private const IN_MEMORY = '7.0';

    /** Absolute path of <ABSPATH>/wp-includes. */
    private string $includesDir = '';

    /** Absolute path of <ABSPATH>/wp-includes/version.php. */
    private string $versionFile = '';

    /** Whether set_up() had to create wp-includes (so tear_down() removes it). */
    private bool $createdIncludesDir = false;

    /** Whether $GLOBALS['wp_version'] existed before this test. */
    private bool $hadGlobal = false;

    /** @var mixed The pre-test $GLOBALS['wp_version'], restored afterwards. */
    private $savedGlobal = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $abspath = rtrim((string) constant('ABSPATH'), '/\\');
        $this->assertNotSame('', $abspath, 'the test ABSPATH must be defined and non-empty');

        $this->includesDir = $abspath . '/wp-includes';
        $this->versionFile = $this->includesDir . '/version.php';
        if (!is_dir($this->includesDir)) {
            mkdir($this->includesDir, 0755, true);
            $this->createdIncludesDir = true;
        }
        $this->removeVersionFile();

        $this->hadGlobal       = array_key_exists('wp_version', $GLOBALS);
        $this->savedGlobal     = $GLOBALS['wp_version'] ?? null;
        $GLOBALS['wp_version'] = self::IN_MEMORY;

        // get_bloginfo('version') returns the global, as core's does
        // (general-template.php: `global $wp_version; $output = $wp_version;`).
        Functions\when('get_bloginfo')->alias(
            static fn ($show = '') => $show === 'version' ? $GLOBALS['wp_version'] : ''
        );
    }

    protected function tear_down(): void
    {
        $this->removeVersionFile();
        if ($this->createdIncludesDir && is_dir($this->includesDir)) {
            @rmdir($this->includesDir); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- test fixture cleanup; another test may have put files here since
        }

        if ($this->hadGlobal) {
            $GLOBALS['wp_version'] = $this->savedGlobal;
        } else {
            unset($GLOBALS['wp_version']);
        }

        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Write wp-includes/version.php with the given body.
     *
     * @param string $body Full file contents.
     */
    private function writeVersionFile(string $body): void
    {
        $this->removeVersionFile();
        file_put_contents($this->versionFile, $body); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test fixture
    }

    /**
     * The version.php WordPress ships, with the given version assigned.
     *
     * @param string $version Version string.
     * @return string
     */
    private static function coreVersionFileBody(string $version): string
    {
        return "<?php\n"
            . "/**\n"
            . " * WordPress Version\n"
            . " *\n"
            . " * @global string \$wp_version\n"
            . " */\n"
            . "\$wp_version = '" . $version . "';\n"
            . "\n"
            . "/**\n"
            . " * @global int \$wp_db_version\n"
            . " */\n"
            . "\$wp_db_version = 60717;\n"
            . "\n"
            . "\$required_php_version = '7.4';\n";
    }

    /**
     * Remove whatever this test put at the version.php path, file or directory.
     */
    private function removeVersionFile(): void
    {
        if ($this->versionFile === '') {
            return;
        }
        if (is_dir($this->versionFile)) {
            rmdir($this->versionFile);
        } elseif (file_exists($this->versionFile)) {
            unlink($this->versionFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }
    }

    /**
     * The real UpdateRunner, with only the upgrader call replaced. Like
     * Core_Upgrader, the fake apply rewrites wp-includes/version.php on disk
     * and leaves the in-memory $wp_version alone.
     *
     * @param string $versionFile  Path the fake upgrade writes.
     * @param string $installed    Version the fake upgrade installs.
     * @param bool   $applySucceeds Whether the fake upgrade reports success.
     * @return UpdateRunner
     */
    private static function runnerWhoseCoreUpgradeWrites(string $versionFile, string $installed, bool $applySucceeds = true): UpdateRunner
    {
        return new class ($versionFile, $installed, $applySucceeds) extends UpdateRunner {
            /** @var array<int,array{string,string,string}> */
            public array $upgraded = [];

            public function __construct(
                private string $versionFile,
                private string $installed,
                private bool $applySucceeds
            ) {
            }

            protected function applyViaUpgrader(string $type, string $slug, string $version): array
            {
                $this->upgraded[] = [$type, $slug, $version];

                if (!$this->applySucceeds) {
                    return [
                        'ok'                  => false,
                        'log'                 => 'Update failed.',
                        'destination_touched' => null,
                    ];
                }

                file_put_contents( // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test double standing in for Core_Upgrader
                    $this->versionFile,
                    "<?php\n\$wp_version = '" . $this->installed . "';\n"
                );

                return [
                    'ok'                  => true,
                    'log'                 => 'Update applied via upgrader.',
                    'destination_touched' => true,
                ];
            }
        };
    }

    /**
     * A snapshot store that is never expected to be touched for core (D3).
     */
    private static function untouchedSnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $captured = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = [$type, $slug, $fromVersion];

                return ['snapshot_id' => '', 'log' => ''];
            }
        };
    }

    // ---------------------------------------------------------------------
    // currentVersion('core')
    // ---------------------------------------------------------------------

    public function test_core_version_is_read_from_disk_not_from_the_in_memory_global(): void
    {
        $this->writeVersionFile(self::coreVersionFileBody('7.1'));

        $this->assertSame(self::IN_MEMORY, get_bloginfo('version'), 'precondition: memory still says 7.0');
        $this->assertSame('7.1', (new UpdateRunner())->currentVersion('core', 'core'));
    }

    public function test_development_and_release_candidate_versions_are_read_from_disk(): void
    {
        $runner = new UpdateRunner();

        foreach (['7.2-alpha-61234-src', '7.1-RC2', '7.1.1'] as $version) {
            $this->writeVersionFile(self::coreVersionFileBody($version));
            $this->assertSame($version, $runner->currentVersion('core', 'core'));
        }
    }

    public function test_a_missing_version_file_falls_back_to_the_in_memory_version(): void
    {
        $this->assertFileDoesNotExist($this->versionFile);

        $this->assertSame(self::IN_MEMORY, (new UpdateRunner())->currentVersion('core', 'core'));
    }

    public function test_an_unreadable_version_file_falls_back_to_the_in_memory_version(): void
    {
        // A directory where the file should be: every read of it fails.
        mkdir($this->versionFile, 0755);

        $this->assertSame(self::IN_MEMORY, (new UpdateRunner())->currentVersion('core', 'core'));
    }

    /**
     * Anything other than a plain, literal version assignment is ignored, and
     * the answer is the in-memory version. Never '' while that is known.
     */
    public function test_an_unparsable_version_file_falls_back_to_the_in_memory_version(): void
    {
        $runner = new UpdateRunner();

        $bodies = [
            'empty file'                 => '',
            'no assignment'              => "<?php\n\$wp_db_version = 60717;\n",
            'commented-out assignment'   => "<?php\n// \$wp_version = '9.9';\n",
            'assignment inside docblock' => "<?php\n/**\n * \$wp_version = '9.9';\n */\n",
            'computed value'             => "<?php\n\$wp_version = get_option('wpmgr_x');\n",
            'path-like value'            => "<?php\n\$wp_version = '../../etc/passwd';\n",
            'value with a space'         => "<?php\n\$wp_version = '7.1 --activate';\n",
            'empty value'                => "<?php\n\$wp_version = '';\n",
            'unterminated quote'         => "<?php\n\$wp_version = '7.1\";\n",
        ];

        foreach ($bodies as $label => $body) {
            $this->writeVersionFile($body);
            $this->assertSame(self::IN_MEMORY, $runner->currentVersion('core', 'core'), $label);
        }
    }

    public function test_a_commented_out_assignment_never_shadows_the_real_one(): void
    {
        $this->writeVersionFile("<?php\n// \$wp_version = '9.9';\n\$wp_version = '7.1';\n");

        $this->assertSame('7.1', (new UpdateRunner())->currentVersion('core', 'core'));
    }

    // ---------------------------------------------------------------------
    // The reported bug, end to end through UpdateCommand::execute()
    // ---------------------------------------------------------------------

    /**
     * Issue #415 as reported: WordPress was updated to 7.1, but the run said
     * "already up to date" and showed the old version as the result.
     */
    public function test_a_real_core_update_reports_succeeded_with_the_installed_version(): void
    {
        $this->writeVersionFile(self::coreVersionFileBody(self::IN_MEMORY));

        $runner    = self::runnerWhoseCoreUpgradeWrites($this->versionFile, '7.1');
        $snapshots = self::untouchedSnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'core', 'slug' => 'core', 'version' => '7.1']],
        ]);

        $this->assertSame([['core', 'core', '7.1']], $runner->upgraded, 'the upgrader ran exactly once');
        $this->assertSame(self::IN_MEMORY, get_bloginfo('version'), 'memory still says 7.0, as it does in WordPress');

        $r = $out['results'][0];
        $this->assertSame('succeeded', $r['status']);
        $this->assertSame(self::IN_MEMORY, $r['from_version']);
        $this->assertSame('7.1', $r['to_version']);
        $this->assertSame([], $snapshots->captured, 'core snapshots stay opt-in (D3)');
    }

    public function test_a_failed_core_update_still_reports_the_version_on_disk(): void
    {
        $this->writeVersionFile(self::coreVersionFileBody(self::IN_MEMORY));

        $runner = self::runnerWhoseCoreUpgradeWrites($this->versionFile, '7.1', false);
        $cmd    = new UpdateCommand(self::untouchedSnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'core', 'slug' => 'core', 'version' => '7.1']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('failed', $r['status']);
        $this->assertSame(self::IN_MEMORY, $r['from_version']);
        $this->assertSame(self::IN_MEMORY, $r['to_version']);
    }
}
