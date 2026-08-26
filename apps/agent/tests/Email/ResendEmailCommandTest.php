<?php
/**
 * ResendEmailCommandTest — verifies the resend_email command contract:
 *
 *   - body_stored=1 → ProviderRouter::send() called, row updated, ok=true
 *   - body_stored=0 → ok=false, detail='body_not_stored', handler never called
 *   - row not found → ok=false, detail='log_row_not_found'
 *   - missing agent_seq → ok=false, detail contains 'agent_seq'
 *   - no email config → ok=false, detail='no email config …'
 *   - provider send fails → ok=false, detail from provider error
 *
 * Uses a FakeResendWpdb (defined at the bottom) to control get_row() / query()
 * without requiring a real database.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Commands\ResendEmailCommand;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\EmailLogger;
use WPMgr\Agent\Email\ProviderHandlerInterface;
use WPMgr\Agent\Email\ProviderRouter;

/**
 * @covers \WPMgr\Agent\Commands\ResendEmailCommand
 */
class ResendEmailCommandTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $optionStore = [];

    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();

        $this->optionStore = [];

        Functions\when('get_option')->alias(
            fn ($k, $d = false) => $this->optionStore[$k] ?? $d
        );
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('get_site_option')->justReturn('__wpmgr_settings_missing__');
        Functions\when('is_multisite')->justReturn(false);
        // sanitize_email is already defined in bootstrap.php; do not re-stub.
    }

    protected function tearDown(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tearDown();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function make_router_with_result(array $result): ProviderRouter
    {
        $handler = $this->createMock(ProviderHandlerInterface::class);
        $handler->method('provider')->willReturn('sendgrid');
        $handler->method('send')->willReturn($result);

        $logger = $this->createMock(EmailLogger::class);
        $logger->method('write')->willReturn(1);

        $keystore = new FakeKeystore('test-secret');
        $router   = new ProviderRouter($keystore, $logger);
        $router->register($handler);
        return $router;
    }

    private function make_log_row(int $id, bool $body_stored, string $body = '', string $message_id = ''): array
    {
        return [
            'id'           => (string) $id,
            'message_id'   => $message_id,
            'mail_to'      => 'recipient@example.com',
            'mail_from'    => 'Sender Name <sender@example.com>',
            'subject'      => 'Original subject',
            'provider'     => 'sendgrid',
            'body_stored'  => $body_stored ? '1' : '0',
            'body'         => $body,
            'resent_count' => '0',
        ];
    }

    /**
     * Build a router around a handler that RECORDS every send it is asked to
     * make, so a test can assert on what was transmitted — or that nothing was.
     */
    private function make_router_with_spy(RecordingProviderHandler $spy): ProviderRouter
    {
        $logger = $this->createMock(EmailLogger::class);
        $logger->method('write')->willReturn(1);

        $router = new ProviderRouter(new FakeKeystore('test-secret'), $logger);
        $router->register($spy);
        return $router;
    }

    private function install_wpdb(?array $row): FakeResendWpdb
    {
        $wpdb            = new FakeResendWpdb($row);
        $GLOBALS['wpdb'] = $wpdb;
        return $wpdb;
    }

    private function install_email_config(): void
    {
        $this->optionStore[EmailConfig::OPTION] = [
            'provider'    => 'sendgrid',
            'from_address' => 'from@example.com',
            'from_name'   => 'WPMgr',
            'log_emails'  => false,
            'store_body'  => false,
        ];
    }

    // -------------------------------------------------------------------------
    // Tests
    // -------------------------------------------------------------------------

    public function test_name_is_correct(): void
    {
        $router = $this->make_router_with_result(['ok' => true, 'message_id' => '', 'error' => '', 'provider_response' => '']);
        $cmd    = new ResendEmailCommand($router);
        $this->assertSame('resend_email', $cmd->name());
    }

    public function test_missing_agent_seq_returns_error(): void
    {
        $router = $this->make_router_with_result(['ok' => true, 'message_id' => '', 'error' => '', 'provider_response' => '']);
        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], []);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('agent_seq', $result['detail']);
    }

    public function test_invalid_agent_seq_zero_returns_error(): void
    {
        $router = $this->make_router_with_result(['ok' => true, 'message_id' => '', 'error' => '', 'provider_response' => '']);
        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], ['agent_seq' => 0]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('agent_seq', $result['detail']);
    }

    public function test_returns_no_email_config_when_unconfigured(): void
    {
        // No email config in option store.
        $this->install_wpdb($this->make_log_row(1, true, '<p>body</p>'));

        $router = $this->make_router_with_result(['ok' => true, 'message_id' => '', 'error' => '', 'provider_response' => '']);
        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], ['agent_seq' => 1]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('no email config', $result['detail']);
    }

    public function test_returns_log_row_not_found_when_row_absent(): void
    {
        $this->install_email_config();
        $this->install_wpdb(null); // get_row() returns null

        $router = $this->make_router_with_result(['ok' => true, 'message_id' => '', 'error' => '', 'provider_response' => '']);
        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], ['agent_seq' => 99]);

        $this->assertFalse($result['ok']);
        $this->assertSame('log_row_not_found', $result['detail']);
    }

    public function test_returns_body_not_stored_when_body_not_captured(): void
    {
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(5, false)); // body_stored=0

        $router = $this->make_router_with_result(['ok' => true, 'message_id' => '', 'error' => '', 'provider_response' => '']);
        // handler must NOT be called — verify via a strict mock expectation.
        $handler = $this->createMock(ProviderHandlerInterface::class);
        $handler->method('provider')->willReturn('sendgrid');
        $handler->expects($this->never())->method('send');

        $logger   = $this->createMock(EmailLogger::class);
        $logger->method('write')->willReturn(1);
        $keystore = new FakeKeystore('secret');
        $router   = new ProviderRouter($keystore, $logger);
        $router->register($handler);

        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], ['agent_seq' => 5]);

        $this->assertFalse($result['ok']);
        $this->assertSame('body_not_stored', $result['detail']);
    }

    public function test_resend_succeeds_when_body_stored(): void
    {
        Functions\when('current_time')->justReturn('2026-06-10 00:00:00');

        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(7, true, '<p>Hello world</p>'));

        $router = $this->make_router_with_result([
            'ok'                => true,
            'message_id'        => 'sg-resent-001',
            'error'             => '',
            'provider_response' => '202',
        ]);

        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], ['agent_seq' => 7]);

        $this->assertTrue($result['ok']);
        $this->assertSame('resent', $result['detail']);
        $this->assertSame('sg-resent-001', $result['message_id']);

        // DB UPDATE must have been called to increment resent_count.
        $this->assertTrue($wpdb->update_called, 'UPDATE query must be executed after a successful resend');
    }

    public function test_resend_does_not_update_row_when_provider_fails(): void
    {
        Functions\when('current_time')->justReturn('2026-06-10 00:00:00');

        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(8, true, 'plain text body'));

        $router = $this->make_router_with_result([
            'ok'                => false,
            'message_id'        => '',
            'error'             => 'API quota exceeded',
            'provider_response' => '429',
        ]);

        $cmd    = new ResendEmailCommand($router);
        $result = $cmd->execute([], ['agent_seq' => 8]);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('quota', $result['detail']);
        $this->assertFalse($wpdb->update_called, 'UPDATE must NOT be called when the provider send failed');
    }

    // -------------------------------------------------------------------------
    // GH #528 — agent_seq is not a safe identity across a database restore.
    //
    // A restore rolls the wpmgr_email_log AUTO_INCREMENT counter back, so later
    // traffic re-uses ids the control plane already bound to other messages. The
    // CP names the message_id it mirrored at ingest; a mismatch means the local
    // row is no longer the message the operator selected, and email cannot be
    // recalled, so the send must not happen at all.
    //
    // These tests do not depend on any process-global constant (the
    // WPMGR_DISABLE_SITE_2FA leak that has silently disabled assertions in other
    // files today), so they need no process isolation: every one of them asserts
    // on the spy's own recorded state.
    // -------------------------------------------------------------------------

    /**
     * The restore scenario itself. Local row 42 is Bob's password reset; the
     * control plane still believes 42 is Alice's invoice. Nothing may be sent.
     */
    public function test_message_id_mismatch_refuses_and_sends_nothing(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();

        // Post-restore: id 42 now holds a completely different message.
        $row             = $this->make_log_row(42, true, '<p>Reset your password, Bob</p>', 'sg-bob-password-reset');
        $row['mail_to']  = 'bob@example.com';
        $row['subject']  = 'Password reset';
        $wpdb            = $this->install_wpdb($row);

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-001', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        // The CP asks for agent_seq 42, the invoice it logged for Alice.
        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        // The load-bearing assertion: no email left the site.
        $this->assertSame(0, $spy->send_count, 'no email may be sent when the named message_id does not match the local row');
        $this->assertSame([], $spy->sent, 'the provider handler must have received nothing at all');

        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_mismatch', $result['detail']);
        $this->assertSame('', $result['message_id']);
        $this->assertFalse($wpdb->update_called, 'a refused resend must not touch the log row');
    }

    /**
     * The refusal detail is a contract string the control plane pins as
     * agentcmd.ResendDetailMessageIDMismatch. It is the WHOLE detail, not a
     * prefix with agent prose appended: raw agent text in front of a user is how
     * GH #520 was reported. Pin the literal on this side too, so a well-meant
     * reword here fails here rather than in a toast.
     */
    public function test_mismatch_detail_is_exactly_the_pinned_contract_literal(): void
    {
        $this->assertSame('message_id_mismatch', ResendEmailCommand::DETAIL_MISMATCH);

        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>body</p>', 'sg-bob-password-reset'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-x', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame('message_id_mismatch', $result['detail'], 'the detail must be the bare contract literal, with nothing appended');
    }

    /**
     * Over-fire guard 1: a legitimate resend, ids matching, still sends.
     */
    public function test_matching_message_id_still_resends(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-002', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame(1, $spy->send_count, 'a verified resend must still send');
        $this->assertTrue($result['ok']);
        $this->assertSame('resent', $result['detail']);
        $this->assertSame('sg-new-002', $result['message_id']);
        $this->assertTrue($wpdb->update_called);
        $this->assertTrue($result['verified'], 'this is the compared-and-matched path, so it attests true');
    }

    /**
     * Over-fire guard 2 — the one that keeps an older control plane working: a
     * command with NO message_id key resends exactly as it does today.
     */
    public function test_absent_message_id_resends_exactly_as_today(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-003', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        // Exactly the pre-#528 payload.
        $result = $cmd->execute([], ['agent_seq' => 42]);

        $this->assertSame(1, $spy->send_count, 'a control plane that sends no message_id must keep working unchanged');
        $this->assertTrue($result['ok']);
        $this->assertSame('resent', $result['detail']);
        $this->assertSame('sg-new-003', $result['message_id']);
        $this->assertTrue($wpdb->update_called);
        $this->assertFalse($result['verified'], 'unchanged behaviour, and honestly reported as unverified');
    }

    /**
     * An explicit JSON null is the encoding of "no value", so it means the same
     * as an absent key: the control plane is not asking for verification.
     * Refusing on it would turn an additive field into a breaking one.
     */
    public function test_null_message_id_is_treated_as_not_supplied(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-004', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => null]);

        $this->assertSame(1, $spy->send_count);
        $this->assertTrue($result['ok']);
    }

    /**
     * The row-has-no-stored-id case, and the decision it forces.
     *
     * The CP's copy of message_id is a mirror of this row's own value at ingest.
     * If the CP holds one and the local row does not, they are not the same row
     * — which is exactly what a rolled-back counter re-issuing an id to a FAILED
     * send (logged with message_id='') looks like. We cannot verify, so we do
     * not send, and we say why.
     */
    public function test_row_without_stored_message_id_refuses_when_cp_names_one(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(42, true, '<p>whatever landed here</p>', ''));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-005', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame(0, $spy->send_count, 'an unverifiable resend must not be sent');
        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_mismatch', $result['detail']);
        $this->assertFalse($wpdb->update_called);
    }

    /**
     * Empty is NOT compared as a value — it means the same as an absent key.
     *
     * The control plane's message_id is NULL for every send that failed at the
     * time (all five provider handlers return '' on failure, and ingest only
     * stores a non-empty pointer), and a failed send is the most common thing an
     * operator resends. The CP omits the key for those rows; treating an empty
     * string the same way means neither side can break the other by changing how
     * it encodes "nothing".
     */
    public function test_empty_message_id_is_treated_as_not_supplied(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>failed original send</p>', ''));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-006', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => '']);

        $this->assertSame(1, $spy->send_count, 'a failed send whose CP-side message_id is NULL must still be resendable');
        $this->assertTrue($result['ok']);
    }

    /**
     * ...and it stays "not supplied" even when the local row does hold an id.
     * An empty value is the absence of a claim, never a claim of absence, so it
     * must not manufacture a refusal.
     */
    public function test_empty_message_id_does_not_refuse_a_row_that_has_one(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-007', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => '']);

        $this->assertSame(1, $spy->send_count);
        $this->assertTrue($result['ok']);
    }

    /**
     * A present-but-unusable message_id is refused before any work is done. An
     * unverifiable resend is never a silent send.
     */
    public function test_non_string_message_id_refuses_and_sends_nothing(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-008', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => ['not', 'a', 'string']]);

        $this->assertSame(0, $spy->send_count);
        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_invalid', $result['detail']);
    }

    /**
     * The verification runs BEFORE the body_stored branch, so an operator whose
     * row was replaced by a restore is told the truth ("this is not your
     * message") rather than a misleading fact about the wrong row.
     */
    public function test_mismatch_is_reported_ahead_of_body_not_stored(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, false, '', 'sg-bob-password-reset'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-009', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame(0, $spy->send_count);
        $this->assertSame('message_id_mismatch', $result['detail']);
    }

    /**
     * REQUIRED over-fire proof: resend the same row twice in a row, and the
     * second one must still send.
     *
     * This is the case the fix is most likely to break. The control plane's copy
     * of message_id is frozen at the ORIGINAL send's value — EmailLogReporter
     * pages rows with `WHERE id > cursor`, so an already-pushed row is never
     * pushed again and the CP never learns a resend's new id. It therefore sends
     * the same original id every time. If a successful resend rewrote the local
     * row's message_id, the second resend would compare the CP's original
     * against the agent's newer value and refuse a completely legitimate action
     * — turning a restore-only defect into a permanent one.
     */
    public function test_repeat_resend_of_the_same_row_still_sends(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-resend-new', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        // The CP sends the same original id both times, because that is the only
        // value it will ever hold for this row.
        $first  = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);
        $second = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);
        $third  = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertTrue($first['ok']);
        $this->assertTrue($second['ok'], 'the second resend of the same row must not refuse itself');
        $this->assertTrue($third['ok'], 'nor the third');
        $this->assertSame(3, $spy->send_count, 'all three resends must actually have been sent');
        $this->assertSame('sg-resend-new', $second['message_id'], 'each resend still returns its own new provider id to the CP');
        $this->assertTrue($first['verified'], 'every one of the three compared and matched');
        $this->assertTrue($second['verified']);
        $this->assertTrue($third['verified']);
    }

    /**
     * The mechanism the test above depends on: the row's message_id survives.
     *
     * EmailLogReporter pages rows with `WHERE id > cursor`, so a row that has
     * already been pushed is never pushed again and the control plane's copy of
     * message_id stays at the original send's value forever. A successful resend
     * therefore must NOT rewrite the row's message_id, or resending the same row
     * twice would refuse itself.
     */
    public function test_successful_resend_preserves_the_rows_message_id(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-010', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $first = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);
        $this->assertTrue($first['ok']);
        $this->assertTrue($wpdb->update_called);

        $updates = array_values(array_filter(
            $wpdb->queries,
            static fn (string $q): bool => stripos($q, 'UPDATE') !== false
        ));
        $this->assertNotEmpty($updates, 'the successful resend must have issued an UPDATE');
        foreach ($updates as $q) {
            $this->assertStringNotContainsString(
                'message_id',
                $q,
                'the row message_id is the identity the CP verifies against and must survive a resend'
            );
        }

        // ...and the row is therefore still resendable with the same id.
        $second = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);
        $this->assertTrue($second['ok'], 'a second resend of the same row must not refuse itself');
        $this->assertSame(2, $spy->send_count);
    }

    // -------------------------------------------------------------------------
    // GH #528, correction 1 — `verified` is THIS AGENT'S attestation, and the
    // control plane must never infer it.
    //
    // The CP cannot learn from its own request whether a check happened. An
    // older agent ignores an unknown message_id, resends happily and answers
    // ok=true; a CP that read "I sent the field" as "the agent compared it"
    // would record a VERIFIED resend and tell the operator and the audit trail
    // that the message was confirmed when no comparison ever ran. That is the
    // exact defect this issue is about, sitting inside its own fix.
    //
    // So the agent states it. `verified` is true on ONE code path: a supplied
    // message_id was compared against the loaded row and matched. The CP reads
    // an ABSENT field as false, which is what makes an older agent's silence
    // mean "unverified" rather than "verified".
    // -------------------------------------------------------------------------

    /**
     * The attestation exists, and it is true exactly where the work was done.
     */
    public function test_verified_is_true_only_when_the_id_was_compared_and_matched(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-011', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertArrayHasKey(
            'verified',
            $result,
            'the agent must state whether it verified; leaving the CP to infer it from its own request is the defect'
        );
        $this->assertTrue($result['verified'], 'supplied, compared, matched — the one case that may attest true');
        $this->assertTrue($result['ok']);
        $this->assertSame(1, $spy->send_count);
    }

    /**
     * The older-control-plane path. No message_id was supplied, so no
     * comparison happened, so nothing may be attested — even though the resend
     * itself succeeds.
     */
    public function test_verified_is_false_when_no_message_id_is_supplied(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-012', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42]);

        $this->assertArrayHasKey('verified', $result);
        $this->assertTrue($result['ok'], 'an unverified resend still sends — that is the additive contract');
        $this->assertFalse($result['verified'], 'a send that skipped the comparison must never be recorded as verified');
    }

    /**
     * null and '' are both "the CP has no id to offer", so both skip the
     * comparison, and a skipped comparison attests false.
     */
    public function test_verified_is_false_when_message_id_is_null_or_empty(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();

        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));
        $nullSpy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-013', 'error' => '', 'provider_response' => '202']);
        $nullCmd = new ResendEmailCommand($this->make_router_with_spy($nullSpy));
        $nullResult = $nullCmd->execute([], ['agent_seq' => 42, 'message_id' => null]);

        $this->assertTrue($nullResult['ok']);
        $this->assertFalse($nullResult['verified'], 'an explicit null supplied nothing to compare');

        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));
        $emptySpy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-014', 'error' => '', 'provider_response' => '202']);
        $emptyCmd = new ResendEmailCommand($this->make_router_with_spy($emptySpy));
        $emptyResult = $emptyCmd->execute([], ['agent_seq' => 42, 'message_id' => '']);

        $this->assertTrue($emptyResult['ok']);
        $this->assertFalse($emptyResult['verified'], 'an empty string supplied nothing to compare');
    }

    /**
     * The other skip named in the contract: the local row carries no stored id.
     * With nothing supplied either, there is nothing to compare on either side,
     * the resend proceeds, and it is reported unverified.
     */
    public function test_verified_is_false_when_the_local_row_has_no_stored_id(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>failed original send</p>', ''));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-015', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42]);

        $this->assertSame(1, $spy->send_count, 'a failed send with no stored id must still be resendable');
        $this->assertTrue($result['ok']);
        $this->assertFalse($result['verified'], 'no stored id means no comparison, which means no attestation');
    }

    /**
     * A refusal is not an attestation. The comparison ran and REFUTED the row,
     * so `verified` is false, not true — stated explicitly rather than left to
     * fall out of the code.
     */
    public function test_verified_is_false_on_the_mismatch_refusal(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Reset your password, Bob</p>', 'sg-bob-password-reset'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-016', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame(0, $spy->send_count);
        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_mismatch', $result['detail']);
        $this->assertFalse($result['verified'], 'a refuted comparison attests false, never true');
    }

    /**
     * Every refusal that returns BEFORE the comparison attests false, because
     * on each of those paths the agent never even loaded a row to compare.
     */
    public function test_verified_is_false_on_every_refusal_before_the_comparison(): void
    {
        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-017', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $missingSeq = $cmd->execute([], []);
        $this->assertFalse($missingSeq['ok']);
        $this->assertFalse($missingSeq['verified'], 'missing agent_seq compared nothing');

        $badSeq = $cmd->execute([], ['agent_seq' => 0]);
        $this->assertFalse($badSeq['ok']);
        $this->assertFalse($badSeq['verified'], 'an invalid agent_seq compared nothing');

        $badType = $cmd->execute([], ['agent_seq' => 42, 'message_id' => ['not', 'a', 'string']]);
        $this->assertFalse($badType['ok']);
        $this->assertSame('message_id_invalid', $badType['detail']);
        $this->assertFalse($badType['verified'], 'an unusable message_id compared nothing');

        // No email config in the option store, and a row that would otherwise send.
        $this->install_wpdb($this->make_log_row(42, true, '<p>body</p>', 'sg-alice-invoice'));
        $noConfig = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);
        $this->assertFalse($noConfig['ok']);
        $this->assertStringContainsString('no email config', $noConfig['detail']);
        $this->assertFalse($noConfig['verified'], 'the config check returns before any row is loaded');

        $this->install_email_config();
        $this->install_wpdb(null);
        $noRow = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);
        $this->assertFalse($noRow['ok']);
        $this->assertSame('log_row_not_found', $noRow['detail']);
        $this->assertFalse($noRow['verified'], 'there was no row to compare against');

        $this->assertSame(0, $spy->send_count, 'none of these paths may send');
    }

    /**
     * The one ok=false path that legitimately attests TRUE, pinned so it is a
     * decision rather than an accident.
     *
     * `verified` describes the identity comparison, not the send: the id was
     * supplied, it was compared against this row, and it matched. That the body
     * was never captured is a separate fact, carried by `ok` and `detail`. The
     * CP records a resend only when ok=true, so this cannot manufacture a
     * verified resend that did not happen.
     */
    public function test_verified_is_true_when_the_match_succeeded_but_the_body_was_not_stored(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, false, '', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-018', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame(0, $spy->send_count);
        $this->assertFalse($result['ok']);
        $this->assertSame('body_not_stored', $result['detail']);
        $this->assertTrue($result['verified'], 'the comparison did run and did match; verified describes that, not the send');
    }

    // -------------------------------------------------------------------------
    // GH #528, correction 2 — provider message ids are OPAQUE. Compare exactly.
    //
    // These identifiers are stored and reported without any general trimming,
    // so trimming only at the comparison invents an equality the rest of the
    // system does not believe in. Two raw ids differing by surrounding
    // whitespace are two different ids, and treating them as one weakens the
    // fail-closed guard in the single direction that matters: it lets a re-used
    // agent_seq select the wrong stored email and send it.
    // -------------------------------------------------------------------------

    /**
     * Whitespace on the SUPPLIED side is a difference, and a difference refuses.
     */
    public function test_supplied_id_padded_with_whitespace_is_a_mismatch(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $wpdb = $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-019', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => "  sg-alice-invoice\t"]);

        $this->assertSame(0, $spy->send_count, 'two raw identifiers that differ must not compare equal');
        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_mismatch', $result['detail']);
        $this->assertFalse($result['verified']);
        $this->assertFalse($wpdb->update_called);
    }

    /**
     * ...and whitespace on the STORED side is equally a difference. Normalising
     * either side alone would still be normalising.
     */
    public function test_stored_id_padded_with_whitespace_is_a_mismatch(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', " sg-alice-invoice "));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-020', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => 'sg-alice-invoice']);

        $this->assertSame(0, $spy->send_count);
        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_mismatch', $result['detail']);
        $this->assertFalse($result['verified']);
    }

    /**
     * A whitespace-only supplied id is a VALUE, not an absence.
     *
     * Trimming before the empty test silently reclassified "   " as "the CP has
     * no id to offer" and sent the mail unverified. Without the trim it is a
     * supplied id like any other, it is compared, it does not match, and the
     * send is refused. Fail closed.
     */
    public function test_whitespace_only_supplied_id_is_compared_not_discarded(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', 'sg-alice-invoice'));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-021', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => '   ']);

        $this->assertSame(0, $spy->send_count, 'a whitespace-only id must be compared, not silently treated as absent');
        $this->assertFalse($result['ok']);
        $this->assertSame('message_id_mismatch', $result['detail']);
        $this->assertFalse($result['verified']);
    }

    /**
     * The over-fire guard for exact comparison: an id that genuinely carries
     * whitespace still resends when BOTH sides carry it identically. The rule
     * is "compare exactly", not "refuse anything with whitespace in it".
     */
    public function test_identically_padded_ids_on_both_sides_still_resend(): void
    {
        Functions\when('current_time')->justReturn('2026-08-26 00:00:00');
        $this->install_email_config();
        $this->install_wpdb($this->make_log_row(42, true, '<p>Invoice for Alice</p>', " sg-alice-invoice "));

        $spy = new RecordingProviderHandler(['ok' => true, 'message_id' => 'sg-new-022', 'error' => '', 'provider_response' => '202']);
        $cmd = new ResendEmailCommand($this->make_router_with_spy($spy));

        $result = $cmd->execute([], ['agent_seq' => 42, 'message_id' => " sg-alice-invoice "]);

        $this->assertSame(1, $spy->send_count, 'byte-identical ids match, whatever bytes they are');
        $this->assertTrue($result['ok']);
        $this->assertTrue($result['verified']);
    }
}

/**
 * A provider handler that records every send it is asked to perform, so a test
 * can assert that nothing was sent rather than merely that an error came back.
 */
class RecordingProviderHandler implements ProviderHandlerInterface
{
    public int $send_count = 0;

    /** @var array<int,array<string,mixed>> Every mail payload handed to send(). */
    public array $sent = [];

    /** @var array{ok:bool,message_id:string,error:string,provider_response:string} */
    private array $result;

    /**
     * @param array{ok:bool,message_id:string,error:string,provider_response:string} $result
     */
    public function __construct(array $result)
    {
        $this->result = $result;
    }

    /**
     * @param array<string,mixed> $mail
     * @param array<string,mixed> $config
     * @return array{ok:bool,message_id:string,error:string,provider_response:string}
     */
    public function send(array $mail, array $config, string $secret): array
    {
        ++$this->send_count;
        $this->sent[] = $mail;
        return $this->result;
    }

    public function provider(): string
    {
        return 'sendgrid';
    }
}

// ---------------------------------------------------------------------------
// Lightweight wpdb double for ResendEmailCommand tests.
// Simulates get_row() and query() (UPDATE) without a real database.
// ---------------------------------------------------------------------------

/**
 * Minimal wpdb double for ResendEmailCommand tests.
 */
class FakeResendWpdb
{
    public string $prefix = 'wp_';

    public bool $update_called = false;

    /** @var array<int,string> Every query text passed to query(). */
    public array $queries = [];

    /** @var array<string,mixed>|null */
    private ?array $row;

    public function __construct(?array $row)
    {
        $this->row = $row;
    }

    /**
     * Substitute placeholders the way wpdb::prepare() does, so query() below can
     * see the VALUES a statement carries and not just its shape. Without this
     * the double cannot model an UPDATE at all, and a test asserting that a
     * resend leaves the row's message_id alone would pass no matter what the
     * command wrote.
     */
    public function prepare(string $query, ...$args): string
    {
        foreach ($args as $arg) {
            $replacement = is_int($arg) ? (string) $arg : "'" . (string) $arg . "'";
            $query       = preg_replace('/%[ds]/', $replacement, $query, 1) ?? $query;
        }
        return $query;
    }

    /**
     * @return array<string,mixed>|null
     */
    public function get_row(string $query, string $output = 'ARRAY_A'): ?array
    {
        return $this->row;
    }

    /**
     * Simulate an UPDATE query (resent_count increment).
     *
     * @return int|false
     */
    public function query(string $query)
    {
        $this->queries[] = $query;
        if (stripos($query, 'UPDATE') !== false) {
            $this->update_called = true;
            // Model the write, so a subsequent get_row() sees what the command
            // actually stored. This is what lets the repeat-resend test go red
            // against an implementation that overwrites message_id on resend.
            if ($this->row !== null && preg_match("/message_id\s*=\s*'([^']*)'/i", $query, $m) === 1) {
                $this->row['message_id'] = $m[1];
            }
            return 1;
        }
        return false;
    }
}
