<?php
/**
 * GH #283: the upload-cursor checkpoint + delete-after-persist invariant.
 *
 * Root cause this guards against: uploadChunks() used to delete each local
 * chunk file immediately after a successful PUT, while the durable
 * uploaded_hashes cursor was only ever persisted once uploadChunks() fully
 * RETURNED. A worker killed mid-loop could therefore delete N local chunk
 * files while persisting zero of them. On resume, the encrypt pass is
 * skipped (its cursor was already marked done), so those chunk files were
 * never regenerated, and because the CP's dedup is manifest-scoped, it
 * re-presigned the very hashes the agent had already uploaded, sending the
 * resumed pass straight into a "missing local chunk for upload" throw.
 *
 * The fix threads an optional checkpoint callback through uploadChunks()
 * that durably persists the partial cursor every PERSIST_EVERY_UPLOAD
 * chunks (and once more at the end of every sweep) BEFORE any of that
 * batch's local files are deleted. These tests exercise that contract
 * directly against EncryptAndUpload, the same seam TaskRunner wires its
 * saveTaskState()-backed checkpoint into.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClassConstant;
use WPMgr\Agent\Backup\EncryptAndUpload;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\EncryptAndUpload
 */
final class EncryptAndUploadCheckpointTest extends TestCase
{
    private string $scratchDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));

        $this->scratchDir = sys_get_temp_dir() . '/wpmgr-encrypt-upload-ckpt-' . bin2hex(random_bytes(6));
        if (!is_dir($this->scratchDir) && !mkdir($this->scratchDir, 0700, true) && !is_dir($this->scratchDir)) {
            self::fail('could not create scratch dir for test');
        }
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->scratchDir);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * A transport stub that records PUTs without doing real network I/O.
     * `$onlyHashes` (when non-null) restricts presignChunks() to return a
     * URL for ONLY those hashes, every other hash is treated by the CP as
     * already-stored (a dedup hit), matching how a real presign response
     * omits hashes the CP already has.
     */
    private function transport(?array $onlyHashes = null): BackupTransport
    {
        return new class($onlyHashes) extends BackupTransport {
            /** @var array<string,string> hash => uploaded bytes PUT */
            public array $puts = [];
            /** @var list<string>|null */
            public ?array $onlyHashes;

            public function __construct(?array $onlyHashes)
            {
                $this->onlyHashes = $onlyHashes;
            }

            public function presignChunks(string $endpoint, string $snapshotId, array $hashes): array
            {
                $uploads = [];
                foreach ($hashes as $h) {
                    if ($this->onlyHashes !== null && !in_array($h, $this->onlyHashes, true)) {
                        continue;
                    }
                    $uploads[$h] = 'https://s3.example/put/' . $h;
                }
                return $uploads;
            }

            public function putChunk(string $presignedUrl, string $ciphertext): bool
            {
                $hash = substr($presignedUrl, strlen('https://s3.example/put/'));
                $this->puts[$hash] = $ciphertext;
                return true;
            }
        };
    }

    private function makePipeline(int $chunkBytes, BackupTransport $transport): EncryptAndUpload
    {
        return new EncryptAndUpload(
            new AgeCrypto(),
            $transport,
            'snap-ckpt-1',
            'age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq',
            'https://cp.example/agent/v1/backups/snap-ckpt-1/presign',
            'https://cp.example/agent/v1/backups/snap-ckpt-1/manifest',
            $chunkBytes
        );
    }

    private function noopProgress(): callable
    {
        return static function (string $phase, array $detail): void {
        };
    }

    /**
     * Write an artifact made of $chunkCount distinct, exactly-$chunkBytes-
     * sized blocks so encryptChunks() produces $chunkCount DISTINCT chunk
     * hashes (content-addressing means identical blocks would collapse to
     * the same hash, which would defeat these tests' per-chunk bookkeeping).
     */
    private function writeDistinctArtifact(string $name, int $chunkBytes, int $chunkCount): string
    {
        $bytes = '';
        for ($i = 0; $i < $chunkCount; $i++) {
            $bytes .= str_pad((string) $i, $chunkBytes, '0', STR_PAD_LEFT);
        }
        $path = $this->scratchDir . DIRECTORY_SEPARATOR . $name;
        file_put_contents($path, $bytes);
        return $path;
    }

    /**
     * Ordered blake3 hash list for a manifest entry, by logical path. Robust
     * to the synthetic environment.json artifact encryptChunks() always
     * appends, callers ask for exactly the entry they care about instead
     * of assuming a fixed total chunk count.
     *
     * @param array<string,mixed> $encCursor
     * @return list<string>
     */
    private function hashesFor(array $encCursor, string $logical): array
    {
        foreach (($encCursor['entries'] ?? []) as $entry) {
            if (($entry['path'] ?? null) === $logical) {
                return array_column($entry['chunks'] ?? [], 'blake3');
            }
        }
        self::fail("no manifest entry for path: {$logical}");
    }

    /**
     * Mirrors EncryptAndUpload::chunkPath() for ENCRYPT_CHUNKS=false (the
     * V0/self-hosted default): `chunks-<hash>.bin` under the scratch dir.
     */
    private function chunkPathFor(string $hash): string
    {
        return $this->scratchDir . DIRECTORY_SEPARATOR . 'chunks-' . $hash . '.bin';
    }

    /**
     * (a) Mid-upload-interrupt resume: seed the resume cursor with hashes
     * whose local files are already gone (as GH #283's fix would leave
     * behind after a durably-checkpointed batch), present a presign
     * response that RE-INCLUDES those hashes plus new ones (the CP's
     * manifest-scoped dedup is blind to in-flight PUTs), and confirm the
     * resumed uploadChunks() skips the already-uploaded hashes instead of
     * throwing "missing local chunk for upload", uploads the remaining
     * hashes, and completes.
     */
    public function test_resume_skips_already_uploaded_hashes_whose_local_file_is_gone(): void
    {
        $artifactPath = $this->writeDistinctArtifact('blob.bin', 16, 4);
        $transport    = $this->transport();
        $pipeline     = $this->makePipeline(16, $transport);
        $noop         = $this->noopProgress();

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'blob.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);

        $blobHashes = $this->hashesFor($encCursor, 'blob.bin');
        self::assertCount(4, $blobHashes, 'expected 4 distinct 16-byte blocks to produce 4 distinct chunks');

        // Simulate the state a GH #283-fixed prior pass leaves behind after
        // a worker kill immediately following a durable checkpoint: the
        // first two hashes are recorded in the persisted cursor AND their
        // local files are already gone.
        $alreadyUploaded = array_slice($blobHashes, 0, 2);
        foreach ($alreadyUploaded as $hash) {
            self::assertTrue(unlink($this->chunkPathFor($hash)));
        }

        $resume = [
            'scratch_dir'     => $this->scratchDir,
            'uploaded_hashes' => $alreadyUploaded,
        ];

        // The CP re-presigns ALL hashes (including the two already
        // uploaded), exactly the root-cause scenario: manifest-scoped
        // dedup is blind to in-flight presigned PUTs from the interrupted
        // run.
        $upCursor = $pipeline->uploadChunks($encCursor, $resume, $noop);

        self::assertTrue($upCursor['done'] ?? false, 'resumed uploadChunks must complete, not throw');
        foreach ($blobHashes as $hash) {
            self::assertContains($hash, $upCursor['uploaded_hashes']);
        }

        // The already-uploaded hashes must be SKIPPED, not re-PUT.
        self::assertArrayNotHasKey($blobHashes[0], $transport->puts, 'an already-uploaded hash must be skipped, not re-PUT');
        self::assertArrayNotHasKey($blobHashes[1], $transport->puts, 'an already-uploaded hash must be skipped, not re-PUT');
        // The genuinely new hashes must have been uploaded.
        self::assertArrayHasKey($blobHashes[2], $transport->puts);
        self::assertArrayHasKey($blobHashes[3], $transport->puts);
    }

    /**
     * (b) Delete-after-persist invariant: for every hash newly reported in a
     * checkpoint call, its local chunk file must still exist AT THE MOMENT
     * the checkpoint fires (deletion happens strictly after the checkpoint
     * callback returns). Uses > PERSIST_EVERY_UPLOAD chunks so the ordering
     * is exercised across a real mid-loop checkpoint, not just the final
     * end-of-sweep flush.
     */
    public function test_delete_after_persist_ordering(): void
    {
        $chunkBytes = 32;
        $chunkCount = 20; // > PERSIST_EVERY_UPLOAD (16): forces a mid-loop checkpoint + a tail flush.

        $artifactPath = $this->writeDistinctArtifact('blob.bin', $chunkBytes, $chunkCount);
        $transport    = $this->transport();
        $pipeline     = $this->makePipeline($chunkBytes, $transport);
        $noop         = $this->noopProgress();

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'blob.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);

        $previouslyPersisted = [];
        $violations          = [];
        $checkpoint          = function (array $partial) use (&$previouslyPersisted, &$violations): void {
            $now       = $partial['uploaded_hashes'] ?? [];
            $newHashes = array_diff($now, $previouslyPersisted);
            foreach ($newHashes as $hash) {
                // checkpointAndFlush() must call the checkpoint BEFORE it
                // deletes any file, so at this exact moment, every
                // newly-persisted hash's local file must still be present.
                if (!is_file($this->chunkPathFor($hash))) {
                    $violations[] = $hash;
                }
            }
            $previouslyPersisted = $now;
        };

        $upCursor = $pipeline->uploadChunks(
            $encCursor,
            ['scratch_dir' => $this->scratchDir],
            $noop,
            $checkpoint
        );

        self::assertTrue($upCursor['done'] ?? false);
        self::assertSame(
            [],
            $violations,
            'a chunk file must never be deleted before its hash is confirmed persisted via the checkpoint callback'
        );

        // Once uploadChunks() has fully returned, every uploaded hash's
        // local file has, in fact, been cleaned up (deletion does happen,
        // just strictly after persistence).
        foreach ($upCursor['uploaded_hashes'] as $hash) {
            self::assertFileDoesNotExist(
                $this->chunkPathFor($hash),
                'local chunk file must be deleted once its hash is durably persisted'
            );
        }
    }

    /**
     * (c) The cursor is persisted mid-loop, not only at the end: for a run
     * spanning multiple PERSIST_EVERY_UPLOAD batches, the checkpoint
     * callback must fire more than once, and at least the first call must
     * report a genuinely partial cursor (done=false, fewer uploaded_hashes
     * than the total).
     */
    public function test_checkpoint_persists_mid_loop_not_only_at_the_end(): void
    {
        $chunkBytes = 32;
        $chunkCount = 20;

        $artifactPath = $this->writeDistinctArtifact('blob.bin', $chunkBytes, $chunkCount);
        $transport    = $this->transport();
        $pipeline     = $this->makePipeline($chunkBytes, $transport);
        $noop         = $this->noopProgress();

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'blob.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);
        $totalHashes = count($encCursor['all_hashes']);

        $persistEvery = (int) (new ReflectionClassConstant(EncryptAndUpload::class, 'PERSIST_EVERY_UPLOAD'))->getValue();
        self::assertGreaterThan($persistEvery, $totalHashes, 'test must exercise at least one mid-loop checkpoint');

        $calls      = [];
        $checkpoint = function (array $partial) use (&$calls): void {
            $calls[] = $partial;
        };

        $upCursor = $pipeline->uploadChunks(
            $encCursor,
            ['scratch_dir' => $this->scratchDir],
            $noop,
            $checkpoint
        );

        self::assertTrue($upCursor['done'] ?? false);
        self::assertGreaterThanOrEqual(
            2,
            count($calls),
            'checkpoint must fire more than once across multiple PERSIST_EVERY_UPLOAD batches'
        );

        $firstCall = $calls[0];
        self::assertFalse($firstCall['done'] ?? true, 'a mid-loop checkpoint must report done=false');
        self::assertLessThan(
            $totalHashes,
            count($firstCall['uploaded_hashes'] ?? []),
            'the first checkpoint must fire while the cursor is still genuinely partial'
        );
    }

    /**
     * (d) Dedup second-sweep hashes (the CP already had them, so they never
     * appear in the presign response) are still handled correctly: counted
     * as dedup hits, their local files cleaned up, and never mistaken for a
     * missing chunk.
     */
    public function test_dedup_second_sweep_hashes_are_not_treated_as_missing(): void
    {
        $chunkBytes   = 16;
        $artifactPath = $this->writeDistinctArtifact('blob.bin', $chunkBytes, 4);
        $transport    = $this->transport();
        $pipeline     = $this->makePipeline($chunkBytes, $transport);
        $noop         = $this->noopProgress();

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'blob.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);

        $blobHashes = $this->hashesFor($encCursor, 'blob.bin');
        self::assertCount(4, $blobHashes);
        $envHashes = $this->hashesFor($encCursor, 'environment.json');

        // The CP reports it already has the first two blob chunks (a dedup
        // hit); they're deliberately OMITTED from the presign response.
        $dedupHashes   = array_slice($blobHashes, 0, 2);
        $missingHashes = array_merge(array_slice($blobHashes, 2), $envHashes);
        $transport->onlyHashes = $missingHashes;

        $calls      = [];
        $checkpoint = function (array $partial) use (&$calls): void {
            $calls[] = $partial;
        };

        $upCursor = $pipeline->uploadChunks(
            $encCursor,
            ['scratch_dir' => $this->scratchDir],
            $noop,
            $checkpoint
        );

        self::assertTrue($upCursor['done'] ?? false, 'a dedup hit must never surface as a missing-chunk failure');
        self::assertSame(2, $upCursor['chunks_dedup'], 'the two CP-already-has hashes must be counted as dedup hits');
        self::assertSame(count($missingHashes), $upCursor['chunks_put']);

        foreach ($dedupHashes as $hash) {
            self::assertArrayNotHasKey($hash, $transport->puts, 'a dedup hash must never be PUT');
            self::assertContains($hash, $upCursor['uploaded_hashes']);
            self::assertFileDoesNotExist($this->chunkPathFor($hash), 'a dedup-hit local chunk file must still be cleaned up');
        }
        foreach ($missingHashes as $hash) {
            self::assertArrayHasKey($hash, $transport->puts);
        }

        self::assertNotEmpty($calls, 'the dedup second sweep must also checkpoint, not just the PUT sweep');
    }

    /**
     * (e) GH #283 follow-up: saveTaskState() has silent no-op paths (no
     * $wpdb, no resolvable table name, a json_encode failure) that persist
     * nothing and return normally. The checkpoint closure in
     * TaskRunner::runEncryptingUploading() reacts to saveTaskState()
     * returning false by throwing a RuntimeException, so a failed persist
     * reaches checkpointAndFlush() as a thrown exception, exactly like this
     * stub simulates. Proves the invariant end to end: when persistence did
     * not durably happen, checkpointAndFlush() must NOT delete the queued
     * local chunk files. Fails against a naive implementation that deletes
     * the pending queue regardless of what the checkpoint callback does.
     */
    public function test_checkpoint_failure_leaves_pending_chunks_on_disk(): void
    {
        $chunkBytes   = 16;
        $artifactPath = $this->writeDistinctArtifact('blob.bin', $chunkBytes, 4);
        $transport    = $this->transport();
        $pipeline     = $this->makePipeline($chunkBytes, $transport);
        $noop         = $this->noopProgress();

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'blob.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);

        $blobHashes = $this->hashesFor($encCursor, 'blob.bin');
        self::assertCount(4, $blobHashes);
        $envHashes = $this->hashesFor($encCursor, 'environment.json');

        // Simulates saveTaskState() signaling a failed persist: the
        // caller's checkpoint closure throws instead of returning normally.
        $failingCheckpoint = function (array $partial): void {
            throw new \RuntimeException('GH283: upload cursor persist failed, keeping local chunks for resume');
        };

        $threw = false;
        try {
            $pipeline->uploadChunks(
                $encCursor,
                ['scratch_dir' => $this->scratchDir],
                $noop,
                $failingCheckpoint
            );
        } catch (\RuntimeException $e) {
            $threw = true;
        }
        self::assertTrue($threw, 'uploadChunks() must propagate a checkpoint persist failure, not swallow it');

        // The whole point of the fix: none of this pass's local chunk files
        // were deleted, because checkpointAndFlush() never reaches the
        // delete step once the checkpoint callback throws.
        foreach (array_merge($blobHashes, $envHashes) as $hash) {
            self::assertFileExists(
                $this->chunkPathFor($hash),
                'a local chunk file must stay on disk when its checkpoint persist failed'
            );
        }
    }

    /**
     * (f) End-to-end interrupt + resume, seeded from what a mid-loop
     * checkpoint actually persisted. This is the scenario GH #283 fixes:
     * pre-fix code deleted each local chunk file immediately after a
     * successful PUT and only persisted the uploaded_hashes cursor once
     * uploadChunks() fully RETURNED, so an interrupt this early would have
     * captured an EMPTY cursor and a resume seeded from it would have hit
     * the "missing local chunk for upload" throw once it reached the
     * already-deleted, not-yet-recorded hashes.
     *
     * Drives a real checkpoint that records the FIRST partial cursor it
     * sees, then throws to simulate a worker kill right after that first
     * durable persist. Asserts the captured cursor is genuinely partial
     * (proves persistence already happened mid-loop, not only at the end),
     * then feeds that exact cursor into a fresh uploadChunks() call and
     * asserts it completes cleanly.
     */
    public function test_interrupted_resume_seeded_from_mid_loop_checkpoint_completes(): void
    {
        $chunkBytes = 32;
        $chunkCount = 20; // > PERSIST_EVERY_UPLOAD (16): forces a real mid-loop checkpoint.

        $artifactPath = $this->writeDistinctArtifact('blob.bin', $chunkBytes, $chunkCount);
        $transport    = $this->transport();
        $pipeline     = $this->makePipeline($chunkBytes, $transport);
        $noop         = $this->noopProgress();

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'blob.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);
        $totalHashes = count($encCursor['all_hashes']);

        $persistEvery = (int) (new ReflectionClassConstant(EncryptAndUpload::class, 'PERSIST_EVERY_UPLOAD'))->getValue();
        self::assertGreaterThan($persistEvery, $totalHashes, 'test must exercise at least one mid-loop checkpoint');

        $capturedCursor = null;
        $interruptingCheckpoint = function (array $partial) use (&$capturedCursor): void {
            $capturedCursor = $partial;
            throw new \RuntimeException('simulated worker kill right after the first durable checkpoint');
        };

        $interrupted = false;
        try {
            $pipeline->uploadChunks(
                $encCursor,
                ['scratch_dir' => $this->scratchDir],
                $noop,
                $interruptingCheckpoint
            );
        } catch (\RuntimeException $e) {
            $interrupted = true;
        }
        self::assertTrue($interrupted, 'the simulated interrupt must abort uploadChunks()');
        self::assertNotNull($capturedCursor, 'the checkpoint must have fired at least once before the interrupt');

        $firstBatchHashes = $capturedCursor['uploaded_hashes'] ?? [];
        self::assertSame(
            $persistEvery,
            count($firstBatchHashes),
            'the first checkpoint must fire after exactly PERSIST_EVERY_UPLOAD chunks, proving persistence already happened mid-loop (pre-fix code persisted nothing until the pass fully returned, so this would have been empty)'
        );

        // Resume: a fresh uploadChunks() call seeded from exactly what the
        // interrupted run's first checkpoint persisted.
        $resumeCursor = $pipeline->uploadChunks(
            $encCursor,
            [
                'scratch_dir'     => $this->scratchDir,
                'uploaded_hashes' => $firstBatchHashes,
            ],
            $noop
        );

        self::assertTrue(
            $resumeCursor['done'] ?? false,
            'a resume seeded from the mid-loop-persisted cursor must complete, not throw missing local chunk for upload'
        );
        foreach ($encCursor['all_hashes'] as $hash) {
            self::assertContains($hash, $resumeCursor['uploaded_hashes']);
        }
    }

    /**
     * Recursively delete a directory tree (used by tear_down).
     */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            if (is_file($dir) || is_link($dir)) {
                @unlink($dir);
            }
            return;
        }
        $entries = scandir($dir);
        if ($entries === false) {
            return;
        }
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $this->rrmdir($dir . DIRECTORY_SEPARATOR . $entry);
        }
        @rmdir($dir);
    }
}
