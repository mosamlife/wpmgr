<?php
/**
 * Unit tests for the v0.8.6 download_artifacts retry-with-backoff path in
 * RestoreRunner. The public surface (`run()`) drives a full state machine
 * that needs a live wpdb + an Ed25519 keystore + a CP endpoint; we don't
 * want to fake all of that just to exercise the retry loop. So we reach in
 * via reflection and call `fetchChunkWithRetries()` against a stubbed
 * BackupTransport. That's the contract this test guards:
 *
 *   - two transient 5xxs followed by a 200 are absorbed silently (the
 *     restore survives a Cloudflare-tunnel blip);
 *   - 5 consecutive 5xxs throw with a structured "HTTP <status> from <host>"
 *     message so the operator can grep for it;
 *   - a terminal 4xx (404) throws immediately on attempt 1 — no point
 *     hammering an URL that's permanently broken;
 *   - the sleeper is invoked between attempts with the expected
 *     exponential-backoff delays (so a flaky network actually gets backoff
 *     before retry, not a tight loop).
 *
 * The Sleeper is replaced with a no-op recorder so the test runs in
 * milliseconds, not the 1+2+4+8 = 15 s of real wall clock the production
 * backoff would inject.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use ReflectionClass;
use WPMgr\Agent\Backup\RestoreRunner;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 */
final class RestoreRunnerRetryTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Two transient 502s followed by a 200 must NOT throw — the runner
     * absorbs the blip silently and returns the chunk bytes. Mirrors the
     * "Cloudflare tunnel hiccup on chunk 187 of 381" production scenario.
     */
    public function test_retries_on_transient_5xx_then_succeeds(): void
    {
        $payload   = "the-chunk-bytes";
        $transport = $this->stubTransport([
            ['ok' => false, 'status' => 502, 'body' => '', 'error' => '', 'body_excerpt' => 'Bad Gateway', 'host' => 's3.test', 'retryable' => true],
            ['ok' => false, 'status' => 502, 'body' => '', 'error' => '', 'body_excerpt' => 'Bad Gateway', 'host' => 's3.test', 'retryable' => true],
            ['ok' => true,  'status' => 200, 'body' => $payload, 'error' => '', 'body_excerpt' => '', 'host' => 's3.test', 'retryable' => false],
        ]);

        [$runner, $delays] = $this->buildRunnerWithRecorder();

        $out = $this->callFetch($runner, $transport, 'wp-content.part001.zip', 5);

        $this->assertSame($payload, $out);
        // Two retry sleeps: 1s then 2s (exponential).
        $this->assertSame([1000, 2000], $delays->getArrayCopy());
    }

    /**
     * All 5 attempts return a 502 — the call must throw, and the message
     * must include "HTTP 502 from s3.test" + the body excerpt so the
     * operator can grep the SSE detail / WP error_log without spelunking.
     */
    public function test_terminal_failure_after_max_attempts_message_is_structured(): void
    {
        $transport = $this->stubTransport(array_fill(0, 5, [
            'ok' => false, 'status' => 502, 'body' => '', 'error' => '',
            'body_excerpt' => 'Bad Gateway', 'host' => 's3.test', 'retryable' => true,
        ]));

        [$runner, $delays] = $this->buildRunnerWithRecorder();

        try {
            $this->callFetch($runner, $transport, 'wp-content.part001.zip', 5);
            $this->fail('expected RuntimeException from terminal failure');
        } catch (\RuntimeException $e) {
            $msg = $e->getMessage();
            $this->assertStringContainsString('after 5 attempts', $msg);
            $this->assertStringContainsString('HTTP 502', $msg);
            $this->assertStringContainsString('s3.test', $msg);
            $this->assertStringContainsString('Bad Gateway', $msg);
            // Should mention which chunk failed (logical path + index).
            $this->assertStringContainsString('wp-content.part001.zip', $msg);
        }
        // Four backoff sleeps between five attempts: 1s/2s/4s/8s. (No
        // sleep after the final failed attempt — we throw instead.)
        $this->assertSame([1000, 2000, 4000, 8000], $delays->getArrayCopy());
    }

    /**
     * 404 is terminal — the URL maps to a chunk that's genuinely gone (or
     * to a bucket the presign was malformed against). Retrying is pure
     * latency cost with no chance of success, so the runner MUST throw on
     * attempt 1.
     */
    public function test_404_is_terminal_no_retry(): void
    {
        $transport = $this->stubTransport([
            ['ok' => false, 'status' => 404, 'body' => '', 'error' => '',
             'body_excerpt' => 'NoSuchKey', 'host' => 's3.test', 'retryable' => false],
        ]);

        [$runner, $delays] = $this->buildRunnerWithRecorder();

        try {
            $this->callFetch($runner, $transport, 'wp-content.part001.zip', 5);
            $this->fail('expected RuntimeException from 404');
        } catch (\RuntimeException $e) {
            $this->assertStringContainsString('HTTP 404', $e->getMessage());
            $this->assertStringContainsString('NoSuchKey', $e->getMessage());
        }
        // No retries on a non-retryable failure — no sleeps recorded.
        $this->assertSame([], $delays->getArrayCopy());
    }

    /**
     * A WP_Error-equivalent network failure (status = 0) is retryable.
     * After exhausting attempts the message must say "transport error"
     * rather than "HTTP 0" (which would confuse a grep).
     */
    public function test_transport_error_message_does_not_say_http_zero(): void
    {
        $transport = $this->stubTransport(array_fill(0, 5, [
            'ok' => false, 'status' => 0, 'body' => '', 'error' => 'cURL error 28: timeout',
            'body_excerpt' => '', 'host' => 's3.test', 'retryable' => true,
        ]));

        [$runner, ] = $this->buildRunnerWithRecorder();

        try {
            $this->callFetch($runner, $transport, 'db.sql.gz', 0);
            $this->fail('expected RuntimeException');
        } catch (\RuntimeException $e) {
            $msg = $e->getMessage();
            $this->assertStringContainsString('transport error', $msg);
            $this->assertStringContainsString('cURL error 28', $msg);
            $this->assertStringNotContainsString('HTTP 0', $msg);
        }
    }

    // ---------------------------------------------------------------
    // Helpers
    // ---------------------------------------------------------------

    /**
     * Build a RestoreRunner with the bare-minimum params + replace the
     * sleeper with a recorder that captures the delays the retry loop
     * asked for (so the test asserts exponential backoff without waiting
     * actual seconds). The delays are recorded into an object property
     * (not a by-ref array) so the reference survives the array-return.
     *
     * @return array{0:RestoreRunner,1:\ArrayObject<int,int>}
     */
    private function buildRunnerWithRecorder(): array
    {
        $runner = new RestoreRunner([
            'snapshot_id' => '11111111-1111-1111-1111-111111111111',
            'restore_id'  => '22222222-2222-2222-2222-222222222222',
            'kind'        => 'files',
            'scratch_dir' => sys_get_temp_dir(),
        ]);

        /** @var \ArrayObject<int,int> $delays */
        $delays = new \ArrayObject();
        $runner->setSleeper(static function (int $ms) use ($delays): void {
            $delays->append($ms);
        });

        return [$runner, $delays];
    }

    /**
     * Build a stub BackupTransport whose getChunkWithStatus returns the
     * next pre-canned result on each call. Lets the test simulate "two
     * transient failures, then success" without a real HTTP server.
     *
     * @param list<array{ok:bool,status:int,body:string,error:string,body_excerpt:string,host:string,retryable:bool}> $script
     */
    private function stubTransport(array $script): BackupTransport
    {
        return new class($script) extends BackupTransport {
            /** @var list<array<string,mixed>> */
            private array $script;
            private int $cursor = 0;

            /** @param list<array<string,mixed>> $script */
            public function __construct(array $script)
            {
                // Intentionally skip parent::__construct — we don't need
                // a real Signer for the GET path; getChunkWithStatus on
                // the real class doesn't touch the signer at all.
                $this->script = $script;
            }

            public function getChunkWithStatus(string $presignedUrl): array
            {
                if ($this->cursor >= count($this->script)) {
                    throw new \LogicException('stub: ran out of scripted responses (cursor=' . $this->cursor . ')');
                }
                return $this->script[$this->cursor++];
            }
        };
    }

    /**
     * Invoke the private fetchChunkWithRetries() via reflection. The
     * hash we pass is a valid-looking 64-char hex so the truncation in
     * the error message (substr 0,12) renders cleanly in assertions.
     */
    private function callFetch(RestoreRunner $runner, BackupTransport $transport, string $logical, int $chunkIdx): string
    {
        // PHP 8.1+ no longer needs setAccessible(true) for private methods
        // accessed via reflection — and PHP 8.5 warns when you call it.
        $rc  = new ReflectionClass(RestoreRunner::class);
        $m   = $rc->getMethod('fetchChunkWithRetries');
        return (string) $m->invoke(
            $runner,
            $transport,
            'https://s3.test/chunks/example?sig=redacted',
            str_repeat('a', 64),
            $logical,
            $chunkIdx
        );
    }
}
