<?php
/**
 * UpdateRunnerAvailabilityTest — GitHub issue #208: a real pending core (and
 * plugin/theme) update was silently reported as "already up to date" because
 * UpdateRunner::availableVersion() read WordPress's cached update_core /
 * update_plugins / update_themes transient WITHOUT forcing a fresh check
 * first, so a momentarily stale/expired/never-yet-populated transient was
 * indistinguishable from "genuinely no update available".
 *
 * These tests exercise UpdateRunner::availableVersion() directly (the public
 * entry point; coreUpdateVersion()/pluginUpdateVersion()/themeUpdateVersion()
 * are private implementation details reached only through it for a 'latest'
 * request) and prove:
 *   1. A stale/empty transient that a forced check WOULD populate with a real
 *      pending update is no longer silently read as "no update" — the forced
 *      check runs first and the real version is returned.
 *   2. A transient that is genuinely up to date even after the forced check
 *      still correctly reports '' (unchanged behavior).
 *   3. A transient that STILL cannot be resolved even after the forced check
 *      (network failure, constrained runtime) reports `null` — a value
 *      distinct from '' — so a caller can never fold "couldn't tell" into
 *      "nothing to do".
 *   4. The forced check runs at most ONCE per availableVersion() call, never
 *      in a loop, and never at all for an explicit (non-'latest') version
 *      request.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateRunner
 */
final class UpdateRunnerAvailabilityTest extends TestCase
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

    // =========================================================================
    // core
    // =========================================================================

    public function test_core_forced_check_discovers_a_real_update_behind_a_stale_empty_transient(): void
    {
        // Simulates the reported bug precisely: get_site_transient() would
        // return false/empty (never yet populated, or expired) BEFORE any
        // check runs. wp_version_check() is the forced fresh check that
        // populates it with a genuine pending core update.
        $transient = false;
        Functions\expect('wp_version_check')
            ->once()
            ->andReturnUsing(function () use (&$transient): void {
                $update           = new \stdClass();
                $update->response = 'upgrade';
                $update->version  = '6.5.2';

                $populated          = new \stdClass();
                $populated->updates = [$update];
                $transient          = $populated;
            });
        Functions\when('get_site_transient')->alias(static function (string $key) use (&$transient) {
            return $key === 'update_core' ? $transient : false;
        });

        $runner = new UpdateRunner();

        $this->assertSame(
            '6.5.2',
            $runner->availableVersion('core', 'core', 'latest'),
            'a real pending core update behind a stale/empty transient must be discovered by the forced check'
        );
    }

    public function test_core_reports_up_to_date_when_the_forced_check_confirms_nothing_pending(): void
    {
        $update           = new \stdClass();
        $update->response = 'latest';
        $update->version  = '6.5.2';

        $transient          = new \stdClass();
        $transient->updates = [$update];

        Functions\expect('wp_version_check')->once()->andReturnNull();
        Functions\when('get_site_transient')->alias(static function (string $key) use ($transient) {
            return $key === 'update_core' ? $transient : false;
        });

        $runner = new UpdateRunner();

        $this->assertSame(
            '',
            $runner->availableVersion('core', 'core', 'latest'),
            'a genuinely up-to-date result after the forced check must still report no update (unchanged behavior)'
        );
    }

    public function test_core_reports_undetermined_when_still_unresolvable_after_forcing(): void
    {
        // The forced check runs but (network failure / constrained runtime)
        // never populates a well-formed transient — it stays false.
        Functions\expect('wp_version_check')->once()->andReturnNull();
        Functions\when('get_site_transient')->justReturn(false);

        $runner = new UpdateRunner();

        $this->assertNull(
            $runner->availableVersion('core', 'core', 'latest'),
            'availability that cannot be determined even after a forced check must be null, not "" (up to date)'
        );
    }

    public function test_core_forced_check_runs_exactly_once_not_in_a_loop(): void
    {
        Functions\expect('wp_version_check')->once()->andReturnNull();
        Functions\when('get_site_transient')->justReturn(false);

        $result = (new UpdateRunner())->availableVersion('core', 'core', 'latest');

        // Functions\expect(...)->once() itself asserts the call count on
        // Monkey\tearDown(); a second invocation here would fail that
        // assertion, proving a single resolution never re-forces the check.
        // The explicit assertion below is just to keep this a non-risky
        // test — the real proof is the mock's own call-count expectation.
        $this->assertNull($result);
    }

    public function test_core_explicit_version_request_never_forces_a_check(): void
    {
        Functions\expect('wp_version_check')->never();

        $runner = new UpdateRunner();

        $this->assertSame('6.4.1', $runner->availableVersion('core', 'core', '6.4.1'));
    }

    // =========================================================================
    // plugin
    // =========================================================================

    public function test_plugin_forced_check_discovers_a_real_update_behind_a_stale_empty_transient(): void
    {
        $transient = false;
        Functions\expect('wp_update_plugins')
            ->once()
            ->andReturnUsing(function () use (&$transient): void {
                $entry              = new \stdClass();
                $entry->new_version = '5.3.1';

                $populated           = new \stdClass();
                $populated->response = ['akismet/akismet.php' => $entry];
                $transient           = $populated;
            });
        Functions\when('get_site_transient')->alias(static function (string $key) use (&$transient) {
            return $key === 'update_plugins' ? $transient : false;
        });

        $runner = new UpdateRunner();

        $this->assertSame(
            '5.3.1',
            $runner->availableVersion('plugin', 'akismet/akismet.php', 'latest'),
            'a real pending plugin update behind a stale/empty transient must be discovered by the forced check'
        );
    }

    public function test_plugin_reports_undetermined_when_still_unresolvable_after_forcing(): void
    {
        Functions\expect('wp_update_plugins')->once()->andReturnNull();
        Functions\when('get_site_transient')->justReturn(false);

        $runner = new UpdateRunner();

        $this->assertNull(
            $runner->availableVersion('plugin', 'akismet/akismet.php', 'latest'),
            'plugin availability that cannot be determined even after a forced check must be null, not ""'
        );
    }

    public function test_plugin_reports_up_to_date_when_the_forced_check_confirms_nothing_pending(): void
    {
        // A well-formed, forced-fresh response with no entry for this slug —
        // WordPress's own "nothing pending for this plugin" shape.
        $transient           = new \stdClass();
        $transient->response = [];

        Functions\expect('wp_update_plugins')->once()->andReturnNull();
        Functions\when('get_site_transient')->alias(static function (string $key) use ($transient) {
            return $key === 'update_plugins' ? $transient : false;
        });

        $runner = new UpdateRunner();

        $this->assertSame('', $runner->availableVersion('plugin', 'akismet/akismet.php', 'latest'));
    }

    // =========================================================================
    // theme
    // =========================================================================

    public function test_theme_forced_check_discovers_a_real_update_behind_a_stale_empty_transient(): void
    {
        $transient = false;
        Functions\expect('wp_update_themes')
            ->once()
            ->andReturnUsing(function () use (&$transient): void {
                $populated           = new \stdClass();
                $populated->response = [
                    'twentytwentyfour' => ['new_version' => '1.2'],
                ];
                $transient           = $populated;
            });
        Functions\when('get_site_transient')->alias(static function (string $key) use (&$transient) {
            return $key === 'update_themes' ? $transient : false;
        });

        $runner = new UpdateRunner();

        $this->assertSame(
            '1.2',
            $runner->availableVersion('theme', 'twentytwentyfour', 'latest'),
            'a real pending theme update behind a stale/empty transient must be discovered by the forced check'
        );
    }

    public function test_theme_reports_undetermined_when_still_unresolvable_after_forcing(): void
    {
        Functions\expect('wp_update_themes')->once()->andReturnNull();
        Functions\when('get_site_transient')->justReturn(false);

        $runner = new UpdateRunner();

        $this->assertNull(
            $runner->availableVersion('theme', 'twentytwentyfour', 'latest'),
            'theme availability that cannot be determined even after a forced check must be null, not ""'
        );
    }
}
