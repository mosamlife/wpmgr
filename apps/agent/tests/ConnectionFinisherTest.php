<?php
/**
 * ConnectionFinisherTest — GitHub issue #274 regression coverage.
 *
 * ConnectionFinisher is the SAPI-aware "flush the response, keep the worker
 * running" ladder shared by every in-process early-ACK command
 * (BackupCommand, DbCleanCommand, DbOrphanDeleteCommand, Plugin::runDiagnostics):
 *
 *   1. fastcgi_finish_request() — PHP-FPM.
 *   2. litespeed_finish_request() — OpenLiteSpeed / LiteSpeed (GH #274; the
 *      root cause this class fixes — LiteSpeed exposes NO
 *      fastcgi_finish_request(), so before this class existed a LiteSpeed
 *      host had no early flush at all, and a reverse proxy in front of the
 *      control plane 504'd waiting on the header read for the full backup).
 *   3. A portable best-effort flush — everything else.
 *
 * Tests (a)-(e) exercise the ladder purely through the constructor-injected
 * seams — no Brain Monkey/Patchwork/separate-process needed, since LiteSpeed
 * cannot be reproduced in CI and the seam is the whole point. Test (f) is the
 * one exception: it exercises the REAL default fallback (header()/
 * ob_get_level()/ob_end_flush()/flush()), which does touch PHP internals, so
 * the class runs under process isolation to keep those redefinitions from
 * leaking into the rest of the suite.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Support\ConnectionFinisher;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\ConnectionFinisher
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class ConnectionFinisherTest extends TestCase
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
    // (a) FPM: fastcgi_finish_request available -> invoked once, 'fpm' returned,
    //     litespeed never invoked, fallback never called.
    // -------------------------------------------------------------------------

    public function test_fpm_rung_invokes_fastcgi_and_returns_fpm(): void
    {
        $available = ['fastcgi_finish_request'];
        $invoked   = [];
        $fallbackCalled = false;

        $finisher = new ConnectionFinisher(
            static fn (string $fn): bool => in_array($fn, $available, true),
            static function (string $fn) use (&$invoked): void {
                $invoked[] = $fn;
            },
            static function () use (&$fallbackCalled): void {
                $fallbackCalled = true;
            }
        );

        $result = $finisher->finish();

        self::assertSame('fpm', $result);
        self::assertSame(['fastcgi_finish_request'], $invoked);
        self::assertFalse($fallbackCalled);
    }

    // -------------------------------------------------------------------------
    // (b) LiteSpeed: fastcgi unavailable, litespeed_finish_request available ->
    //     invoked once, 'litespeed' returned. THE #274 fix.
    // -------------------------------------------------------------------------

    public function test_litespeed_rung_invokes_litespeed_and_returns_litespeed(): void
    {
        $available = ['litespeed_finish_request'];
        $invoked   = [];
        $fallbackCalled = false;

        $finisher = new ConnectionFinisher(
            static fn (string $fn): bool => in_array($fn, $available, true),
            static function (string $fn) use (&$invoked): void {
                $invoked[] = $fn;
            },
            static function () use (&$fallbackCalled): void {
                $fallbackCalled = true;
            }
        );

        $result = $finisher->finish();

        self::assertSame('litespeed', $result);
        self::assertSame(['litespeed_finish_request'], $invoked);
        self::assertFalse($fallbackCalled);
    }

    // -------------------------------------------------------------------------
    // (c) Neither available -> fallback called once, 'fallback' returned.
    // -------------------------------------------------------------------------

    public function test_neither_available_calls_fallback_and_returns_fallback(): void
    {
        $invoked        = [];
        $fallbackCalls  = 0;

        $finisher = new ConnectionFinisher(
            static fn (string $fn): bool => false,
            static function (string $fn) use (&$invoked): void {
                $invoked[] = $fn;
            },
            static function () use (&$fallbackCalls): void {
                $fallbackCalls++;
            }
        );

        $result = $finisher->finish();

        self::assertSame('fallback', $result);
        self::assertSame(1, $fallbackCalls);
        self::assertSame([], $invoked);
    }

    // -------------------------------------------------------------------------
    // (d) Precedence: both available -> fpm wins, litespeed NOT invoked.
    // -------------------------------------------------------------------------

    public function test_when_both_available_fpm_wins_over_litespeed(): void
    {
        $invoked = [];

        $finisher = new ConnectionFinisher(
            static fn (string $fn): bool => true, // both fastcgi_finish_request AND litespeed_finish_request "exist"
            static function (string $fn) use (&$invoked): void {
                $invoked[] = $fn;
            },
            static function (): void {
                self::fail('fallback must not run when fastcgi_finish_request is available');
            }
        );

        $result = $finisher->finish();

        self::assertSame('fpm', $result);
        self::assertSame(['fastcgi_finish_request'], $invoked);
    }

    // -------------------------------------------------------------------------
    // (e) Guard-order invariant: $invoke is NEVER called for a name whose
    //     $available returned false for.
    // -------------------------------------------------------------------------

    public function test_invoke_is_never_called_for_a_name_available_said_no_to(): void
    {
        // $available only allows litespeed_finish_request -> the ladder must
        // ask about fastcgi_finish_request (and get 'no'), then ask about
        // litespeed_finish_request (and get 'yes'), then invoke ONLY the
        // latter. If $invoke were ever called with 'fastcgi_finish_request'
        // that would mean the ladder dispatched a function $available said
        // was missing.
        $askedButUnavailable = [];

        $finisher = new ConnectionFinisher(
            static function (string $fn) use (&$askedButUnavailable): bool {
                if ($fn === 'litespeed_finish_request') {
                    return true;
                }
                $askedButUnavailable[] = $fn;
                return false;
            },
            static function (string $fn) use (&$askedButUnavailable): void {
                if (in_array($fn, $askedButUnavailable, true)) {
                    self::fail("invoke() called for '{$fn}', which available() reported as missing");
                }
            },
            static function (): void {
                self::fail('fallback must not run when litespeed_finish_request is available');
            }
        );

        $result = $finisher->finish();

        self::assertSame('litespeed', $result);
        self::assertSame(['fastcgi_finish_request'], $askedButUnavailable);
    }

    // -------------------------------------------------------------------------
    // (e2) canDetach() answers the SAME question finish() answers, without
    //      flushing. A caller whose decision to start work at all depends on
    //      whether the connection can be released has to ask BEFORE the
    //      response is ready (the CP-commanded self-update refuses the whole
    //      operation at its arm on this answer), so the two must agree on every
    //      configuration or that caller refuses sites that would have worked,
    //      or promises sites that cannot.
    // -------------------------------------------------------------------------

    public function test_can_detach_agrees_with_the_rung_finish_would_return(): void
    {
        $configurations = [
            'fpm'       => ['fastcgi_finish_request'],
            'litespeed' => ['litespeed_finish_request'],
            'both'      => ['fastcgi_finish_request', 'litespeed_finish_request'],
            'neither'   => [],
        ];

        foreach ($configurations as $label => $available) {
            $probe = static fn (string $fn): bool => in_array($fn, $available, true);
            $noop  = static function (string $fn): void {
            };

            // Two instances, because finish() has side effects and canDetach()
            // must not need them to have happened.
            $canDetach = (new ConnectionFinisher($probe, $noop, static function (): void {
            }))->canDetach();
            $rung = (new ConnectionFinisher($probe, $noop, static function (): void {
            }))->finish();

            self::assertSame(
                in_array($rung, ConnectionFinisher::DETACHING_RUNGS, true),
                $canDetach,
                "canDetach() disagrees with finish() on the '{$label}' SAPI, which returned '{$rung}'"
            );
        }
    }

    /**
     * The rung-name list is the shared vocabulary between a caller that decides
     * before the flush and one that decides after, so it is pinned rather than
     * left to drift.
     */
    public function test_the_detaching_rung_names_are_pinned(): void
    {
        self::assertSame(['fpm', 'litespeed'], ConnectionFinisher::DETACHING_RUNGS);
        self::assertNotContains('fallback', ConnectionFinisher::DETACHING_RUNGS);
    }

    // -------------------------------------------------------------------------
    // (f) defaultFallbackFlush() — the ONLY case touching real PHP internals.
    //     In the PHPUnit CLI SAPI, function_exists('fastcgi_finish_request')
    //     and function_exists('litespeed_finish_request') are both naturally
    //     false, so a plain `new ConnectionFinisher()` (all-default seams)
    //     already reaches the real fallback without needing to fake
    //     unavailability.
    // -------------------------------------------------------------------------

    public function test_default_fallback_flush_flushes_without_fatal_and_sends_connection_close(): void
    {
        $capturedHeaders = [];
        Functions\when('headers_sent')->justReturn(false);
        Functions\when('header')->alias(static function (string $header) use (&$capturedHeaders): void {
            $capturedHeaders[] = $header;
        });

        // Two buffer levels deep -> the while(ob_get_level() > 0) loop must
        // drain both via ob_end_flush(), never echoing a body of its own.
        $level = 2;
        Functions\when('ob_get_level')->alias(static function () use (&$level): int {
            return $level;
        });
        Functions\expect('ob_end_flush')
            ->twice()
            ->andReturnUsing(static function () use (&$level): bool {
                $level--;
                return true;
            });
        Functions\expect('flush')->once();

        $finisher = new ConnectionFinisher();
        $result   = $finisher->finish();

        self::assertSame('fallback', $result);
        self::assertContains('Connection: close', $capturedHeaders);
        self::assertContains('Content-Encoding: none', $capturedHeaders);
    }
}
