<?php
/**
 * GitHub issue #232 regression: TaskRunner's two layered mutual-exclusion
 * guards — GET_LOCK (fast cross-process reject) + flock (connection-
 * independent, AUTHORITATIVE).
 *
 * Root cause recap: scheduled backups intermittently stalled PERMANENTLY at
 * phase=queued because the agent had no cron-independent starter that ran
 * unconditionally. The fix (BackupCommand + this class) makes the in-process
 * shutdown-function runner ALWAYS register, which guarantees a double-fire
 * against the separate wp-cron-dispatched worker (and now also the reaper)
 * for the same snapshot_id.
 *
 * Adversarial-review follow-up (HIGH, pre-ship): GET_LOCK alone is NOT
 * sufficient. It is scoped to $wpdb's own mysqli connection and is silently
 * released the instant that connection drops — a short `wait_timeout` on a
 * managed host, a MySQL restart/failover, or `$wpdb->check_connection()`
 * reconnecting after "server has gone away" can all sever it mid-backup
 * (DbDumper streams over its OWN separate mysqli handle and only calls back
 * to $wpdb once per table, so a large-table dump can easily outlive
 * $wpdb's connection). A second caller can then legitimately win GET_LOCK
 * while the true owner is still alive and running — concurrently entering
 * the SAME scratch dir. flock() on a deterministic per-snapshot lock file is
 * the fix: it is held by the OS for as long as the owning PHP process is
 * alive, independent of $wpdb entirely, and releases automatically on
 * process exit for ANY reason (including a SIGKILL that skips every
 * PHP-level `finally`).
 *
 * Covers:
 *   1. Contended dispatch (GET_LOCK='0') never strands the row: the loser
 *      returns without mutating phase and writes only a lock_contended
 *      breadcrumb.
 *   2. GET_LOCK NULL (unsupported host): run() proceeds and completes
 *      exactly as it did before #232 — no lock semantics, no regression —
 *      AND, separately, a flock held by a live process still blocks a
 *      second caller even when GET_LOCK is entirely unsupported.
 *   3. Concurrent-into-same-scratch is BLOCKED by flock even when GET_LOCK
 *      reports "free" (simulating the dead-connection scenario above): the
 *      second caller must return without ever entering the phase machine.
 *   7. Happy path (GET_LOCK='1' + flock acquired): full queued -> ... ->
 *      completed, the row is cleaned up, and BOTH guards are released via
 *      the `finally` — including on the throw/failed path.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Support\AgeCrypto;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class TaskRunnerLockTest extends TestCase
{
    private const SNAPSHOT_ID = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';

    private string $root       = '';
    private string $scratchDir = '';
    private string $wpContent  = '';
    private string $keyFile    = '';

    /** @var array<string,mixed> in-memory wp-option store, shared by Keystore. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!class_exists(\ZipArchive::class)) {
            self::markTestSkipped('ext-zip not available');
        }

        $this->root       = sys_get_temp_dir() . '/wpmgr-lock-test-' . bin2hex(random_bytes(6));
        $this->scratchDir = $this->root . '/scratch';
        $this->wpContent  = $this->root . '/wp-content';
        mkdir($this->scratchDir, 0755, true);
        mkdir($this->wpContent, 0755, true);
        file_put_contents($this->wpContent . '/marker.txt', 'a real file for FilesArchiver to pack');

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-lock-test-key-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));

        // Pre-provision the site's Ed25519 keypair so TaskRunner's internal
        // `new Keystore()` (constructed fresh inside runEncryptingUploading)
        // finds it via the shared in-memory option store above — exactly the
        // SignerTest pattern.
        (new Keystore())->generateSiteKeypair();
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Stub wp_remote_post/wp_remote_request/is_wp_error/wp_remote_retrieve_*
     * so BackupTransport's presignChunks()/putChunk()/submitManifest() all
     * succeed without any real network I/O. presignChunks() always reports
     * "nothing needs upload" (empty uploads map) — a legitimate CP response
     * (full content-addressed dedup) — so putChunk()/wp_remote_request is
     * never exercised and doesn't need stubbing either.
     */
    private function stubNetworkSuccess(string $presignEndpoint, string $manifestEndpoint): void
    {
        Functions\when('wp_remote_post')->alias(function ($url, $args) use ($presignEndpoint, $manifestEndpoint) {
            if ($url === $presignEndpoint) {
                return ['response' => ['code' => 200], 'body' => (string) json_encode(['uploads' => []])];
            }
            if ($url === $manifestEndpoint) {
                return ['response' => ['code' => 200], 'body' => (string) json_encode(['ok' => true, 'chunk_count' => 0, 'stored_count' => 0])];
            }
            return new \WP_Error('unexpected_url', 'unexpected URL: ' . $url);
        });
        Functions\when('is_wp_error')->alias(static fn ($v) => $v instanceof \WP_Error);
        Functions\when('wp_remote_retrieve_response_code')->alias(static fn ($r) => is_array($r) ? ($r['response']['code'] ?? 0) : 0);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? ($r['body'] ?? '') : '');
    }

    /**
     * @return array{0:array<string,mixed>,1:string} [params, real age recipient]
     */
    private function buildParams(): array
    {
        $identity = (new AgeCrypto())->generateIdentity();

        $params = [
            'snapshot_id'       => self::SNAPSHOT_ID,
            'kind'              => 'files',
            'age_recipient'     => $identity['recipient'],
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/lock-test/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/lock-test/manifest',
            'progress_endpoint' => '',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->wpContent,
            'db'                => ['host' => 'localhost', 'user' => 'u', 'password' => 'p', 'name' => 'n', 'prefix' => 'wp_'],
        ];

        return [$params, $identity['recipient']];
    }

    // ------------------------------------------------------------------
    // Test 1: contended dispatch (GET_LOCK='0') never strands the row.
    // ------------------------------------------------------------------

    public function test_contended_dispatch_does_not_strand(): void
    {
        [$params] = $this->buildParams();

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = ['0']; // loser on the very first (and only) probe.
        $wpdb->seedRow(self::SNAPSHOT_ID, ['sub_state' => (string) json_encode(['params' => $params])]);
        $GLOBALS['wpdb'] = $wpdb;

        $runner = new TaskRunner($params);
        $runner->run();

        $row = $wpdb->rows[self::SNAPSHOT_ID] ?? null;
        $this->assertNotNull($row, 'the loser must never delete the row');
        $this->assertSame('queued', $row['phase'], 'the loser must never mutate phase');

        $subState = json_decode((string) $row['sub_state'], true);
        $this->assertIsArray($subState);
        $this->assertSame('lock_contended', $subState['stage'] ?? null, 'the loser must write a lock_contended breadcrumb');

        $this->assertSame(0, $wpdb->releaseCallCount, 'the loser never held the lock — RELEASE_LOCK must never be called');
    }

    // ------------------------------------------------------------------
    // Test 2: GET_LOCK NULL (unsupported host) — no regression.
    // ------------------------------------------------------------------

    public function test_get_lock_null_falls_through_and_completes_like_today(): void
    {
        [$params, $recipient] = $this->buildParams();
        $this->stubNetworkSuccess($params['presign_endpoint'], $params['manifest_endpoint']);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null]; // GET_LOCK unsupported.
        $GLOBALS['wpdb'] = $wpdb;

        $runner = new TaskRunner($params);
        $phase  = $runner->run();

        $this->assertSame(TaskRunner::PHASE_COMPLETED, $phase, 'a null GET_LOCK() must not block completion');
        $this->assertArrayNotHasKey(self::SNAPSHOT_ID, $wpdb->rows, 'a completed run deletes its row regardless of lock support');
        $this->assertSame(0, $wpdb->releaseCallCount, 'no lock was held (null token) — RELEASE_LOCK must never be called');
    }

    /**
     * Adversarial-review follow-up: even when MySQL locking is ENTIRELY
     * unavailable (GET_LOCK returns null on every call — e.g. a non-MySQL
     * $wpdb driver), a live process's flock alone must still prevent a
     * second caller from ever entering the phase machine. This is the "no
     * unlocked race" guarantee flock backstops independent of GET_LOCK.
     */
    public function test_get_lock_null_and_flock_held_still_blocks_a_second_runner(): void
    {
        [$params] = $this->buildParams();

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null]; // GET_LOCK unsupported for every caller.
        $wpdb->seedRow(self::SNAPSHOT_ID, ['sub_state' => (string) json_encode(['params' => $params])]);
        $GLOBALS['wpdb'] = $wpdb;

        [$lockHandle] = $this->externallyHoldTheFlock($params);

        try {
            $runnerB = new TaskRunner($params);
            $runnerB->run();

            $row = $wpdb->rows[self::SNAPSHOT_ID] ?? null;
            $this->assertNotNull($row, 'the flock-loser must never delete the row');
            $this->assertSame(
                'queued',
                $row['phase'],
                'without ANY MySQL lock support, a live process holding the flock must still prevent a concurrent run'
            );

            $subState = json_decode((string) $row['sub_state'], true);
            $this->assertSame('lock_contended', $subState['stage'] ?? null);
        } finally {
            flock($lockHandle, LOCK_UN);
            fclose($lockHandle);
        }
    }

    // ------------------------------------------------------------------
    // Test 3: concurrent-into-same-scratch is BLOCKED by flock even when
    // GET_LOCK reports "free" (the exact dead-connection scenario the
    // adversarial review flagged: GET_LOCK is scoped to $wpdb's connection
    // and is silently released if that connection drops mid-backup, but the
    // true owner — and its flock — is still alive and running).
    // ------------------------------------------------------------------

    /**
     * Simulates the HIGH-severity double-run finding directly: runner A
     * holds the flock (a live process, regardless of its $wpdb connection
     * state); runner B's GET_LOCK call reports '1' ("free" — as it would if
     * A's mysqli connection had silently dropped). Runner B must still be
     * rejected by the authoritative flock check, and — decisively — must
     * NEVER create a single new file under the scratch dir (kind=files
     * drives archiving_files, which needs no network/DB and so is fully
     * self-contained: if B were let through, as the pre-fix GET_LOCK-only
     * code would, it would produce real zip/files.list artifacts on disk
     * here, no external dependency needed to observe the corruption). This
     * guard sits at the very top of run(), before ANY phase dispatch, so it
     * protects dumping_db/database.sql.gz — the concrete artifact the
     * adversarial review's report centered on — identically; kind=files is
     * used here only because it makes the "B touched nothing" proof
     * observable without a live MySQL connection in the test sandbox.
     *
     * This test FAILS against a GET_LOCK-only implementation (no flock):
     * with only GET_LOCK, B's '1' would let it straight into the phase loop
     * and it would create real archive artifacts before ever touching
     * anything DB/network-dependent.
     */
    public function test_concurrent_into_same_scratch_is_blocked_by_flock_even_when_get_lock_reports_free(): void
    {
        [$params] = $this->buildParams();

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = ['1']; // GET_LOCK reports "free" — simulates A's dead $wpdb connection.
        $wpdb->seedRow(self::SNAPSHOT_ID, ['sub_state' => (string) json_encode(['params' => $params])]);
        $GLOBALS['wpdb'] = $wpdb;

        [$lockHandle, $lockPath] = $this->externallyHoldTheFlock($params);
        $beforeScratchListing = $this->scratchDirListing();

        try {
            $runnerB = new TaskRunner($params);
            $runnerB->run();

            $row = $wpdb->rows[self::SNAPSHOT_ID] ?? null;
            $this->assertNotNull($row, 'B must never delete the row it lost the race for');
            $this->assertSame(
                'queued',
                $row['phase'],
                'B must never advance phase — GET_LOCK reporting "free" must not be enough once the authoritative flock rejects it'
            );

            $subState = json_decode((string) $row['sub_state'], true);
            $this->assertSame('lock_contended', $subState['stage'] ?? null);

            $this->assertSame(
                $beforeScratchListing,
                $this->scratchDirListing(),
                'B must create ZERO new files in the scratch dir — this is the exact concurrent-write corruption the adversarial review flagged'
            );
            $this->assertFileDoesNotExist(
                $this->scratchDir . '/database.sql.gz',
                'B must never reach dumping_db (the guard is phase-agnostic, checked before ANY phase work)'
            );

            $this->assertSame(
                1,
                $wpdb->releaseCallCount,
                'B must release the GET_LOCK it provisionally won once the authoritative flock rejects it'
            );

            // GH #232 lock-file GC follow-up: a contended loser must NEVER
            // delete the winner's (A's) lock file — A is still alive and
            // running; only the invocation that actually reaches a terminal
            // outcome for a snapshot may ever reclaim that snapshot's lock
            // file, and B never reaches the `finally` code that does so at
            // all (it returned above, before the try/finally even begins).
            $this->assertFileExists(
                $lockPath,
                "the contended loser (B) must never delete the winner's (A's) still-held lock file"
            );
        } finally {
            flock($lockHandle, LOCK_UN);
            fclose($lockHandle);
        }
    }

    /**
     * Resolve the SAME per-snapshot flock path TaskRunner itself would
     * compute for $params (via reflection on a throwaway instance sharing
     * the same params — avoids re-implementing/duplicating the path
     * derivation in the test, which would risk silently drifting out of
     * sync with the production logic), then open + hold an exclusive,
     * non-blocking flock on it — simulating a live process ("runner A")
     * that already owns this snapshot's run.
     *
     * @param array<string,mixed> $params
     * @return array{0:resource,1:string} [held lock handle, resolved path]
     */
    private function externallyHoldTheFlock(array $params): array
    {
        $probe    = new TaskRunner($params);
        $method   = new \ReflectionMethod(TaskRunner::class, 'fileLockPath');
        $lockPath = $method->invoke($probe);
        $this->assertIsString($lockPath);
        $this->assertNotSame('', $lockPath, 'sanity: a lock path must be resolvable from these params');

        $dir = dirname($lockPath);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }

        $handle = fopen($lockPath, 'c');
        $this->assertNotFalse($handle, 'sanity: the lock file must be openable in the test');
        $this->assertTrue(flock($handle, LOCK_EX | LOCK_NB), 'sanity: runner A must be able to acquire the flock first');

        return [$handle, $lockPath];
    }

    /**
     * Recursive listing of every file under the scratch dir, relative paths
     * sorted — used to prove a blocked runner created ZERO new artifacts.
     *
     * @return list<string>
     */
    private function scratchDirListing(): array
    {
        if (!is_dir($this->scratchDir)) {
            return [];
        }
        $out = [];
        $it  = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($this->scratchDir, \FilesystemIterator::SKIP_DOTS)
        );
        foreach ($it as $file) {
            /** @var \SplFileInfo $file */
            $out[] = substr($file->getPathname(), strlen($this->scratchDir));
        }
        sort($out);
        return $out;
    }

    // ------------------------------------------------------------------
    // Test 7: happy path (GET_LOCK='1' + flock) completes; both guards
    // released via the finally.
    // ------------------------------------------------------------------

    public function test_happy_path_completes_and_releases_lock(): void
    {
        [$params, $recipient] = $this->buildParams();
        $this->stubNetworkSuccess($params['presign_endpoint'], $params['manifest_endpoint']);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = ['1'];
        $GLOBALS['wpdb'] = $wpdb;

        $runner = new TaskRunner($params);
        $phase  = $runner->run();

        $this->assertSame(TaskRunner::PHASE_COMPLETED, $phase);
        $this->assertArrayNotHasKey(self::SNAPSHOT_ID, $wpdb->rows, 'a completed run must delete its row');
        $this->assertSame(1, $wpdb->lockCallCount, 'exactly one GET_LOCK probe per run() invocation');
        $this->assertSame(1, $wpdb->releaseCallCount, 'the winner must RELEASE_LOCK exactly once, via the finally');

        // Confirm the runner_started breadcrumb was actually stamped BEFORE
        // any phase advancement — i.e. the write that set stage=runner_started
        // must itself still carry phase='queued' (the observability ladder's
        // "lock held, first phase failed to persist" signal is exactly this
        // transient state, captured here via the update() history since the
        // row itself is deleted by the time run() returns).
        $runnerStartedPhase = null;
        foreach ($wpdb->updates as $u) {
            $subState = isset($u['data']['sub_state']) ? json_decode((string) $u['data']['sub_state'], true) : null;
            if (is_array($subState) && ($subState['stage'] ?? null) === 'runner_started') {
                $runnerStartedPhase = $u['data']['phase'] ?? null;
                break;
            }
        }
        $this->assertSame(
            TaskRunner::PHASE_QUEUED,
            $runnerStartedPhase,
            'the runner_started breadcrumb must be stamped while phase is still queued — the pre-advancement state a crash would leave behind'
        );

        $this->assertFlockIsFree($params, 'the winner must release its flock (in the finally) so a later legit resume can acquire it');

        // GH #232 lock-file GC follow-up: a completed run must reclaim its
        // OWN <snapshot_id>.lock coordination file — not merely release the
        // flock on it (assertFlockIsFree() above proves that separately) —
        // so it never accumulates as an orphaned 0-byte file forever.
        $probe    = new TaskRunner($params);
        $lockPath = (new \ReflectionMethod(TaskRunner::class, 'fileLockPath'))->invoke($probe);
        $this->assertFileDoesNotExist(
            $lockPath,
            'a completed run must delete its own lock file, not just release the flock on it'
        );
    }

    /**
     * RELEASE_LOCK AND the flock must both fire via the `finally` even when
     * a phase handler throws (the failed/catch path) — not only on the
     * happy/completed path. A malformed age_recipient makes
     * AgeCrypto::decodeRecipient() throw during the encrypting_uploading
     * phase, well before any network call, so no network stubbing is
     * required for this test.
     */
    public function test_release_lock_called_on_the_throw_path(): void
    {
        [$params] = $this->buildParams();
        $params['age_recipient'] = 'not-a-valid-age-recipient';

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = ['1'];
        $GLOBALS['wpdb'] = $wpdb;

        $runner = new TaskRunner($params);
        $phase  = $runner->run();

        $this->assertSame(TaskRunner::PHASE_FAILED, $phase, 'a malformed recipient must fail the run, not throw out of run()');
        $this->assertArrayNotHasKey(self::SNAPSHOT_ID, $wpdb->rows, 'the failure path deletes its row too');
        $this->assertSame(1, $wpdb->releaseCallCount, 'RELEASE_LOCK must fire via the finally even on the failed/throw path');

        $this->assertFlockIsFree($params, 'the flock must be released via the finally even on the failed/throw path, so a later legit resume can acquire it');

        // GH #232 lock-file GC follow-up: 'failed' is a terminal outcome too
        // (the row is DELETEd on this path — see the DELETE assertion above
        // — so the lock file has nothing left to protect against).
        $probe    = new TaskRunner($params);
        $lockPath = (new \ReflectionMethod(TaskRunner::class, 'fileLockPath'))->invoke($probe);
        $this->assertFileDoesNotExist(
            $lockPath,
            'a failed run must also delete its own lock file (failed is a terminal outcome, same as completed)'
        );
    }

    /**
     * Assert the flock for $params is currently free by acquiring (and
     * immediately releasing) it ourselves — proof that TaskRunner's own
     * `finally` released it rather than leaving it held past the end of
     * run().
     *
     * `fopen($lockPath, 'c')` CREATES the file if it is missing — which it
     * will be, by design, after a terminal run's own cleanupFileLock() has
     * already reclaimed it (see the GH #232 lock-file GC follow-up). This
     * probe therefore always deletes the (possibly just-recreated) file
     * again before returning, so a subsequent assertFileDoesNotExist() check
     * in the same test reflects TaskRunner's OWN cleanup, never a side
     * effect of this probe having resurrected the file.
     *
     * @param array<string,mixed> $params
     */
    private function assertFlockIsFree(array $params, string $message): void
    {
        $probe    = new TaskRunner($params);
        $method   = new \ReflectionMethod(TaskRunner::class, 'fileLockPath');
        $lockPath = $method->invoke($probe);
        $this->assertIsString($lockPath);

        $handle = fopen($lockPath, 'c');
        $this->assertNotFalse($handle, 'sanity: the lock file must be openable in the test');
        $this->assertTrue(flock($handle, LOCK_EX | LOCK_NB), $message);
        flock($handle, LOCK_UN);
        fclose($handle);

        wp_delete_file($lockPath);
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
