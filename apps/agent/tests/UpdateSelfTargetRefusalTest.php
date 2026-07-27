<?php
/**
 * Tests for the self-target refusal shared by the update and rollback
 * commands: a control-plane task may never point at the agent's own plugin.
 *
 * Applying an update to the agent from inside a control-plane command would
 * delete and re-copy the directory that is running the request and still has
 * to serialize its response, with the rollback watchdog unavailable by design,
 * so the control plane could not tell success from a bricked site. These tests
 * pin the refusal itself (by plugin key and by a directory that resolves to
 * WPMGR_AGENT_DIR), that it costs an ordinary third-party update nothing, and
 * that the refused path starts no filesystem work at all.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\UpdateCommand
 * @covers \WPMgr\Agent\Commands\RollbackCommand
 */
final class UpdateSelfTargetRefusalTest extends TestCase
{
    /** The agent's own plugin key in a stock install. */
    private const SELF_KEY = 'wpmgr-agent/wpmgr-agent.php';

    /** Absolute path to the `.maintenance` marker under the test ABSPATH. */
    private string $maintenanceFile = '';

    /** @var array<int,string> Paths created by a test, removed on teardown. */
    private array $cleanup = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $abspath = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
        if ($abspath !== '' && !is_dir($abspath)) {
            mkdir($abspath, 0755, true);
        }
        $this->maintenanceFile = $abspath . '/.maintenance';
        if (file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile);
        }

        // See RollbackCommandTest: once ANY test in the process stubs this,
        // function_exists() is true for every later test, so stub it here too
        // rather than per-test.
        Functions\when('delete_site_transient')->justReturn(true);
    }

    protected function tear_down(): void
    {
        foreach ($this->cleanup as $path) {
            if (is_link($path) || is_file($path)) {
                unlink($path);
            } elseif (is_dir($path)) {
                $this->rrmdir($path);
            }
        }
        $this->cleanup = [];

        if ($this->maintenanceFile !== '' && file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // =====================================================================
    // Doubles
    // =====================================================================

    /**
     * Runner double that records every call. Its apply() also writes a
     * sentinel file, so a test can prove on disk (not only by call record)
     * that no apply was ever started.
     */
    private function recordingRunner(string $sentinelPath = ''): UpdateRunner
    {
        return new class ($sentinelPath) extends UpdateRunner {
            /** @var array<int,string> Every call, as "method:type:slug". */
            public array $calls = [];
            /** @var array<int,array{string,string,string}> */
            public array $applied = [];
            /** @var array<string,string> */
            public array $versions = [];
            /** @var array<string,string> */
            public array $available = [];

            public function __construct(private string $sentinelPath)
            {
            }

            public function currentVersion(string $type, string $slug): string
            {
                $this->calls[] = 'currentVersion:' . $type . ':' . $slug;

                return $this->versions[$type . ':' . $slug] ?? '';
            }

            public function isInstalled(string $type, string $slug): bool
            {
                $this->calls[] = 'isInstalled:' . $type . ':' . $slug;

                return true;
            }

            public function availableVersion(string $type, string $slug, string $requested): ?string
            {
                $this->calls[] = 'availableVersion:' . $type . ':' . $slug;

                return $this->available[$type . ':' . $slug] ?? ($requested !== 'latest' ? $requested : '');
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->calls[]   = 'apply:' . $type . ':' . $slug;
                $this->applied[] = [$type, $slug, $version];

                if ($this->sentinelPath !== '') {
                    file_put_contents($this->sentinelPath, 'applied ' . $type . ':' . $slug);
                }

                $this->versions[$type . ':' . $slug] = $version === 'latest' ? '9.9.9' : $version;

                return ['ok' => true, 'log' => 'applied'];
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                $this->calls[] = 'isComplete:' . $type . ':' . $slug;

                return true;
            }

            public function lastIncompleteReason(): string
            {
                return '';
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };
    }

    /**
     * Snapshot double that records capture/restore/cleanup and touches no
     * filesystem of its own.
     */
    private function recordingSnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $captured = [];
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];
            /** @var array<int,string> */
            public array $cleaned = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = [$type, $slug, $fromVersion];

                return ['snapshot_id' => 'snap_selftarget', 'log' => 'captured'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => true, 'log' => 'restored'];
            }

            public function snapshotExists(string $snapshotId): bool
            {
                return true;
            }

            public function recordedVersion(string $snapshotId): string
            {
                return '1.0.0';
            }

            public function cleanup(string $snapshotId): bool
            {
                $this->cleaned[] = $snapshotId;

                return true;
            }

            public function markSucceeded(string $snapshotId): void
            {
            }
        };
    }

    // =====================================================================
    // UpdateCommand
    // =====================================================================

    public function test_update_refuses_the_agent_by_its_exact_plugin_key(): void
    {
        $runner = $this->recordingRunner();
        // A genuinely pending update for the agent itself: without the
        // refusal this item reaches snapshot + apply, which is the whole
        // hazard (the running code overwriting itself mid-request).
        $runner->versions['plugin:' . self::SELF_KEY]  = '1.0.0';
        $runner->available['plugin:' . self::SELF_KEY] = '2.0.0';

        $snapshots = $this->recordingSnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => self::SELF_KEY, 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'skipped',
            $r['status'],
            'an update task aimed at the agent itself must be refused, using the existing skipped status'
        );
        $this->assertStringContainsString(
            'own update channel',
            $r['log'],
            'the refusal must tell the operator the agent updates through its own channel'
        );
        $this->assertSame([], $runner->applied, 'no apply may be started for the agent itself');
        $this->assertSame([], $snapshots->captured, 'no snapshot may be captured for the agent itself');
        $this->assertSame(
            [],
            $runner->calls,
            'the refusal must land before any detection call, not after the item has been probed'
        );
        $this->assertTrue($out['ok'], 'a refusal is not a failure');
    }

    public function test_update_refuses_the_agent_by_its_bare_folder_slug(): void
    {
        $runner = $this->recordingRunner();
        $runner->versions['plugin:wpmgr-agent']  = '1.0.0';
        $runner->available['plugin:wpmgr-agent'] = '2.0.0';

        $cmd = new UpdateCommand($this->recordingSnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'wpmgr-agent', 'version' => 'latest']],
        ]);

        $this->assertSame('skipped', $out['results'][0]['status']);
        $this->assertSame([], $runner->applied);
    }

    /**
     * A signed command that was forged or replayed carries whatever casing its
     * author chose, and a case-insensitive filesystem (macOS, Windows, a
     * casefolded or SMB/NFS-backed wp-content) resolves that casing to the
     * agent's own directory. The agent is the only guard left at that point,
     * so it must refuse the mixed-case form exactly as it refuses the stock
     * one. Pinned as an end-to-end refusal, not only at the matcher, so a
     * future change cannot fold case in the matcher and lose it at the command
     * boundary.
     */
    public function test_update_refuses_the_agent_by_a_mixed_case_plugin_key(): void
    {
        $slug = 'WPMGR-Agent/WPMGR-Agent.php';

        $runner = $this->recordingRunner();
        $runner->versions['plugin:' . $slug]  = '1.0.0';
        $runner->available['plugin:' . $slug] = '2.0.0';

        $snapshots = $this->recordingSnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $slug, 'version' => 'latest']],
        ]);

        $this->assertSame(
            'skipped',
            $out['results'][0]['status'],
            'a mixed-case spelling of the agent key must be refused, not applied'
        );
        $this->assertSame([], $runner->applied, 'no apply may be started for the agent itself');
        $this->assertSame([], $snapshots->captured, 'no snapshot may be captured for the agent itself');
        $this->assertSame([], $runner->calls, 'the refusal must land before any detection call');
    }

    /**
     * The install directory can legitimately carry a different name (a
     * renamed folder, or a symlink into the plugins tree). Matching only the
     * stock key would let exactly that install update itself, so the refusal
     * must also match any slug whose directory RESOLVES to WPMGR_AGENT_DIR.
     */
    public function test_update_refuses_a_slug_whose_directory_resolves_to_the_agent_directory(): void
    {
        $link = $this->linkToAgentDirInPluginRoot();

        $runner = $this->recordingRunner();
        $runner->versions['plugin:' . $link]  = '1.0.0';
        $runner->available['plugin:' . $link] = '2.0.0';

        $cmd = new UpdateCommand($this->recordingSnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $link, 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'skipped',
            $r['status'],
            'a differently-named install directory that resolves to the agent must be refused too'
        );
        $this->assertSame([], $runner->applied, 'no apply may be started for the agent itself');
        $this->assertSame([], $runner->calls, 'the refusal must land before any detection call');
    }

    /**
     * The dry-run/preview path can also originate an apply on the control
     * plane's next step, so it must refuse identically rather than reporting
     * the agent as updatable.
     */
    public function test_dry_run_refuses_the_agent_instead_of_reporting_it_updatable(): void
    {
        $runner = $this->recordingRunner();
        $runner->versions['plugin:' . self::SELF_KEY]  = '1.0.0';
        $runner->available['plugin:' . self::SELF_KEY] = '2.0.0';

        $cmd = new UpdateCommand($this->recordingSnapshots(), $runner);

        $out = $cmd->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => self::SELF_KEY, 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('skipped', $r['status']);
        $this->assertNotSame(
            'would_update',
            $r['status'],
            'a preview must never advertise the agent as an updatable target'
        );
    }

    /**
     * The gate must be narrow: an ordinary third-party plugin still updates
     * exactly as before, in the same batch as a refused agent item.
     */
    public function test_a_third_party_plugin_update_is_unaffected(): void
    {
        $runner = $this->recordingRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        $runner->versions['theme:twentytwentyfour']      = '1.0';
        $runner->available['theme:twentytwentyfour']     = '1.1';
        $runner->versions['plugin:' . self::SELF_KEY]    = '1.0.0';
        $runner->available['plugin:' . self::SELF_KEY]   = '2.0.0';

        $snapshots = $this->recordingSnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [
                ['type' => 'plugin', 'slug' => self::SELF_KEY, 'version' => 'latest'],
                ['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest'],
                ['type' => 'theme', 'slug' => 'twentytwentyfour', 'version' => 'latest'],
            ],
        ]);

        $this->assertSame('skipped', $out['results'][0]['status'], 'the agent item is refused');
        $this->assertSame('succeeded', $out['results'][1]['status'], 'a third-party plugin still updates');
        $this->assertSame('succeeded', $out['results'][2]['status'], 'an ordinary theme still updates');
        $this->assertSame(
            [
                ['plugin', 'akismet/akismet.php', 'latest'],
                ['theme', 'twentytwentyfour', 'latest'],
            ],
            $runner->applied,
            'exactly the non-agent items may be applied'
        );
        $this->assertSame(
            [
                ['plugin', 'akismet/akismet.php', '5.0'],
                ['theme', 'twentytwentyfour', '1.0'],
            ],
            $snapshots->captured,
            'only the non-agent items may be snapshotted'
        );
    }

    /**
     * Nothing at all may be written for a refused item: no snapshot, no
     * in-flight marker, no upgrader work, and no change to the agent's own
     * directory. Asserted on the filesystem (an apply sentinel, an empty
     * uploads tree, an unchanged agent directory listing), not only on the
     * doubles' call records.
     */
    public function test_a_refused_self_target_writes_nothing_to_disk(): void
    {
        $tmp = sys_get_temp_dir() . '/wpmgr-selftarget-' . bin2hex(random_bytes(6));
        mkdir($tmp, 0755, true);
        $this->cleanup[] = $tmp;
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $tmp]);

        $sentinel = $tmp . '/apply-sentinel.txt';
        $before   = $this->agentDirFingerprint();

        $runner = $this->recordingRunner($sentinel);
        $runner->versions['plugin:' . self::SELF_KEY]  = '1.0.0';
        $runner->available['plugin:' . self::SELF_KEY] = '2.0.0';

        $snapshots = $this->recordingSnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => self::SELF_KEY, 'version' => 'latest']],
        ]);

        // Disk first: this test is about what reached the filesystem, so the
        // filesystem assertions are the ones that must speak first.
        $this->assertFileDoesNotExist($sentinel, 'the upgrader must never be reached for the agent itself');
        $this->assertSame(
            [],
            $this->filesUnder($tmp),
            'a refused item must leave no snapshot, marker or other file behind'
        );
        $this->assertSame(
            $before,
            $this->agentDirFingerprint(),
            "the agent's own directory must be untouched by a refused update"
        );
        $this->assertSame('skipped', $out['results'][0]['status']);
    }

    /**
     * Defense in depth: the runner is today reachable only through the gated
     * command boundary, but it must refuse a self-target on its own so a
     * future apply origin cannot reintroduce the hazard. Exercised against
     * the REAL UpdateRunner, which must decide this before it consults
     * WP-CLI or any upgrader API.
     */
    public function test_the_update_runner_itself_refuses_a_self_target_as_a_backstop(): void
    {
        $result = (new UpdateRunner())->apply('plugin', self::SELF_KEY, 'latest');

        $this->assertFalse($result['ok'], 'the runner must refuse to apply an update to the agent itself');
        $this->assertStringContainsString('does not update itself', $result['log']);
    }

    // =====================================================================
    // RollbackCommand (the sibling path that also replaces a live directory)
    // =====================================================================

    public function test_rollback_refuses_the_agent_by_its_exact_plugin_key(): void
    {
        $snapshots = $this->recordingSnapshots();
        $cmd       = new RollbackCommand($snapshots, $this->recordingRunner());

        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => self::SELF_KEY,
            'snapshot_id' => 'snap_selftarget',
        ]);

        $this->assertFalse($out['ok'], 'a rollback aimed at the agent itself must be refused');
        $this->assertStringContainsString('own update channel', $out['log']);
        $this->assertSame([], $snapshots->restored, 'the live agent directory must never be replaced');
        $this->assertSame([], $snapshots->cleaned);
    }

    public function test_rollback_refuses_a_slug_whose_directory_resolves_to_the_agent_directory(): void
    {
        $link = $this->linkToAgentDirInPluginRoot();

        $snapshots = $this->recordingSnapshots();
        $cmd       = new RollbackCommand($snapshots, $this->recordingRunner());

        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => $link,
            'snapshot_id' => 'snap_selftarget',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame([], $snapshots->restored, 'the live agent directory must never be replaced');
    }

    public function test_rollback_refuses_the_agent_by_a_mixed_case_plugin_key(): void
    {
        $snapshots = $this->recordingSnapshots();
        $cmd       = new RollbackCommand($snapshots, $this->recordingRunner());

        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => 'WPMGR-Agent/WPMGR-Agent.php',
            'snapshot_id' => 'snap_selftarget',
        ]);

        $this->assertFalse($out['ok'], 'a mixed-case rollback target that is the agent must be refused');
        $this->assertStringContainsString('own update channel', $out['log']);
        $this->assertSame([], $snapshots->restored, 'the live agent directory must never be replaced');
    }

    public function test_a_third_party_rollback_is_unaffected(): void
    {
        $snapshots = $this->recordingSnapshots();
        $runner    = $this->recordingRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $cmd = new RollbackCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_third_party',
        ]);

        $this->assertTrue($out['ok'], 'an ordinary plugin rollback must still run');
        $this->assertSame([['plugin', 'akismet/akismet.php', 'snap_third_party']], $snapshots->restored);
        $this->assertSame('5.0', $out['restored_version']);
    }

    // =====================================================================
    // The matcher itself (shared by both commands and by UpdateRunner)
    // =====================================================================

    /**
     * The control plane folds case in its own matcher
     * (apps/api/internal/agentplugin) precisely because a hand-built payload
     * can spell the slug any way it likes. The agent is the guard that still
     * stands when a signed command has been forged or replayed, so it may not
     * be the weaker of the two: every casing of the stock key and folder is
     * pinned here directly against the matcher.
     */
    public function test_the_matcher_folds_case_for_the_stock_key_and_folder(): void
    {
        $variants = [
            'wpmgr-agent/wpmgr-agent.php',
            'WPMGR-Agent/WPMGR-Agent.php',
            'WPMgr-Agent/wpmgr-agent.php',
            'wpmgr-agent/WPMGR-AGENT.PHP',
            'WPMGR-AGENT',
            'WpMgr-Agent',
        ];

        foreach ($variants as $slug) {
            $this->assertTrue(
                UpdateCommand::isSelfTarget('plugin', $slug),
                sprintf('"%s" spells the agent\'s own plugin and must be refused', $slug)
            );
        }
    }

    /**
     * Folding case must not widen the gate: a plugin that merely resembles the
     * agent's slug is a different plugin and still updates.
     */
    public function test_the_matcher_still_admits_plugins_that_only_resemble_the_agent(): void
    {
        $allowed = [
            'akismet/akismet.php',
            'wpmgr-agentx/wpmgr-agentx.php',
            'my-wpmgr-agent',
            'wpmgr-agents',
            'wpmgr_agent',
        ];

        foreach ($allowed as $slug) {
            $this->assertFalse(
                UpdateCommand::isSelfTarget('plugin', $slug),
                sprintf('"%s" is a third-party plugin and must keep updating', $slug)
            );
        }
    }

    /**
     * The second plugin signal is the agent's ACTUAL directory name, which
     * carries an install renamed off the stock folder. It has to fold case
     * too: on a case-insensitive filesystem the differently-cased spelling
     * names that very directory.
     */
    public function test_the_matcher_folds_case_for_the_agent_directory_name(): void
    {
        $this->assertTrue(defined('WPMGR_AGENT_DIR'), 'precondition: the test bootstrap defines WPMGR_AGENT_DIR');

        $dirName = basename(rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\'));
        $shouted = strtoupper($dirName);
        $this->assertNotSame($shouted, $dirName, 'precondition: the agent directory name contains letters to fold');

        $this->assertTrue(
            UpdateCommand::isSelfTarget('plugin', $dirName),
            "the agent's own directory name is a self-target"
        );
        $this->assertTrue(
            UpdateCommand::isSelfTarget('plugin', $shouted),
            "a differently-cased spelling of the agent's own directory name is the same directory"
        );
    }

    // =====================================================================
    // Helpers
    // =====================================================================

    /**
     * Create a symlink inside the plugin root that points at the agent's own
     * directory, and return its slug. This models an install reached under a
     * name other than the stock folder: only path resolution can recognise it.
     */
    private function linkToAgentDirInPluginRoot(): string
    {
        $this->assertTrue(defined('WPMGR_AGENT_DIR'), 'precondition: the test bootstrap defines WPMGR_AGENT_DIR');

        // Guard-define the plugin-root constants (another test file in this
        // suite may already have frozen them) using the same idiom as
        // SnapshotManagerTest::ensurePluginRootConstants().
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins');
        }
        $pluginRoot = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');
        if (!is_dir($pluginRoot)) {
            mkdir($pluginRoot, 0755, true);
        }

        $slug = 'renamed-agent-' . bin2hex(random_bytes(4));
        $link = $pluginRoot . '/' . $slug;
        $this->assertTrue(
            symlink(rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\'), $link),
            'precondition: the fixture symlink into the plugin root must be creatable'
        );
        $this->cleanup[] = $link;

        return $slug;
    }

    /**
     * Depth-one listing of the agent's own directory with each entry's size
     * and mtime, so any write into it shows up as a changed fingerprint.
     *
     * @return array<string,string>
     */
    private function agentDirFingerprint(): array
    {
        $dir = rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\');
        clearstatcache();

        $out = [];
        foreach (scandir($dir) ?: [] as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $path        = $dir . '/' . $entry;
            $out[$entry] = (string) filesize($path) . ':' . (string) filemtime($path);
        }
        ksort($out);

        return $out;
    }

    /**
     * Every FILE (directories ignored) under a root, relative and sorted.
     *
     * @param string $root Absolute directory.
     * @return array<int,string>
     */
    private function filesUnder(string $root): array
    {
        if (!is_dir($root)) {
            return [];
        }

        $found    = [];
        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($root, \FilesystemIterator::SKIP_DOTS)
        );
        foreach ($iterator as $file) {
            if ($file->isFile()) {
                $found[] = substr($file->getPathname(), strlen($root) + 1);
            }
        }
        sort($found);

        return $found;
    }

    /** Recursively remove a directory tree created by a test. */
    private function rrmdir(string $dir): void
    {
        foreach (scandir($dir) ?: [] as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $path = $dir . '/' . $entry;
            if (is_link($path) || is_file($path)) {
                unlink($path);
            } elseif (is_dir($path)) {
                $this->rrmdir($path);
            }
        }
        rmdir($dir);
    }
}
