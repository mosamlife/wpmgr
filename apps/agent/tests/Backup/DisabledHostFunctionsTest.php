<?php
/**
 * Hosts that list set_time_limit, ignore_user_abort, disk_free_space,
 * disk_total_space or link in disable_functions. On PHP 8 such a function is
 * undefined, and calling it throws an Error that `@` does not silence, so
 * the backup and restore code reaches them only through guarded helpers.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class DisabledHostFunctionsTest extends TestCase
{
    /** Functions a host may disable that the backup and restore code needs. */
    private const GUARDED = ['set_time_limit', 'ignore_user_abort', 'disk_free_space', 'disk_total_space'];

    private string $root = '';

    protected function tear_down(): void
    {
        if ($this->root !== '') {
            $this->removeTree($this->root);
        }
        parent::tear_down();
    }

    public function testBackupAndRestoreRunOnAHostThatDisablesThem(): void
    {
        $this->root = sys_get_temp_dir() . '/wpmgr-disabled-fns-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0700, true);
        $agentDir = dirname(__DIR__, 2);
        $cmd = [
            PHP_BINARY,
            '-d', 'disable_functions=' . implode(',', self::GUARDED) . ',link',
            '-d', 'memory_limit=-1',
            __DIR__ . '/disabled-host-functions-child.php',
            $this->root,
        ];
        self::assertTrue(function_exists('proc_open'), 'proc_open() is needed to run the child PHP');
        $process = proc_open($cmd, [1 => ['pipe', 'w'], 2 => ['pipe', 'w']], $pipes, $agentDir);
        self::assertIsResource($process, 'the child PHP did not start');
        $out  = (string) stream_get_contents($pipes[1]);
        $err  = (string) stream_get_contents($pipes[2]);
        fclose($pipes[1]);
        fclose($pipes[2]);
        $exit = proc_close($process);

        self::assertSame(0, $exit, "the child PHP failed:\n" . $out . $err);
        $lines  = array_values(array_filter(explode("\n", trim($out)), static fn (string $l): bool => str_starts_with($l, '{')));
        $report = json_decode((string) end($lines), true);
        self::assertIsArray($report, "the child printed no report:\n" . $out . $err);
        fwrite(STDOUT, "\ndisabled-functions child: " . json_encode($report) . "\n");

        self::assertSame(array_fill_keys([...self::GUARDED, 'link'], true), $report['disabled'], 'the child really ran with these functions disabled');
        self::assertSame([], $report['errors'], 'no backup or restore step may fail on such a host');
        self::assertTrue($report['steps']['extend_limits']);
        self::assertSame(['free' => null, 'total' => null], $report['steps']['disk_space']);
        self::assertIsString($report['steps']['task_runner'], 'TaskRunner::run() returns a phase instead of throwing');
        self::assertSame(2, $report['steps']['archive']);
        self::assertSame(['photo' => 'RESTORED_PHOTO', 'store' => 'LIVE_CHUNK', 'disk_fallback' => 0], $report['steps']['restore']);
    }

    public function testBackupAndRestoreCodeCallsThemOnlyThroughTheGuard(): void
    {
        $agentDir = dirname(__DIR__, 2);
        $files = array_merge(
            glob($agentDir . '/includes/backup/*.php') ?: [],
            glob($agentDir . '/includes/backup/destinations/*.php') ?: [],
            [$agentDir . '/includes/commands/class-backup-command.php', $agentDir . '/includes/commands/class-restore-command.php']
        );
        self::assertGreaterThan(15, count($files), 'the backup and restore sources were found');

        $raw = [];
        foreach ($files as $file) {
            foreach ($this->calls((string) file_get_contents($file)) as [$fn, $line]) {
                $raw[] = basename($file) . ':' . $line . ' ' . $fn . '()';
            }
        }
        self::assertSame([], $raw, 'call these through WPMgr\Agent\Support\LongRunningJob, which checks function_exists() first');

        // Positive control: the scan finds the calls the guard itself makes.
        $guard = array_column($this->calls((string) file_get_contents($agentDir . '/includes/support/class-long-running-job.php')), 0);
        sort($guard);
        self::assertSame(['disk_free_space', 'disk_total_space', 'ignore_user_abort', 'set_time_limit'], $guard);
    }

    /**
     * Direct calls to the guarded functions in a PHP source, comments and
     * strings excluded.
     *
     * @return list<array{0:string,1:int}>
     */
    private function calls(string $source): array
    {
        $tokens = token_get_all($source);
        $found  = [];
        $count  = count($tokens);
        for ($i = 0; $i < $count; $i++) {
            $token = $tokens[$i];
            if (!is_array($token) || !in_array($token[0], [T_STRING, T_NAME_FULLY_QUALIFIED], true)) {
                continue;
            }
            $name = ltrim(strtolower($token[1]), '\\');
            if (!in_array($name, self::GUARDED, true)) {
                continue;
            }
            $prev = $this->neighbour($tokens, $i, -1);
            $next = $this->neighbour($tokens, $i, 1);
            if ($next !== '(' || in_array($prev, ['->', '?->', '::', 'function', 'new'], true)) {
                continue;
            }
            $found[] = [$name, (int) $token[2]];
        }
        return $found;
    }

    /** @param list<array{0:int,1:string,2:int}|string> $tokens */
    private function neighbour(array $tokens, int $i, int $step): string
    {
        for ($j = $i + $step; isset($tokens[$j]); $j += $step) {
            $t = $tokens[$j];
            if (is_array($t) && in_array($t[0], [T_WHITESPACE, T_COMMENT, T_DOC_COMMENT], true)) {
                continue;
            }
            return is_array($t) ? strtolower($t[1]) : $t;
        }
        return '';
    }

    private function removeTree(string $dir): void
    {
        if (!is_dir($dir) || is_link($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->removeTree($path);
            } else {
                @unlink($path);
            }
        }
        @rmdir($dir);
    }
}
