<?php
/**
 * SiteUpdateLockConcurrencyTest - the site lock across TWO REAL OS PROCESSES
 * (GitHub issue #328).
 *
 * WHAT THIS TEST CLAIMS, precisely. It does NOT claim to have tested the
 * atomicity of WordPress core's `INSERT IGNORE` at
 * wp-admin/includes/class-wp-upgrader.php:1065; that primitive is not
 * reimplemented here and a test pretending otherwise would encode a false
 * guarantee. What it DOES test is everything the agent layers on top of it,
 * against a compare-and-set of the same shape (an insert-if-absent performed
 * under an exclusive flock in the child fixture): that two concurrent callers
 * never both believe they hold the lock, that the loser refuses instead of
 * proceeding, and that a released lock is genuinely re-acquirable by a
 * different process.
 *
 * Single-process unit tests cannot observe any of that, which is why this file
 * pays for two child processes.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\SiteUpdateLock
 */
final class SiteUpdateLockConcurrencyTest extends TestCase
{
    /** Scratch directory for the shared option store and observation log. */
    private string $dir = '';

    protected function set_up(): void
    {
        parent::set_up();

        if (!function_exists('proc_open')) {
            $this->markTestSkipped('proc_open() is disabled, so cross-process locking cannot be exercised');
        }

        $this->dir = sys_get_temp_dir() . '/wpmgr-lockconc-' . bin2hex(random_bytes(6));
        mkdir($this->dir, 0777, true);
    }

    protected function tear_down(): void
    {
        foreach ((array) glob($this->dir . '/*') as $file) {
            if (is_string($file) && is_file($file)) {
                unlink($file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        if (is_dir($this->dir)) {
            rmdir($this->dir);
        }

        parent::tear_down();
    }

    /**
     * Start one child process.
     *
     * @param int $holdMs How long the child holds the lock once acquired.
     * @return resource
     */
    private function spawn(int $holdMs)
    {
        $cmd = [
            PHP_BINARY,
            '-d',
            'error_reporting=E_ALL',
            __DIR__ . '/fixtures/site-update-lock-child.php',
            $this->dir . '/store.json',
            $this->dir . '/log.txt',
            (string) $holdMs,
        ];

        $pipes   = [];
        $process = proc_open($cmd, [1 => ['pipe', 'w'], 2 => ['pipe', 'w']], $pipes);
        if (!is_resource($process)) {
            $this->fail('could not start the lock child process');
        }
        foreach ($pipes as $pipe) {
            stream_set_blocking($pipe, false);
        }
        $this->pipes[] = $pipes;

        return $process;
    }

    /** @var array<int,array<int,resource>> Open pipes, drained in wait(). */
    private array $pipes = [];

    /**
     * Wait for every child and return the observation log lines.
     *
     * @param array<int,resource> $processes Child handles.
     * @return array<int,string>
     */
    private function wait(array $processes): array
    {
        $stderr = '';
        foreach ($processes as $index => $process) {
            while (proc_get_status($process)['running']) {
                usleep(10000);
            }
            if (isset($this->pipes[$index][2])) {
                $stderr .= (string) stream_get_contents($this->pipes[$index][2]);
            }
            foreach ($this->pipes[$index] ?? [] as $pipe) {
                fclose($pipe);
            }
            proc_close($process);
        }
        $this->pipes = [];

        $raw = @file_get_contents($this->dir . '/log.txt');
        $this->assertIsString(
            $raw,
            'no child wrote an observation line; child stderr was: ' . ($stderr !== '' ? $stderr : '(empty)')
        );

        return array_values(array_filter(explode("\n", (string) $raw), static fn ($l) => trim($l) !== ''));
    }

    /**
     * TWO PROCESSES, ONE SITE. The winner holds the lock for 600ms; the loser
     * must refuse rather than proceed, and must do so WITHOUT ever entering the
     * critical section.
     */
    public function test_two_concurrent_processes_never_both_hold_the_lock(): void
    {
        $processes = [$this->spawn(600), $this->spawn(600)];
        $lines     = $this->wait($processes);

        $enters = array_values(array_filter($lines, static fn ($l) => str_starts_with($l, 'ENTER ')));
        $busy   = array_values(array_filter($lines, static fn ($l) => str_starts_with($l, 'BUSY ')));

        $this->assertNotSame(
            [],
            $enters,
            'the test cannot pass by BOTH children failing to acquire; at least one must enter'
        );
        $this->assertCount(
            1,
            $enters,
            'two processes entered the critical section at once: ' . implode(' | ', $lines)
        );
        $this->assertCount(1, $busy, 'the loser must refuse explicitly, not proceed: ' . implode(' | ', $lines));
    }

    /**
     * A released lock is genuinely free for a DIFFERENT process. Without the
     * owner-checked release this would either leak the lock for its whole TTL
     * or free somebody else's.
     */
    public function test_a_released_lock_is_reacquirable_by_another_process(): void
    {
        $lines = $this->wait([$this->spawn(0)]);
        $this->assertSame(['ENTER', 'EXIT'], $this->kinds($lines));

        $lines = $this->wait([$this->spawn(0)]);
        $this->assertSame(
            ['ENTER', 'EXIT', 'ENTER', 'EXIT'],
            $this->kinds($lines),
            'a lock released by one process must be immediately acquirable by the next'
        );
    }

    /**
     * The critical sections of the two runs must not overlap in wall-clock
     * time. This is the property the whole feature exists to provide, measured
     * rather than asserted from the code.
     */
    public function test_critical_sections_never_overlap(): void
    {
        // Three children, staggered by nothing: the two losers refuse and the
        // winner's interval is the only one recorded.
        $processes = [$this->spawn(300), $this->spawn(300), $this->spawn(300)];
        $lines     = $this->wait($processes);

        $intervals = [];
        $open      = [];
        foreach ($lines as $line) {
            $parts = explode(' ', $line);
            if ($parts[0] === 'ENTER') {
                $open[$parts[1]] = (float) $parts[2];
            } elseif ($parts[0] === 'EXIT' && isset($open[$parts[1]])) {
                $intervals[] = [$open[$parts[1]], (float) $parts[2]];
                unset($open[$parts[1]]);
            }
        }

        $this->assertNotSame([], $intervals, 'no child entered the critical section at all');
        $this->assertSame([], $open, 'a child entered the critical section and never left it');

        foreach ($intervals as $i => $a) {
            foreach ($intervals as $j => $b) {
                if ($i >= $j) {
                    continue;
                }
                $this->assertTrue(
                    $a[1] <= $b[0] || $b[1] <= $a[0],
                    'two critical sections overlapped: ' . implode(' | ', $lines)
                );
            }
        }
    }

    /**
     * Reduce log lines to their kind, in order.
     *
     * @param array<int,string> $lines Observation lines.
     * @return array<int,string>
     */
    private function kinds(array $lines): array
    {
        return array_map(static fn ($l) => explode(' ', $l)[0], $lines);
    }
}
