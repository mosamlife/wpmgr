<?php
/**
 * BackupConnectionFinisherTest — GitHub issue #274 regression: BackupCommand
 * releases the CP's HTTP connection via the SAPI-aware ConnectionFinisher
 * (fastcgi/litespeed/fallback) from inside the in-process shutdown-function
 * runner, for every SAPI the ladder supports — not only PHP-FPM.
 *
 * Coverage:
 *   - fpm/litespeed/fallback configs: the shutdown closure calls the
 *     injected ConnectionFinisher exactly once, dispatching the correct
 *     rung for the given SAPI config, AFTER the dedup lock is claimed and
 *     the shutdown runner is registered, and BEFORE the heavy TaskRunner
 *     work begins. A probe exception thrown by the injected
 *     available/invoke/fallback closures aborts the closure the instant
 *     finish() completes, so TaskRunner — which needs a live
 *     Keystore/Signer/DB connection — never gets constructed. That keeps
 *     this test fast and independent of TaskRunner's own machinery, which
 *     is covered elsewhere (TaskRunnerTest, TaskRunnerLockTest, ...).
 *   - A validation-refused run (bad recipient) never registers the shutdown
 *     runner, so finish() can never fire.
 *   - A dedup-refused run (an in-flight claim already exists) never
 *     registers the shutdown runner either, and its refuse() body carries
 *     code=runner_in_flight for the CP to branch on.
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
use WPMgr\Agent\Support\ConnectionFinisher;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\BackupCommand
 * @covers \WPMgr\Agent\Support\ConnectionFinisher
 */
final class BackupConnectionFinisherTest extends TestCase
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
            $this->contentDir = sys_get_temp_dir() . '/wpmgr-cf-' . bin2hex(random_bytes(6));
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
        Functions\when('spawn_cron')->justReturn(true);
        Functions\when('site_url')->justReturn('https://open.example.com/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn([
            'response' => ['code' => 200, 'message' => 'OK'],
            'headers'  => [],
            'body'     => '',
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);

        $GLOBALS['wpdb'] = new ConnectionFinisherFakeWpdb();
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
                // Keystore needed since recipient()/recipientMatches() are
                // both overridden below.
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

    /**
     * A ConnectionFinisher whose $invoke/$fallback closures record the rung
     * that fired and then throw a sentinel exception. Because the exception
     * propagates out of the shutdown closure's `$finisher->finish()` call —
     * BEFORE that closure's own try/catch (which wraps only the TaskRunner
     * construction/run()) is reached — this proves both WHICH rung fired
     * and that it fired strictly before the heavy work could have started.
     *
     * @param string       $availableName The one function name available()
     *                                    answers 'yes' to; '' means none (the
     *                                    fallback rung).
     * @param list<string> $log           Recording sink, by reference.
     */
    private function probeFinisher(string $availableName, array &$log): ConnectionFinisher
    {
        return new ConnectionFinisher(
            static fn (string $fn): bool => $availableName !== '' && $fn === $availableName,
            static function (string $fn) use (&$log): void {
                $log[] = 'invoke:' . $fn;
                throw new ConnectionFinisherProbeException($fn);
            },
            static function () use (&$log): void {
                $log[] = 'fallback';
                throw new ConnectionFinisherProbeException('fallback');
            }
        );
    }

    /**
     * Drive execute() to acceptance, capture the registered shutdown closure
     * (WITHOUT letting it run as a real PHP shutdown handler), then invoke
     * it once ourselves and assert the probe fired exactly once with the
     * expected rung.
     */
    private function driveAndAssertRung(string $sapi, string $availableName, string $expectedLogEntry): void
    {
        $log      = [];
        $finisher = $this->probeFinisher($availableName, $log);

        /** @var callable|null $captured */
        $captured = null;
        Functions\expect('register_shutdown_function')
            ->once()
            ->andReturnUsing(function ($cb) use (&$captured): bool {
                $captured = $cb;
                return true;
            });

        $cmd = new BackupCommand($this->stubIdentity(), $finisher);
        $res = $cmd->execute([], $this->acceptParams());

        self::assertTrue($res['ok'] ?? false, "{$sapi}: acceptance failed: " . ($res['detail'] ?? ''));
        self::assertNotNull($captured, "{$sapi}: shutdown runner was not registered");
        self::assertSame([], $log, "{$sapi}: finish() must not fire before the shutdown closure runs");

        try {
            ($captured)();
            self::fail("{$sapi}: expected the ConnectionFinisher probe exception to propagate out of the shutdown closure");
        } catch (ConnectionFinisherProbeException $e) {
            // Expected — proves finish() fired, and fired BEFORE TaskRunner
            // ever constructed (TaskRunner's own try/catch only wraps ITS
            // construction/run(), so it could not have swallowed this).
        }

        self::assertSame([$expectedLogEntry], $log, "{$sapi}: finish() must fire exactly once, via the {$sapi} rung");
    }

    public function test_fpm_config_finish_fires_once_via_fastcgi(): void
    {
        $this->driveAndAssertRung('fpm', 'fastcgi_finish_request', 'invoke:fastcgi_finish_request');
    }

    public function test_litespeed_config_finish_fires_once_via_litespeed(): void
    {
        $this->driveAndAssertRung('litespeed', 'litespeed_finish_request', 'invoke:litespeed_finish_request');
    }

    public function test_fallback_config_finish_fires_once_via_fallback(): void
    {
        $this->driveAndAssertRung('fallback', '', 'fallback');
    }

    public function test_validation_refused_run_never_registers_shutdown_runner(): void
    {
        Functions\expect('register_shutdown_function')->never();

        $log      = [];
        $finisher = $this->probeFinisher('fastcgi_finish_request', $log);
        $cmd      = new BackupCommand($this->stubIdentity(), $finisher);

        $params                   = $this->acceptParams();
        $params['age_recipient']  = ''; // fails the "missing age recipient" guard, before the dedup claim.

        $res = $cmd->execute([], $params);

        self::assertFalse($res['ok'] ?? true);
        self::assertSame([], $log, 'finish() must never fire on a validation refusal');
    }

    public function test_dedup_refused_run_never_registers_shutdown_runner_and_carries_code(): void
    {
        Functions\expect('register_shutdown_function')->never();

        $wpdb = $GLOBALS['wpdb'];
        self::assertInstanceOf(ConnectionFinisherFakeWpdb::class, $wpdb);
        $wpdb->existingClaim = true; // Simulate an in-flight claim within the dedup window.

        $log      = [];
        $finisher = $this->probeFinisher('fastcgi_finish_request', $log);
        $cmd      = new BackupCommand($this->stubIdentity(), $finisher);

        $res = $cmd->execute([], $this->acceptParams());

        self::assertFalse($res['ok'] ?? true);
        self::assertSame('runner_in_flight', $res['code'] ?? null, 'the in-flight refuse body must carry code=runner_in_flight');
        self::assertSame([], $log, 'finish() must never fire on a dedup refusal');
    }
}

/**
 * Sentinel exception the probe finisher throws to prove finish() fired,
 * and fired before any heavier work, without needing a live TaskRunner.
 */
final class ConnectionFinisherProbeException extends \RuntimeException
{
}

/**
 * Minimal in-memory $wpdb double for the acceptance path BackupCommand::
 * execute() touches (preflight + dedup-claim + task-row-seed). Mirrors
 * AlwaysOnFakeWpdb (BackupAlwaysOnRunnerTest.php) with one addition:
 * $existingClaim toggles tryClaimDedup() into the "already in flight" branch.
 */
final class ConnectionFinisherFakeWpdb
{
    public string $prefix = 'wp_';

    /** When true, get_row() reports an existing, still-fresh dedup claim. */
    public bool $existingClaim = false;

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
     * Dedup-claim SELECT.
     *
     * @param mixed $output
     * @return object|null
     */
    public function get_row(string $prepared, $output = null)
    {
        if (!$this->existingClaim) {
            return null;
        }
        return (object) ['pid' => 12345, 'started_at' => time()];
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
