<?php
/**
 * GH #279: BackupTransport::decodeJsonResponse() must surface the HTTP status
 * and a truncated response-body excerpt on a rejected CP callback, instead of
 * the old generic "control plane callback rejected." message. This is the
 * seam BOTH presignChunks() and submitManifest() route through, so a rejected
 * presign/manifest call now tells the operator (via the task-runner log /
 * exception message) exactly why the CP said no, without a separate
 * CP-side log correlation step.
 *
 * We reach decodeJsonResponse() directly via reflection on a
 * newInstanceWithoutConstructor() instance: it never touches $this->signer,
 * so this keeps the test hermetic (no Keystore/Signer/DB seam needed) per the
 * "self-contained fakes" test convention.
 *
 * @package WPMgr\Agent\Tests\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Support;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\BackupTransport
 */
final class BackupTransportErrorDetailTest extends TestCase
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
     * Build a BackupTransport without running its constructor (decode is
     * signer-independent) and return the reflected private decodeJsonResponse
     * method, ready to invoke.
     */
    private function decodeMethod(): array
    {
        $reflection = new ReflectionClass(BackupTransport::class);
        $transport  = $reflection->newInstanceWithoutConstructor();
        $method     = $reflection->getMethod('decodeJsonResponse');

        return [$transport, $method];
    }

    /**
     * A rejected 422 (e.g. "snapshot_not_in_progress") must surface both the
     * HTTP status and the CP's JSON error body in the thrown message. This is
     * the exact shape a stalled-then-hard-failed presign call would hit
     * before the fix (was: "control plane callback rejected.", no detail).
     */
    public function test_non_2xx_status_includes_http_status_in_message(): void
    {
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(422);
        Functions\when('wp_remote_retrieve_body')->justReturn(
            '{"error":{"code":"snapshot_not_in_progress","message":"snapshot is not in progress"}}'
        );

        [$transport, $method] = $this->decodeMethod();

        try {
            $method->invoke($transport, ['response' => ['code' => 422]]);
            self::fail('expected RuntimeException on a non-2xx response');
        } catch (\RuntimeException $e) {
            self::assertStringContainsString('HTTP 422', $e->getMessage());
            self::assertStringContainsString('snapshot_not_in_progress', $e->getMessage());
            self::assertStringContainsString('snapshot is not in progress', $e->getMessage());
        }
    }

    /**
     * A 5xx (e.g. a CP-side 500 during a presign call) must ALSO carry the
     * status + body excerpt; the fix is not limited to 4xx.
     */
    public function test_5xx_status_includes_http_status_in_message(): void
    {
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(500);
        Functions\when('wp_remote_retrieve_body')->justReturn(
            '{"error":{"code":"internal","message":"unexpected server error"}}'
        );

        [$transport, $method] = $this->decodeMethod();

        try {
            $method->invoke($transport, ['response' => ['code' => 500]]);
            self::fail('expected RuntimeException on a 5xx response');
        } catch (\RuntimeException $e) {
            self::assertStringContainsString('HTTP 500', $e->getMessage());
            self::assertStringContainsString('unexpected server error', $e->getMessage());
        }
    }

    /**
     * The body excerpt is truncated to 200 characters (mirrors
     * getChunkWithStatus()'s body_excerpt pattern) so a large/garbled error
     * body can never blow out the exception message or a downstream log line.
     */
    public function test_body_excerpt_is_truncated_to_200_chars(): void
    {
        $longBody = str_repeat('x', 500);

        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(409);
        Functions\when('wp_remote_retrieve_body')->justReturn($longBody);

        [$transport, $method] = $this->decodeMethod();

        try {
            $method->invoke($transport, ['response' => ['code' => 409]]);
            self::fail('expected RuntimeException on a non-2xx response');
        } catch (\RuntimeException $e) {
            self::assertStringContainsString('HTTP 409', $e->getMessage());
            // 200 'x' characters, not the full 500.
            self::assertStringContainsString(str_repeat('x', 200), $e->getMessage());
            self::assertStringNotContainsString(str_repeat('x', 201), $e->getMessage());
        }
    }

    /**
     * A WP_Error (network/DNS/timeout; never reached an HTTP status) keeps
     * its distinct "control plane unreachable" message; it must NOT be
     * conflated with the new HTTP-status-detail path (there is no status to
     * report).
     */
    public function test_wp_error_keeps_unreachable_message_without_http_status(): void
    {
        Functions\when('is_wp_error')->justReturn(true);

        [$transport, $method] = $this->decodeMethod();

        try {
            $method->invoke($transport, new \WP_Error('http_request_failed', 'timeout'));
            self::fail('expected RuntimeException on a WP_Error response');
        } catch (\RuntimeException $e) {
            self::assertStringContainsString('control plane unreachable', $e->getMessage());
            self::assertStringNotContainsString('HTTP', $e->getMessage());
        }
    }

    /**
     * A 2xx response with a well-formed JSON body still decodes successfully
     * (regression guard: the new $raw-before-status-check reordering must not
     * break the success path).
     */
    public function test_2xx_response_still_decodes_successfully(): void
    {
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('{"ok":true,"chunk_count":3}');

        [$transport, $method] = $this->decodeMethod();

        $data = $method->invoke($transport, ['response' => ['code' => 200]]);

        self::assertTrue($data['ok']);
        self::assertSame(3, $data['chunk_count']);
    }
}
