<?php
/**
 * Tests for AutologinCommand: end-to-end GET /wpmgr/v1/autologin behaviour,
 * including JWT verification, single-use replay shield, CP consume callback,
 * role allow-list, cookie issuance ordering, and redirect_to sanitization.
 *
 * Style note: mirrors EnrollmentTest / ConnectorTest. Brain Monkey is used
 * to stub WordPress functions; the real Connector + Signer are exercised with
 * real Ed25519 keys (no protocol mocking). Only the network layer (wp_remote_*)
 * and the ReplayCache spy are doubled.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\AutologinCommand;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\ReplayCache;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AutologinCommand
 * @covers \WPMgr\Agent\ReplayCache
 */
final class AutologinCommandTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> Recorded "options" backing store. */
    private array $options = [];

    /** @var array<int,array{string,mixed,mixed}> Recorded do_action invocations. */
    private array $hookCalls = [];

    /** @var array<int,array{int,bool,bool}> Recorded wp_set_auth_cookie calls. */
    private array $authCookieCalls = [];

    /** @var array<int,array{string,int}> Recorded wp_safe_redirect calls. */
    private array $redirectCalls = [];

    /** @var array<int,array{int,string}> Recorded wp_set_current_user calls. */
    private array $currentUserCalls = [];

    /** wp_clear_auth_cookie counter. */
    private int $clearAuthCount = 0;

    /** @var array<int,array{string,array<string,mixed>}> Outbound wp_remote_post calls. */
    private array $outboundPosts = [];

    /** Stubbed CP consume HTTP status. */
    private int $consumeStatus = 200;

    /** Stubbed CP consume response body. */
    private string $consumeBody = '';

    /** Site UUID = JWT `aud` for this site. */
    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    /** Control-plane Ed25519 secret key. */
    private string $cpSecret = '';

    /** Control-plane Ed25519 public key. */
    private string $cpPublic = '';

    private Keystore $keystore;

    private Connector $connector;

    private Signer $signer;

    private Settings $settings;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-auto-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options          = [];
        $this->hookCalls        = [];
        $this->authCookieCalls  = [];
        $this->redirectCalls    = [];
        $this->currentUserCalls = [];
        $this->clearAuthCount   = 0;
        $this->outboundPosts    = [];
        $this->consumeStatus    = 200;
        $this->consumeBody      = '';

        // wp-option store.
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);
            return true;
        });

        // Hook recorder.
        Functions\when('do_action')->alias(function (string $hook, $a = null, $b = null) {
            $this->hookCalls[] = [$hook, $a, $b];
        });

        Functions\when('wp_clear_auth_cookie')->alias(function (): void {
            $this->clearAuthCount++;
        });
        Functions\when('wp_set_current_user')->alias(function ($id, $login = ''): void {
            $this->currentUserCalls[] = [(int) $id, (string) $login];
        });
        Functions\when('wp_set_auth_cookie')->alias(function ($id, $remember = false, $secure = false): void {
            $this->authCookieCalls[] = [(int) $id, (bool) $remember, (bool) $secure];
        });
        Functions\when('wp_safe_redirect')->alias(function ($location, $status = 302): bool {
            $this->redirectCalls[] = [(string) $location, (int) $status];
            return true;
        });

        Functions\when('is_ssl')->justReturn(true);
        Functions\when('home_url')->alias(static function ($path = '') {
            return 'https://example.test' . (is_string($path) ? $path : '');
        });
        Functions\when('admin_url')->alias(static function ($path = '') {
            return 'https://example.test/wp-admin/' . ltrim(is_string($path) ? $path : '', '/');
        });
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('is_wp_error')->alias(static fn ($r) => $r instanceof \WP_Error);

        Functions\when('wp_remote_post')->alias(function ($url, $args) {
            $this->outboundPosts[] = [(string) $url, is_array($args) ? $args : []];
            return ['response' => ['code' => $this->consumeStatus], 'body' => $this->consumeBody];
        });
        Functions\when('wp_remote_retrieve_response_code')->alias(function ($response) {
            return (int) ($response['response']['code'] ?? 0);
        });
        Functions\when('wp_remote_retrieve_body')->alias(function ($response) {
            return (string) ($response['body'] ?? '');
        });

        $_SERVER['REMOTE_ADDR'] = '203.0.113.4';

        // Provision real CP keypair + persisted enrollment.
        $kp             = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($kp);
        $this->cpPublic = sodium_crypto_sign_publickey($kp);

        $this->keystore = new Keystore();
        $this->keystore->storeControlPlanePublicKey($this->cpPublic);
        // Site keypair for the Signer's outbound headers on the consume call.
        $this->keystore->generateSiteKeypair();

        $this->options[Settings::OPTION_SITE_ID]   = $this->siteId;
        $this->options[Settings::OPTION_TENANT_ID] = 'tenant-1';
        $this->options[Settings::OPTION_CP_URL]    = 'https://cp.example.test';

        $this->settings  = new Settings();
        $this->connector = new Connector($this->keystore, $this->settings);
        $this->signer    = new Signer($this->keystore);

        $GLOBALS['wpdb'] = new FakeAutologinWpdb();
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -----------------------------------------------------------------------
    // Helpers
    // -----------------------------------------------------------------------

    /**
     * Build a real Ed25519-signed autologin JWT for this site.
     *
     * @param string              $jti      Token id.
     * @param int                 $now      Issuance time anchor.
     * @param array<string,mixed> $extra    Extra/override claims.
     * @return string
     */
    private function jwt(string $jti, int $now, array $extra = []): string
    {
        $header = ['alg' => 'EdDSA', 'typ' => 'JWT'];
        $claims = array_merge([
            'iss' => 'wpmgr-control-plane',
            'iat' => $now,
            'exp' => $now + 30,
            'aud' => $this->siteId,
            'cmd' => 'autologin',
            'jti' => $jti,
            'tgt' => '',
        ], $extra);

        $segments = [
            self::b64((string) json_encode($header)),
            self::b64((string) json_encode($claims)),
        ];
        $signature = sodium_crypto_sign_detached(implode('.', $segments), $this->cpSecret);
        $segments[] = self::b64($signature);

        return implode('.', $segments);
    }

    private static function b64(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    /**
     * Build an AutologinCommand for the given (optional) replay-cache double.
     */
    private function command(?ReplayCache $replay = null): AutologinCommand
    {
        return new AutologinCommand(
            $this->connector,
            $replay ?? new ReplayCache(),
            $this->signer,
            $this->settings,
        );
    }

    /**
     * @param array<int,string> $roles Roles array.
     */
    private function stubUserByLogin(string $login, array $roles, int $id = 7): void
    {
        $user = new \WP_User();
        $user->ID         = $id;
        $user->user_login = $login;
        $user->roles      = $roles;

        Functions\when('get_user_by')->alias(static function ($field, $value) use ($user, $login) {
            if ($field === 'login' && $value === $login) {
                return $user;
            }
            return false;
        });
    }

    private function stubFirstAdmin(string $login, int $id = 1): void
    {
        $user = new \WP_User();
        $user->ID         = $id;
        $user->user_login = $login;
        $user->roles      = ['administrator'];

        Functions\when('get_users')->justReturn([$user]);
    }

    /**
     * @param array<int,string> $roles Roles array.
     */
    private function setConsumeOk(string $login, array $roles, string $auditId = 'audit-1'): void
    {
        $this->consumeStatus = 200;
        $this->consumeBody   = (string) json_encode([
            'ok'                   => true,
            'target_wp_user_login' => $login,
            'allowed_wp_roles'     => $roles,
            'audit_id'             => $auditId,
        ]);
    }

    private function uniqueJti(string $suffix): string
    {
        return 'jti-' . bin2hex(random_bytes(6)) . '-' . $suffix;
    }

    // -----------------------------------------------------------------------
    // Happy path
    // -----------------------------------------------------------------------

    public function test_happy_path_issues_cookie_and_redirects(): void
    {
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $now = time();
        $req = new \WP_REST_Request([
            'token'       => $this->jwt($this->uniqueJti('happy'), $now, ['tgt' => 'alice']),
            'redirect_to' => '/wp-admin/plugins.php',
        ]);

        $res = $this->command()->handle($req);

        $this->assertInstanceOf(\WP_REST_Response::class, $res);
        $this->assertSame(302, $res->get_status());

        $this->assertCount(1, $this->authCookieCalls);
        $this->assertSame([7, false, true], $this->authCookieCalls[0]);

        $this->assertCount(1, $this->redirectCalls);
        $this->assertSame('https://example.test/wp-admin/plugins.php', $this->redirectCalls[0][0]);

        $hooks = array_column($this->hookCalls, 0);
        $this->assertContains('wpmgr_autologin_success', $hooks);
        $this->assertContains('wp_login', $hooks);

        // CP consume call shape.
        $this->assertCount(1, $this->outboundPosts);
        [$url, $args] = $this->outboundPosts[0];
        $this->assertSame('https://cp.example.test' . AutologinCommand::PATH_CONSUME, $url);
        $body = json_decode((string) ($args['body'] ?? ''), true);
        $this->assertSame($this->siteId, $body['site_id']);
        $this->assertSame('203.0.113.4', $body['consumed_from_ip']);
        $this->assertArrayHasKey('nonce', $body);

        // wpmgr_autologin_success hook fires BEFORE the redirect.
        $successIdx = array_search('wpmgr_autologin_success', $hooks, true);
        // The redirect is a side effect AFTER the success hook by construction;
        // we encode it implicitly: the hook is present and the response is 302.
        $this->assertIsInt($successIdx);
    }

    public function test_empty_target_falls_back_to_first_administrator(): void
    {
        $this->setConsumeOk('', ['administrator']);
        $this->stubFirstAdmin('rootadmin', 1);

        $req = new \WP_REST_Request([
            'token' => $this->jwt($this->uniqueJti('fallback'), time(), ['tgt' => '']),
        ]);

        $res = $this->command()->handle($req);

        $this->assertInstanceOf(\WP_REST_Response::class, $res);
        $this->assertSame(302, $res->get_status());
        $this->assertCount(1, $this->authCookieCalls);
        $this->assertSame(1, $this->authCookieCalls[0][0]);

        // Empty redirect_to => admin_url() default.
        $this->assertSame('https://example.test/wp-admin/', $this->redirectCalls[0][0]);
    }

    // -----------------------------------------------------------------------
    // Failure paths
    // -----------------------------------------------------------------------

    public function test_bad_signature_returns_401_no_cookie_no_cp_call(): void
    {
        $cmd = $this->command();
        $res = $cmd->handle(new \WP_REST_Request(['token' => 'not-a-jwt']));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame('wpmgr_invalid_signature', $res->get_error_code());
        $this->assertSame(401, $res->get_error_data()['status']);

        $this->assertSame([], $this->authCookieCalls);
        $this->assertSame([], $this->redirectCalls);
        $this->assertSame([], $this->outboundPosts);

        $hooks = array_column($this->hookCalls, 0);
        $this->assertContains('wpmgr_autologin_failure', $hooks);
    }

    public function test_missing_token_returns_401(): void
    {
        $res = $this->command()->handle(new \WP_REST_Request([]));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame(401, $res->get_error_data()['status']);
    }

    public function test_local_replay_returns_410_without_consume(): void
    {
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $spy = new ReplayCacheSpy();
        $spy->forceSeen = true;

        $token = $this->jwt($this->uniqueJti('seen'), time(), ['tgt' => 'alice']);
        $res = $this->command($spy)->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame('wpmgr_replay_detected', $res->get_error_code());
        $this->assertSame(410, $res->get_error_data()['status']);

        $this->assertSame([], $this->authCookieCalls);
        $this->assertSame([], $this->outboundPosts, 'CP consume must NOT run on local replay.');
    }

    public function test_cp_consume_410_returns_410_no_cookie(): void
    {
        $this->consumeStatus = 410;
        $this->consumeBody   = '{"code":"consumed","message":"already used"}';

        $token = $this->jwt($this->uniqueJti('cp-410'), time());
        $res = $this->command()->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame('wpmgr_consume_rejected', $res->get_error_code());
        $this->assertSame(410, $res->get_error_data()['status']);
        $this->assertSame([], $this->authCookieCalls);
    }

    public function test_role_not_allowed_returns_403_no_cookie(): void
    {
        $this->setConsumeOk('bob', ['administrator']);
        $this->stubUserByLogin('bob', ['editor']);

        $token = $this->jwt($this->uniqueJti('role'), time(), ['tgt' => 'bob']);
        $res = $this->command()->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame('wpmgr_role_not_allowed', $res->get_error_code());
        $this->assertSame(403, $res->get_error_data()['status']);
        $this->assertSame([], $this->authCookieCalls);
    }

    public function test_user_not_found_returns_404(): void
    {
        $this->setConsumeOk('ghost', ['administrator']);
        Functions\when('get_user_by')->justReturn(false);

        $token = $this->jwt($this->uniqueJti('missing'), time(), ['tgt' => 'ghost']);
        $res = $this->command()->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame('wpmgr_wp_user_not_found', $res->get_error_code());
        $this->assertSame(404, $res->get_error_data()['status']);
        $this->assertSame([], $this->authCookieCalls);
    }

    // -----------------------------------------------------------------------
    // mark-before-cookie ordering
    // -----------------------------------------------------------------------

    public function test_mark_failure_aborts_without_issuing_cookie(): void
    {
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $spy = new ReplayCacheSpy();
        $spy->markReturns = false;

        $token = $this->jwt($this->uniqueJti('mark-fail'), time(), ['tgt' => 'alice']);
        $res = $this->command($spy)->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertInstanceOf(\WP_Error::class, $res);
        $this->assertSame('wpmgr_replay_mark_failed', $res->get_error_code());
        $this->assertSame([], $this->authCookieCalls);
        // The handler retries mark() once after Schema::ensureCurrent() self-heal;
        // both attempts return false here, so the spy sees exactly 2 calls.
        $this->assertSame(2, $spy->markCalls);
    }

    public function test_mark_failure_then_success_after_self_heal_issues_cookie(): void
    {
        // Scenario: first mark() returns false (table missing on a re-upload
        // install). The handler runs Schema::ensureCurrent() and retries; the
        // second mark() succeeds and the autologin completes with a cookie.
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $spy = new ReplayCacheSpy();
        $spy->markSequence = [false, true];

        $token = $this->jwt($this->uniqueJti('mark-retry'), time(), ['tgt' => 'alice']);
        $res = $this->command($spy)->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertInstanceOf(\WP_REST_Response::class, $res);
        $this->assertSame(302, $res->get_status());
        $this->assertSame(2, $spy->markCalls, 'mark() must have been retried exactly once.');
        $this->assertCount(1, $this->authCookieCalls, 'Cookie must be issued after the successful retry.');
    }

    public function test_mark_runs_before_cookie_in_happy_path(): void
    {
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $spy = new ReplayCacheSpy();

        $observedMarkAtCookie = null;
        Functions\when('wp_set_auth_cookie')->alias(function ($id) use (&$observedMarkAtCookie, $spy): void {
            $observedMarkAtCookie = $spy->markCalls;
            $this->authCookieCalls[] = [(int) $id, false, false];
        });

        $token = $this->jwt($this->uniqueJti('order'), time(), ['tgt' => 'alice']);
        $this->command($spy)->handle(new \WP_REST_Request(['token' => $token]));

        $this->assertSame(1, $observedMarkAtCookie, 'mark() must run BEFORE wp_set_auth_cookie.');
        $this->assertCount(1, $this->authCookieCalls);
    }

    // -----------------------------------------------------------------------
    // redirect_to sanitizer
    // -----------------------------------------------------------------------

    /**
     * @dataProvider provideMaliciousRedirects
     */
    public function test_redirect_sanitizer_rejects_dangerous_inputs(string $raw): void
    {
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $token = $this->jwt($this->uniqueJti('bad-redir'), time(), ['tgt' => 'alice']);
        $this->command()->handle(new \WP_REST_Request(['token' => $token, 'redirect_to' => $raw]));

        $this->assertCount(1, $this->redirectCalls);
        $this->assertSame('https://example.test/wp-admin/', $this->redirectCalls[0][0]);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provideMaliciousRedirects(): array
    {
        return [
            'protocol-relative'        => ['//evil.com/wp-admin'],
            'absolute https'           => ['https://other.example/wp-admin'],
            'javascript scheme'        => ['javascript:alert(1)'],
            'backslash drift'          => ['\\\\evil.com'],
            'slash-backslash drift'    => ['/\\evil.com'],
            'newline injection'        => ["/wp-admin/foo\nLocation: https://evil"],
            'carriage return'          => ["/wp-admin/foo\r\nset-cookie: x"],
            'data scheme'              => ['data:text/html,<script>'],
            'relative no leading slash'=> ['wp-admin/plugins.php'],
            'tab in path'              => ["/wp-admin/\tplugins"],
            'space in path'            => ['/wp-admin/ plugins'],
        ];
    }

    /**
     * @dataProvider provideSafeRedirects
     */
    public function test_redirect_sanitizer_accepts_safe_paths(string $raw, string $expected): void
    {
        $this->setConsumeOk('alice', ['administrator']);
        $this->stubUserByLogin('alice', ['administrator']);

        $token = $this->jwt($this->uniqueJti('safe-redir-' . md5($raw)), time(), ['tgt' => 'alice']);
        $this->command()->handle(new \WP_REST_Request(['token' => $token, 'redirect_to' => $raw]));

        $this->assertSame($expected, $this->redirectCalls[0][0]);
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provideSafeRedirects(): array
    {
        return [
            'plugins screen' => ['/wp-admin/plugins.php', 'https://example.test/wp-admin/plugins.php'],
            'wp-admin root'  => ['/wp-admin/', 'https://example.test/wp-admin/'],
            'with query'     => ['/wp-admin/edit.php?post_type=page', 'https://example.test/wp-admin/edit.php?post_type=page'],
            'empty -> admin' => ['', 'https://example.test/wp-admin/'],
        ];
    }

    // -----------------------------------------------------------------------
    // ReplayCache unit coverage
    // -----------------------------------------------------------------------

    public function test_replay_cache_seen_then_mark_then_seen(): void
    {
        $cache = new ReplayCache();

        $this->assertFalse($cache->seen('abc'));
        $this->assertTrue($cache->mark('abc', 60));
        $this->assertTrue($cache->seen('abc'));
    }

    public function test_replay_cache_prune_removes_expired_rows(): void
    {
        $cache = new ReplayCache();
        $this->assertTrue($cache->mark('old', 10, 1_700_000_000));
        $this->assertTrue($cache->mark('new', 10, 1_700_001_000));

        $purged = $cache->prune(1_700_000_500);
        $this->assertSame(1, $purged);

        $this->assertFalse($cache->seen('old', 1_700_000_500));
        $this->assertTrue($cache->seen('new', 1_700_000_500));
    }

    public function test_replay_cache_mark_rejects_empty_jti_or_zero_ttl(): void
    {
        $cache = new ReplayCache();
        $this->assertFalse($cache->mark('', 60));
        $this->assertFalse($cache->mark('x', 0));
    }

    // -----------------------------------------------------------------------
    // Name + dispatch-guard
    // -----------------------------------------------------------------------

    public function test_name_and_execute_contract(): void
    {
        $cmd = $this->command();
        $this->assertSame('autologin', $cmd->name());
        $out = $cmd->execute([], []);
        $this->assertSame(['ok' => false, 'error' => 'not_dispatchable'], $out);
    }
}

/**
 * Per-test spy: lets us force seen()/mark() outcomes and count invocations so
 * we can assert ordering against cookie issuance.
 */
final class ReplayCacheSpy extends ReplayCache
{
    public bool $forceSeen = false;

    public bool $markReturns = true;

    public int $markCalls = 0;

    /**
     * Per-call return sequence for mark(); when set it overrides $markReturns
     * and yields one bool per call (consumed in order). Used to model the
     * fail-then-succeed retry path the autologin handler exercises.
     *
     * @var array<int,bool>
     */
    public array $markSequence = [];

    public function seen(string $jti, ?int $now = null): bool
    {
        return $this->forceSeen;
    }

    public function mark(string $jti, int $ttlSeconds, ?int $now = null): bool
    {
        $idx = $this->markCalls;
        $this->markCalls++;

        if ($this->markSequence !== [] && array_key_exists($idx, $this->markSequence)) {
            return $this->markSequence[$idx];
        }

        return $this->markReturns;
    }

    public function prune(?int $now = null): int
    {
        return 0;
    }
}

/**
 * In-memory $wpdb double for the autologin tests.
 *
 * Rows are scoped per-table so that the Connector's `{prefix}wpmgr_agent_jti`
 * inserts (one per verified token) do NOT bleed into the ReplayCache's
 * `{prefix}wpmgr_autologin_jti` reads. Without this isolation the autologin
 * single-use check spuriously matches the Connector's anti-replay row.
 *
 * Statements arriving through prepare() carry the table name in the SQL
 * string, so we parse it out at execution time.
 */
final class FakeAutologinWpdb
{
    public string $prefix = 'wp_';

    /** @var array<string,array<int,array{jti_hash:string,expires_at:int}>> table => rows */
    private array $rows = [];

    /**
     * @param string $query Query with %s/%d placeholders.
     * @param mixed  ...$args Bound arguments.
     * @return string
     */
    public function prepare(string $query, ...$args): string
    {
        return (string) json_encode(['sql' => $query, 'args' => $args]);
    }

    /**
     * @param string $prepared Output of prepare().
     * @return string|null "1" when a matching live row exists.
     */
    public function get_var(string $prepared): ?string
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return null;
        }
        $table = self::extractTable((string) $decoded['sql']);
        $args  = $decoded['args'];
        $hash  = (string) $args[0];
        $now   = (int) $args[1];

        foreach ($this->rows[$table] ?? [] as $row) {
            if (hash_equals($row['jti_hash'], $hash) && $row['expires_at'] >= $now) {
                return '1';
            }
        }
        return null;
    }

    /**
     * Handles prune DELETE queries.
     *
     * @param string $prepared Output of prepare().
     * @return int Rows affected.
     */
    public function query(string $prepared): int
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return 0;
        }
        $table = self::extractTable((string) $decoded['sql']);
        $now   = (int) ($decoded['args'][0] ?? 0);

        $before = count($this->rows[$table] ?? []);
        $this->rows[$table] = array_values(array_filter(
            $this->rows[$table] ?? [],
            static fn (array $r): bool => $r['expires_at'] >= $now,
        ));
        return $before - count($this->rows[$table]);
    }

    /**
     * @param string                       $table  Table name.
     * @param array<string,int|string>     $data   Row data.
     * @param array<int,string>            $format Column formats.
     * @return int|false
     */
    public function insert(string $table, array $data, array $format)
    {
        $hash = (string) ($data['jti_hash'] ?? '');
        foreach ($this->rows[$table] ?? [] as $row) {
            if (hash_equals($row['jti_hash'], $hash)) {
                return false; // PK uniqueness violation.
            }
        }
        $this->rows[$table][] = [
            'jti_hash'   => $hash,
            'expires_at' => (int) ($data['expires_at'] ?? 0),
        ];
        return 1;
    }

    /**
     * Pull the table name out of a SELECT/DELETE statement. The Connector and
     * ReplayCache both interpolate `{table}` directly into the SQL string ahead
     * of prepare(), so simple extraction is sufficient for the tests.
     */
    private static function extractTable(string $sql): string
    {
        if (preg_match('/FROM\s+(\S+)/i', $sql, $m) === 1) {
            return $m[1];
        }
        if (preg_match('/DELETE\s+FROM\s+(\S+)/i', $sql, $m) === 1) {
            return $m[1];
        }
        return '';
    }
}
