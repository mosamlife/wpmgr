<?php
/**
 * Tests for SendgridHandler: endpoint, auth header, success code, payload shape,
 * and error mapping.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Email;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Email\Handlers\SendgridHandler;

/**
 * @covers \WPMgr\Agent\Email\Handlers\SendgridHandler
 */
class SendgridHandlerTest extends TestCase
{
    protected function setUp(): void
    {
        parent::setUp();
        Monkey\setUp();
    }

    protected function tearDown(): void
    {
        Monkey\tearDown();
        parent::tearDown();
    }

    private function base_mail(): array
    {
        return [
            'to'          => ['recipient@example.com'],
            'cc'          => [],
            'bcc'         => [],
            'reply_to'    => [],
            'from'        => 'sender@example.com',
            'from_name'   => 'WPMgr Test',
            'subject'     => 'Hello from WPMgr',
            'body_text'   => 'Plain text body.',
            'body_html'   => '<p>HTML body.</p>',
            'charset'     => 'UTF-8',
            'headers'     => [],
            'attachments' => [],
            'return_path' => false,
            'x_site_id'   => 'site-abc',
        ];
    }

    public function test_send_posts_to_correct_endpoint_with_bearer_token(): void
    {
        $captured_url     = null;
        $captured_args    = null;
        $captured_headers = null;

        Functions\when('wp_json_encode')->alias('json_encode');

        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured_url, &$captured_args, &$captured_headers) {
                $captured_url     = $url;
                $captured_args    = $args;
                $captured_headers = $args['headers'] ?? [];
                return [
                    'response' => ['code' => 202, 'message' => 'Accepted'],
                    'body'     => '',
                    'headers'  => ['x-message-id' => 'sg-msg-001'],
                ];
            }
        );

        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->alias(
            fn($r) => $r['response']['code'] ?? 0
        );
        Functions\when('wp_remote_retrieve_body')->alias(
            fn($r) => $r['body'] ?? ''
        );
        Functions\when('wp_remote_retrieve_header')->alias(
            fn($r, $key) => $r['headers'][$key] ?? ''
        );

        $handler = new SendgridHandler();
        $result  = $handler->send($this->base_mail(), [], 'SG.test-api-key');

        $this->assertSame('https://api.sendgrid.com/v3/mail/send', $captured_url);
        $this->assertStringStartsWith('Bearer ', $captured_headers['Authorization'] ?? '');
        $this->assertStringContainsString('SG.test-api-key', $captured_headers['Authorization'] ?? '');
        $this->assertSame('application/json', $captured_headers['Content-Type'] ?? '');
        $this->assertTrue($result['ok']);
        $this->assertSame('sg-msg-001', $result['message_id']);
    }

    public function test_send_returns_ok_false_on_non_202(): void
    {
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->justReturn([
            'response' => ['code' => 401, 'message' => 'Unauthorized'],
            'body'     => '{"errors":[{"message":"The provided authorization grant is invalid"}]}',
            'headers'  => [],
        ]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(401);
        Functions\when('wp_remote_retrieve_body')->justReturn(
            '{"errors":[{"message":"The provided authorization grant is invalid"}]}'
        );

        $handler = new SendgridHandler();
        $result  = $handler->send($this->base_mail(), [], 'bad-key');

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('401', $result['error']);
        $this->assertStringContainsString('authorization grant', $result['error']);
    }

    public function test_send_returns_ok_false_when_secret_empty(): void
    {
        // wp_remote_post should NOT be called.
        Functions\expect('wp_remote_post')->never();

        $handler = new SendgridHandler();
        $result  = $handler->send($this->base_mail(), [], '');

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('API key', $result['error']);
    }

    public function test_send_returns_ok_false_on_wp_error(): void
    {
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->justReturn(null);
        Functions\when('is_wp_error')->justReturn(true);

        $wp_error = $this->createMock(\WP_Error::class);
        $wp_error->method('get_error_message')->willReturn('cURL error: could not resolve host');

        Functions\when('wp_remote_post')->justReturn($wp_error);
        Functions\when('is_wp_error')->alias(fn($v) => $v instanceof \WP_Error);

        $handler = new SendgridHandler();
        $result  = $handler->send($this->base_mail(), [], 'SG.valid');

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('cURL', $result['error']);
    }

    public function test_payload_includes_html_and_text_content(): void
    {
        $captured_body = null;

        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured_body) {
                $captured_body = json_decode($args['body'], true);
                return [
                    'response' => ['code' => 202],
                    'body'     => '',
                    'headers'  => [],
                ];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $handler->send($this->base_mail(), [], 'SG.key');

        $this->assertIsArray($captured_body);
        $content_types = array_column($captured_body['content'] ?? [], 'type');
        $this->assertContains('text/plain', $content_types);
        $this->assertContains('text/html', $content_types);
    }

    public function test_payload_includes_x_site_header(): void
    {
        $captured_body = null;

        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured_body) {
                $captured_body = json_decode($args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => []];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $handler->send($this->base_mail(), [], 'SG.key');

        $this->assertSame('site-abc', $captured_body['headers']['X-WPMgr-Site'] ?? null);
    }

    // -------------------------------------------------------------------------
    // GH #312: a "Name <email>" reply_to/to/cc/bcc must become a bare `email`
    // field with the name carried in the sibling `name` field, not rejected.
    // -------------------------------------------------------------------------

    public function test_reporter_exact_reply_to_string_becomes_bare_email_with_name(): void
    {
        $mail = $this->base_mail();
        $mail['reply_to'] = ['Andrea Somigli <salesianalibri@gmail.com>'];

        $captured = null;
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured) {
                $captured = json_decode((string) $args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => ['x-message-id' => 'sg-1']];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('sg-1');

        $handler = new SendgridHandler();
        $result  = $handler->send($mail, [], 'SG.key');

        $this->assertTrue($result['ok'], 'a display-name Reply-To must not fail the send');
        $this->assertSame(
            ['email' => 'salesianalibri@gmail.com', 'name' => 'Andrea Somigli'],
            $captured['reply_to'] ?? null
        );
    }

    public function test_to_cc_bcc_with_display_names_become_email_name_objects(): void
    {
        $mail = $this->base_mail();
        $mail['to']  = ['Terry To <terry@example.com>'];
        $mail['cc']  = ['Carla Cee <carla@example.com>'];
        $mail['bcc'] = ['Bob Bee <bob@example.com>'];

        $captured = null;
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured) {
                $captured = json_decode((string) $args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => []];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $result  = $handler->send($mail, [], 'SG.key');

        $this->assertTrue($result['ok']);
        $personalisation = $captured['personalizations'][0];
        $this->assertSame([['email' => 'terry@example.com', 'name' => 'Terry To']], $personalisation['to']);
        $this->assertSame([['email' => 'carla@example.com', 'name' => 'Carla Cee']], $personalisation['cc']);
        $this->assertSame([['email' => 'bob@example.com', 'name' => 'Bob Bee']], $personalisation['bcc']);
    }

    public function test_quoted_comma_display_name_is_parsed_correctly(): void
    {
        $mail = $this->base_mail();
        $mail['to'] = ['"Rossi, Andrea" <a@x.com>'];

        $captured = null;
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured) {
                $captured = json_decode((string) $args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => []];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $result  = $handler->send($mail, [], 'SG.key');

        $this->assertTrue($result['ok']);
        $this->assertSame(
            [['email' => 'a@x.com', 'name' => 'Rossi, Andrea']],
            $captured['personalizations'][0]['to']
        );
    }

    public function test_multiple_reply_to_addresses_use_reply_to_list(): void
    {
        $mail = $this->base_mail();
        $mail['reply_to'] = ['a@x.com', 'b@y.com'];

        $captured = null;
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured) {
                $captured = json_decode((string) $args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => []];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $result  = $handler->send($mail, [], 'SG.key');

        $this->assertTrue($result['ok']);
        $this->assertArrayNotHasKey('reply_to', $captured);
        $this->assertSame(
            [['email' => 'a@x.com'], ['email' => 'b@y.com']],
            $captured['reply_to_list'] ?? null
        );
    }

    public function test_malformed_cc_entry_is_dropped_not_fatal(): void
    {
        $mail = $this->base_mail();
        $mail['cc'] = ['not-an-email', 'good@example.com'];

        $captured = null;
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured) {
                $captured = json_decode((string) $args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => []];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $result  = $handler->send($mail, [], 'SG.key');

        $this->assertTrue($result['ok'], 'one malformed Cc entry must not fail the whole send');
        $this->assertSame(
            [['email' => 'good@example.com']],
            $captured['personalizations'][0]['cc']
        );
    }

    public function test_empty_reply_to_omits_reply_to_fields(): void
    {
        $mail = $this->base_mail();
        $mail['reply_to'] = [];

        $captured = null;
        Functions\when('wp_json_encode')->alias('json_encode');
        Functions\when('wp_remote_post')->alias(
            function (string $url, array $args) use (&$captured) {
                $captured = json_decode((string) $args['body'], true);
                return ['response' => ['code' => 202], 'body' => '', 'headers' => []];
            }
        );
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(202);
        Functions\when('wp_remote_retrieve_body')->justReturn('');
        Functions\when('wp_remote_retrieve_header')->justReturn('');

        $handler = new SendgridHandler();
        $result  = $handler->send($mail, [], 'SG.key');

        $this->assertTrue($result['ok']);
        $this->assertArrayNotHasKey('reply_to', $captured);
        $this->assertArrayNotHasKey('reply_to_list', $captured);
    }
}
