<?php
/**
 * THE OFFER THE UPGRADER READS CONTAINS OUR ENTRY (GitHub issue #334).
 *
 * That sentence is the invariant the CP-commanded self-update lives or dies by,
 * and it is the one 0.61.108 did not assert. 0.61.108 asserted "the
 * update_plugins transient exists", rebuilt it when it was absent, and shipped.
 * A transient that was PRESENT and simply carried no entry of ours walked
 * straight past that guard into a named failure, which is what a live 21-site
 * fleet reported.
 *
 * WHY NO EXISTING TEST COULD HAVE CAUGHT IT, which is half the answer to "how
 * did this happen". Every apply test in SelfUpdateInRequestApplyTest stubs
 * get_site_transient() as a plain array lookup with NO filter dispatch, and its
 * add_filter() stub records a string. So injectUpdate(), the filter that is
 * supposed to PRODUCE the entry, was never in the loop in any apply test, and
 * every green apply test handed the upgrader a transient that already contained
 * the entry. Two halves tested in isolation and the seam between them tested
 * nowhere.
 *
 * This file closes that. Its get_site_transient() is core's:
 * pre_site_transient_{$transient} first, returning immediately on anything but
 * false (wp-includes/option.php:2580 to :2584), then the stored value, then
 * site_transient_{$transient} (:2620). Its Plugin_Upgrader double re-reads the
 * transient through that chain exactly as Plugin_Upgrader::upgrade() does
 * (wp-admin/includes/class-plugin-upgrader.php:199 to :206), and what it saw is
 * what every test below asserts on. Not "a failure was recorded". Not "the
 * transient exists". The entry, named by version, in the offer the upgrader
 * actually read.
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
 */
final class SelfUpdateOfferInvariantTest extends TestCase
{
    /** Raw 64-byte CP Ed25519 secret key. */
    private string $cpSecret = '';

    /** Raw 32-byte CP Ed25519 public key. */
    private string $cpPublic = '';

    /** The enrolled site_id for this test run. */
    private string $siteId = 'test-site-uuid-offer-invariant';

    /** On-disk plugin version. */
    private string $onDiskVersion = '0.10.5';

    /** The version the control plane offers. */
    private string $targetVersion = '';

    private Keystore $keystore;
    private Settings $settings;
    private Signer $signer;

    /** @var array<string,mixed> wp-option store. */
    private array $options = [];

    /** @var array<string,mixed> site_transient store (the STORED value only). */
    private array $siteTransients = [];

    /**
     * Registered hooks: hook name => priority => list of callbacks.
     *
     * @var array<string,array<int,list<callable>>>
     */
    private array $hooks = [];

    /** @var list<array{hook:string,priority:int,callback:mixed}> Every add_action() seen. */
    private array $actions = [];

    /** @var list<string> Every key handed to delete_site_transient(). */
    private array $deletedSiteTransients = [];

    /** How many times the control-plane manifest endpoint was called. */
    private int $manifestFetches = 0;

    /** The offer entry Plugin_Upgrader read for our plugin, or null. */
    private ?object $offerSeenByUpgrader = null;

    /** The whole transient object Plugin_Upgrader read, or null. */
    private ?object $transientSeenByUpgrader = null;

    /** Temporary key file for the Keystore master key. */
    private string $keyFile = '';

    /** Whether THIS file defined WPMGR_AGENT_KEY_FILE and so owns the file. */
    private bool $ownsKeyFile = false;

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

        // WPMGR_AGENT_KEY_FILE is a real constant, so whichever test file in
        // this process defines it first owns the path for the whole run. Adopt
        // whatever won and write the key material THERE, rather than to a path
        // this file names but the Keystore will never read.
        if (defined('WPMGR_AGENT_KEY_FILE')) {
            $this->keyFile = (string) constant('WPMGR_AGENT_KEY_FILE');
        } else {
            $this->keyFile = sys_get_temp_dir() . '/wpmgr-soi-test-' . bin2hex(random_bytes(8)) . '.key';
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
            $this->ownsKeyFile = true;
        }
        file_put_contents($this->keyFile, random_bytes(32));

        $cpKeypair      = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($cpKeypair);
        $this->cpPublic = sodium_crypto_sign_publickey($cpKeypair);

        $this->options                 = [];
        $this->siteTransients          = [];
        $this->hooks                   = [];
        $this->actions                 = [];
        $this->deletedSiteTransients   = [];
        $this->manifestFetches         = 0;
        $this->offerSeenByUpgrader     = null;
        $this->transientSeenByUpgrader = null;

        \Plugin_Upgrader::$behaviour    = null;
        \Plugin_Upgrader::$calls        = [];
        \Plugin_Upgrader::$restoreCalls = [];

        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins');
        }
        $this->contentDir = rtrim((string) constant('WP_CONTENT_DIR'), '/\\');
        $this->pluginDir  = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');
        if (!is_dir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG)) {
            mkdir($this->pluginDir . '/' . UpdateChecker::PLUGIN_SLUG, 0755, true);
        }

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

        // ------------------------------------------------------------------
        // THE POINT OF THIS FILE: a real filter chain, in core's real order.
        // ------------------------------------------------------------------
        Functions\when('get_site_transient')->alias(function (string $key) {
            // wp-includes/option.php:2580 to :2584 - the pre-filter runs FIRST
            // and anything but false ends the read there and then.
            $pre = $this->dispatchFilter('pre_site_transient_' . $key, false, $key);
            if ($pre !== false) {
                return $pre;
            }

            $value = $this->siteTransients[$key] ?? false;

            // wp-includes/option.php:2620.
            return $this->dispatchFilter('site_transient_' . $key, $value, $key);
        });
        Functions\when('set_site_transient')->alias(function (string $key, $value, int $ttl = 0) {
            $this->siteTransients[$key] = $value;
            return true;
        });
        Functions\when('delete_site_transient')->alias(function (string $key) {
            $this->deletedSiteTransients[] = $key;
            // wp-includes/option.php:2520 - fired as the FIRST act, before
            // anything is deleted. flushCache() is bound to exactly this.
            $this->dispatchAction('delete_site_transient_' . $key, $key);
            unset($this->siteTransients[$key]);
            return true;
        });

        Functions\when('add_filter')->alias(function (string $hook, $callback, int $priority = 10) {
            $this->hooks[$hook][$priority][] = $callback;
            return true;
        });
        Functions\when('remove_filter')->alias(function (string $hook, $callback, int $priority = 10) {
            foreach ($this->hooks[$hook][$priority] ?? [] as $index => $registered) {
                if ($registered === $callback) {
                    unset($this->hooks[$hook][$priority][$index]);
                }
            }
            return true;
        });
        Functions\when('add_action')->alias(function (string $hook, $callback, int $priority = 10) {
            $this->actions[]                 = ['hook' => $hook, 'priority' => $priority, 'callback' => $callback];
            $this->hooks[$hook][$priority][] = $callback;
            return true;
        });
        Functions\when('remove_action')->alias(function (string $hook, $callback, int $priority = 10) {
            foreach ($this->hooks[$hook][$priority] ?? [] as $index => $registered) {
                if ($registered === $callback) {
                    unset($this->hooks[$hook][$priority][$index]);
                }
            }
            return true;
        });
        Functions\when('apply_filters')->alias(function (string $hook, $value, ...$args) {
            return $this->dispatchFilter($hook, $value, ...$args);
        });
        Functions\when('do_action')->alias(function (string $hook, ...$args): void {
            $this->dispatchAction($hook, ...$args);
        });

        Functions\when('wp_update_plugins')->alias(function (): void {
            $this->fail('the apply must never call wp_update_plugins(): it round-trips wordpress.org');
        });
        Functions\when('wp_clear_scheduled_hook')->justReturn(1);
        Functions\when('wp_parse_url')->alias(fn (string $url) => parse_url($url));
        Functions\when('get_plugin_data')->alias(fn () => ['Version' => $this->onDiskVersion]);
        Functions\when('is_wp_error')->alias(fn ($thing) => $thing instanceof \WP_Error);
        Functions\when('set_time_limit')->justReturn(true);
        Functions\when('wp_delete_file')->alias(function (string $file) {
            @unlink($file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });
        Functions\when('headers_sent')->justReturn(true);
        Functions\when('ob_get_level')->justReturn(0);
        Functions\when('flush')->justReturn(null);

        if (!defined('WPMGR_AGENT_VERSION')) {
            define('WPMGR_AGENT_VERSION', $this->onDiskVersion);
        }
        // onDiskVersion() prefers get_plugin_data() and only falls back to the
        // WPMGR_AGENT_VERSION constant, so a test that needs the version to
        // MOVE (the build landing between the arm and the apply) has to reach
        // it through the header read. OptimizerRumTest already guard-defines
        // this constant and sorts ahead of this file, so the full suite runs
        // with it defined either way; defining it here as well is what makes
        // this file behave identically when it is run on its own.
        if (!defined('WPMGR_AGENT_FILE')) {
            define('WPMGR_AGENT_FILE', __FILE__);
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
        if ($this->ownsKeyFile && $this->keyFile !== '' && is_file($this->keyFile)) {
            @unlink($this->keyFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // The hook dispatcher
    // -------------------------------------------------------------------------

    /**
     * Run every callback registered on $hook, in priority order, threading the
     * value through. This is apply_filters(), which is what makes a late
     * registration at PHP_INT_MAX able to win: every callback runs, and the
     * last one to run answers.
     *
     * @param string $hook  Hook name.
     * @param mixed  $value Value to filter.
     * @param mixed  ...$args Extra arguments.
     * @return mixed
     */
    private function dispatchFilter(string $hook, $value, ...$args)
    {
        $byPriority = $this->hooks[$hook] ?? [];
        ksort($byPriority);

        foreach ($byPriority as $callbacks) {
            foreach ($callbacks as $callback) {
                if (is_callable($callback)) {
                    $value = $callback($value, ...$args);
                }
            }
        }

        return $value;
    }

    /**
     * Run every callback registered on $hook, in priority order, discarding
     * return values.
     *
     * @param string $hook Hook name.
     * @param mixed  ...$args Arguments.
     * @return void
     */
    private function dispatchAction(string $hook, ...$args): void
    {
        $byPriority = $this->hooks[$hook] ?? [];
        ksort($byPriority);

        foreach ($byPriority as $callbacks) {
            foreach ($callbacks as $callback) {
                if (is_callable($callback)) {
                    $callback(...$args);
                }
            }
        }
    }

    // -------------------------------------------------------------------------
    // Fixture helpers
    // -------------------------------------------------------------------------

    /**
     * Bind the two hooks UpdateChecker::install() binds for this channel.
     *
     * install() carries a process-global `static $installed` guard, so calling
     * it here would register for the FIRST test in this process and silently
     * no-op for every one after it. The bindings are therefore made by hand,
     * and test_install_binds_the_filters_this_fixture_reproduces reads the
     * production source to prove this list has not drifted from it.
     *
     * @param UpdateChecker $checker The checker under test.
     * @return void
     */
    private function bindAgentFilters(UpdateChecker $checker): void
    {
        add_filter('site_transient_update_plugins', [$checker, 'injectUpdate']);
        add_action('delete_site_transient_update_plugins', [$checker, 'flushCache']);
    }

    /**
     * Build a checker with the real (CLI) connection probe, so the apply runs
     * on the last rung exactly as it does on a mod_php host.
     *
     * @return UpdateChecker
     */
    private function makeChecker(): UpdateChecker
    {
        return new UpdateChecker($this->signer, $this->settings, $this->keystore, $this->replayCache);
    }

    /**
     * Stub the CP manifest endpoint with a signed offer of the target version,
     * counting every call so a test can assert the apply made none.
     *
     * @return void
     */
    /**
     * The control plane answers 204 No Content: nothing to install. This is the
     * release-withdrawn shape, and the one outcome that legitimately retires a
     * cached offer.
     */
    private function stubNoUpdate(): void
    {
        Functions\when('wp_remote_get')->alias(function (string $url, array $args = []) {
            $this->manifestFetches++;

            return ['response' => ['code' => 204], 'body' => ''];
        });
        Functions\when('wp_remote_retrieve_response_code')->alias(fn ($response) => $response['response']['code'] ?? 0);
        Functions\when('wp_remote_retrieve_body')->alias(fn ($response) => $response['body'] ?? '');
    }

    /**
     * Call primeRecoveryOffer() the way applyPendingUpdate()'s finally does.
     *
     * It is private on purpose, so this reaches it by reflection rather than
     * widening production visibility for a test. What matters is that it runs
     * on the SAME checker instance that verifyDownload() just ran on, because
     * the guard under test is instance state set by that call.
     *
     * @param array<string,mixed> $claims
     */
    private function invokeRecoveryOffer(UpdateChecker $checker, array $claims): void
    {
        $m = new \ReflectionMethod(UpdateChecker::class, 'primeRecoveryOffer');
        $m->setAccessible(true);
        $m->invoke($checker, $claims);
    }

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
            'tested'         => '7.0',
            'requires'       => '6.0',
            'requires_php'   => '8.1',
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

        Functions\when('wp_remote_get')->alias(function (string $url, array $args = []) use ($body) {
            $this->manifestFetches++;

            return ['response' => ['code' => 200], 'body' => $body];
        });
        Functions\when('wp_remote_retrieve_response_code')->alias(fn ($response) => $response['response']['code'] ?? 0);
        Functions\when('wp_remote_retrieve_body')->alias(fn ($response) => $response['body'] ?? '');
    }

    /**
     * Make the Plugin_Upgrader double read the update transient the way
     * Plugin_Upgrader::upgrade() does, through the whole filter chain, and
     * record what it saw.
     *
     * wp-admin/includes/class-plugin-upgrader.php:199 to :206: the transient is
     * re-read INSIDE upgrade(), and a missing entry returns a bare false having
     * done nothing. A locally repaired variable in the caller is worth nothing
     * here, which is exactly why the fix is a filter.
     *
     * @return void
     */
    private function upgraderReadsTheOffer(): void
    {
        \Plugin_Upgrader::$behaviour = function (string $plugin) {
            $current                       = get_site_transient('update_plugins');
            $this->transientSeenByUpgrader = is_object($current) ? $current : null;

            if (!is_object($current) || !isset($current->response[$plugin])) {
                $this->offerSeenByUpgrader = null;

                return false;
            }

            $this->offerSeenByUpgrader = $current->response[$plugin];

            return true;
        };
    }

    /**
     * Run one whole command request: the arm, then the seam, exactly as
     * WordPress dispatches it.
     *
     * Nothing is reached into here. planSelfUpdate() arms, serveThenApply()
     * releases the response and applies, and the claims travel between them in
     * memory, which is the property under test.
     *
     * @param UpdateChecker $checker The checker under test.
     * @return array<string,mixed> The arm answer.
     */
    private function armAndApply(UpdateChecker $checker): array
    {
        $answer = $checker->planSelfUpdate();
        $checker->serveThenApply(false, null, null, null);

        return $answer;
    }

    /** The stored apply outcome record, or null. */
    private function record(): ?array
    {
        $stored = $this->options[AgentSelfUpdateCommand::OPTION_RESULT] ?? null;

        return is_array($stored) ? $stored : null;
    }

    /**
     * THE ASSERTION. Every green case below ends here.
     *
     * Not "a failure was not recorded", not "the transient exists": the offer
     * Plugin_Upgrader read carried OUR entry, naming the build this arm
     * verified, with the sentinel package that routes the download back through
     * verifyDownload()'s full re-verification.
     *
     * @return void
     */
    private function assertTheUpgraderReadOurOffer(): void
    {
        $this->assertSame(
            [UpdateChecker::PLUGIN_KEY],
            \Plugin_Upgrader::$calls,
            'the upgrade must have been attempted'
        );
        $this->assertIsObject(
            $this->offerSeenByUpgrader,
            'the offer Plugin_Upgrader::upgrade() read must contain an entry for this plugin'
        );
        $this->assertSame(
            $this->targetVersion,
            (string) $this->offerSeenByUpgrader->new_version,
            'and that entry must name the build THIS arm verified'
        );
        $this->assertSame(
            UpdateChecker::PACKAGE_SENTINEL,
            (string) $this->offerSeenByUpgrader->package,
            'and route the download through verifyDownload(), which re-verifies the signed manifest from scratch'
        );
        $this->assertSame('applied', (string) ($this->record()['status'] ?? ''));
    }

    // =========================================================================
    // T1 to T5: the states that used to fail
    // =========================================================================

    /**
     * T1. A HOSTILE pre_site_transient_update_plugins SHORT-CIRCUIT.
     *
     * This is the most likely explanation for the reported incident and the
     * reason a bare "rebuild the transient" fix could never have been enough.
     * get_site_transient() applies pre_site_transient_{$transient} first and
     * returns the moment it is handed anything but false
     * (wp-includes/option.php:2580 to :2584), never reaching the
     * site_transient_{$transient} filter at :2620 that injectUpdate() is bound
     * to. Any "disable updates" plugin, security plugin or managed-host
     * mu-plugin sitting there makes this apply fail on that one site, every
     * single time, while the mechanism works everywhere else.
     *
     * The apply wins because it registers LAST at PHP_INT_MAX, and apply_filters
     * runs every callback rather than stopping at the first non-false answer.
     */
    public function test_a_hostile_pre_site_transient_filter_cannot_starve_the_apply(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        // A third-party plugin that answers the read itself and offers nothing.
        add_filter('pre_site_transient_update_plugins', static function () {
            return (object) ['response' => [], 'no_update' => [], 'last_checked' => time()];
        }, 10);

        $this->armAndApply($checker);

        $this->assertTheUpgraderReadOurOffer();
    }

    /**
     * T2. THE STATE THE REPORTER DESCRIBED, and the reason it is NOT on its own
     * the cause of what they saw.
     *
     * GH #334 reported that update_plugins being a valid-but-stale object,
     * populated by WordPress's own periodic check without our entry, skipped
     * 0.61.108's `!is_object($offer)` rebuild and fell through to "the plugin
     * update transient carried no entry for this plugin". That reading of the
     * guard is correct, and the guard really was too narrow.
     *
     * But this state alone does not starve the apply, and this test is green
     * BOTH BEFORE AND AFTER the fix. It is deliberately kept and deliberately
     * labelled, because a test that cannot fail is worthless as a regression
     * test and actively misleading if it is presented as one.
     *
     * What it does establish is why: our entry is never STORED in
     * update_plugins, it is produced at read time by injectUpdate() on the
     * site_transient_update_plugins filter. So a stale object is decorated on
     * the very next read and the upgrader finds its offer regardless. The
     * fixture below dispatches the real filter chain, which is what makes that
     * visible; the older harness did a plain array lookup and could not see it.
     *
     * The real starvation needs something that stops injectUpdate() being
     * reached or being able to answer, which is what T1 (a hostile
     * pre_site_transient short circuit) and T3 (the manifest cache wiped
     * mid-request) cover. Those two are red before this fix and green after.
     */
    public function test_a_valid_transient_alone_does_not_starve_the_apply(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        // Exactly what WordPress's own periodic check leaves behind.
        $this->siteTransients['update_plugins'] = (object) [
            'last_checked' => time(),
            'checked'      => ['some-other-plugin/some-other-plugin.php' => '1.0.0'],
            'response'     => [],
            'no_update'    => ['some-other-plugin/some-other-plugin.php' => (object) ['new_version' => '1.0.0']],
        ];

        $this->armAndApply($checker);

        $this->assertTheUpgraderReadOurOffer();
    }

    /**
     * T3. THE MANIFEST CACHE IS DELETED MID-REQUEST, AND THE APPLY DOES NOT
     * CARE (the regression #328 introduced).
     *
     * awaitSiteUpdateLock() waits up to SITE_LOCK_WAIT_SECONDS between the arm
     * and the swap, and the process it waits on is precisely the one that calls
     * delete_site_transient('update_plugins') at the end of a plugin update.
     * That fires delete_site_transient_update_plugins, which is bound to
     * flushCache(), which deletes TRANSIENT_MANIFEST from another process.
     *
     * Simulated here at its most hostile: both caches are wiped after the arm
     * and before the apply. The apply must still install, and must not make a
     * single control-plane request to do it, because a blocking fetch inside
     * the apply is what writes the one-hour negative sentinel on failure and
     * poisons every retry for the next hour.
     */
    public function test_the_apply_survives_the_manifest_cache_being_deleted_mid_request(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $answer = $checker->planSelfUpdate();
        $this->assertSame('scheduled', $answer['status']);

        $fetchesAfterTheArm = $this->manifestFetches;
        $this->assertGreaterThan(0, $fetchesAfterTheArm, 'the arm itself verifies against the control plane');

        // A sibling update command finishes here and clears the plugin caches.
        delete_site_transient('update_plugins');
        $this->assertArrayNotHasKey(
            UpdateChecker::TRANSIENT_MANIFEST,
            $this->siteTransients,
            'flushCache() must have taken the manifest with it, or this test is not reproducing the race'
        );

        $checker->serveThenApply(false, null, null, null);

        $this->assertTheUpgraderReadOurOffer();
        $this->assertSame(
            $fetchesAfterTheArm,
            $this->manifestFetches,
            'the apply must not round-trip the control plane: on a failure that write is a one-hour "no update"'
        );
    }

    /**
     * T4. THE FORCED OFFER DOES NOT OUTLIVE THE APPLY.
     *
     * A pre_site_transient filter at PHP_INT_MAX beats every other plugin's
     * update machinery by construction, so it may exist for exactly one call to
     * Plugin_Upgrader::upgrade() and not one read longer. After the apply, a
     * site with nothing on offer must read as a site with nothing on offer.
     */
    public function test_the_forced_offer_is_withdrawn_and_cannot_affect_a_later_read(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->armAndApply($checker);
        $this->assertTheUpgraderReadOurOffer();

        // The world after the apply: nothing is published any more, and the
        // negative sentinel says so without a control-plane call.
        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = 'wpmgr-no-update';
        $this->siteTransients['update_plugins']                  = (object) [
            'last_checked' => time(),
            'response'     => [],
            'no_update'    => [],
        ];

        $later = get_site_transient('update_plugins');

        $this->assertIsObject($later);
        $this->assertArrayNotHasKey(
            UpdateChecker::PLUGIN_KEY,
            (array) $later->response,
            'the forced offer must not survive the apply that installed it'
        );
        $this->assertSame(
            [],
            array_filter($this->hooks['pre_site_transient_update_plugins'] ?? []),
            'and the filter that served it must have been unbound, not merely emptied of its value'
        );
    }

    /**
     * T5. A BUILD ALREADY ON DISK IS BENIGN, AND NO OFFER IS FORCED FOR IT.
     *
     * The already-at-target branch is decided FIRST, before any offer is built,
     * so the agent can never force an offer for a build it is already running.
     * Reporting this as a failure would halt a rollout wave over a site running
     * exactly the build it should.
     */
    public function test_a_build_already_on_disk_records_already_applied_and_forces_nothing(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $answer = $checker->planSelfUpdate();
        $this->assertSame('scheduled', $answer['status']);

        // The build lands some other way between the arm and the apply.
        $this->onDiskVersion = $this->targetVersion;

        $checker->serveThenApply(false, null, null, null);

        $this->assertSame([], \Plugin_Upgrader::$calls, 'nothing may be swapped onto a site already at the target');
        $this->assertSame('already_applied', (string) ($this->record()['status'] ?? ''));
        $this->assertSame(
            [],
            $this->hooks['pre_site_transient_update_plugins'] ?? [],
            'and no offer may be forced for a build that is already installed'
        );
    }

    // =========================================================================
    // The guards around the mechanism
    // =========================================================================

    /**
     * THE NEGATIVE CONTROL, and the test that would have rejected the fix this
     * issue was reported with.
     *
     * The suggested fix was to call delete_site_transient('update_plugins')
     * unconditionally at the top of the apply. That function fires
     * do_action("delete_site_transient_update_plugins") as its FIRST act
     * (wp-includes/option.php:2520), and flushCache() is bound to exactly that
     * action, so it would have deleted the verified manifest the arm wrote nine
     * lines earlier, forced a synchronous control-plane fetch inside the apply,
     * and on any transport failure written a one-hour "no update" that fails
     * this apply AND poisons every retry for the next hour.
     *
     * The apply may never delete that transient. Core's own
     * upgrader_process_complete binding clears the plugin caches after a
     * successful install, which is a different thing at a different time.
     */
    public function test_the_apply_never_deletes_the_plugin_update_transient(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->armAndApply($checker);

        $this->assertTheUpgraderReadOurOffer();
        $this->assertNotContains(
            'update_plugins',
            $this->deletedSiteTransients,
            'deleting update_plugins destroys the verified manifest through flushCache(); it is never the fix here'
        );
    }

    /**
     * The forced offer is a COMPLETE update-transient object.
     *
     * upgrader_process_complete fires while this is what every reader sees
     * (wp-admin/includes/class-plugin-upgrader.php:220), and third-party
     * listeners routinely read ->no_update, ->checked and ->last_checked. Under
     * PHP 8 a missing property is a warning, and a listener that promotes
     * warnings to exceptions would turn the agent's own self-update into a
     * fatal at the single most dangerous moment in the request.
     */
    public function test_the_forced_offer_is_a_complete_transient_object(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->armAndApply($checker);

        $seen = $this->transientSeenByUpgrader;
        $this->assertIsObject($seen);
        foreach (['response', 'no_update', 'checked', 'last_checked', 'translations'] as $property) {
            $this->assertObjectHasProperty(
                $property,
                $seen,
                'a listener reading ->' . $property . ' must not meet an undefined property warning'
            );
        }
    }

    /**
     * The forced offer has a shutdown backstop, because a FATAL inside the
     * upgrade skips runUpgrade()'s finally entirely.
     *
     * It must sit after core's rollback (shutdown, 10), core's cleanup (100)
     * and this agent's own restore guard (101), because a Throwable escaping a
     * shutdown callback abandons the rest of the queue. It must sit BEFORE the
     * outcome push, so the metadata that push sends is read from an unforced
     * transient.
     */
    public function test_a_fatal_skipping_the_finally_still_clears_the_forced_offer_at_shutdown(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->armAndApply($checker);

        $clear = null;
        foreach ($this->actions as $action) {
            if ($action['hook'] === 'shutdown'
                && is_array($action['callback'])
                && $action['callback'][1] === 'clearForcedOffer'
            ) {
                $clear = $action;
            }
        }

        $this->assertNotNull($clear, 'the forced offer must have a shutdown backstop');
        $this->assertGreaterThan(101, (int) $clear['priority'], 'it must run after core\'s rollback and this agent\'s guard');
        $this->assertLessThan(9998, (int) $clear['priority'], 'and before the outcome push, which reads site metadata');

        // And calling it twice is a no-op, since the finally has already run.
        $checker->clearForcedOffer();
        $checker->clearForcedOffer();
        $this->addToAssertionCount(1);
    }

    /**
     * The forced offer is scoped to OUR plugin key and nothing else, so it can
     * never route another plugin's update through this agent's package.
     */
    public function test_the_forced_offer_carries_only_our_own_entry(): void
    {
        $this->stubSignedOffer();
        $this->upgraderReadsTheOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->armAndApply($checker);

        $seen = $this->transientSeenByUpgrader;
        $this->assertIsObject($seen);
        $this->assertSame(
            [UpdateChecker::PLUGIN_KEY],
            array_keys((array) $seen->response),
            'a forced offer that named any other plugin would hand it this agent\'s package'
        );
    }

    /**
     * THE FIXTURE IS CHECKED AGAINST THE PRODUCTION BINDINGS.
     *
     * bindAgentFilters() reproduces install() by hand because install() carries
     * a process-global static guard. A harness that reproduces the wrong world
     * is worse than no harness at all, so the two hook names are read back out
     * of the production source.
     */
    public function test_install_binds_the_filters_this_fixture_reproduces(): void
    {
        $source = (string) file_get_contents(
            dirname(__DIR__) . '/includes/support/class-update-checker.php'
        );

        $this->assertStringContainsString(
            "add_filter('site_transient_update_plugins', [\$this, 'injectUpdate']);",
            $source,
            'the offer is produced by a filter on site_transient_update_plugins; this fixture binds the same one'
        );
        $this->assertStringContainsString(
            "add_action('delete_site_transient_update_plugins', [\$this, 'flushCache']);",
            $source,
            'and flushCache() is bound to the delete action, which is why deleting update_plugins is destructive here'
        );
    }

    // =========================================================================
    // The 12h positive cache, which the OTHER two installers still depend on
    // =========================================================================

    /**
     * AN INSTALL ATTEMPT CORRECTS THE 12 HOUR POSITIVE CACHE.
     *
     * The commanded apply now depends on no cache at all, but the other two
     * installers of this plugin do: the dashboard's own "Update now" and core's
     * auto-updater both read the offer injectUpdate() builds from claims cached
     * for 12 hours. Those claims can name a build the control plane has since
     * moved past.
     *
     * They can never INSTALL the wrong build, because verifyDownload() re-fetches
     * and re-verifies the signed manifest from scratch and installs whatever the
     * control plane publishes at that moment. What they could do is print a stale
     * version beside the button. verifyDownload() now writes the fresh claims
     * back over the cache, at the one moment the truth is in hand for free, so
     * any install attempt corrects the label for the next read.
     */
    /**
     * THE RECOVERY OFFER MUST NOT UNDO WHAT THE DOWNLOAD JUST SETTLED.
     *
     * Both run inside one apply: verifyDownload() first, then
     * primeRecoveryOffer() from applyPendingUpdate()'s finally, milliseconds
     * later. primeRecoveryOffer() writes whenever the arm's claims are still
     * newer than what is on disk, which after a FAILED apply is always true.
     *
     * This is the release-withdrawn case. The operator pulls the build
     * mid-rollout, the control plane answers no_update, verifyDownload()
     * retires the dead claims, and without a guard the finally writes them
     * straight back for another twelve hours, leaving the dashboard offering a
     * button that cannot work.
     */
    public function test_a_withdrawn_release_is_not_resurrected_by_the_recovery_offer(): void
    {
        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = [
            'version' => $this->targetVersion,
            'slug'    => 'wpmgr-agent',
        ];

        // The control plane now says there is nothing to install.
        $this->stubNoUpdate();

        $checker = $this->makeChecker();
        $checker->verifyDownload(false, UpdateChecker::PACKAGE_SENTINEL, null, null);

        $this->assertArrayNotHasKey(
            UpdateChecker::TRANSIENT_MANIFEST,
            $this->siteTransients,
            'a definitive no_update must retire the claims that led here'
        );

        // Now the finally runs. It must stand down rather than resurrect them.
        $this->invokeRecoveryOffer($checker, [
            'version' => $this->targetVersion,
            'slug'    => 'wpmgr-agent',
        ]);

        $this->assertArrayNotHasKey(
            UpdateChecker::TRANSIENT_MANIFEST,
            $this->siteTransients,
            'the recovery offer must not write back a build the control plane just withdrew'
        );
    }

    /**
     * THE OTHER HALF: a version the download corrected FORWARD must not be
     * downgraded back to the arm's older one.
     *
     * Reachable whenever the control plane publishes during the apply's own
     * window, which GH #328's site-lock wait widened to as much as 240
     * seconds.
     */
    public function test_a_corrected_version_is_not_downgraded_by_the_recovery_offer(): void
    {
        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = [
            'version' => '0.10.6',
            'slug'    => 'wpmgr-agent',
        ];

        $this->stubSignedOffer();

        $checker = $this->makeChecker();
        $checker->verifyDownload(false, UpdateChecker::PACKAGE_SENTINEL, null, null);

        $cached = $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] ?? null;
        $this->assertIsArray($cached);
        $this->assertSame($this->targetVersion, (string) ($cached['version'] ?? ''));

        // The arm's claims are OLDER than what the download just verified.
        $this->invokeRecoveryOffer($checker, [
            'version' => '0.10.6',
            'slug'    => 'wpmgr-agent',
        ]);

        $cached = $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] ?? null;
        $this->assertIsArray($cached);
        $this->assertSame(
            $this->targetVersion,
            (string) ($cached['version'] ?? ''),
            'the recovery offer must not downgrade a label the download just corrected forward'
        );
    }

    /**
     * And the guard must not disable the recovery offer outright: when
     * verifyDownload() never ran, a failed apply still leaves the dashboard
     * the claims this request verified.
     */
    public function test_the_recovery_offer_still_primes_when_no_download_ran(): void
    {
        unset($this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST]);

        $checker = $this->makeChecker();
        $this->invokeRecoveryOffer($checker, [
            'version' => $this->targetVersion,
            'slug'    => 'wpmgr-agent',
        ]);

        $cached = $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] ?? null;
        $this->assertIsArray(
            $cached,
            'with no download to defer to, the recovery path must still be primed'
        );
        $this->assertSame($this->targetVersion, (string) ($cached['version'] ?? ''));
    }

    public function test_a_download_refreshes_the_cached_claims_from_the_fresh_manifest(): void
    {
        // A cache entry from twelve hours ago naming a build that has been
        // superseded since.
        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = [
            'version' => '0.10.6',
            'slug'    => 'wpmgr-agent',
        ];

        $this->stubSignedOffer();

        $checker = $this->makeChecker();
        $result  = $checker->verifyDownload(false, UpdateChecker::PACKAGE_SENTINEL, null, null);

        // The download itself cannot succeed in this fixture (wp_remote_get is
        // stubbed with the manifest body, not a package), and that is fine:
        // what is under test is the cache write that precedes it.
        unset($result);

        $cached = $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] ?? null;
        $this->assertIsArray($cached);
        $this->assertSame(
            $this->targetVersion,
            (string) ($cached['version'] ?? ''),
            'the stale label must be corrected from the manifest this download just verified'
        );
        $this->assertArrayNotHasKey(
            'package_url',
            $cached,
            'and the presigned URL is a bearer credential that is never cached'
        );
    }

    /**
     * AN OVERTAKEN CACHED OFFER IS RETIRED BY THE FIRST READ THAT FINDS IT
     * FALSE, not answered from for the rest of its 12 hour window.
     *
     * Nothing can cache claims that are not newer than the on-disk build, since
     * verifyManifest()'s downgrade guard refuses them, so this state means the
     * build on disk moved after the entry was written and every install route
     * that moves it clears this cache. What is left is an out-of-band change: a
     * file copy, a deploy, a restore. Answering "no update" from it would hold
     * the dashboard's recovery path shut for up to twelve hours.
     */
    public function test_an_overtaken_cached_offer_is_retired_after_a_single_read(): void
    {
        $this->stubSignedOffer();

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        // Claims naming the build this site is already running.
        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = [
            'version' => $this->onDiskVersion,
            'slug'    => 'wpmgr-agent',
        ];
        $this->siteTransients['update_plugins'] = (object) [
            'last_checked' => time(),
            'response'     => [],
            'no_update'    => [],
        ];

        $first = get_site_transient('update_plugins');

        $this->assertIsObject($first);
        $this->assertArrayNotHasKey(UpdateChecker::PLUGIN_KEY, (array) $first->response);
        $this->assertSame(0, $this->manifestFetches, 'retiring a false entry must not cost a control-plane call');
        $this->assertArrayNotHasKey(
            UpdateChecker::TRANSIENT_MANIFEST,
            $this->siteTransients,
            'the entry the on-disk build overtook must not survive the read that found it false'
        );

        // The next read is a genuine miss, so it asks, and gets the truth.
        $second = get_site_transient('update_plugins');

        $this->assertIsObject($second);
        $this->assertSame(1, $this->manifestFetches);
        $this->assertArrayHasKey(UpdateChecker::PLUGIN_KEY, (array) $second->response);
        $this->assertSame(
            $this->targetVersion,
            (string) $second->response[UpdateChecker::PLUGIN_KEY]->new_version,
            'and the dashboard now names the build the control plane actually publishes'
        );
    }

    /**
     * A DEFINITIVE "NOTHING PUBLISHED" RETIRES THE CACHED OFFER THAT LED TO THE
     * ATTEMPT.
     *
     * Something read a cached entry and tried to install it; the control plane
     * answered 204. Those claims are provably false and must not keep offering a
     * button that cannot work.
     */
    public function test_a_control_plane_204_retires_the_cached_offer_that_led_to_it(): void
    {
        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = [
            'version' => $this->targetVersion,
            'slug'    => 'wpmgr-agent',
        ];

        Functions\when('wp_remote_get')->alias(function () {
            $this->manifestFetches++;

            return ['response' => ['code' => 204], 'body' => ''];
        });
        Functions\when('wp_remote_retrieve_response_code')->alias(fn ($response) => $response['response']['code'] ?? 0);
        Functions\when('wp_remote_retrieve_body')->alias(fn ($response) => $response['body'] ?? '');

        $checker = $this->makeChecker();
        $result  = $checker->verifyDownload(false, UpdateChecker::PACKAGE_SENTINEL, null, null);

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertArrayNotHasKey(
            UpdateChecker::TRANSIENT_MANIFEST,
            $this->siteTransients,
            'a withdrawn release must not keep being offered for the rest of the 12h window'
        );
    }

    /**
     * A CONTROL-PLANE OUTAGE RETIRES NOTHING.
     *
     * The counterpart to the test above, and the more important half. An answer
     * that did not arrive teaches nothing about what is published, so a control
     * plane that goes down must never be able to blank a fleet's update offers.
     */
    public function test_a_control_plane_outage_leaves_the_cached_offer_alone(): void
    {
        $cached = ['version' => $this->targetVersion, 'slug' => 'wpmgr-agent'];

        $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] = $cached;

        Functions\when('wp_remote_get')->alias(static fn () => new \WP_Error('http_request_failed', 'unreachable'));

        $checker = $this->makeChecker();
        $result  = $checker->verifyDownload(false, UpdateChecker::PACKAGE_SENTINEL, null, null);

        $this->assertInstanceOf(\WP_Error::class, $result);
        $this->assertSame(
            $cached,
            $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] ?? null,
            'an unreachable control plane must not cost this site its update offer'
        );
    }

    /**
     * A FAILED APPLY HANDS THE RECOVERY PATH A VERIFIED OFFER, over no network.
     *
     * A dashboard-initiated update is the documented recovery route for a build
     * whose commanded apply is broken, and it is the one installer this fix does
     * not carry claims into. So the apply leaves the claims it verified in this
     * request where injectUpdate() will find them, which is what stops the
     * recovery path paying a control-plane round trip that, if it fails, writes
     * a one-hour "no update" and hides the button exactly when it is needed.
     */
    public function test_a_failed_apply_hands_the_recovery_path_a_verified_offer(): void
    {
        $this->stubSignedOffer();
        \Plugin_Upgrader::$behaviour = static fn () => false;

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->assertSame('scheduled', $checker->planSelfUpdate()['status']);

        // A sibling update command clears the plugin caches while this apply is
        // still waiting for the site lock, taking the manifest with it.
        delete_site_transient('update_plugins');
        $fetchesAfterTheArm = $this->manifestFetches;

        $checker->serveThenApply(false, null, null, null);

        $this->assertSame('failed', (string) ($this->record()['status'] ?? ''));

        $cached = $this->siteTransients[UpdateChecker::TRANSIENT_MANIFEST] ?? null;
        $this->assertIsArray($cached, 'the recovery path must not be left reading an empty cache');
        $this->assertSame($this->targetVersion, (string) ($cached['version'] ?? ''));
        $this->assertArrayNotHasKey('package_url', $cached, 'a presigned bearer credential is never cached');
        $this->assertSame(
            $fetchesAfterTheArm,
            $this->manifestFetches,
            'and priming it costs no control-plane call, because the claims were verified in this request'
        );
    }

    /**
     * A SUCCESSFUL APPLY DOES NOT RE-PRIME THE CACHE IT JUST FLUSHED.
     *
     * The priming is self-guarding: it writes only while the verified build is
     * still newer than what is on disk. After a swap that landed, the site is at
     * the target, so nothing is written back over the flush and the next read
     * asks the control plane what comes next.
     */
    public function test_a_successful_apply_does_not_re_prime_the_cache_it_flushed(): void
    {
        $this->stubSignedOffer();
        \Plugin_Upgrader::$behaviour = function (string $plugin) {
            $current                       = get_site_transient('update_plugins');
            $this->transientSeenByUpgrader = is_object($current) ? $current : null;
            $this->offerSeenByUpgrader     = is_object($current) && isset($current->response[$plugin])
                ? $current->response[$plugin]
                : null;

            // The swap lands, so the new build is what the header reports now.
            $this->onDiskVersion = $this->targetVersion;

            return true;
        };

        $checker = $this->makeChecker();
        $this->bindAgentFilters($checker);

        $this->armAndApply($checker);

        $this->assertTheUpgraderReadOurOffer();
        $this->assertArrayNotHasKey(
            UpdateChecker::TRANSIENT_MANIFEST,
            $this->siteTransients,
            'an installed build leaves nothing to offer; the next read asks what comes after it'
        );
    }
}
