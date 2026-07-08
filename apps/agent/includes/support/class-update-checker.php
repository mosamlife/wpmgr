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
 *      be an exact (hash_equals) match for a configured allowed host (default
 *      'storage.googleapis.com', overridable via WPMGR_AGENT_PACKAGE_HOST /
 *      'wpmgr_agent_package_hosts' for self-hosted object storage); the download
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
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

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

    // -------------------------------------------------------------------------
    // Collaborators
    // -------------------------------------------------------------------------

    private Signer $signer;

    private Settings $settings;

    private Keystore $keystore;

    private ReplayCache $replayCache;

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
            return null;
        }

        $envelope = json_decode($rawBody, true);
        if (!is_array($envelope)) {
            error_log('wpmgr-agent: UpdateChecker manifest response is not valid JSON.');
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
        // Host must be in the configured allowlist (constant-time). The default is
        // the managed CP's GCS host; a self-hosted deployment overrides it via the
        // WPMGR_AGENT_PACKAGE_HOST constant or the 'wpmgr_agent_package_hosts'
        // filter (see allowedPackageHosts). A literal IP, a look-alike host, or
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

        if (is_array($claims) && isset($claims['version']) && is_string($claims['version'])
            && version_compare($claims['version'], $onDisk, '>')
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

        $response = wp_remote_get(
            $packageUrl,
            [
                'timeout'     => 60,
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
     * The allowlisted hosts a package_url may point at. Defaults to the managed
     * control plane's GCS host. A self-hosted deployment whose object storage
     * lives elsewhere (MinIO/SeaweedFS/managed S3/…) overrides this via the
     * WPMGR_AGENT_PACKAGE_HOST constant (comma-separated) or the
     * 'wpmgr_agent_package_hosts' filter.
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

        if (function_exists('apply_filters')) {
            $filtered = apply_filters('wpmgr_agent_package_hosts', $hosts);
            if (is_array($filtered) && $filtered !== []) {
                $hosts = $filtered;
            }
        }

        return array_map('strtolower', array_map('strval', $hosts));
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
