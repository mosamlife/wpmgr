<?php
/**
 * THE wp.org-build lock for the agent self-update command.
 *
 * `make agent-zip-wporg` physically excludes
 * includes/support/class-update-checker.php from the distribution build
 * (Guideline 8), but includes/commands/class-agent-self-update-command.php
 * SURVIVES that rsync. So in the wp.org build the command file exists in a tree
 * where the UpdateChecker class does not. If that file named the class at file
 * scope (a `use` import, a typed property, a typed parameter), the plugin's
 * autoloader would go looking for a file that is not there, and the command
 * would fatal instead of answering.
 *
 * The correct answer in that build is a clean "not_eligible".
 *
 * This cannot be proven in-process: the PHPUnit process loads the whole plugin
 * through Composer's classmap, so UpdateChecker always exists there. Both tests
 * below therefore spawn a disposable child process that loads ONLY the command
 * file and its interface: no Composer autoloader, no plugin bootstrap, and
 * critically no UpdateChecker anywhere on disk from that process's point of
 * view. The child asserts the class really is absent, then calls the command
 * and reports what it got as JSON. Same disposable-subprocess idiom as
 * MetadataCommandTest's open_basedir probe.
 *
 * The static companions go further than the command file. includes/class-plugin.php
 * also survives the rsync and is the ONLY surviving file that names the
 * self-updater in executable code (`new UpdateChecker(...)`), which is safe
 * only because it sits inside a guard. That naming is checked here with the
 * same analyser the agent-zip-wporg build-time assertion runs against the
 * staged artifact.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Tools\WporgUpdateCheckerGuard;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AgentSelfUpdateCommand
 * @covers \WPMgr\Agent\Plugin
 */
final class AgentSelfUpdateWporgBuildTest extends TestCase
{
    /**
     * The wp.org build: WPMGR_WPORG_BUILD is defined true (exactly as
     * agent-zip-wporg stamps the staged main file) AND the self-updater source
     * file is absent. The command must load and answer "not_eligible".
     */
    public function test_command_answers_not_eligible_in_a_tree_without_the_self_updater(): void
    {
        $result = $this->runProbe(true);

        $this->assertSame('agent_self_update', $result['name']);
        $this->assertSame('not_eligible', $result['result']['status']);
        $this->assertTrue($result['result']['ok']);
        $this->assertSame('', $result['result']['to_version']);
    }

    /**
     * The same tree WITHOUT the constant: a hand-assembled or partially-copied
     * install that carries the command but not the self-updater must still
     * answer rather than fatal. This is the case the lazy, string-keyed
     * class_exists() resolution inside execute() exists for. The constant
     * guard alone would not cover it.
     */
    public function test_command_answers_not_eligible_when_only_the_self_updater_is_missing(): void
    {
        $result = $this->runProbe(false);

        $this->assertSame('not_eligible', $result['result']['status']);
        $this->assertTrue($result['result']['ok']);
    }

    /**
     * Static companion to the runtime probes: the command source must name no
     * UpdateChecker symbol at file scope. A future edit that adds a `use`
     * import or a concrete type hint would still pass the probes today (the
     * probe process registers no autoloader at all, so an unresolved symbol
     * never gets looked up) but would fatal in the real wp.org build, where the
     * plugin's own autoloader IS registered.
     */
    public function test_command_source_names_no_self_updater_symbol_at_file_scope(): void
    {
        $source = (string) file_get_contents(
            dirname(__DIR__) . '/includes/commands/class-agent-self-update-command.php'
        );

        // Strip comments and doc blocks: the file legitimately EXPLAINS the rule
        // in prose, and prose is never resolved by an autoloader.
        $code = '';
        foreach (token_get_all($source) as $token) {
            if (is_array($token) && ($token[0] === T_COMMENT || $token[0] === T_DOC_COMMENT)) {
                continue;
            }
            $code .= is_array($token) ? $token[1] : $token;
        }

        // The only permitted occurrence is inside the single-quoted, escaped
        // class-name STRING constant, which no autoloader ever sees.
        $code = str_replace("'WPMgr\\\\Agent\\\\Support\\\\UpdateChecker'", "''", $code);

        $this->assertStringNotContainsString(
            'UpdateChecker',
            $code,
            'class-agent-self-update-command.php ships in the wp.org build, where the self-updater does not exist;'
            . ' it must resolve that symbol lazily by string, never name it in executable code'
        );
    }

    /**
     * THE regression this file exists to prevent, checked on the file that
     * actually carries the risk.
     *
     * The test above inspects class-agent-self-update-command.php, which by
     * design names the self-updater nowhere, so it can never fail on the real
     * hazard. includes/class-plugin.php is the file that does name it in
     * executable code:
     *
     *   new UpdateChecker(...)          inside a WPMGR_WPORG_BUILD guard
     *
     * class-plugin.php SURVIVES the wp.org rsync while
     * includes/support/class-update-checker.php does not, so that line, if
     * dedented out of its guard, is a hard class fetch that raises
     * "Class WPMgr\Agent\Support\UpdateChecker not found" on every request of
     * every wp.org install. It sits among roughly a dozen visually identical
     * unconditional constructor calls, which makes that dedent a plausible
     * refactor rather than an exotic one.
     *
     * Shares its analyser with the build-time assertion the agent-zip-wporg
     * target runs against the staged artifact, so the source tree and the
     * shipped tree are judged by one implementation.
     */
    public function test_plugin_source_resolves_no_self_updater_symbol_outside_a_wporg_guard(): void
    {
        require_once dirname(__DIR__) . '/tools/assert-wporg-updatechecker-guard.php';

        $pluginFile = dirname(__DIR__) . '/includes/class-plugin.php';
        $report     = WporgUpdateCheckerGuard::analyse((string) file_get_contents($pluginFile));

        $rendered = '';
        foreach ($report['violations'] as $violation) {
            $rendered .= "\n  class-plugin.php:{$violation['line']}  {$violation['context']}";
        }

        $this->assertSame(
            [],
            $report['violations'],
            'class-plugin.php ships in the wp.org build, where the self-updater does not exist. Every executable'
            . ' naming of that class must stay inside a WPMGR_WPORG_BUILD or updateChecker-not-null guard, or the'
            . ' plugin fatals on every request of every wp.org install. Unguarded:' . $rendered
        );

        // Guard against the assertion going vacuous: if class-plugin.php stops
        // naming the class in executable code at all, the check above passes
        // for the wrong reason.
        $this->assertGreaterThanOrEqual(
            1,
            $report['resolving'],
            'class-plugin.php no longer resolves the self-updater anywhere, so this test proves nothing;'
            . ' if the self-update wiring moved, move this assertion with it'
        );
    }

    /**
     * Same rule, applied to every source file that survives the wp.org rsync.
     * The hazard is not specific to class-plugin.php: any file the build keeps
     * can introduce it. Only includes/support/class-update-checker.php is
     * exempt, because the build removes that file outright.
     */
    public function test_no_surviving_source_file_resolves_the_self_updater_outside_a_guard(): void
    {
        require_once dirname(__DIR__) . '/tools/assert-wporg-updatechecker-guard.php';

        $root     = dirname(__DIR__) . '/includes';
        $excluded = dirname(__DIR__) . '/' . WporgUpdateCheckerGuard::EXCLUDED_FILE;

        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($root, \FilesystemIterator::SKIP_DOTS)
        );

        $offenders = [];
        /** @var \SplFileInfo $file */
        foreach ($iterator as $file) {
            if (!$file->isFile() || $file->getExtension() !== 'php') {
                continue;
            }
            if ($file->getPathname() === $excluded) {
                continue;
            }

            $source = (string) file_get_contents($file->getPathname());
            if (!str_contains($source, WporgUpdateCheckerGuard::CLASS_NAME)) {
                continue;
            }

            foreach (WporgUpdateCheckerGuard::analyse($source)['violations'] as $violation) {
                $offenders[] = $file->getBasename() . ':' . $violation['line'] . '  ' . $violation['context'];
            }
        }

        $this->assertSame(
            [],
            $offenders,
            'these files ship in the wp.org build and resolve the absent self-updater class outside a guard: '
            . implode(' | ', $offenders)
        );
    }

    // -------------------------------------------------------------------------
    // Probe harness
    // -------------------------------------------------------------------------

    /**
     * Stage a tree containing ONLY the command file and its interface (i.e. the
     * wp.org build's view of the world), then run the probe against it.
     *
     * @param bool $defineWporgConstant Whether the child defines WPMGR_WPORG_BUILD.
     * @return array{name:string,result:array<string,mixed>}
     */
    private function runProbe(bool $defineWporgConstant): array
    {
        if (!function_exists('exec')) {
            $this->markTestSkipped('exec() is unavailable in this PHP configuration.');
        }

        $agentDir = dirname(__DIR__);
        $stageDir = sys_get_temp_dir() . '/wpmgr-wporg-probe-' . bin2hex(random_bytes(6));
        mkdir($stageDir . '/includes/commands', 0755, true);
        mkdir($stageDir . '/includes/support', 0755, true);

        copy(
            $agentDir . '/includes/commands/interface-command-interface.php',
            $stageDir . '/includes/commands/interface-command-interface.php'
        );
        copy(
            $agentDir . '/includes/commands/class-agent-self-update-command.php',
            $stageDir . '/includes/commands/class-agent-self-update-command.php'
        );

        // Assert the premise rather than assume it: includes/support/ is staged
        // but deliberately EMPTY of the self-updater, exactly as agent-zip-wporg
        // leaves it.
        $this->assertFileDoesNotExist($stageDir . '/includes/support/class-update-checker.php');

        $probeScript = $stageDir . '/probe.php';
        file_put_contents($probeScript, self::probeSource());

        $outputLines = [];
        $exitCode    = 1;
        try {
            $cmd = escapeshellarg(PHP_BINARY)
                . ' -f ' . escapeshellarg($probeScript)
                . ' -- ' . escapeshellarg($stageDir)
                . ' ' . escapeshellarg($defineWporgConstant ? '1' : '0');

            exec($cmd . ' 2>&1', $outputLines, $exitCode); // phpcs:ignore WordPress.PHP.DiscouragedFunctions.exec -- test-only: spawns a disposable subprocess to load the command file in a tree that genuinely lacks the self-updater; all inputs are internally generated absolute paths, none are request-derived
        } finally {
            $this->rrmdir($stageDir);
        }

        $rawOutput = implode("\n", $outputLines);
        $this->assertSame(
            0,
            $exitCode,
            'the command file must LOAD AND ANSWER in a tree without the self-updater, never fatal: ' . $rawOutput
        );

        $decoded = json_decode($rawOutput, true);
        $this->assertIsArray($decoded, 'probe subprocess must emit parseable JSON: ' . $rawOutput);

        /** @var array{name:string,result:array<string,mixed>} $decoded */
        return $decoded;
    }

    /**
     * Source of the child process. Loads the two staged files by hand (no
     * Composer autoloader is registered, so an unresolvable symbol would be a
     * hard fatal, which is precisely the failure this test is looking for),
     * proves the self-updater class is absent, then invokes the command.
     *
     * @return string
     */
    private static function probeSource(): string
    {
        return <<<'PHP'
<?php
declare(strict_types=1);

$stageDir = $argv[1] ?? '';
$wporg    = ($argv[2] ?? '0') === '1';

if ($wporg) {
    define('WPMGR_WPORG_BUILD', true);
}

require_once $stageDir . '/includes/commands/interface-command-interface.php';
require_once $stageDir . '/includes/commands/class-agent-self-update-command.php';

if (class_exists('WPMgr\Agent\Support\UpdateChecker', false)) {
    fwrite(STDERR, "premise violated: UpdateChecker is present in the probe process\n");
    exit(2);
}

$command = new WPMgr\Agent\Commands\AgentSelfUpdateCommand(null);

echo json_encode([
    'name'   => $command->name(),
    'result' => $command->execute([], []),
]);
PHP;
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }
}
