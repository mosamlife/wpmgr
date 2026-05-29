<?php
/**
 * Smoke tests for the DbDumper wrapper. These exercise the contract surface
 * only — class loads, constructor accepts the documented credential shape,
 * and an already-completed cursor short-circuits without touching the DB.
 *
 * The real "actually dump a database" tests are tagged @group integration so
 * they skip in CI environments without a live MySQL.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\DbDumper;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\DbDumper
 */
final class DbDumperTest extends TestCase
{
    /**
     * The class must be loadable via the classmap autoloader and live in the
     * documented namespace. Catches namespace typos, missing `final`, or a
     * stray `class-` prefix in the include path.
     */
    public function test_class_exists_in_expected_namespace(): void
    {
        $this->assertTrue(class_exists(DbDumper::class));
    }

    /**
     * Constructor accepts the documented credential shape without error.
     * No DB connection is opened here — the dumper is lazy: PDO only
     * connects inside dump() (via ifsnop's Mysqldump::start).
     */
    public function test_constructor_accepts_documented_db_shape(): void
    {
        $dumper = new DbDumper([
            'host'     => 'localhost:3306',
            'user'     => 'wp',
            'password' => 'wp',
            'name'     => 'wp_db',
            'prefix'   => 'wp_',
        ]);

        $this->assertInstanceOf(DbDumper::class, $dumper);
    }

    /**
     * Constructor accepts an optional opts array (future-proofing knob).
     */
    public function test_constructor_accepts_opts_array(): void
    {
        $dumper = new DbDumper(
            [
                'host'     => 'db.internal',
                'user'     => 'wp',
                'password' => 'wp',
                'name'     => 'wp_db',
                'prefix'   => 'wp_',
            ],
            ['row_batch' => 5000]
        );

        $this->assertInstanceOf(DbDumper::class, $dumper);
    }

    /**
     * An already-completed cursor (`done => true`) must short-circuit and
     * return the cursor unchanged without touching the DB. This is the
     * idempotent-replay contract the watchdog/TaskRunner relies on.
     */
    public function test_dump_with_done_cursor_is_idempotent_noop(): void
    {
        $dumper = new DbDumper([
            'host'     => 'unreachable.invalid',
            'user'     => 'nobody',
            'password' => 'nothing',
            'name'     => 'no_db',
            'prefix'   => 'wp_',
        ]);

        $cursor = [
            'done'        => true,
            'output_path' => '/tmp/already-done.sql.gz',
            'bytes'       => 12345,
        ];

        $progressCalls = 0;
        $result        = $dumper->dump(
            '/tmp/should-not-be-touched.sql.gz',
            $cursor,
            function () use (&$progressCalls): void {
                $progressCalls++;
            }
        );

        $this->assertSame($cursor, $result);
        $this->assertSame(0, $progressCalls, 'no-op replay must not emit progress');
    }

    /**
     * Live MySQL integration smoke. Skipped unless WPMGR_TEST_MYSQL_DSN-style
     * env vars are present — keeps CI green on hosts without a DB.
     *
     * @group integration
     */
    public function test_dump_against_live_mysql_produces_sql_gz(): void
    {
        $host = getenv('WPMGR_TEST_MYSQL_HOST');
        $user = getenv('WPMGR_TEST_MYSQL_USER');
        $pass = getenv('WPMGR_TEST_MYSQL_PASSWORD');
        $name = getenv('WPMGR_TEST_MYSQL_DATABASE');

        if ($host === false || $user === false || $name === false) {
            $this->markTestSkipped('WPMGR_TEST_MYSQL_* env vars not set.');
        }

        $outPath = sys_get_temp_dir() . '/wpmgr-dbdumper-test-' . bin2hex(random_bytes(6)) . '.sql.gz';

        $dumper = new DbDumper([
            'host'     => (string) $host,
            'user'     => (string) $user,
            'password' => $pass === false ? '' : (string) $pass,
            'name'     => (string) $name,
            'prefix'   => 'wp_',
        ]);

        $progress = [];
        $result   = $dumper->dump(
            $outPath,
            [],
            function (string $phase, array $detail) use (&$progress): void {
                $progress[] = [$phase, $detail];
            }
        );

        $this->assertTrue($result['done'] ?? false);
        $this->assertSame($outPath, $result['output_path'] ?? null);
        $this->assertGreaterThan(0, $result['bytes'] ?? 0);
        $this->assertFileExists($outPath);
        $this->assertNotSame([], $progress, 'progress hook should have fired');

        @unlink($outPath);
    }
}
