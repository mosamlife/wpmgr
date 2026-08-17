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

    // -------------------------------------------------------------------------
    // GH #381 phase 2 (security review, second round): the bare regex in
    // redact_email_addresses() only recognises a well-formed ASCII address,
    // but PHPMailer's own "Invalid address" errors fire BECAUSE the address
    // failed that exact validation -- so the regex is weakest on precisely
    // the inputs this code path emits. The remedy is two-stage: remove every
    // literal known recipient (and its obvious encoded variants) first, then
    // run the regex as a net for anything not already known. This table is
    // the reviewer's full leak catalogue plus the forms that already worked;
    // every row must come out address-free once a matching known address is
    // supplied, exactly as the two real call sites now supply it.
    // -------------------------------------------------------------------------

    /** @return array<string,array{0:string,1:string,2:string}> label => [known address, text, needle that must not survive] */
    public static function redaction_table(): array
    {
        return [
            'raw IDN domain' => ['kunde@münchen.de', 'SMTP Error: recipients failed: kunde@münchen.de: 550 5.1.1 User unknown', 'kunde@münchen.de'],
            'quoted local part' => ['"john doe"@example.com', 'Invalid address: (to): "john doe"@example.com', '"john doe"@example.com'],
            'IP-literal domain' => ['admin@[192.168.13.7]', 'Invalid address: (to): admin@[192.168.13.7]', 'admin@[192.168.13.7]'],
            'bare-IP domain' => ['admin@203.0.113.9', 'Invalid address: (to): admin@203.0.113.9', 'admin@203.0.113.9'],
            'spaced' => ['alice@example.com', 'Invalid address: (to): alice @ example.com', 'alice @ example.com'],
            'percent-encoded bare' => ['alice@example.com', 'Invalid address: (to): alice%40example.com', 'alice%40example.com'],
            'percent-encoded in URL' => ['alice@example.com', 'see https://relay.example/bounce?addr=alice%40example.com', 'alice%40example.com'],
            'split across line break' => ['alice@example.com', "Invalid address: (to): alice@\nexample.com", "alice@\nexample.com"],
            'base64' => ['alice@example.com', 'raw recipient: YWxpY2VAZXhhbXBsZS5jb20=', 'YWxpY2VAZXhhbXBsZS5jb20='],
            '1-char TLD' => ['x@y.c', 'Invalid address: (to): x@y.c', 'x@y.c'],
            'numeric TLD' => ['admin@example.12345', 'Invalid address: (to): admin@example.12345', 'admin@example.12345'],
            'non-ASCII local part' => ['jösef@example.com', 'Invalid address: (to): jösef@example.com', 'jösef@example.com'],
            // Forms that already redacted correctly before this fix -- must stay fixed.
            'punycode' => ['user@xn--mnchen-3ya.de', 'Invalid address: (to): user@xn--mnchen-3ya.de', 'user@xn--mnchen-3ya.de'],
            'angle-bracket' => ['a@x.com', 'Invalid address:  (to): Name <a@x.com>', 'a@x.com'],
            'uppercase' => ['A@X.COM', 'Invalid address: (to): A@X.COM', 'A@X.COM'],
            'trailing dot' => ['a@x.com.', 'Invalid address: (to): a@x.com.', 'a@x.com.'],
        ];
    }

    /**
     * @dataProvider redaction_table
     */
    public function test_redact_email_addresses_removes_every_leaking_shape_given_the_known_address(string $known, string $text, string $needle): void
    {
        $redacted = AddressParser::redact_email_addresses($text, [$known]);

        $this->assertStringNotContainsString($needle, $redacted, "'{$needle}' must not survive redaction when it is a known recipient.");
    }

    /**
     * The reviewer's own end-to-end proof case, pinned permanently and
     * independent of the data provider above.
     */
    public function test_redact_email_addresses_removes_the_reviewers_idn_proof_case(): void
    {
        $text = 'SMTP Error: The following recipients failed: kunde@münchen.de: 550 5.1.1 User unknown';

        $redacted = AddressParser::redact_email_addresses($text, ['kunde@münchen.de']);

        $this->assertStringNotContainsString('kunde@münchen.de', $redacted);
        $this->assertStringContainsString('550 5.1.1 User unknown', $redacted, 'Diagnostic detail must survive redaction.');
    }

    /**
     * Over-fire control: diagnostic detail that is NOT address-shaped must
     * come through completely unchanged, known-address list or not.
     */
    public function test_redact_email_addresses_preserves_diagnostic_detail(): void
    {
        $cases = [
            '550 5.1.1 User unknown',
            'connect to mail.example.com:587 failed after 3 tries',
            'Postfix 3.7.4 error 4.4.1 timeout',
        ];

        foreach ($cases as $text) {
            $this->assertSame($text, AddressParser::redact_email_addresses($text, ['unrelated@example.com']));
        }
    }

    /**
     * No catastrophic backtracking: a 40,000-char pathological input must
     * redact in a small fraction of a second, matching the reviewer's own
     * 0.0001s measurement on the same shape of input.
     */
    public function test_redact_email_addresses_has_no_catastrophic_backtracking(): void
    {
        $pathological = str_repeat('a', 40000) . '@' . str_repeat('b.', 20000) . 'com';

        $start = microtime(true);
        AddressParser::redact_email_addresses($pathological, ['known@example.com']);
        $elapsed = microtime(true) - $start;

        $this->assertLessThan(1.0, $elapsed, "Redaction took {$elapsed}s on a 40,000-char pathological input -- possible catastrophic backtracking.");
    }
}
