<?php
/**
 * Tests for the v0.9.0 on-demand refresh_inventory command.
 *
 * The Router/Connector already enforces the signed-JWT contract (aud, cmd,
 * jti, exp) BEFORE the command's execute() runs. These tests therefore focus
 * on the surface the command itself owns: body-shape validation, and the
 * transient-refresh + metadata-push delegation contract.
 *
 * The command takes Closures rather than concrete Enrollment / Scheduler
 * references (those classes are `final` and cannot be doubled), so the tests
 * inject simple counter / sink closures.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Commands\RefreshInventoryCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\RefreshInventoryCommand
 */
final class RefreshInventoryCommandTest extends TestCase
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

    public function test_name_is_refresh_inventory(): void
    {
        $cmd = new RefreshInventoryCommand(
            static function (): void {},
            static fn (): array => ['ok' => true, 'status' => 200, 'code' => 'ok', 'message' => 'OK.']
        );
        $this->assertSame('refresh_inventory', $cmd->name());
    }

    public function test_refuses_non_empty_body(): void
    {
        $refreshCalls = 0;
        $pushCalls    = 0;
        $cmd = new RefreshInventoryCommand(
            static function () use (&$refreshCalls): void {
                $refreshCalls++;
            },
            static function () use (&$pushCalls): array {
                $pushCalls++;
                return ['ok' => true];
            }
        );

        $res = $cmd->execute([], ['unexpected' => true]);

        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('empty object', $res['detail']);
        // Body validation runs BEFORE either dependency is touched.
        $this->assertSame(0, $refreshCalls);
        $this->assertSame(0, $pushCalls);
    }

    public function test_success_refreshes_transients_then_pushes_metadata(): void
    {
        $order        = [];
        $refreshCalls = 0;
        $pushCalls    = 0;

        $cmd = new RefreshInventoryCommand(
            static function () use (&$order, &$refreshCalls): void {
                $order[]        = 'refresh';
                $refreshCalls++;
            },
            static function () use (&$order, &$pushCalls): array {
                $order[]    = 'push';
                $pushCalls++;
                return ['ok' => true, 'status' => 200, 'code' => 'ok', 'message' => 'OK.'];
            }
        );

        $res = $cmd->execute([], []);

        $this->assertTrue($res['ok']);
        $this->assertSame('inventory refreshed and pushed', $res['detail']);
        $this->assertSame(1, $refreshCalls);
        $this->assertSame(1, $pushCalls);
        // Refresh MUST happen before push so the pushed inventory reflects the
        // fresh transient state.
        $this->assertSame(['refresh', 'push'], $order);
    }

    public function test_failure_when_push_metadata_returns_not_ok(): void
    {
        $cmd = new RefreshInventoryCommand(
            static function (): void {},
            static fn (): array => [
                'ok'      => false,
                'status'  => 0,
                'code'    => 'unreachable',
                'message' => 'Control plane is unreachable.',
            ]
        );

        $res = $cmd->execute([], []);

        $this->assertFalse($res['ok']);
        $this->assertSame('Control plane is unreachable.', $res['detail']);
    }

    public function test_push_proceeds_even_when_refresh_throws(): void
    {
        $pushCalls = 0;
        $cmd = new RefreshInventoryCommand(
            static function (): void {
                throw new \RuntimeException('wp.org unreachable');
            },
            static function () use (&$pushCalls): array {
                $pushCalls++;
                return ['ok' => true, 'status' => 200, 'code' => 'ok', 'message' => 'OK.'];
            }
        );

        $res = $cmd->execute([], []);

        $this->assertTrue($res['ok']);
        $this->assertSame(1, $pushCalls);
    }
}
