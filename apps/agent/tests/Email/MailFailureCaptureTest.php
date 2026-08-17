<?php
/**
 * MailFailureCaptureTest — GH #381 phase 1: WordPress core's own
 * `wp_mail_failed` action fires on ANY failed send (configured or not), but
 * nothing in this plugin listened for it. MailRouter::intercept()
 * (class-mail-router.php) only sees a failure when EmailConfig::is_configured()
 * is true, so a site sending through core PHPMailer or a third-party SMTP
 * plugin was invisible to WPMgr. MailFailureCapture closes that gap by
 * listening on wp_mail_failed unconditionally, independent of EmailConfig.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WP_Error;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\MailFailureCapture;

/**
 * @covers \WPMgr\Agent\Email\MailFailureCapture
 */
class MailFailureCaptureTest extends TestCase
{
    /** @var array<string,array<int,callable>> hook name => registered callbacks. */
    private array $registeredActions = [];

    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();

        $this->registeredActions = [];

        // Brain Monkey's built-in add_action()/do_action() are expectation
        // trackers, not a working dispatcher: HookExpectationExecutor::execute()
        // returns null for a hook with no expectation configured, so
        // do_action() alone never invokes what add_action() registered. These
        // aliases build a tiny real pub/sub so do_action() genuinely calls
        // whatever register_hooks() wired -- proving the WIRING, not just
        // calling capture() directly. Same pattern as
        // tests/SelfUpdateInRequestApplyTest.php's add_action alias.
        Functions\when('add_action')->alias(function (string $hook, $callback, int $priority = 10, int $accepted_args = 1): bool {
            $this->registeredActions[$hook][] = $callback;
            return true;
        });
        Functions\when('do_action')->alias(function (string $hook, ...$args): void {
            foreach ($this->registeredActions[$hook] ?? [] as $callback) {
                $callback(...$args);
            }
        });

        Functions\when('current_time')->justReturn('2026-08-17 00:00:00');

        // EmailConfig::load() -> get_option(OPTION); default to an empty
        // array so the decoded config is unconfigured (provider === '').
        Functions\when('get_option')->justReturn([]);
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    /**
     * Minimal fake $wpdb: records every insert() call's $data array so a
     * test can assert row count and shape without a real database. Uses an
     * array (not a single captured row) because some tests need to count
     * multiple potential inserts.
     */
    private function fakeWpdb(): object
    {
        return new class () {
            public string $prefix = 'wp_';

            /** @var array<int,array<string,mixed>> */
            public array $rows = [];

            public int $insert_id = 0;

            /** @param array<string,mixed> $data */
            public function insert(string $table, array $data, array $formats): bool
            {
                $this->rows[] = $data;
                $this->insert_id++;
                return true;
            }
        };
    }

    /**
     * RED/GREEN case: core's own wp_mail_failed fires on an UNCONFIGURED
     * site (no WPMgr provider set) and must produce exactly one 'failed' row.
     * The send body ('message' in core's error data) must never appear in
     * any persisted column, under any key, regardless of store_body.
     */
    public function test_wp_mail_failed_writes_a_failed_row_when_no_provider_is_configured(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        do_action('wp_mail_failed', new WP_Error('wp_mail_failed', 'SMTP connect() failed', [
            'to'      => ['a@x.com'],
            'subject' => 'Order #12',
            'message' => 'SECRET-BODY',
        ]));

        $this->assertCount(1, $fakeWpdb->rows, 'Exactly one failed row must be written for an unrouted core mail_failed.');

        $row = $fakeWpdb->rows[0];
        $this->assertSame('failed', $row['status']);
        $this->assertSame('wp_mail', $row['provider']);
        $this->assertNotSame('', $row['error']);

        // Security-relevant assertion: the raw message body must appear in
        // NO field of the persisted row.
        $this->assertStringNotContainsString('SECRET-BODY', (string) json_encode($row));
        foreach ($row as $column => $value) {
            $this->assertNotSame('SECRET-BODY', $value, "Column '{$column}' must never carry the raw message body.");
        }

        unset($GLOBALS['wpdb']);
    }

    /**
     * No-double-log guard: when MailRouter's own provider send fails, it
     * already (a) logs the row itself via ProviderRouter::send() ->
     * EmailLogger::write(), and (b) fires wp_mail_failed itself (post
     * cb7737c) with error_data carrying a `wpmgr_provider_detail` marker
     * that only ever appears on a WP_Error MailRouter fired -- core's own
     * native wp_mail_failed data never has that key.
     *
     * Scoping: this test only proves that MailFailureCapture::capture(),
     * reached here through the real wp_mail_failed dispatch, adds ZERO rows
     * when that marker is present. It does not exercise
     * ProviderRouter::send()'s own separate EmailLogger::write() call (the
     * "first" log write in "logged once not twice") -- that call happens on
     * a different code path outside this test's scope.
     */
    public function test_router_handled_failure_is_logged_once_not_twice(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        do_action('wp_mail_failed', new WP_Error('wp_mail_failed', 'Provider rejected', [
            'to'                    => ['a@x.com'],
            'subject'               => 'S',
            'headers'               => [],
            'attachments'           => [],
            'wpmgr_provider_detail' => 'Provider rejected',
        ]));

        $this->assertCount(
            0,
            $fakeWpdb->rows,
            'MailFailureCapture must add zero rows when the wpmgr_provider_detail marker shows MailRouter already logged this failure.'
        );

        unset($GLOBALS['wpdb']);
    }

    /**
     * Over-fire control. There is no success-path fire to test directly --
     * core only calls wp_mail_failed on failure, never on a successful send
     * -- so the most meaningful concrete check available here is that
     * capture() is stateless across repeated fires: two INDEPENDENT
     * unconfigured failures must produce two independent rows, not a single
     * corrupted/merged entry, proving the listener doesn't leak state or
     * drop a second in-process failure.
     */
    public function test_two_independent_unconfigured_failures_produce_two_independent_rows(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        do_action('wp_mail_failed', new WP_Error('wp_mail_failed', 'first failure', [
            'to'      => ['first@x.com'],
            'subject' => 'First',
        ]));
        do_action('wp_mail_failed', new WP_Error('wp_mail_failed', 'second failure', [
            'to'      => ['second@x.com'],
            'subject' => 'Second',
        ]));

        $this->assertCount(2, $fakeWpdb->rows);
        $this->assertSame('first failure', $fakeWpdb->rows[0]['error']);
        $this->assertSame('second failure', $fakeWpdb->rows[1]['error']);
        $this->assertStringContainsString('first@x.com', $fakeWpdb->rows[0]['mail_to']);
        $this->assertStringContainsString('second@x.com', $fakeWpdb->rows[1]['mail_to']);

        unset($GLOBALS['wpdb']);
    }
}
