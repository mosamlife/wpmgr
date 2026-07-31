<?php
/**
 * SmtpHandlerTest: first coverage of SmtpHandler::send(), the exact call
 * site that produced the GH #312 report:
 *
 *   {"summary": "Invalid address:  (Reply-To): Andrea Somigli <salesianalibri@gmail.com>"}
 *
 * Uses the self-contained \PHPMailer\PHPMailer\PHPMailer double declared in
 * fake-phpmailer.php (see that file's docblock for why it lives separately).
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use PHPMailer\PHPMailer\FakePhpMailerRegistry;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\Handlers\SmtpHandler;

require_once __DIR__ . '/fake-phpmailer.php';

/**
 * @covers \WPMgr\Agent\Email\Handlers\SmtpHandler
 */
class SmtpHandlerTest extends TestCase
{
    private function base_mail(): array
    {
        return [
            'to'          => ['recipient@example.com'],
            'cc'          => [],
            'bcc'         => [],
            'reply_to'    => [],
            'from'        => 'sender@example.com',
            'from_name'   => 'WPMgr Test',
            'subject'     => 'Hello',
            'body_text'   => 'Plain body',
            'body_html'   => '',
            'charset'     => 'UTF-8',
            'headers'     => [],
            'attachments' => [],
            'return_path' => false,
            'x_site_id'   => 'site-abc',
            'x_tenant_id' => 'tenant-abc',
        ];
    }

    private function config(): array
    {
        return ['host' => 'smtp.example.com', 'port' => 587, 'auth' => false];
    }

    // -------------------------------------------------------------------------
    // GH #312: the reporter's exact case
    // -------------------------------------------------------------------------

    public function test_reporter_exact_reply_to_is_delivered_with_name_and_does_not_fail_the_send(): void
    {
        $mail = $this->base_mail();
        $mail['reply_to'] = ['Andrea Somigli <salesianalibri@gmail.com>'];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok'], 'the send must succeed, not fail the whole message over one Reply-To');
        $this->assertStringNotContainsString('Invalid address', $result['error']);

        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertNotNull($phpmailer);
        $this->assertSame(
            [['address' => 'salesianalibri@gmail.com', 'name' => 'Andrea Somigli']],
            $phpmailer->recordedReplyTo,
            'the display name must be preserved, not discarded'
        );
    }

    // -------------------------------------------------------------------------
    // to / cc / bcc: same defect class as reply_to
    // -------------------------------------------------------------------------

    public function test_to_cc_bcc_display_names_are_all_preserved(): void
    {
        $mail = $this->base_mail();
        $mail['to']  = ['Terry To <terry@example.com>'];
        $mail['cc']  = ['Carla Cee <carla@example.com>'];
        $mail['bcc'] = ['Bob Bee <bob@example.com>'];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok']);

        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertSame([['address' => 'terry@example.com', 'name' => 'Terry To']], $phpmailer->recordedTo);
        $this->assertSame([['address' => 'carla@example.com', 'name' => 'Carla Cee']], $phpmailer->recordedCc);
        $this->assertSame([['address' => 'bob@example.com', 'name' => 'Bob Bee']], $phpmailer->recordedBcc);
    }

    // -------------------------------------------------------------------------
    // Quoted display name containing a comma
    // -------------------------------------------------------------------------

    public function test_quoted_comma_display_name_is_parsed_correctly(): void
    {
        $mail = $this->base_mail();
        $mail['to'] = ['"Rossi, Andrea" <a@x.com>'];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok']);
        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertSame([['address' => 'a@x.com', 'name' => 'Rossi, Andrea']], $phpmailer->recordedTo);
    }

    // -------------------------------------------------------------------------
    // A plain bare address is unaffected
    // -------------------------------------------------------------------------

    public function test_plain_bare_address_is_unaffected(): void
    {
        $mail = $this->base_mail();
        $mail['to'] = ['plain@example.com'];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok']);
        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertSame([['address' => 'plain@example.com', 'name' => '']], $phpmailer->recordedTo);
    }

    // -------------------------------------------------------------------------
    // A single header carrying a comma-separated list of addresses
    // -------------------------------------------------------------------------

    public function test_single_header_value_with_multiple_addresses_is_split(): void
    {
        $mail = $this->base_mail();
        $mail['cc'] = ['a@x.com, b@y.com'];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok']);
        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertSame(
            [
                ['address' => 'a@x.com', 'name' => ''],
                ['address' => 'b@y.com', 'name' => ''],
            ],
            $phpmailer->recordedCc
        );
    }

    // -------------------------------------------------------------------------
    // Malformed input never fatals the send: one bad address is dropped,
    // the rest of the message still goes out.
    // -------------------------------------------------------------------------

    public function test_malformed_reply_to_is_dropped_not_fatal(): void
    {
        $mail = $this->base_mail();
        $mail['reply_to'] = ['not-an-email', 'good@example.com'];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok'], 'a bad Reply-To must not fail an otherwise-valid send');
        $this->assertStringContainsString('not-an-email', $result['provider_response'], 'the skipped address must be reported');

        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertSame([['address' => 'good@example.com', 'name' => '']], $phpmailer->recordedReplyTo);
    }

    public function test_empty_reply_to_is_fine(): void
    {
        $mail = $this->base_mail();
        $mail['reply_to'] = [];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok']);
        $this->assertSame([], FakePhpMailerRegistry::$last->recordedReplyTo);
    }

    public function test_all_recipients_invalid_fails_cleanly_without_throwing(): void
    {
        $mail = $this->base_mail();
        $mail['to'] = ['garbage garbage', ''];

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('no valid recipient', $result['error']);
    }

    // -------------------------------------------------------------------------
    // From: same category of bug (a display-name From must not throw either)
    // -------------------------------------------------------------------------

    public function test_from_with_display_name_does_not_throw_and_is_split(): void
    {
        $mail = $this->base_mail();
        $mail['from']      = 'Shop Name <shop@example.com>';
        $mail['from_name'] = '';

        $handler = new SmtpHandler();
        $result  = $handler->send($mail, $this->config(), '');

        $this->assertTrue($result['ok']);
        $phpmailer = FakePhpMailerRegistry::$last;
        $this->assertSame(['address' => 'shop@example.com', 'name' => 'Shop Name'], $phpmailer->recordedFrom);
    }
}
