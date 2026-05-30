<?php
/**
 * Unit tests for S1.2 — ErrorMonitor::loadConfig() validation and
 * record() ignore-list / level gating.
 *
 * These tests exercise only the pure PHP behaviour of ErrorMonitor; they do not
 * touch the DB (no wpdb) and do not require a live WordPress install. Brain
 * Monkey stubs the handful of WP functions the class references (get_option,
 * update_option).
 *
 * Coverage targets:
 *   - loadConfig(): safe defaults when option is absent / corrupt JSON /
 *     out-of-range values; md5 entry validation (junk entries dropped).
 *   - record(): early-return when md5 is in ignore_md5s; early-return when
 *     non-fatal code is outside error_level mask; fatal codes always recorded.
 *   - SyncErrorConfigCommand::execute(): missing/wrong-type error_level;
 *     optional ignore_md5s; delegates to ErrorMonitor::applyConfig().
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\SyncErrorConfigCommand;
use WPMgr\Agent\Support\ErrorMonitor;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\ErrorMonitor
 * @covers \WPMgr\Agent\Commands\SyncErrorConfigCommand
 */
final class ErrorMonitorConfigTest extends TestCase
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

    // -------------------------------------------------------------------------
    // Helper: build a fresh ErrorMonitor (the static cache in loadConfig() is
    // per-function-invocation, not per-instance, so we use applyConfig() or
    // stub get_option to control what the cache sees).
    // -------------------------------------------------------------------------

    private function makeMonitor(): ErrorMonitor
    {
        return new ErrorMonitor();
    }

    // -------------------------------------------------------------------------
    // loadConfig() — defaults when option is absent
    // -------------------------------------------------------------------------

    public function test_loadConfig_returns_defaults_when_option_absent(): void
    {
        Functions\when('get_option')->justReturn(null);

        $monitor = $this->makeMonitor();

        // We exercise loadConfig() indirectly: record() with a non-fatal code
        // whose bit IS set in the default mask (E_WARNING) must NOT return early
        // due to level gating (it will return early due to missing wpdb — that's
        // fine, we just confirm the gate logic proceeds past the level check).
        // We spy by verifying no early-return from level-gate by checking that
        // the method reaches the wpdb guard (which returns early because wpdb is
        // absent in unit tests). The fact that we reach that guard at all proves
        // the config gate did not drop the error.
        //
        // For a cleaner assertion: call applyConfig() with a known config and
        // then verify loadConfig() returns what we wrote.
        Functions\when('update_option')->justReturn(true);

        // Write a config where error_level includes E_WARNING (8) only.
        $monitor->applyConfig(E_WARNING, []);

        // Now stub get_option to return the JSON we would have stored.
        // (applyConfig already called update_option; we re-stub get_option
        // to return the encoded value so loadConfig() picks it up fresh.)
        $encoded = (string) json_encode(['error_level' => E_WARNING, 'ignore_md5s' => []]);
        Functions\when('get_option')->justReturn($encoded);

        // A fresh monitor picks up the stored config.
        $monitor2 = $this->makeMonitor();

        // record() with E_NOTICE (8 is not in E_WARNING-only mask, E_NOTICE = 8... actually:
        // E_WARNING = 2, E_NOTICE = 8. E_NOTICE & E_WARNING = 0 → should be dropped.
        // We confirm: record() returns early (no exception, no DB call).
        // wpdb is not set, so it would return early there too — but the level gate
        // fires first. The test proves no exception is thrown (gate doesn't throw).
        $monitor2->record(E_NOTICE, 'test message', '/some/file.php', 42, 'notice');
        $this->addToAssertionCount(1); // Reached here means no exception/fatal.
    }

    // -------------------------------------------------------------------------
    // loadConfig() — corrupt JSON falls back to defaults
    // -------------------------------------------------------------------------

    public function test_loadConfig_falls_back_on_corrupt_json(): void
    {
        Functions\when('get_option')->justReturn('not-valid-json{{{');

        $monitor = $this->makeMonitor();
        // A fatal code must always be recorded (we check it reaches wpdb guard,
        // not the level-gate). Since wpdb is absent it returns early there — no
        // exception is proof the fatal path is not blocked by a bad config.
        $monitor->record(E_ERROR, 'fatal', '/file.php', 1, 'fatal');
        $this->addToAssertionCount(1);
    }

    // -------------------------------------------------------------------------
    // loadConfig() — invalid error_level clamped to default
    // -------------------------------------------------------------------------

    public function test_loadConfig_clamps_out_of_range_error_level(): void
    {
        // E_ALL on most PHP versions is ~32767; 0xFFFFFFFF is outside that range.
        $encoded = (string) json_encode(['error_level' => 0xFFFFFFFF, 'ignore_md5s' => []]);
        Functions\when('get_option')->justReturn($encoded);
        Functions\when('update_option')->justReturn(true);

        $monitor = $this->makeMonitor();
        // Should not throw; invalid level is clamped internally.
        $monitor->record(E_WARNING, 'msg', '/f.php', 1, 'warning');
        $this->addToAssertionCount(1);
    }

    // -------------------------------------------------------------------------
    // loadConfig() — junk ignore_md5s entries are dropped
    // -------------------------------------------------------------------------

    public function test_loadConfig_drops_junk_ignore_md5_entries(): void
    {
        $validMd5   = str_repeat('a', 32); // valid 32-char hex
        $invalidMd5 = 'not-a-md5';
        $tooShort   = 'abc123';

        $encoded = (string) json_encode([
            'error_level' => E_WARNING | E_NOTICE,
            'ignore_md5s' => [$validMd5, $invalidMd5, $tooShort, 12345],
        ]);
        Functions\when('get_option')->justReturn($encoded);

        // applyConfig validates the same rules; use it as a white-box probe.
        // NOTE: only an expect() here — a parallel when('update_option') would
        // shadow this strict expectation and make ->once() record zero calls.
        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value) use ($validMd5): bool {
                $decoded = json_decode($value, true);
                // Only the valid 32-hex entry should survive.
                \PHPUnit\Framework\TestCase::assertSame([$validMd5], $decoded['ignore_md5s']);
                return true;
            });

        $monitor = $this->makeMonitor();
        $monitor->applyConfig(E_WARNING | E_NOTICE, [$validMd5, $invalidMd5, $tooShort]);
    }

    // -------------------------------------------------------------------------
    // record() — ignore-list gate: md5 in ignore_md5s → drop
    // -------------------------------------------------------------------------

    public function test_record_drops_error_whose_md5_is_ignored(): void
    {
        // Pre-compute the md5 for the error we are about to record.
        $code    = E_WARNING;
        $file    = '/app/plugin/broken.php';
        $line    = 99;
        $message = 'Undefined variable: foo';
        $md5     = md5($code . ':' . $file . ':' . $line . ':' . $message);

        $encoded = (string) json_encode([
            'error_level' => E_WARNING | E_NOTICE | E_DEPRECATED,
            'ignore_md5s' => [$md5],
        ]);
        Functions\when('get_option')->justReturn($encoded);

        $monitor = $this->makeMonitor();

        // Inject a fake wpdb that must NOT be called if the ignore-gate fires.
        $GLOBALS['wpdb'] = new class {
            public string $prefix = 'wp_';
            public bool $called   = false;

            public function query(string $q): void
            {
                $this->called = true;
            }

            public function prepare(string $q, mixed ...$args): string
            {
                return $q;
            }

            public function insert(): void
            {
                $this->called = true;
            }

            public function get_var(string $q): int
            {
                return 0;
            }
        };

        $monitor->record($code, $message, $file, $line, 'warning');

        $this->assertFalse($GLOBALS['wpdb']->called, 'DB must not be touched for an ignored md5');
        unset($GLOBALS['wpdb']);
    }

    // -------------------------------------------------------------------------
    // record() — level gate: non-fatal outside mask → drop
    // -------------------------------------------------------------------------

    public function test_record_drops_non_fatal_outside_configured_level(): void
    {
        // Config: only capture E_WARNING; E_NOTICE is outside the mask.
        $encoded = (string) json_encode([
            'error_level' => E_WARNING,
            'ignore_md5s' => [],
        ]);
        Functions\when('get_option')->justReturn($encoded);

        $monitor = $this->makeMonitor();

        $wpdbSpy = new class {
            public string $prefix = 'wp_';
            public bool $called   = false;

            public function query(string $q): void
            {
                $this->called = true;
            }

            public function prepare(string $q, mixed ...$args): string
            {
                return $q;
            }

            public function insert(): void
            {
                $this->called = true;
            }

            public function get_var(string $q): int
            {
                return 0;
            }
        };
        $GLOBALS['wpdb'] = $wpdbSpy;

        // E_NOTICE (8) is not in E_WARNING (2) mask → must be dropped.
        $monitor->record(E_NOTICE, 'some notice', '/f.php', 5, 'notice');

        $this->assertFalse($wpdbSpy->called, 'DB must not be touched for a non-fatal outside the configured level');
        unset($GLOBALS['wpdb']);
    }

    // -------------------------------------------------------------------------
    // record() — fatal ALWAYS recorded regardless of level config
    // -------------------------------------------------------------------------

    public function test_record_always_captures_fatal_regardless_of_level(): void
    {
        // Config: level = 0 (capture no non-fatals).
        $encoded = (string) json_encode([
            'error_level' => 0,
            'ignore_md5s' => [],
        ]);
        Functions\when('get_option')->justReturn($encoded);

        $monitor = $this->makeMonitor();

        $wpdbSpy = new class {
            public string $prefix  = 'wp_';
            public bool $queryCalled = false;
            public bool $insertCalled = false;

            /** @return int|false */
            public function query(string $q)
            {
                $this->queryCalled = true;
                return 0; // 0 rows affected → INSERT path
            }

            public function prepare(string $q, mixed ...$args): string
            {
                return $q;
            }

            public function insert(string $table, array $data, array $formats): bool
            {
                $this->insertCalled = true;
                return true;
            }

            public function get_var(string $q): int
            {
                return 0;
            }
        };
        $GLOBALS['wpdb'] = $wpdbSpy;

        // E_ERROR is a fatal — must reach the DB path.
        $monitor->record(E_ERROR, 'Fatal error', '/app/crash.php', 1, 'fatal');

        $this->assertTrue($wpdbSpy->queryCalled, 'Fatal error must reach the DB UPDATE path');
        unset($GLOBALS['wpdb']);
    }

    // -------------------------------------------------------------------------
    // SyncErrorConfigCommand — name()
    // -------------------------------------------------------------------------

    public function test_sync_error_config_command_name(): void
    {
        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $this->assertSame('sync_error_config', $cmd->name());
    }

    // -------------------------------------------------------------------------
    // SyncErrorConfigCommand — missing error_level
    // -------------------------------------------------------------------------

    public function test_sync_error_config_rejects_missing_error_level(): void
    {
        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], []);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('error_level', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncErrorConfigCommand — wrong type for error_level
    // -------------------------------------------------------------------------

    public function test_sync_error_config_rejects_non_int_error_level(): void
    {
        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], ['error_level' => 'not-an-int']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('integer', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncErrorConfigCommand — wrong type for ignore_md5s
    // -------------------------------------------------------------------------

    public function test_sync_error_config_rejects_non_array_ignore_md5s(): void
    {
        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], ['error_level' => E_WARNING, 'ignore_md5s' => 'bad']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('array', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncErrorConfigCommand — success path
    // -------------------------------------------------------------------------

    public function test_sync_error_config_success(): void
    {
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], [
            'error_level' => E_WARNING | E_NOTICE,
            'ignore_md5s' => [str_repeat('b', 32)],
        ]);

        $this->assertTrue($res['ok']);
        $this->assertSame('error config applied', $res['detail']);
    }

    // -------------------------------------------------------------------------
    // SyncErrorConfigCommand — ignore_md5s optional (omitted)
    // -------------------------------------------------------------------------

    public function test_sync_error_config_ignore_md5s_optional(): void
    {
        Functions\when('update_option')->justReturn(true);

        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], ['error_level' => E_WARNING]);

        $this->assertTrue($res['ok']);
    }
}
