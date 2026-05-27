<?php
/**
 * Signer: builds the canonical message and the four agent-auth headers for
 * outbound (agent -> control-plane) requests.
 *
 * The canonical message that is signed with the agent's Ed25519 PRIVATE key is,
 * byte-for-byte (LF separators, no trailing newline):
 *
 *   METHOD\nPATH\nTIMESTAMP\nNONCE\nhex(sha256(body))
 *
 *   - METHOD     upper-case HTTP verb, e.g. "POST".
 *   - PATH       request path only (no host, no query), e.g. "/agent/v1/metadata".
 *   - TIMESTAMP  the exact X-WPMgr-Timestamp header string (Unix seconds).
 *   - NONCE      the exact X-WPMgr-Nonce header string.
 *   - body-hash  lower-case hex SHA-256 of the raw request body (sha256 of the
 *                empty string when there is no body).
 *
 * The detached signature and the agent public key are transmitted as
 * base64 (standard alphabet, with padding) headers.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Produces Ed25519-signed request headers for the control plane.
 */
final class Signer
{
    /** Header carrying the base64-std agent Ed25519 public key. */
    public const HEADER_KEY = 'X-WPMgr-Agent-Key';

    /** Header carrying the Unix-seconds timestamp (decimal string). */
    public const HEADER_TIMESTAMP = 'X-WPMgr-Timestamp';

    /** Header carrying the per-request nonce (random hex). */
    public const HEADER_NONCE = 'X-WPMgr-Nonce';

    /** Header carrying the base64-std detached Ed25519 signature. */
    public const HEADER_SIGNATURE = 'X-WPMgr-Signature';

    private Keystore $keystore;

    /**
     * @param Keystore $keystore Source of the agent keypair.
     */
    public function __construct(Keystore $keystore)
    {
        $this->keystore = $keystore;
    }

    /**
     * Build the exact canonical signing message.
     *
     * @param string $method    HTTP method (forced upper-case).
     * @param string $path       Request path only (no host/query).
     * @param string $timestamp  Exact timestamp header string.
     * @param string $nonce      Exact nonce header string.
     * @param string $body       Raw request body ('' when none).
     * @return string Canonical message: METHOD\nPATH\nTS\nNONCE\nhex(sha256(body)).
     */
    public static function canonicalMessage(
        string $method,
        string $path,
        string $timestamp,
        string $nonce,
        string $body
    ): string {
        $bodyHash = hash('sha256', $body);

        return strtoupper($method) . "\n"
            . $path . "\n"
            . $timestamp . "\n"
            . $nonce . "\n"
            . $bodyHash;
    }

    /**
     * Build the four agent-auth headers for a request.
     *
     * @param string   $method HTTP method.
     * @param string   $path   Request path only (no host/query).
     * @param string   $body   Raw request body ('' when none).
     * @param int|null $now    Override timestamp (testing); defaults to time().
     * @return array{
     *     'X-WPMgr-Agent-Key':string,
     *     'X-WPMgr-Timestamp':string,
     *     'X-WPMgr-Nonce':string,
     *     'X-WPMgr-Signature':string
     * }
     * @throws \RuntimeException When the agent keypair is missing/invalid.
     */
    public function signHeaders(string $method, string $path, string $body, ?int $now = null): array
    {
        $keypair = $this->keystore->getSiteKeypair();
        if ($keypair === null || $keypair === '') {
            throw new \RuntimeException('WPMgr Agent: site keypair not provisioned.');
        }

        $secretKey = sodium_crypto_sign_secretkey($keypair);
        $publicKey = sodium_crypto_sign_publickey($keypair);
        sodium_memzero($keypair);

        $timestamp = (string) ($now ?? time());
        $nonce     = bin2hex(random_bytes(16));

        $message   = self::canonicalMessage($method, $path, $timestamp, $nonce, $body);
        $signature = sodium_crypto_sign_detached($message, $secretKey);
        sodium_memzero($secretKey);

        return [
            self::HEADER_KEY       => base64_encode($publicKey),
            self::HEADER_TIMESTAMP => $timestamp,
            self::HEADER_NONCE     => $nonce,
            self::HEADER_SIGNATURE => base64_encode($signature),
        ];
    }

    /**
     * Return the agent's raw Ed25519 public key, generating the keypair if it
     * does not yet exist.
     *
     * @return string Raw 32-byte public key.
     */
    public function agentPublicKey(): string
    {
        $keypair = $this->keystore->getSiteKeypair();
        if ($keypair === null || $keypair === '') {
            return $this->keystore->generateSiteKeypair();
        }

        $publicKey = sodium_crypto_sign_publickey($keypair);
        sodium_memzero($keypair);

        return $publicKey;
    }

    /**
     * Return the agent's public key as a base64-std string (the wire format).
     *
     * @return string
     */
    public function agentPublicKeyBase64(): string
    {
        return base64_encode($this->agentPublicKey());
    }
}
