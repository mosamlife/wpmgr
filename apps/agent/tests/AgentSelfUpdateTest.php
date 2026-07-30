<?php
/**
 * Tests for the CP-commanded three-beat agent self-update.
 *
 * BEAT 1 (ARM) is UpdateChecker::stageSelfUpdate(), reached through the thin
 * AgentSelfUpdateCommand shell. BEAT 2 (APPLY) is
 * UpdateChecker::applyStagedSelfUpdate(), which runs in a separate WordPress
 * bootstrap. BEAT 3 (CONFIRM) lives in Plugin and is covered by
 * PluginVersionChangedPushTest.
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
use WPMgr\Agent\Support\UpdateChecker;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateChecker
 * @covers \WPMgr\Agent\Commands\AgentSelfUpdateCommand
 */
final class AgentSelfUpdateTest extends TestCase
{
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
        // legitimately rely on it being absent. Only stub what beat 1 and
        // beat 2 actually reach.
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
        Functions\when('spawn_cron')->justReturn(true);

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

    private function makeChecker(): UpdateChecker
    {
        return new UpdateChecker($this->signer, $this->settings, $this->keystore, $this->replayCache);
    }

    // =========================================================================
    // BEAT 1 (ARM)
    // =========================================================================

    /**
     * A signed manifest offering a newer build stages a record, schedules the
     * apply cron, and moves NOTHING on disk. The disk snapshot is the load-
     * bearing assertion here: "scheduled" must mean "nothing has happened yet".
     */
    public function test_stage_schedules_the_apply_cron_and_touches_nothing_on_disk(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $before = $this->snapshotDisk();
        $answer = $this->makeChecker()->stageSelfUpdate();
        $after  = $this->snapshotDisk();

        $this->assertSame('scheduled', $answer['status']);
        $this->assertTrue($answer['ok']);
        $this->assertSame($this->onDiskVersion, $answer['from_version']);
        $this->assertSame($this->targetVersion, $answer['to_version']);
        $this->assertSame('loopback', $answer['cron_mode']);
        $this->assertGreaterThan(time(), $answer['expires_at']);

        $this->assertSame($before, $after, 'ARM must not create, delete or modify a single byte on disk');

        $staged = $this->options[UpdateChecker::OPTION_STAGED] ?? null;
        $this->assertIsArray($staged, 'ARM must persist exactly one staged record');
        $this->assertSame($this->targetVersion, $staged['to_version']);
        $this->assertSame($this->onDiskVersion, $staged['from_version']);
        $this->assertNotSame('', (string) $staged['token']);

        $this->assertCount(1, $this->scheduledEvents, 'ARM must schedule exactly one apply event');
        $this->assertSame(UpdateChecker::HOOK_APPLY, $this->scheduledEvents[0]['hook']);
        $this->assertSame([$staged['token']], $this->scheduledEvents[0]['args'], 'the event must carry the stage token');

        $this->assertCount(1, $this->httpCalls, 'ARM must make exactly one CP call (the manifest fetch) and no package download');
    }

    /**
     * The CP publishing nothing (HTTP 204) is the steady state, not an error.
     */
    public function test_stage_returns_up_to_date_when_the_cp_publishes_nothing(): void
    {
        $this->stubManifestEndpoint(204, '');

        $answer = $this->makeChecker()->stageSelfUpdate();

        $this->assertSame('up_to_date', $answer['status']);
        $this->assertTrue($answer['ok']);
        $this->assertSame('', $answer['to_version']);
        $this->assertArrayNotHasKey(UpdateChecker::OPTION_STAGED, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A correctly signed manifest that is not NEWER than the on-disk build is
     * also up_to_date, not an error: the existing downgrade guard rejects it
     * and beat 1 must classify that rejection as benign.
     */
    public function test_stage_returns_up_to_date_when_the_published_build_is_not_newer(): void
    {
        $claims = $this->makeClaims(['version' => $this->onDiskVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $answer = $this->makeChecker()->stageSelfUpdate();

        $this->assertSame('up_to_date', $answer['status']);
        $this->assertArrayNotHasKey(UpdateChecker::OPTION_STAGED, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A manifest that fails the signature chain is an ERROR, and stages
     * nothing. Distinguishing this from up_to_date is the whole reason the
     * outcome tag exists.
     */
    public function test_stage_returns_error_when_the_manifest_fails_verification(): void
    {
        $claims                = $this->makeClaims();
        $envelope              = $this->signClaims($claims);
        $envelope['signature'] = $this->b64url(random_bytes(SODIUM_CRYPTO_SIGN_BYTES));
        $this->stubManifestEndpoint(200, (string) json_encode($envelope));

        $answer = $this->makeChecker()->stageSelfUpdate();

        $this->assertSame('error', $answer['status']);
        $this->assertFalse($answer['ok']);
        $this->assertArrayNotHasKey(UpdateChecker::OPTION_STAGED, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    /**
     * A second ARM inside the staging window is a no-op: one staged record, one
     * scheduled event, no second CP round trip.
     */
    public function test_stage_refuses_to_stage_twice_inside_the_window(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $checker = $this->makeChecker();
        $first   = $checker->stageSelfUpdate();
        $second  = $checker->stageSelfUpdate();

        $this->assertSame('scheduled', $first['status']);
        $this->assertSame('already_scheduled', $second['status']);
        $this->assertSame($this->targetVersion, $second['to_version']);
        $this->assertCount(1, $this->scheduledEvents, 'a second ARM must not schedule a second apply event');
        $this->assertCount(1, $this->httpCalls, 'a second ARM must not re-hit the CP manifest endpoint');
    }

    /**
     * An un-enrolled site has no control plane to obey.
     */
    public function test_stage_is_not_eligible_when_the_site_is_not_enrolled(): void
    {
        $this->forceUnenrolled = true;
        $this->settings        = new Settings();

        $answer = $this->makeChecker()->stageSelfUpdate();

        $this->assertSame('not_eligible', $answer['status']);
        $this->assertArrayNotHasKey(UpdateChecker::OPTION_STAGED, $this->options);
        $this->assertSame([], $this->scheduledEvents);
    }

    // =========================================================================
    // The command shell
    // =========================================================================

    public function test_command_name_is_agent_self_update(): void
    {
        $this->assertSame('agent_self_update', (new AgentSelfUpdateCommand())->name());
    }

    public function test_command_forwards_the_stage_answer_verbatim(): void
    {
        $claims = $this->makeClaims(['version' => $this->targetVersion]);
        $this->stubManifestEndpoint(200, (string) json_encode($this->signClaims($claims)));

        $result = (new AgentSelfUpdateCommand($this->makeChecker()))->execute([], []);

        $this->assertSame('scheduled', $result['status']);
        $this->assertSame($this->targetVersion, $result['to_version']);
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
    // BEAT 2 (APPLY)
    // =========================================================================

    /**
     * No staged record is the normal state on every site: the apply beat is a
     * silent no-op and records no outcome.
     */
    public function test_apply_is_a_silent_no_op_without_a_staged_record(): void
    {
        $this->makeChecker()->applyStagedSelfUpdate('some-token');

        $this->assertArrayNotHasKey(AgentSelfUpdateCommand::OPTION_RESULT, $this->options);
    }

    /**
     * A site whose loopback cron is broken never reaches beat 2 in time. The
     * record must then be DISCARDED, not retried: no disk was touched at ARM,
     * so an expired stage is safely a non-event that surfaces as unconfirmed.
     */
    public function test_apply_discards_an_expired_staged_record_without_retrying(): void
    {
        $this->options[UpdateChecker::OPTION_STAGED] = [
            'from_version' => '0.10.5',
            'to_version'   => $this->targetVersion,
            'staged_at'    => time() - (UpdateChecker::STAGED_TTL_SECONDS + 120),
            'expires_at'   => time() - 120,
            'token'        => 'stale-token',
        ];

        $this->makeChecker()->applyStagedSelfUpdate('stale-token');

        $this->assertArrayNotHasKey(
            UpdateChecker::OPTION_STAGED,
            $this->options,
            'an expired record must be discarded, never left behind for a later tick to retry'
        );
        $this->assertSame([], $this->scheduledEvents, 'the apply beat must never schedule a retry');

        $result = $this->options[AgentSelfUpdateCommand::OPTION_RESULT] ?? null;
        $this->assertIsArray($result);
        $this->assertSame('expired', $result['status']);
        $this->assertSame($this->targetVersion, $result['to_version']);
    }

    /**
     * The staged record is CLAIMED (deleted) before any upgrade work starts, so
     * an apply that dies partway can never be retried by a later cron tick. The
     * upgrader is deliberately unavailable in this process, which exercises the
     * failure path: the record is gone, a failure is recorded for the next
     * metadata push, and nothing is rescheduled.
     */
    public function test_apply_claims_the_record_before_upgrading_and_never_retries_a_failure(): void
    {
        $this->options[UpdateChecker::OPTION_STAGED] = [
            'from_version' => '0.10.5',
            'to_version'   => $this->targetVersion,
            'staged_at'    => time(),
            'expires_at'   => time() + UpdateChecker::STAGED_TTL_SECONDS,
            'token'        => 'live-token',
        ];

        $before = $this->snapshotDisk();
        $this->makeChecker()->applyStagedSelfUpdate('live-token');
        $after = $this->snapshotDisk();

        $this->assertSame($before, $after);
        $this->assertArrayNotHasKey(
            UpdateChecker::OPTION_STAGED,
            $this->options,
            'the record must be claimed and deleted before the upgrade, so a mid-copy death cannot loop'
        );
        $this->assertSame([], $this->scheduledEvents, 'a failed apply must not schedule a retry');

        $result = $this->options[AgentSelfUpdateCommand::OPTION_RESULT] ?? null;
        $this->assertIsArray($result);
        $this->assertSame('failed', $result['status']);
        $this->assertSame($this->targetVersion, $result['to_version']);
    }

    /**
     * A stale duplicate event from an earlier stage must not consume the record
     * belonging to the CURRENT stage.
     */
    public function test_apply_ignores_an_event_whose_token_does_not_match_the_record(): void
    {
        $record = [
            'from_version' => '0.10.5',
            'to_version'   => $this->targetVersion,
            'staged_at'    => time(),
            'expires_at'   => time() + UpdateChecker::STAGED_TTL_SECONDS,
            'token'        => 'current-token',
        ];
        $this->options[UpdateChecker::OPTION_STAGED] = $record;

        $this->makeChecker()->applyStagedSelfUpdate('token-from-an-earlier-stage');

        $this->assertSame($record, $this->options[UpdateChecker::OPTION_STAGED]);
        $this->assertArrayNotHasKey(AgentSelfUpdateCommand::OPTION_RESULT, $this->options);
    }

    /**
     * A target the site is already running is discarded rather than reinstalled.
     */
    public function test_apply_discards_a_record_whose_target_is_already_on_disk(): void
    {
        $this->options[UpdateChecker::OPTION_STAGED] = [
            'from_version' => '0.10.4',
            'to_version'   => $this->onDiskVersion,
            'staged_at'    => time(),
            'expires_at'   => time() + UpdateChecker::STAGED_TTL_SECONDS,
            'token'        => 'live-token',
        ];

        $this->makeChecker()->applyStagedSelfUpdate('live-token');

        $this->assertArrayNotHasKey(UpdateChecker::OPTION_STAGED, $this->options);
        $result = $this->options[AgentSelfUpdateCommand::OPTION_RESULT] ?? null;
        $this->assertIsArray($result);
        $this->assertSame('already_applied', $result['status']);
    }

    /**
     * The apply outcome reaches the control plane through the next metadata
     * push, which is the ONLY channel a cron-side failure has.
     */
    public function test_metadata_payload_carries_the_last_apply_outcome(): void
    {
        Functions\when('is_multisite')->justReturn(false);

        $collector = new \WPMgr\Agent\Commands\MetadataCommand();
        $this->assertArrayNotHasKey(
            'agent_self_update',
            $collector->collect(),
            'a site that never staged a self-update must not carry the key at all'
        );

        $this->options[AgentSelfUpdateCommand::OPTION_RESULT] = [
            'status'       => 'failed',
            'from_version' => '0.10.5',
            'to_version'   => $this->targetVersion,
            'detail'       => 'Upgrader API unavailable.',
            'at'           => time(),
        ];

        $payload = $collector->collect();
        $this->assertArrayHasKey('agent_self_update', $payload);
        $this->assertSame('failed', $payload['agent_self_update']['status']);
        $this->assertSame($this->targetVersion, $payload['agent_self_update']['to_version']);
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
            'failed',
            '0.10.5',
            $this->targetVersion,
            'download failed: https://storage.googleapis.com/bucket/a.zip?X-Goog-Signature=deadbeef'
        );

        $detail = (string) $this->options[AgentSelfUpdateCommand::OPTION_RESULT]['detail'];
        $this->assertStringNotContainsString('X-Goog-Signature', $detail);
        $this->assertStringNotContainsString('https://', $detail);
        $this->assertStringContainsString('[url]', $detail);
    }
}
