<?php
/**
 * AuthHeaderShieldTest: regression coverage for AuthHeaderShield, including
 * the LiveCanvas-class regression the shield exists to prevent: a third-party
 * plugin's global Authorization/JWT auth filter throwing on the agent's own
 * signed bearer token before the agent's own permission_callback ever runs.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use ReflectionMethod;
use ReflectionProperty;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Router;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\AuthHeaderShield
 */
final class AuthHeaderShieldTest extends TestCase
{
    /** @var array<string,mixed> Snapshot of the relevant $_SERVER keys, restored in tear_down(). */
    private array $serverSnapshot = [];

    protected function set_up(): void
    {
        parent::set_up();

        $this->serverSnapshot = [
            'REQUEST_URI'                  => $_SERVER['REQUEST_URI'] ?? null,
            'HTTP_AUTHORIZATION'           => $_SERVER['HTTP_AUTHORIZATION'] ?? null,
            'REDIRECT_HTTP_AUTHORIZATION'  => $_SERVER['REDIRECT_HTTP_AUTHORIZATION'] ?? null,
        ];

        unset($_SERVER['REQUEST_URI'], $_SERVER['HTTP_AUTHORIZATION'], $_SERVER['REDIRECT_HTTP_AUTHORIZATION']);
        $this->resetShieldStash();
    }

    protected function tear_down(): void
    {
        foreach ($this->serverSnapshot as $key => $value) {
            if ($value === null) {
                unset($_SERVER[$key]);
            } else {
                $_SERVER[$key] = $value;
            }
        }
        $this->resetShieldStash();

        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Core stripping behaviour
    // -------------------------------------------------------------------------

    public function test_strips_bearer_on_wpmgr_command_route(): void
    {
        $_SERVER['REQUEST_URI']        = '/wp-json/wpmgr/v1/command/update';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer ABC.DEF.GHI';

        AuthHeaderShield::protect();

        $this->assertArrayNotHasKey('HTTP_AUTHORIZATION', $_SERVER);
        $this->assertSame('ABC.DEF.GHI', AuthHeaderShield::bearer());
    }

    public function test_strips_bearer_on_plain_permalink_rest_route_query_string(): void
    {
        $_SERVER['REQUEST_URI']        = '/?rest_route=%2Fwpmgr%2Fv1%2Fcommand%2Fupdate';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer PLAIN.PERMALINK.TOKEN';

        AuthHeaderShield::protect();

        $this->assertArrayNotHasKey('HTTP_AUTHORIZATION', $_SERVER);
        $this->assertSame('PLAIN.PERMALINK.TOKEN', AuthHeaderShield::bearer());
    }

    public function test_falls_back_to_redirect_http_authorization(): void
    {
        $_SERVER['REQUEST_URI']                 = '/wp-json/wpmgr/v1/info';
        $_SERVER['REDIRECT_HTTP_AUTHORIZATION'] = 'Bearer REDIRECT.VARIANT.TOKEN';

        AuthHeaderShield::protect();

        $this->assertArrayNotHasKey('HTTP_AUTHORIZATION', $_SERVER);
        $this->assertArrayNotHasKey('REDIRECT_HTTP_AUTHORIZATION', $_SERVER);
        $this->assertSame('REDIRECT.VARIANT.TOKEN', AuthHeaderShield::bearer());
    }

    public function test_multisite_subdirectory_prefixed_route_is_stripped(): void
    {
        $_SERVER['REQUEST_URI']        = '/site2/wp-json/wpmgr/v1/command/update';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer MULTISITE.TOKEN.HERE';

        AuthHeaderShield::protect();

        $this->assertArrayNotHasKey('HTTP_AUTHORIZATION', $_SERVER);
        $this->assertSame('MULTISITE.TOKEN.HERE', AuthHeaderShield::bearer());
    }

    // -------------------------------------------------------------------------
    // Things the shield must NOT touch
    // -------------------------------------------------------------------------

    public function test_non_wpmgr_route_is_left_untouched(): void
    {
        $_SERVER['REQUEST_URI']        = '/wp-json/wp/v2/posts';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer X';

        AuthHeaderShield::protect();

        $this->assertSame('Bearer X', $_SERVER['HTTP_AUTHORIZATION']);
        $this->assertNull(AuthHeaderShield::bearer());
    }

    public function test_non_bearer_scheme_on_wpmgr_route_is_left_untouched(): void
    {
        $_SERVER['REQUEST_URI']        = '/wp-json/wpmgr/v1/command/update';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Basic dXNlcjpwYXNz';

        AuthHeaderShield::protect();

        $this->assertSame('Basic dXNlcjpwYXNz', $_SERVER['HTTP_AUTHORIZATION']);
        $this->assertNull(AuthHeaderShield::bearer());
    }

    public function test_missing_request_uri_is_a_noop(): void
    {
        // REQUEST_URI intentionally left unset.
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer X';

        AuthHeaderShield::protect();

        $this->assertSame('Bearer X', $_SERVER['HTTP_AUTHORIZATION']);
        $this->assertNull(AuthHeaderShield::bearer());
    }

    /**
     * The route match is path-vs-query anchored, not an unanchored substring
     * search over the whole raw URI: a completely unrelated route whose query
     * string merely CONTAINS the literal "/wpmgr/v1/" inside some other
     * parameter's value (here, a redirect target) must not be stripped.
     */
    public function test_namespace_literal_inside_an_unrelated_query_param_is_left_untouched(): void
    {
        $_SERVER['REQUEST_URI']        = '/wp-json/wp/v2/posts?redirect=/wpmgr/v1/info';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer X';

        AuthHeaderShield::protect();

        $this->assertSame('Bearer X', $_SERVER['HTTP_AUTHORIZATION']);
        $this->assertNull(AuthHeaderShield::bearer());
    }

    // -------------------------------------------------------------------------
    // The regression this class exists to prevent
    // -------------------------------------------------------------------------

    /**
     * Simulates the reported failure mode: a third-party plugin registers a
     * global auth hook that throws when it sees ANY Authorization: Bearer
     * value it does not recognize as its own JWT format. Proves both halves
     * of the fix: the fatal is prevented (the header is gone by the time such
     * a hook would run), AND the agent's own Router still authenticates the
     * request via the shield's stash.
     */
    public function test_stripped_header_prevents_third_party_filter_from_throwing_and_router_still_authenticates(): void
    {
        $_SERVER['REQUEST_URI']        = '/wp-json/wpmgr/v1/command/update';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer ABC.DEF.GHI';

        // This runs at plugin-include time, before any hook a plugin could
        // register has a chance to fire.
        AuthHeaderShield::protect();

        // Stand-in for a third-party plugin's global determine_current_user /
        // rest_authentication_errors filter: unconditionally throws if it
        // sees a Bearer-scheme Authorization header it doesn't recognize.
        $thirdPartyGlobalAuthFilter = static function (): void {
            $header = $_SERVER['HTTP_AUTHORIZATION'] ?? '';
            if (is_string($header) && stripos($header, 'Bearer ') === 0) {
                throw new \DomainException('Algorithm not supported');
            }
        };

        // Does not throw: the header is already gone.
        $thirdPartyGlobalAuthFilter();
        $this->addToAssertionCount(1);

        // The WP_REST_Request WordPress builds after this point never had the
        // header to begin with (it was removed from $_SERVER before REST
        // dispatch populated headers from the superglobals), so its own
        // 'authorization' header reads empty. Router::bearerToken() must
        // still resolve the real token from the shield's stash.
        $router  = $this->makeRouter();
        $request = new \WP_REST_Request();

        // ReflectionMethod::setAccessible() is unneeded (and deprecated since
        // PHP 8.5): PHP 8.1+ reflection can invoke a private method directly.
        $bearerToken = new ReflectionMethod(Router::class, 'bearerToken');
        $token       = $bearerToken->invoke($router, $request);

        $this->assertSame('ABC.DEF.GHI', $token);
    }

    /**
     * Backward-compat: when the shield has nothing stashed for this request
     * (e.g. a cookie-authenticated REST call, or a unit test that injects the
     * header directly), Router::bearerToken() still falls back to reading the
     * live request header, exactly as before this change.
     */
    public function test_router_bearer_token_falls_back_to_request_header_when_shield_has_no_stash(): void
    {
        // No REQUEST_URI / Authorization ever set on $_SERVER in this test,
        // and the shield's stash was reset in set_up(), so AuthHeaderShield::bearer()
        // is null: protect() was never even called here.
        $router  = $this->makeRouter();
        $request = new \WP_REST_Request();
        $request->set_header('authorization', 'Bearer XYZ');

        $bearerToken = new ReflectionMethod(Router::class, 'bearerToken');
        $token       = $bearerToken->invoke($router, $request);

        $this->assertSame('XYZ', $token);
    }

    // -------------------------------------------------------------------------
    // Namespace-drift guard
    // -------------------------------------------------------------------------

    /**
     * The shield's fast-path match string is hardcoded (not built from
     * Router::NAMESPACE) so protect() never has to load the Router class on
     * the non-wpmgr fast path. This test proves the hardcoded literal still
     * matches Router::NAMESPACE by building a request URI FROM the constant
     * and confirming the shield still recognizes it: if a future change to
     * Router::NAMESPACE is not mirrored in AuthHeaderShield::protect(), this
     * test fails.
     */
    public function test_hardcoded_match_literal_still_matches_router_namespace(): void
    {
        $_SERVER['REQUEST_URI']        = '/wp-json/' . Router::NAMESPACE . '/command/update';
        $_SERVER['HTTP_AUTHORIZATION'] = 'Bearer NAMESPACE.DRIFT.GUARD';

        AuthHeaderShield::protect();

        $this->assertArrayNotHasKey('HTTP_AUTHORIZATION', $_SERVER);
        $this->assertSame('NAMESPACE.DRIFT.GUARD', AuthHeaderShield::bearer());
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Reset AuthHeaderShield's private static stash between tests.
     *
     * ReflectionProperty::setAccessible() is unneeded (and deprecated since
     * PHP 8.5): PHP 8.1+ reflection can read/write a private property directly.
     */
    private function resetShieldStash(): void
    {
        $prop = new ReflectionProperty(AuthHeaderShield::class, 'stashedBearer');
        $prop->setValue(null, null);
    }

    /**
     * Build a real Router without invoking Connector's or Router's own
     * constructors' external dependencies, mirroring RouterTest's approach:
     * Connector is final and requires a Keystore + Settings we don't need for
     * bearerToken(), which never touches the Connector.
     */
    private function makeRouter(): Router
    {
        $rc        = new \ReflectionClass(Connector::class);
        $connector = $rc->newInstanceWithoutConstructor();

        return new Router($connector, []);
    }
}
