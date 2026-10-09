<?php
/**
 * GH #369: a chunk PUT that fails names its cause, and a transient failure is
 * retried instead of failing the whole backup.
 *
 * Before the fix, BackupTransport::putChunk() turned every PUT outcome into a
 * bool and EncryptAndUpload::uploadChunks() threw `PUT failed for chunk
 * <hash>` on the first false: no HTTP status, no S3 error code, no transport
 * error, no host, no retry, and no checkpoint of the chunks already uploaded.
 *
 * These tests drive uploadChunks() through the production path:
 * EncryptAndUpload -> CpDestination -> BackupTransport::putChunkWithStatus()
 * -> wp_remote_request(). Only wp_remote_request() (the network) and the
 * signed presign callback are faked, so the status parsing, S3 <Code>
 * extraction, retry classification and URL redaction under test are the real
 * ones.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use ReflectionClassConstant;
use WPMgr\Agent\Backup\EncryptAndUpload;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\EncryptAndUpload
 * @covers \WPMgr\Agent\Backup\Destinations\CpDestination
 * @covers \WPMgr\Agent\Support\BackupTransport
 */
final class EncryptAndUploadPutFailureTest extends TestCase
{
    /** Plaintext chunk size: large enough that environment.json is one or two chunks. */
    private const CHUNK_BYTES = 4096;

    private const PUT_PREFIX = 'https://s3.example/put/';

    private const SIGNATURE_DOES_NOT_MATCH = '<?xml version="1.0" encoding="UTF-8"?><Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided.</Message></Error>';

    private const EXPIRED = '<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Request has expired</Message><Expires>2026-10-09T10:00:00Z</Expires></Error>';

    private string $scratchDir = '';

    /** @var list<array{url:string,args:array<string,mixed>}> every wp_remote_request() call, in order */
    private array $requests = [];

    /** @var list<int> backoff delays handed to the sleeper, in ms */
    private array $sleeps = [];

    /** @var list<array<string,mixed>> every progress detail emitted */
    private array $progress = [];

    /** @var list<array<string,mixed>> every checkpoint cursor persisted */
    private array $checkpoints = [];

    /** @var list<string> ordered log of 'put:<hash>' and 'checkpoint' events */
    private array $events = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_remote_retrieve_body')->alias(static function ($response): string {
            return is_array($response) && isset($response['body']) && is_string($response['body']) ? $response['body'] : '';
        });

        $this->requests    = [];
        $this->sleeps      = [];
        $this->progress    = [];
        $this->checkpoints = [];
        $this->events      = [];

        $this->scratchDir = sys_get_temp_dir() . '/wpmgr-encrypt-upload-put-' . bin2hex(random_bytes(6));
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

    // ------------------------------------------------------------------
    // The regression tests named in the brief: (a) to (d).
    // ------------------------------------------------------------------

    /**
     * (a) The first PUT gets a 503 once, then 200. Before the fix this threw
     * `PUT failed for chunk` on the 503; now the same chunk is retried on the
     * same URL after a backoff and a heartbeat, and the pass completes.
     */
    public function test_a_transient_503_is_retried_and_every_chunk_is_uploaded(): void
    {
        $this->respondWith(function (string $url, int $n) {
            return $n === 1 ? $this->response(503, '<Error><Code>SlowDown</Code></Error>') : $this->response(200);
        });
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);

        $up = $this->upload($transport, $enc);

        self::assertTrue($up['done'] ?? false, 'one 503 must not fail the backup');
        $all = $enc['all_hashes'];
        self::assertEqualsCanonicalizing($all, $up['uploaded_hashes']);
        self::assertSame(count($all), $up['chunks_put']);
        self::assertCount(count($all) + 1, $this->requests, 'every chunk is PUT once, plus one retry');
        self::assertSame(
            $this->requests[0]['url'],
            $this->requests[1]['url'],
            'the 503 is retried on the same presigned URL'
        );
        $succeeded = array_map(fn (array $r): string => $this->hashOfUrl($r['url']), array_slice($this->requests, 1));
        self::assertEqualsCanonicalizing($all, $succeeded, 'every hash got its own successful PUT');
        self::assertSame([2000], $this->sleeps, 'one backoff before the retry');
        self::assertSame([1], $this->retryHeartbeats(), 'a heartbeat is emitted between the two attempts');
        self::assertCount(1, $transport->presignCalls, 'a 5xx is retried, not re-presigned');
        foreach ($this->requests as $request) {
            self::assertSame('PUT', $request['args']['method'] ?? null);
            self::assertSame($this->transportConstant('PUT_TIMEOUT'), $request['args']['timeout'] ?? null);
        }
    }

    /**
     * (b) A 403 SignatureDoesNotMatch from s3.example: the failure names the
     * status, the S3 code and the host, and never the URL, its query string
     * or the storage's free-text message. Before the fix the message was only
     * the chunk hash.
     */
    public function test_b_403_message_names_status_code_and_host_but_not_the_url(): void
    {
        $this->respondWith(fn (): array => $this->response(403, self::SIGNATURE_DOES_NOT_MATCH));
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);

        $message = $this->uploadExpectingFailure($transport, $enc);

        self::assertStringContainsString('403', $message);
        self::assertStringContainsString('SignatureDoesNotMatch', $message);
        self::assertStringContainsString('s3.example', $message);
        self::assertStringNotContainsString('/put/', $message);
        self::assertStringNotContainsString('?X-Amz-', $message);
        self::assertStringNotContainsString('X-Amz-Signature', $message);
        self::assertStringNotContainsString('The request signature we calculated', $message, 'the storage body is never forwarded');
        self::assertCount(1, $this->requests, 'a terminal 4xx is not retried');
        self::assertSame([], $this->sleeps);
        self::assertCount(1, $transport->presignCalls, 'SignatureDoesNotMatch is not re-presigned');
    }

    /**
     * (c) A timeout on every attempt: the attempts stop at the bound, the
     * message carries the transport error, and the checkpoint runs after the
     * last attempt and before the throw, persisting the chunk that did upload.
     */
    public function test_c_timeout_on_every_attempt_stops_at_the_bound_and_checkpoints_before_throwing(): void
    {
        $this->respondWith(function (string $url, int $n) {
            return $n === 1
                ? $this->response(200)
                : new \WP_Error('http_request_failed', 'cURL error 28: Operation timed out after 120001 milliseconds with 0 out of 4096 bytes received');
        });
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);
        $first     = $this->hashOfUrl($transport->urlsInFirstPresign()[0]);
        $failing   = $this->hashOfUrl($transport->urlsInFirstPresign()[1]);

        $message = $this->uploadExpectingFailure($transport, $enc);

        $maxAttempts = $this->uploaderConstant('PUT_MAX_ATTEMPTS');
        self::assertCount(1 + $maxAttempts, $this->requests, 'one success, then exactly PUT_MAX_ATTEMPTS attempts on the failing chunk');
        foreach (array_slice($this->requests, 1) as $request) {
            self::assertSame($failing, $this->hashOfUrl($request['url']));
        }
        self::assertStringContainsString('cURL error 28', $message);
        self::assertStringContainsString('s3.example', $message);
        self::assertStringContainsString($maxAttempts . ' attempts', $message);
        self::assertStringContainsString(
            'cURL error 28',
            substr($message, 0, 240),
            'TaskRunner keeps 240 characters of a failure message; the cause must be inside them'
        );
        self::assertSame([2000, 4000], $this->sleeps);
        self::assertSame([1, 2], $this->retryHeartbeats(), 'a heartbeat after each failed attempt that is retried');

        // The checkpoint ran after the final attempt and before the throw.
        self::assertNotEmpty($this->checkpoints, 'the checkpoint must run before the failure is thrown');
        self::assertSame('checkpoint', end($this->events), 'the last thing before the throw is the checkpoint');
        $lastPut = max(array_keys($this->events, 'put:' . $failing, true));
        self::assertGreaterThan($lastPut, (int) array_key_last($this->events), 'the checkpoint follows the last attempt');
        $persisted = end($this->checkpoints);
        self::assertFalse($persisted['done']);
        self::assertContains($first, $persisted['uploaded_hashes'], 'the chunk that did upload survives the failure');
        self::assertNotContains($failing, $persisted['uploaded_hashes']);
    }

    /**
     * (d) A hash whose PUT never returned 2xx is never in any checkpoint's
     * uploaded set, and its local chunk file is left on disk; the hash that
     * did get a 2xx is persisted and only then cleaned up.
     */
    public function test_d_a_hash_without_a_2xx_is_never_recorded_as_uploaded(): void
    {
        $this->respondWith(function (string $url, int $n) {
            return $n === 1 ? $this->response(200) : $this->response(403, self::SIGNATURE_DOES_NOT_MATCH);
        });
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);
        $first     = $this->hashOfUrl($transport->urlsInFirstPresign()[0]);
        $failing   = $this->hashOfUrl($transport->urlsInFirstPresign()[1]);

        $this->uploadExpectingFailure($transport, $enc);

        self::assertNotEmpty($this->checkpoints, 'the failure path checkpoints what did upload');
        foreach ($this->checkpoints as $cursor) {
            self::assertNotContains($failing, $cursor['uploaded_hashes'], 'a hash is recorded only after its own 2xx');
        }
        self::assertContains($first, end($this->checkpoints)['uploaded_hashes']);
        self::assertFileExists($this->chunkPathFor($failing), 'the failed chunk stays on disk for a retry');
        self::assertFileDoesNotExist($this->chunkPathFor($first), 'the uploaded chunk is removed after its persist');
    }

    // ------------------------------------------------------------------
    // Re-presign on a 403 a fresh URL may get past.
    // ------------------------------------------------------------------

    /**
     * An expired presigned URL (403 AccessDenied, "Request has expired") gets
     * one fresh single-hash presign and one more PUT, which succeeds.
     */
    public function test_expired_url_is_re_presigned_once_and_the_upload_completes(): void
    {
        $this->respondWith(function (string $url, int $n) {
            return $n === 1 ? $this->response(403, self::EXPIRED) : $this->response(200);
        });
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);
        $first     = $this->hashOfUrl($transport->urlsInFirstPresign()[0]);

        $up = $this->upload($transport, $enc);

        self::assertTrue($up['done'] ?? false, 'a fresh URL gets past an expired one');
        self::assertCount(2, $transport->presignCalls);
        self::assertSame([$first], $transport->presignCalls[1], 're-presign asks for that one hash only');
        self::assertStringContainsString('sig-round-1', $this->requests[0]['url']);
        self::assertStringContainsString('sig-round-2', $this->requests[1]['url'], 'the retry uses the fresh URL');
        self::assertSame($first, $this->hashOfUrl($this->requests[1]['url']));
        self::assertSame(count($enc['all_hashes']), $up['chunks_put']);
        self::assertSame([], $this->sleeps, 'a re-presign needs no backoff');
        self::assertSame([1], $this->retryHeartbeats(), 'a heartbeat precedes the re-presign');
    }

    /**
     * The re-presign is bounded to once: a second 403 fails the backup with
     * the status and code, after two attempts.
     */
    public function test_re_presign_is_attempted_once_then_the_403_is_reported(): void
    {
        $this->respondWith(fn (): array => $this->response(403, self::EXPIRED));
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);

        $message = $this->uploadExpectingFailure($transport, $enc);

        self::assertCount(2, $transport->presignCalls, 'one bulk presign and exactly one re-presign');
        self::assertCount(2, $this->requests);
        self::assertStringContainsString('HTTP 403 AccessDenied', $message);
        self::assertStringContainsString('2 attempts', $message);
        self::assertStringNotContainsString('Request has expired', $message, 'the storage body is never forwarded');
    }

    /**
     * When the re-presign answers that the CP already holds the chunk, it is
     * counted as stored (a dedup hit, exactly like the bulk presign's), not
     * as a PUT, and the pass completes.
     */
    public function test_re_presign_that_finds_the_chunk_already_stored_counts_it_as_a_dedup_hit(): void
    {
        $this->respondWith(function (string $url, int $n) {
            return $n === 1 ? $this->response(403, self::EXPIRED) : $this->response(200);
        });
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);
        $first     = $this->hashOfUrl($transport->urlsInFirstPresign()[0]);
        $transport->storedOnRepresign = [$first];

        $up = $this->upload($transport, $enc);

        self::assertTrue($up['done'] ?? false);
        self::assertContains($first, $up['uploaded_hashes']);
        self::assertSame(1, $up['chunks_dedup']);
        self::assertSame(count($enc['all_hashes']) - 1, $up['chunks_put']);
        self::assertCount(count($enc['all_hashes']), $this->requests, 'no PUT is made for the already-stored chunk after its 403');
    }

    /**
     * A 404 is terminal: no retry, no backoff, no re-presign.
     */
    public function test_404_is_terminal(): void
    {
        $this->respondWith(fn (): array => $this->response(404, '<Error><Code>NoSuchBucket</Code></Error>'));
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);

        $message = $this->uploadExpectingFailure($transport, $enc);

        self::assertCount(1, $this->requests);
        self::assertSame([], $this->sleeps);
        self::assertCount(1, $transport->presignCalls);
        self::assertStringContainsString('HTTP 404 NoSuchBucket', $message);
        self::assertStringContainsString('1 attempt,', $message);
    }

    /**
     * A transport that echoes the request URL into its error (a filter on
     * pre_http_request, a proxy) must not carry the presigned URL into the
     * failure message.
     */
    public function test_a_transport_error_that_echoes_the_url_is_redacted_in_the_failure_message(): void
    {
        $this->respondWith(static function (string $url): \WP_Error {
            return new \WP_Error('http_request_failed', 'Blocked outbound request to ' . $url);
        });
        $transport = $this->transport();
        $enc       = $this->encrypt($transport, 3);

        $message = $this->uploadExpectingFailure($transport, $enc);

        self::assertStringContainsString('Blocked outbound request to s3.example', $message);
        self::assertStringNotContainsString('/put/', $message);
        self::assertStringNotContainsString('sig-round', $message);
        self::assertStringNotContainsString('X-Amz-Signature', $message);
    }

    /**
     * TaskRunner keeps 240 bytes of a failure message, and the report is
     * JSON, which refuses invalid UTF-8. With a long storage host and a long
     * multi-byte (translated) transport error, the message still fits in 240
     * bytes, is valid UTF-8, encodes as JSON, and keeps the host and the
     * start of the cause.
     */
    public function test_long_multibyte_failure_message_fits_the_report_and_stays_valid_utf8(): void
    {
        $longHost = str_repeat('backups', 12) . '.s3.example';
        $error    = str_repeat('Сбой соединения с хранилищем; ', 12);
        $this->respondWith(static fn (): \WP_Error => new \WP_Error('http_request_failed', $error));
        $transport       = $this->transport();
        $transport->host = $longHost;
        $enc             = $this->encrypt($transport, 3);
        $first           = $this->hashOfUrl($transport->urlsInFirstPresign()[0]);

        $message = $this->uploadExpectingFailure($transport, $enc);

        self::assertLessThanOrEqual(240, strlen($message));
        self::assertSame(1, preg_match('//u', $message), 'the failure message must be valid UTF-8');
        self::assertNotFalse(json_encode(['message' => $message]), 'the failure report must JSON-encode');
        self::assertStringContainsString($longHost, $message);
        self::assertStringContainsString('Сбой соединения', $message);
        self::assertStringContainsString(substr($first, 0, 16), $message);
    }

    // ------------------------------------------------------------------
    // BackupTransport::putChunkWithStatus() on its own.
    // ------------------------------------------------------------------

    /**
     * capUtf8() keeps failure text valid UTF-8: a cut inside a multi-byte
     * character drops that character, and text that is not UTF-8 keeps only
     * its ASCII.
     */
    public function test_cap_utf8_never_returns_invalid_utf8(): void
    {
        $text = str_repeat('é', 100); // 200 bytes, 2 per character.
        for ($max = 0; $max <= 12; $max++) {
            $cut = BackupTransport::capUtf8($text, $max);
            self::assertSame(1, preg_match('//u', $cut), "cut at {$max} bytes");
            self::assertSame(str_repeat('é', intdiv($max, 2)), $cut);
        }
        $fourByte = str_repeat("\u{1F600}", 5);
        for ($max = 0; $max <= 9; $max++) {
            self::assertSame(str_repeat("\u{1F600}", intdiv($max, 4)), BackupTransport::capUtf8($fourByte, $max));
        }
        self::assertSame('caf timeout', BackupTransport::capUtf8("caf\xE9 timeout", 100), 'Latin-1 input keeps its ASCII');
        self::assertSame('short', BackupTransport::capUtf8('short', 100));

        $transport = $this->bareTransport();
        $this->respondWith(static fn (): \WP_Error => new \WP_Error('http_request_failed', str_repeat('Ошибка ', 40)));
        $error = $transport->putChunkWithStatus(self::PUT_PREFIX . 'abc', 'bytes')['error'];
        self::assertLessThanOrEqual(160, strlen($error));
        self::assertSame(1, preg_match('//u', $error));
    }

    /**
     * Retry classification matches getChunkWithStatus(): transport errors,
     * 408, 425, 429 and 5xx are retryable; other 4xx are not. Only a 403 with
     * AccessDenied, ExpiredToken or an expired-request message is marked for
     * a re-presign.
     */
    public function test_put_result_classification(): void
    {
        $cases = [
            // status, body, retryable, represign
            [408, '', true, false],
            [425, '', true, false],
            [429, '<Error><Code>SlowDown</Code></Error>', true, false],
            [500, '<Error><Code>InternalError</Code></Error>', true, false],
            [503, '', true, false],
            [400, '<Error><Code>BadDigest</Code></Error>', false, false],
            [403, self::SIGNATURE_DOES_NOT_MATCH, false, false],
            [403, '<Error><Code>AccessDenied</Code></Error>', false, true],
            [403, '<Error><Code>ExpiredToken</Code></Error>', false, true],
            [403, '<Error><Code>RequestExpired</Code><Message>Request has expired</Message></Error>', false, true],
            [404, '<Error><Code>AccessDenied</Code></Error>', false, false],
        ];
        $transport = $this->bareTransport();
        foreach ($cases as [$status, $body, $retryable, $represign]) {
            $this->respondWith(fn (): array => $this->response($status, $body));
            $result = $transport->putChunkWithStatus(self::PUT_PREFIX . 'abc?X-Amz-Signature=s', 'bytes');
            self::assertFalse($result['ok'], "HTTP {$status}");
            self::assertSame($status, $result['status']);
            self::assertSame($retryable, $result['retryable'], "retryable for HTTP {$status}");
            self::assertSame($represign, $result['represign'], "represign for HTTP {$status} {$body}");
            self::assertSame('s3.example', $result['host']);
            self::assertSame('', $result['error']);
        }

        $this->respondWith(static fn (): \WP_Error => new \WP_Error('http_request_failed', 'cURL error 7: Failed to connect'));
        $result = $transport->putChunkWithStatus(self::PUT_PREFIX . 'abc', 'bytes');
        self::assertFalse($result['ok']);
        self::assertSame(0, $result['status']);
        self::assertTrue($result['retryable'], 'a transport error is retryable');
        self::assertSame('cURL error 7: Failed to connect', $result['error']);

        $this->respondWith(fn (): array => $this->response(200));
        $result = $transport->putChunkWithStatus(self::PUT_PREFIX . 'abc', 'bytes');
        self::assertTrue($result['ok']);
        self::assertFalse($result['retryable']);
        self::assertTrue($transport->putChunk(self::PUT_PREFIX . 'abc', 'bytes'), 'putChunk() is the bool form of the same result');
    }

    /**
     * The S3 <Code> reaches a failure email, so only a single alphanumeric
     * token of at most 64 characters is kept.
     */
    public function test_s3_code_is_kept_only_when_it_is_a_short_alphanumeric_token(): void
    {
        $cases = [
            '<Error><Code>SignatureDoesNotMatch</Code></Error>'            => 'SignatureDoesNotMatch',
            '<Error><Code>XAmzContentSHA256Mismatch</Code></Error>'        => 'XAmzContentSHA256Mismatch',
            "<Error>\n  <Code> AccessDenied </Code>\n</Error>"              => 'AccessDenied',
            '<Error><Code>Evil<script>alert(1)</script></Code></Error>'    => '',
            '<Error><Code>Has Space</Code></Error>'                        => '',
            '<Error><Code>' . str_repeat('A', 65) . '</Code></Error>'      => '',
            '<Error><Code>' . str_repeat('A', 64) . '</Code></Error>'      => str_repeat('A', 64),
            '<html><body>502 Bad Gateway</body></html>'                     => '',
            ''                                                             => '',
        ];
        $transport = $this->bareTransport();
        foreach ($cases as $body => $expected) {
            $this->respondWith(fn (): array => $this->response(403, (string) $body));
            $result = $transport->putChunkWithStatus(self::PUT_PREFIX . 'abc', 'bytes');
            self::assertSame($expected, $result['s3_code'], 'body: ' . $body);
        }
    }

    /**
     * The transport error never carries the presigned URL, any other URL's
     * path or query, or a bare signature parameter; control characters are
     * flattened and the excerpt is capped.
     */
    public function test_transport_error_is_redacted_and_capped(): void
    {
        $url       = self::PUT_PREFIX . 'abc?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=deadbeef';
        $transport = $this->bareTransport();

        $this->respondWith(static fn (): \WP_Error => new \WP_Error(
            'http_request_failed',
            "Redirected from {$url} to https://other.example/a/b?Signature=xyz&Expires=1, then X-Amz-Credential=AKIAEXAMPLE\r\nfailed"
        ));
        $error = $transport->putChunkWithStatus($url, 'bytes')['error'];

        self::assertStringContainsString('s3.example', $error);
        self::assertStringContainsString('other.example', $error);
        foreach (['/put/', '/a/b', 'deadbeef', 'AKIAEXAMPLE', 'xyz', 'X-Amz-Algorithm=AWS4', "\r", "\n"] as $secret) {
            self::assertStringNotContainsString($secret, $error);
        }

        $this->respondWith(static fn (): \WP_Error => new \WP_Error('http_request_failed', str_repeat('e', 500)));
        self::assertLessThanOrEqual(160, strlen($transport->putChunkWithStatus($url, 'bytes')['error']));
    }

    /**
     * The host reported for a failure is reduced to hostname characters.
     */
    public function test_reported_host_is_reduced_to_hostname_characters(): void
    {
        $transport = $this->bareTransport();
        $this->respondWith(fn (): array => $this->response(500));

        self::assertSame('s3.example', $transport->putChunkWithStatus('https://s3.example/put/abc?x=1', 'b')['host']);
        self::assertSame('[::1]', $transport->putChunkWithStatus('https://[::1]:9000/put/abc', 'b')['host']);
        self::assertSame('bucket.s3.example', $transport->putChunkWithStatus('https://bucket.s3.example:9000/put/abc', 'b')['host']);
    }

    /**
     * Watchdog budget. A PUT blocks without emitting progress, so the longest
     * silent window is (heartbeat interval, presign callback timeout or
     * backoff, whichever is largest) + one PUT timeout + TaskRunner's
     * progress-write throttle. It must stay below the backup watchdog's stall
     * threshold, or the watchdog re-enters a run that is alive and retrying.
     * The PUT timeout must also stay above the callback timeout it replaced.
     */
    public function test_retry_windows_fit_inside_the_watchdog_stall_threshold(): void
    {
        $putTimeout      = $this->transportConstant('PUT_TIMEOUT');
        $callbackTimeout = $this->transportConstant('TIMEOUT');
        $maxBackoffSec   = (int) ceil(
            $this->uploaderConstant('PUT_BACKOFF_BASE_MS') * (1 << ($this->uploaderConstant('PUT_MAX_ATTEMPTS') - 2)) / 1000
        );
        $throttle = (int) (new ReflectionClassConstant(TaskRunner::class, 'PROGRESS_DB_THROTTLE_SECONDS'))->getValue();
        $preGap   = max((int) ceil(EncryptAndUpload::DEFAULT_HEARTBEAT_INTERVAL_SECONDS), $callbackTimeout, $maxBackoffSec);

        self::assertGreaterThan($callbackTimeout, $putTimeout, 'a chunk PUT gets longer than a CP callback');
        self::assertLessThan(
            Watchdog::STALL_THRESHOLD_SECONDS,
            $preGap + $putTimeout + $throttle,
            'the longest silent window during an upload must stay below the watchdog stall threshold'
        );
    }

    // ------------------------------------------------------------------
    // Harness.
    // ------------------------------------------------------------------

    /**
     * A real BackupTransport whose presign callback is faked (no signer, no
     * CP). Its PUT path is the production one, reaching the faked
     * wp_remote_request(). Presigned URLs carry a query string so a leak of
     * it is detectable.
     */
    private function transport(): BackupTransport
    {
        return new class() extends BackupTransport {
            /** @var list<list<string>> hashes asked for, per presign call */
            public array $presignCalls = [];

            /** @var list<string> hashes the CP reports as already stored on a re-presign */
            public array $storedOnRepresign = [];

            /** Storage host the presigned URLs point at. */
            public string $host = 's3.example';

            /** @var list<string> URLs returned by the first presign, in order */
            private array $firstUrls = [];

            public function __construct()
            {
            }

            public function presignChunks(string $endpoint, string $snapshotId, array $hashes): array
            {
                $this->presignCalls[] = array_values($hashes);
                $round                = count($this->presignCalls);
                $uploads              = [];
                foreach ($hashes as $hash) {
                    if ($round > 1 && in_array($hash, $this->storedOnRepresign, true)) {
                        continue;
                    }
                    $uploads[$hash] = 'https://' . $this->host . '/put/' . $hash . '?X-Amz-Signature=sig-round-' . $round;
                }
                if ($round === 1) {
                    $this->firstUrls = array_values($uploads);
                }
                return $uploads;
            }

            /** @return list<string> */
            public function urlsInFirstPresign(): array
            {
                return $this->firstUrls;
            }
        };
    }

    /** A BackupTransport with no signer, for direct putChunkWithStatus() calls. */
    private function bareTransport(): BackupTransport
    {
        return (new ReflectionClass(BackupTransport::class))->newInstanceWithoutConstructor();
    }

    /**
     * Fake the network: every wp_remote_request() call is logged and answered
     * by $respond(string $url, int $callNumber).
     */
    private function respondWith(callable $respond): void
    {
        $this->requests = [];
        Functions\when('wp_remote_request')->alias(function (string $url, array $args = []) use ($respond) {
            $this->requests[] = ['url' => $url, 'args' => $args];
            $this->events[]   = 'put:' . $this->hashOfUrl($url);
            return $respond($url, count($this->requests));
        });
    }

    /**
     * @return array<string,mixed> A wp_remote_request()-shaped response.
     */
    private function response(int $status, string $body = ''): array
    {
        return [
            'headers'  => [],
            'body'     => $body,
            'response' => ['code' => $status, 'message' => ''],
            'cookies'  => [],
        ];
    }

    /**
     * Run the encrypt pass over an artifact of $chunks distinct blocks, then
     * run one presign so the test can name the PUT order. The presign log is
     * reset afterwards so the upload pass's own presign calls are counted.
     *
     * @return array<string,mixed> The encrypt cursor.
     */
    private function encrypt(BackupTransport $transport, int $chunks): array
    {
        $bytes = '';
        for ($i = 0; $i < $chunks; $i++) {
            $bytes .= str_pad((string) $i, self::CHUNK_BYTES, '0', STR_PAD_LEFT);
        }
        $path = $this->scratchDir . DIRECTORY_SEPARATOR . 'blob.bin';
        file_put_contents($path, $bytes);

        $enc = $this->pipeline($transport)->encryptChunks(
            $this->scratchDir,
            [['path' => $path, 'logical' => 'blob.bin']],
            [],
            static function (string $phase, array $detail): void {
            }
        );
        self::assertTrue($enc['done'] ?? false);
        self::assertGreaterThanOrEqual($chunks, count($enc['all_hashes']));

        // Learn the PUT order (the upload pass PUTs in presign-response order).
        $transport->presignChunks('', '', $enc['all_hashes']);
        $transport->presignCalls = [];

        return $enc;
    }

    private function pipeline(BackupTransport $transport): EncryptAndUpload
    {
        return new EncryptAndUpload(
            new AgeCrypto(),
            $transport,
            'snap-put-1',
            'age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq',
            'https://cp.example/agent/v1/backups/snap-put-1/presign',
            'https://cp.example/agent/v1/backups/snap-put-1/manifest',
            self::CHUNK_BYTES,
            null,
            EncryptAndUpload::DEFAULT_HEARTBEAT_INTERVAL_SECONDS,
            function (int $ms): void {
                $this->sleeps[] = $ms;
            }
        );
    }

    /**
     * @param array<string,mixed> $enc Encrypt cursor.
     * @return array<string,mixed> The upload cursor.
     */
    private function upload(BackupTransport $transport, array $enc): array
    {
        return $this->pipeline($transport)->uploadChunks(
            $enc,
            ['scratch_dir' => $this->scratchDir],
            function (string $phase, array $detail): void {
                $this->progress[] = $detail;
            },
            function (array $cursor): void {
                $this->checkpoints[] = $cursor;
                $this->events[]      = 'checkpoint';
            }
        );
    }

    /**
     * @param array<string,mixed> $enc Encrypt cursor.
     * @return string The failure message.
     */
    private function uploadExpectingFailure(BackupTransport $transport, array $enc): string
    {
        try {
            $this->upload($transport, $enc);
        } catch (\RuntimeException $e) {
            return $e->getMessage();
        }
        self::fail('uploadChunks() must fail when a chunk cannot be uploaded');
    }

    /** @return list<int> the `put_retrying` attempt numbers on retry heartbeats, in order */
    private function retryHeartbeats(): array
    {
        $out = [];
        foreach ($this->progress as $detail) {
            if (isset($detail['put_retrying'])) {
                self::assertTrue($detail['heartbeat'] ?? false);
                $out[] = (int) $detail['put_retrying'];
            }
        }
        return $out;
    }

    private function hashOfUrl(string $url): string
    {
        $path = (string) parse_url($url, PHP_URL_PATH);
        return substr($path, strlen('/put/'));
    }

    private function chunkPathFor(string $hash): string
    {
        return $this->scratchDir . DIRECTORY_SEPARATOR . 'chunks-' . $hash . '.bin';
    }

    private function transportConstant(string $name): int
    {
        return (int) (new ReflectionClassConstant(BackupTransport::class, $name))->getValue();
    }

    private function uploaderConstant(string $name): int
    {
        return (int) (new ReflectionClassConstant(EncryptAndUpload::class, $name))->getValue();
    }

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
