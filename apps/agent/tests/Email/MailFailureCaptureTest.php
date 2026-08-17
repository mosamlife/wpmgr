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
 * Consent (log_emails) governs WHAT MailFailureCapture::capture() writes,
 * never WHETHER it writes: a row always exists for a genuine failure (that
 * is the alert), but the recipient address and subject line are included
 * only when the site has opted into email logging -- otherwise they are
 * redacted to empty. See class-mail-failure-capture.php's docblock for the
 * full reasoning (security-reviewer finding + owner ruling on the initial
 * commit).
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

        // EmailConfig::load() -> get_option(OPTION). Default to an empty
        // array: unconfigured (provider === '') AND log_emails === false --
        // the real default shape on a site that never touched email
        // settings, i.e. this feature's entire target population. Tests
        // that need log_emails=true override this per-test.
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
     * Required test 1 (owner-specified): on an unrouted site with
     * log_emails=false (the real default), a wp_mail_failed fire must still
     * write a row -- status='failed', provider='wp_mail', error non-empty --
     * so the alert exists, but the row must contain NEITHER the recipient
     * address NOR the subject line in any column.
     *
     * FINDING A / A2 (security review): the fixture here used to be
     * 'SMTP connect() failed' -- the one common failure shape that happens
     * to omit the address, which let the assertion below pass for the wrong
     * reason (nothing to leak, not "nothing leaked"). A real PHPMailer/SMTP
     * failure routinely embeds the address verbatim in the message text
     * (this exact shape is what the reviewer used to prove the defect, and
     * matches tests/Email/fake-phpmailer.php:138's
     * 'Invalid address:  (%s): %s'), so this fixture now does too, and the
     * assertion covers the `error` column explicitly, not just the whole
     * serialized row.
     */
    public function test_row_is_written_but_redacted_when_log_emails_is_disabled(): void
    {
        Functions\when('get_option')->justReturn([]); // log_emails=false, provider=''

        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        do_action('wp_mail_failed', new WP_Error(
            'wp_mail_failed',
            'SMTP Error: The following recipients failed: a@x.com: 550 5.1.1 User unknown',
            [
                'to'      => ['a@x.com'],
                'subject' => 'Order #12',
            ]
        ));

        $this->assertCount(1, $fakeWpdb->rows, 'A row must always be written for a genuine failure, even with log_emails off.');

        $row = $fakeWpdb->rows[0];
        $this->assertSame('failed', $row['status']);
        $this->assertSame('wp_mail', $row['provider']);
        $this->assertNotSame('', $row['error']);
        $this->assertStringNotContainsString('a@x.com', $row['error'], 'The address embedded in the error text must not survive when log_emails is off.');
        $this->assertStringContainsString('550 5.1.1 User unknown', $row['error'], 'Redaction must not destroy the diagnostic detail an operator needs -- only the address.');

        $json = (string) json_encode($row);
        $this->assertStringNotContainsString('a@x.com', $json, 'Recipient must not appear anywhere in the row when log_emails is off.');
        $this->assertStringNotContainsString('Order #12', $json, 'Subject must not appear anywhere in the row when log_emails is off.');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Required test 2 (owner-specified): on an unrouted site with
     * log_emails=true, the row DOES carry the real recipient and subject --
     * the opt-in has to still mean something.
     */
    public function test_row_carries_real_recipient_and_subject_when_log_emails_is_enabled(): void
    {
        Functions\when('get_option')->justReturn(['log_emails' => true]);

        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        do_action('wp_mail_failed', new WP_Error(
            'wp_mail_failed',
            'SMTP Error: The following recipients failed: a@x.com: 550 5.1.1 User unknown',
            [
                'to'      => ['a@x.com'],
                'subject' => 'Order #12',
            ]
        ));

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertSame('failed', $row['status']);
        $this->assertSame('wp_mail', $row['provider']);
        $this->assertStringContainsString('a@x.com', $row['mail_to'], 'Opting in to log_emails must actually surface the real recipient.');
        $this->assertSame('Order #12', $row['subject']);
        $this->assertStringContainsString('a@x.com', $row['error'], 'Opting in to log_emails must not redact the error text either -- the opt-in means the real data, unmodified.');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Required test 3 (owner-specified): with store_body=true (in addition
     * to cases 1/2 above), the message body is still excluded from every
     * column, regardless of log_emails or store_body. capture() never sets
     * body_html/body_text on the $mail payload it builds, so
     * EmailLogger::write() has nothing to read into the body column no
     * matter what store_body says.
     *
     * Note on `body_stored`: that column reflects the SITE's store_body
     * setting, not whether any body content was actually captured --
     * EmailLogger::write() sets it from $cfg->store_body unconditionally
     * inside its `if ( $cfg->store_body )` branch (class-email-logger.php:75-76),
     * regardless of whether $mail carries body_html/body_text. So
     * body_stored=1 here is expected and NOT a leak; the actual property
     * under test is that the `body` column itself, and every other column,
     * never contains the raw message text.
     */
    public function test_body_is_never_persisted_when_store_body_is_enabled(): void
    {
        Functions\when('get_option')->justReturn(['log_emails' => true, 'store_body' => true]);

        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        do_action('wp_mail_failed', new WP_Error('wp_mail_failed', 'SMTP connect() failed', [
            'to'      => ['a@x.com'],
            'subject' => 'Order #12',
            'message' => 'SECRET-BODY',
        ]));

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertNotSame('SECRET-BODY', $row['body'], "The 'body' column must never carry the raw message text.");
        $this->assertStringNotContainsString('SECRET-BODY', (string) json_encode($row));

        unset($GLOBALS['wpdb']);
    }

    /**
     * Required test 4 / security-reviewer finding B: a malformed `to`
     * (an array containing a non-string element) must not throw an uncaught
     * Error out of the wp_mail_failed hook. Without validation,
     * EmailLogger::write()'s implode(', ', $to_raw) fatals on a non-string
     * element (e.g. an object with no __toString) -- and since
     * wp_mail_failed fires from inside core's wp_mail() catch block, an
     * uncaught Error here WSODs whatever request triggered the mail send
     * (checkout, password reset, etc). capture() filters non-string
     * elements out before they can reach implode().
     */
    public function test_capture_does_not_fatal_on_non_string_to_element(): void
    {
        Functions\when('get_option')->justReturn(['log_emails' => true]);

        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $capture = new MailFailureCapture(new EmailLogger());
        $capture->register_hooks();

        $malformedRecipient = new class () {
            // Deliberately no __toString(): (string) cast or implode() on
            // this object fatals with an uncaught Error.
        };

        do_action('wp_mail_failed', new WP_Error('wp_mail_failed', 'malformed to element', [
            'to'      => ['good@example.com', $malformedRecipient],
            'subject' => 'S',
        ]));

        $this->assertCount(1, $fakeWpdb->rows, 'A malformed to-element must not prevent the well-formed recipient from being logged.');
        $this->assertStringContainsString('good@example.com', $fakeWpdb->rows[0]['mail_to']);

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
     * a different code path outside this test's scope. The marker check
     * runs before EmailConfig is even loaded, so this holds regardless of
     * log_emails.
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
     * log_emails-enabled failures must produce two independent rows, not a
     * single corrupted/merged entry, proving the listener doesn't leak
     * state or drop a second in-process failure.
     */
    public function test_two_independent_failures_produce_two_independent_rows(): void
    {
        Functions\when('get_option')->justReturn(['log_emails' => true]);

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
