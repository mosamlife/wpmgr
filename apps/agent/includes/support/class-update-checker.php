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
 * This file also carries the CP-commanded three-beat self-update (ARM in the
 * signed command request, APPLY in a separate cron bootstrap, CONFIRM by the
 * new code phoning home). It lives HERE, and not in a new file, because
 * `make agent-zip-wporg` physically excludes this one path from the wp.org
 * distribution build; see the block comment above stageSelfUpdate().
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
     * Cron hook that runs beat 2 (APPLY) of the three-beat self-update
     * protocol, in its own WordPress bootstrap. Bound in Plugin::registerHooks()
     * behind the same updateChecker-not-null guard that skips this whole class
     * on the wp.org distribution build, so the event can never fire into a
     * missing class there.
     */
    public const HOOK_APPLY = 'wpmgr_agent_self_update_apply';

    /**
     * wp-option holding the single staged self-update record written by beat 1
     * (ARM). Shape:
     *   ['from_version'=>string,'to_version'=>string,'staged_at'=>int,
     *    'expires_at'=>int,'token'=>string]
     *
     * AUTOLOADED on purpose, which is a change from the original cron-only
     * design. Beat 2 is no longer reachable only through the cron event: Plugin
     * binds a request-bound backstop on plugins_loaded whose ENTIRE gate is
     * "does this record exist", so while a record is live it is read on every
     * request the site serves. An autoloaded row rides the alloptions read
     * WordPress already performs on every boot, so the gate costs no extra query
     * in the one window where it is read often. The record is deleted the moment
     * it is claimed, so it exists for minutes in the whole life of a site.
     */
    public const OPTION_STAGED = 'wpmgr_agent_self_update_staged';

    /**
     * wp-option marking an apply that is CURRENTLY RUNNING: a unix timestamp
     * written by the request that won the claim, deleted when that apply reaches
     * any terminal path.
     *
     * The atomic claim of OPTION_STAGED (claimStagedRecord) already guarantees
     * that exactly one caller ever applies a GIVEN staged record, which is what
     * keeps the cron event and the request-bound backstop from both applying the
     * same stage. This marker covers the other overlap, the one the backstop
     * newly widens: a control plane that arms a SECOND update while the first is
     * still copying files stages a second record, and the backstop would claim
     * it on the very next request. Two Plugin_Upgrader runs over the agent's own
     * directory at once is precisely the outcome the three-beat design exists to
     * prevent.
     *
     * Read only on the rare request that has ALREADY found a staged record, so
     * it is deliberately not autoloaded. Stale-safe: a marker older than
     * LongRunningJob::TIME_LIMIT_SECONDS is ignored, the same bound the apply
     * itself runs under, so a hard-killed apply cannot block the next one for
     * longer than it could legitimately have run.
     */
    private const OPTION_APPLYING = 'wpmgr_agent_self_update_applying';

    /**
     * Seconds a staged record stays claimable. Beat 2 discards (never retries)
     * an expired record: a site whose loopback cron is broken simply never
     * reaches beat 2, which is safe because ARM moved nothing on disk.
     *
     * COUPLED TO THE CONTROL PLANE'S CONFIRM DEADLINE. Read this before
     * changing either number.
     *
     * The CP waits for beat 3 (the new build phoning home) for a bounded
     * window that depends on the cron_mode this agent reported in beat 1:
     *
     *   cron_mode "loopback" : 20 minutes  (agentConfirmDeadline)
     *   cron_mode "external" : 90 minutes  (agentConfirmDeadlineExternalCron)
     *
     * Both live in apps/api/internal/update/agent_worker.go. The stage MUST
     * outlive the CP's LONGEST patience, otherwise a site whose system cron
     * runs hourly expires its own staged record before the cron tick that
     * would have applied it, and the CP records a confirm timeout for a build
     * that was never given a chance to install. On a fleet that is not an edge
     * case, it is a coin flip, and it halts canary rollouts for no reason.
     *
     * 7200s (2 hours) sits 30 minutes clear of the 90-minute external-cron
     * deadline. If the CP window is ever raised, raise this FIRST and keep the
     * headroom. This value must never fall below agentConfirmDeadlineExternalCron.
     */
    public const STAGED_TTL_SECONDS = 7200;

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
     * Why the most recent fetchManifest()/verifyManifest() pair returned null.
     *
     * fetchManifest() collapses "the CP published nothing" and "the manifest
     * failed verification" into the same null return, which is exactly the
     * right shape for the transient-injection path but not for beat 1 of the
     * self-update protocol: the control plane has to be able to tell
     * "up_to_date" (benign, nothing to do) apart from "error" (worth surfacing).
     * Every terminal path of the two methods tags itself here so
     * stageSelfUpdate() can classify a null without re-running any check.
     *
     * Values: '' (never run), 'ok', 'no_update' (HTTP 204), 'up_to_date'
     * (signed manifest verified but not newer than on-disk), 'unavailable'
     * (transport/config), 'invalid' (unparseable body), 'rejected' (any other
     * verification-chain failure).
     */
    private string $lastManifestOutcome = '';

    // -------------------------------------------------------------------------
    // Constructor
    // -------------------------------------------------------------------------

    /**
     * @param Signer      $signer      Builds the four X-WPMgr-* auth headers.
     * @param Settings    $settings    Provides isEnrolled(), siteId(), cpUrl().
     * @param Keystore    $keystore    Provides the CP Ed25519 public key.
     * @param ReplayCache $replayCache jti single-use store.
     */
    public function __construct(
        Signer $signer,
        Settings $settings,
        Keystore $keystore,
        ReplayCache $replayCache
    ) {
        $this->signer      = $signer;
        $this->settings    = $settings;
        $this->keystore    = $keystore;
        $this->replayCache = $replayCache;
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
            // steady state, not a failure. Tag it so beat 1 can answer
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
        // applyStagedSelfUpdate() already armed the same bound for the whole
        // beat-2 request, and this second call is deliberate rather than
        // redundant: it re-arms the counter from zero so the download's budget
        // is measured from the transfer's own start instead of inheriting
        // whatever the manifest round trip above already spent, and it is the
        // only arming on the other route into this filter (an operator-initiated
        // or WordPress-auto-update install, which never passes through beat 2).
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
    // Three-beat CP-commanded self-update
    //
    // A normal update command cannot be used on the agent's own directory: the
    // plugin would overwrite its own files while its code is the code doing the
    // overwriting, inside the request that has to report the outcome. If that
    // request dies partway there is no working code left to report it, and the
    // snapshot + automatic rollback that protects every other plugin update
    // deliberately does NOT arm for the agent's own directory (whatever performs
    // the rollback is what is being replaced). Recovery would need per-site
    // filesystem access; across a fleet that is unrecoverable.
    //
    // So the CP-commanded path is split into three beats, each in its own
    // request:
    //   1. ARM    (stageSelfUpdate, inside the signed command request):
    //             verify only. Nothing on disk moves.
    //   2. APPLY  (applyStagedSelfUpdate): a SEPARATE WordPress bootstrap runs
    //             Plugin_Upgrader::upgrade(). No CP response rides on that
    //             request, so a mid-copy death is not a lost answer. Reached
    //             from the cron event AND from a request-bound plugins_loaded
    //             backstop, so a site whose WP-Cron never fires still applies.
    //   3. CONFIRM: the only trustworthy success signal is the NEW code
    //             phoning home: the version-changed boot metadata push in
    //             Plugin. A "scheduled" acknowledgement is NEVER success.
    // -------------------------------------------------------------------------

    /**
     * BEAT 1 (ARM). Verify that a newer agent build is on offer and, if so,
     * stage it for the apply cron. NOTHING on disk is moved, unpacked or
     * overwritten here; the only writes are the staged wp-option record and
     * the manifest site-transient.
     *
     * Runs the FULL existing verification chain by way of fetchManifest():
     * Ed25519 signature, cmd/slug/aud binding, iat/exp window, jti replay,
     * monotonic-iat anti-rollback, downgrade guard, https + exact-host
     * allowlist and package size cap. Nothing new is trusted here that the
     * one-click path does not already trust.
     *
     * @return array{status:string,ok:bool,from_version:string,to_version:string,detail:string,cron_mode:string,expires_at:int}
     */
    public function stageSelfUpdate(): array
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

        if (!function_exists('update_option') || !function_exists('wp_schedule_single_event')) {
            return $this->stageAnswer('not_eligible', $onDisk, '', 'WordPress option or cron API unavailable.');
        }

        // An unexpired record is already staged: never stage twice, and never
        // re-schedule. The existing event owns this window.
        $existing = $this->readStagedRecord();
        if ($existing !== null && time() <= (int) $existing['expires_at']) {
            $answer = $this->stageAnswer(
                'already_scheduled',
                (string) $existing['from_version'],
                (string) $existing['to_version'],
                'A self-update is already staged for this site.'
            );
            $answer['expires_at'] = (int) $existing['expires_at'];

            return $answer;
        }

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
        // credential) so beat 2's injectUpdate() offers this exact build to
        // Plugin_Upgrader without another CP round trip. Identical shape to the
        // caching injectUpdate() already performs.
        $toCache = $claims;
        unset($toCache['package_url']);
        if (function_exists('set_site_transient') && defined('HOUR_IN_SECONDS')) {
            set_site_transient(self::TRANSIENT_MANIFEST, $toCache, 12 * HOUR_IN_SECONDS);
        }

        $now    = time();
        $token  = bin2hex(random_bytes(16));
        $record = [
            'from_version' => $onDisk,
            'to_version'   => $toVersion,
            'staged_at'    => $now,
            'expires_at'   => $now + self::STAGED_TTL_SECONDS,
            'token'        => $token,
        ];
        update_option(self::OPTION_STAGED, $record, true);

        // Drop any previous outcome so a stale 'failed' from an earlier run can
        // never be read back as the result of THIS one.
        if (function_exists('delete_option')) {
            delete_option(AgentSelfUpdateCommand::OPTION_RESULT);
        }

        // The token rides in the event args for two reasons: it makes each
        // stage a distinct event (WP silently drops a duplicate single event
        // scheduled within 10 minutes of an identical one), and it lets beat 2
        // ignore a stale event left over from an earlier stage.
        $scheduled = wp_schedule_single_event($now, self::HOOK_APPLY, [$token]);

        if (function_exists('spawn_cron')) {
            @spawn_cron(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- spawn_cron may emit a harmless notice when DISABLE_WP_CRON is set; @-suppressed intentionally
        }

        // THIS RETURN VALUE USED TO BE DISCARDED, AND THAT WAS THE DEFECT.
        // wp_schedule_single_event answers false whenever another plugin
        // short-circuits the pre_schedule_event filter, whenever a schedule_event
        // filter returns something falsy, and for a duplicate single event
        // scheduled within ten minutes of an identical one. Beat 1 reported
        // "scheduled" in every one of those cases, so the control plane armed a
        // confirmation poll and then sat out its entire confirm window waiting
        // for a cron beat that had never been created. A site can be perfectly
        // healthy and chatty while this happens, which is what made it so hard
        // to read from the outside.
        //
        // The staged record is deliberately KEPT here rather than rolled back.
        // The apply no longer depends on the cron event at all: Plugin's
        // request-bound plugins_loaded backstop applies the very same record on
        // the next request this site serves, which on any site the control plane
        // can reach is seconds away. Deleting the record would throw that
        // recovery away and leave a fleet permanently unable to update itself
        // for as long as the offending filter is installed. The answer is still
        // "error" because the arm genuinely failed to do the thing it tried to
        // do, and an immediate, named failure is worth far more to an operator
        // than a twenty-minute silence. A re-run then answers already_scheduled
        // (record still live) or up_to_date (backstop already applied it), and
        // both of those resolve correctly at the control plane.
        if ($scheduled === false || (function_exists('is_wp_error') && is_wp_error($scheduled))) {
            return $this->stageAnswer(
                'error',
                $onDisk,
                '',
                'WordPress refused to schedule the apply event, so no cron beat exists for this stage '
                . '(a plugin or filter on this site is blocking wp_schedule_single_event). The stage was '
                . 'kept and the agent applies it from its own request-bound backstop instead; re-run this '
                . 'command to pick up the outcome.'
            );
        }

        $answer = $this->stageAnswer('scheduled', $onDisk, $toVersion, 'Self-update staged; apply runs in a separate request.');
        $answer['expires_at'] = $record['expires_at'];

        return $answer;
    }

    /**
     * True when a staged self-update record is present. This is the WHOLE gate
     * for Plugin's request-bound plugins_loaded backstop, and it is why that
     * backstop is close to free: on the overwhelmingly common boot there is no
     * record, and OPTION_STAGED is autoloaded so a live one costs no query at
     * all.
     *
     * Deliberately does NOT apply the expiry. An expired record still needs an
     * apply beat to run so that beat can DISCARD it and record 'expired', which
     * is the only way that outcome ever reaches the control plane.
     *
     * @return bool
     */
    public function hasStagedSelfUpdate(): bool
    {
        if (defined('WPMGR_WPORG_BUILD') && constant('WPMGR_WPORG_BUILD')) {
            return false;
        }

        return $this->readStagedRecord() !== null;
    }

    /**
     * BEAT 2 (APPLY). Runs in a SEPARATE WordPress bootstrap from the one that
     * answered the control plane. No CP response rides on this request, so a
     * death mid-copy is not a lost answer.
     *
     * TWO ENTRY POINTS, ONE APPLY. The cron event (HOOK_APPLY) was originally
     * the only one, and a single blocked or never-firing WP-Cron therefore
     * stranded the whole update with nothing left to retry it. Plugin now ALSO
     * binds a request-bound backstop on plugins_loaded, the same WP-Cron-
     * independence treatment GitHub issues #226 and #232 already applied to the
     * snapshot reclaim and the stalled-backup reaper. Both entry points land
     * here, and the claim below is what keeps them from ever both applying.
     *
     * THE REQUEST-ISOLATION INVARIANT IS PRESERVED, AND IT IS LOAD-BEARING. The
     * swap must never happen inside the request that has to report the outcome.
     * The backstop's gate runs on plugins_loaded, which on the arm's own request
     * fires long BEFORE the REST callback writes the staged record, so the arm
     * request finds nothing and arms nothing. Every apply therefore still
     * happens in a later request than the arm.
     *
     * Single-shot by construction: the staged record is CLAIMED before any
     * upgrade work begins, with a single atomic statement rather than a
     * read-then-delete, so two racing requests cannot both proceed. A request
     * that dies partway can never be retried. An expired record is discarded,
     * not retried. Nothing here schedules a follow-up event.
     *
     * Reuses the existing upgrader_pre_download / upgrader_source_selection
     * chain unchanged; there is zero new download, verification or unzip code
     * on this path.
     *
     * EVERY EXIT LEAVES A TRACE. Two of these paths used to return in total
     * silence, and because the recorded result is the only channel beat 2 has
     * back to the control plane, a stuck site was indistinguishable from a site
     * that had never been asked. A silent early return on a path whose entire
     * job is to report back is the same defect class as an arm reporting a
     * success it never verified.
     *
     * @param string $token Stage token carried in the cron event args. Empty
     *                      from the request-bound backstop, which is not an
     *                      event and therefore cannot be a STALE event.
     * @return void
     */
    public function applyStagedSelfUpdate(string $token = ''): void
    {
        if (defined('WPMGR_WPORG_BUILD') && constant('WPMGR_WPORG_BUILD')) {
            // Unreachable by construction (the wp.org build does not ship this
            // file at all), recorded anyway so no exit on this method is silent.
            $this->recordDiagnosticExit('not_eligible', '', '', 'Self-update is not part of this distribution build.');
            return;
        }
        if (!function_exists('get_option') || !function_exists('delete_option')) {
            // recordDiagnosticExit needs the very option API that is missing, so
            // it will no-op. Called regardless so this exit is not special-cased
            // into silence if the guard is ever relaxed.
            $this->recordDiagnosticExit('no_option_api', '', '', 'WordPress option API unavailable in the apply request.');
            return;
        }

        $record = $this->readStagedRecord();
        if ($record === null) {
            // Nothing staged, or already claimed by the other entry point. Both
            // are ordinary, and both used to be invisible. Recorded (once, see
            // recordDiagnosticExit) so an operator can tell "the apply beat ran
            // and found nothing" apart from "the apply beat never ran", which
            // from the control plane look identical.
            $this->recordDiagnosticExit(
                'not_staged',
                $this->onDiskVersion(),
                '',
                'An apply beat ran with no staged record to apply.'
            );
            return;
        }

        $recordToken = (string) $record['token'];
        if ($token !== '' && $recordToken !== '' && !hash_equals($recordToken, $token)) {
            // A stale duplicate event from an earlier stage. Leave the current
            // record alone so its own event, or the request-bound backstop, can
            // still claim it.
            $this->recordDiagnosticExit(
                'token_mismatch',
                (string) $record['from_version'],
                (string) $record['to_version'],
                'A stale apply event was ignored; its token does not match the live staged record.'
            );
            return;
        }

        // An apply that is ALREADY RUNNING owns the plugin directory. Checked
        // BEFORE the claim on purpose: bailing out here must leave the record
        // staged, so the next request applies it once the running one is done.
        // Only the claim WINNER takes ownership of the marker (below), so a
        // request that loses the claim can never clear a marker it does not own.
        if ($this->applyInProgress()) {
            $this->recordDiagnosticExit(
                'apply_in_progress',
                (string) $record['from_version'],
                (string) $record['to_version'],
                'Another apply beat is already running; the stage was left in place for it.'
            );
            return;
        }

        // CLAIM, atomically. Everything below runs at most once per staged
        // record, however many requests reach this line at the same moment.
        if (!$this->claimStagedRecord()) {
            $this->recordDiagnosticExit(
                'claim_lost',
                (string) $record['from_version'],
                (string) $record['to_version'],
                'Another request claimed this staged record first.'
            );
            return;
        }

        $this->beginApply();

        try {
            $this->applyClaimedRecord($record);
        } finally {
            $this->endApply();
        }
    }

    /**
     * The apply itself, once this caller is the confirmed sole owner of the
     * staged record AND of the in-flight marker. Split out so the claim/discard
     * state machine above reads as one, and so the marker is released on every
     * path through a single finally.
     *
     * @param array{from_version:string,to_version:string,staged_at:int,expires_at:int,token:string} $record Claimed record.
     * @return void
     */
    private function applyClaimedRecord(array $record): void
    {
        $from      = (string) $record['from_version'];
        $to        = (string) $record['to_version'];
        $expiresAt = (int) $record['expires_at'];

        if ($expiresAt > 0 && time() > $expiresAt) {
            $this->recordSelfUpdateResult('expired', $from, $to, 'Staged self-update expired before the apply request ran.');
            return;
        }

        $onDisk = $this->onDiskVersion();
        if (!version_compare($this->normalizeVersion($to), $this->normalizeVersion($onDisk), '>')) {
            // Already at (or past) the target: a second event for a build that
            // landed some other way. Discard rather than reinstall.
            $this->recordSelfUpdateResult('already_applied', $from, $to, 'On-disk version already at or past the staged target.');
            return;
        }

        // BEAT 2 IS A LONG-RUNNING BACKGROUND JOB AND HAS TO BE ARMED AS ONE.
        // Everything below pulls a few MiB over the network and then runs
        // WordPress's non-atomic install_package() over the result, inside a
        // wp-cron request whose PHP max_execution_time is 30s on a great many
        // hosts. Without this the download is killed by PHP long before the HTTP
        // client's own budget expires and the apply silently never happens: the
        // record was already CLAIMED by the caller, so nothing retries it, and
        // the control plane is left with a confirm timeout for a build that was
        // cut off mid-fetch.
        //
        // Bounded, never 0 (infinite). LongRunningJob's class doc and
        // UpdateCommand::APPLY_TIME_LIMIT_SECONDS carry the full reasoning:
        // max_execution_time is the ONE timer whose fatal still runs shutdown
        // functions, and discarding it leaves only an FPM SIGTERM that runs none
        // of them. 900s is the same bound every other long job in the agent
        // uses, and it also keeps a worst-case apply inside the control plane's
        // SHORTEST beat-3 confirm deadline (20 minutes on loopback cron), so a
        // site that does hit this cap still gets to report the failure instead
        // of going quiet past the deadline. PACKAGE_TIMEOUT (300s) is sized to
        // sit at one third of it, so the transfer can never be the thing that
        // exhausts this budget.
        //
        // Placed HERE, past every early exit in the caller, on purpose. A cron
        // request runs every due event and the request-bound backstop runs on
        // every request at all, so the normal case is a caller that found
        // nothing to do; neither must raise the limit for whatever unrelated
        // work follows it in the same request.
        if (function_exists('set_time_limit')) {
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,Squiz.PHP.DiscouragedFunctions.Discouraged -- bounded-but-generous cap so a hung self-update apply hits a recoverable max_execution_time fatal rather than only an unrecoverable FPM hard-kill; @-guarded, no-op when disabled
        }

        $this->runUpgrade($from, $to);
    }

    /**
     * The Plugin_Upgrader half of beat 2, split out so applyStagedSelfUpdate()
     * reads as the claim/discard state machine it is.
     *
     * Mirrors UpdateRunner's plugin branch: it captures active state BEFORE the
     * upgrade (Plugin_Upgrader::upgrade() silently deactivates and never
     * re-activates) and clears maintenance mode on EVERY terminal path, so a
     * failed apply leaves a working, active site rather than a deactivated
     * agent behind a maintenance page.
     *
     * @param string $from On-disk version at stage time.
     * @param string $to   Verified target version.
     * @return void
     */
    private function runUpgrade(string $from, string $to): void
    {
        if (!defined('ABSPATH')) {
            $this->recordSelfUpdateResult('failed', $from, $to, 'ABSPATH is not defined; upgrader unavailable.');
            return;
        }

        $adminDir = (string) constant('ABSPATH') . 'wp-admin/includes/';
        foreach (['file.php', 'misc.php', 'plugin.php', 'class-wp-upgrader.php'] as $adminInclude) {
            if (is_file($adminDir . $adminInclude)) {
                require_once $adminDir . $adminInclude;
            }
        }

        if (!class_exists('\Plugin_Upgrader') || !class_exists('\Automatic_Upgrader_Skin')) {
            $this->recordSelfUpdateResult('failed', $from, $to, 'Upgrader API unavailable.');
            return;
        }

        $wasActive        = function_exists('is_plugin_active')             ? \is_plugin_active(self::PLUGIN_KEY) : false;
        $wasNetworkActive = function_exists('is_plugin_active_for_network') ? \is_plugin_active_for_network(self::PLUGIN_KEY) : false;

        // Force WordPress to rebuild the plugin-update transient so the
        // site_transient_update_plugins filter (injectUpdate) offers the build
        // stage() just verified. Plugin_Upgrader::upgrade() reads that transient
        // to find the package.
        if (function_exists('delete_site_transient')) {
            delete_site_transient('update_plugins');
        }

        $upgrader = new \Plugin_Upgrader(new \Automatic_Upgrader_Skin());
        $result   = null;
        $thrown   = '';

        try {
            $result = $upgrader->upgrade(self::PLUGIN_KEY);
        } catch (\Throwable $e) {
            $thrown = $e->getMessage();
        } finally {
            // GUARANTEE: maintenance mode is cleared on every terminal path of
            // this upgrade, whether it succeeded, returned a WP_Error, or threw.
            Maintenance::clear($upgrader);
        }

        if ($thrown !== '') {
            $this->recordSelfUpdateResult('failed', $from, $to, 'Upgrader threw: ' . $thrown);
            $this->restoreActiveState($wasActive, $wasNetworkActive);
            return;
        }

        if (is_object($result) && method_exists($result, 'get_error_message')) {
            $code    = method_exists($result, 'get_error_code') ? (string) $result->get_error_code() : '';
            $message = (string) $result->get_error_message();
            $this->recordSelfUpdateResult(
                'failed',
                $from,
                $to,
                'WP_Error' . ($code !== '' ? ' (' . $code . ')' : '') . ': ' . $message
            );
            $this->restoreActiveState($wasActive, $wasNetworkActive);
            return;
        }

        if ($result === false || $result === null) {
            $this->recordSelfUpdateResult('failed', $from, $to, 'Upgrader reported no result.');
            $this->restoreActiveState($wasActive, $wasNetworkActive);
            return;
        }

        // The new files are on disk, but THIS request is still running the old
        // code, so it is in no position to declare success to anyone. It records
        // a local outcome and stops. Beat 3, the version-changed boot metadata
        // push from the NEW code, is the only success signal the CP trusts.
        $this->restoreActiveState($wasActive, $wasNetworkActive);
        $this->flushCache();
        if (function_exists('delete_site_transient')) {
            delete_site_transient('update_plugins');
        }
        $this->recordSelfUpdateResult('applied', $from, $to, '');
    }

    /**
     * Re-activate the agent when Plugin_Upgrader::upgrade() left it deactivated.
     *
     * WordPress's upgrader registers an upgrader_pre_install hook that calls
     * deactivate_plugins($plugin, silent=true) and does NOT re-activate
     * afterwards. For any other plugin that is merely untidy; for the agent it
     * would sever the control-plane connection, so restoring the pre-upgrade
     * state is part of "leave the site working".
     *
     * @param bool $wasActive        Site-level active state before the upgrade.
     * @param bool $wasNetworkActive Network active state before the upgrade.
     * @return void
     */
    private function restoreActiveState(bool $wasActive, bool $wasNetworkActive): void
    {
        if (!$wasActive && !$wasNetworkActive) {
            return;
        }
        if (!function_exists('activate_plugin')) {
            return;
        }

        // Refresh the plugin cache first: the upgrade may have rewritten the
        // main plugin file, so the cached header data is stale.
        if (function_exists('wp_clean_plugins_cache')) {
            \wp_clean_plugins_cache(true);
        }

        try {
            $activated = \activate_plugin(self::PLUGIN_KEY, '', $wasNetworkActive, true);
            if (function_exists('is_wp_error') && \is_wp_error($activated)) {
                error_log('wpmgr-agent: UpdateChecker post-apply reactivation failed.');
            }
        } catch (\Throwable $e) {
            error_log('wpmgr-agent: UpdateChecker post-apply reactivation threw: ' . $e->getMessage());
        }
    }

    /**
     * Read and shape-validate the staged record. Returns null when absent or
     * malformed (a malformed record is treated as "nothing staged").
     *
     * Does NOT apply the expiry; callers decide what an expired record means
     * (beat 1 re-stages over it, beat 2 discards it and records the reason).
     *
     * @return array{from_version:string,to_version:string,staged_at:int,expires_at:int,token:string}|null
     */
    private function readStagedRecord(): ?array
    {
        if (!function_exists('get_option')) {
            return null;
        }

        $stored = get_option(self::OPTION_STAGED, null);
        if (!is_array($stored)) {
            return null;
        }

        $to = isset($stored['to_version']) && is_string($stored['to_version']) ? $stored['to_version'] : '';
        if ($to === '') {
            return null;
        }

        return [
            'from_version' => isset($stored['from_version']) && is_string($stored['from_version']) ? $stored['from_version'] : '',
            'to_version'   => $to,
            'staged_at'    => isset($stored['staged_at']) && is_numeric($stored['staged_at']) ? (int) $stored['staged_at'] : 0,
            'expires_at'   => isset($stored['expires_at']) && is_numeric($stored['expires_at']) ? (int) $stored['expires_at'] : 0,
            'token'        => isset($stored['token']) && is_string($stored['token']) ? $stored['token'] : '',
        ];
    }

    /**
     * ATOMICALLY claim the staged record: delete it in ONE statement and answer
     * whether THIS caller is the one that removed it.
     *
     * The original read-then-delete_option pair was a claim in name only. Two
     * requests could both read the record and both call delete_option, and both
     * would then run Plugin_Upgrader over the agent's own directory at the same
     * time. That window was narrow while a cron event was the only way in; with
     * the request-bound backstop firing on every request of a chatty site it is
     * not narrow at all. A single DELETE is decided by the database, so exactly
     * one caller can ever see one row removed, across processes AND across
     * nodes, which an flock() would not cover on a shared filesystem.
     *
     * The option caches are invalidated immediately afterwards because the raw
     * statement bypasses them. Correctness does not rest on that: a caller
     * served a stale cached record still has to win its own DELETE, and it
     * cannot.
     *
     * Degrades to the plain delete_option when there is no usable $wpdb (there
     * always is one inside WordPress; this keeps the method callable from a
     * bare unit-test bootstrap).
     *
     * @return bool True when this caller removed the row and now owns the apply.
     */
    private function claimStagedRecord(): bool
    {
        global $wpdb;

        $usable = isset($wpdb)
            && is_object($wpdb)
            && isset($wpdb->options)
            && is_string($wpdb->options)
            && $wpdb->options !== ''
            && method_exists($wpdb, 'query')
            && method_exists($wpdb, 'prepare');

        if (!$usable) {
            if (function_exists('delete_option')) {
                delete_option(self::OPTION_STAGED);
            }

            return true;
        }

        $table = $wpdb->options;

        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- compare-and-delete claim of one option row; get_option/delete_option is a read-then-write with no atomicity and no core API exposes an atomic equivalent, and a cached read is exactly what must not decide the winner
        $deleted = $wpdb->query(
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- $table is $wpdb->options, a WordPress-derived table name, never user input; the only value is bound through prepare()
            $wpdb->prepare("DELETE FROM {$table} WHERE option_name = %s", self::OPTION_STAGED)
        );

        // The statement went behind WordPress's back, so drop what it cached.
        if (function_exists('wp_cache_delete')) {
            wp_cache_delete(self::OPTION_STAGED, 'options');
            wp_cache_delete('alloptions', 'options');
        }

        return is_numeric($deleted) && (int) $deleted === 1;
    }

    /**
     * True when another apply beat is running and has not yet passed its own
     * execution ceiling. See OPTION_APPLYING for what this covers that the
     * record claim does not.
     *
     * @return bool
     */
    private function applyInProgress(): bool
    {
        if (!function_exists('get_option')) {
            return false;
        }

        $startedAt = (int) get_option(self::OPTION_APPLYING, 0);
        if ($startedAt <= 0) {
            return false;
        }

        // A marker older than the ceiling the apply itself runs under belongs to
        // a request that can no longer be alive. Ignore it rather than let one
        // hard-killed apply block this site's updates forever.
        return (time() - $startedAt) < LongRunningJob::TIME_LIMIT_SECONDS;
    }

    /**
     * Take ownership of the in-flight marker. Called ONLY by the caller that
     * won the record claim, so the release in its finally can never clear a
     * marker belonging to somebody else.
     *
     * @return void
     */
    private function beginApply(): void
    {
        if (function_exists('update_option')) {
            update_option(self::OPTION_APPLYING, time(), false);
        }
    }

    /**
     * Release the in-flight marker. Runs in a finally, so it also runs when the
     * apply threw.
     *
     * @return void
     */
    private function endApply(): void
    {
        if (function_exists('delete_option')) {
            delete_option(self::OPTION_APPLYING);
        }
    }

    /**
     * Record an apply-beat outcome for a path that did NO work, so no exit of
     * beat 2 is invisible from the control plane.
     *
     * NEVER OVERWRITES. A terminal outcome (applied, failed, expired,
     * already_applied) is written unconditionally by recordSelfUpdateResult;
     * this one writes only when nothing is stored at all. Two reasons, both
     * load-bearing:
     *
     *   - Both entry points run in the same wp-cron request when a cron tick
     *     also carries the due apply event. Whichever runs second finds nothing
     *     staged, and without this rule it would clobber the winner's 'applied'
     *     with a 'not_staged' and report the exact opposite of what happened.
     *   - The in-flight path is reached by every request that arrives while an
     *     apply is running. Writing on each of those would be an option write
     *     per request for as long as the apply takes.
     *
     * Beat 1 deletes the stored result when it stages, so "nothing is stored"
     * means "nothing has been recorded for THIS cycle yet", which is exactly the
     * scope wanted.
     *
     * @param string $status Diagnostic status (not one of the terminal four).
     * @param string $from   Best-known on-disk version (may be empty).
     * @param string $to     Staged target version (may be empty).
     * @param string $detail Human-readable, non-secret detail.
     * @return void
     */
    private function recordDiagnosticExit(string $status, string $from, string $to, string $detail): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $stored = get_option(AgentSelfUpdateCommand::OPTION_RESULT, null);
        if (is_array($stored) && isset($stored['status']) && is_string($stored['status']) && $stored['status'] !== '') {
            return;
        }

        $this->recordSelfUpdateResult($status, $from, $to, $detail);
    }

    /**
     * Persist the apply outcome so the next metadata push carries it to the CP.
     *
     * The stored detail is scrubbed of anything URL-shaped: upgrader and skin
     * messages can echo a package string back, and the manifest's package_url
     * is a short-lived bearer credential that must never reach a CP-visible
     * field or a log line.
     *
     * @param string $status A terminal outcome (applied|failed|expired|
     *                       already_applied) or, through recordDiagnosticExit,
     *                       one of the did-no-work diagnostics. The control
     *                       plane branches on the four it knows and surfaces
     *                       anything else verbatim, so a status added here needs
     *                       no control-plane change to reach an operator.
     * @param string $from   On-disk version at stage time.
     * @param string $to     Staged target version.
     * @param string $detail Human-readable, non-secret detail (may be empty).
     * @return void
     */
    private function recordSelfUpdateResult(string $status, string $from, string $to, string $detail): void
    {
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
            ],
            false
        );
    }

    /**
     * Build a beat-1 answer in the single shape the control plane parses.
     *
     * @param string $status One of not_eligible|up_to_date|scheduled|already_scheduled|error.
     * @param string $from   On-disk version.
     * @param string $to     Target version ('' when there is none).
     * @param string $detail Human-readable, non-secret detail.
     * @return array{status:string,ok:bool,from_version:string,to_version:string,detail:string,cron_mode:string,expires_at:int}
     */
    private function stageAnswer(string $status, string $from, string $to, string $detail): array
    {
        return [
            'status'       => $status,
            // 'scheduled' is an ACKNOWLEDGEMENT, never a success: the update has
            // not been applied and may never be. The CP treats it as unconfirmed
            // until the new code phones home (beat 3).
            'ok'           => $status !== 'error',
            'from_version' => $from,
            'to_version'   => $to,
            'detail'       => $detail,
            'cron_mode'    => (defined('DISABLE_WP_CRON') && constant('DISABLE_WP_CRON')) ? 'external' : 'loopback',
            'expires_at'   => 0,
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
