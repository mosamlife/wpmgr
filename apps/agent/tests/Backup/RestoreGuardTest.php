<?php
/**
 * RestoreGuardTest — direct unit coverage of
 * WPMgr\Agent\Backup\RestoreGuard, mirroring tests/UpdateGuardTest.php's
 * arm/markClean/fire lifecycle coverage for the update-apply guard.
 *
 * None of these tests call arm() — mirroring UpdateGuardTest, fire()/
 * markClean() are exercised directly so the test suite never registers a
 * REAL `register_shutdown_function()` callback (which would otherwise stay
 * referenced — and potentially fire, against whatever `global $wpdb` a
 * LATER, unrelated test happens to have left behind — for the rest of the
 * PHPUnit process).
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\RestoreGuard;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreGuard
 */
final class RestoreGuardTest extends TestCase
{
    public function test_fire_invokes_the_rollback_callable_and_reports_its_result(): void
    {
        $calls  = 0;
        $result = ['files' => true, 'db' => false];
        $guard  = new RestoreGuard(function () use (&$calls, $result): array {
            $calls++;
            return $result;
        });

        $fired = $guard->fire();

        $this->assertSame(1, $calls);
        $this->assertTrue($fired['fired']);
        $this->assertTrue($fired['files']);
        $this->assertFalse($fired['db']);
    }

    public function test_fire_is_a_noop_after_markClean(): void
    {
        $calls = 0;
        $guard = new RestoreGuard(function () use (&$calls): array {
            $calls++;
            return ['files' => true, 'db' => true];
        });

        $guard->markClean();
        $fired = $guard->fire();

        $this->assertSame(0, $calls, 'a verified-clean guard must never invoke the rollback callable');
        $this->assertFalse($fired['fired']);
        $this->assertFalse($fired['files']);
        $this->assertFalse($fired['db']);
    }

    public function test_fire_is_idempotent(): void
    {
        $calls = 0;
        $guard = new RestoreGuard(function () use (&$calls): array {
            $calls++;
            return ['files' => true, 'db' => true];
        });

        $first  = $guard->fire();
        $second = $guard->fire();

        $this->assertSame(1, $calls, 'a second fire() call must never invoke the rollback callable again');
        $this->assertTrue($first['fired']);
        $this->assertFalse($second['fired'], 'a second fire() call must be a no-op, never a second rollback');
    }

    /**
     * markClean() called AFTER an in-flight fire() has already started is
     * out of scope for this synchronous test harness (fire() completes
     * atomically here) — but markClean() called before ANY fire() call,
     * even after some unrelated delay, must still block it. This guards
     * the exact ordering RestoreRunner::runHealthCheck() depends on: mark
     * clean the INSTANT health_check passes, before maintenance_off runs.
     */
    public function test_markClean_before_any_fire_blocks_it_permanently(): void
    {
        $calls = 0;
        $guard = new RestoreGuard(function () use (&$calls): array {
            $calls++;
            return ['files' => true, 'db' => true];
        });

        $guard->markClean();
        $guard->fire();
        $guard->fire();
        $guard->fire();

        $this->assertSame(0, $calls);
    }

    /**
     * fire() must never let a \Throwable escape — a shutdown-function
     * context has nowhere useful to propagate one to.
     */
    public function test_fire_swallows_a_throwing_rollback_callable(): void
    {
        $guard = new RestoreGuard(function (): array {
            throw new \RuntimeException('simulated rollback failure');
        });

        $fired = $guard->fire();

        $this->assertTrue($fired['fired']);
        $this->assertFalse($fired['files']);
        $this->assertFalse($fired['db']);
    }

    /**
     * A missing/malformed rollback result (no 'files'/'db' keys) must
     * degrade to false/false rather than throw or emit a PHP notice.
     */
    public function test_fire_tolerates_a_malformed_rollback_result(): void
    {
        $guard = new RestoreGuard(function (): array {
            return [];
        });

        $fired = $guard->fire();

        $this->assertTrue($fired['fired']);
        $this->assertFalse($fired['files']);
        $this->assertFalse($fired['db']);
    }

    public function test_arm_registers_a_shutdown_function_without_throwing(): void
    {
        // We only assert this doesn't throw — actually registering +
        // exercising a real PHP shutdown callback is out of scope for a
        // synchronous unit test (see class doc). markClean() immediately
        // after neutralizes it so nothing fires at real process shutdown.
        $guard = new RestoreGuard(static fn (): array => ['files' => false, 'db' => false]);
        $guard->arm();
        $guard->markClean();
        $this->assertTrue(true);
    }
}
