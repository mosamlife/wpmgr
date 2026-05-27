<?php
/**
 * Connector: Ed25519 JWT verification + anti-replay enforcement.
 *
 * Trust model:
 *   - The control plane signs a compact JWT (header.payload.signature) with its
 *     Ed25519 secret key. We verify the detached signature with the stored
 *     control-plane PUBLIC key using sodium_crypto_sign_verify_detached.
 *   - The signature is verified over `header.payload` (the signing input)
 *     BEFORE any claim is parsed or trusted.
 *   - Anti-replay: each token carries a unique `jti`. We reject a token whose
 *     `jti` has been seen inside the replay window, whose `exp` is in the past,
 *     or whose `exp` is more than 60s in the future (clock-skew clamp).
 *
 * No RSA. No phpseclib. Ed25519 only.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Verifies inbound signed requests and guards against token replay.
 */
final class Connector
{
    /** Replay window / max accepted token lifetime, in seconds. */
    public const REPLAY_WINDOW = 300;

    /** Maximum tolerated forward clock skew on `exp`, in seconds. */
    public const MAX_FUTURE_EXP = 60;

    /** Unqualified jti table name (prefixed at runtime). */
    public const JTI_TABLE = 'wpmgr_agent_jti';

    private Keystore $keystore;

    /**
     * @param Keystore $keystore Provides the control-plane public key.
     */
    public function __construct(Keystore $keystore)
    {
        $this->keystore = $keystore;
    }

    /**
     * Verify a compact JWS/JWT and enforce anti-replay.
     *
     * @param string $jwt       Compact token: base64url(header).base64url(payload).base64url(sig).
     * @param int|null $now     Override "current time" (testing); defaults to time().
     * @return array<string,mixed> The validated claim set on success.
     * @throws \RuntimeException With a generic message on ANY failure.
     */
    public function verify(string $jwt, ?int $now = null): array
    {
        $now = $now ?? time();

        $parts = explode('.', $jwt);
        if (count($parts) !== 3) {
            throw new \RuntimeException('WPMgr Agent: malformed token.');
        }

        [$encodedHeader, $encodedPayload, $encodedSig] = $parts;

        $signingInput = $encodedHeader . '.' . $encodedPayload;
        $signature    = self::base64UrlDecode($encodedSig);

        // ---- 1. Verify signature FIRST, before trusting anything else. ----
        $publicKey = $this->keystore->getControlPlanePublicKey();
        if ($publicKey === null || strlen($publicKey) !== SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES) {
            throw new \RuntimeException('WPMgr Agent: control-plane key not provisioned.');
        }

        if ($signature === '' || strlen($signature) !== SODIUM_CRYPTO_SIGN_BYTES) {
            throw new \RuntimeException('WPMgr Agent: invalid signature.');
        }

        $valid = sodium_crypto_sign_verify_detached($signature, $signingInput, $publicKey);
        if ($valid !== true) {
            throw new \RuntimeException('WPMgr Agent: signature verification failed.');
        }

        // ---- 2. Now it is safe to parse the header and payload. ----
        $header = self::decodeJson(self::base64UrlDecode($encodedHeader));
        if (!isset($header['alg']) || !is_string($header['alg']) || !hash_equals('EdDSA', $header['alg'])) {
            throw new \RuntimeException('WPMgr Agent: unexpected algorithm.');
        }

        $claims = self::decodeJson(self::base64UrlDecode($encodedPayload));

        // ---- 3. Temporal validation. ----
        if (!isset($claims['exp']) || !is_numeric($claims['exp'])) {
            throw new \RuntimeException('WPMgr Agent: missing exp.');
        }
        $exp = (int) $claims['exp'];

        if ($exp <= $now) {
            throw new \RuntimeException('WPMgr Agent: token expired.');
        }
        if ($exp > $now + self::MAX_FUTURE_EXP) {
            throw new \RuntimeException('WPMgr Agent: exp too far in the future.');
        }

        // ---- 4. Anti-replay via unique jti. ----
        if (!isset($claims['jti']) || !is_string($claims['jti']) || $claims['jti'] === '') {
            throw new \RuntimeException('WPMgr Agent: missing jti.');
        }
        $jti = $claims['jti'];

        if ($this->isJtiSeen($jti, $now)) {
            throw new \RuntimeException('WPMgr Agent: token replay detected.');
        }

        $this->recordJti($jti, $exp, $now);

        return $claims;
    }

    /**
     * Has this jti been recorded within the live replay window?
     *
     * @param string $jti Token identifier.
     * @param int    $now Current timestamp.
     * @return bool
     */
    private function isJtiSeen(string $jti, int $now): bool
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return false;
        }

        $table = $this->jtiTableName();
        $hash  = $this->hashJti($jti);

        // $table is built from a class constant + the trusted wpdb prefix (no
        // user input), so interpolating it ahead of prepare() is safe.
        // @phpstan-ignore-next-line
        $sql = $wpdb->prepare("SELECT 1 FROM {$table} WHERE jti_hash = %s AND expires_at >= %d LIMIT 1", $hash, $now);
        if (!is_string($sql)) {
            return false;
        }

        return $wpdb->get_var($sql) !== null;
    }

    /**
     * Record a jti so subsequent presentations are rejected as replays.
     *
     * @param string $jti Token identifier.
     * @param int    $exp Token expiry timestamp.
     * @param int    $now Current timestamp.
     * @return void
     */
    private function recordJti(string $jti, int $exp, int $now): void
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return;
        }

        // Opportunistically prune expired rows so the table stays bounded.
        $table = $this->jtiTableName();
        // $table is built from a class constant + the trusted wpdb prefix.
        // @phpstan-ignore-next-line
        $pruneSql = $wpdb->prepare("DELETE FROM {$table} WHERE expires_at < %d", $now);
        if (is_string($pruneSql)) {
            $wpdb->query($pruneSql);
        }

        $wpdb->insert(
            $table,
            [
                'jti_hash'   => $this->hashJti($jti),
                'expires_at' => max($exp, $now + self::REPLAY_WINDOW),
                'created_at' => $now,
            ],
            ['%s', '%d', '%d']
        );
    }

    /**
     * Compute the storage hash of a jti (fixed-width, avoids storing raw ids).
     *
     * @param string $jti Token identifier.
     * @return string Hex SHA-256.
     */
    private function hashJti(string $jti): string
    {
        return hash('sha256', $jti);
    }

    /**
     * Fully-qualified jti table name including the WP table prefix.
     *
     * @return string
     */
    private function jtiTableName(): string
    {
        $wpdb   = $this->wpdb();
        $prefix = ($wpdb !== null && $wpdb->prefix !== '') ? $wpdb->prefix : 'wp_';

        return $prefix . self::JTI_TABLE;
    }

    /**
     * Resolve the WordPress database handle, if one is available.
     *
     * Returns null outside a WordPress runtime (e.g. unit tests without a DB),
     * letting callers degrade gracefully.
     *
     * @return \wpdb|null
     */
    private function wpdb(): ?object
    {
        if (!isset($GLOBALS['wpdb']) || !is_object($GLOBALS['wpdb'])) {
            return null;
        }

        /** @var \wpdb $wpdb */
        $wpdb = $GLOBALS['wpdb'];

        return $wpdb;
    }

    /**
     * Decode a base64url segment to raw bytes.
     *
     * @param string $input Base64url string.
     * @return string Decoded bytes (empty string on failure).
     */
    private static function base64UrlDecode(string $input): string
    {
        $remainder = strlen($input) % 4;
        if ($remainder !== 0) {
            $input .= str_repeat('=', 4 - $remainder);
        }

        $decoded = base64_decode(strtr($input, '-_', '+/'), true);

        return $decoded === false ? '' : $decoded;
    }

    /**
     * Decode a JSON object into an associative array.
     *
     * @param string $json JSON bytes.
     * @return array<string,mixed>
     * @throws \RuntimeException On invalid JSON or non-object payload.
     */
    private static function decodeJson(string $json): array
    {
        $data = json_decode($json, true);
        if (!is_array($data)) {
            throw new \RuntimeException('WPMgr Agent: invalid token segment.');
        }

        /** @var array<string,mixed> $data */
        return $data;
    }
}
