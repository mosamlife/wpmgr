<?php
/**
 * THE APPLY of the CP-commanded agent self-update, which now runs inside the
 * signed command request, from core's own rest_pre_serve_request filter, after
 * the acknowledgement has been released.
 *
 * WHY THAT POSITION IS THE WHOLE CHANGE. The apply used to run from a shutdown
 * callback at priority 9997. WP_Upgrader::run() registers its rollback,
 * restore_temp_backup, on shutdown at priority 10 and its cleanup,
 * delete_temp_backup, at 100. A callback added to a hook that is ALREADY being
 * dispatched cannot be reached at a priority the dispatcher has gone past, so
 * from 9997 both were skipped outright and a failed swap had nothing left to put
 * the directory back. rest_pre_serve_request fires while do_action('shutdown')
 * has not started at all, so a callback registered at priority 10 during the
 * apply is dispatched normally afterwards.
 *
 * That property is pinned below by replaying the shutdown queue in priority
 * order. This suite has no WordPress core to dispatch through (the bundled tree
 * under tools/plugincheck is developer-only and is not present in CI), so the
 * replay is the agent's own: what it proves is that the apply completes BEFORE
 * the shutdown queue begins, and that a priority 10 callback registered during
 * it is still ahead of the agent's own guard at 101. The re-entrancy behaviour
 * of core's WP_Hook itself is core's, and was verified against the real class
 * when this position was chosen.
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
use WPMgr\Agent\Support\LongRunningJob;
use WPMgr\Agent\Support\UpdateChecker;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateChecker
 */
final class SelfUpdateInRequestApplyTest extends TestCase
{
    /** The wp-option core's WP_Upgrader lock writes for this channel. */
    private const LOCK_OPTION = 'wpmgr_agent_self_update.lock';

    /** Raw 64-byte CP Ed25519 secret key. */
    private string $cpSecret = '';

    /** Raw 32-byte CP Ed25519 public key. */
    private string $cpPublic = '';

    /** The enrolled site_id for this test run. */
    private string $siteId = 'test-site-uuid-inrequest';

    /** On-disk plugin version. */
    private string $onDiskVersion = '0.10.5';

    /** The version the CP offers. */
    private string $targetVersion = '';

    private Keystore $keystore;
    private Settings $settings;
    private Signer $signer;

    /** @var array<string,mixed> wp-option store. */
    private array $options = [];

    /** @var array<string,mixed> site_transient store. */
    private array $siteTransients = [];

    /** @var list<array{hook:string,priority:int,callback:mixed}> Every add_action() seen. */
    private array $actions = [];

    /** @var list<string> Filters added, as "hook@priority". */
    private array $filters = [];

    /** @var list<int> Arguments handed to set_time_limit(). */
    private array $timeLimits = [];

    /** @var list<string> Actions fired through do_action(). */
    private array $firedActions = [];

    /** Which ConnectionFinisher rung the fake SAPI reports. */
    private string $rung = 'fpm';

    /** Temporary key file for the Keystore master key. */
    private string $keyFile = '';

    /** Fake ReplayCache instance. */
    private object $replayCache;

    /** Temp tree standing in for WP_CONTENT_DIR. */
    private string $contentDir = '';

    /** Temp tree standing in for WP_PLUGIN_DIR. */
    private string $pluginDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-sua-test-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
        file_put_contents($this->keyFile, random_bytes(32));

        $cpKeypair      = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($cpKeypair);
        $this->cpPublic = sodium_crypto_sign_publickey($cpKeypair);

        $this->options        = [];
        $this->siteTransients = [];
        $this->actions        = [];
        $this->filters        = [];
        $this->timeLimits     = [];
        $this->firedActions   = [];
        $this->rung           = 'fpm';

        \Plugin_Upgrader::$behaviour    = null;
        \Plugin_Upgrader::$calls        = [];
        \Plugin_Upgrader::$restoreCalls = [];

        // WP_CONTENT_DIR and WP_PLUGIN_DIR are real PHP constants: whichever
        // test file in this process defines them first owns them for the rest
        // of the run. Adopt whatever won and build the fixture INSIDE it, the
        // same idiom the other filesystem tests here use, rather than pinning a
        // per-test path a later set_up() could no longer reach.
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins');
        }
        $this->contentDir = rtrim((string) constant('WP_CONTENT_DIR'), '/\\');
        $this->pluginDir  = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');

        $this->clearFixtureTree();
        mkdir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG, 0755, true);
        mkdir($this->contentDir . '/upgrade', 0755, true);

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
        Functions\when('get_site_transient')->alias(function (string $key) {
            return $this->siteTransients[$key] ?? false;
        });
        Functions\when('set_site_transient')->alias(function (string $key, $value, int $ttl = 0) {
            $this->siteTransients[$key] = $value;
            return true;
        });
        Functions\when('delete_site_transient')->alias(function (string $key) {
            unset($this->siteTransients[$key]);
            return true;
        });
        Functions\when('wp_update_plugins')->alias(function (): void {
            // Core writes the transient BEFORE it calls wordpress.org, so a site
            // that cannot reach wordpress.org still ends up with an object here.
            $this->siteTransients['update_plugins'] = (object) ['response' => [], 'no_update' => []];
        });
        Functions\when('wp_clear_scheduled_hook')->justReturn(1);
        Functions\when('wp_parse_url')->alias(fn (string $url) => parse_url($url));
        Functions\when('get_plugin_data')->alias(fn () => ['Version' => $this->onDiskVersion]);
        Functions\when('is_wp_error')->alias(fn ($thing) => $thing instanceof \WP_Error);
        Functions\when('set_time_limit')->alias(function (int $seconds) {
            $this->timeLimits[] = $seconds;
            return true;
        });
        // ignore_user_abort() is left REAL: it is not in patchwork.json's
        // redefinable internals, and under the CLI SAPI it is a harmless no-op.
        Functions\when('wp_delete_file')->alias(function (string $file) {
            @unlink($file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });

        // Record hook registrations rather than let Brain Monkey swallow them:
        // the ORDER of the shutdown queue is what several assertions here are
        // about, and the replay below needs the callbacks themselves.
        Functions\when('add_action')->alias(function (string $hook, $callback, int $priority = 10) {
            $this->actions[] = ['hook' => $hook, 'priority' => $priority, 'callback' => $callback];
            return true;
        });
        Functions\when('add_filter')->alias(function (string $hook, $callback, int $priority = 10) {
            $this->filters[] = $hook . '@' . $priority;
            return true;
        });
        Functions\when('remove_filter')->justReturn(true);
        Functions\when('do_action')->alias(function (string $hook): void {
            $this->firedActions[] = $hook;
        });

        // Neutralise ConnectionFinisher's last-rung flush under the CLI SAPI.
        // Only real PHP functions are stubbed: DEFINING fastcgi_finish_request
        // or litespeed_finish_request would flip function_exists() process-wide
        // and change which rung unrelated tests observe.
        Functions\when('headers_sent')->justReturn(true);
        Functions\when('ob_get_level')->justReturn(0);
        Functions\when('flush')->justReturn(null);

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

        $this->replayCache = new class extends ReplayCache {
            public function __construct()
            {
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
    }

    protected function tear_down(): void
    {
        \Plugin_Upgrader::$behaviour    = null;
        \Plugin_Upgrader::$calls        = [];
        \Plugin_Upgrader::$restoreCalls = [];
        unset($GLOBALS['wp_filesystem']);
        if ($this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }
        $this->clearFixtureTree();
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Fixture helpers
    // -------------------------------------------------------------------------

    /**
     * Remove only the sub-trees this fixture creates. WP_CONTENT_DIR itself may
     * be shared with other test files in the same process, so it is never
     * deleted wholesale.
     *
     * @return void
     */
    private function clearFixtureTree(): void
    {
        $this->rrmdir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG);
        $this->rrmdir($this->contentDir . '/upgrade');
        $this->rrmdir($this->contentDir . '/upgrade-temp-backup');
    }

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
     * Build an UpdateChecker whose SAPI probe is injected.
     *
     * Under the PHPUnit CLI SAPI neither finish-request function exists, and
     * defining either here would flip function_exists() process-wide and change
     * which rung every other test observes, so the probe is handed in instead.
     * The un-injected default is exercised deliberately by the test that meets
     * the last rung as a real mod_php host does, which is the one case where the
     * real CLI SAPI IS the fixture.
     *
     * The arm behaves identically either way. The probe changes which rung the
     * apply reports, never whether there is one.
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

    /**
     * Stub the CP manifest endpoint with a signed offer of the target version.
     *
     * @return void
     */
    private function stubSignedOffer(): void
    {
        $now    = time();
        $claims = [
            'aud'            => $this->siteId,
            'cmd'            => 'update_manifest',
            'slug'           => 'wpmgr-agent',
            'version'        => $this->targetVersion,
            'min_version'    => '0.0.0',
            'package_url'    => 'https://storage.googleapis.com/wpmgr/agent.zip?sig=x',
            'package_sha256' => str_repeat('ab', 32),
            'package_size'   => 359578,
            'iat'            => $now,
            'exp'            => $now + 300,
            'jti'            => bin2hex(random_bytes(16)),
        ];

        $payloadRaw = (string) json_encode($claims);
        $envelope   = [
            'manifest'  => rtrim(strtr(base64_encode($payloadRaw), '+/', '-_'), '='),
            'signature' => rtrim(strtr(base64_encode(sodium_crypto_sign_detached($payloadRaw, $this->cpSecret)), '+/', '-_'), '='),
        ];
        $body = (string) json_encode($envelope);

        Functions\when('wp_remote_get')->alias(fn (string $url, array $args = []) => [
            'response' => ['code' => 200],
            'body'     => $body,
        ]);
        Functions\when('wp_remote_retrieve_response_code')->alias(fn ($response) => $response['response']['code'] ?? 0);
        Functions\when('wp_remote_retrieve_body')->alias(fn ($response) => $response['body'] ?? '');
    }

    /**
     * Run one whole command request: arm, then run the apply exactly as
     * serveThenApply() does once the connection has been released, then replay
     * the shutdown queue in priority order.
     *
     * The rung is passed explicitly rather than probed, for the same reason
     * makeChecker() injects the arm's probe: neither fastcgi_finish_request nor
     * litespeed_finish_request can be defined in this process without flipping
     * function_exists() process-wide and changing which rung every other test
     * observes. The apply runs on every rung, so what this seam exercises is
     * which rung gets RECORDED and that no rung is skipped. What the real CLI
     * SAPI does with an un-injected checker has its own test below.
     *
     * @param bool $replayShutdown Whether to replay the shutdown queue after.
     * @return array<string,mixed> The arm answer.
     */
    private function runApply(bool $replayShutdown = true): array
    {
        $checker = $this->makeChecker();
        $answer  = $checker->planSelfUpdate();

        $method = new \ReflectionMethod($checker, 'applyPendingUpdate');
        $method->invoke(
            $checker,
            $this->rung,
            (string) $answer['apply_id'],
            (string) $answer['from_version'],
            (string) $answer['to_version']
        );

        if ($replayShutdown) {
            $this->replayShutdown();
        }

        return $answer;
    }

    /**
     * Pin which rung runApply() hands the apply.
     *
     * @param string $rung One of fpm|litespeed|fallback.
     * @return void
     */
    private function pinFinisherRung(string $rung): void
    {
        $this->rung = $rung;
    }

    /**
     * Replay every shutdown callback registered during the request, in the
     * order WordPress would dispatch them.
     *
     * @return list<string> Labels of the callbacks that fired, in order.
     */
    private function replayShutdown(): array
    {
        $queue = [];
        foreach ($this->actions as $index => $action) {
            if ($action['hook'] !== 'shutdown') {
                continue;
            }
            $queue[] = ['priority' => $action['priority'], 'seq' => $index, 'callback' => $action['callback']];
        }

        usort($queue, static function (array $a, array $b): int {
            return $a['priority'] <=> $b['priority'] ?: $a['seq'] <=> $b['seq'];
        });

        $fired = [];
        foreach ($queue as $entry) {
            $callback = $entry['callback'];
            $fired[]  = is_array($callback) ? (string) $callback[1] : 'closure';
            if (is_callable($callback)) {
                $callback();
            }
        }

        return $fired;
    }

    /** The stored apply outcome record, or null. */
    private function record(): ?array
    {
        $stored = $this->options[AgentSelfUpdateCommand::OPTION_RESULT] ?? null;

        return is_array($stored) ? $stored : null;
    }

    // =========================================================================
    // WHERE the apply runs
    // =========================================================================

    /**
     * THE APPLY RUNS FROM rest_pre_serve_request, NOT FROM shutdown.
     *
     * This is the inverse of the assertion the old backstop test carried, and it
     * is the single most load-bearing test of this change. The old position, a
     * shutdown callback at priority 9997, sat past the point at which core's own
     * restore_temp_backup (shutdown, priority 10) could still be dispatched, so
     * every failed swap was unrecoverable. Nothing in the agent may register the
     * apply on shutdown again.
     */
    public function test_the_apply_runs_from_rest_pre_serve_request_not_from_shutdown(): void
    {
        $this->stubSignedOffer();

        $checker = $this->makeChecker();
        $checker->planSelfUpdate();

        $this->assertContains(
            'rest_pre_serve_request@999',
            $this->filters,
            'the apply must be registered on the filter that fires in the request body'
        );

        foreach ($this->actions as $action) {
            $this->assertNotSame(
                'shutdown',
                $action['hook'],
                'the arm must register nothing on shutdown; the apply has not even started at this point'
            );
        }
    }

    /**
     * NO AGENT SHUTDOWN CALLBACK MAY SIT BELOW PRIORITY 100.
     *
     * Core registers restore_temp_backup at 10 and delete_temp_backup at 100. A
     * fatal raised inside a shutdown callback abandons the rest of the queue, so
     * anything of the agent's placed ahead of those two can kill the rollback it
     * is supposed to be backing up. Every agent binding must therefore sit after
     * both. Read out of the source so a new binding anywhere in the plugin is
     * covered, not just the ones this file happens to know about.
     */
    public function test_no_agent_shutdown_binding_sits_ahead_of_cores_rollback(): void
    {
        $offenders = [];
        foreach ($this->agentSourceFiles() as $file) {
            $source = $this->executableSource((string) file_get_contents($file));
            if (!preg_match_all("/add_action\(\s*'shutdown'\s*,(.+?)\)\s*;/s", $source, $matches, PREG_SET_ORDER)) {
                continue;
            }
            foreach ($matches as $match) {
                $priority = $this->priorityOf(trim($match[1]));
                if ($priority !== null && $priority <= 100) {
                    $offenders[] = basename($file) . ' at priority ' . $priority;
                }
            }
        }

        $this->assertSame(
            [],
            $offenders,
            'these agent shutdown bindings run before core\'s temp-backup restore (10) and cleanup (100), where a '
            . 'throw would abandon the rest of the queue: ' . implode(', ', $offenders)
        );
    }

    /**
     * The warm list rots the moment somebody adds a shutdown callback and
     * forgets it, so it is checked against the source rather than trusted.
     * Every class that binds a shutdown callback runs AFTER the swap by
     * definition, so every one of them has to be in memory before the swap.
     */
    public function test_every_shutdown_binding_names_a_warmed_class(): void
    {
        $missing = [];
        foreach ($this->agentSourceFiles() as $file) {
            $source = $this->executableSource((string) file_get_contents($file));
            if (!str_contains($source, "add_action('shutdown'")) {
                continue;
            }
            if (!preg_match('/^namespace\s+([^;]+);/m', $source, $ns) || !preg_match('/^(?:final\s+)?class\s+(\w+)/m', $source, $cls)) {
                continue;
            }

            $fqn = trim($ns[1]) . '\\' . $cls[1];
            if (!in_array($fqn, UpdateChecker::POST_SWAP_CLASSES, true)) {
                $missing[] = $fqn;
            }
        }

        $this->assertSame(
            [],
            $missing,
            'these classes register a shutdown callback, so they run after the plugin directory has been replaced, '
            . 'and must be warmed into memory before the swap: ' . implode(', ', $missing)
        );
    }

    // =========================================================================
    // The rung: recorded, never obeyed
    // =========================================================================

    /**
     * A SAPI THAT CANNOT DETACH ARMS AND SWAPS LIKE ANY OTHER.
     *
     * The fixture is the REAL default probe under the real CLI SAPI, which
     * exposes neither finish-request function, so nothing is injected at all:
     * this is the last rung as a mod_php or plain-CGI host actually meets it.
     *
     * Both halves of the old refusal are pinned as gone here. The arm answers
     * scheduled and registers the seam, and dispatching that seam runs the
     * upgrade rather than recording a refusal. WordPress applies plugin updates
     * on exactly this host class from wp-admin every day, on a fully attached
     * browser connection, and refusing here left a large share of shared
     * hosting on an agent the fleet could never upgrade.
     */
    public function test_a_sapi_that_cannot_detach_arms_and_applies(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $checker = new UpdateChecker($this->signer, $this->settings, $this->keystore, $this->replayCache);
        $answer  = $checker->planSelfUpdate();

        $this->assertSame('scheduled', $answer['status']);
        $this->assertContains(
            'rest_pre_serve_request@999',
            $this->filters,
            'the apply seam must be registered on a host that cannot detach, exactly as on one that can'
        );
        $this->assertNotSame(
            0,
            (int) ($this->options[self::LOCK_OPTION] ?? 0),
            'the arm holds the upgrader lock on this host class too'
        );

        $this->assertTrue(
            $checker->serveThenApply(false, null, null, null),
            'the seam takes over the response here as it does anywhere else'
        );

        $this->assertSame(
            [UpdateChecker::PLUGIN_KEY],
            \Plugin_Upgrader::$calls,
            'the upgrade must run on a still-attached connection, which is what core itself does here'
        );
        $this->assertSame('applied', (string) ($this->record()['status'] ?? ''));
        $this->assertSame(
            'fallback',
            (string) ($this->record()['rung'] ?? ''),
            'and the record must say which rung it ran on, so a fleet reading can still tell'
        );
    }

    /**
     * THE APPLY PROCEEDS ON A NON-DETACHING RUNG, AND SAYS SO.
     *
     * This is the inverse of the assertion this file used to carry. A rung of
     * 'fallback' after the acknowledgement used to abandon the apply and record
     * a refusal; it now runs the identical upgrade the detaching rungs run and
     * records the rung beside the outcome. Reached through the seam the class
     * exposes for the rung, because no real SAPI in this process produces one.
     */
    public function test_a_non_detaching_rung_after_the_ack_still_applies(): void
    {
        $this->stubSignedOffer();
        $this->pinFinisherRung('fallback');
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $answer = $this->runApply();

        $this->assertSame('scheduled', $answer['status']);
        $this->assertSame(
            [UpdateChecker::PLUGIN_KEY],
            \Plugin_Upgrader::$calls,
            'the rung is a diagnostic; it may not decide whether the upgrade happens'
        );

        $record = $this->record();
        $this->assertIsArray($record);
        $this->assertSame('applied', $record['status']);
        $this->assertSame('fallback', $record['rung']);
        $this->assertSame(
            $answer['apply_id'],
            $record['apply_id'],
            'the outcome must name the apply that produced it'
        );

        $this->assertArrayNotHasKey(
            self::LOCK_OPTION,
            $this->options,
            'and the lock is released when the apply returns, on every rung'
        );
    }

    /**
     * The detaching rungs do get a swap. Same fixture, one different rung.
     */
    public function test_a_detaching_sapi_runs_the_upgrade(): void
    {
        $this->stubSignedOffer();
        $this->pinFinisherRung('fpm');
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $this->runApply();

        $this->assertSame([UpdateChecker::PLUGIN_KEY], \Plugin_Upgrader::$calls);
        $this->assertSame('applied', (string) ($this->record()['status'] ?? ''));
        $this->assertSame('fpm', (string) ($this->record()['rung'] ?? ''));
    }

    /**
     * THE EXECUTION GUARDS ARE ARMED ON EVERY RUNG. THIS IS THE HOIST.
     *
     * ignore_user_abort(true) and the bounded set_time_limit() used to sit
     * BELOW a rung check that returned early on 'fallback', which made them
     * dead code on precisely the host class where they are the only defence:
     * on a SAPI that cannot release the connection, ignore_user_abort() is what
     * keeps a peer hanging up from ending the swap halfway. Removing that early
     * return is what put them on every rung, so a future edit that reintroduces
     * any rung-keyed skip above them has to fail here.
     *
     * set_time_limit() is observed directly (it is stubbed by this fixture).
     * ignore_user_abort() is left real, because it is not redefinable and a
     * Patchwork redefinition would leak across the suite, so it is observed
     * through its own return value: called with no argument it reports the
     * current setting without changing it.
     */
    public function test_the_apply_arms_its_execution_guards_on_every_rung(): void
    {
        $wasIgnoring = ignore_user_abort();

        try {
            foreach (['fpm', 'litespeed', 'fallback'] as $rung) {
                ignore_user_abort(false);

                // Clear only what one cycle leaves behind. Wiping the whole
                // option store would take the site's own keypair with it, and an
                // arm that cannot sign never reaches an apply at all.
                unset($this->options[AgentSelfUpdateCommand::OPTION_RESULT], $this->options[self::LOCK_OPTION]);
                $this->filters           = [];
                $this->actions           = [];
                $this->timeLimits        = [];
                \Plugin_Upgrader::$calls = [];

                $this->stubSignedOffer();
                $this->pinFinisherRung($rung);
                $this->siteTransients['update_plugins'] = $this->offerTransient();

                $this->runApply(false);

                $this->assertSame(
                    [UpdateChecker::PLUGIN_KEY],
                    \Plugin_Upgrader::$calls,
                    "the '{$rung}' rung must reach the upgrade"
                );
                $this->assertContains(
                    LongRunningJob::TIME_LIMIT_SECONDS,
                    $this->timeLimits,
                    "the '{$rung}' rung must arm the bounded execution cap before the swap"
                );
                $this->assertSame(
                    1,
                    ignore_user_abort(),
                    "the '{$rung}' rung must set ignore_user_abort, which is the last rung's only defence"
                );
            }
        } finally {
            ignore_user_abort((bool) $wasIgnoring);
        }
    }

    // =========================================================================
    // The upgrade itself
    // =========================================================================

    /**
     * THE P1 REGRESSION TEST.
     *
     * Plugin_Upgrader::upgrade() reads the plugin update transient to find the
     * package and bails to its up_to_date branch, returning a bare false, when
     * there is no entry for the plugin. This path used to DELETE that transient
     * a few lines above the call, so core took that bail on every site, every
     * time, and answered false with nothing said. A missing offer is now one
     * named, control-plane-visible failure instead of a silent one.
     */
    public function test_a_missing_update_offer_is_recorded_as_a_named_failure(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = (object) ['response' => [], 'no_update' => []];

        $this->runApply();

        $this->assertSame([], \Plugin_Upgrader::$calls, 'the upgrade must not be attempted without an offer');

        $record = $this->record();
        $this->assertIsArray($record);
        $this->assertSame('failed', $record['status']);
        $this->assertStringContainsString('no entry for this plugin', (string) $record['detail']);
    }

    /**
     * A site that is already at the target between the arm and the apply is
     * benign, not a failure. Reporting it as failed would give the control
     * plane an attributed record whose status is not "applied", which halts a
     * rollout wave over a site that is running exactly the build it should.
     */
    public function test_a_site_already_at_the_target_records_already_applied(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = (object) [
            'response'  => [],
            'no_update' => [UpdateChecker::PLUGIN_KEY => (object) ['new_version' => $this->onDiskVersion]],
        ];

        // The target is the version already on disk: the build landed some
        // other way between the arm and this line.
        $checker = $this->makeChecker();
        $method  = new \ReflectionMethod($checker, 'runUpgrade');
        $method->invoke($checker, 'apply-id', '0.10.4', $this->onDiskVersion);

        $this->assertSame([], \Plugin_Upgrader::$calls);
        $this->assertSame('already_applied', (string) ($this->record()['status'] ?? ''));
    }

    /**
     * The upgrade must run with wp_doing_cron() forced true, and must put it
     * back afterwards.
     *
     * That flag is what makes core take its BACKGROUND-update branch:
     * Plugin_Upgrader::deactivate_plugin_before_upgrade() returns early under
     * cron without touching active_plugins, so the agent is never silently
     * deactivated and there is nothing to re-activate by hand, and
     * active_before()/active_after() put maintenance mode over the destructive
     * window. Leaving the filter behind would change how every later plugin
     * update in the process behaves, so the removal is asserted too.
     */
    public function test_the_upgrade_runs_with_wp_doing_cron_forced_true_and_restores_it_after(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        /** @var list<string> $seen */
        $seen = [];
        Functions\when('add_filter')->alias(function (string $hook, $callback, int $priority = 10) use (&$seen) {
            $this->filters[] = $hook . '@' . $priority;
            if ($hook === 'wp_doing_cron') {
                $seen[] = 'added';
            }
            return true;
        });
        Functions\when('remove_filter')->alias(function (string $hook) use (&$seen) {
            if ($hook === 'wp_doing_cron') {
                $seen[] = 'removed';
            }
            return true;
        });
        \Plugin_Upgrader::$behaviour = static function () use (&$seen) {
            $seen[] = 'upgraded';
            return true;
        };

        $this->runApply();

        $this->assertSame(
            ['added', 'upgraded', 'removed'],
            $seen,
            'the cron flag must be on for the upgrade and only for the upgrade'
        );
    }

    /**
     * The apply arms a bounded PHP execution limit before any download.
     *
     * max_execution_time is 30s on a great many hosts, and the transfer alone is
     * budgeted at 300s. Bounded, never 0: max_execution_time is the ONE timer
     * whose fatal still runs shutdown functions, which is exactly what the
     * rollback depends on. Asserted as intent rather than as an observed OS
     * effect, since set_time_limit() is a no-op under most CLI SAPIs.
     */
    public function test_the_apply_raises_a_bounded_php_execution_limit(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $this->runApply();

        $this->assertNotSame([], $this->timeLimits, 'the apply must raise the execution limit before it downloads anything');
        $this->assertSame(LongRunningJob::TIME_LIMIT_SECONDS, $this->timeLimits[0]);
        $this->assertGreaterThan(300, $this->timeLimits[0], 'the limit must exceed the package download budget');
    }

    /**
     * A REQUEST THAT ARMED NOTHING IS LEFT EXACTLY AS IT WAS FOUND.
     *
     * The seam is registered by the arm, but it is a public filter callback and
     * anything at all may dispatch it. With no pending apply it must hand the
     * response back and touch nothing: no execution limit raised, no upgrade
     * started, no outcome recorded. This used to be covered incidentally by the
     * SAPI refusal, which arrived at the same state for a reason that no longer
     * exists, so it is now asserted for its own sake.
     */
    public function test_a_seam_dispatch_with_nothing_armed_leaves_the_request_alone(): void
    {
        $checker = $this->makeChecker();

        $this->assertFalse(
            $checker->serveThenApply(false, null, null, null),
            'with nothing pending the response belongs to core'
        );

        $this->assertSame([], $this->timeLimits, 'a request that armed nothing must not have its limits rewritten');
        $this->assertSame([], \Plugin_Upgrader::$calls, 'and nothing may be swapped');
        $this->assertNull($this->record(), 'and no outcome may be recorded for an apply that never existed');
    }

    /**
     * AN APPLY THAT COULD NOT ANSWER DOES NOT HAPPEN.
     *
     * The reason for taking over the response body is that the control plane is
     * told what is about to happen before it happens. If writing that
     * acknowledgement throws, the swap is abandoned and the response is handed
     * back to core: an un-upgraded site re-arms on the next command, a site
     * upgraded behind the control plane's back does not.
     */
    public function test_an_acknowledgement_that_cannot_be_written_abandons_the_apply(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $server = new class {
            /**
             * @param object $response Response object.
             * @param bool   $embed    Whether to embed links.
             * @return array<string,mixed>
             */
            public function response_to_data($response, $embed): array
            {
                throw new \RuntimeException('response serialisation exploded');
            }
        };

        $checker = $this->makeChecker();
        $checker->planSelfUpdate();

        $this->assertFalse(
            $checker->serveThenApply(false, new \stdClass(), null, $server),
            'the response must be handed back to core, not swallowed'
        );
        $this->assertSame([], \Plugin_Upgrader::$calls, 'nothing may be swapped');
        $this->assertSame('failed', (string) ($this->record()['status'] ?? ''));
        $this->assertArrayNotHasKey(self::LOCK_OPTION, $this->options, 'and the lock must not be left held');
    }

    /**
     * The seam is a ONE-SHOT. It reads the pending apply and clears it in the
     * same breath, so a second dispatch of the filter, from whatever cause,
     * passes the response through untouched and starts nothing.
     */
    public function test_the_seam_applies_at_most_once_per_request(): void
    {
        $this->stubSignedOffer();

        $checker = $this->makeChecker();
        $checker->planSelfUpdate();

        $this->assertTrue($checker->serveThenApply(false, null, null, null));

        $this->options = [];
        $this->assertFalse(
            $checker->serveThenApply(false, null, null, null),
            'a second dispatch must hand the response back exactly as it found it'
        );
        $this->assertNull($this->record(), 'and it must record nothing, because it did nothing');
    }

    /**
     * THE LOCK MUST OUTLIVE AN APPLY THAT RUNS TO ITS OWN CAP, AND NOT ONE
     * SECOND LONGER. A shorter lock lets a second apply start while the first is
     * still copying files; a longer one blocks the site for time during which
     * the first apply cannot possibly still be alive.
     */
    public function test_the_apply_lock_ttl_equals_the_apply_execution_limit(): void
    {
        $this->stubSignedOffer();

        $checker = $this->makeChecker();
        $answer  = $checker->planSelfUpdate();

        $this->assertSame('scheduled', $answer['status']);
        $this->assertSame(
            time() + LongRunningJob::TIME_LIMIT_SECONDS,
            (int) $answer['expires_at'],
            'the answered window is the lock window, which is the execution budget the apply arms for itself'
        );
    }

    // =========================================================================
    // Failure paths and the rollback
    // =========================================================================

    /**
     * A shutdown callback registered by the upgrade IS dispatched afterwards.
     *
     * This is the property the old position lost. Core's WP_Upgrader::run()
     * registers restore_temp_backup on shutdown at priority 10 when the install
     * fails; from a shutdown callback at 9997 that registration was already
     * behind the dispatcher and never ran. From the request body it is an
     * ordinary registration on a hook that has not started.
     */
    public function test_the_upgrade_registers_the_temp_backup_restore_on_a_dispatchable_shutdown(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $restoreFired = false;
        \Plugin_Upgrader::$behaviour = static function () use (&$restoreFired) {
            // Exactly what WP_Upgrader::run() does when install_package fails.
            add_action('shutdown', static function () use (&$restoreFired): void {
                $restoreFired = true;
            }, 10);

            return new \WP_Error('install_failed', 'Could not copy files.');
        };

        $fired = $this->runApply();

        $this->assertTrue($restoreFired, 'core\'s rollback must still be reachable after the apply returns');
        $this->assertSame('failed', (string) ($this->record()['status'] ?? ''));
        $this->assertStringContainsString('install_failed', (string) ($this->record()['detail'] ?? ''));
        unset($fired);
    }

    /**
     * A Throwable escaping upgrade() means WP_Upgrader::run() never reached the
     * branch where it registers core's shutdown restore, so the catch has to
     * call core's own public restorer itself. It is precondition-gated on the
     * plugin directory being absent AND the backup being present, which is what
     * stops it reverting an install that had already succeeded before some
     * later listener threw.
     */
    public function test_a_throwable_from_the_upgrader_calls_cores_temp_backup_restore(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();
        $GLOBALS['wp_filesystem']               = new \stdClass();

        // The world after a fatal inside install_package: the directory has been
        // moved aside and the only copy is core's temp backup.
        $this->rrmdir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG);
        mkdir($this->contentDir . '/upgrade-temp-backup/plugins/' . UpdateChecker::PLUGIN_SLUG, 0755, true);

        \Plugin_Upgrader::$behaviour = static function (): void {
            throw new \RuntimeException('upgrader exploded');
        };

        $this->runApply();

        $this->assertCount(1, \Plugin_Upgrader::$restoreCalls, 'the catch must put the directory back');
        $this->assertSame(
            [['slug' => UpdateChecker::PLUGIN_SLUG, 'src' => WP_PLUGIN_DIR, 'dir' => 'plugins']],
            \Plugin_Upgrader::$restoreCalls[0],
            'core\'s restorer must be called with core\'s own argument shape'
        );
        $this->assertSame('failed', (string) ($this->record()['status'] ?? ''));
    }

    /**
     * The shutdown guard restores only when the plugin directory is genuinely
     * missing and core's backup is genuinely present, and no-ops otherwise.
     * Both directions, because a guard that fires when core has already restored
     * would delete a working directory and move a stale copy over it.
     */
    public function test_the_shutdown_guard_restores_when_the_directory_is_missing_and_no_ops_when_it_is_not(): void
    {
        $this->stubSignedOffer();
        $GLOBALS['wp_filesystem'] = new \stdClass();

        $checker  = $this->makeChecker();
        $upgrader = new \Plugin_Upgrader();
        $property = new \ReflectionProperty($checker, 'upgrader');
        $property->setValue($checker, $upgrader);

        // Directory present: nothing to do, whatever else is lying around.
        mkdir($this->contentDir . '/upgrade-temp-backup/plugins/' . UpdateChecker::PLUGIN_SLUG, 0755, true);
        $checker->restoreAgentIfDirectoryMissing();
        $this->assertSame([], \Plugin_Upgrader::$restoreCalls, 'a present directory must never be restored over');

        // Directory gone, backup present: restore.
        $this->rrmdir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG);
        $checker->restoreAgentIfDirectoryMissing();
        $this->assertCount(1, \Plugin_Upgrader::$restoreCalls);

        // Directory gone, no backup: nothing to restore FROM, so no call.
        $this->rrmdir($this->contentDir . '/upgrade-temp-backup');
        $checker->restoreAgentIfDirectoryMissing();
        $this->assertCount(1, \Plugin_Upgrader::$restoreCalls);
    }

    /**
     * The guard never throws. A Throwable escaping a shutdown callback abandons
     * the rest of the queue, which here would mean the outcome push, the
     * catch-up heartbeat, and on a badly-ordered binding core's own rollback.
     */
    public function test_the_shutdown_guard_swallows_everything(): void
    {
        $checker  = $this->makeChecker();
        $upgrader = new class {
            /**
             * @param array<int,array<string,string>> $temp_backups Backups.
             * @return bool
             */
            public function restore_temp_backup(array $temp_backups = []): bool
            {
                throw new \RuntimeException('filesystem is on fire');
            }
        };
        $property = new \ReflectionProperty($checker, 'upgrader');
        $property->setValue($checker, $upgrader);

        $GLOBALS['wp_filesystem'] = new \stdClass();
        $this->rrmdir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG);
        mkdir($this->contentDir . '/upgrade-temp-backup/plugins/' . UpdateChecker::PLUGIN_SLUG, 0755, true);

        $checker->restoreAgentIfDirectoryMissing();

        $this->addToAssertionCount(1);
    }

    /**
     * ONLY LITERAL TRUE IS A SUCCESS: a DEFAULT DENY, not a list of known-bad
     * values.
     *
     * Plugin_Upgrader::upgrade() returns literal true down exactly one path,
     * reached only after WP_Upgrader's $result property came back truthy and
     * not a WP_Error. That property is assigned in ONE place, inside
     * install_package(), and run() never resets it, so true is the only value
     * that positively proves the install ran to completion.
     *
     * Nothing else is. run()'s early bails never touch the property, and
     * Plugin_Upgrader redeclares `public $result;` with no initialiser,
     * shadowing WP_Upgrader's `= array()`, so those bails come back as null; a
     * WP_Error from the upgrader_install_package_result filter rewrites a local
     * inside run() and never reaches this return value at all. A test that
     * names values to refuse cannot cover a set shaped like that, so the check
     * requires the one value that means success, and this pins BOTH ends: the
     * null a real bail produces, and a value no enumeration would have thought
     * to list.
     */
    public function test_only_a_literal_true_upgrader_result_counts_as_applied(): void
    {
        foreach (['null' => static fn () => null, 'empty array' => static fn () => []] as $label => $behaviour) {
            unset($this->options[AgentSelfUpdateCommand::OPTION_RESULT], $this->options[self::LOCK_OPTION]);
            $this->filters = [];
            $this->actions = [];

            $this->stubSignedOffer();
            $this->siteTransients['update_plugins'] = $this->offerTransient();
            \Plugin_Upgrader::$behaviour            = $behaviour;

            $this->runApply();

            $record = $this->record();
            $this->assertSame('failed', (string) ($record['status'] ?? ''), $label . ' must not read as applied');
            $this->assertStringContainsString('filesystem', (string) ($record['detail'] ?? ''));
        }
    }

    // =========================================================================
    // What the control plane is told
    // =========================================================================

    /**
     * EVERY recorded outcome carries the apply id, because the control plane
     * has exactly one way to know a version move was caused by the run it armed,
     * and that is comparing this field whole.
     */
    public function test_every_recorded_outcome_carries_the_apply_id(): void
    {
        $behaviours = [
            'applied' => static fn () => true,
            'failed'  => static fn () => new \WP_Error('nope', 'no'),
            'threw'   => static function (): void {
                throw new \RuntimeException('boom');
            },
        ];

        foreach ($behaviours as $label => $behaviour) {
            // Clear only what one cycle leaves behind. Wiping the whole option
            // store would take the site's own keypair with it, and an arm that
            // cannot sign answers "error" with an empty apply id, which would
            // make this test pass by comparing nothing to nothing.
            unset($this->options[AgentSelfUpdateCommand::OPTION_RESULT], $this->options[self::LOCK_OPTION]);
            $this->filters = [];
            $this->actions = [];
            \Plugin_Upgrader::$behaviour = $behaviour;
            $this->stubSignedOffer();
            $this->siteTransients['update_plugins'] = $this->offerTransient();

            $answer = $this->runApply(false);

            $this->assertSame('scheduled', $answer['status'], $label . ': the arm must have armed something');
            $this->assertNotSame('', (string) $answer['apply_id'], $label . ': and it must have minted an id');

            $record = $this->record();
            $this->assertIsArray($record, $label . ': every terminal path must record an outcome');
            $this->assertSame(
                $answer['apply_id'],
                $record['apply_id'],
                $label . ': the recorded apply id must be the one the arm answered, or attribution is impossible'
            );
        }
    }

    /**
     * A FAILED apply changes no version, so the version-changed push never fires
     * for it, and without a nudge its record would wait for the 30-minute
     * metadata cron, which is longer than the control plane's shortest confirm
     * deadline. It queues its own push at a shutdown priority AFTER the rollback
     * guard, so what it reports is the post-rollback truth.
     */
    public function test_a_failed_apply_pushes_its_outcome_before_the_confirm_deadline(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();
        \Plugin_Upgrader::$behaviour            = static fn () => new \WP_Error('nope', 'no');

        $this->runApply(false);

        $push = null;
        foreach ($this->actions as $action) {
            if ($action['hook'] === 'shutdown' && is_array($action['callback']) && $action['callback'][1] === 'pushOutcomeNow') {
                $push = $action;
            }
        }

        $this->assertNotNull($push, 'a failed apply must not wait for the metadata cron');
        $this->assertGreaterThan(
            101,
            (int) $push['priority'],
            'the push must run after the rollback guard, so it reports the post-rollback truth'
        );

        $this->replayShutdown();
        $this->assertContains(
            'wpmgr_agent_self_update_recorded',
            $this->firedActions,
            'the push fires an action Plugin binds to its metadata sender'
        );
    }

    /**
     * A SUCCESSFUL apply queues no push: the new code announces itself on its
     * first boot, and this old code is in no position to declare a success it
     * cannot observe.
     */
    public function test_a_successful_apply_leaves_the_announcement_to_the_new_code(): void
    {
        $this->stubSignedOffer();
        $this->siteTransients['update_plugins'] = $this->offerTransient();

        $this->runApply(false);

        foreach ($this->actions as $action) {
            if ($action['hook'] !== 'shutdown' || !is_array($action['callback'])) {
                continue;
            }
            $this->assertNotSame('pushOutcomeNow', $action['callback'][1]);
        }
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /** A plugin-update transient carrying an offer for the agent. */
    private function offerTransient(): object
    {
        $entry              = new \stdClass();
        $entry->slug        = UpdateChecker::PLUGIN_SLUG;
        $entry->plugin      = UpdateChecker::PLUGIN_KEY;
        $entry->new_version = $this->targetVersion;
        $entry->package     = UpdateChecker::PACKAGE_SENTINEL;

        return (object) ['response' => [UpdateChecker::PLUGIN_KEY => $entry], 'no_update' => []];
    }

    /**
     * Source with comments and doc blocks stripped.
     *
     * Both scans below read hook registrations out of the source, and this
     * file's own prose quotes core's `add_action('shutdown', restore_temp_backup,
     * 10)` verbatim. Prose is never dispatched by anything, so it must not be
     * read as a binding.
     *
     * @param string $source Raw PHP source.
     * @return string Executable source only.
     */
    private function executableSource(string $source): string
    {
        $code = '';
        foreach (token_get_all($source) as $token) {
            if (is_array($token) && ($token[0] === T_COMMENT || $token[0] === T_DOC_COMMENT)) {
                continue;
            }
            $code .= is_array($token) ? $token[1] : $token;
        }

        return $code;
    }

    /**
     * The priority of one add_action('shutdown', ...) argument list.
     *
     * Resolves a `self::SOME_CONSTANT` priority through reflection so the
     * source is free to name its priorities rather than spell out magic
     * numbers, which is exactly what makes them reviewable.
     *
     * @param string $arguments Everything after the hook name.
     * @return int|null The priority, or null when the third argument is an
     *                  expression this cannot resolve (which is reported).
     */
    private function priorityOf(string $arguments): ?int
    {
        if (preg_match('/,\s*(\d+)\s*$/', $arguments, $found)) {
            return (int) $found[1];
        }

        if (preg_match('/,\s*self::([A-Z0-9_]+)\s*$/', $arguments, $found)) {
            $value = (new \ReflectionClass(UpdateChecker::class))->getConstant($found[1]);
            $this->assertIsInt($value, 'shutdown priority constant ' . $found[1] . ' must resolve to an int');

            return (int) $value;
        }

        // No third argument at all: WordPress defaults to 10, which is exactly
        // the position this rule exists to forbid.
        return 10;
    }

    /**
     * Every PHP source file of the plugin.
     *
     * @return list<string>
     */
    private function agentSourceFiles(): array
    {
        $files    = [];
        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator(dirname(__DIR__) . '/includes', \FilesystemIterator::SKIP_DOTS)
        );

        /** @var \SplFileInfo $file */
        foreach ($iterator as $file) {
            if ($file->isFile() && $file->getExtension() === 'php') {
                $files[] = $file->getPathname();
            }
        }
        sort($files);

        return $files;
    }
}
