<?php
/**
 * RestoreHealthCheckTest — unit coverage of GH #146's post-restore health
 * check: `RestoreHealthCheck::checkDatabase()` (Probe A) in isolation, and
 * the combined `run()` verdict (`ok = ProbeA.ok && (ProbeB.ok ||
 * ProbeB.inconclusive)`) exercised through the WP HTTP API stubs.
 *
 * Includes the post-adversarial-review hardening:
 *   - CRITICAL 1: a maintenance-mode 503 (the runner's OWN `.maintenance`
 *     drop-file, present for the ENTIRE health_check window by design) must
 *     downgrade to inconclusive, never hard-fail a DB-healthy restore.
 *   - CRITICAL 2: an all-connect-refused Probe B is ALWAYS inconclusive —
 *     never a hard failure — regardless of the wp-cron.php sentinel's
 *     result or of `DISABLE_WP_CRON`'s state. These tests no longer need to
 *     skip when `DISABLE_WP_CRON` leaks true from an earlier test in the
 *     same process (see class doc on the old skip guard, removed).
 *   - Risk-2 strengthening: a blank/marker-less 2xx body downgrades to
 *     inconclusive rather than a confident pass.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\RestoreHealthCheck;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreHealthCheck
 */
final class RestoreHealthCheckTest extends TestCase
{
    private FakeRestoreRunnerWpdb $wpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->wpdb = new FakeRestoreRunnerWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;

        Functions\when('home_url')->alias(static fn (string $path = '/') => 'https://example.test' . $path);
        Functions\when('admin_url')->alias(static fn (string $path = '') => 'https://example.test/wp-admin/' . $path);
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // ==================================================================
    // Probe A — in-process DB check.
    // ==================================================================

    public function test_probe_a_fails_when_wpdb_is_not_available(): void
    {
        unset($GLOBALS['wpdb']);
        $result = (new RestoreHealthCheck())->checkDatabase();
        $this->assertFalse($result['ok']);
        $this->assertNotEmpty($result['failures']);
    }

    public function test_probe_a_fails_when_siteurl_is_empty_after_swap(): void
    {
        $this->wpdb->siteurlValue = '';
        $result = (new RestoreHealthCheck())->checkDatabase();
        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('siteurl', implode(' ', $result['failures']));
    }

    /**
     * The GH #146 headline scenario: a table the swap should have produced
     * no longer answers (missing / DB error) — must be a HARD failure.
     */
    public function test_probe_a_fails_on_missing_swapped_users_table(): void
    {
        $this->wpdb->usersErrors = true;
        $result = (new RestoreHealthCheck())->checkDatabase();
        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('users', implode(' ', $result['failures']));
    }

    public function test_probe_a_fails_on_missing_swapped_posts_table(): void
    {
        $this->wpdb->postsErrors = true;
        $result = (new RestoreHealthCheck())->checkDatabase();
        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('posts', implode(' ', $result['failures']));
    }

    public function test_probe_a_passes_when_all_reads_succeed(): void
    {
        $result = (new RestoreHealthCheck())->checkDatabase();
        $this->assertTrue($result['ok']);
        $this->assertSame([], $result['failures']);
    }

    // ==================================================================
    // Combined verdict — Probe B via the WP HTTP API.
    // ==================================================================

    public function test_5xx_on_home_url_is_a_hard_failure(): void
    {
        Functions\when('wp_remote_get')->alias(static function (string $url, array $args = []) {
            if (str_contains($url, 'wp-cron.php')) {
                return ['response' => ['code' => 200]];
            }
            return ['response' => ['code' => 500], 'body' => 'Internal Server Error'];
        });
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        // No .maintenance present (default ABSPATH from the test bootstrap
        // has no such file) — a real, non-maintenance 5xx must still be a
        // hard failure.
        $result = (new RestoreHealthCheck())->run();

        $this->assertFalse($result['ok']);
        $this->assertNotEmpty($result['failures']);
    }

    public function test_fatal_error_body_signature_is_a_hard_failure_even_on_http_200(): void
    {
        // A WSOD often still returns HTTP 200 under display_errors — the
        // status code alone must not be trusted.
        Functions\when('wp_remote_get')->alias(static function (string $url, array $args = []) {
            if (str_contains($url, 'wp-cron.php')) {
                return ['response' => ['code' => 200]];
            }
            return ['response' => ['code' => 200], 'body' => '<b>Fatal error</b>: Uncaught Error in wp-content/...'];
        });
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertFalse($result['ok']);
        $this->assertNotEmpty($result['failures']);
    }

    public function test_2xx_passes(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 200], 'body' => '<html>ok</html>']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok']);
        $this->assertSame([], $result['failures']);
    }

    public function test_401_login_walled_passes(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 401], 'body' => 'Unauthorized']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok']);
    }

    public function test_403_forbidden_passes(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 403], 'body' => 'Forbidden']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok']);
    }

    /**
     * Risk-2 strengthening: a swallowed fatal under `display_errors=Off`
     * can leave a blank (or marker-less) 200 behind — that must NOT read as
     * a confident pass. Downgrades to inconclusive (a warning), not a
     * rollback trigger, and not silently treated as healthy either.
     */
    public function test_blank_200_body_is_inconclusive_not_a_confident_pass(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 200], 'body' => '']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok'], 'ambiguous must never itself roll back a DB-healthy restore');
        $this->assertSame([], $result['failures']);
        $this->assertNotEmpty($result['warnings'], 'a blank 200 must surface as a warning, not be silently treated as healthy');
    }

    public function test_200_body_without_html_marker_is_inconclusive(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 200], 'body' => 'ok']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok']);
        $this->assertNotEmpty($result['warnings']);
    }

    /**
     * Every probe target connect-fails AND the wp-cron.php sentinel ALSO
     * fails (a redirect, mirroring BackupCommand::isLoopbackGated()'s own
     * gate signal) — the host structurally blocks loopback HTTP. This MUST
     * be inconclusive (a warning), never a rollback trigger. No longer
     * needs a DISABLE_WP_CRON skip guard: the sentinel's result only
     * enriches the diagnostic detail now, it never changes the verdict.
     */
    public function test_structurally_gated_loopback_is_inconclusive_not_a_failure(): void
    {
        Functions\when('wp_remote_get')->alias(static function (string $url, array $args = []) {
            if (str_contains($url, 'wp-cron.php')) {
                // Redirect -> BackupCommand::isLoopbackGated()'s own gate signal.
                return ['response' => ['code' => 302], 'headers' => ['location' => 'https://example.test/login']];
            }
            return new \WP_Error('http_request_failed', 'Could not resolve host');
        });

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok'], 'a structurally-gated loopback must never fail the verdict');
        $this->assertNotEmpty($result['warnings'], 'the gate must surface as a warning, not silently pass');
        $this->assertSame([], $result['failures']);
    }

    /**
     * GH #146 review CRITICAL 2: every probe target connect-fails, and the
     * wp-cron.php sentinel SUCCEEDS. This is NO LONGER a hard failure — a
     * destructive rollback must fire only on a POSITIVE fatal signal (an
     * actual 5xx or a fatal body signature), never on mere unreachability.
     * `DISABLE_WP_CRON` (extremely common on managed hosts) makes the
     * sentinel short-circuit to "not gated" WITHOUT even probing, which
     * previously turned this exact scenario into a destructive rollback on
     * essentially any such host — the fix makes the verdict identical
     * whether the sentinel is gated or not.
     */
    public function test_all_targets_refused_while_sentinel_succeeds_is_inconclusive_not_a_hard_failure(): void
    {
        Functions\when('wp_remote_get')->alias(static function (string $url, array $args = []) {
            if (str_contains($url, 'wp-cron.php')) {
                return ['response' => ['code' => 200]];
            }
            return new \WP_Error('http_request_failed', 'Connection refused');
        });

        $result = (new RestoreHealthCheck())->run();

        $this->assertTrue($result['ok'], 'connect-refused must never itself roll back a DB-healthy restore, sentinel result notwithstanding');
        $this->assertSame([], $result['failures']);
        $this->assertNotEmpty($result['warnings']);
    }

    public function test_verdict_fails_when_probe_a_fails_even_if_probe_b_passes(): void
    {
        $this->wpdb->siteurlValue = '';
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 200], 'body' => '<html>ok</html>']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck())->run();

        $this->assertFalse($result['ok'], 'a broken DB must fail the verdict regardless of a booting front end');
    }

    // ==================================================================
    // CRITICAL 1: maintenance-mode-aware Probe B.
    // ==================================================================

    /**
     * The headline CRITICAL-1 regression: `health_check` runs BEFORE
     * `maintenance_off` by design (so a rollback happens while the site is
     * still in maintenance mode) — which means the runner's OWN
     * `.maintenance` drop-file is present for the entire health-check
     * window on EVERY ordinary restore, and WordPress answers every
     * loopback probe with its own 503 maintenance page for as long as that
     * file exists. That 503 must NOT hard-fail a DB-healthy restore.
     */
    public function test_maintenance_mode_503_does_not_hard_fail_a_db_healthy_restore(): void
    {
        $wpRoot = $this->makeMaintenanceWpRoot();

        Functions\when('wp_remote_get')->alias(static function (string $url, array $args = []) {
            if (str_contains($url, 'wp-cron.php')) {
                return ['response' => ['code' => 503], 'body' => 'Briefly unavailable for scheduled maintenance.'];
            }
            return [
                'response' => ['code' => 503],
                'body'     => '<!DOCTYPE html><html><body>Briefly unavailable for scheduled maintenance. '
                    . 'Check back in a minute.</body></html>',
            ];
        });
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck($wpRoot))->run();

        $this->assertTrue($result['ok'], 'a maintenance-mode 503 must never hard-fail a DB-healthy restore');
        $this->assertSame([], $result['failures']);
        $this->assertNotEmpty($result['warnings'], 'the maintenance override should still surface as a warning for operator visibility');

        $this->rrmdir($wpRoot);
    }

    /**
     * Same 503 body, but WITHOUT `.maintenance` present — proves the
     * override is conditional (a genuine, non-maintenance 5xx is still a
     * hard failure), not a blanket "5xx never fails" downgrade.
     */
    public function test_same_503_without_maintenance_file_is_still_a_hard_failure(): void
    {
        $wpRoot = sys_get_temp_dir() . '/wpmgr-hc-nomaint-' . bin2hex(random_bytes(6));
        mkdir($wpRoot, 0755, true);

        Functions\when('wp_remote_get')->alias(static function (string $url, array $args = []) {
            if (str_contains($url, 'wp-cron.php')) {
                return ['response' => ['code' => 200]];
            }
            return [
                'response' => ['code' => 503],
                'body'     => '<!DOCTYPE html><html><body>Briefly unavailable for scheduled maintenance.</body></html>',
            ];
        });
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck($wpRoot))->run();

        $this->assertFalse($result['ok'], 'without .maintenance present, a 503 must still be treated as a genuine fatal');
        $this->assertNotEmpty($result['failures']);

        $this->rrmdir($wpRoot);
    }

    /**
     * Probe A alone must still be able to fail the verdict during
     * maintenance mode — Probe A is unaffected by the maintenance page
     * (it talks to `$wpdb` directly) and remains the destructive gate for
     * this window, per CRITICAL-1's fix.
     */
    public function test_probe_a_still_fails_during_maintenance_mode(): void
    {
        $wpRoot = $this->makeMaintenanceWpRoot();
        $this->wpdb->siteurlValue = '';

        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => [
            'response' => ['code' => 503],
            'body'     => '<!DOCTYPE html><html><body>Briefly unavailable for scheduled maintenance.</body></html>',
        ]);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        $result = (new RestoreHealthCheck($wpRoot))->run();

        $this->assertFalse($result['ok'], 'Probe A must remain the destructive gate during the maintenance window');
        $this->assertStringContainsString('siteurl', implode(' ', $result['failures']));

        $this->rrmdir($wpRoot);
    }

    /**
     * @return string Absolute path of a fresh temp dir with a `.maintenance`
     *                 file, mirroring RestoreRunner::maintenanceOn()'s format.
     */
    private function makeMaintenanceWpRoot(): string
    {
        $wpRoot = sys_get_temp_dir() . '/wpmgr-hc-maint-' . bin2hex(random_bytes(6));
        mkdir($wpRoot, 0755, true);
        file_put_contents($wpRoot . '/.maintenance', "<?php\n\$upgrading = " . time() . ';');
        return $wpRoot;
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
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
