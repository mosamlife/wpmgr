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
use WP_Error;
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

    /**
     * Build a MailRouter whose ProviderRouter::send() is stubbed to return a
     * fixed result, and whose EmailConfig::load() sees a configured provider
     * (so intercept() does not bail out on the "not configured" early return).
     *
     * @param array{ok:bool,message_id:string,detail:string} $sendResult
     */
    private function routerWithSendResult(array $sendResult): MailRouter
    {
        Functions\when('get_option')->justReturn(['provider' => 'smtp']);

        $providerRouter = $this->createMock(ProviderRouter::class);
        $providerRouter->method('send')->willReturn($sendResult);
        $settings = new Settings();

        return new MailRouter($providerRouter, $settings);
    }

    /** @return array<string,mixed> */
    private function baseAtts(): array
    {
        return [
            'to'      => 'someone@example.com',
            'subject' => 'Subject',
            'message' => 'Body',
            'headers' => '',
        ];
    }

    // -------------------------------------------------------------------------
    // GH #439: wp_mail() must report the truth, not a hard-coded `true`.
    //
    // pre_wp_mail short-circuits wp_mail() and its return value BECOMES
    // wp_mail()'s return value verbatim (wp-includes/pluggable.php:
    // `if ( null !== $pre_wp_mail ) { return $pre_wp_mail; }`). Only `null`
    // lets WP fall through to its own PHPMailer path; any other value --
    // including `false` -- still short-circuits. So `false` is both an
    // honest failure signal AND still prevents WP from re-attempting the
    // send through its own PHPMailer.
    // -------------------------------------------------------------------------

    public function test_failed_send_returns_false_not_true(): void
    {
        Functions\when('do_action')->justReturn(null);

        $router = $this->routerWithSendResult([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'smtp connect() failed: Connection refused',
        ]);

        $result = $router->intercept(null, $this->baseAtts());

        $this->assertSame(false, $result, 'A failed send must make wp_mail() return false, not a truthy placeholder');
    }

    public function test_failed_send_return_value_still_satisfies_cores_own_short_circuit_condition(): void
    {
        // This re-states, verbatim, the exact condition wp_mail() itself uses
        // in wp-includes/pluggable.php to decide whether to run its own
        // PHPMailer path: `if ( null !== $pre_wp_mail ) { return $pre_wp_mail; }`
        // A failed send must keep satisfying it -- i.e. never fall back to
        // `null` -- or WordPress will attempt a second, duplicate delivery.
        Functions\when('do_action')->justReturn(null);

        $router = $this->routerWithSendResult([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'provider rejected the message',
        ]);

        $preWpMail = $router->intercept(null, $this->baseAtts());

        $coreWouldShortCircuit = ( null !== $preWpMail );
        $this->assertTrue($coreWouldShortCircuit, 'core must still short-circuit on a failed send, or WP retries the send itself and duplicates the email');
        $this->assertFalse($preWpMail, 'the short-circuited value must be the honest false, not a placeholder true');
    }

    public function test_failed_send_fires_wp_mail_failed_exactly_once_with_wp_error(): void
    {
        /** @var list<array{string,array<mixed>}> */
        $fired = [];
        Functions\when('do_action')->alias(function (string $hook, ...$args) use (&$fired): void {
            $fired[] = [$hook, $args];
        });

        $router = $this->routerWithSendResult([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'smtp connect() failed: Connection refused',
        ]);

        $atts = $this->baseAtts();
        $atts['message'] = 'SECRET-MESSAGE-BODY-MUST-NEVER-LEAK';

        $router->intercept(null, $atts);

        $wpMailFailedCalls = array_values(array_filter($fired, static fn (array $c) => $c[0] === 'wp_mail_failed'));
        $this->assertCount(1, $wpMailFailedCalls, 'wp_mail_failed must fire exactly once on a failed send');

        $error = $wpMailFailedCalls[0][1][0] ?? null;
        $this->assertInstanceOf(WP_Error::class, $error, 'wp_mail_failed must be passed a WP_Error, matching core');
        $this->assertSame('smtp connect() failed: Connection refused', $error->get_error_message());

        // FINDING D6: the body must appear in NO field of the error data,
        // under ANY key -- not merely absent under the literal key
        // 'message'. The old version of this assertion was
        // assertArrayNotHasKey('message', $data), which a leak under a
        // DIFFERENT key (e.g. 'body') sails straight through; the
        // adversarial review proved exactly that by relocating the leak.
        // Serialize the whole structure and search for the body text itself
        // instead of trusting any particular key name to stay empty.
        $data       = $error->get_error_data();
        $serialized = (string) json_encode($data);
        $this->assertStringNotContainsString(
            'SECRET-MESSAGE-BODY-MUST-NEVER-LEAK',
            $serialized,
            'wp_mail_failed error data must never carry the message body, under any key'
        );
        $this->assertSame('someone@example.com', $data['to']);
        $this->assertSame('Subject', $data['subject']);
    }

    // -------------------------------------------------------------------------
    // Adversarial review of GH #439: three HIGH regressions.
    // -------------------------------------------------------------------------

    /**
     * Replays a set of registered `pre_wp_mail` filters in the SAME
     * priority-then-insertion order WP_Hook::apply_filters() uses. Brain
     * Monkey's built-in add_filter()/apply_filters() are expectation
     * trackers, not a working dispatcher, so a faithful multi-filter replay
     * needs a real pub-sub -- same reasoning as MailFailureCaptureTest's
     * add_action/do_action alias pair. usort() is stable as of PHP 8, which
     * this plugin already requires (composer.json: "php": ">=8.1"), so
     * same-priority ties keep registration order exactly like core.
     *
     * @param list<array{priority:int,callback:callable}> $filters
     * @param array<string,mixed>                          $atts
     * @return mixed
     */
    private function dispatchPreWpMail(array $filters, array $atts)
    {
        usort($filters, static fn (array $a, array $b): int => $a['priority'] <=> $b['priority']);

        $return = null;
        foreach ($filters as $filter) {
            $return = ($filter['callback'])($return, $atts);
        }

        return $return;
    }

    /**
     * Registers the add_filter() alias and returns the list that it will
     * populate. IMPORTANT: this list must be read AFTER register_hooks()
     * runs, and by the SAME reference this method returns -- an earlier
     * version of this helper returned a plain array *by value* before any
     * filter had been registered, so callers held a permanently-empty copy
     * while the real registrations landed in this method's own (dead) local
     * scope. Wrapping it in an object gives callers a live handle instead.
     *
     * @return object{filters: list<array{priority:int,callback:callable}>}
     */
    private function captureRegisteredPreWpMailFilters(): object
    {
        $box = new class () {
            /** @var list<array{priority:int,callback:callable}> */
            public array $filters = [];
        };
        Functions\when('add_filter')->alias(function (string $hook, $callback, int $priority = 10, int $acceptedArgs = 1) use ($box): bool {
            if ($hook === 'pre_wp_mail') {
                $box->filters[] = ['priority' => $priority, 'callback' => $callback];
            }
            return true;
        });

        return $box;
    }

    /**
     * REGRESSION A, RED case: a later, ordinary third-party idiom
     * (`if (!$return) return null;`) resets our honest `false` back to
     * `null`. Per wp-includes/pluggable.php, only `null` lets wp_mail() fall
     * through to its own PHPMailer -- so this reproduces core running a
     * SECOND, duplicate delivery attempt for a message WPMgr already tried
     * and knows failed. Fails on the current single-filter registration;
     * must pass once a second, PHP_INT_MAX-priority filter re-asserts the
     * short-circuit.
     */
    public function test_a_later_falsy_testing_filter_does_not_cause_a_duplicate_send(): void
    {
        $filters = $this->captureRegisteredPreWpMailFilters();

        /** @var list<array{string,array<mixed>}> */
        $fired = [];
        Functions\when('do_action')->alias(function (string $hook, ...$args) use (&$fired): void {
            $fired[] = [$hook, $args];
        });

        $router = $this->routerWithSendResult([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'smtp connect() failed: Connection refused',
        ]);
        $router->register_hooks();

        // A1 from the adversarial review: an entirely ordinary idiom, at a
        // later priority than our own primary filter (10).
        $filters->filters[] = [
            'priority' => 20,
            'callback' => static function ($return, array $atts) {
                if (!$return) {
                    return null;
                }
                return $return;
            },
        ];

        $finalReturn = $this->dispatchPreWpMail($filters->filters, $this->baseAtts());

        // Faithful re-implementation of wp_mail()'s own short-circuit
        // condition and its fallback to core's own PHPMailer.
        $phpMailerRuns = 0;
        if (null !== $finalReturn) {
            $wpMailReturn = $finalReturn;
        } else {
            ++$phpMailerRuns;
            $wpMailReturn = true;
        }

        $this->assertSame(
            0,
            $phpMailerRuns,
            'a message WPMgr already attempted and knows failed must never be re-sent by core -- this is the duplicate-send regression'
        );
        $this->assertFalse($wpMailReturn, "wp_mail()'s return value must stay the honest false, not core's own (successful) second attempt");

        $wpMailFailedCalls = array_values(array_filter($fired, static fn (array $c) => $c[0] === 'wp_mail_failed'));
        $this->assertCount(1, $wpMailFailedCalls, 'the re-assert filter must not cause a second wp_mail_failed fire');
    }

    /**
     * REGRESSION A, over-fire control: a completely successful send must
     * pass through the second filter unchanged, and the second filter must
     * never itself call do_action.
     */
    public function test_a_successful_send_passes_through_the_reassert_filter_unchanged(): void
    {
        $filters = $this->captureRegisteredPreWpMailFilters();
        Functions\when('do_action')->justReturn(null);

        $sendResult = ['ok' => true, 'message_id' => 'abc123', 'detail' => ''];
        $router     = $this->routerWithSendResult($sendResult);
        $router->register_hooks();

        $finalReturn = $this->dispatchPreWpMail($filters->filters, $this->baseAtts());

        $this->assertSame($sendResult, $finalReturn, 'a successful send must be completely unchanged by the reassert filter');
    }

    /**
     * REGRESSION A, over-fire control: a plugin that legitimately
     * short-circuits BEFORE WPMgr (an earlier priority) must still win, and
     * WPMgr must never have attempted a send at all.
     */
    public function test_a_plugin_that_short_circuits_before_wpmgr_still_wins(): void
    {
        $filters = $this->captureRegisteredPreWpMailFilters();

        /** @var list<array{string,array<mixed>}> */
        $fired = [];
        Functions\when('do_action')->alias(function (string $hook, ...$args) use (&$fired): void {
            $fired[] = [$hook, $args];
        });
        Functions\when('get_option')->justReturn(['provider' => 'smtp']);

        $providerRouter = $this->createMock(ProviderRouter::class);
        $providerRouter->expects($this->never())->method('send');
        $settings = new Settings();
        $router   = new MailRouter($providerRouter, $settings);
        $router->register_hooks();

        $filters->filters[] = [
            'priority' => 5,
            'callback' => static fn ($return, array $atts) => 'other-plugin-handled-it',
        ];

        $finalReturn = $this->dispatchPreWpMail($filters->filters, $this->baseAtts());

        $this->assertSame('other-plugin-handled-it', $finalReturn, 'a plugin that already short-circuited before WPMgr must still win');
        $this->assertCount(0, array_filter($fired, static fn (array $c) => $c[0] === 'wp_mail_failed'), 'wp_mail_failed must not fire when WPMgr never attempted a send');
    }

    /**
     * REGRESSION E1, RED case: a wp_mail_failed listener that throws must
     * not propagate out of intercept() -- core's own wp_mail() does not
     * catch anything from this hook, so an uncaught exception here would
     * escape wp_mail() itself and fatal whatever called it (a checkout, a
     * password reset). intercept() must still produce its honest `false`.
     */
    public function test_a_throwing_wp_mail_failed_listener_does_not_prevent_the_honest_false_return(): void
    {
        Functions\when('do_action')->alias(function (string $hook, ...$args): void {
            if ($hook === 'wp_mail_failed') {
                throw new \RuntimeException('a badly-behaved listener blew up');
            }
        });

        $router = $this->routerWithSendResult([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'smtp connect() failed: Connection refused',
        ]);

        $result = $router->intercept(null, $this->baseAtts());

        $this->assertFalse($result, 'a throwing wp_mail_failed listener must not stop intercept() from returning the honest false');
    }

    /**
     * REGRESSION E2, RED case: a wp_mail_failed listener that itself calls
     * wp_mail() -- exactly what a failure-alerting plugin does -- must not
     * recurse without bound when the alert send ALSO fails. Reproduced by
     * the adversarial review to depth 2001. The 50-call ceiling here is an
     * adversarial safety valve only: the assertion is that the PRODUCTION
     * guard stops this at a small, fixed depth, not that the valve caught
     * it.
     */
    public function test_a_wp_mail_failed_listener_that_calls_wp_mail_again_does_not_recurse_unboundedly(): void
    {
        Functions\when('get_option')->justReturn(['provider' => 'smtp']);

        $providerRouter = $this->createMock(ProviderRouter::class);
        $providerRouter->method('send')->willReturn([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'provider is completely broken',
        ]);
        $settings = new Settings();
        $router   = new MailRouter($providerRouter, $settings);

        $wpMailCalls = 0;
        // Faithful re-implementation of wp_mail(): dispatch to intercept()
        // exactly as pre_wp_mail would, then -- per
        // wp-includes/pluggable.php -- only run core's own PHPMailer path
        // when the result is exactly null.
        $fakeWpMail = function (array $atts) use (&$fakeWpMail, &$wpMailCalls, $router): void {
            ++$wpMailCalls;
            if ($wpMailCalls > 50) {
                throw new \RuntimeException('unbounded recursion: wp_mail() re-entered more than 50 times');
            }
            $router->intercept(null, $atts);
        };

        // A textbook failure-alerting plugin: on wp_mail_failed, send an
        // alert -- through wp_mail() again. Wired via a real do_action so
        // the recursive call happens exactly where core triggers it,
        // synchronously, inside MailRouter's own do_action('wp_mail_failed').
        Functions\when('do_action')->alias(function (string $hook, ...$args) use ($fakeWpMail): void {
            if ($hook === 'wp_mail_failed') {
                $fakeWpMail([
                    'to'      => 'admin@example.com',
                    'subject' => 'Mail alert',
                    'message' => 'a send failed',
                    'headers' => '',
                ]);
            }
        });

        $fakeWpMail($this->baseAtts());

        $this->assertLessThanOrEqual(2, $wpMailCalls, 'a wp_mail_failed listener that itself calls wp_mail() must not recurse without bound');
    }

    /**
     * FINDING C: headers pass through to every third-party wp_mail_failed
     * listener verbatim today (matching core's own shape), so a Bcc archive
     * address or a bearer token a site set for its own delivery would reach
     * every OTHER plugin subscribed to the hook -- on a failure path that
     * never fired at all before GH #439 made a failed send honest. Known-
     * sensitive header VALUES are redacted; the header NAME (and every
     * other header) stays visible for diagnosis.
     */
    public function test_error_data_headers_redact_bcc_and_auth_shaped_values_but_keep_others(): void
    {
        /** @var list<array{string,array<mixed>}> */
        $fired = [];
        Functions\when('do_action')->alias(function (string $hook, ...$args) use (&$fired): void {
            $fired[] = [$hook, $args];
        });

        $router = $this->routerWithSendResult([
            'ok'         => false,
            'message_id' => '',
            'detail'     => 'provider rejected the message',
        ]);

        $atts             = $this->baseAtts();
        $atts['headers']  = "Content-Type: text/plain\r\n"
            . "Bcc: secret-archive@internal.example\r\n"
            . "X-Auth-Token: Bearer abc123\r\n"
            . 'X-WPMgr-Site: site-123';

        $router->intercept(null, $atts);

        $wpMailFailedCalls = array_values(array_filter($fired, static fn (array $c) => $c[0] === 'wp_mail_failed'));
        $error             = $wpMailFailedCalls[0][1][0];
        $data              = $error->get_error_data();
        $serialized        = (string) json_encode($data['headers']);

        $this->assertStringNotContainsString('secret-archive@internal.example', $serialized, 'the Bcc VALUE must be redacted');
        $this->assertStringNotContainsString('abc123', $serialized, 'the auth token VALUE must be redacted');
        $this->assertStringContainsString('Bcc', $serialized, 'the header NAME stays visible for diagnosis');
        $this->assertStringContainsString('X-Auth-Token', $serialized, 'the header NAME stays visible for diagnosis');
        $this->assertStringContainsString('X-WPMgr-Site: site-123', $serialized, 'a non-sensitive header passes through unchanged');
    }

    public function test_successful_send_is_unchanged_and_does_not_fire_wp_mail_failed(): void
    {
        /** @var list<array{string,array<mixed>}> */
        $fired = [];
        Functions\when('do_action')->alias(function (string $hook, ...$args) use (&$fired): void {
            $fired[] = [$hook, $args];
        });

        $sendResult = [
            'ok'         => true,
            'message_id' => 'abc123',
            'detail'     => '',
        ];
        $router = $this->routerWithSendResult($sendResult);

        $result = $router->intercept(null, $this->baseAtts());

        $this->assertSame($sendResult, $result, 'a successful send must return the same value as before this fix');
        $wpMailFailedCalls = array_filter($fired, static fn (array $c) => $c[0] === 'wp_mail_failed');
        $this->assertCount(0, $wpMailFailedCalls, 'wp_mail_failed must never fire on a successful send');
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
