<?php
/**
 * Tests for the ARM beat of the CP-commanded agent self-update.
 *
 * ARM is UpdateChecker::planSelfUpdate(), reached through the thin
 * AgentSelfUpdateCommand shell. The APPLY that follows it in the same request
 * is covered by SelfUpdateInRequestApplyTest. CONFIRM lives in Plugin and is
 * covered by PluginVersionChangedPushTest.
 *
 * The fixture mirrors UpdateCheckerTest: a real per-test Ed25519 keypair so the
 * signature chain is exercised authentically, Brain Monkey for the WordPress
 * function surface, and a configurable ReplayCache double.
 *
 * The load-bearing assertion of the "scheduled" case is that ARM touches
 * NOTHING on disk. It is asserted structurally (a hashed snapshot of a fixture
 * tree taken before and after the call) rather than by trusting the return
 * value.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\AgentSelfUpdateCommand;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\ReplayCache;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\ConnectionFinisher;
use WPMgr\Agent\Support\UpdateChecker;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateChecker
 * @covers \WPMgr\Agent\Commands\AgentSelfUpdateCommand
 */
final class AgentSelfUpdateTest extends TestCase
{
    /**
     * The wp-option core's WP_Upgrader lock writes. Named by literal because the
     * agent's lock-name constant is private and this is the row the agent has to
     * be observed taking.
     */
    private const LOCK_OPTION = 'wpmgr_agent_self_update.lock';

    /** Raw 64-byte CP Ed25519 secret key (for signing manifests). */
    private string $cpSecret;

    /** Raw 32-byte CP Ed25519 public key (stored in Keystore). */
    private string $cpPublic;

    /** The enrolled site_id for this test run. */
    private string $siteId = 'test-site-uuid-selfupdate';

    /** On-disk plugin version (simulates the current installed version). */
    private string $onDiskVersion = '0.10.5';

    /**
     * The version the CP offers: always one numeric segment above the on-disk
     * core, so the fixture stays valid whatever WPMGR_AGENT_VERSION another
     * test file in this process pinned first. Derived from the NORMALIZED core
     * because a dev suffix never participates in the comparison.
     */
    private string $targetVersion = '';

    private Keystore $keystore;
    private Settings $settings;
    private Signer $signer;

    /** @var array<string,mixed> wp-option store. */
    private array $options = [];

    /** @var array<string,mixed> site_transient store. */
    private array $siteTransients = [];

    /** @var list<array{hook:string,args:array<int,mixed>,timestamp:int}> */
    private array $scheduledEvents = [];

    /** @var list<string> Every URL handed to wp_remote_get during a test. */
    private array $httpCalls = [];

    /** Temporary key file for the Keystore master key. */
    private string $keyFile = '';

    /** Fixture tree standing in for the on-disk plugin directory. */
    private string $diskFixture = '';

    /** Fake ReplayCache instance. */
    private object $replayCache;

    /** Set true to make the site look un-enrolled. */
    public bool $forceUnenrolled = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-asu-test-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
        file_put_contents($this->keyFile, random_bytes(32));

        $cpKeypair      = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($cpKeypair);
        $this->cpPublic = sodium_crypto_sign_publickey($cpKeypair);

        $this->options         = [];
        $this->siteTransients  = [];
        $this->scheduledEvents = [];
        $this->httpCalls       = [];
        $this->forceUnenrolled = false;
        unset($GLOBALS['wpdb']);

        Functions\when('get_option')->alias(function (string $name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('update_option')->alias(function (string $name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('delete_option')->alias(function (string $name) {
            unset($this->options[$name]);
            return true;
        });
        // NOTE: get_site_transient is deliberately NOT stubbed. Brain Monkey
        // defines a stubbed function process-wide for the remainder of the
        // PHPUnit run, and this file sorts FIRST in the suite, so stubbing a
        // function nothing on these code paths calls would flip
        // function_exists() to true inside unrelated, later tests that
        // legitimately rely on it being absent. Only stub what the ARM beat
        // actually reaches. The apply, which does read that transient, is
        // exercised in SelfUpdateInRequestApplyTest, which sorts after the
        // files that already stub it.
        Functions\when('set_site_transient')->alias(function (string $key, $value, int $ttl = 0) {
            $this->siteTransients[$key] = $value;
            return true;
        });
        Functions\when('delete_site_transient')->alias(function (string $key) {
            unset($this->siteTransients[$key]);
            return true;
        });
        Functions\when('wp_parse_url')->alias(fn (string $url) => parse_url($url));
        Functions\when('get_plugin_data')->alias(fn () => ['Version' => $this->onDiskVersion]);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_clear_scheduled_hook')->justReturn(1);

        // Kept purely so the assertions below can prove the arm schedules
        // NOTHING. The apply is no longer an event, and a stub that records a
        // call nobody makes is exactly how that stays true.
        Functions\when('wp_schedule_single_event')->alias(
            function (int $timestamp, string $hook, array $args = []) {
                $this->scheduledEvents[] = ['hook' => $hook, 'args' => $args, 'timestamp' => $timestamp];
                return true;
            }
        );

        // PHP constants are process-global and cannot be undefined, so another
        // file in this (non-isolated) suite may already have pinned the agent
        // version. Adopt whatever won rather than asserting against a literal,
        // and keep the get_plugin_data() stub in agreement with it so
        // onDiskVersion() answers the same value down either of its two paths.
        //
        // WPMGR_AGENT_FILE is deliberately NOT defined here: this file sorts
        // FIRST in the suite, and pinning that constant would change what later
        // tests (which guard-define it to their own __FILE__) observe.
        if (!defined('WPMGR_AGENT_VERSION')) {
            define('WPMGR_AGENT_VERSION', $this->onDiskVersion);
        }
        $this->onDiskVersion = (string) constant('WPMGR_AGENT_VERSION');
        $this->targetVersion = (string) preg_replace('/[-+].*$/', '', $this->onDiskVersion) . '.1';
        if (!defined('HOUR_IN_SECONDS')) {
            define('HOUR_IN_SECONDS', 3600);
        }

        $this->keystore = new Keystore();
        $this->keystore->storeControlPlanePublicKey($this->cpPublic);

        Functions\when('get_site_option')->alias(function (string $key, $default = null) {
            if ($this->forceUnenrolled) {
                return $default === null ? false : $default;
            }
            if ($key === Settings::OPTION_SITE_ID) {
                return $this->siteId;
            }
            if ($key === Settings::OPTION_CP_URL) {
                return 'https://cp.example.com';
            }
            return $default === null ? false : $default;
        });

        $this->settings = new Settings();
        $this->keystore->generateSiteKeypair();
        $this->signer = new Signer($this->keystore);

        $self = $this;
        $this->replayCache = new class($self) extends ReplayCache {
            private AgentSelfUpdateTest $test;
            public function __construct(AgentSelfUpdateTest $test)
            {
                $this->test = $test;
            }
            public function seen(string $jti, ?int $now = null): bool
            {
                return false;
            }
            public function mark(string $jti, int $ttlSeconds, ?int $now = null): bool
            {
                return true;
            }
        };

        // A small tree standing in for the on-disk plugin directory, so the
        // "ARM touches nothing on disk" assertion is structural.
        $this->diskFixture = sys_get_temp_dir() . '/wpmgr-asu-disk-' . bin2hex(random_bytes(6));
        mkdir($this->diskFixture . '/includes', 0755, true);
        file_put_contents($this->diskFixture . '/wpmgr-agent.php', "<?php // v{$this->onDiskVersion}\n");
        file_put_contents($this->diskFixture . '/includes/class-plugin.php', "<?php // plugin\n");
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        if ($this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }
        $this->rrmdir($this->diskFixture);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }

    /**
     * Content-addressed snapshot of the fixture tree: relative path -> sha256.
     *
     * @return array<string,string>
     */
    private function snapshotDisk(): array
    {
        $out      = [];
        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($this->diskFixture, \FilesystemIterator::SKIP_DOTS)
        );
        /** @var \SplFileInfo $file */
        foreach ($iterator as $file) {
            if (!$file->isFile()) {
                continue;
            }
            $rel       = substr($file->getPathname(), strlen($this->diskFixture) + 1);
            $out[$rel] = (string) hash_file('sha256', $file->getPathname());
        }
        ksort($out);

        return $out;
    }

    /**
     * Build a valid manifest claims array with safe defaults.
     *
     * @param array<string,mixed> $overrides Fields to override.
     * @return array<string,mixed>
     */
    private function makeClaims(array $overrides = []): array
    {
        $now = time();

        return array_merge([
            'aud'            => $this->siteId,
            'cmd'            => 'update_manifest',
            'slug'           => 'wpmgr-agent',
            'version'        => $this->targetVersion,
            'min_version'    => '0.0.0',
            'package_url'    => 'https://storage.googleapis.com/wpmgr-chunks-prod/agent-releases/0.11.0/wpmgr-agent.zip?sig=xxx',
            'package_sha256' => str_repeat('ab', 32),
            'package_size'   => 359578,
            'requires'       => '6.0',
            'requires_php'   => '8.1',
            'tested'         => '6.8',
            'iat'            => $now,
            'exp'            => $now + 300,
            'jti'            => bin2hex(random_bytes(16)),
        ], $overrides);
    }

    /**
     * Sign a claims array and return the wire envelope.
     *
     * @param array<string,mixed> $claims Claims to sign.
     * @return array{manifest:string, signature:string}
     */
    private function signClaims(array $claims): array
    {
        $payloadRaw = (string) json_encode($claims);
        $sigRaw     = sodium_crypto_sign_detached($payloadRaw, $this->cpSecret);

        return [
            'manifest'  => $this->b64url($payloadRaw),
            'signature' => $this->b64url($sigRaw),
        ];
    }

    /** URL-safe base64 no-padding encode. */
    private function b64url(string $bytes): string
    {
        return rtrim(strtr(base64_encode($bytes), '+/', '-_'), '=');
    }

    /**
     * Stub the CP manifest endpoint with the given HTTP status and body.
     *
     * @param int    $status HTTP status the manifest endpoint returns.
     * @param string $body   Response body.
     * @return void
     */
    private function stubManifestEndpoint(int $status, string $body): void
    {
        Functions\when('wp_remote_get')->alias(function (string $url, array $args = []) use ($status, $body) {
            $this->httpCalls[] = $url;
            return ['response' => ['code' => $status], 'body' => $body, 'filename' => ''];
        });
        Functions\when('wp_remote_retrieve_response_code')->alias(
            fn ($response) => $response['response']['code'] ?? 0
        );
        Functions\when('wp_remote_retrieve_body')->alias(fn ($response) => $response['body'] ?? '');
    }

    /**
     * Build an UpdateChecker whose SAPI probe is injected.
     *
     * The arm refuses outright on a SAPI that cannot release the
     * control-plane connection, and under the PHPUnit CLI SAPI neither
     * fastcgi_finish_request nor litespeed_finish_request exists, so an
     * un-injected checker would answer "not_eligible" to every arm in this
     * file. Neither function can be DEFINED here either: that flips
     * function_exists() process-wide and changes what every other test in the
     * suite observes. So the probe is handed in, exactly as ConnectionFinisher's
     * own tests hand it in, and the refusal gets its own test below.
     *
     * @param bool $detachable Whether the fake SAPI exposes a detaching rung.
     * @return UpdateChecker
     */
    private function makeChecker(bool $detachable = true): UpdateChecker
    {
        $finisher = new ConnectionFinisher(
            static fn (string $fn): bool => $detachable && $fn === 'fastcgi_finish_request',
            static function (string $fn): void {
            },
            static function (): void {
            }
        );

        return new UpdateChecker($this->signer, $this->settings, $this->keystore, $this->replayCache, $finisher);
    }

    // =========================================================================
    // ARM
    // =========================================================================

    /**
     * A signed manifest offering a newer build takes the apply lock, registers
     * the in-request apply, and moves NOTHING on disk. The disk snapshot is the
     * load-bearing assertion here: "scheduled" must mean "nothing has happened
     * yet", and it stays true even now that the apply runs later in this same
     * request, because the arm returns long before the response is released.
     *
     * The registration is asserted as a LITERAL hook name. That hook is the
     * whole mechanism: rest_pre_serve_request runs in the request BODY, before
     * do_action('shutdown') has started, which is the only position from which
     * core's own temp-backup rollback is still dispatchable.
     */
    public function test_the_arm_touches_nothing_on_disk_and_registers_the_in_request_apply(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $checker = $this->makeChecker();

        $before = $this->snapshotDisk();
        $answer = $checker->planSelfUpdate();
        $after  = $this->snapshotDisk();

        $this->assertSame('scheduled', $answer['status']);
        $this->assertTrue($answer['ok']);
        $this->assertSame($this->onDiskVersion, $answer['from_version']);
        $this->assertSame($this->targetVersion, $answer['to_version']);
        $this->assertSame('loopback', $answer['cron_mode']);
        $this->assertGreaterThan(time(), $answer['expires_at']);
        $this->assertMatchesRegularExpression(
            '/^[0-9a-f]{32}$/',
            (string) $answer['apply_id'],
            'a scheduled arm must mint an apply id, or nothing it does can ever be attributed to it'
        );

        $this->assertSame($before, $after, 'ARM must not create, delete or modify a single byte on disk');

        $this->assertTrue(
            has_filter('rest_pre_serve_request', [$checker, 'serveThenApply']) !== false,
            'the apply must be registered on rest_pre_serve_request, which runs in the request body'
        );
        $this->assertSame([], $this->scheduledEvents, 'the apply is no longer an event; nothing may be scheduled');

        $this->assertNotSame(
            0,
            (int) ($this->options[self::LOCK_OPTION] ?? 0),
            'the arm must hold core\'s own upgrader lock for the duration of the apply'
        );

        $this->assertCount(1, $this->httpCalls, 'ARM must make exactly one CP call (the manifest fetch) and no package download');
    }

    /**
     * The CP publishing nothing (HTTP 204) is the steady state, not an error.
     */
    public function test_arm_returns_up_to_date_when_the_cp_publishes_nothing(): void
    {
        $this->stubManifestEndpoint(204, '');

        $answer = $this->makeChecker()->planSelfUpdate();

        $this->assertSame('up_to_date', $answer['status']);
        $this->assertTrue($answer['ok']);
        $this->assertSame('', $answer['to_version']);
        $this->assertSame('', $answer['apply_id']);
        $this->assertArrayNotHasKey(self::LOCK_OPTION, $this->options, 'an up-to-date site must not take the apply lock');
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A correctly signed manifest that is not NEWER than the on-disk build is
     * also up_to_date, not an error: the existing downgrade guard rejects it
     * and the arm must classify that rejection as benign.
     */
    public function test_arm_returns_up_to_date_when_the_published_build_is_not_newer(): void
    {
        $claims = $this->makeClaims(['version' => $this->onDiskVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $answer = $this->makeChecker()->planSelfUpdate();

        $this->assertSame('up_to_date', $answer['status']);
        $this->assertArrayNotHasKey(self::LOCK_OPTION, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A manifest that fails the signature chain is an ERROR, and arms nothing.
     * Distinguishing this from up_to_date is the whole reason the outcome tag
     * exists.
     */
    public function test_arm_returns_error_when_the_manifest_fails_verification(): void
    {
        $claims                = $this->makeClaims();
        $envelope              = $this->signClaims($claims);
        $envelope['signature'] = $this->b64url(random_bytes(SODIUM_CRYPTO_SIGN_BYTES));
        $this->stubManifestEndpoint(200, (string) json_encode($envelope));

        $answer = $this->makeChecker()->planSelfUpdate();

        $this->assertSame('error', $answer['status']);
        $this->assertFalse($answer['ok']);
        $this->assertSame('', $answer['apply_id']);
        $this->assertArrayNotHasKey(self::LOCK_OPTION, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A second arm while the lock is held is NOT a second apply.
     *
     * This replaces two separate tests (a second arm inside the staging window,
     * and a retry after a scheduling failure) because there is now exactly one
     * mechanism behind both: core's own upgrader lock, one atomic INSERT IGNORE,
     * which is what the hand-rolled claim, in-flight marker and stage token were
     * all approximating.
     *
     * The answer must still name the apply that holds the lock. An
     * already_scheduled that carries no apply id leaves the control plane with
     * nothing to attribute a later version move to, and confirming on version
     * movement alone is precisely the unproven confirmation this channel exists
     * to stop making.
     */
    public function test_a_second_arm_while_the_lock_is_held_answers_already_scheduled(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $checker = $this->makeChecker();
        $first   = $checker->planSelfUpdate();
        $second  = $checker->planSelfUpdate();

        $this->assertSame('scheduled', $first['status']);
        $this->assertSame('already_scheduled', $second['status']);
        $this->assertSame($this->targetVersion, $second['to_version']);
        $this->assertGreaterThan(time(), $second['expires_at']);
        $this->assertSame(
            $first['apply_id'],
            $second['apply_id'],
            'the second arm must name the apply that actually holds the lock, not a fresh id and not an empty one'
        );
        $this->assertCount(2, $this->httpCalls, 'the second arm still verifies a manifest; only the apply is refused');
    }

    /**
     * An un-enrolled site has no control plane to obey.
     */
    public function test_arm_is_not_eligible_when_the_site_is_not_enrolled(): void
    {
        $this->forceUnenrolled = true;
        $this->settings        = new Settings();

        $answer = $this->makeChecker()->planSelfUpdate();

        $this->assertSame('not_eligible', $answer['status']);
        $this->assertArrayNotHasKey(self::LOCK_OPTION, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A SITE WHOSE PHP SAPI CANNOT DETACH ARMS EXACTLY LIKE ANY OTHER SITE.
     *
     * The arm used to refuse such a host outright, answering "not_eligible" and
     * recording a "sapi_cannot_detach" outcome, on the reasoning that an
     * upgrade on a still-attached connection could be cut mid swap. That
     * refusal is gone. WordPress has no SAPI check anywhere in its own upgrade
     * path and updates plugins on these hosts from wp-admin every day, and this
     * agent's own update command already applies plugin, theme and core
     * upgrades inline and attached on every SAPI. Refusing here stranded a
     * large share of shared hosting on an agent that could never be upgraded
     * from the fleet.
     *
     * So the fixture is the same manifest and the same site as the ordinary arm
     * test, with only the SAPI probe flipped, and the expected answer is the
     * same one: scheduled, the lock taken, the seam registered, a real apply id,
     * and NO apply-result record, because at arm time nothing has been applied.
     *
     * "not_eligible" now has exactly two meanings, both about identity rather
     * than capability: the wordpress.org distribution build, and an un-enrolled
     * site. Each has its own test above.
     */
    public function test_a_sapi_that_cannot_detach_still_arms(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $checker = $this->makeChecker(false);

        $before = $this->snapshotDisk();
        $answer = $checker->planSelfUpdate();

        $this->assertSame(
            'scheduled',
            $answer['status'],
            'the SAPI is not a reason to decline; a host that cannot detach applies attached instead'
        );
        $this->assertTrue($answer['ok']);
        $this->assertSame($this->targetVersion, $answer['to_version']);
        $this->assertMatchesRegularExpression(
            '/^[0-9a-f]{32}$/',
            (string) $answer['apply_id'],
            'an armed apply must be attributable on every host class'
        );
        $this->assertGreaterThan(time(), $answer['expires_at']);

        $this->assertSame($before, $this->snapshotDisk(), 'the ARM still touches nothing on disk');
        $this->assertCount(1, $this->httpCalls, 'the arm verifies the manifest here exactly as anywhere else');
        $this->assertNotSame(
            0,
            (int) ($this->options[self::LOCK_OPTION] ?? 0),
            'the arm must hold core\'s own upgrader lock on this host class too'
        );
        $this->assertNotFalse(
            has_filter('rest_pre_serve_request', [$checker, 'serveThenApply']),
            'the apply seam must be registered, or the swap this arm just promised can never run'
        );

        $this->assertArrayNotHasKey(
            AgentSelfUpdateCommand::OPTION_RESULT,
            $this->options,
            'the arm applies nothing, so it must record no apply outcome at all'
        );
    }

    /**
     * The refused-outcome vocabulary is gone from the shipped source, not just
     * from the path that used to reach it. A leftover constant or literal is how
     * a retired refusal quietly comes back, and this string was also visible to
     * operators, so it is pinned as absent rather than assumed absent.
     */
    public function test_the_retired_sapi_refusal_vocabulary_is_gone_from_the_source(): void
    {
        $source = (string) file_get_contents(dirname(__DIR__) . '/includes/support/class-update-checker.php');

        $this->assertStringNotContainsString('sapi_cannot_detach', $source);
        $this->assertStringNotContainsString('RESULT_SAPI_CANNOT_DETACH', $source);
        $this->assertStringNotContainsString('DETACH_REFUSAL_DETAIL', $source);
        $this->assertStringNotContainsString(
            'canDetach',
            $source,
            'the self-updater must not ask whether it may detach; it applies either way'
        );
    }

    /**
     * A site arriving from the retired cron-and-stage design sheds its leftovers
     * on the first arm, and it does so BEFORE the manifest fetch. Once a fleet
     * is current the common answer is up_to_date, which returns early, so a
     * cleanup placed after that return would never run on the sites most likely
     * to still be carrying an autoloaded staged row.
     */
    public function test_the_arm_sheds_the_retired_staging_state_even_when_up_to_date(): void
    {
        $this->options['wpmgr_agent_self_update_staged']   = ['to_version' => '9.9.9'];
        $this->options['wpmgr_agent_self_update_applying'] = time();

        /** @var list<string> $cleared */
        $cleared = [];
        Functions\when('wp_clear_scheduled_hook')->alias(function (string $hook) use (&$cleared): int {
            $cleared[] = $hook;

            return 1;
        });

        $this->stubManifestEndpoint(204, '');

        $answer = $this->makeChecker()->planSelfUpdate();

        $this->assertSame('up_to_date', $answer['status']);
        $this->assertArrayNotHasKey('wpmgr_agent_self_update_staged', $this->options);
        $this->assertArrayNotHasKey('wpmgr_agent_self_update_applying', $this->options);
        $this->assertContains('wpmgr_agent_self_update_apply', $cleared);
    }

    // =========================================================================
    // The command shell
    // =========================================================================

    public function test_command_name_is_agent_self_update(): void
    {
        $this->assertSame('agent_self_update', (new AgentSelfUpdateCommand())->name());
    }

    public function test_command_forwards_the_arm_answer_verbatim(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $result = (new AgentSelfUpdateCommand($this->makeChecker()))->execute([], []);

        $this->assertSame('scheduled', $result['status']);
        $this->assertSame($this->targetVersion, $result['to_version']);
        $this->assertNotSame('', (string) $result['apply_id']);
    }

    /**
     * A null self-updater (exactly what Plugin passes on the wp.org build) is
     * answered, never fataled. The in-process counterpart to
     * AgentSelfUpdateWporgBuildTest, which proves the same thing in a process
     * where the class genuinely does not exist.
     */
    public function test_command_answers_not_eligible_when_no_self_updater_is_injected(): void
    {
        $result = (new AgentSelfUpdateCommand(null))->execute([], []);

        $this->assertSame('not_eligible', $result['status']);
        $this->assertTrue($result['ok']);
        $this->assertSame('', $result['to_version']);
    }

    /**
     * This command takes no parameters, so an unexpected one is IGNORED and
     * cannot change the answer. It certainly cannot be read as a target
     * version: the version is whatever the signed manifest says, and a body key
     * is not a signed input. Rejecting the body instead is what halted a
     * production rollout wave, so the tolerance is the contract now.
     */
    public function test_command_ignores_unexpected_parameters(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $result = (new AgentSelfUpdateCommand($this->makeChecker()))->execute([], ['version' => '9.9.9']);

        $this->assertSame('scheduled', $result['status']);
        $this->assertSame(
            $this->targetVersion,
            $result['to_version'],
            'the target version comes from the signed manifest, never from the request body'
        );
    }

    // =========================================================================
    // What reaches the control plane
    // =========================================================================

    /**
     * The apply outcome reaches the control plane through the next metadata
     * push, which is the ONLY channel a failed apply has.
     */
    public function test_metadata_payload_carries_the_last_apply_outcome(): void
    {
        Functions\when('is_multisite')->justReturn(false);

        $collector = new \WPMgr\Agent\Commands\MetadataCommand();
        $this->assertArrayNotHasKey(
            'agent_self_update',
            $collector->collect(),
            'a site that never armed a self-update must not carry the key at all'
        );

        $this->options[AgentSelfUpdateCommand::OPTION_RESULT] = [
            'status'       => 'failed',
            'from_version' => '0.10.5',
            'to_version'   => $this->targetVersion,
            'detail'       => 'Upgrader API unavailable.',
            'at'           => time(),
            'apply_id'     => 'ff00ff00ff00ff00ff00ff00ff00ff00',
        ];

        $payload = $collector->collect();
        $this->assertArrayHasKey('agent_self_update', $payload);
        $this->assertSame('failed', $payload['agent_self_update']['status']);
        $this->assertSame($this->targetVersion, $payload['agent_self_update']['to_version']);
    }

    /**
     * THE METADATA PAYLOAD IS THE ONLY PRODUCER OF THIS WIRE FIELD, AND IT IS A
     * WHITELIST.
     *
     * MetadataCommand rebuilds the record key by key before Enrollment signs it,
     * so anything the self-updater stores that this list does not name is
     * dropped on the floor. That is not a theoretical hazard: with apply_id
     * missing from the list, every apply reaches the control plane
     * unattributable, a perfectly successful upgrade is recorded as unproven,
     * and a canary that upgraded exactly as asked halts its own rollout. Pin the
     * keys, in both directions, so adding a field to the record and forgetting
     * this list fails here.
     */
    public function test_the_metadata_payload_pins_the_self_update_wire_keys(): void
    {
        Functions\when('is_multisite')->justReturn(false);

        $this->options[AgentSelfUpdateCommand::OPTION_RESULT] = [
            'status'       => 'applied',
            'from_version' => '0.10.5',
            'to_version'   => $this->targetVersion,
            'detail'       => '',
            'at'           => 1750000000,
            'apply_id'     => 'abcdef0123456789abcdef0123456789',
            'rung'         => 'fallback',
            'ignored_key'  => 'must not travel',
        ];

        $payload = (new \WPMgr\Agent\Commands\MetadataCommand())->collect();

        $this->assertSame(
            ['status', 'from_version', 'to_version', 'detail', 'at', 'apply_id', 'rung'],
            array_keys($payload['agent_self_update']),
            'the agent_self_update wire shape is pinned; change it here and in the control plane contract together'
        );
        $this->assertSame(
            'abcdef0123456789abcdef0123456789',
            $payload['agent_self_update']['apply_id'],
            'apply_id must survive the whitelist, or attribution is impossible by construction'
        );
        $this->assertSame(
            'fallback',
            $payload['agent_self_update']['rung'],
            'the rung is how a fleet-wide reading tells an attached apply from a released one; it must survive too'
        );
    }

    /**
     * An agent that predates apply-id stamping stored no such key. Its record
     * must still travel, with an empty apply id, which the control plane reads
     * as "cannot be attributed" rather than as a malformed payload.
     */
    public function test_a_record_from_an_older_agent_travels_with_an_empty_apply_id(): void
    {
        Functions\when('is_multisite')->justReturn(false);

        $this->options[AgentSelfUpdateCommand::OPTION_RESULT] = [
            'status'       => 'failed',
            'from_version' => '0.61.106',
            'to_version'   => '0.61.107',
            'detail'       => 'Upgrader reported no result.',
            'at'           => 1750000000,
        ];

        $payload = (new \WPMgr\Agent\Commands\MetadataCommand())->collect();

        $this->assertSame('failed', $payload['agent_self_update']['status']);
        $this->assertSame('', $payload['agent_self_update']['apply_id']);
        $this->assertSame(
            '',
            $payload['agent_self_update']['rung'],
            'the rung is additive in the same way: absent on an older agent, never a decode failure'
        );
    }

    /**
     * The recorded detail must never carry a URL: upgrader and skin messages
     * can echo a package string back, and the manifest's package_url is a
     * short-lived bearer credential.
     */
    public function test_recorded_failure_detail_is_scrubbed_of_urls(): void
    {
        $checker = $this->makeChecker();
        $method  = new \ReflectionMethod($checker, 'recordSelfUpdateResult');
        $method->invoke(
            $checker,
            'apply-id-under-test',
            'failed',
            '0.10.5',
            $this->targetVersion,
            'download failed: https://storage.googleapis.com/bucket/a.zip?X-Goog-Signature=deadbeef'
        );

        $record = $this->options[AgentSelfUpdateCommand::OPTION_RESULT];
        $detail = (string) $record['detail'];
        $this->assertStringNotContainsString('X-Goog-Signature', $detail);
        $this->assertStringNotContainsString('https://', $detail);
        $this->assertStringContainsString('[url]', $detail);
        $this->assertSame('apply-id-under-test', $record['apply_id']);
    }
}
