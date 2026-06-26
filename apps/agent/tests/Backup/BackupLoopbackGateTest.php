<?php
/**
 * Tests for the loopback-gate detection introduced in GitHub issue #117.
 *
 * On sites behind a membership or privacy gate (e.g. SureDash "private
 * community"), every front-end request — including /wp-cron.php — is
 * redirected to a login page for unauthenticated visitors. This means
 * spawn_cron()'s non-blocking POST to /wp-cron.php is silently swallowed by
 * the redirect, so the wpmgr_backup_run cron event never executes and the
 * backup stalls with no progress.
 *
 * The fix:
 *   1. BackupCommand::isLoopbackGated() probes the /wp-cron.php URL before
 *      calling spawn_cron(). When the probe returns a 3xx redirect (or a
 *      WP_Error — unreachable loopback), the backup runner is started in the
 *      same PHP process via register_shutdown_function after the ACK response
 *      is sent.
 *   2. Watchdog::detectQueuedStallReason() (called when max_resumes is
 *      exhausted AND phase is still 'queued') probes the loopback and surfaces
 *      a clear, actionable failure message instead of the generic stall text.
 *
 * Test seam: both isLoopbackGated() and detectQueuedStallReason() are
 * `protected` / private-static. We invoke them via ReflectionMethod so we do
 * not need to subclass the `final` BackupCommand or Watchdog classes.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionMethod;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\BackupCommand
 * @covers \WPMgr\Agent\Backup\Watchdog
 */
final class BackupLoopbackGateTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    // ------------------------------------------------------------------
    // Helpers
    // ------------------------------------------------------------------

    /** Stub AgeIdentity with a controllable recipient. */
    private function stubIdentity(
        string $recipient = 'age1test0000000000000000000000000000000000000000000000000000'
    ): AgeIdentity {
        return new class($recipient) extends AgeIdentity {
            private string $rcpt;

            public function __construct(string $rcpt)
            {
                $this->rcpt = $rcpt;
            }

            public function recipient(): string
            {
                return $this->rcpt;
            }

            public function recipientMatches(string $candidate): bool
            {
                return hash_equals($this->rcpt, $candidate);
            }
        };
    }

    /** Return a ReflectionMethod for BackupCommand::isLoopbackGated(). */
    private function reflectIsLoopbackGated(BackupCommand $cmd): ReflectionMethod
    {
        $method = new ReflectionMethod(BackupCommand::class, 'isLoopbackGated');
        // setAccessible is a no-op since PHP 8.1 and deprecated since 8.5.
        return $method;
    }

    // ------------------------------------------------------------------
    // isLoopbackGated(): unit tests for the probe logic
    // ------------------------------------------------------------------

    /**
     * A 302 redirect response from the loopback probe means the site is gated.
     */
    public function test_isLoopbackGated_returns_true_on_redirect_response(): void
    {
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            $this->markTestSkipped('DISABLE_WP_CRON is true — probe is skipped in this process.');
        }

        Functions\when('site_url')->justReturn('https://private.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 302, 'message' => 'Found'],
            'headers'  => ['location' => 'https://private.example.com/portal-login/'],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(302);
        Functions\when('wp_remote_retrieve_header')->justReturn('https://private.example.com/portal-login/');

        $cmd    = new BackupCommand($this->stubIdentity());
        $method = $this->reflectIsLoopbackGated($cmd);

        $this->assertTrue($method->invoke($cmd));
    }

    /**
     * A 301 redirect is also treated as a gate.
     */
    public function test_isLoopbackGated_returns_true_on_301_redirect(): void
    {
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            $this->markTestSkipped('DISABLE_WP_CRON is true — probe is skipped in this process.');
        }

        Functions\when('site_url')->justReturn('https://private.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 301, 'message' => 'Moved Permanently'],
            'headers'  => ['location' => 'https://private.example.com/login/'],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(301);
        Functions\when('wp_remote_retrieve_header')->justReturn('https://private.example.com/login/');

        $cmd    = new BackupCommand($this->stubIdentity());
        $method = $this->reflectIsLoopbackGated($cmd);

        $this->assertTrue($method->invoke($cmd));
    }

    /**
     * A WP_Error (loopback unreachable) is also treated as a gate
     * (safer to run in-process than to enqueue into a black hole).
     */
    public function test_isLoopbackGated_returns_true_on_wp_error(): void
    {
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            $this->markTestSkipped('DISABLE_WP_CRON is true — probe is skipped in this process.');
        }

        Functions\when('site_url')->justReturn('https://private.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn(
            new \WP_Error('http_request_failed', 'cURL error 7: Connection refused')
        );
        Functions\when('is_wp_error')->justReturn(true);

        $cmd    = new BackupCommand($this->stubIdentity());
        $method = $this->reflectIsLoopbackGated($cmd);

        $this->assertTrue($method->invoke($cmd));
    }

    /**
     * A 200 response (WP-Cron ran fine) is NOT gated.
     */
    public function test_isLoopbackGated_returns_false_on_200_response(): void
    {
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            $this->markTestSkipped('DISABLE_WP_CRON is true — probe is skipped in this process.');
        }

        Functions\when('site_url')->justReturn('https://open.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200, 'message' => 'OK'],
            'headers'  => [],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);

        $cmd    = new BackupCommand($this->stubIdentity());
        $method = $this->reflectIsLoopbackGated($cmd);

        $this->assertFalse($method->invoke($cmd));
    }

    /**
     * A 403 response (security plugin returns 403) is NOT treated as a gate —
     * spawn_cron will still work for cron dispatch since the loopback is reachable.
     */
    public function test_isLoopbackGated_returns_false_on_403_response(): void
    {
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            $this->markTestSkipped('DISABLE_WP_CRON is true — probe is skipped in this process.');
        }

        Functions\when('site_url')->justReturn('https://open.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 403, 'message' => 'Forbidden'],
            'headers'  => [],
            'body'     => 'Forbidden',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(403);

        $cmd    = new BackupCommand($this->stubIdentity());
        $method = $this->reflectIsLoopbackGated($cmd);

        $this->assertFalse($method->invoke($cmd));
    }

    // ------------------------------------------------------------------
    // execute() validation guards — these run BEFORE the loopback probe
    // so they must still refuse bad input regardless of gate state.
    // ------------------------------------------------------------------

    /**
     * execute() must still refuse requests with a missing age recipient.
     * The loopback probe happens after input validation, so this guard fires
     * first and no wp_remote_get call should be made.
     */
    public function test_execute_refuses_missing_recipient(): void
    {
        $cmd = new BackupCommand($this->stubIdentity());
        $res = $cmd->execute([], [
            'snapshot_id'       => '11111111-2222-3333-4444-555555555555',
            'kind'              => 'files',
            'age_recipient'     => '',
            'presign_endpoint'  => 'https://cp.example/presign',
            'manifest_endpoint' => 'https://cp.example/manifest',
        ]);
        $this->assertFalse($res['ok']);
        $this->assertSame('missing age recipient', $res['detail']);
    }

    /**
     * execute() must still refuse an age recipient mismatch.
     */
    public function test_execute_refuses_recipient_mismatch(): void
    {
        $knownRecipient = 'age1test0000000000000000000000000000000000000000000000000000';
        $cmd = new BackupCommand($this->stubIdentity($knownRecipient));
        $res = $cmd->execute([], [
            'snapshot_id'       => '11111111-2222-3333-4444-555555555555',
            'kind'              => 'files',
            'age_recipient'     => 'age1differentrecipientthatshouldnotmatch0000000000000000000',
            'presign_endpoint'  => 'https://cp.example/presign',
            'manifest_endpoint' => 'https://cp.example/manifest',
        ]);
        $this->assertFalse($res['ok']);
        $this->assertSame('age recipient mismatch', $res['detail']);
    }

    // ------------------------------------------------------------------
    // Watchdog stall-reason detection
    // ------------------------------------------------------------------

    /**
     * Watchdog::detectQueuedStallReason() must return the generic stall
     * message when the phase is NOT queued (stall in a later phase has a
     * different root cause and the loopback probe is irrelevant).
     */
    public function test_detectQueuedStallReason_generic_for_non_queued_phase(): void
    {
        $method = new ReflectionMethod(Watchdog::class, 'detectQueuedStallReason');

        $result = $method->invoke(null, 'archiving_files', 'test-snap-id');

        $this->assertIsString($result);
        $this->assertStringContainsString('stalled in queued phase', $result);
        $this->assertStringNotContainsString('login', $result);
        $this->assertStringNotContainsString('redirect', $result);
    }

    /**
     * When the loopback probe returns a redirect and the phase IS queued,
     * detectQueuedStallReason() must return a message mentioning "redirect"
     * and "membership or privacy gate" for operator clarity.
     */
    public function test_detectQueuedStallReason_gate_message_on_redirect(): void
    {
        Functions\when('site_url')->justReturn('https://private.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 302, 'message' => 'Found'],
            'headers'  => ['location' => 'https://private.example.com/portal-login/'],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(302);
        Functions\when('wp_remote_retrieve_header')->justReturn('https://private.example.com/portal-login/');
        Functions\when('esc_url_raw')->alias(static fn ($u) => $u);

        $method = new ReflectionMethod(Watchdog::class, 'detectQueuedStallReason');
        $result = $method->invoke(null, 'queued', 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee');

        $this->assertIsString($result);
        $this->assertStringContainsString('redirect', $result);
        $this->assertStringContainsString('membership or privacy gate', $result);
        $this->assertStringContainsString('/wp-cron.php', $result);
    }

    /**
     * When the loopback is unreachable (WP_Error), detectQueuedStallReason()
     * must mention "could not be reached" with the error detail.
     */
    public function test_detectQueuedStallReason_unreachable_message_on_wp_error(): void
    {
        Functions\when('site_url')->justReturn('https://private.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn(
            new \WP_Error('http_request_failed', 'Connection refused')
        );
        Functions\when('is_wp_error')->justReturn(true);
        Functions\when('esc_url_raw')->alias(static fn ($u) => $u);

        $method = new ReflectionMethod(Watchdog::class, 'detectQueuedStallReason');
        $result = $method->invoke(null, 'queued', 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee');

        $this->assertIsString($result);
        $this->assertStringContainsString('could not be reached', $result);
        $this->assertStringContainsString('Connection refused', $result);
    }

    /**
     * When the loopback returns 200, detectQueuedStallReason() falls back to
     * the generic stall message (the gate is not the cause of the stall).
     */
    public function test_detectQueuedStallReason_generic_when_loopback_accessible(): void
    {
        Functions\when('site_url')->justReturn('https://open.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200, 'message' => 'OK'],
            'headers'  => [],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('esc_url_raw')->alias(static fn ($u) => $u);

        $method = new ReflectionMethod(Watchdog::class, 'detectQueuedStallReason');
        $result = $method->invoke(null, 'queued', 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee');

        $this->assertIsString($result);
        $this->assertStringContainsString('backup runner was never dispatched', $result);
        $this->assertStringNotContainsString('redirect', $result);
        $this->assertStringNotContainsString('login', $result);
    }

    /**
     * When DISABLE_WP_CRON is true, the probe is skipped and returns false.
     *
     * NOTE: This test uses define() which is process-wide. It MUST run last
     * in this class to avoid poisoning the probe-dependent tests above.
     * The identical pattern is used in PingCommandTest (see line ~107 there).
     */
    public function test_isLoopbackGated_returns_false_when_disable_wp_cron(): void
    {
        if (defined('DISABLE_WP_CRON')) {
            $this->markTestSkipped('DISABLE_WP_CRON already defined in this process.');
        }
        define('DISABLE_WP_CRON', true);

        $cmd    = new BackupCommand($this->stubIdentity());
        $method = $this->reflectIsLoopbackGated($cmd);

        $this->assertFalse($method->invoke($cmd));
    }
}
