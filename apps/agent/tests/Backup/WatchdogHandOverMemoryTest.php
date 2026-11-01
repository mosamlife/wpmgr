<?php
/**
 * GH #954: a backup that needs many hand-overs keeps no earlier worker's
 * archive cursor reachable.
 *
 * A worker that has used most of its time limit stops itself after a saved
 * part and the next worker continues through the watchdog's resume path. The
 * cursor of a large site (every closed part, every settled skipped path) is
 * megabytes, so every copy of it that outlives its worker is paid again at
 * every hand-over. This test follows each cursor decoded from the task row by
 * a marker object stored in it, and asserts that when a worker starts, no
 * cursor decoded before its own is still reachable. It counts references,
 * never bytes, so its result does not depend on the host's memory accounting.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\TaskRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

require_once __DIR__ . '/FakeBackupTasksWpdb.php';

/**
 * @covers \WPMgr\Agent\Backup\TaskRunner
 * @covers \WPMgr\Agent\Backup\Watchdog
 */
final class WatchdogHandOverMemoryTest extends TestCase
{
    private const ID = '95409540-aaaa-4bbb-8ccc-dddddddddddd';

    /** Hand-overs in one run: the chain has one worker more than this. */
    private const HAND_OVERS = 6;

    /** Settled skipped paths in the cursor every worker carries. */
    private const SKIPPED_PATHS = 50000;

    private string $root = '';

    private string $exceptionIgnoreArgs = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-954-memory-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/runs/' . self::ID, 0700, true);
        mkdir($this->root . '/src', 0700, true);
        $this->root = (string) realpath($this->root);
        Functions\when('esc_html')->returnArg();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('wp_remote_post')->justReturn(['response' => ['code' => 200], 'body' => '{}']);
        \Patchwork\redefine('WPMgr\Agent\Signer::signHeaders', fn (): array => ['X-Test-Signed' => '1']); // Instance method: the closure must not be static.
        // Keep call arguments in exception traces, as a development php.ini or
        // no php.ini at all does: the worst case for what a hand-over keeps.
        $this->exceptionIgnoreArgs = (string) ini_get('zend.exception_ignore_args');
        ini_set('zend.exception_ignore_args', '0');
    }

    protected function tear_down(): void
    {
        ini_set('zend.exception_ignore_args', $this->exceptionIgnoreArgs);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        parent::tear_down();
    }

    public function testNoCursorStaysReachableOnceItsWorkerHasHandedOver(): void
    {
        $db = new FakeBackupTasksWpdb();
        $db->seedRow(self::ID, ['phase' => TaskRunner::PHASE_ARCHIVING_FILES, 'sub_state' => (string) json_encode(['params' => $this->params(), 'files' => $this->cursor()])]);
        $GLOBALS['wpdb'] = $db;

        // Every cursor decoded from the task row: a weak reference to its
        // marker, who decoded it, and how many workers had started by then.
        $decoded   = [];
        $leaks     = [];
        $started   = 0;
        $handOvers = self::HAND_OVERS; // A redefinition is bound to the class it replaces: self:: would not be this test.

        // PHP cannot hold a weak reference to an array, so each cursor decoded
        // from the task row gets a marker object inside it: while any copy of
        // that cursor is reachable, so is its marker.
        \Patchwork\redefine('WPMgr\Agent\Backup\TaskRunner::decodeSubStateColumn', function ($raw) use (&$decoded, &$started) {
            $state = \Patchwork\relay();
            if (!is_array($state['files'] ?? null)) {
                return $state;
            }
            $by = 'other';
            foreach (debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS, 12) as $frame) {
                if (($frame['function'] ?? '') === 'decodeSubState') { $by = 'watchdog'; break; }
                if (($frame['function'] ?? '') === 'loadTask') { $by = 'worker'; break; }
            }
            $marker = new \stdClass();
            $state['files']['marker'] = $marker;
            $decoded[] = ['ref' => \WeakReference::create($marker), 'by' => $by, 'after' => $started];
            return $state;
        });
        // Every worker hands over at its first saved part.
        \Patchwork\redefine('WPMgr\Agent\Backup\TaskRunner::shouldHandOver', function (): bool { return true; }); // Instance method: the closure must not be static.
        // The archive pass is replaced by one that first checks what is still
        // reachable, then closes a part and saves it through the runner's own
        // checkpoint callback, which hands over; the last worker stops instead.
        // Findings are collected, not asserted here: an assertion thrown inside
        // a worker is swallowed as that worker's failure.
        \Patchwork\redefine('WPMgr\Agent\Backup\FilesArchiver::archive', function (string $outDir, array $resume, callable $progress, ?array $prevMap = null, ?callable $checkpoint = null) use (&$decoded, &$leaks, &$started, $handOvers): array { // Instance method: the closure must not be static.
            $started++;
            gc_collect_cycles();
            $live = $resume['marker'] ?? null;
            if (!$live instanceof \stdClass) {
                $leaks['blind-' . $started] = sprintf('worker %d: its cursor carries no marker, so this check could not follow it', $started);
            }
            foreach ($decoded as $i => $entry) {
                $held = $entry['ref']->get();
                if ($held !== null && $held !== $live && !isset($leaks[$i])) {
                    $leaks[$i] = sprintf(
                        'worker %d started while %s is still reachable',
                        $started,
                        $entry['by'] === 'watchdog'
                            ? sprintf('the cursor the watchdog decoded for hand-over %d', $entry['after'])
                            : sprintf('the cursor worker %d loaded', $entry['after'] + 1)
                    );
                }
            }
            unset($held);
            if ($started > $handOvers) {
                throw new \RuntimeException('the last worker of the chain stops here');
            }
            $resume['parts_completed'][] = sprintf('plugins.g000.part%03d.zip', $started);
            $resume['part_kinds'][]      = 'plugin';
            $resume['part_sizes'][]      = 1024;
            ($checkpoint)($resume);
            throw new \LogicException('a saved checkpoint must hand over');
        });

        $phase = (new TaskRunner($this->params()))->run();

        self::assertSame(TaskRunner::PHASE_FAILED, $phase, 'the last worker of the chain is the one that stops');
        self::assertSame(self::HAND_OVERS + 1, $started, 'every hand-over started the next worker through the watchdog');
        self::assertGreaterThanOrEqual(
            self::HAND_OVERS,
            count(array_filter($decoded, static fn (array $d): bool => $d['by'] === 'watchdog')),
            'the watchdog decoded the task row for every hand-over, so the check followed those cursors'
        );
        self::assertSame([], array_values($leaks), "a cursor stayed reachable after its worker handed over:\n" . implode("\n", $leaks));
    }

    /**
     * A cursor shaped like the archiver's own checkpoint, with every settled
     * skipped path of a large uploads tree.
     *
     * @return array<string,mixed>
     */
    private function cursor(): array
    {
        $skipped = [];
        for ($i = 0; $i < self::SKIPPED_PATHS; $i++) {
            $skipped[] = sprintf('wp-content/uploads/%d/%02d/photo-%06d-1024x768.jpg', 2015 + $i % 10, 1 + $i % 12, $i);
        }
        return [
            'checkpoint'            => 1,
            'complete'              => false,
            'file_index'            => 120000,
            'parts_completed'       => ['uploads.g000.part001.zip'],
            'part_kinds'            => ['upload'],
            'part_sizes'            => [52428800],
            'skipped'               => $skipped,
            'skipped_sources_count' => count($skipped),
            'skipped_sources'       => array_slice($skipped, 0, 20),
        ];
    }

    /** @return array<string,mixed> */
    private function params(): array
    {
        return [
            'snapshot_id'       => self::ID,
            'kind'              => 'files',
            'age_recipient'     => 'age1' . str_repeat('q', 58),
            'presign_endpoint'  => 'https://cp.invalid/presign',
            'manifest_endpoint' => 'https://cp.invalid/manifest',
            'progress_endpoint' => 'https://cp.invalid/agent/v1/backups/' . self::ID . '/progress',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->root . '/runs/' . self::ID,
            'wp_content_path'   => $this->root . '/src',
            'db'                => ['prefix' => 'wp_'],
        ];
    }

    private function removeTree(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) { return; }
        foreach (new \FilesystemIterator($dir) as $file) {
            if ($file->isDir() && !$file->isLink()) { $this->removeTree($file->getPathname()); }
            else { unlink($file->getPathname()); }
        }
        rmdir($dir);
    }
}
