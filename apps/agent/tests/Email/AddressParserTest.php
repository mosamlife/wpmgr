<?php
/**
 * AddressParserTest: pins the GH #312 fix, parsing bare addresses, the RFC
 * 5322 "Display Name <addr>" form, a quoted display name that itself
 * contains a comma, mixed comma-separated lists, and malformed/empty input
 * (which must never throw and must never fatal).
 *
 * @package WPMgr\Agent\Tests\Email
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\AddressParser;

/**
 * @covers \WPMgr\Agent\Email\AddressParser
 */
class AddressParserTest extends TestCase
{
    // -------------------------------------------------------------------------
    // parse_one(): the reporter's exact case (GH #312)
    // -------------------------------------------------------------------------

    /**
     * The literal string from the bug report:
     *   {"summary": "Invalid address:  (Reply-To): Andrea Somigli <salesianalibri@gmail.com>"}
     * must parse to a bare address with the display name preserved, not dropped.
     */
    public function test_reporter_exact_reply_to_string_parses_with_name_preserved(): void
    {
        $parsed = AddressParser::parse_one('Andrea Somigli <salesianalibri@gmail.com>');

        $this->assertNotNull($parsed, 'the reporter\'s exact Reply-To value must parse, not be rejected');
        $this->assertSame('salesianalibri@gmail.com', $parsed['address']);
        $this->assertSame('Andrea Somigli', $parsed['name']);
    }

    // -------------------------------------------------------------------------
    // parse_one(): bare address
    // -------------------------------------------------------------------------

    public function test_bare_address_parses_with_empty_name(): void
    {
        $parsed = AddressParser::parse_one('plain@example.com');

        $this->assertNotNull($parsed);
        $this->assertSame('plain@example.com', $parsed['address']);
        $this->assertSame('', $parsed['name']);
    }

    public function test_bare_address_with_surrounding_whitespace_is_trimmed(): void
    {
        $parsed = AddressParser::parse_one("  plain@example.com  \t");

        $this->assertNotNull($parsed);
        $this->assertSame('plain@example.com', $parsed['address']);
    }

    // -------------------------------------------------------------------------
    // parse_one(): quoted display name containing a comma
    // -------------------------------------------------------------------------

    public function test_quoted_display_name_with_comma_is_preserved(): void
    {
        $parsed = AddressParser::parse_one('"Rossi, Andrea" <a@x.com>');

        $this->assertNotNull($parsed);
        $this->assertSame('a@x.com', $parsed['address']);
        $this->assertSame('Rossi, Andrea', $parsed['name'], 'the comma inside the quoted name must survive, not be treated as a list separator');
    }

    public function test_quoted_display_name_with_escaped_quote_is_unescaped(): void
    {
        $parsed = AddressParser::parse_one('"Jane \\"JD\\" Doe" <jane@example.com>');

        $this->assertNotNull($parsed);
        $this->assertSame('jane@example.com', $parsed['address']);
        $this->assertSame('Jane "JD" Doe', $parsed['name']);
    }

    // -------------------------------------------------------------------------
    // parse_one(): never throws; malformed input degrades to null
    // -------------------------------------------------------------------------

    public function test_empty_string_returns_null_without_throwing(): void
    {
        $this->assertNull(AddressParser::parse_one(''));
    }

    public function test_whitespace_only_string_returns_null(): void
    {
        $this->assertNull(AddressParser::parse_one('   '));
    }

    public function test_garbage_text_returns_null_without_throwing(): void
    {
        $this->assertNull(AddressParser::parse_one('this is not an email address'));
    }

    public function test_angle_brackets_with_invalid_address_return_null(): void
    {
        $this->assertNull(AddressParser::parse_one('Some Name <not-an-email>'));
    }

    public function test_empty_angle_brackets_return_null(): void
    {
        $this->assertNull(AddressParser::parse_one('Empty Name <>'));
    }

    public function test_unterminated_angle_bracket_returns_null_not_a_crash(): void
    {
        $this->assertNull(AddressParser::parse_one('Broken <a@x.com'));
    }

    // -------------------------------------------------------------------------
    // split_list(): comma-separated lists, quote/angle-bracket aware
    // -------------------------------------------------------------------------

    public function test_split_list_on_plain_bare_addresses(): void
    {
        $this->assertSame(
            ['a@example.com', 'b@example.com'],
            AddressParser::split_list('a@example.com, b@example.com')
        );
    }

    public function test_split_list_does_not_split_inside_quoted_name(): void
    {
        $this->assertSame(
            ['"Rossi, Andrea" <a@x.com>', 'b@y.com'],
            AddressParser::split_list('"Rossi, Andrea" <a@x.com>, b@y.com')
        );
    }

    public function test_split_list_mixed_bare_named_and_quoted_comma(): void
    {
        $this->assertSame(
            ['"Rossi, Andrea" <a@x.com>', 'b@y.com', 'Charlie <c@z.com>'],
            AddressParser::split_list('"Rossi, Andrea" <a@x.com>, b@y.com, Charlie <c@z.com>')
        );
    }

    public function test_split_list_empty_string_returns_empty_array(): void
    {
        $this->assertSame([], AddressParser::split_list(''));
    }

    public function test_split_list_semicolon_separated(): void
    {
        $this->assertSame(
            ['a@example.com', 'b@example.com'],
            AddressParser::split_list('a@example.com; b@example.com')
        );
    }

    // -------------------------------------------------------------------------
    // parse_list() / parse_list_verbose(): the shape every handler consumes
    // -------------------------------------------------------------------------

    public function test_parse_list_accepts_a_single_multi_address_string(): void
    {
        // The exact shape MailRouter stores for a "Cc: a@x.com, b@y.com" header:
        // ONE array entry whose value itself packs two addresses.
        $parsed = AddressParser::parse_list(['a@x.com, b@y.com']);

        $this->assertCount(2, $parsed);
        $this->assertSame('a@x.com', $parsed[0]['address']);
        $this->assertSame('b@y.com', $parsed[1]['address']);
    }

    public function test_parse_list_accepts_array_of_separate_header_entries(): void
    {
        $parsed = AddressParser::parse_list(['Andrea Somigli <salesianalibri@gmail.com>', 'plain@example.com']);

        $this->assertCount(2, $parsed);
        $this->assertSame('salesianalibri@gmail.com', $parsed[0]['address']);
        $this->assertSame('Andrea Somigli', $parsed[0]['name']);
        $this->assertSame('plain@example.com', $parsed[1]['address']);
        $this->assertSame('', $parsed[1]['name']);
    }

    public function test_parse_list_drops_invalid_entries_without_throwing(): void
    {
        $parsed = AddressParser::parse_list(['good@example.com', 'not-an-email', '']);

        $this->assertCount(1, $parsed);
        $this->assertSame('good@example.com', $parsed[0]['address']);
    }

    public function test_parse_list_empty_array_returns_empty_array(): void
    {
        $this->assertSame([], AddressParser::parse_list([]));
    }

    public function test_parse_list_verbose_reports_invalid_entries_separately(): void
    {
        $result = AddressParser::parse_list_verbose(['good@example.com', 'not-an-email']);

        $this->assertCount(1, $result['valid']);
        $this->assertSame('good@example.com', $result['valid'][0]['address']);
        $this->assertSame(['not-an-email'], $result['invalid']);
    }

    public function test_parse_list_verbose_never_throws_on_completely_malformed_input(): void
    {
        $result = AddressParser::parse_list_verbose(['', '   ', '<>', 'garbage text with spaces']);

        $this->assertSame([], $result['valid']);
    }

    // -------------------------------------------------------------------------
    // format() / format_list(): the inverse, used when re-serialising to a
    // raw header value.
    // -------------------------------------------------------------------------

    public function test_format_bare_address_has_no_angle_brackets(): void
    {
        $this->assertSame('a@x.com', AddressParser::format(['address' => 'a@x.com', 'name' => '']));
    }

    public function test_format_with_name_produces_rfc5322_form(): void
    {
        $this->assertSame(
            'Andrea Somigli <salesianalibri@gmail.com>',
            AddressParser::format(['address' => 'salesianalibri@gmail.com', 'name' => 'Andrea Somigli'])
        );
    }

    public function test_format_quotes_a_name_containing_a_comma(): void
    {
        $this->assertSame(
            '"Rossi, Andrea" <a@x.com>',
            AddressParser::format(['address' => 'a@x.com', 'name' => 'Rossi, Andrea'])
        );
    }

    public function test_format_strips_crlf_from_name_and_address(): void
    {
        $formatted = AddressParser::format(['address' => "a@x.com\r\nBcc: evil@x.com", 'name' => "Evil\r\nName"]);

        $this->assertStringNotContainsString("\r", $formatted);
        $this->assertStringNotContainsString("\n", $formatted);
    }

    public function test_format_list_joins_multiple_entries(): void
    {
        $formatted = AddressParser::format_list([
            ['address' => 'a@x.com', 'name' => 'Alice'],
            ['address' => 'b@y.com', 'name' => ''],
        ]);

        $this->assertSame('Alice <a@x.com>, b@y.com', $formatted);
    }

    // -------------------------------------------------------------------------
    // Round trip: parse -> format reproduces an equivalent, re-parseable value.
    // -------------------------------------------------------------------------

    public function test_round_trip_quoted_comma_name(): void
    {
        $parsed    = AddressParser::parse_one('"Rossi, Andrea" <a@x.com>');
        $formatted = AddressParser::format($parsed);
        $reparsed  = AddressParser::parse_one($formatted);

        $this->assertSame($parsed['address'], $reparsed['address']);
        $this->assertSame($parsed['name'], $reparsed['name']);
    }

    // -------------------------------------------------------------------------
    // GH #312 review: a display name must never be able to redirect the message.
    //
    // The angle-addr pattern is greedy, so it binds to the LAST bracketed token.
    // Header values are routinely built by interpolating a user supplied name
    // into "{name} <{email}>", so a submitted name carrying its own angle-addr
    // would otherwise resolve to the attacker's address while displaying the
    // legitimate one. Before this parser existed the transport rejected such an
    // entry outright, and refusing it here preserves that.
    // -------------------------------------------------------------------------

    public function test_a_second_angle_addr_in_the_display_name_is_refused(): void
    {
        $this->assertNull(AddressParser::parse_one('Bob <bob@example.com> <evil@attacker.com>'));
    }

    public function test_a_smuggled_angle_addr_never_wins_over_the_real_one(): void
    {
        // The whole point: it must not silently resolve to evil@attacker.com.
        $parsed = AddressParser::parse_one('Bob <bob@example.com> <evil@attacker.com>');
        if ($parsed !== null) {
            $this->assertNotSame('evil@attacker.com', $parsed['address']);
        } else {
            $this->assertNull($parsed);
        }
    }

    public function test_an_angle_bracket_inside_a_quoted_display_name_is_still_allowed(): void
    {
        // Quoted, therefore inert, and legal RFC 5322. It must keep working.
        $parsed = AddressParser::parse_one('"Bob <not-real>" <bob@x.com>');

        $this->assertNotNull($parsed);
        $this->assertSame('bob@x.com', $parsed['address']);
        $this->assertSame('Bob <not-real>', $parsed['name']);
    }

    // -------------------------------------------------------------------------
    // GH #312 review: our validator must never be stricter than the transport.
    //
    // FILTER_VALIDATE_EMAIL rejects an internationalised domain, while PHPMailer
    // detects an 8-bit domain BEFORE it validates and punycodes it on send. The
    // parser is allowed to refuse these, but only because every caller passes a
    // refused entry through raw so the transport stays the judge. This test
    // documents the refusal so the passthrough is never removed as redundant.
    // -------------------------------------------------------------------------

    public function test_an_internationalised_domain_is_refused_and_must_be_passed_through_raw(): void
    {
        $this->assertNull(AddressParser::parse_one('kunde@exämple.de'));
    }

    public function test_the_reporters_exact_case_parses(): void
    {
        $parsed = AddressParser::parse_one('Andrea Somigli <salesianalibri@gmail.com>');

        $this->assertNotNull($parsed);
        $this->assertSame('salesianalibri@gmail.com', $parsed['address']);
        $this->assertSame('Andrea Somigli', $parsed['name']);
    }
}
