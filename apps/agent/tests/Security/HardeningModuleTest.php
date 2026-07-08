<?php
/**
 * P1 outcome test (GH #170 Wave 5): `HardeningModule::install()` registers a
 * PHP-runtime fallback applier for each toggle that has one (the defence-in-
 * depth path that actually enforces the rule on nginx sites, or on Apache
 * sites where the .htaccess write failed). Existing coverage
 * (SyncSecurityHardeningCommandTest, WafGateHardeningTest, ServerConfigWriterTest)
 * exercises persistence, the WAF mu-plugin boot gate, and the .htaccess string
 * directives — but never actually INVOKES the registered closures to prove
 * their block/allow DECISION. A stub applier that registers a hook and does
 * nothing (leaving the rule unenforced on nginx) passes every one of those
 * suites today.
 *
 * `HardeningModule::install()` itself is guarded by a function-local
 * `static $installed` idempotency latch (the same convention as every other
 * security module's install() — see HideBackendModule) that fires at most
 * ONCE per PHP process, regardless of how many HardeningModule instances or
 * configs are involved. Rather than pay `@runInSeparateProcess` overhead for
 * every single toggle (and rely on execution order for a shared static that
 * cannot be reset from outside), these tests invoke each private applier
 * method DIRECTLY via reflection — the exact same methods install() calls,
 * carrying the exact same closures — capturing what add_action()/add_filter()
 * are given via `Functions\expect(...)->andReturnUsing(...)`, then invoking
 * those captured closures with representative inputs and asserting the real
 * block/allow OUTCOME.
 *
 * Coverage:
 *   - applyBanFilters:        a banned user-agent reaches the 403 block
 *                              (immediately before exit); a clean UA passes.
 *   - applyRestRestrict:      an anonymous REST request is blocked with a 401
 *                              WP_Error; the agent's own /wpmgr/v1/* namespace,
 *                              /oembed/1.0/*, and logged-in requests all pass.
 *   - applyForceSsl:          a plain-HTTP request is redirected to https with
 *                              301 (and never redirected when already SSL);
 *                              the HSTS header is sent only over HTTPS.
 *   - applyXmlrpc:            xmlrpc_mode=off registers WP core's
 *                              __return_false as the xmlrpc_enabled callback,
 *                              and that callback truly evaluates to false.
 *   - applyLoginIdentifier:   username-mode removes EXACTLY
 *                              wp_authenticate_email_password (never the
 *                              other one); email-mode removes EXACTLY
 *                              wp_authenticate_username_password; both-mode
 *                              registers no hook at all.
 *   - applyDisableFileEditor: strips edit_themes/edit_plugins caps only when
 *                              the check is actually about those caps.
 *   - applyAuthorArchiveEnum: hides /wp/v2/users(/me) from anonymous REST
 *                              index requests, but not from logged-in ones.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\HardeningConfig;
use WPMgr\Agent\Security\HardeningModule;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\HardeningModule
 */
final class HardeningModuleTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('sanitize_text_field')->alias(fn ($v) => $v);
        Functions\when('wp_unslash')->alias(fn ($v) => $v);
        Functions\when('headers_sent')->justReturn(false);
        Functions\when('is_ssl')->justReturn(false);

        // add_action()/add_filter() are deliberately NOT stubbed here — each
        // test sets up its own Functions\expect()/Functions\when() so the
        // capture-and-invoke pattern below works (mirrors HideBackendModuleTest).
    }

    protected function tear_down(): void
    {
        unset(
            $_SERVER['HTTP_USER_AGENT'],
            $_SERVER['HTTP_HOST'],
            $_SERVER['REQUEST_URI'],
            $GLOBALS['wp']
        );
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function module(): HardeningModule
    {
        return new HardeningModule();
    }

    /**
     * Build a HardeningConfig with a single overridden toggle; every other
     * toggle stays at its safe off-default.
     *
     * @param array<string,mixed> $overrides
     */
    private function configWith(array $overrides): HardeningConfig
    {
        return HardeningConfig::fromArray(['config' => $overrides]);
    }

    /**
     * Invoke a private per-toggle applier method directly, bypassing
     * install()'s process-wide idempotency latch (see class docblock).
     */
    private function invokeApplier(HardeningModule $mod, string $method, HardeningConfig $config): void
    {
        $ref = new \ReflectionMethod($mod, $method);
        $ref->setAccessible(true);
        $ref->invoke($mod, $config);
    }

    // =========================================================================
    // applyBanFilters — banned user-agent block
    // =========================================================================

    public function test_banned_user_agent_reaches_403_block(): void
    {
        $mod    = $this->module();
        $config = HardeningConfig::fromArray([
            'bans' => [['id' => 'b1', 'type' => 'user_agent', 'value' => 'EvilBot']],
        ]);

        $captured = null;
        Functions\expect('add_action')
            ->once()
            ->with('init', \Mockery::type('callable'), 1)
            ->andReturnUsing(function ($hook, $cb, $priority) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyBanFilters', $config);
        $this->assertIsCallable($captured, 'a user-agent ban must register an init callback at priority 1');

        $_SERVER['HTTP_USER_AGENT'] = 'Mozilla/5.0 EvilBot/1.0';

        Functions\expect('http_response_code')->once()->with(403);
        // exit cannot be intercepted; stub the last function called on the
        // block statement immediately before it and treat reaching it as proof.
        Functions\when('header')->alias(function (string $h) {
            if (strpos($h, 'Cache-Control') === 0) {
                throw new \RuntimeException('marker:ua_blocked');
            }
            return null;
        });

        $threw = false;
        try {
            ($captured)();
        } catch (\RuntimeException $e) {
            $threw = ($e->getMessage() === 'marker:ua_blocked');
        }

        $this->assertTrue($threw, 'a banned user-agent must reach the 403 block (immediately before exit)');
    }

    public function test_clean_user_agent_passes_through_ban_filter(): void
    {
        $mod    = $this->module();
        $config = HardeningConfig::fromArray([
            'bans' => [['id' => 'b1', 'type' => 'user_agent', 'value' => 'EvilBot']],
        ]);

        $captured = null;
        Functions\expect('add_action')
            ->once()
            ->with('init', \Mockery::type('callable'), 1)
            ->andReturnUsing(function ($hook, $cb, $priority) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyBanFilters', $config);

        $_SERVER['HTTP_USER_AGENT'] = 'Mozilla/5.0 Chrome/120.0';

        Functions\expect('http_response_code')->never();

        // Must return normally — no exception, no exit.
        ($captured)();
        $this->addToAssertionCount(1);
    }

    // =========================================================================
    // applyRestRestrict — anonymous REST 401, allowlisted routes/logged-in pass
    // =========================================================================

    private function restRestrictClosure(HardeningModule $mod, HardeningConfig $config): callable
    {
        $captured = null;
        Functions\expect('add_filter')
            ->once()
            ->with('rest_authentication_errors', \Mockery::type('callable'))
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyRestRestrict', $config);
        $this->assertIsCallable($captured);
        return $captured;
    }

    public function test_rest_restrict_blocks_anonymous_request_with_401(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_rest_api' => HardeningConfig::REST_RESTRICTED]);
        $closure = $this->restRestrictClosure($mod, $config);

        $GLOBALS['wp'] = (object) ['query_vars' => ['rest_route' => '/wp/v2/posts']];

        $result = $closure(null);

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame('rest_not_logged_in', $result->get_error_code());
        $this->assertSame(401, $result->get_error_data()['status'] ?? null);
    }

    public function test_rest_restrict_allows_agent_namespace(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_rest_api' => HardeningConfig::REST_RESTRICTED]);
        $closure = $this->restRestrictClosure($mod, $config);

        $GLOBALS['wp'] = (object) ['query_vars' => ['rest_route' => '/wpmgr/v1/ping']];

        $this->assertNull($closure(null), 'the agent-owned REST namespace must always pass, even for anonymous requests');
    }

    public function test_rest_restrict_allows_oembed(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_rest_api' => HardeningConfig::REST_RESTRICTED]);
        $closure = $this->restRestrictClosure($mod, $config);

        $GLOBALS['wp'] = (object) ['query_vars' => ['rest_route' => '/oembed/1.0/embed']];

        $this->assertNull($closure(null), 'the oembed consumer route must always pass');
    }

    public function test_rest_restrict_allows_logged_in_user(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_rest_api' => HardeningConfig::REST_RESTRICTED]);
        $closure = $this->restRestrictClosure($mod, $config);

        Functions\when('is_user_logged_in')->justReturn(true);
        $GLOBALS['wp'] = (object) ['query_vars' => ['rest_route' => '/wp/v2/posts']];

        $this->assertNull($closure(null), 'a logged-in user must always pass, regardless of route');
    }

    // =========================================================================
    // applyForceSsl — 301 redirect to https + HSTS header
    // =========================================================================

    /**
     * @return array{0:callable,1:callable} [template_redirect closure, send_headers closure]
     */
    private function forceSslClosures(HardeningModule $mod, HardeningConfig $config): array
    {
        $redirectClosure = null;
        $headersClosure   = null;

        Functions\expect('add_action')
            ->once()
            ->with('template_redirect', \Mockery::type('callable'), 1)
            ->andReturnUsing(function ($hook, $cb, $priority) use (&$redirectClosure) {
                $redirectClosure = $cb;
                return true;
            });
        Functions\expect('add_action')
            ->once()
            ->with('send_headers', \Mockery::type('callable'))
            ->andReturnUsing(function ($hook, $cb) use (&$headersClosure) {
                $headersClosure = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyForceSsl', $config);

        $this->assertIsCallable($redirectClosure);
        $this->assertIsCallable($headersClosure);

        return [$redirectClosure, $headersClosure];
    }

    public function test_force_ssl_redirects_plain_request_to_https_with_301(): void
    {
        Functions\when('php_sapi_name')->justReturn('fpm-fcgi');
        Functions\when('is_ssl')->justReturn(false);

        $mod    = $this->module();
        $config = $this->configWith(['force_ssl' => true]);
        [$redirectClosure, ] = $this->forceSslClosures($mod, $config);

        $_SERVER['HTTP_HOST']   = 'example.com';
        $_SERVER['REQUEST_URI'] = '/some/path?x=1';

        $capturedArgs = null;
        Functions\when('wp_safe_redirect')->alias(function (string $location, int $status = 302) use (&$capturedArgs) {
            $capturedArgs = [$location, $status];
            throw new \RuntimeException('marker:redirected');
        });

        $threw = false;
        try {
            ($redirectClosure)();
        } catch (\RuntimeException $e) {
            $threw = ($e->getMessage() === 'marker:redirected');
        }

        $this->assertTrue($threw, 'a plain-HTTP request must reach the wp_safe_redirect() call (immediately before exit)');
        $this->assertSame(['https://example.com/some/path?x=1', 301], $capturedArgs);
    }

    public function test_force_ssl_does_not_redirect_when_already_ssl(): void
    {
        Functions\when('php_sapi_name')->justReturn('fpm-fcgi');
        Functions\when('is_ssl')->justReturn(true);

        $mod    = $this->module();
        $config = $this->configWith(['force_ssl' => true]);
        [$redirectClosure, ] = $this->forceSslClosures($mod, $config);

        Functions\expect('wp_safe_redirect')->never();

        ($redirectClosure)();
        $this->addToAssertionCount(1);
    }

    public function test_force_ssl_sends_hsts_header_on_https_response(): void
    {
        Functions\when('is_ssl')->justReturn(true);

        $mod    = $this->module();
        $config = $this->configWith(['force_ssl' => true]);
        [, $headersClosure] = $this->forceSslClosures($mod, $config);

        $capturedHeaders = [];
        Functions\when('header')->alias(function (string $h) use (&$capturedHeaders) {
            $capturedHeaders[] = $h;
            return null;
        });

        ($headersClosure)();

        $this->assertContains(
            'Strict-Transport-Security: max-age=31536000; includeSubDomains',
            $capturedHeaders
        );
    }

    public function test_force_ssl_does_not_send_hsts_header_on_plain_http(): void
    {
        Functions\when('is_ssl')->justReturn(false);

        $mod    = $this->module();
        $config = $this->configWith(['force_ssl' => true]);
        [, $headersClosure] = $this->forceSslClosures($mod, $config);

        Functions\expect('header')->never();

        ($headersClosure)();
        $this->addToAssertionCount(1);
    }

    // =========================================================================
    // applyXmlrpc — off mode's registered callback truly evaluates to false
    // =========================================================================

    public function test_xmlrpc_off_registers_a_callback_that_actually_returns_false(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['xmlrpc_mode' => HardeningConfig::XMLRPC_OFF]);

        $captured = null;
        Functions\expect('add_filter')
            ->once()
            ->with('xmlrpc_enabled', \Mockery::type('string'))
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyXmlrpc', $config);

        $this->assertSame(
            '__return_false',
            $captured,
            'xmlrpc off must register the WP core always-false callback'
        );
        $this->assertFalse(
            call_user_func($captured),
            'the registered xmlrpc_enabled callback must actually evaluate to false, not merely be registered'
        );
    }

    // =========================================================================
    // applyLoginIdentifier — remove exactly the right core auth filter
    // =========================================================================

    private function loginIdentifierClosure(HardeningModule $mod, HardeningConfig $config): callable
    {
        $captured = null;
        Functions\expect('add_action')
            ->once()
            ->with('init', \Mockery::type('callable'))
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyLoginIdentifier', $config);
        $this->assertIsCallable($captured);
        return $captured;
    }

    public function test_login_identifier_username_mode_removes_only_email_password_filter(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_login_identifier' => HardeningConfig::LOGIN_USERNAME]);
        $closure = $this->loginIdentifierClosure($mod, $config);

        Functions\expect('remove_filter')
            ->once()
            ->with('authenticate', 'wp_authenticate_email_password', 20);
        Functions\expect('remove_filter')
            ->never()
            ->with('authenticate', 'wp_authenticate_username_password', 20);

        ($closure)();
        $this->addToAssertionCount(1);
    }

    public function test_login_identifier_email_mode_removes_only_username_password_filter(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_login_identifier' => HardeningConfig::LOGIN_EMAIL]);
        $closure = $this->loginIdentifierClosure($mod, $config);

        Functions\expect('remove_filter')
            ->once()
            ->with('authenticate', 'wp_authenticate_username_password', 20);
        Functions\expect('remove_filter')
            ->never()
            ->with('authenticate', 'wp_authenticate_email_password', 20);

        ($closure)();
        $this->addToAssertionCount(1);
    }

    public function test_login_identifier_both_mode_registers_no_hook(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['restrict_login_identifier' => HardeningConfig::LOGIN_BOTH]);

        Functions\expect('add_action')->never();

        $this->invokeApplier($mod, 'applyLoginIdentifier', $config);
        $this->addToAssertionCount(1);
    }

    // =========================================================================
    // applyDisableFileEditor — strips edit_themes/edit_plugins only when relevant
    // =========================================================================

    public function test_disable_file_editor_strips_theme_and_plugin_edit_caps(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['disable_file_editor' => true]);

        $captured = null;
        Functions\expect('add_filter')
            ->once()
            ->with('user_has_cap', \Mockery::type('callable'), 10, 2)
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyDisableFileEditor', $config);
        $this->assertIsCallable($captured);

        $caps   = ['edit_themes' => true, 'edit_plugins' => true, 'read' => true];
        $result = $captured($caps, ['edit_themes']);

        $this->assertFalse($result['edit_themes']);
        $this->assertFalse($result['edit_plugins']);
        $this->assertTrue($result['read'], 'unrelated caps must be left untouched');
    }

    public function test_disable_file_editor_leaves_unrelated_cap_checks_untouched(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['disable_file_editor' => true]);

        $captured = null;
        Functions\expect('add_filter')
            ->once()
            ->with('user_has_cap', \Mockery::type('callable'), 10, 2)
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyDisableFileEditor', $config);

        $caps   = ['edit_themes' => true, 'edit_plugins' => true, 'read' => true];
        $result = $captured($caps, ['read']);

        $this->assertTrue($result['edit_themes'], 'a cap check unrelated to file-editing must not strip edit_themes');
        $this->assertTrue($result['edit_plugins'], 'a cap check unrelated to file-editing must not strip edit_plugins');
    }

    // =========================================================================
    // applyAuthorArchiveEnum — hide /wp/v2/users(/me) from anonymous REST index
    // =========================================================================

    public function test_author_archive_enum_hides_users_endpoint_from_anonymous_rest(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['disable_author_archive_enum' => true]);

        // template_redirect registration is not under test in this method.
        Functions\when('add_action')->justReturn(true);

        $captured = null;
        Functions\expect('add_filter')
            ->once()
            ->with('rest_endpoints', \Mockery::type('callable'))
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyAuthorArchiveEnum', $config);
        $this->assertIsCallable($captured);

        $endpoints = [
            '/wp/v2/users'    => ['x'],
            '/wp/v2/users/me' => ['y'],
            '/wp/v2/posts'    => ['z'],
        ];

        $result = $captured($endpoints);

        $this->assertArrayNotHasKey('/wp/v2/users', $result);
        $this->assertArrayNotHasKey('/wp/v2/users/me', $result);
        $this->assertArrayHasKey('/wp/v2/posts', $result, 'unrelated REST endpoints must survive');
    }

    public function test_author_archive_enum_leaves_users_endpoint_for_logged_in_requests(): void
    {
        $mod    = $this->module();
        $config = $this->configWith(['disable_author_archive_enum' => true]);

        Functions\when('add_action')->justReturn(true);
        Functions\when('is_user_logged_in')->justReturn(true);

        $captured = null;
        Functions\expect('add_filter')
            ->once()
            ->with('rest_endpoints', \Mockery::type('callable'))
            ->andReturnUsing(function ($hook, $cb) use (&$captured) {
                $captured = $cb;
                return true;
            });

        $this->invokeApplier($mod, 'applyAuthorArchiveEnum', $config);

        $endpoints = ['/wp/v2/users' => ['x'], '/wp/v2/posts' => ['z']];
        $result    = $captured($endpoints);

        $this->assertArrayHasKey('/wp/v2/users', $result, 'a logged-in request must see the full endpoint list');
    }
}
