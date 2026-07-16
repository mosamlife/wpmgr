<?php
/**
 * GitHub issue #232 regression: BackupCommand::execute() must register the
 * in-process shutdown-function runner UNCONDITIONALLY on every accepted
 * backup trigger, not only when isLoopbackGated() detects a membership/
 * privacy redirect.
 *
 * Root cause this locks in: before the fix, a non-gated site relied ENTIRELY
 * on WP-Cron (wp_schedule_single_event('wpmgr_backup_run') + spawn_cron()) to
 * ever invoke TaskRunner::run() for the first time. On a low-traffic/off-peak
 * schedule (or DISABLE_WP_CRON, where spawn_cron() is a documented no-op)
 * nothing started the runner, so the freshly-seeded row never left
 * phase=queued and stayed stuck there forever.
 *
 * register_shutdown_function() is intercepted via Brain Monkey's
 * Functions\expect(), which records the call WITHOUT invoking the passed
 * closure — so this test exercises the real acceptance path (preflight,
 * dedup claim, scratch dir, task-row seed, watchdog schedule) without ever
 * constructing a live TaskRunner/Keystore/Signer.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\BackupCommand
 */
final class BackupAlwaysOnRunnerTest extends TestCase
{
    private string $contentDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!class_exists(\ZipArchive::class)) {
            self::markTestSkipped('ext-zip not available');
        }

        if (!defined('WP_CONTENT_DIR')) {
            $this->contentDir = sys_get_temp_dir() . '/wpmgr-alwayson-' . bin2hex(random_bytes(6));
            mkdir($this->contentDir, 0755, true);
            define('WP_CONTENT_DIR', $this->contentDir);
        } else {
            $this->contentDir = WP_CONTENT_DIR;
            if (!is_dir($this->contentDir)) {
                mkdir($this->contentDir, 0755, true);
            }
        }

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('get_option')->justReturn(Schema::CURRENT_VERSION);
        Functions\when('wp_schedule_single_event')->justReturn(true);

        $GLOBALS['wpdb'] = new AlwaysOnFakeWpdb();
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** A stub AgeIdentity that matches any recipient (mirrors BackupCommandTest). */
    private function stubIdentity(): AgeIdentity
    {
        return new class extends AgeIdentity {
            public function __construct()
            {
                // Deliberately does NOT call parent::__construct() — no
                // Keystore is needed since recipient()/recipientMatches() are
                // both overridden below (mirrors BackupCommandTest's
                // stubIdentity()).
            }

            public function recipient(): string
            {
                return 'age1test0000000000000000000000000000000000000000000000000000';
            }

            public function recipientMatches(string $candidate): bool
            {
                return true;
            }
        };
    }

    /** @return array<string,mixed> */
    private function acceptParams(): array
    {
        return [
            'snapshot_id'       => '11111111-2222-3333-4444-555555555555',
            'kind'              => 'files',
            'age_recipient'     => 'age1test0000000000000000000000000000000000000000000000000000',
            'chunk_bytes'       => 4 << 20,
            'presign_endpoint'  => 'https://cp.example/agent/v1/backups/x/presign',
            'manifest_endpoint' => 'https://cp.example/agent/v1/backups/x/manifest',
            'progress_endpoint' => '',
        ];
    }

    /** Stub the loopback probe as NOT gated (a normal 200 response). */
    private function stubLoopbackNotGated(): void
    {
        Functions\when('site_url')->justReturn('https://open.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200, 'message' => 'OK'],
            'headers'  => [],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
    }

    /** Stub the loopback probe as GATED (a 302 redirect to a login page). */
    private function stubLoopbackGated(): void
    {
        Functions\when('site_url')->justReturn('https://private.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 302, 'message' => 'Found'],
            'headers'  => ['location' => 'https://private.example.com/login/'],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(302);
        Functions\when('wp_remote_retrieve_header')->justReturn('https://private.example.com/login/');
    }

    /**
     * Non-gated site: register_shutdown_function must STILL be called
     * exactly once — this is the exact bug (#232): before the fix it was
     * called ZERO times on a non-gated site.
     */
    public function test_shutdown_runner_registers_on_non_gated_site(): void
    {
        $this->stubLoopbackNotGated();
        Functions\expect('register_shutdown_function')->once();
        Functions\expect('spawn_cron')->once()->andReturn(true);

        $cmd = new BackupCommand($this->stubIdentity());
        $res = $cmd->execute([], $this->acceptParams());

        $this->assertTrue($res['ok'] ?? false, 'acceptance failed: ' . ($res['detail'] ?? ''));
    }

    /**
     * Gated site: register_shutdown_function must ALSO be called exactly
     * once (this path already worked pre-#232; it must not regress).
     */
    public function test_shutdown_runner_registers_on_gated_site(): void
    {
        $this->stubLoopbackGated();
        Functions\expect('register_shutdown_function')->once();
        Functions\expect('spawn_cron')->once()->andReturn(true);

        $cmd = new BackupCommand($this->stubIdentity());
        $res = $cmd->execute([], $this->acceptParams());

        $this->assertTrue($res['ok'] ?? false, 'acceptance failed: ' . ($res['detail'] ?? ''));
    }
}

/**
 * Minimal in-memory $wpdb double supporting exactly the surface
 * BackupCommand::execute()'s preflight + dedup-claim + task-row-seed steps
 * touch. Follows the shared "prepare() returns a {sql,args} JSON envelope"
 * convention used across this test suite (see tests/FakeWpdb.php,
 * PacketLimitedWpdb in TaskRunnerSubStatePacketTest.php).
 */
final class AlwaysOnFakeWpdb
{
    public string $prefix = 'wp_';

    public function prepare(string $query, ...$args): string
    {
        return json_encode(['sql' => $query, 'args' => $args]) ?: '';
    }

    /** @return string|null */
    public function get_var(string $sql)
    {
        if ($sql === 'SELECT 1') {
            return '1';
        }
        if (strpos($sql, 'max_allowed_packet') !== false) {
            return '67108864'; // 64 MiB — clears both the hard-fail and warn thresholds.
        }
        return null;
    }

    /**
     * @param mixed $mode
     * @return array<int,array<string,mixed>>
     */
    public function get_results(string $sql, $mode = null): array
    {
        return []; // No tables — estimateBackupSize()'s DB contribution is 0.
    }

    /**
     * Dedup-claim SELECT — always "no existing claim" so tryClaimDedup()
     * falls through to the insert() branch.
     *
     * @param mixed $output
     * @return null
     */
    public function get_row(string $prepared, $output = null)
    {
        return null;
    }

    /**
     * @param array<string,mixed> $data
     * @param array<int,string>   $format
     */
    public function insert(string $table, array $data, array $format = []): int
    {
        return 1;
    }

    /** seedTaskRow()'s raw INSERT IGNORE. */
    public function query(string $prepared): int
    {
        return 1;
    }
}
