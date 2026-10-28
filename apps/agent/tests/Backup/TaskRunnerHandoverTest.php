<?php
/**
 * GH #983: a checkpoint save that finds the task row gone stops the worker
 * without reporting success, and a worker that has used most of its time
 * limit hands over after a saved part, through the watchdog's resume path,
 * to a next worker that continues from that part.
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

/** Task storage that records every save and can lose the row at a chosen save. */
final class RecordingTasksWpdb
{
    public string $prefix = 'wp_';

    /** @var list<array{phase:string,sub_state:array<string,mixed>}> Every save attempted, accepted or not. */
    public array $saves = [];

    /** @var (callable(array<string,mixed>, RecordingTasksWpdb): void)|null Runs before each save. */
    public $beforeSave = null;

    public function __construct(public FakeBackupTasksWpdb $inner)
    {
    }

    /** @param mixed ...$args */
    public function prepare(string $query, ...$args): string
    {
        return $this->inner->prepare($query, ...$args);
    }

    /** @return string|null */
    public function get_var(string $prepared)
    {
        return $this->inner->get_var($prepared);
    }

    /** @return array<string,mixed>|null */
    public function get_row(string $prepared, $output = null)
    {
        return $this->inner->get_row($prepared, $output);
    }

    /** @return list<string> */
    public function get_col(string $prepared): array
    {
        return $this->inner->get_col($prepared);
    }

    /** @param array<string,mixed> $data @param array<string,mixed> $where */
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null): int
    {
        if (isset($data['sub_state'], $data['phase'])) {
            $state = TaskRunner::decodeSubStateColumn($data['sub_state']);
            if ($this->beforeSave !== null) { ($this->beforeSave)($state, $this); }
            $this->saves[] = ['phase' => (string) $data['phase'], 'sub_state' => $state];
        }
        return $this->inner->update($table, $data, $where, $format, $whereFormat);
    }

    public function query(string $prepared): int
    {
        return $this->inner->query($prepared);
    }

    /** @param array<string,mixed> $where */
    public function delete(string $table, array $where, $whereFormat = null): int
    {
        return $this->inner->delete($table, $where, $whereFormat);
    }
}

/**
 * @covers \WPMgr\Agent\Backup\TaskRunner
 * @covers \WPMgr\Agent\Backup\Watchdog
 */
final class TaskRunnerHandoverTest extends TestCase
{
    private const ID = '98309831-aaaa-4bbb-8ccc-dddddddddddd';

    private string $root = '';

    /** @var list<array{phase:string,detail:array<string,mixed>}> */
    private array $posts = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-983-runner-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/runs', 0700, true);
        mkdir($this->root . '/src', 0700, true);
        $this->root = (string) realpath($this->root);
        Functions\when('esc_html')->returnArg();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_schedule_single_event')->justReturn(true);
        \Patchwork\redefine('WPMgr\Agent\Signer::signHeaders', fn (): array => ['X-Test-Signed' => '1']); // Instance method: the closure must not be static.
        $this->posts = [];
        Functions\when('wp_remote_post')->alias(function ($url, $args) {
            $body = json_decode((string) $args['body'], true);
            if (str_ends_with((string) $url, '/progress') && is_array($body) && isset($body['phase'])) {
                $this->posts[] = ['phase' => (string) $body['phase'], 'detail' => (array) ($body['phase_detail'] ?? [])];
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        parent::tear_down();
    }

    // ---- Item 3: a 0-row save means the task is gone ----------------------

    public function testZeroRowSaveIsTaskGoneOnlyWhenTheRowIsProvablyMissing(): void
    {
        $save = new \ReflectionMethod(TaskRunner::class, 'saveTaskState');
        foreach (['0' => true, '1' => false, 'unknown' => false] as $count => $gone) {
            $GLOBALS['wpdb'] = new class ($count === 'unknown' ? null : (string) $count) {
                public string $prefix = 'wp_';
                public function __construct(private ?string $count) {}
                /** @param mixed ...$args */
                public function prepare(string $q, ...$args): string { return $q; }
                public function get_var(string $q) { return $this->count; }
                /** @param array<string,mixed> $d @param array<string,mixed> $w */
                public function update(string $t, array $d, array $w, $f = null, $wf = null): int { return 0; }
            };
            try {
                $result = $save->invoke(new TaskRunner($this->params()), TaskRunner::PHASE_ARCHIVING_FILES, []);
                self::assertFalse($gone, 'count=' . $count . ' must stop the worker');
                self::assertTrue($result, 'an unchanged row (count=' . $count . ') is not a gone task');
            } catch (\RuntimeException $e) {
                self::assertTrue($gone, 'count=' . $count . ' must not stop the worker: ' . $e->getMessage());
                self::assertSame(TaskRunner::CODE_TASK_GONE, $e->getCode());
            }
        }
    }

    public function testWorkerWhoseTaskIsGivenUpMidArchiveStopsWithoutSuccess(): void
    {
        $this->tree(['plugins/acme/acme.php', 'uploads/a.jpg', 'themes/t/style.css']);
        $wpdb = $this->seed();
        // The watchdog gives the task up while the worker saves its first checkpoint.
        $wpdb->beforeSave = static function (array $state, RecordingTasksWpdb $db): void {
            if (isset($state['files']['checkpoint'])) { unset($db->inner->rows[self::ID]); }
        };

        $phase = (new TaskRunner($this->params()))->run();

        self::assertSame(TaskRunner::PHASE_FAILED, $phase);
        self::assertSame([], array_values(array_filter($wpdb->saves, static fn (array $s): bool => $s['phase'] !== TaskRunner::PHASE_ARCHIVING_FILES)), 'the worker never moves on from the orphaned archive');
        self::assertCount(1, array_filter($wpdb->saves, static fn (array $s): bool => isset($s['sub_state']['files']['checkpoint'])), 'it stops at the first checkpoint that finds no row');
        self::assertNotContains(TaskRunner::PHASE_COMPLETED, array_column($this->posts, 'phase'));
        self::assertSame([TaskRunner::PHASE_FAILED], array_values(array_unique(array_filter(array_column($this->posts, 'phase'), static fn (string $p): bool => $p === TaskRunner::PHASE_FAILED))));
        self::assertStringContainsString('task record on this site was removed', (string) end($this->posts)['detail']['message']);
        self::assertArrayNotHasKey(self::ID, $wpdb->inner->rows, 'the row is never written back');
        self::assertDirectoryDoesNotExist($this->root . '/runs/' . self::ID, 'the worker reclaims its own scratch under its guards');
    }

    // ---- Item 5: hand over before the time limit --------------------------

    public function testWorkerPastItsTimeBudgetHandsOverAfterASavedPartAndTheNextWorkerContinues(): void
    {
        $files = ['plugins/acme/acme.php', 'plugins/acme/readme.txt', 'uploads/a.jpg', 'uploads/b.jpg', 'themes/t/style.css', 'index.php'];
        $this->tree($files);
        $wpdb = $this->seed();
        $archived = null;
        $wpdb->beforeSave = function (array $state) use (&$archived): void {
            if (!empty($state['files']['done']) && $archived === null) { $archived = $this->entries($state['files']['parts']); }
        };
        $first = new TaskRunner($this->params());
        // This worker's time limit is spent: it must hand over at its first saved part.
        (new \ReflectionProperty($first, 'timeLimit'))->setValue($first, 1e-9);

        $first->run();

        $workers = [];
        foreach ($wpdb->saves as $save) {
            $worker = $save['sub_state']['worker'] ?? null;
            if (is_array($worker) && !in_array($worker['seq'], array_column($workers, 'seq'), true)) { $workers[] = $worker; }
        }
        self::assertCount(2, $workers, 'a second worker started after the first saved a part');
        self::assertGreaterThan($workers[0]['seq'], $workers[1]['seq'], 'the second worker started after saved progress');
        $handOff = array_values(array_filter($wpdb->inner->updates, static fn (array $u): bool => array_key_exists('resume_count', $u['data'])));
        self::assertSame([0], array_column(array_column($handOff, 'data'), 'resume_count'), 'the hand-over goes through the watchdog resume path and is not counted');

        $firstPart = null;
        foreach ($wpdb->saves as $save) {
            if (($save['sub_state']['worker']['seq'] ?? null) === $workers[0]['seq'] && !empty($save['sub_state']['files']['parts_completed'])) {
                $firstPart = $save['sub_state']['files']['parts_completed'][0];
            }
        }
        self::assertIsString($firstPart, 'the first worker saved a closed part before handing over');
        $done = null;
        foreach ($wpdb->saves as $save) {
            if (!empty($save['sub_state']['files']['done'])) { $done = $save['sub_state']['files']; break; }
        }
        self::assertIsArray($done, 'the next worker finished the archive');
        self::assertSame($firstPart, $done['parts'][0], 'the part the first worker saved is kept, never rebuilt');
        sort($files);
        self::assertSame($files, $archived, 'every file is archived exactly once across both workers');
        fwrite(STDOUT, "\nhand-over workers=" . json_encode($workers) . ' parts=' . json_encode($done['parts']) . "\n");
    }

    public function testWorkerWithinItsTimeBudgetDoesNotHandOver(): void
    {
        $this->tree(['plugins/acme/acme.php', 'uploads/a.jpg']);
        $wpdb = $this->seed();
        $runner = new TaskRunner($this->params());
        (new \ReflectionProperty($runner, 'timeLimit'))->setValue($runner, 1e9);

        $runner->run();

        $seqs = array_unique(array_filter(array_map(static fn (array $s) => $s['sub_state']['worker']['seq'] ?? null, $wpdb->saves), 'is_int'));
        self::assertCount(1, $seqs, 'one worker runs the whole archive');
        self::assertSame([], array_values(array_filter($wpdb->inner->updates, static fn (array $u): bool => array_key_exists('resume_count', $u['data']))));
    }

    // ---- helpers -----------------------------------------------------------

    private function seed(): RecordingTasksWpdb
    {
        $inner = new FakeBackupTasksWpdb();
        $inner->seedRow(self::ID, ['phase' => TaskRunner::PHASE_ARCHIVING_FILES, 'sub_state' => (string) json_encode(['params' => $this->params()])]);
        $wpdb = new RecordingTasksWpdb($inner);
        $GLOBALS['wpdb'] = $wpdb;
        return $wpdb;
    }

    /** @param list<string> $files */
    private function tree(array $files): void
    {
        foreach ($files as $i => $rel) {
            $path = $this->root . '/src/' . $rel;
            if (!is_dir(dirname($path))) { mkdir(dirname($path), 0700, true); }
            file_put_contents($path, str_repeat(chr(65 + $i), 64));
        }
    }

    /**
     * @param list<string> $parts
     * @return list<string>
     */
    private function entries(array $parts): array
    {
        $entries = [];
        foreach ($parts as $part) {
            $zip = new \ZipArchive();
            self::assertTrue($zip->open($this->root . '/runs/' . self::ID . '/' . $part) === true, 'unreadable part ' . $part);
            for ($i = 0; $i < $zip->numFiles; $i++) { $entries[] = (string) $zip->getNameIndex($i); }
            $zip->close();
        }
        sort($entries);
        return $entries;
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
