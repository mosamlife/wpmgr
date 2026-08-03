<?php
/**
 * SiteUpdateLockTest - the per-site update lock (GitHub issue #328).
 *
 * WHAT THESE TESTS DO AND DO NOT CLAIM. The mutual-exclusion guarantee belongs
 * to core's single `INSERT IGNORE` at wp-admin/includes/class-wp-upgrader.php:1065,
 * which this project does not reimplement and must not claim to have tested.
 * What IS tested here is the logic layered on top of it: the three-way acquire
 * verdict, the owner token that stops a stale holder freeing a live one, the
 * renewal, and (in SiteUpdateLockConcurrencyTest) the behaviour of that logic
 * across two real OS processes over an atomic compare-and-set.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\LongRunningJob;
use WPMgr\Agent\Support\SiteUpdateLock;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\SiteUpdateLock
 */
final class SiteUpdateLockTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store shared by the stubs. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];
        \WP_Upgrader::$failCreateLock = false;
        SiteUpdateLock::resetForTests();

        Functions\when('get_option')->alias(function (string $name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('update_option')->alias(function (string $name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('delete_option')->alias(function (string $name) {
            unset($this->options[$name]);
            return true;
        });
    }

    protected function tear_down(): void
    {
        SiteUpdateLock::resetForTests();
        \WP_Upgrader::$failCreateLock = false;
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_a_second_acquire_from_another_request_is_refused_while_the_first_holds(): void
    {
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());

        // A second REQUEST is simulated by dropping this process's in-memory
        // ownership while leaving the option store exactly as it is.
        SiteUpdateLock::resetForTests();

        $this->assertSame(
            SiteUpdateLock::HELD_BY_OTHER,
            SiteUpdateLock::acquire(),
            'a live lock taken by another request must refuse, never fall through'
        );
    }

    public function test_acquire_release_acquire_succeeds(): void
    {
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());
        SiteUpdateLock::release();

        SiteUpdateLock::resetForTests();
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());
    }

    public function test_a_second_acquire_in_the_same_request_is_reentrant(): void
    {
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());
        $this->assertSame(
            SiteUpdateLock::ACQUIRED,
            SiteUpdateLock::acquire(),
            'a request that already holds the lock must not refuse its own work'
        );
    }

    /**
     * THE OWNERSHIP REGRESSION LOCK. WP_Upgrader::release_lock() is an
     * unconditional delete_option() (class-wp-upgrader.php:1102 to :1103), so a
     * late release from a holder whose lock had already expired and been stolen
     * would free a LIVE holder. Do not simplify the owner check away.
     */
    public function test_release_from_a_stale_owner_does_not_free_a_live_holder(): void
    {
        // A acquires.
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());

        // A's lock expires and B steals it, writing its own owner token and a
        // fresh stamp. (Core performs exactly this steal internally at
        // class-wp-upgrader.php:1080 to :1083.)
        $this->options['wpmgr_site_update.lock']  = time();
        $this->options['wpmgr_site_update_owner'] = 'ffffffffffffffffffffffffffffffff';

        // A now releases, believing it still owns the lock.
        SiteUpdateLock::release();

        $this->assertArrayHasKey(
            'wpmgr_site_update.lock',
            $this->options,
            'a stale holder must never delete the live holder\'s lock row'
        );
        $this->assertSame('ffffffffffffffffffffffffffffffff', $this->options['wpmgr_site_update_owner']);
    }

    public function test_renew_only_restamps_a_lock_we_still_own(): void
    {
        SiteUpdateLock::acquire();
        $this->options['wpmgr_site_update.lock'] = time() - 100;

        SiteUpdateLock::renew();
        $this->assertGreaterThan(
            time() - 5,
            (int) $this->options['wpmgr_site_update.lock'],
            'renew() must move the stored timestamp forward, exactly as core does at class-wp-upgrader.php:1087'
        );

        // Now somebody else owns it: renew must leave their stamp alone.
        $this->options['wpmgr_site_update_owner'] = 'ffffffffffffffffffffffffffffffff';
        $this->options['wpmgr_site_update.lock']  = 1000;
        SiteUpdateLock::renew();
        $this->assertSame(1000, $this->options['wpmgr_site_update.lock']);
    }

    /**
     * THE FAIL-OPEN REGRESSION LOCK. Core's helper reports "locked" whenever its
     * INSERT failed AND no lock row is readable, which conflates an
     * infrastructure failure with a held lock. Reporting HELD_BY_OTHER there
     * would refuse EVERY update on that site permanently, with no in-product
     * remedy. This is the same class of mistake as guarding what core does not
     * guard, and it must stay closed.
     */
    public function test_acquire_reports_unavailable_when_the_lock_row_cannot_be_created_or_read(): void
    {
        \WP_Upgrader::$failCreateLock = true;

        $this->assertSame(
            SiteUpdateLock::UNAVAILABLE,
            SiteUpdateLock::acquire(),
            'an unreadable, uncreatable lock must NOT be reported as held by someone else'
        );
        $this->assertFalse(SiteUpdateLock::heldByThisRequest());
    }

    /**
     * A readable lock row is the ONLY thing that turns a failed create_lock()
     * into a refusal. With the row present, the same infrastructure failure is
     * correctly reported as held by someone else.
     */
    public function test_a_failed_create_with_a_readable_lock_row_is_still_a_refusal(): void
    {
        $this->options['wpmgr_site_update.lock'] = time();
        \WP_Upgrader::$failCreateLock            = true;

        $this->assertSame(SiteUpdateLock::HELD_BY_OTHER, SiteUpdateLock::acquire());
    }

    /** An UNAVAILABLE acquire holds nothing, so release() must not delete anything. */
    public function test_release_after_an_unavailable_acquire_touches_nothing(): void
    {
        $this->options['some_unrelated_option'] = 'keep me';
        \WP_Upgrader::$failCreateLock           = true;

        $this->assertSame(SiteUpdateLock::UNAVAILABLE, SiteUpdateLock::acquire());
        SiteUpdateLock::release();

        $this->assertSame(['some_unrelated_option' => 'keep me'], $this->options);
    }

    /**
     * The TTL, the update command's PHP execution budget and the long-running
     * job budget are the SAME number by design: the lock must outlive an apply
     * that runs to its own cap and not one second longer. Pinned so the three
     * cannot drift apart silently.
     */
    public function test_ttl_matches_the_apply_budget(): void
    {
        $applyLimit = new \ReflectionClassConstant(UpdateCommand::class, 'APPLY_TIME_LIMIT_SECONDS');

        $this->assertSame(900, SiteUpdateLock::TTL);
        $this->assertSame(SiteUpdateLock::TTL, $applyLimit->getValue());
        $this->assertSame(SiteUpdateLock::TTL, LongRunningJob::TIME_LIMIT_SECONDS);
    }

    /**
     * The lock name must NOT be core's own 'auto_updater'. create_lock() judges
     * staleness against the CALLER's timeout, so taking core's name with our
     * 900s would steal a lock core honours for an hour, and core's own release
     * would then delete ours: exactly the concurrency this class removes.
     */
    public function test_lock_name_is_not_cores_auto_updater(): void
    {
        SiteUpdateLock::acquire();

        $this->assertArrayHasKey('wpmgr_site_update.lock', $this->options);
        $this->assertArrayNotHasKey('auto_updater.lock', $this->options);
    }

    public function test_held_until_reports_the_stamp_plus_the_ttl(): void
    {
        $this->assertSame(0, SiteUpdateLock::heldUntil(), 'no lock row means no expiry to report');

        SiteUpdateLock::acquire();
        $stamp = (int) $this->options['wpmgr_site_update.lock'];

        $this->assertSame($stamp + SiteUpdateLock::TTL, SiteUpdateLock::heldUntil());
    }
}
