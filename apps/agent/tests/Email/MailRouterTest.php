<?php
/**
 * MailRouterTest: first coverage of MailRouter::build_mail_payload(), the
 * method that reads wp_mail()'s raw $headers argument and is the ROOT of
 * GH #312: it stored a Reply-To/Cc/Bcc header's raw value verbatim, so
 * "Andrea Somigli <salesianalibri@gmail.com>" reached the provider handler
 * as one opaque string instead of a parseable address.
 *
 * This test pins the payload SHAPE MailRouter still produces (raw string
 * entries; see interface-provider-handler.php for why parsing happens at
 * the handler, not here) while proving the exact reporter header round-trips
 * through AddressParser correctly, and that a multi-address header (the
 * second, independent bug: "Cc: a@x.com, b@y.com" stored as ONE string) is
 * still carried faithfully so the handler-side parser can split it.
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\AddressParser;
use WPMgr\Agent\Email\EmailConfig;
use WPMgr\Agent\Email\MailRouter;
use WPMgr\Agent\Email\ProviderRouter;
use WPMgr\Agent\Settings;

/**
 * @covers \WPMgr\Agent\Email\MailRouter
 */
class MailRouterTest extends TestCase
{
    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();

        // No wp_mail_from / wp_mail_from_name filters registered; the second
        // argument (the computed default) passes straight through.
        Functions\when('apply_filters')->returnArg(2);
        Functions\when('home_url')->justReturn('https://example.com/');
        Functions\when('get_option')->justReturn('');
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    private function router(): MailRouter
    {
        $providerRouter = $this->createMock(ProviderRouter::class);
        $settings       = new Settings();

        return new MailRouter($providerRouter, $settings);
    }

    // -------------------------------------------------------------------------
    // GH #312: the reporter's exact header, verbatim
    // -------------------------------------------------------------------------

    public function test_reporter_exact_reply_to_header_is_carried_through_and_parses_cleanly(): void
    {
        $cfg  = new EmailConfig(['provider' => 'smtp']);
        $atts = [
            'to'      => 'shop-owner@example.com',
            'subject' => 'New order',
            'message' => 'Order details',
            'headers' => "Content-Type: text/plain\r\nReply-To: Andrea Somigli <salesianalibri@gmail.com>",
        ];

        $mail = $this->router()->build_mail_payload($atts, $cfg);

        $this->assertSame(['Andrea Somigli <salesianalibri@gmail.com>'], $mail['reply_to']);

        // The payload keeps the raw entry (see interface docblock); confirm it
        // is still exactly what AddressParser needs to recover the address+name.
        $parsed = AddressParser::parse_one($mail['reply_to'][0]);
        $this->assertNotNull($parsed);
        $this->assertSame('salesianalibri@gmail.com', $parsed['address']);
        $this->assertSame('Andrea Somigli', $parsed['name']);
    }

    // -------------------------------------------------------------------------
    // cc / bcc: same defect class as reply-to, not just reply-to
    // -------------------------------------------------------------------------

    public function test_cc_and_bcc_headers_with_display_names_are_captured(): void
    {
        $cfg  = new EmailConfig(['provider' => 'smtp']);
        $atts = [
            'to'      => 'to@example.com',
            'subject' => 'Subject',
            'message' => 'Body',
            'headers' => [
                'Cc: Carla Cee <carla@example.com>',
                'Bcc: Bob Bee <bob@example.com>',
            ],
        ];

        $mail = $this->router()->build_mail_payload($atts, $cfg);

        $this->assertSame(['Carla Cee <carla@example.com>'], $mail['cc']);
        $this->assertSame(['Bob Bee <bob@example.com>'], $mail['bcc']);
    }

    // -------------------------------------------------------------------------
    // Second, independent bug: a header with MULTIPLE addresses is stored as
    // one array entry (nothing splits it here); confirm and pin that shape,
    // since AddressParser::parse_list() at the handler is what splits it.
    // -------------------------------------------------------------------------

    public function test_multi_address_cc_header_is_stored_as_a_single_unsplit_entry(): void
    {
        $cfg  = new EmailConfig(['provider' => 'smtp']);
        $atts = [
            'to'      => 'to@example.com',
            'subject' => 'Subject',
            'message' => 'Body',
            'headers' => 'Cc: a@x.com, b@y.com',
        ];

        $mail = $this->router()->build_mail_payload($atts, $cfg);

        $this->assertSame(['a@x.com, b@y.com'], $mail['cc'], 'MailRouter does not split a multi-address header value; the handler does');

        // Confirm the handler-side parser recovers both addresses from it.
        $parsed = AddressParser::parse_list($mail['cc']);
        $this->assertCount(2, $parsed);
        $this->assertSame('a@x.com', $parsed[0]['address']);
        $this->assertSame('b@y.com', $parsed[1]['address']);
    }

    // -------------------------------------------------------------------------
    // `to`: quote-aware split (upgraded from a naive explode(','))
    // -------------------------------------------------------------------------

    public function test_to_string_with_quoted_comma_name_splits_correctly(): void
    {
        $cfg  = new EmailConfig(['provider' => 'smtp']);
        $atts = [
            'to'      => '"Rossi, Andrea" <a@x.com>, b@y.com',
            'subject' => 'Subject',
            'message' => 'Body',
        ];

        $mail = $this->router()->build_mail_payload($atts, $cfg);

        $this->assertSame(['"Rossi, Andrea" <a@x.com>', 'b@y.com'], $mail['to']);
    }

    public function test_to_array_is_passed_through_unchanged(): void
    {
        $cfg  = new EmailConfig(['provider' => 'smtp']);
        $atts = [
            'to'      => ['a@x.com', 'b@y.com'],
            'subject' => 'Subject',
            'message' => 'Body',
        ];

        $mail = $this->router()->build_mail_payload($atts, $cfg);

        $this->assertSame(['a@x.com', 'b@y.com'], $mail['to']);
    }

    // -------------------------------------------------------------------------
    // Empty / absent headers never throw and produce empty lists
    // -------------------------------------------------------------------------

    public function test_no_headers_produces_empty_cc_bcc_reply_to(): void
    {
        $cfg  = new EmailConfig(['provider' => 'smtp']);
        $atts = [
            'to'      => 'to@example.com',
            'subject' => 'Subject',
            'message' => 'Body',
        ];

        $mail = $this->router()->build_mail_payload($atts, $cfg);

        $this->assertSame([], $mail['cc']);
        $this->assertSame([], $mail['bcc']);
        $this->assertSame([], $mail['reply_to']);
    }
}
