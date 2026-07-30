<?php
/**
 * UpdateChecker: CP-driven WordPress agent self-update (ADR-042 Phase 2).
 *
 * Hooks into the standard WordPress plugin-update machinery so the WPMgr agent
 * appears in the dashboard "Plugins with updates" list and can be installed with
 * one click — exactly like a wp.org plugin — while applying a full security
 * verification chain BEFORE any bytes are swapped to disk.
 *
 * Verification chain (verifyManifest), enforced in order, abort-on-first-failure:
 *   1. base64url-decode manifest + signature; reject if sizes wrong (sig must be
 *      exactly SODIUM_CRYPTO_SIGN_BYTES = 64 bytes).
 *   2. sodium_crypto_sign_verify_detached(sig, rawPayload, cpPubKeyRaw).
 *   3. json_decode rawPayload → claims; reject if not an array.
 *   4. hash_equals checks: cmd == "update_manifest", slug == "wpmgr-agent",
 *      aud == enrolled site_id.
 *   5. Temporal: exp is int, now <= exp (strict: reject if now > exp; allow
 *      60s clock-skew grace on iat — see inline comments). iat is int and
 *      iat <= now+60 (not absurdly future).
 *   6. jti non-empty; ReplayCache single-use (reject replays).
 *   7. Monotonic iat anti-rollback: persist highest accepted iat in wp-option
 *      wpmgr_agent_update_last_iat; reject if iat < last_iat. Update on accept.
 *   8. Downgrade guard: version_compare(claims.version, on-disk, '>') AND
 *      version_compare(on-disk, claims.min_version, '>=').
 *   9. Host allowlist on package_url: scheme must be exactly 'https', host must
 *      be an exact (hash_equals) match for a configured allowed host (baseline
 *      'storage.googleapis.com', replaceable via WPMGR_AGENT_PACKAGE_HOST for
 *      self-hosted object storage, always unioned with the agent's own
 *      control-plane host, and absolutely overridable via the
 *      'wpmgr_agent_package_hosts' filter; see allowedPackageHosts); the download
 *      itself uses redirection=>0. package_size must be > 0 and <= MAX_PACKAGE_BYTES.
 *      An exact-host allowlist inherently rejects literal IPs (incl. the cloud
 *      metadata IP) and look-alike hosts, which is the anti-SSRF boundary.
 *
 * Security invariants:
 *   - No field from the manifest is trusted before step 2 (signature) passes.
 *   - package_url is NEVER logged or cached (it is a short-lived bearer
 *     credential; it is stripped from the cached claims in injectUpdate()).
 *   - All string comparisons use hash_equals.
 *   - WP_Error is returned on any download/sha failure (aborts the update
 *     visibly; temp file is unlinked before returning).
 *
 * This file also carries the CP-commanded self-update (ARM plus APPLY inside
 * the signed command request, CONFIRM by the new code phoning home). It lives
 * HERE, and not in a new file, because `make agent-zip-wporg` physically
 * excludes this one path from the wp.org distribution build; see the block
 * comment above planSelfUpdate().
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

use WPMgr\Agent\Commands\AgentSelfUpdateCommand;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\ReplayCache;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;

/**
 * Hooks the WPMgr agent into WordPress plugin-update machinery, with a
 * CP-signed manifest verified before any install occurs.
 */
final class UpdateChecker
{
    // -------------------------------------------------------------------------
    // Constants
    // -------------------------------------------------------------------------

    /** CP endpoint path for fetching the signed manifest. */
    private const MANIFEST_PATH = '/agent/v1/update/manifest';

    /** Plugin key used by WordPress to identify our plugin. */
    public const PLUGIN_KEY = 'wpmgr-agent/wpmgr-agent.php';

    /** Plugin slug (also the folder name inside wp-content/plugins/). */
    public const PLUGIN_SLUG = 'wpmgr-agent';

    /** Sentinel package value injected into the transient (NOT a real URL). */
    public const PACKAGE_SENTINEL = 'wpmgr-agent-self-update';

    /** Site-transient key for the 12h-cached verified manifest claims. */
    public const TRANSIENT_MANIFEST = 'wpmgr_agent_update_manifest';

    /**
     * Sentinel cached in TRANSIENT_MANIFEST to negative-cache the common
     * "no update published" (HTTP 204) / verification-miss path. Without it,
     * injectUpdate() re-fetches the CP manifest synchronously on EVERY admin
     * page load (site_transient_update_plugins fires per request), adding a
     * blocking round-trip — and up to GET_TIMEOUT seconds of hang on CP latency
     * — to every wp-admin load. Cached for a shorter window than a positive hit
     * so a newly published update is still picked up within ~1h.
     */
    private const NO_UPDATE_SENTINEL = 'wpmgr-no-update';

    /** Negative-cache TTL (seconds) for the no-update sentinel. */
    private const NO_UPDATE_TTL = 3600;

    /**
     * wp-option key for the monotonic anti-rollback iat counter.
     * Holds the highest iat accepted from a valid manifest. Initialised to 0 on
     * first use (any manifest iat >= 0 is accepted the very first time).
     */
    public const OPTION_LAST_IAT = 'wpmgr_agent_update_last_iat';

    /**
     * Absolute maximum package size in bytes (64 MiB). A manifest claiming a
     * larger package is rejected — this guards against zip-bomb or against a
     * manipulated package_size driving an unbounded memory allocation in the
     * download step.
     */
    public const MAX_PACKAGE_BYTES = 64 * 1024 * 1024;

    /**
     * Core's own cross-request upgrader lock for this channel, taken with
     * WP_Upgrader::create_lock() and released with WP_Upgrader::release_lock().
     * One apply per site at a time, decided by a single INSERT IGNORE, which is
     * the same primitive WordPress uses to serialise its own background
     * updates. It replaces the hand-rolled claim, in-flight marker and stage
     * token this channel used to carry.
     *
     * The release timeout handed to create_lock() is
     * LongRunningJob::TIME_LIMIT_SECONDS (900), deliberately the SAME number as
     * the PHP execution budget the apply arms for itself: the lock must outlive
     * an apply that runs to its own cap and not one second longer, so a worker
     * killed mid apply blocks the next attempt for exactly as long as it could
     * conceivably still have been alive. Pinned by a test.
     */
    private const APPLY_LOCK = 'wpmgr_agent_self_update';

    /**
     * wp-option holding the apply id of the run that currently owns APPLY_LOCK.
     *
     * Core's lock row stores a timestamp and nothing else, so without this an
     * arm that finds the lock held could name no apply at all, and the control
     * plane would be left confirming on version movement alone, which is
     * exactly the unattributed confirmation this channel is trying to stop
     * making. Written by the run that takes the lock, echoed to any later arm
     * that finds it held, deleted with the lock.
     *
     * Read only on the rare request that is arming a self-update, so it is
     * deliberately not autoloaded.
     */
    private const OPTION_APPLY_ID = 'wpmgr_agent_self_update_apply_id';

    /**
     * Every agent class that can still be reached AFTER the plugin directory
     * has been swapped. warmPostSwapClasses() loads each one before the swap
     * starts, so the code that runs between the swap and the end of the request
     * is already in memory when its file stops existing.
     *
     * The post-swap surface is finite and enumerable here, which is the whole
     * reason the apply moved into the request body: what remains after the swap
     * is the return into WP_REST_Server::serve_request(), the die() that
     * follows it, and the shutdown queue. A test walks every
     * add_action('shutdown', ...) binding in the agent and fails when a
     * callback's class is missing from this list, because a list like this rots
     * silently otherwise.
     *
     * @var list<string>
     */
    public const POST_SWAP_CLASSES = [
        'WPMgr\\Agent\\Support\\Maintenance',
        'WPMgr\\Agent\\Support\\DebugLog',
        'WPMgr\\Agent\\Support\\LongRunningJob',
        'WPMgr\\Agent\\Support\\ErrorMonitor',
        'WPMgr\\Agent\\Support\\UpdateGuard',
        'WPMgr\\Agent\\Support\\ConnectionFinisher',
        'WPMgr\\Agent\\Support\\UpdateChecker',
        'WPMgr\\Agent\\Commands\\AgentSelfUpdateCommand',
        'WPMgr\\Agent\\HeartbeatCatchup',
        'WPMgr\\Agent\\Enrollment',
        'WPMgr\\Agent\\Settings',
        'WPMgr\\Agent\\Plugin',
    ];

    /**
     * Shutdown priority of restoreAgentIfDirectoryMissing().
     *
     * ABOVE core's two temp-backup callbacks, and that is the whole point.
     * WP_Upgrader::run() registers restore_temp_backup at priority 10
     * (class-wp-upgrader.php:936) and delete_temp_backup at 100 (:953). A fatal
     * raised inside a shutdown callback abandons the rest of the queue, so a
     * guard placed AHEAD of core's restore could kill the very rollback it
     * exists to back up. Running after both gives identical coverage and can
     * shadow neither: on the WP_Error path core has already restored, so this
     * guard's precondition is false; on the path core never got to register
     * anything (a max_execution_time fatal raised inside install_package) this
     * guard is the only thing left.
     */
    private const RESTORE_GUARD_PRIORITY = 101;

    /**
     * Shutdown priority of pushOutcomeNow(). Sits after the restore guard, so
     * what it pushes is the post-rollback truth rather than a mid-flight guess.
     */
    private const OUTCOME_PUSH_PRIORITY = 9998;

    /**
     * Clock-skew grace for the exp field (seconds). The agent rejects a manifest
     * whose exp is in the past, but allows up to this many seconds of clock drift
     * between the CP and the agent host. We apply NO grace on the exp upper bound
     * (the manifest TTL is already 300s) but we accept exp up to SKEW_GRACE_S
     * seconds in the past to tolerate a slow agent clock.
     *
     * Decision: "reject if now > exp + SKEW_GRACE_S" — a manifest whose nominal
     * exp is up to 60s ago is still accepted. This is clearly commented below.
     */
    private const SKEW_GRACE_S = 60;

    /** Outbound GET timeout in seconds. */
    private const GET_TIMEOUT = 10;

    /**
     * Whole-operation timeout, in seconds, for the PACKAGE download in
     * verifyDownload(). Distinct from GET_TIMEOUT, which covers the small JSON
     * manifest fetch. This is a cURL total-transfer cap and not an idle cap: the
     * transfer gets this long in wall clock from the first byte requested to the
     * last byte written, so it is exactly the slowest AVERAGE rate a site may
     * sustain and still self-update.
     *
     * WHY 60 WAS TOO LOW. The published package is about 3.4 MiB (3405063 bytes
     * at the time of writing). At 60s that demanded 3405063/60 = 56751 B/s,
     * roughly 55 KB/s sustained across the whole transfer. The slow-consumer
     * band the control plane's own package-stream work measured and set out to
     * carry sat between 25 and 40 KB/s, entirely BELOW that floor: those sites
     * downloaded happily for 60 seconds, were cut off mid-file, failed the size
     * check, and retried the identical failure every six hours forever. The
     * control plane now bounds PROGRESS rather than duration and carries a
     * transfer that keeps moving up to a size-derived ceiling
     * (packageStreamTotalLimit in
     * apps/api/internal/agent/update_package_limits.go, about 55 minutes for a
     * package this size), which left this value as the only floor on the path.
     *
     * WHY 300. The same package now demands 3405063/300 = 11350 B/s, about
     * 11 KB/s. That is under half the bottom of the measured band, so a site at
     * 25 KB/s finishes in 133s and one at 40 KB/s in 83s, both with room to
     * spare. It is also short enough that a transfer which is genuinely dead
     * rather than merely slow parks one cron request for five minutes, not for
     * the quarter hour PHP would otherwise let it hold.
     *
     * HOW THIS RELATES TO THE EXECUTION LIMIT, WHICH IS THE POINT. The apply
     * path arms LongRunningJob::TIME_LIMIT_SECONDS (900s) through
     * set_time_limit() before any of this runs, and re-arms it immediately
     * before the transfer below, so this budget always starts against a full
     * 900s PHP clock. 300 is one third of 900. A download that burns its ENTIRE
     * budget therefore still leaves 600s for the unzip, the non-atomic copy and
     * the reactivation that follow it. cURL is guaranteed to give up before PHP
     * does, which is what makes a too-slow site fail as a clean WP_Error with
     * the temp file discarded rather than as a max_execution_time fatal partway
     * through the write, leaving a partial temp file behind. THAT ORDERING IS
     * THE INVARIANT: this value must stay well under the execution limit, and
     * moving either number without the other reintroduces the defect.
     *
     * COUPLED TO THE PACKAGE SIZE. This is a fixed wall-clock cap sized for a
     * 3.4 MiB package. MAX_PACKAGE_BYTES permits 64 MiB, and a package near that
     * would need 218 KB/s to fit in 300s, which is the original defect again at
     * a different scale. If the shipped package grows materially, recompute the
     * implied rate before shipping it.
     */
    private const PACKAGE_TIMEOUT = 300;

    // -------------------------------------------------------------------------
    // Collaborators
    // -------------------------------------------------------------------------

    private Signer $signer;

    private Settings $settings;

    private Keystore $keystore;

    private ReplayCache $replayCache;

    /**
     * The SAPI-aware early-flush ladder. Held rather than constructed on the
     * spot so the rung that actually fired and the explanation recorded beside
     * it come from one probe rather than from two that could disagree.
     *
     * Releasing the connection first is a preference, never a precondition. On
     * the last rung the apply runs anyway, attached, which is what WordPress
     * itself does on the same hosts through wp-admin every day.
     */
    private ConnectionFinisher $finisher;

    /**
     * The ConnectionFinisher rung the apply in THIS request ran on, or '' when
     * no apply ran here.
     *
     * Stamped into every apply-result record so a fleet-wide reading can tell
     * which sites upgraded on an attached connection from which upgraded on a
     * released one. It is an observation and nothing reads it back to decide
     * anything.
     */
    private string $applyRung = '';

    /**
     * Why the most recent fetchManifest()/verifyManifest() pair returned null.
     *
     * fetchManifest() collapses "the CP published nothing" and "the manifest
     * failed verification" into the same null return, which is exactly the
     * right shape for the transient-injection path but not for the ARM beat of
     * the self-update protocol: the control plane has to be able to tell
     * "up_to_date" (benign, nothing to do) apart from "error" (worth surfacing).
     * Every terminal path of the two methods tags itself here so
     * planSelfUpdate() can classify a null without re-running any check.
     *
     * Values: '' (never run), 'ok', 'no_update' (HTTP 204), 'up_to_date'
     * (signed manifest verified but not newer than on-disk), 'unavailable'
     * (transport/config), 'invalid' (unparseable body), 'rejected' (any other
     * verification-chain failure).
     */
    private string $lastManifestOutcome = '';

    /**
     * The apply this request armed and has not yet run, or null.
     *
     * Set by planSelfUpdate() once the manifest is verified and the apply lock
     * is held, read exactly once by serveThenApply() after the acknowledgement
     * has been released to the control plane. Nulled as it is read, so a second
     * dispatch of the filter can never start a second apply.
     *
     * @var array{apply_id:string,from:string,to:string}|null
     */
    private ?array $pendingApply = null;

    /**
     * The Plugin_Upgrader instance of the apply that is running, or null.
     *
     * Held on the object only so the shutdown guard can call core's own public
     * restorer with core's own argument shape. Nulled when the apply returns.
     *
     * @var object|null
     */
    private ?object $upgrader = null;

    /**
     * True once a recorded outcome has queued its own metadata push, so a
     * request cannot queue two.
     */
    private bool $outcomePushQueued = false;

    // -------------------------------------------------------------------------
    // Constructor
    // -------------------------------------------------------------------------

    /**
     * @param Signer                 $signer      Builds the four X-WPMgr-* auth headers.
     * @param Settings               $settings    Provides isEnrolled(), siteId(), cpUrl().
     * @param Keystore               $keystore    Provides the CP Ed25519 public key.
     * @param ReplayCache            $replayCache jti single-use store.
     * @param ConnectionFinisher|null $finisher   The early-flush ladder. Injectable
     *                                            because neither finish-request
     *                                            function can be defined in a test
     *                                            process without flipping
     *                                            function_exists() for every other
     *                                            test in it, so the rung a test wants
     *                                            to exercise has to be handed in.
     */
    public function __construct(
        Signer $signer,
        Settings $settings,
        Keystore $keystore,
        ReplayCache $replayCache,
        ?ConnectionFinisher $finisher = null
    ) {
        $this->signer      = $signer;
        $this->settings    = $settings;
        $this->keystore    = $keystore;
        $this->replayCache = $replayCache;
        $this->finisher    = $finisher ?? new ConnectionFinisher();
    }

    // -------------------------------------------------------------------------
    // install() — bind all hooks (idempotent)
    // -------------------------------------------------------------------------

    /**
     * Register WordPress hooks for the update channel. Idempotent (static guard).
     * Self-gates on isEnrolled(): there is nothing to do on unenrolled sites.
     *
     * @return void
     */
    public function install(): void
    {
        static $installed = false;
        if ($installed) {
            return;
        }
        $installed = true;

        if (!$this->settings->isEnrolled()) {
            return;
        }

        if (!function_exists('add_filter') || !function_exists('add_action')) {
            return;
        }

        add_filter('site_transient_update_plugins', [$this, 'injectUpdate']);
        add_filter('plugins_api', [$this, 'pluginInfo'], 20, 3);
        add_filter('upgrader_pre_download', [$this, 'verifyDownload'], 10, 4);
        add_filter('upgrader_source_selection', [$this, 'renameSource'], 10, 4);
        // The "Check again" button in Plugins > Updates triggers this action.
        add_action('delete_site_transient_update_plugins', [$this, 'flushCache']);
    }

    // -------------------------------------------------------------------------
    // fetchManifest() — signed GET to the CP manifest endpoint
    // -------------------------------------------------------------------------

    /**
     * Fetch and verify a fresh signed manifest from the control plane.
     *
     * Returns the verified claims array (never containing package_url — callers
     * that need the URL call fetchManifest() again at install time). Returns null
     * on HTTP 204 (no update published) or on ANY verification failure.
     *
     * package_url is intentionally not cached here and is fetched fresh each time
     * it is needed, because it is a short-lived presigned HTTPS credential.
     *
     * @return array<string,mixed>|null Verified claims, or null.
     */
    public function fetchManifest(): ?array
    {
        // Default tag for every transport/config bail-out below; the parse and
        // verification steps overwrite it with a more specific value.
        $this->lastManifestOutcome = 'unavailable';

        $base = $this->settings->controlPlaneUrl();
        if ($base === '') {
            return null;
        }

        $path = self::MANIFEST_PATH;

        try {
            $authHeaders = $this->signer->signHeaders('GET', $path, '');
        } catch (\Throwable $e) {
            error_log('wpmgr-agent: UpdateChecker could not sign manifest request: ' . $e->getMessage());
            return null;
        }

        $headers = array_merge(
            ['Accept' => 'application/json'],
            $authHeaders
        );

        if (!function_exists('wp_remote_get')) {
            return null;
        }

        $response = wp_remote_get(
            $base . $path,
            [
                'timeout'     => self::GET_TIMEOUT,
                'redirection' => 0,
                'headers'     => $headers,
            ]
        );

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            error_log('wpmgr-agent: UpdateChecker manifest request failed (wp_error).');
            return null;
        }

        $status = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        if ($status === 204) {
            // No update published — this is normal, not an error.
            $this->lastManifestOutcome = 'no_update';
            return null;
        }

        if ($status !== 200) {
            error_log(sprintf('wpmgr-agent: UpdateChecker manifest endpoint returned HTTP %d.', $status));
            return null;
        }

        // SECURITY: $rawBody (and the decoded manifest) contains the short-lived
        // presigned package_url — a bearer credential. NEVER error_log($rawBody),
        // the envelope, or the decoded claims. Only log generic, non-secret
        // diagnostics below.
        $rawBody = function_exists('wp_remote_retrieve_body')
            ? (string) wp_remote_retrieve_body($response)
            : '';

        if ($rawBody === '') {
            error_log('wpmgr-agent: UpdateChecker manifest response body is empty.');
            $this->lastManifestOutcome = 'invalid';
            return null;
        }

        $envelope = json_decode($rawBody, true);
        if (!is_array($envelope)) {
            error_log('wpmgr-agent: UpdateChecker manifest response is not valid JSON.');
            $this->lastManifestOutcome = 'invalid';
            return null;
        }

        return $this->verifyManifest($envelope);
    }

    // -------------------------------------------------------------------------
    // verifyManifest() — the security core
    // -------------------------------------------------------------------------

    /**
     * Verify a manifest envelope returned by the CP.
     *
     * Enforces the full verification chain defined in ADR-042. Returns the
     * verified claims on full success; returns null and logs a concise (non-
     * secret) warning on the first failure.
     *
     * The package_url field is present in the returned claims when all checks
     * pass, but callers that only need to know "is there an update?" should strip
     * it before caching (see injectUpdate). verifyDownload always re-fetches.
     *
     * @param array<string,mixed> $envelope {'manifest': <b64url>, 'signature': <b64url>}
     * @return array<string,mixed>|null Verified claims or null.
     */
    public function verifyManifest(array $envelope): ?array
    {
        // Default tag for every rejection below; step 8 narrows the benign
        // "verified but not newer" case to 'up_to_date' and the success path
        // tags 'ok'. See $lastManifestOutcome for why this distinction exists.
        $this->lastManifestOutcome = 'rejected';

        // ---- Step 1: base64url-decode manifest + signature ------------------
        $manifestB64 = isset($envelope['manifest']) && is_string($envelope['manifest'])
            ? $envelope['manifest']
            : '';
        $signatureB64 = isset($envelope['signature']) && is_string($envelope['signature'])
            ? $envelope['signature']
            : '';

        if ($manifestB64 === '' || $signatureB64 === '') {
            error_log('wpmgr-agent: UpdateChecker manifest envelope missing manifest or signature field.');
            return null;
        }

        $payloadRaw = $this->base64UrlDecode($manifestB64);
        $sigRaw     = $this->base64UrlDecode($signatureB64);

        if ($payloadRaw === '') {
            error_log('wpmgr-agent: UpdateChecker manifest field could not be base64url-decoded.');
            return null;
        }

        if ($sigRaw === '') {
            error_log('wpmgr-agent: UpdateChecker signature field could not be base64url-decoded.');
            return null;
        }

        // Signature must be exactly SODIUM_CRYPTO_SIGN_BYTES (64 bytes).
        if (strlen($sigRaw) !== SODIUM_CRYPTO_SIGN_BYTES) {
            error_log(sprintf(
                'wpmgr-agent: UpdateChecker signature has wrong length (%d bytes, expected %d).',
                strlen($sigRaw),
                SODIUM_CRYPTO_SIGN_BYTES
            ));
            return null;
        }

        // ---- Step 2: Ed25519 signature verification -------------------------
        // Verify over the EXACT decoded bytes (payloadRaw), not a re-serialized
        // version. This is critical: any re-encoding could change the bytes and
        // break the signature even for a valid manifest.
        $cpPubKeyRaw = $this->keystore->getControlPlanePublicKey();
        if ($cpPubKeyRaw === null || strlen($cpPubKeyRaw) !== SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES) {
            error_log('wpmgr-agent: UpdateChecker CP public key not provisioned or invalid length.');
            return null;
        }

        $sigValid = sodium_crypto_sign_verify_detached($sigRaw, $payloadRaw, $cpPubKeyRaw);
        if ($sigValid !== true) {
            error_log('wpmgr-agent: UpdateChecker Ed25519 signature verification failed.');
            return null;
        }

        // ---- Step 3: parse claims -------------------------------------------
        // Only parse the payload AFTER the signature is verified. Nothing from
        // the envelope is trusted until this point.
        $claims = json_decode($payloadRaw, true);
        if (!is_array($claims)) {
            error_log('wpmgr-agent: UpdateChecker manifest payload is not valid JSON.');
            return null;
        }

        // ---- Step 4: required claim checks (constant-time) ------------------
        $cmd  = isset($claims['cmd'])  && is_string($claims['cmd'])  ? $claims['cmd']  : '';
        $slug = isset($claims['slug']) && is_string($claims['slug']) ? $claims['slug'] : '';
        $aud  = isset($claims['aud'])  && is_string($claims['aud'])  ? $claims['aud']  : '';

        if (!hash_equals('update_manifest', $cmd)) {
            error_log('wpmgr-agent: UpdateChecker manifest cmd claim mismatch.');
            return null;
        }
        if (!hash_equals('wpmgr-agent', $slug)) {
            error_log('wpmgr-agent: UpdateChecker manifest slug claim mismatch.');
            return null;
        }

        $siteId = $this->settings->siteId();
        if ($siteId === '') {
            error_log('wpmgr-agent: UpdateChecker site_id not set (not enrolled).');
            return null;
        }
        if (!hash_equals($siteId, $aud)) {
            error_log('wpmgr-agent: UpdateChecker manifest aud claim mismatch.');
            return null;
        }

        // ---- Step 5: temporal checks ----------------------------------------
        $now = time();

        $exp = isset($claims['exp']) && is_int($claims['exp']) ? $claims['exp'] : null;
        if ($exp === null) {
            error_log('wpmgr-agent: UpdateChecker manifest missing or non-integer exp claim.');
            return null;
        }
        // Clock-skew: reject if now > exp + SKEW_GRACE_S.
        // A manifest expires at 'exp' but we tolerate up to SKEW_GRACE_S seconds
        // of local clock lag. This means a CP that issues a manifest with TTL=300s
        // is honoured up to 60s past its expiry on a slow-clock agent.
        // We do NOT apply grace to future exp: an exp far in the future (like a
        // command token) is still accepted here because the manifest legitimately
        // has exp=iat+300.
        if ($now > $exp + self::SKEW_GRACE_S) {
            error_log('wpmgr-agent: UpdateChecker manifest is expired (exp=' . $exp . ', now=' . $now . ').');
            return null;
        }

        $iat = isset($claims['iat']) && is_int($claims['iat']) ? $claims['iat'] : null;
        if ($iat === null) {
            error_log('wpmgr-agent: UpdateChecker manifest missing or non-integer iat claim.');
            return null;
        }
        // Reject if iat is absurdly in the future (more than 60s ahead).
        if ($iat > $now + 60) {
            error_log('wpmgr-agent: UpdateChecker manifest iat is too far in the future.');
            return null;
        }

        // ---- Step 6: jti single-use (anti-replay) ---------------------------
        $jti = isset($claims['jti']) && is_string($claims['jti']) ? $claims['jti'] : '';
        if ($jti === '') {
            error_log('wpmgr-agent: UpdateChecker manifest missing or empty jti claim.');
            return null;
        }

        // The manifest TTL is up to 300s + 60s skew; use a 600s window to be
        // safe (covers the full skew-extended validity + replay budget).
        $replayTtl = max(600, ($exp - $iat) + self::SKEW_GRACE_S * 2);

        if ($this->replayCache->seen($jti)) {
            error_log('wpmgr-agent: UpdateChecker manifest jti replay detected.');
            return null;
        }
        if (!$this->replayCache->mark($jti, $replayTtl)) {
            // mark() returns false on insert failure (e.g. DB unavailable or
            // duplicate key — both should be treated as potential replay).
            error_log('wpmgr-agent: UpdateChecker could not mark jti (treating as replay).');
            return null;
        }

        // ---- Step 7: monotonic iat anti-rollback ----------------------------
        // Reject any manifest whose iat is older than the highest iat we've
        // previously accepted. This prevents an attacker who has captured a valid
        // older manifest from replaying it (step 6 handles the jti, but a fresh
        // jti with an old iat would slip through without this check).
        // Tolerate SKEW_GRACE_S of BACKWARD drift: the CP runs many Cloud Run
        // instances, and the install-time fetch may be served by an instance whose
        // clock trails the one that served the earlier check. A genuinely replayed
        // OLD manifest is minutes+ stale, so this tolerance does not weaken
        // rollback protection. Persist max(iat,last) so an in-tolerance accept can
        // never LOWER the high-water mark.
        $lastIat = $this->getLastAcceptedIat();
        if ($iat < $lastIat - self::SKEW_GRACE_S) {
            error_log(sprintf(
                'wpmgr-agent: UpdateChecker manifest iat (%d) < last accepted iat (%d) — anti-rollback rejection.',
                $iat,
                $lastIat
            ));
            return null;
        }
        $this->setLastAcceptedIat(max($iat, $lastIat));

        // ---- Step 8: downgrade guard ----------------------------------------
        $claimedVersion = isset($claims['version'])     && is_string($claims['version'])     ? $claims['version']     : '';
        $minVersion     = isset($claims['min_version']) && is_string($claims['min_version']) ? $claims['min_version'] : '';

        // On-disk version comes from the plugin file header, NOT from the CP.
        $onDisk = $this->onDiskVersion();

        if ($claimedVersion === '') {
            error_log('wpmgr-agent: UpdateChecker manifest missing version claim.');
            return null;
        }
        // Compare the NORMALIZED (bare-semver) cores so a dev-suffixed on-disk
        // version cannot be side-graded. PHP version_compare() treats
        // '0.10.5-cron-selfheal' as a pre-release of (i.e. LOWER than) '0.10.5',
        // which would otherwise let a manifest 'version: 0.10.5' pass as "newer"
        // (security review finding 2). normalizeVersion() strips the suffix so a
        // real update MUST bump the numeric core.
        if (!version_compare($this->normalizeVersion($claimedVersion), $this->normalizeVersion($onDisk), '>')) {
            error_log(sprintf(
                'wpmgr-agent: UpdateChecker downgrade guard: manifest version %s is not newer than on-disk %s.',
                $claimedVersion,
                $onDisk
            ));
            // A correctly signed manifest that simply is not newer is the
            // steady state, not a failure. Tag it so the ARM can answer
            // "up_to_date" instead of "error".
            $this->lastManifestOutcome = 'up_to_date';
            return null;
        }
        // min_version is mandatory (the CP always sets at least 0.0.0). An empty
        // floor is rejected rather than silently skipped (security review finding 5).
        if ($minVersion === '') {
            error_log('wpmgr-agent: UpdateChecker manifest missing min_version claim.');
            return null;
        }
        if (!version_compare($this->normalizeVersion($onDisk), $this->normalizeVersion($minVersion), '>=')) {
            error_log(sprintf(
                'wpmgr-agent: UpdateChecker downgrade guard: on-disk version %s is below min_version %s.',
                $onDisk,
                $minVersion
            ));
            return null;
        }

        // ---- Step 9: host allowlist on package_url (anti-SSRF) --------------
        $packageUrl  = isset($claims['package_url'])  && is_string($claims['package_url'])  ? $claims['package_url']  : '';
        $packageSize = isset($claims['package_size']) && is_int($claims['package_size'])    ? $claims['package_size'] : 0;
        $packageSha  = isset($claims['package_sha256']) && is_string($claims['package_sha256']) ? $claims['package_sha256'] : '';

        if ($packageUrl === '') {
            error_log('wpmgr-agent: UpdateChecker manifest missing package_url.');
            return null;
        }

        $parsed = function_exists('wp_parse_url') ? wp_parse_url($packageUrl) : parse_url($packageUrl);
        if (!is_array($parsed)) {
            error_log('wpmgr-agent: UpdateChecker package_url could not be parsed.');
            return null;
        }

        $scheme = isset($parsed['scheme']) && is_string($parsed['scheme']) ? strtolower($parsed['scheme']) : '';
        $host   = isset($parsed['host'])   && is_string($parsed['host'])   ? $parsed['host']              : '';

        // Scheme must be exactly 'https' (constant-time compare).
        if (!hash_equals('https', $scheme)) {
            error_log('wpmgr-agent: UpdateChecker package_url scheme is not https.');
            return null;
        }
        // Host must be in the configured allowlist (constant-time). The baseline
        // is the managed CP's GCS host, the agent's own control-plane host is
        // always included, and a self-hosted deployment can further adjust the
        // list via the WPMGR_AGENT_PACKAGE_HOST constant or the
        // 'wpmgr_agent_package_hosts' filter (see allowedPackageHosts, which
        // documents which layer replaces and which unions). A literal IP, a
        // look-alike host, or
        // the cloud-metadata IP (169.254.169.254) never matches an allowlisted
        // hostname, so this single exact-host check is the anti-SSRF boundary.
        if (!$this->isAllowedPackageHost($host)) {
            error_log('wpmgr-agent: UpdateChecker package_url host is not in the allowlist.');
            return null;
        }

        // Size clamp.
        if ($packageSize <= 0 || $packageSize > self::MAX_PACKAGE_BYTES) {
            error_log(sprintf(
                'wpmgr-agent: UpdateChecker package_size %d is invalid (must be 1..%d).',
                $packageSize,
                self::MAX_PACKAGE_BYTES
            ));
            return null;
        }

        if ($packageSha === '') {
            error_log('wpmgr-agent: UpdateChecker manifest missing package_sha256.');
            return null;
        }

        // All checks passed. Return the full verified claims.
        // Note: package_url IS included here so verifyDownload can use it.
        // injectUpdate() strips it before caching.
        $this->lastManifestOutcome = 'ok';

        /** @var array<string,mixed> $claims */
        return $claims;
    }

    // -------------------------------------------------------------------------
    // injectUpdate() — site_transient_update_plugins filter
    // -------------------------------------------------------------------------

    /**
     * Inject our update (or no_update) entry into the plugin-update transient.
     *
     * Uses a 12h-cached copy of the verified manifest claims (package_url stripped
     * before caching — it is a short-lived bearer credential). On cache miss,
     * calls fetchManifest() and caches the result.
     *
     * @param mixed $transient The current site transient value.
     * @return mixed Modified transient.
     */
    public function injectUpdate($transient)
    {
        if (!is_object($transient)) {
            return $transient;
        }

        // Read the 12h-cached verified claims.
        $claims = function_exists('get_site_transient')
            ? get_site_transient(self::TRANSIENT_MANIFEST)
            : false;

        // Negative-cache hit: a prior check found no update. Skip the CP call.
        // (Must precede the !is_array() branch — the sentinel is a string, which
        // would otherwise be treated as a cache miss and re-fetch every request.)
        if ($claims === self::NO_UPDATE_SENTINEL) {
            $claims = null;
        } elseif (!is_array($claims)) {
            // Cache miss — fetch and cache fresh (package_url stripped).
            $fresh = $this->fetchManifest();
            if (is_array($fresh)) {
                $toCache = $fresh;
                // Strip the presigned URL — it must not be cached.
                unset($toCache['package_url']);
                if (function_exists('set_site_transient')) {
                    set_site_transient(self::TRANSIENT_MANIFEST, $toCache, 12 * HOUR_IN_SECONDS);
                }
                $claims = $toCache;
            } else {
                // No update / verification miss: negative-cache the sentinel for
                // NO_UPDATE_TTL so we do NOT re-hit the CP on every admin load.
                // checkNow()/flushCache() delete the transient, clearing this too.
                if (function_exists('set_site_transient')) {
                    set_site_transient(self::TRANSIENT_MANIFEST, self::NO_UPDATE_SENTINEL, self::NO_UPDATE_TTL);
                }
                $claims = null;
            }
        }

        $onDisk = $this->onDiskVersion();

        // Normalize BOTH sides, exactly as the verifyManifest() downgrade guard
        // does. This comparison used to be made on the raw strings, which meant
        // the two gates could disagree: PHP version_compare() reads
        // '0.10.5-cron-selfheal' as a PRE-RELEASE of (lower than) '0.10.5', so a
        // manifest 'version: 0.10.5' was refused by verifyManifest() but still
        // offered here whenever a cached claims array reached injectUpdate(),
        // surfacing a dashboard update entry for a build that the security chain
        // would then refuse to install. One normalization rule, both gates.
        if (is_array($claims) && isset($claims['version']) && is_string($claims['version'])
            && version_compare($this->normalizeVersion($claims['version']), $this->normalizeVersion($onDisk), '>')
        ) {
            // Inject the update entry. Package is the SENTINEL (not a presigned
            // URL): the real URL is fetched fresh inside verifyDownload().
            $entry = new \stdClass();
            $entry->slug        = self::PLUGIN_SLUG;
            $entry->plugin      = self::PLUGIN_KEY;
            $entry->new_version = $claims['version'];
            $entry->package     = self::PACKAGE_SENTINEL;
            $entry->url         = '';
            $entry->tested      = isset($claims['tested'])       && is_string($claims['tested'])       ? $claims['tested']       : '';
            $entry->requires    = isset($claims['requires'])     && is_string($claims['requires'])     ? $claims['requires']     : '';
            $entry->requires_php = isset($claims['requires_php']) && is_string($claims['requires_php']) ? $claims['requires_php'] : '';

            if (!isset($transient->response) || !is_array($transient->response)) {
                $transient->response = [];
            }
            $transient->response[self::PLUGIN_KEY] = $entry;
        } else {
            // No update available — populate no_update so the auto-update toggle
            // renders correctly in Plugins > Updates.
            $entry = new \stdClass();
            $entry->slug        = self::PLUGIN_SLUG;
            $entry->plugin      = self::PLUGIN_KEY;
            $entry->new_version = $onDisk;
            $entry->package     = '';
            $entry->url         = '';

            if (!isset($transient->no_update) || !is_array($transient->no_update)) {
                $transient->no_update = [];
            }
            $transient->no_update[self::PLUGIN_KEY] = $entry;
        }

        return $transient;
    }

    // -------------------------------------------------------------------------
    // pluginInfo() — plugins_api filter
    // -------------------------------------------------------------------------

    /**
     * Return plugin information for the "View details" modal.
     *
     * Only acts when $action === 'plugin_information' and $args->slug === our slug.
     *
     * @param mixed  $result Current result (false or stdClass from another source).
     * @param string $action API action being requested.
     * @param mixed  $args   API arguments (expected: object with ->slug property).
     * @return mixed Our stdClass for our slug, or $result untouched.
     */
    public function pluginInfo($result, string $action, $args)
    {
        if ($action !== 'plugin_information') {
            return $result;
        }
        if (!is_object($args) || !isset($args->slug) || !is_string($args->slug)) {
            return $result;
        }
        if (!hash_equals(self::PLUGIN_SLUG, $args->slug)) {
            return $result;
        }

        $claims = function_exists('get_site_transient')
            ? get_site_transient(self::TRANSIENT_MANIFEST)
            : false;

        if (!is_array($claims)) {
            return $result;
        }

        $version    = isset($claims['version'])      && is_string($claims['version'])      ? $claims['version']      : $this->onDiskVersion();
        $requires   = isset($claims['requires'])     && is_string($claims['requires'])     ? $claims['requires']     : '';
        $tested     = isset($claims['tested'])       && is_string($claims['tested'])       ? $claims['tested']       : '';
        $requiresPhp = isset($claims['requires_php']) && is_string($claims['requires_php']) ? $claims['requires_php'] : '';
        $description = isset($claims['sections']['description']) && is_string($claims['sections']['description'])
            ? $claims['sections']['description']
            : '';

        $info = new \stdClass();
        $info->name          = 'WPMgr Agent';
        $info->slug          = self::PLUGIN_SLUG;
        $info->version       = $version;
        $info->author        = 'WPMgr contributors';
        $info->requires      = $requires;
        $info->tested        = $tested;
        $info->requires_php  = $requiresPhp;
        $info->sections      = ['description' => $description];
        $info->download_link = self::PACKAGE_SENTINEL;

        return $info;
    }

    // -------------------------------------------------------------------------
    // verifyDownload() — upgrader_pre_download filter
    // -------------------------------------------------------------------------

    /**
     * Gate the actual plugin installation behind the full security chain.
     *
     * Called by WP_Upgrader just before it downloads the package. We intercept
     * calls for our plugin key (or the sentinel package), fetch a FRESH manifest
     * (with a fresh presigned URL, fully re-verified including downgrade guard),
     * download the package, verify the sha256, and return the local temp-file
     * path so WP installs from it.
     *
     * Any failure returns a WP_Error (visible to the operator; aborts the install).
     * On sha256 mismatch the temp file is unlinked before returning.
     *
     * @param mixed       $reply      Current reply (false = not handled yet).
     * @param string|null $package    Package URL or sentinel from the transient.
     *                                WP core passes null (or occasionally false)
     *                                whenever the current update-transient row for
     *                                SOME OTHER plugin/theme has no ->package at
     *                                all — see the is_string() guard below.
     * @param mixed       $upgrader   WP_Upgrader instance (filter arg 3; unused).
     * @param mixed       $hook_extra Extra info from WP_Upgrader (array with 'plugin' key;
     *                                absent on WP < 5.5, hence nullable + the sentinel
     *                                fallback below).
     * @return mixed Local temp path (string), WP_Error, or $reply untouched.
     */
    public function verifyDownload($reply, $package, $upgrader = null, $hook_extra = null)
    {
        // upgrader_pre_download is a GLOBAL filter — install()'s add_filter()
        // registers this method for EVERY plugin/theme download, not only our
        // own. WP core calls apply_filters('upgrader_pre_download', false,
        // null, ...) whenever the CURRENT update-transient row has no
        // ->package at all, which is legitimate for premium plugins that run
        // their own updater (e.g. one that leaves ->package unset until a
        // license check succeeds). $package therefore arrives here as null
        // (or occasionally false/non-string) on every such download. Bail out
        // immediately, before hash_equals() below would otherwise be handed a
        // non-string and fatal with a TypeError — that fatal stranded the
        // whole bulk-update request (and the site) in maintenance mode
        // (GitHub issue #182). is_string() (not merely !== null) also covers
        // false/int. Return $reply UNCHANGED — never $package itself, which
        // would make WP treat this return as a resolved local file path and
        // break the other plugin/theme's own download — so WordPress
        // continues its normal handling for a package that was never ours.
        if (!is_string($package)) {
            return $reply;
        }

        // Only act on our plugin key or our sentinel package. The sentinel is the
        // primary signal (it is what we inject as ->package, and it is present on
        // every WP version); hook_extra['plugin'] is a secondary signal available
        // on WP 5.5+. The upgrader_pre_download filter passes 4 args
        // ($reply, $package, $upgrader, $hook_extra) — we MUST register all four
        // (10, 4) or $hook_extra arrives as the WP_Upgrader object.
        $isOurPlugin = is_array($hook_extra) && hash_equals(self::PLUGIN_KEY, (string) ($hook_extra['plugin'] ?? ''));
        $isSentinel  = hash_equals(self::PACKAGE_SENTINEL, $package);

        if (!$isOurPlugin && !$isSentinel) {
            return $reply;
        }

        // Fetch a FRESH manifest (new presigned URL, fully re-verified).
        $claims = $this->fetchManifest();
        if (!is_array($claims)) {
            return new \WP_Error(
                'wpmgr_update_manifest_failed',
                'WPMgr agent update: could not fetch or verify the update manifest from the control plane.'
            );
        }

        $packageUrl  = isset($claims['package_url'])    && is_string($claims['package_url'])    ? $claims['package_url']    : '';
        $packageSize = isset($claims['package_size'])   && is_int($claims['package_size'])      ? $claims['package_size']   : 0;
        $packageSha  = isset($claims['package_sha256']) && is_string($claims['package_sha256']) ? $claims['package_sha256'] : '';

        if ($packageUrl === '' || $packageSize <= 0 || $packageSha === '') {
            return new \WP_Error(
                'wpmgr_update_manifest_incomplete',
                'WPMgr agent update: manifest is missing download fields.'
            );
        }

        // Download the package with size clamping. We use wp_remote_get with a
        // streaming approach so we can verify the sha256 without holding the
        // entire zip in memory.
        if (!function_exists('wp_remote_get') || !function_exists('wp_remote_retrieve_body')) {
            return new \WP_Error(
                'wpmgr_update_no_http',
                'WPMgr agent update: WordPress HTTP API not available.'
            );
        }

        // ARM THE PHP CLOCK IMMEDIATELY BEFORE THE TRANSFER. PACKAGE_TIMEOUT
        // below is a cURL budget; it says nothing about PHP's own
        // max_execution_time, which is 30s on a great many hosts and which was
        // killing this request mid-download with the temp file half written.
        // serveThenApply() already armed the same bound for the whole apply, and
        // this second call is deliberate rather than redundant: it re-arms the
        // counter from zero so the download's budget is measured from the
        // transfer's own start instead of inheriting whatever the manifest round
        // trip above already spent, and it is the only arming on the other route
        // into this filter (an operator-initiated or WordPress-auto-update
        // install, which never passes through the CP-commanded apply).
        //
        // Safe to place here because the not-ours bail-out above has already
        // returned: this can only ever raise the limit for OUR package, never
        // for the unrelated plugin/theme downloads this global filter also sees.
        //
        // Bounded, never 0 (infinite), for the reason set out in
        // LongRunningJob's class doc and UpdateCommand::APPLY_TIME_LIMIT_SECONDS.
        if (function_exists('set_time_limit')) {
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,Squiz.PHP.DiscouragedFunctions.Discouraged -- bounded-but-generous cap so PHP cannot kill a slow package download mid-transfer; @-guarded, no-op when disabled
        }

        $response = wp_remote_get(
            $packageUrl,
            [
                'timeout'     => self::PACKAGE_TIMEOUT,
                'redirection' => 0,
                'stream'      => true,
                'filename'    => $this->tempFilePath(),
            ]
        );

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return new \WP_Error(
                'wpmgr_update_download_failed',
                'WPMgr agent update: package download failed.'
            );
        }

        $dlStatus = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        if ($dlStatus !== 200) {
            return new \WP_Error(
                'wpmgr_update_download_status',
                sprintf('WPMgr agent update: package download returned HTTP %d.', $dlStatus)
            );
        }

        // The streamed file path is in the response headers when 'stream' => true.
        $tempFile = isset($response['filename']) && is_string($response['filename'])
            ? $response['filename']
            : '';

        if ($tempFile === '' || !is_file($tempFile)) {
            return new \WP_Error(
                'wpmgr_update_no_tempfile',
                'WPMgr agent update: downloaded file not found.'
            );
        }

        // Verify size.
        $actualSize = filesize($tempFile);
        if ($actualSize === false || $actualSize !== $packageSize) {
            @unlink($tempFile);
            return new \WP_Error(
                'wpmgr_update_size_mismatch',
                sprintf(
                    'WPMgr agent update: package size mismatch (expected %d, got %s).',
                    $packageSize,
                    $actualSize === false ? 'unknown' : (string) $actualSize
                )
            );
        }

        // Compute streaming sha256 of the downloaded file.
        $actualSha = $this->sha256File($tempFile);
        if ($actualSha === null) {
            @unlink($tempFile);
            return new \WP_Error(
                'wpmgr_update_hash_failed',
                'WPMgr agent update: could not compute sha256 of downloaded package.'
            );
        }

        // Constant-time sha256 comparison.
        if (!hash_equals($packageSha, $actualSha)) {
            @unlink($tempFile);
            return new \WP_Error(
                'wpmgr_update_sha_mismatch',
                'WPMgr update integrity check failed.'
            );
        }

        // All checks passed. Return the local temp file path so WP_Upgrader
        // installs from it (skipping its own download step).
        return $tempFile;
    }

    // -------------------------------------------------------------------------
    // renameSource() — upgrader_source_selection filter
    // -------------------------------------------------------------------------

    /**
     * Ensure the unzipped plugin directory is named 'wpmgr-agent' (no suffix).
     *
     * WordPress sometimes creates a versioned folder like 'wpmgr-agent-0.10.6'.
     * We rename it to 'wpmgr-agent' so the update is truly in-place.
     *
     * Traversal-safe: uses wp_basename() to strip any path components in the
     * source name before comparing.
     *
     * @param string|null $source        Path to the extracted source directory.
     *                                   Like verifyDownload()'s $package, WP core
     *                                   can hand this filter a non-string (e.g. a
     *                                   WP_Error passed through from an earlier,
     *                                   already-failed step) for a download this
     *                                   method was never meant to touch — see the
     *                                   is_string() guard below.
     * @param string      $remote_source Path to the remote source temp dir.
     * @param mixed       $upgrader      WP_Upgrader instance.
     * @param mixed       $hook_extra    Extra context from WP_Upgrader.
     * @return mixed Corrected source path (string), or $source untouched.
     */
    public function renameSource($source, string $remote_source, $upgrader, $hook_extra)
    {
        // Same strict_types(1) hazard as verifyDownload(): this is also a
        // GLOBAL upgrader filter, so a non-string $source (e.g. a WP_Error
        // already carried through from an earlier failed step, for a plugin
        // this method was never meant to touch) must never reach the
        // wp_basename()/basename() calls below. Return it untouched.
        if (!is_string($source)) {
            return $source;
        }

        // Only act on our plugin.
        if (!is_array($hook_extra) || !isset($hook_extra['plugin'])) {
            return $source;
        }
        if (!hash_equals(self::PLUGIN_KEY, (string) $hook_extra['plugin'])) {
            return $source;
        }

        $sourceName = function_exists('wp_basename') ? wp_basename($source) : basename($source);
        // Traversal-safe: basename strips directory separators.
        $sourceName = basename($sourceName);

        // Already correct.
        if (hash_equals('wpmgr-agent', rtrim($sourceName, '/\\'))) {
            return $source;
        }

        // The expected target: remote_source/wpmgr-agent/
        $newSource = rtrim($remote_source, '/\\') . '/wpmgr-agent/';

        // Use $wp_filesystem if available; otherwise try a plain rename.
        global $wp_filesystem;
        if (isset($wp_filesystem) && is_object($wp_filesystem) && method_exists($wp_filesystem, 'move')) {
            $moved = $wp_filesystem->move($source, $newSource, true);
            if ($moved) {
                return $newSource;
            }
            error_log('wpmgr-agent: UpdateChecker renameSource: wp_filesystem->move failed; using original source.');
            return $source;
        }

        // Fallback: plain PHP rename.
        if (@rename($source, $newSource)) {
            return $newSource;
        }

        error_log('wpmgr-agent: UpdateChecker renameSource: rename() failed; using original source.');
        return $source;
    }

    // -------------------------------------------------------------------------
    // Admin helpers
    // -------------------------------------------------------------------------

    /**
     * Force an immediate re-check: flush both transients and fetch a fresh
     * manifest. Called by the admin "Check for updates" action.
     *
     * @return void
     */
    public function checkNow(): void
    {
        // Flush the 12h manifest cache.
        if (function_exists('delete_site_transient')) {
            delete_site_transient(self::TRANSIENT_MANIFEST);
            // Also flush the global update_plugins transient so WP re-evaluates.
            delete_site_transient('update_plugins');
        }

        // Fetch a fresh manifest and re-cache it.
        $fresh = $this->fetchManifest();
        if (is_array($fresh)) {
            $toCache = $fresh;
            unset($toCache['package_url']);
            if (function_exists('set_site_transient')) {
                set_site_transient(self::TRANSIENT_MANIFEST, $toCache, 12 * HOUR_IN_SECONDS);
            }
        }
    }

    /**
     * Flush the cached manifest. Bound to delete_site_transient_update_plugins
     * (the "Check again" link in Plugins > Updates).
     *
     * @return void
     */
    public function flushCache(): void
    {
        if (function_exists('delete_site_transient')) {
            delete_site_transient(self::TRANSIENT_MANIFEST);
        }
    }

    // -------------------------------------------------------------------------
    // CP-commanded self-update, in two beats
    //
    // A normal update command cannot be used on the agent's own directory: the
    // plugin would overwrite its own files while its code is the code doing the
    // overwriting. What that really demands is that the swap runs somewhere
    // WordPress's own upgrade machinery is still fully functional, and "a
    // separate request" turned out to be the wrong way to buy it. An apply that
    // ran from a shutdown callback sat PAST the point at which core's own
    // temp-backup rollback can still be dispatched, so the one protection that
    // makes a failed swap survivable was silently unreachable.
    //
    // Two beats now, and the first of them is one request:
    //   1. ARM then APPLY. planSelfUpdate() runs inside the signed command
    //      request and verifies only; nothing on disk moves. It takes core's own
    //      upgrader lock and registers serveThenApply() on core's
    //      rest_pre_serve_request filter, which runs the upgrade AFTER the
    //      acknowledgement is on the wire and the connection has been released.
    //      See serveThenApply() for why that position, and only that position,
    //      keeps core's rollback dispatchable.
    //   2. CONFIRM. The only trustworthy success signal is the NEW code phoning
    //      home: the version-changed boot metadata push in Plugin. A
    //      "scheduled" acknowledgement is NEVER a success.
    // -------------------------------------------------------------------------

    /**
     * THE ARM. Verify that a newer agent build is on offer and, if so, take
     * the apply lock and register the in-request apply. NOTHING on disk is
     * moved, unpacked or overwritten here: the only writes are the verified
     * manifest site-transient, core's lock row, and the apply id beside it.
     *
     * Runs the FULL existing verification chain by way of fetchManifest():
     * Ed25519 signature, cmd/slug/aud binding, iat/exp window, jti replay,
     * monotonic-iat anti-rollback, downgrade guard, https + exact-host
     * allowlist and package size cap. Nothing new is trusted here that the
     * one-click path does not already trust.
     *
     * EVERY REASON THIS SITE CANNOT BE UPGRADED IS DECIDED HERE, before the
     * answer goes out, because an answer the control plane has to wait out a
     * confirm deadline to disbelieve costs a rollout wave. There are exactly
     * two such reasons and both are about identity rather than capability: the
     * wordpress.org distribution build, which ships no self-updater at all, and
     * a site that is not enrolled. The PHP SAPI is not one of them.
     *
     * @return array{status:string,ok:bool,from_version:string,to_version:string,detail:string,cron_mode:string,expires_at:int,apply_id:string}
     */
    public function planSelfUpdate(): array
    {
        $onDisk = $this->onDiskVersion();

        // The wp.org distribution build ships without this file at all, so in
        // practice the command shell answers not_eligible long before we get
        // here. Kept as a belt-and-braces guard for a hand-assembled tree that
        // defines the constant but still carries the source file.
        if (defined('WPMGR_WPORG_BUILD') && constant('WPMGR_WPORG_BUILD')) {
            return $this->stageAnswer('not_eligible', $onDisk, '', 'Self-update is not part of this distribution build.');
        }

        if (!$this->settings->isEnrolled()) {
            return $this->stageAnswer('not_eligible', $onDisk, '', 'Site is not enrolled.');
        }

        if (!function_exists('update_option')) {
            return $this->stageAnswer('not_eligible', $onDisk, '', 'WordPress option API unavailable.');
        }

        // Shed the retired staging state BEFORE the manifest fetch. A site that
        // is already current answers up_to_date and returns below, so a cleanup
        // placed after that return would never run on the sites most likely to
        // still be carrying the leftovers.
        $this->clearRetiredStagingState();

        $claims = $this->fetchManifest();
        if (!is_array($claims)) {
            if ($this->lastManifestOutcome === 'no_update' || $this->lastManifestOutcome === 'up_to_date') {
                return $this->stageAnswer('up_to_date', $onDisk, '', 'No newer agent build is published for this site.');
            }

            // Deliberately reports only the coarse outcome tag. The manifest
            // body carries a short-lived presigned package_url; nothing derived
            // from it may reach a CP-visible string.
            return $this->stageAnswer(
                'error',
                $onDisk,
                '',
                'Update manifest could not be fetched or verified (' . $this->lastManifestOutcome . ').'
            );
        }

        $toVersion = isset($claims['version']) && is_string($claims['version']) ? $claims['version'] : '';
        if ($toVersion === '') {
            return $this->stageAnswer('error', $onDisk, '', 'Verified manifest carries no version.');
        }

        // Cache the verified claims (package_url stripped, since it is a bearer
        // credential) so the apply's injectUpdate() offers this exact build to
        // Plugin_Upgrader without another CP round trip. Identical shape to the
        // caching injectUpdate() already performs.
        $toCache = $claims;
        unset($toCache['package_url']);
        if (function_exists('set_site_transient') && defined('HOUR_IN_SECONDS')) {
            set_site_transient(self::TRANSIENT_MANIFEST, $toCache, 12 * HOUR_IN_SECONDS);
        }

        // WP_Upgrader::create_lock is a static on the upgrader class, so the
        // wp-admin includes have to be loaded before the lock, not just before
        // the upgrade.
        if (!$this->loadUpgraderApi()) {
            return $this->stageAnswer('error', $onDisk, '', 'The WordPress upgrader API is unavailable in this request.');
        }

        // Core's own cross-request lock: one atomic INSERT IGNORE, self-expiring
        // after the release timeout. This is what replaces the hand-rolled
        // claim, the in-flight marker and the stage token.
        if (!\WP_Upgrader::create_lock(self::APPLY_LOCK, LongRunningJob::TIME_LIMIT_SECONDS)) {
            $answer = $this->stageAnswer(
                'already_scheduled',
                $onDisk,
                $toVersion,
                'Another self-update apply already holds this site lock.'
            );
            $answer['expires_at'] = $this->applyLockExpiry();
            // The apply id of the run that holds the lock, so the control plane
            // can still tie the outcome record to a real apply instead of
            // confirming on version movement alone.
            $answer['apply_id'] = $this->currentApplyId();

            return $answer;
        }

        $applyId = bin2hex(random_bytes(16));
        update_option(self::OPTION_APPLY_ID, $applyId, false);

        // Drop any previous outcome so a stale record from an earlier run can
        // never be read back as the result of THIS one.
        if (function_exists('delete_option')) {
            delete_option(AgentSelfUpdateCommand::OPTION_RESULT);
        }

        if (!function_exists('add_filter')) {
            // No filter API means no seam to apply from. Release the lock rather
            // than hold it for 900 seconds over an apply that cannot happen.
            $this->releaseApplyLock();

            return $this->stageAnswer('error', $onDisk, '', 'WordPress filter API unavailable; the apply has no seam to run from.');
        }

        $this->pendingApply = [
            'apply_id' => $applyId,
            'from'     => $onDisk,
            'to'       => $toVersion,
        ];

        add_filter('rest_pre_serve_request', [$this, 'serveThenApply'], 999, 4);

        $answer = $this->stageAnswer(
            'scheduled',
            $onDisk,
            $toVersion,
            'Self-update verified; the agent applies it in this request once the response is released.'
        );
        $answer['expires_at'] = time() + LongRunningJob::TIME_LIMIT_SECONDS;
        $answer['apply_id']   = $applyId;

        return $answer;
    }

    /**
     * THE SEAM. Core's own documented "send the response yourself" filter
     * (wp-includes/rest-api/class-wp-rest-server.php, whose docblock reads
     * "Allow sending the request manually"). serve_request() has already set the
     * status and sent every header by the time it fires, so only the body is
     * ours to emit.
     *
     * WHY HERE AND NOT ON 'shutdown'. rest_api_loaded() calls die() after
     * serve_request() returns (wp-includes/rest-api.php), and die() runs PHP's
     * shutdown functions, among them shutdown_action_hook (registered in
     * wp-settings.php, defined in wp-includes/load.php), which is the sole
     * dispatcher of do_action('shutdown'). So while this method runs,
     * do_action('shutdown') has NOT started. The
     * add_action('shutdown', restore_temp_backup, 10) that WP_Upgrader::run()
     * registers on install failure (class-wp-upgrader.php) therefore lands in a
     * hook at nesting level 0 and is dispatched normally, as is
     * delete_temp_backup at 100. From a shutdown callback at priority 9997 both
     * were skipped outright by WP_Hook::resort_active_iterations, which is why
     * the control-plane commanded self-update has never had a working rollback.
     *
     * It is also the only position where core's stated reason for putting the
     * rollback on shutdown still holds: a max_execution_time fatal raised HERE
     * is a fatal in the request body, and PHP still runs the shutdown queue
     * afterwards.
     *
     * THE FLUSH IS ATTEMPTED, NOT REQUIRED. finish() takes the best rung this
     * SAPI offers and reports which one it was. On PHP-FPM or LiteSpeed that
     * genuinely releases the control-plane connection and the apply runs with
     * nobody waiting on it. On every other SAPI the last rung flushes what is
     * buffered and the worker stays attached, so the control plane holds its
     * connection until the apply finishes. Both then run the identical apply.
     * The rung is recorded with the outcome and decides nothing.
     *
     * @param mixed $served  Whether the response has already been sent.
     * @param mixed $result  The response object core was about to serialise.
     * @param mixed $request The request being served (unused).
     * @param mixed $server  The WP_REST_Server instance.
     * @return mixed True once this method has emitted the body itself.
     */
    public function serveThenApply($served = false, $result = null, $request = null, $server = null)
    {
        unset($request);

        $pending = $this->pendingApply;
        if ($pending === null) {
            return $served;
        }

        // Read once and clear: a second dispatch of this filter, from whatever
        // cause, can never start a second apply.
        $this->pendingApply = null;

        $applyId = (string) $pending['apply_id'];
        $from    = (string) $pending['from'];
        $to      = (string) $pending['to'];

        try {
            $this->emitAck((bool) $served, $result, $server);
        } catch (\Throwable $e) {
            // NEVER SWAP WITHOUT HAVING ANSWERED. The whole point of taking over
            // the body is that the control plane is told what is about to happen
            // before it happens. If that failed, hand the response back to core
            // and leave the site exactly as it is: an un-upgraded site re-arms
            // on the next command, a site upgraded behind the control plane's
            // back does not.
            DebugLog::write('WPMgr Agent: self-update could not emit its acknowledgement: ' . $e->getMessage());
            $this->recordSelfUpdateResult(
                $applyId,
                'failed',
                $from,
                $to,
                'The acknowledgement could not be written to the response, so the apply was abandoned before it started.'
            );
            $this->releaseApplyLock();

            return $served;
        }

        $rung = 'fallback';
        try {
            $rung = $this->finisher->finish();
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: self-update could not release the connection: ' . $e->getMessage());
        }

        $this->applyPendingUpdate($rung, $applyId, $from, $to);

        return true;
    }

    /**
     * Everything that happens once the response has been released: the
     * execution guards, the class warming, and the upgrade.
     *
     * Split from serveThenApply() because the rung is an input a test cannot
     * otherwise choose. Neither finish-request function can be defined in a
     * test process without changing which rung every other test observes, so
     * the apply is reachable directly with the rung handed in.
     *
     * THE RUNG IS RECORDED, NOT OBEYED. This method used to refuse the whole
     * upgrade when finish() reported the last rung, on the reasoning that a
     * still-attached connection could be cut mid swap. It no longer does, for
     * three reasons that hold together. WordPress core has no SAPI check
     * anywhere in its upgrade path and updates plugins on these same hosts from
     * wp-admin every day, on a fully attached browser connection. This agent's
     * own update command already applies plugin, theme and core upgrades inline
     * and attached on every SAPI, guarded by nothing more than the two calls
     * below. And those two calls were the guards the refusal made unreachable
     * on precisely the hosts that need them: on a SAPI that cannot detach,
     * ignore_user_abort() IS the defence, and it was dead code here.
     *
     * What the rung still buys is diagnosis. It is stamped into the apply
     * result, so a fleet-wide reading can tell which sites upgraded on an
     * attached connection, and on the last rung the reason this host has no
     * detaching one is written to the debug log in the same breath.
     *
     * @param string $rung    The rung ConnectionFinisher reported.
     * @param string $applyId Opaque id of this apply.
     * @param string $from    On-disk version at arm time.
     * @param string $to      Verified target version.
     * @return void
     */
    private function applyPendingUpdate(string $rung, string $applyId, string $from, string $to): void
    {
        $this->applyRung = $rung;

        try {
            if (!in_array($rung, ConnectionFinisher::DETACHING_RUNGS, true)) {
                DebugLog::write(rtrim(
                    'WPMgr Agent: self-update applying on the ' . $rung . ' rung, so the control-plane connection '
                    . 'stays attached until the apply finishes. ' . $this->finisher->detachReason()
                ));
            }

            // BOTH OF THESE MATTER MOST ON THE RUNG THAT DID NOT DETACH, which
            // is why they are the first thing the apply does on EVERY rung. On
            // a detaching rung the client is already gone. On the last rung it
            // is still there, and ignore_user_abort() is what keeps a peer that
            // hangs up from ending the swap halfway. The execution cap is
            // bounded on purpose: max_execution_time is the ONE timer whose
            // fatal still runs shutdown functions, which is exactly what the
            // rollback depends on.
            if (function_exists('ignore_user_abort')) {
                ignore_user_abort(true);
            }
            if (function_exists('set_time_limit')) {
                @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,Squiz.PHP.DiscouragedFunctions.Discouraged -- bounded-but-generous cap so a hung self-update apply hits a recoverable max_execution_time fatal rather than only an unrecoverable hard-kill; @-guarded, no-op when disabled
            }

            self::warmPostSwapClasses();

            $this->runUpgrade($applyId, $from, $to);
        } catch (\Throwable $e) {
            $this->recordSelfUpdateResult($applyId, 'failed', $from, $to, 'Self-update apply threw: ' . $e->getMessage());
        } finally {
            $this->releaseApplyLock();
        }
    }

    /**
     * Load every agent class that is still reachable AFTER the plugin directory
     * has been replaced, so none of them has to be read off a path that no
     * longer exists.
     *
     * The list is POST_SWAP_CLASSES and a test walks the agent's shutdown
     * bindings against it, because a warm list maintained by hand rots the first
     * time somebody adds a callback.
     *
     * @return void
     */
    private static function warmPostSwapClasses(): void
    {
        foreach (self::POST_SWAP_CLASSES as $className) {
            try {
                class_exists($className);
            } catch (\Throwable $e) {
                // A class that will not load now would not have loaded after the
                // swap either. Nothing useful to do about it from here.
                continue;
            }
        }
    }

    /**
     * Emit the acknowledgement body ourselves, because returning true from
     * rest_pre_serve_request tells core not to.
     *
     * response_to_data() is public on WP_REST_Server; get_json_encode_options()
     * is protected, so the encode goes through wp_json_encode(). The
     * rest_pre_echo_response filter is deliberately not applied here: this body
     * is a machine-to-machine acknowledgement with a pinned shape, and running
     * an arbitrary third-party filter over it inside the request that is about
     * to replace this plugin's directory buys nothing and can throw.
     *
     * @param bool  $served Whether another filter already sent the response.
     * @param mixed $result The response object core was about to serialise.
     * @param mixed $server The WP_REST_Server instance.
     * @return void
     */
    private function emitAck(bool $served, $result, $server): void
    {
        if ($served) {
            return;
        }
        if (!is_object($result) || !is_object($server) || !method_exists($server, 'response_to_data')) {
            return;
        }
        if (!function_exists('wp_json_encode')) {
            return;
        }

        $body = wp_json_encode($server->response_to_data($result, false));
        if (!is_string($body)) {
            return;
        }

        echo $body; // phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped -- JSON produced by core's own response_to_data() and wp_json_encode(); escaping it would corrupt the response body
    }

    /**
     * The Plugin_Upgrader half of the apply, run from serveThenApply() with the
     * response already released.
     *
     * Runs the upgrade with wp_doing_cron() forced true for its duration, which
     * buys two documented core behaviours and is the reason this method no
     * longer has to restore the plugin's active state by hand:
     * Plugin_Upgrader::deactivate_plugin_before_upgrade() returns early under
     * cron without touching active_plugins, so the agent is never silently
     * deactivated, and active_before()/active_after() put maintenance mode over
     * the destructive window and take it off again.
     *
     * @param string $applyId Opaque id of this apply, stamped into the outcome.
     * @param string $from    On-disk version at arm time.
     * @param string $to      Verified target version.
     * @return void
     */
    private function runUpgrade(string $applyId, string $from, string $to): void
    {
        if (!$this->loadUpgraderApi() || !class_exists('\Plugin_Upgrader') || !class_exists('\Automatic_Upgrader_Skin')) {
            $this->recordSelfUpdateResult($applyId, 'failed', $from, $to, 'Upgrader API unavailable.');
            return;
        }

        // Plugin_Upgrader::upgrade() reads the plugin update transient to find
        // the package and bails to its up_to_date branch when the entry is
        // absent, returning a bare false with nothing else said. THIS PATH USED
        // TO DELETE THAT TRANSIENT A FEW LINES ABOVE THE CALL, which is why the
        // CP-commanded self-update never applied on any site. Rebuild it the way
        // core's own background updater does. wp_update_plugins() writes the
        // transient BEFORE it calls api.wordpress.org, so a site that cannot
        // reach wordpress.org still ends up with an object here, and reading it
        // back runs our own site_transient_update_plugins filter, which is what
        // offers the build this arm just verified.
        $offer = function_exists('get_site_transient') ? get_site_transient('update_plugins') : null;
        if (!is_object($offer) && function_exists('wp_update_plugins')) {
            wp_update_plugins();
            $offer = get_site_transient('update_plugins');
        }

        if (!is_object($offer) || !isset($offer->response[self::PLUGIN_KEY])) {
            // At or past the target already: the build landed some other way
            // between the arm and this line. Benign, and it must not be reported
            // as a failure.
            if (
                is_object($offer)
                && isset($offer->no_update[self::PLUGIN_KEY])
                && !version_compare($this->normalizeVersion($to), $this->normalizeVersion($this->onDiskVersion()), '>')
            ) {
                $this->recordSelfUpdateResult(
                    $applyId,
                    'already_applied',
                    $from,
                    $to,
                    'On-disk version is already at or past the verified target.'
                );
                return;
            }

            $this->recordSelfUpdateResult(
                $applyId,
                'failed',
                $from,
                $to,
                'No update offer reached the upgrader: the plugin update transient carried no entry for this plugin.'
            );
            return;
        }

        // A fatal skips every try/finally, so maintenance mode gets a shutdown
        // backstop of its own before anything destructive begins.
        Maintenance::armShutdownGuard();

        $upgrader       = new \Plugin_Upgrader(new \Automatic_Upgrader_Skin());
        $this->upgrader = $upgrader;

        if (function_exists('add_action')) {
            add_action('shutdown', [$this, 'restoreAgentIfDirectoryMissing'], self::RESTORE_GUARD_PRIORITY);
        }

        $result = null;
        $thrown = '';

        try {
            if (function_exists('add_filter')) {
                add_filter('wp_doing_cron', '__return_true', 9999);
            }
            $result = $upgrader->upgrade(self::PLUGIN_KEY);
        } catch (\Throwable $e) {
            $thrown = $e->getMessage();
        } finally {
            if (function_exists('remove_filter')) {
                remove_filter('wp_doing_cron', '__return_true', 9999);
            }
            // GUARANTEE: maintenance mode is cleared on every terminal path of
            // this upgrade, whether it succeeded, returned a WP_Error, or threw.
            Maintenance::clear($upgrader);
        }

        if ($thrown !== '') {
            // A Throwable escaping upgrade() means WP_Upgrader::run() never
            // reached the branch where it registers core's shutdown restore, so
            // call core's own public restorer directly. It is precondition-gated
            // (see restoreAgentIfDirectoryMissing), which is what keeps it from
            // reverting an install that had already succeeded before some later
            // listener threw.
            $this->restoreAgentIfDirectoryMissing();
            $this->recordSelfUpdateResult($applyId, 'failed', $from, $to, 'Upgrader threw: ' . $thrown);
            return;
        }

        if (is_object($result) && method_exists($result, 'get_error_message')) {
            $code    = method_exists($result, 'get_error_code') ? (string) $result->get_error_code() : '';
            $message = (string) $result->get_error_message();
            $this->recordSelfUpdateResult(
                $applyId,
                'failed',
                $from,
                $to,
                'WP_Error' . ($code !== '' ? ' (' . $code . ')' : '') . ': ' . $message
            );
            return;
        }

        // ONLY LITERAL TRUE IS A SUCCESS. This is a default deny, not a list of
        // known-bad values, and the difference is the point.
        //
        // Plugin_Upgrader::upgrade() returns literal true down exactly one path
        // (class-plugin-upgrader.php:268), reached only after WP_Upgrader's
        // $result property came back truthy and not a WP_Error
        // (class-plugin-upgrader.php:250). That property is assigned in ONE
        // place, inside install_package() (class-wp-upgrader.php:719, and again
        // at :733 when the upgrader_post_install filter rewrote it), and run()
        // never resets it. So true is the only value that positively proves
        // install_package ran to completion, and every other value is either a
        // known failure or something this method has no way to enumerate.
        //
        // Two of those it could not enumerate. run()'s early bails, an
        // fs_connect that returns false and a download or unpack WP_Error,
        // never touch the property at all, and Plugin_Upgrader redeclares
        // `public $result;` with NO initialiser (class-plugin-upgrader.php:31),
        // shadowing WP_Upgrader's `= array()`, so upgrade() hands those bails
        // back as null. Separately, a WP_Error returned by the
        // upgrader_install_package_result filter (class-wp-upgrader.php:917)
        // rewrites a LOCAL inside run() and never touches the property, so it
        // changes run()'s return value and never reaches this one. Naming
        // values to refuse cannot cover a set shaped like that. Requiring the
        // one value that means success can, and under apply-id attribution a
        // wrong answer here would now travel with a matching id and be believed.
        if ($result !== true) {
            $this->recordSelfUpdateResult(
                $applyId,
                'failed',
                $from,
                $to,
                'The upgrader did not report success. The commonest cause is WordPress being unable to connect to '
                . 'the filesystem, which makes its upgrader bail before it copies anything.'
            );
            return;
        }

        // The new files are on disk, but THIS request is still running the old
        // code, so it is in no position to declare success to anyone. It records
        // a local outcome and stops. The CONFIRM beat, the version-changed boot
        // metadata push from the NEW code, is the only success signal the CP
        // trusts. Core clears the plugin caches itself through its own
        // upgrader_process_complete binding, so nothing is deleted here.
        $this->flushCache();
        $this->recordSelfUpdateResult($applyId, 'applied', $from, $to, $this->installNotes());
    }

    /**
     * Restore the agent's directory from core's own temp backup, but only when
     * the directory is genuinely missing and the backup is genuinely there.
     *
     * This is not a hand-rolled swap: it calls CORE's public restorer with
     * CORE's own argument shape, on the condition that core's own backup is
     * present and the plugin is not. It exists because core registers its
     * restore only AFTER install_package() has returned a WP_Error, so a fatal
     * raised INSIDE install_package skips the registration entirely and nothing
     * at all is left to put the directory back.
     *
     * BOUND AT A PRIORITY ABOVE CORE'S, NOT BELOW IT. See
     * RESTORE_GUARD_PRIORITY: a Throwable escaping a shutdown callback abandons
     * the rest of the queue, so a guard placed ahead of core's restore could
     * kill the very rollback it is backing up. Running after core costs nothing,
     * because on every path where core did restore, the precondition below is
     * false and this is a no-op.
     *
     * Public so WordPress can call it as a named hook callback. Never throws.
     *
     * @return void
     */
    public function restoreAgentIfDirectoryMissing(): void
    {
        try {
            $upgrader = $this->upgrader;
            if ($upgrader === null || !method_exists($upgrader, 'restore_temp_backup')) {
                return;
            }
            if (!defined('WP_PLUGIN_DIR') || !defined('WP_CONTENT_DIR')) {
                return;
            }
            // restore_temp_backup() dereferences the global filesystem object,
            // which is null when the upgrade died before fs_connect().
            if (!isset($GLOBALS['wp_filesystem']) || !is_object($GLOBALS['wp_filesystem'])) {
                return;
            }

            $pluginDir = (string) constant('WP_PLUGIN_DIR') . '/' . self::PLUGIN_SLUG;
            $backupDir = (string) constant('WP_CONTENT_DIR') . '/upgrade-temp-backup/plugins/' . self::PLUGIN_SLUG;

            // The directories moved inside this very process, so the stat cache
            // is not to be trusted about either of them.
            clearstatcache();

            if (is_dir($pluginDir) || !is_dir($backupDir)) {
                return;
            }

            $restored = $upgrader->restore_temp_backup([
                [
                    'slug' => self::PLUGIN_SLUG,
                    'src'  => constant('WP_PLUGIN_DIR'),
                    'dir'  => 'plugins',
                ],
            ]);

            if (function_exists('is_wp_error') && is_wp_error($restored)) {
                DebugLog::write(
                    'WPMgr Agent: self-update rollback could not restore the plugin directory from its temporary backup.'
                );
            }
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: self-update rollback guard threw: ' . $e->getMessage());
        }
    }

    /**
     * Fire the recorded-outcome action from shutdown, so an apply that FAILED
     * still reaches the control plane inside its confirm window.
     *
     * A successful apply reports itself through the CONFIRM beat, the
     * version-changed push from the new code. A failed apply changes no version,
     * so without this its record would wait for the 30-minute metadata cron,
     * which is longer than the shortest confirm deadline the control plane runs.
     *
     * Public so WordPress can call it as a named hook callback. Never throws.
     *
     * @return void
     */
    public function pushOutcomeNow(): void
    {
        try {
            if (function_exists('do_action')) {
                do_action('wpmgr_agent_self_update_recorded');
            }
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: self-update outcome push failed: ' . $e->getMessage());
        }
    }

    /**
     * Load the wp-admin upgrader API. Idempotent (require_once).
     *
     * @return bool True when WP_Upgrader is available afterwards.
     */
    private function loadUpgraderApi(): bool
    {
        if (class_exists('\WP_Upgrader')) {
            return true;
        }
        if (!defined('ABSPATH')) {
            return false;
        }

        $adminDir = (string) constant('ABSPATH') . 'wp-admin/includes/';
        foreach (['file.php', 'misc.php', 'plugin.php', 'class-wp-upgrader.php'] as $adminInclude) {
            if (is_file($adminDir . $adminInclude)) {
                require_once $adminDir . $adminInclude;
            }
        }

        return class_exists('\WP_Upgrader');
    }

    /**
     * Release the apply lock and the apply id stored beside it.
     *
     * @return void
     */
    private function releaseApplyLock(): void
    {
        if (class_exists('\WP_Upgrader') && method_exists('\WP_Upgrader', 'release_lock')) {
            \WP_Upgrader::release_lock(self::APPLY_LOCK);
        }
        if (function_exists('delete_option')) {
            delete_option(self::OPTION_APPLY_ID);
        }
    }

    /**
     * The apply id of whoever currently holds the lock ('' when unreadable).
     *
     * @return string
     */
    private function currentApplyId(): string
    {
        if (!function_exists('get_option')) {
            return '';
        }
        $stored = get_option(self::OPTION_APPLY_ID, '');

        return is_string($stored) ? $stored : '';
    }

    /**
     * When the currently-held apply lock self-expires. Core stores the moment
     * the lock was taken in the option it names <lock>.lock, and honours it for
     * the release timeout that was passed to create_lock().
     *
     * @return int Unix timestamp.
     */
    private function applyLockExpiry(): int
    {
        $startedAt = function_exists('get_option') ? (int) get_option(self::APPLY_LOCK . '.lock', 0) : 0;
        if ($startedAt <= 0) {
            $startedAt = time();
        }

        return $startedAt + LongRunningJob::TIME_LIMIT_SECONDS;
    }

    /**
     * Delete the wp-options and the cron event of the retired staging design.
     *
     * Named by STRING literal rather than by constant on purpose: the staged
     * record, the in-flight marker and the apply event no longer exist in this
     * class, and reintroducing constants for them would put them back into its
     * vocabulary. A site upgrading from an agent that predates the in-request
     * apply sheds them the first time it is asked to self-update.
     *
     * @return void
     */
    private function clearRetiredStagingState(): void
    {
        if (function_exists('delete_option')) {
            delete_option('wpmgr_agent_self_update_staged');
            delete_option('wpmgr_agent_self_update_applying');
        }
        if (function_exists('wp_clear_scheduled_hook')) {
            wp_clear_scheduled_hook('wpmgr_agent_self_update_apply');
        }
    }

    /**
     * Non-secret notes recorded alongside a successful apply.
     *
     * The one fact worth carrying is whether the install was atomic. Core's
     * move_dir() tries a filesystem move first and only falls back to a
     * recursive copy when that fails, and a move across devices always fails, so
     * a site whose upgrade working directory and plugin directory sit on
     * different mounts has a real half-written window where a same-device site
     * has essentially none. Nobody can say how much of a fleet that is by
     * reasoning about it, so it is measured and reported instead.
     *
     * @return string
     */
    private function installNotes(): string
    {
        if (!defined('WP_CONTENT_DIR') || !defined('WP_PLUGIN_DIR')) {
            return '';
        }

        $working = (string) constant('WP_CONTENT_DIR') . '/upgrade';
        $plugins = (string) constant('WP_PLUGIN_DIR');
        if (!is_dir($working) || !is_dir($plugins)) {
            return '';
        }

        $workingStat = @stat($working); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort device probe; an unreadable path simply yields no note
        $pluginsStat = @stat($plugins); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort device probe; an unreadable path simply yields no note
        if (!is_array($workingStat) || !is_array($pluginsStat)) {
            return '';
        }
        if (!isset($workingStat['dev'], $pluginsStat['dev']) || $workingStat['dev'] === $pluginsStat['dev']) {
            return '';
        }

        return 'The working directory and the plugin directory are on different devices, '
            . 'so the install is a copy rather than a rename.';
    }

    /**
     * Persist the apply outcome so the next metadata push carries it to the CP.
     *
     * The stored detail is scrubbed of anything URL-shaped: upgrader and skin
     * messages can echo a package string back, and the manifest's package_url
     * is a short-lived bearer credential that must never reach a CP-visible
     * field or a log line.
     *
     * SINGLE WRITER. Every terminal outcome of the apply comes through here, so
     * every one of them is stamped with the same apply id. That id is what lets
     * the control plane say whether the version it now sees moved BECAUSE of the
     * run it armed, rather than assuming it.
     *
     * Every caller is now an apply outcome, so every record names a real apply.
     * The one caller that used to pass an empty id was the arm's SAPI refusal,
     * and that refusal is gone: this channel no longer declines a site for the
     * SAPI it runs on.
     *
     * The rung is stamped from the apply that is running, or left empty when
     * this record was written outside one. It is an observation for a fleet-wide
     * reading, never an input to anything.
     *
     * @param string $applyId Opaque id of the apply that produced this outcome.
     * @param string $status  A terminal outcome (applied|failed|expired|
     *                        already_applied) or a named diagnostic. The control
     *                        plane branches on the four it knows and surfaces
     *                        anything else verbatim, so a status added here needs
     *                        no control-plane change to reach an operator.
     * @param string $from    On-disk version at arm time.
     * @param string $to      Verified target version.
     * @param string $detail  Human-readable, non-secret detail (may be empty).
     * @return void
     */
    private function recordSelfUpdateResult(
        string $applyId,
        string $status,
        string $from,
        string $to,
        string $detail
    ): void {
        if (!function_exists('update_option')) {
            return;
        }

        $scrubbed = (string) preg_replace('#https?://\S+#i', '[url]', $detail);
        if (strlen($scrubbed) > 500) {
            $scrubbed = substr($scrubbed, 0, 500);
        }

        update_option(
            AgentSelfUpdateCommand::OPTION_RESULT,
            [
                'status'       => $status,
                'from_version' => $from,
                'to_version'   => $to,
                'detail'       => $scrubbed,
                'at'           => time(),
                'apply_id'     => $applyId,
                'rung'         => $this->applyRung,
            ],
            false
        );

        $this->queueOutcomePush($status);
    }

    /**
     * Queue the outcome push for a NON-applied outcome, at most once per
     * request. A successful apply needs no push: the new code announces itself.
     *
     * @param string $status The status just recorded.
     * @return void
     */
    private function queueOutcomePush(string $status): void
    {
        if ($status === 'applied' || $this->outcomePushQueued) {
            return;
        }
        if (!function_exists('add_action')) {
            return;
        }

        $this->outcomePushQueued = true;
        add_action('shutdown', [$this, 'pushOutcomeNow'], self::OUTCOME_PUSH_PRIORITY);
    }

    /**
     * Build an ARM answer in the single shape the control plane parses.
     *
     * @param string $status One of not_eligible|up_to_date|scheduled|already_scheduled|error.
     * @param string $from   On-disk version.
     * @param string $to     Target version ('' when there is none).
     * @param string $detail Human-readable, non-secret detail.
     * @return array{status:string,ok:bool,from_version:string,to_version:string,detail:string,cron_mode:string,expires_at:int,apply_id:string}
     */
    private function stageAnswer(string $status, string $from, string $to, string $detail): array
    {
        return [
            'status'       => $status,
            // 'scheduled' is an ACKNOWLEDGEMENT, never a success: the update has
            // not been applied and may never be. The CP treats it as unconfirmed
            // until the new code phones home (the CONFIRM beat).
            'ok'           => $status !== 'error',
            'from_version' => $from,
            'to_version'   => $to,
            'detail'       => $detail,
            'cron_mode'    => (defined('DISABLE_WP_CRON') && constant('DISABLE_WP_CRON')) ? 'external' : 'loopback',
            'expires_at'   => 0,
            // Overwritten by the two answers that name a real apply. Empty
            // everywhere else, which is exactly what an unattributable answer
            // should say about itself.
            'apply_id'     => '',
        ];
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Decode a base64url-nopad string to raw bytes. Returns '' on failure.
     *
     * Matches SODIUM_BASE64_VARIANT_URLSAFE_NO_PADDING: URL-safe alphabet
     * ('-' for '+', '_' for '/'), no padding.
     *
     * @param string $input base64url input (no padding).
     * @return string Raw bytes, or '' on decode failure.
     */
    private function base64UrlDecode(string $input): string
    {
        // Re-add padding before decoding.
        $remainder = strlen($input) % 4;
        if ($remainder !== 0) {
            $input .= str_repeat('=', 4 - $remainder);
        }

        $decoded = base64_decode(strtr($input, '-_', '+/'), true);

        return $decoded === false ? '' : $decoded;
    }

    /**
     * Strip a pre-release/build suffix to the bare numeric semver core for
     * comparison. PHP version_compare() treats '0.10.5-foo' as a PRE-RELEASE of
     * (i.e. LOWER than) '0.10.5', so comparing a dev-suffixed on-disk version
     * directly would let a manifest 'version: 0.10.5' pass the downgrade guard
     * against an on-disk '0.10.5-cron-selfheal'. Normalising both sides to
     * 'X.Y.Z' closes that sidegrade hole: a real update MUST bump the numeric
     * core (the descriptive suffix never participates in the comparison).
     *
     * @param string $version e.g. '0.10.6-cron-selfheal'.
     * @return string e.g. '0.10.6'.
     */
    private function normalizeVersion(string $version): string
    {
        $bare = preg_replace('/[-+].*$/', '', trim($version));
        return is_string($bare) && $bare !== '' ? $bare : '0';
    }

    /**
     * The allowlisted hosts a package_url may point at.
     *
     * Resolution order, and why each layer behaves the way it does:
     *
     *   1. Baseline: the managed control plane's object-storage host.
     *   2. WPMGR_AGENT_PACKAGE_HOST (comma-separated) REPLACES the baseline.
     *      This REPLACE semantic is deliberately preserved. Operators running a
     *      self-hosted install already set that constant to point the agent at
     *      their own object storage, and several of them set it precisely so the
     *      managed baseline host stops being trusted. Turning it into an append
     *      would silently re-trust a host they chose to drop, so the constant
     *      keeps meaning "these are the object-storage hosts, instead of ours".
     *   3. The agent's OWN control-plane host is UNIONED in, after the constant,
     *      so the constant cannot remove it. A self-hosted control plane mirrors
     *      the agent package into its own bucket, and a mirrored object presigns
     *      onto a storage host that differs per install, so no default host list
     *      can ever cover it. Trusting the control plane the agent is already
     *      enrolled with (and already fetches the signed manifest from) is what
     *      removes the per-site wp-config.php edit: the agent derives this host
     *      from the control-plane URL it was enrolled with, so there is nothing
     *      new to configure on any site. The union is safe because that host is
     *      the same origin that signs the manifest; an operator who can change
     *      it already controls the update channel end to end.
     *   4. The 'wpmgr_agent_package_hosts' filter remains an ABSOLUTE override,
     *      including over the control-plane host from step 3. That is intended:
     *      an operator who needs strict pinning (for example, allowing only one
     *      internal mirror) must have a way to express a closed list, and code
     *      in wp-config.php or an mu-plugin is the right place for it.
     *
     * The package_url is INSIDE the signed, sha256-verified manifest, so the host
     * is already operator-controlled; this exact-host allowlist is defense in
     * depth against a CP misconfiguration aiming the download at an unexpected or
     * internal host.
     *
     * @return array<int,string> Lower-cased allowed hostnames.
     */
    private function allowedPackageHosts(): array
    {
        $hosts = ['storage.googleapis.com'];

        if (defined('WPMGR_AGENT_PACKAGE_HOST')) {
            $configured = array_values(array_filter(array_map(
                'trim',
                explode(',', (string) constant('WPMGR_AGENT_PACKAGE_HOST'))
            )));
            if ($configured !== []) {
                $hosts = $configured;
            }
        }

        // Union, never replace: this runs AFTER the constant so the constant
        // cannot drop the control plane the agent is enrolled with.
        $cpHost = $this->controlPlaneHost();
        if ($cpHost !== '') {
            $hosts[] = $cpHost;
        }

        if (function_exists('apply_filters')) {
            $filtered = apply_filters('wpmgr_agent_package_hosts', $hosts);
            if (is_array($filtered) && $filtered !== []) {
                $hosts = $filtered;
            }
        }

        return array_values(array_unique(array_map('strtolower', array_map('strval', $hosts))));
    }

    /**
     * The host of the agent's own control-plane base URL, lower-cased.
     *
     * Returns '' when the site is not enrolled, or when the stored URL has no
     * parseable host. Only the host is taken: any port, path, or credentials in
     * the stored URL are discarded, and the https-only scheme check on
     * package_url is enforced separately in verifyManifest(), so a control plane
     * reachable over plain http still never makes an http package_url passable.
     *
     * @return string Lower-cased hostname, or ''.
     */
    private function controlPlaneHost(): string
    {
        $base = $this->settings->controlPlaneUrl();
        if ($base === '') {
            return '';
        }

        $parsed = function_exists('wp_parse_url') ? wp_parse_url($base) : parse_url($base);
        if (!is_array($parsed) || !isset($parsed['host']) || !is_string($parsed['host'])) {
            return '';
        }

        return strtolower($parsed['host']);
    }

    /**
     * Whether $host exactly matches an allowed package host (constant-time).
     *
     * @param string $host Host parsed from the package URL.
     * @return bool
     */
    private function isAllowedPackageHost(string $host): bool
    {
        if ($host === '') {
            return false;
        }
        $host = strtolower($host);
        foreach ($this->allowedPackageHosts() as $allowed) {
            if (hash_equals($allowed, $host)) {
                return true;
            }
        }
        return false;
    }

    /**
     * Read the on-disk plugin version via get_plugin_data(). Falls back to the
     * WPMGR_AGENT_VERSION constant (defined in the plugin header file) as a
     * secondary source.
     *
     * NEVER uses a CP-supplied version value here.
     *
     * @return string Version string (e.g. '0.10.5-cron-selfheal').
     */
    private function onDiskVersion(): string
    {
        if (defined('WPMGR_AGENT_FILE') && function_exists('get_plugin_data')) {
            try {
                $data = get_plugin_data((string) constant('WPMGR_AGENT_FILE'), false, false);
                if (is_array($data) && isset($data['Version']) && is_string($data['Version']) && $data['Version'] !== '') {
                    return $data['Version'];
                }
            } catch (\Throwable $e) {
                // Fall through to constant.
            }
        }

        return defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '0.0.0';
    }

    /**
     * Compute the SHA-256 hash of a file using streaming reads.
     * Returns null on any I/O error.
     *
     * @param string $path Absolute path to the file.
     * @return string|null Lowercase hex SHA-256, or null on failure.
     */
    private function sha256File(string $path): ?string
    {
        $fh = @fopen($path, 'rb');
        if ($fh === false) {
            return null;
        }

        $ctx = hash_init('sha256');
        while (!feof($fh)) {
            $chunk = fread($fh, 65536);
            if ($chunk === false) {
                fclose($fh);
                return null;
            }
            hash_update($ctx, $chunk);
        }
        fclose($fh);

        return hash_final($ctx);
    }

    /**
     * Generate a temporary file path for the package download.
     *
     * @return string
     */
    private function tempFilePath(): string
    {
        return sys_get_temp_dir() . '/wpmgr-agent-update-' . bin2hex(random_bytes(8)) . '.zip';
    }

    /**
     * Retrieve the highest iat we have previously accepted (anti-rollback).
     * Returns 0 on first use or if the option is absent.
     *
     * @return int
     */
    private function getLastAcceptedIat(): int
    {
        if (!function_exists('get_option')) {
            return 0;
        }
        $stored = get_option(self::OPTION_LAST_IAT, 0);
        return is_numeric($stored) ? (int) $stored : 0;
    }

    /**
     * Persist the new highest accepted iat (anti-rollback high-water mark).
     *
     * @param int $iat Unix timestamp.
     * @return void
     */
    private function setLastAcceptedIat(int $iat): void
    {
        if (function_exists('update_option')) {
            update_option(self::OPTION_LAST_IAT, $iat, false);
        }
    }
}
