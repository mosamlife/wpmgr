<?php
/**
 * Shared dbDelta() capture bridge for tests that drive Schema::ensureCurrent().
 *
 * The dbDelta() shim is a SINGLE process-global function for the whole PHPUnit
 * run (PHP can't redeclare or scope a function per-test). Tests therefore
 * declare it once — `function dbDelta($sql) { TestDbDeltaCapture::record($sql); }`
 * — and route captures through the currently-installed closure here. Each test
 * installs its own `$onRecord` in set_up() and clears it in tear_down(), so the
 * one global dbDelta() always forwards to whichever test is running (or no-ops
 * when no closure is installed). This lives in its own PSR-4 file so any test
 * (e.g. PluginActivationTest, SchemaTest) can reference it regardless of which
 * test file the autoloader has loaded first.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Bridge between the globally-declared dbDelta() shim and per-test capture
 * arrays.
 */
final class TestDbDeltaCapture
{
    /** @var (callable(string):void)|null */
    public static $onRecord = null;

    public static function reset(): void
    {
        self::$onRecord = null;
    }

    public static function record(string $sql): void
    {
        if (self::$onRecord !== null) {
            (self::$onRecord)($sql);
        }
    }
}
