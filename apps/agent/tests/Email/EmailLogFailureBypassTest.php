<?php
/**
 * EmailLogFailureBypassTest — GH #381 phase 4: ProviderRouter::maybe_log()
 * discarded a DETECTED failure whenever the site owner had turned log_emails
 * off, silently defeating phase 1's failure alerting for that tenant.
 *
 * log_emails is a volume-and-retention preference about keeping a record of
 * mail that WORKED. A failure is an incident, not a record of routine
 * traffic; someone asking for a quieter log did not ask to be blinded to
 * failures. store_body is an independent gate (EmailLogger::write()) that
 * controls whether ANY row -- success or failure -- carries the message
 * body, and it is untouched by this change: turning log_emails off must
 * still mean no bodies are ever stored, and must still mean a SUCCESSFUL
 * send is not recorded at all.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\ProviderHandlerInterface;
use WPMgr\Agent\Email\ProviderRouter;
use WPMgr\Agent\Email\SuppressionCheckerInterface;

/**
 * @covers \WPMgr\Agent\Email\ProviderRouter
 */
class EmailLogFailureBypassTest extends TestCase
{
    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();
        Functions\when('current_time')->justReturn('2026-08-17 00:00:00');
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    /** @param array{ok:bool,message_id:string,error:string,provider_response:string} $result */
    private function make_handler(string $provider, array $result): ProviderHandlerInterface
    {
        $handler = $this->createMock(ProviderHandlerInterface::class);
        $handler->method('provider')->willReturn($provider);
        $handler->method('send')->willReturn($result);
        return $handler;
    }

    /** @return array<string,mixed> */
    private function base_mail(): array
    {
        return [
            'to'          => ['to@example.com'],
            'cc'          => [],
            'bcc'         => [],
            'reply_to'    => [],
            'from'        => 'a@example.com',
            'from_name'   => 'Sender',
            'subject'     => 'Test',
            'body_text'   => 'SECRET-BODY-TEXT',
            'body_html'   => '',
            'charset'     => 'UTF-8',
            'headers'     => [],
            'attachments' => [],
            'return_path' => false,
            'x_site_id'   => 'site-123',
        ];
    }

    /**
     * Minimal fake $wpdb: records every insert() call's $data array so a
     * test can assert row count and shape without a real database. Same
     * pattern as MailFailureCaptureTest::fakeWpdb().
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
     * RED today: maybe_log() returns early on `! $cfg->log_emails` before
     * ever looking at the outcome, so a DETECTED failure (ok:false, an error
     * string already computed by send()) produces zero rows -- nothing for
     * the push to ship, nothing for the control plane to alert on.
     */
    public function test_failed_send_is_logged_even_when_log_emails_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'smtp connect() failed: Connection refused',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $result = $router->send($this->base_mail(), $cfg, true);

        $this->assertFalse($result['ok']);
        $this->assertCount(1, $fakeWpdb->rows, 'a detected failure must be logged even when log_emails is off');
        $this->assertSame('failed', $fakeWpdb->rows[0]['status']);
        $this->assertSame('smtp connect() failed: Connection refused', $fakeWpdb->rows[0]['error']);

        unset($GLOBALS['wpdb']);
    }

    /**
     * The important control: this must be GREEN both BEFORE and AFTER the
     * fix. It guards the bypass from quietly becoming "ignore the
     * preference entirely" -- a successful send is exactly the record a
     * user who turned log_emails off asked NOT to keep.
     */
    public function test_successful_send_is_still_not_logged_when_log_emails_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => true,
            'message_id'        => 'smtp-ok-1',
            'error'             => '',
            'provider_response' => '250 OK',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $result = $router->send($this->base_mail(), $cfg, true);

        $this->assertTrue($result['ok']);
        $this->assertCount(0, $fakeWpdb->rows, 'a successful send must not be recorded when log_emails is off');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Privacy gate: store_body stays independently false regardless of the
     * newly-widened failure path. Prove the raw message body never reaches
     * any column of the failure row this fix now writes.
     */
    public function test_failed_row_carries_no_message_body_when_store_body_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'smtp connect() failed: Connection refused',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $router->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertSame(0, $row['body_stored'], 'body_stored must be 0 when store_body is false, even on the newly-widened failure path');
        $this->assertNull($row['body'], 'body column must be NULL when store_body is false');
        $this->assertStringNotContainsString('SECRET-BODY-TEXT', (string) json_encode($row));
        foreach ($row as $column => $value) {
            $this->assertNotSame('SECRET-BODY-TEXT', $value, "Column '{$column}' must never carry the raw message body when store_body is false.");
        }

        unset($GLOBALS['wpdb']);
    }

    /**
     * Over-fire control: with log_emails TRUE, behaviour is completely
     * unchanged for both success and failure -- this path was never gated
     * by the outcome before, and must not become gated by it now.
     */
    public function test_log_emails_true_behaviour_is_unchanged_for_success_and_failure(): void
    {
        $cfg = new EmailConfig(['provider' => 'smtp', 'log_emails' => true, 'store_body' => false]);

        // Success leg.
        $fakeWpdbOk        = $this->fakeWpdb();
        $GLOBALS['wpdb']   = $fakeWpdbOk;
        $okHandler         = $this->make_handler('smtp', [
            'ok'                => true,
            'message_id'        => 'smtp-ok-2',
            'error'             => '',
            'provider_response' => '250 OK',
        ]);
        $okRouter = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $okRouter->register($okHandler);
        $okRouter->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdbOk->rows, 'a successful send must still be logged when log_emails is on');
        $this->assertSame('sent', $fakeWpdbOk->rows[0]['status']);
        unset($GLOBALS['wpdb']);

        // Failure leg.
        $fakeWpdbFail      = $this->fakeWpdb();
        $GLOBALS['wpdb']   = $fakeWpdbFail;
        $failHandler       = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'boom',
            'provider_response' => '',
        ]);
        $failRouter = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $failRouter->register($failHandler);
        $failRouter->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdbFail->rows, 'a failed send must still be logged when log_emails is on');
        $this->assertSame('failed', $fakeWpdbFail->rows[0]['status']);
        unset($GLOBALS['wpdb']);
    }

    /**
     * Security-reviewer finding B (GH #381 phase 4): maybe_log()'s forced
     * write for a detected failure carried the real `to`/`subject`
     * regardless of log_emails, and the free-text `error` from a real
     * provider routinely embeds the recipient address too (matching
     * MailFailureCaptureTest's phase-1 fixture and the reviewer's proof
     * case). RED before the fix: mail_to/subject were never redacted on
     * this path at all, and `error` was written verbatim.
     *
     * Also proves the redaction line: the address-shaped substring is
     * removed from `error`, but the diagnostic detail ("550 5.1.1 ...
     * rejected") survives -- a blanket empty error string would be a
     * regression in its own right (see class-provider-router.php's
     * maybe_log() docblock).
     */
    public function test_failed_row_redacts_recipient_subject_and_embedded_address_in_error_when_log_emails_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'SMTP Error: The following recipients failed: to@example.com: 550 5.1.1 User unknown',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $result = $router->send($this->base_mail(), $cfg, true);

        $this->assertFalse($result['ok']);
        $this->assertCount(1, $fakeWpdb->rows);

        $row = $fakeWpdb->rows[0];
        $this->assertSame('', $row['mail_to'], 'Recipient must be redacted when log_emails is off.');
        $this->assertSame('', $row['subject'], 'Subject must be redacted when log_emails is off.');
        $this->assertStringNotContainsString('to@example.com', $row['error'], 'The address embedded in the error text must not survive when log_emails is off.');
        $this->assertStringContainsString('550 5.1.1 User unknown', $row['error'], 'Redaction must not destroy the diagnostic detail an operator needs -- only the address.');

        $json = (string) json_encode($row);
        $this->assertStringNotContainsString('to@example.com', $json, 'Recipient must not appear anywhere in the row when log_emails is off.');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Over-fire control for finding B: with log_emails=true, the forced
     * write on failure carries the REAL recipient, subject, and the
     * unredacted error text (including any embedded address) -- the opt-in
     * still has to mean something, on this path exactly as on phase 1's.
     */
    public function test_failed_row_carries_real_recipient_subject_and_error_when_log_emails_is_true(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => true, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'SMTP Error: The following recipients failed: to@example.com: 550 5.1.1 User unknown',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $router->send($this->base_mail(), $cfg, true);

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertStringContainsString('to@example.com', $row['mail_to'], 'Opting in to log_emails must actually surface the real recipient.');
        $this->assertSame('Test', $row['subject']);
        $this->assertStringContainsString('to@example.com', $row['error'], 'Opting in to log_emails must not redact the error text either.');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Security review, GH #381 phase 2 (second round): the reviewer's exact
     * end-to-end proof, on the routed (phase 4) path. kunde@münchen.de is a
     * raw IDN domain that AddressParser::parse_one() refuses to parse, so
     * the old ASCII-only regex left it verbatim in `error`. maybe_log() must
     * now pass the real `to` to redact_email_addresses() so it is removed as
     * a literal string. Pinned permanently.
     */
    public function test_failed_row_redacts_the_reviewers_idn_proof_case(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'SMTP Error: The following recipients failed: kunde@münchen.de: 550 5.1.1 User unknown',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail            = $this->base_mail();
        $mail['to']      = ['kunde@münchen.de'];
        $result          = $router->send($mail, $cfg, true);

        $this->assertFalse($result['ok']);
        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertStringNotContainsString('kunde@münchen.de', $row['error'], 'The IDN address embedded in the error text must not survive when log_emails is off.');
        $this->assertStringContainsString('550 5.1.1 User unknown', $row['error'], 'Redaction must not destroy the diagnostic detail an operator needs -- only the address.');

        $json = (string) json_encode($row);
        $this->assertStringNotContainsString('kunde@münchen.de', $json, 'Recipient must not appear anywhere in the row when log_emails is off.');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Data-provider over the full GH #381 phase-2 leak catalogue on the
     * phase-4 routed path -- same catalogue as
     * MailFailureCaptureTest::addressLeakTable(), proven independently here
     * because maybe_log() reads the recipient off `$mail['to']`, not the
     * WP_Error `to` key phase 1 uses.
     *
     * @dataProvider addressLeakTable
     */
    public function test_failed_row_redacts_every_leaking_address_shape(string $address, string $errorTemplate): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg          = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $errorMessage = sprintf($errorTemplate, $address);
        $handler      = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => $errorMessage,
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail       = $this->base_mail();
        $mail['to'] = [$address];
        $router->send($mail, $cfg, true);

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertStringNotContainsString($address, $row['error'], "'{$address}' must not survive redaction.");

        unset($GLOBALS['wpdb']);
    }

    /** @return array<string,array{0:string,1:string}> label => [address, error sprintf template with one %s] */
    public static function addressLeakTable(): array
    {
        return [
            'raw IDN domain' => ['kunde@münchen.de', 'Invalid address: (to): %s'],
            'quoted local part' => ['"john doe"@example.com', 'Invalid address: (to): %s'],
            'IP-literal domain' => ['admin@[192.168.13.7]', 'Invalid address: (to): %s'],
            'bare-IP domain' => ['admin@203.0.113.9', 'Invalid address: (to): %s'],
            '1-char TLD' => ['x@y.c', 'Invalid address: (to): %s'],
            'numeric TLD' => ['admin@example.12345', 'Invalid address: (to): %s'],
            'non-ASCII local part' => ['jösef@example.com', 'Invalid address: (to): %s'],
        ];
    }

    /**
     * Ruling on the 'suppressed' status (security-reviewer finding B,
     * open question): maybe_log()'s failure gate used to be
     * `'sent' !== $status`, so a routine suppression-list hit was force-
     * logged with the real recipient too, exactly like a genuine failure.
     *
     * A suppression hit is not an incident -- WPMgr already knows the
     * address is bad; there is nothing new here for the control-plane
     * alert to act on, unlike a freshly DETECTED SMTP/provider failure.
     * So 'suppressed' now follows the ordinary log_emails gate, the same
     * as 'sent': no forced write, and so nothing to redact. When log_emails
     * is on, a suppressed send is still logged (unchanged; see
     * ProviderRouterSuppressionTest::test_suppressed_send_writes_log_row_with_suppressed_status).
     */
    public function test_suppressed_send_is_not_logged_when_log_emails_is_false(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);

        $cache = $this->createMock(SuppressionCheckerInterface::class);
        $cache->method('is_suppressed')->willReturn(true);

        $handler = $this->make_handler('smtp', [
            'ok' => true, 'message_id' => 'unused', 'error' => '', 'provider_response' => '',
        ]);
        $handler->expects($this->never())->method('send');

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger(), $cache);
        $router->register($handler);

        $result = $router->send($this->base_mail(), $cfg, true);

        $this->assertFalse($result['ok']);
        $this->assertSame('recipient suppressed', $result['detail']);
        $this->assertCount(0, $fakeWpdb->rows, 'A routine suppression-list hit is not an incident and must not force a row when log_emails is off.');

        unset($GLOBALS['wpdb']);
    }

    /**
     * Round 4 (three review bots + two security reviews, each finding a
     * DIFFERENT leaking field on this same forced-write path): Cc, Bcc,
     * Reply-To, mail_from, the body (when store_body is true), and
     * attachment names. Rather than pin four more named-column assertions
     * (a blocklist test has the exact same blind spot as the blocklist code
     * it is proving), this iterates every key of the ACTUAL inserted row
     * against a small allowlist of columns known to carry no personal data.
     * Anything else must be empty -- so a fifth field added to the mail
     * payload or to EmailLogger::write() tomorrow, without anyone touching
     * this test, still fails it instead of shipping.
     */
    private const NON_PERSONAL_COLUMNS = [
        'message_id', 'provider', 'status', 'response', 'error',
        'retries', 'resent_count', 'connection_key', 'created_at',
    ];

    public function test_forced_row_excludes_every_field_outside_the_allowlist(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        // store_body TRUE on purpose: proves the allowlist wins even when
        // the site's own preference would otherwise permit a body column.
        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => true]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'SMTP Error: The following recipients failed: cc@example.com, bcc@example.com: 550 5.1.1 User unknown',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail                 = $this->base_mail();
        $mail['cc']           = ['cc@example.com'];
        $mail['bcc']          = ['bcc@example.com'];
        $mail['reply_to']     = ['reply@example.com'];
        $mail['attachments']  = [['name' => 'invoice.pdf', 'size_bytes' => 100]];
        $router->send($mail, $cfg, true);

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];

        foreach ($row as $column => $value) {
            if (in_array($column, self::NON_PERSONAL_COLUMNS, true)) {
                continue;
            }
            $this->assertTrue(
                $value === '' || $value === null || $value === 0,
                "Column '{$column}' must be empty on the forced (log_emails=false) path; got: " . var_export($value, true)
            );
        }
    }

    /**
     * RED today: a rejected Cc address survives in `error` because
     * maybe_log() only ever built its redaction dictionary from `to`. Uses a
     * malformed-but-real (bare-IP domain) address, matching
     * addressLeakTable() above, because a plain ASCII address is already
     * caught by AddressParser::redact_email_addresses()'s unconditional
     * generic regex pass -- that pass is what would mask this exact gap if
     * the test used an ordinary address instead.
     */
    public function test_rejected_cc_address_is_redacted_from_error(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'no valid recipient address (rejected: admin@203.0.113.9)',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail       = $this->base_mail();
        $mail['cc'] = ['admin@203.0.113.9'];
        $router->send($mail, $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertStringNotContainsString('admin@203.0.113.9', $row['error'], 'A rejected Cc address must not survive in the error text.');
        $this->assertStringNotContainsString('admin@203.0.113.9', (string) json_encode($row));
    }

    /** RED today: a rejected Bcc address survives in `error` because maybe_log() only ever built its redaction dictionary from `to`. Same malformed-address rationale as the Cc case above. */
    public function test_rejected_bcc_address_is_redacted_from_error(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'no valid recipient address (rejected: admin@[192.168.13.7])',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail        = $this->base_mail();
        $mail['bcc'] = ['admin@[192.168.13.7]'];
        $router->send($mail, $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertStringNotContainsString('admin@[192.168.13.7]', $row['error'], 'A rejected Bcc address must not survive in the error text.');
        $this->assertStringNotContainsString('admin@[192.168.13.7]', (string) json_encode($row));
    }

    /** RED today: a rejected Reply-To address survives in `error` because maybe_log() only ever built its redaction dictionary from `to`. Same malformed-address rationale as the Cc case above. */
    public function test_rejected_reply_to_address_is_redacted_from_error(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'no valid recipient address (rejected: "john doe"@example.com)',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail             = $this->base_mail();
        $mail['reply_to'] = ['"john doe"@example.com'];
        $router->send($mail, $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertStringNotContainsString('"john doe"@example.com', $row['error'], 'A rejected Reply-To address must not survive in the error text.');
        $this->assertStringNotContainsString('"john doe"@example.com', (string) json_encode($row));
    }

    /** RED today: mail_from column carries the real sender address regardless of log_emails. */
    public function test_sender_mail_from_is_not_persisted(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok' => false, 'message_id' => '', 'error' => 'boom', 'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail         = $this->base_mail();
        $mail['from'] = 'sender-victim@example.com';
        $router->send($mail, $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertSame('', $row['mail_from'], 'mail_from must be empty on the forced path.');
        $this->assertStringNotContainsString('sender-victim@example.com', (string) json_encode($row));
    }

    /** RED today: store_body=true persists the real message body into a row the owner never asked for. */
    public function test_body_is_excluded_when_store_body_is_true(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => true]);
        $handler = $this->make_handler('smtp', [
            'ok' => false, 'message_id' => '', 'error' => 'boom', 'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $router->send($this->base_mail(), $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertSame(0, $row['body_stored'], 'body_stored must be forced to 0 on the forced path, regardless of store_body.');
        $this->assertNull($row['body'], 'body must be NULL on the forced path, regardless of store_body.');
        $this->assertStringNotContainsString('SECRET-BODY-TEXT', (string) json_encode($row));
    }

    /** RED today: attachment names are persisted regardless of log_emails. */
    public function test_attachment_name_is_not_persisted(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok' => false, 'message_id' => '', 'error' => 'boom', 'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail                = $this->base_mail();
        $mail['attachments'] = [['name' => 'confidential-invoice.pdf', 'size_bytes' => 4096]];
        $router->send($mail, $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertNull($row['attachments'], 'attachments must be NULL on the forced path.');
        $this->assertStringNotContainsString('confidential-invoice.pdf', (string) json_encode($row));
    }

    /**
     * Diagnostics survive: redaction removes only the address-shaped
     * substring, never the SMTP/provider detail an operator needs to act on.
     *
     * @dataProvider diagnosticSurvivesTable
     */
    public function test_diagnostic_detail_survives_redaction(string $errorMessage, string $mustSurvive): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => false, 'store_body' => false]);
        $handler = $this->make_handler('smtp', [
            'ok' => false, 'message_id' => '', 'error' => $errorMessage, 'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $router->send($this->base_mail(), $cfg, true);

        $row = $fakeWpdb->rows[0];
        $this->assertStringContainsString($mustSurvive, $row['error']);
    }

    /** @return array<string,array{0:string,1:string}> label => [error message, substring that must survive] */
    public static function diagnosticSurvivesTable(): array
    {
        return [
            'user unknown' => [
                'SMTP Error: The following recipients failed: to@example.com: 550 5.1.1 User unknown',
                '550 5.1.1 User unknown',
            ],
            'connect timeout, no address at all' => [
                'connect to mail.example.com:587 failed after 3 tries',
                'connect to mail.example.com:587 failed after 3 tries',
            ],
        ];
    }

    /**
     * Over-fire control, round 4: with log_emails TRUE, Cc, Bcc, Reply-To,
     * mail_from, the body and the attachment name are ALL present and
     * unchanged -- the allowlist applies only to the forced path; the
     * opt-in path this fix leaves untouched still means real data.
     */
    public function test_log_emails_true_still_includes_every_field_the_allowlist_would_strip(): void
    {
        $fakeWpdb        = $this->fakeWpdb();
        $GLOBALS['wpdb'] = $fakeWpdb;

        $cfg     = new EmailConfig(['provider' => 'smtp', 'log_emails' => true, 'store_body' => true]);
        $handler = $this->make_handler('smtp', [
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'SMTP Error: The following recipients failed: cc@example.com, bcc@example.com: 550 5.1.1 User unknown',
            'provider_response' => '',
        ]);

        $router = new ProviderRouter(new FakeKeystore(), new EmailLogger());
        $router->register($handler);

        $mail                = $this->base_mail();
        $mail['cc']          = ['cc@example.com'];
        $mail['bcc']         = ['bcc@example.com'];
        $mail['reply_to']    = ['reply@example.com'];
        $mail['attachments'] = [['name' => 'invoice.pdf', 'size_bytes' => 100]];
        $router->send($mail, $cfg, true);

        $this->assertCount(1, $fakeWpdb->rows);
        $row = $fakeWpdb->rows[0];
        $this->assertStringContainsString('cc@example.com', $row['error']);
        $this->assertSame('Sender <a@example.com>', $row['mail_from']);
        $this->assertSame(1, $row['body_stored']);
        $this->assertSame('SECRET-BODY-TEXT', $row['body']);
        $this->assertNotNull($row['attachments']);
        $this->assertStringContainsString('invoice.pdf', (string) $row['attachments']);
    }
}
