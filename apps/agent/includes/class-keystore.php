<?php
/**
 * Keystore: AES-256-GCM encrypted-at-rest storage of cryptographic material.
 *
 * Stores:
 *   - The control-plane Ed25519 PUBLIC key (used to verify inbound JWTs).
 *   - This site's own Ed25519 keypair (generated on activation; the public
 *     half is shared with the control plane, the secret half signs responses).
 *
 * The AES-256-GCM master key never touches the database. It lives in a file
 * outside the web root, located via the WPMGR_AGENT_KEY_FILE constant (define
 * it in wp-config.php). If absent, a path under wp-content/../ is derived and
 * the file is created with 0600 perms on first use.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Encrypted keystore backed by wp-options + a file-based master key.
 */
final class Keystore
{
    /** Option name holding the encrypted control-plane public key. */
    public const OPTION_CP_PUBLIC_KEY = 'wpmgr_agent_cp_public_key';

    /** Option name holding the encrypted site Ed25519 keypair. */
    public const OPTION_SITE_KEYPAIR = 'wpmgr_agent_site_keypair';

    /** Length in bytes of an AES-256 key. */
    private const KEY_BYTES = 32;

    /**
     * Encrypt a plaintext blob with AES-256-GCM using the master key.
     *
     * Layout of the returned (base64) envelope: iv (12) || tag (16) || ciphertext.
     *
     * @param string $plaintext Raw bytes to protect.
     * @return string Base64-encoded envelope.
     * @throws \RuntimeException On encryption failure.
     */
    public function encrypt(string $plaintext): string
    {
        $key = $this->masterKey();
        $iv  = random_bytes(12);
        $tag = '';

        $ciphertext = openssl_encrypt(
            $plaintext,
            'aes-256-gcm',
            $key,
            OPENSSL_RAW_DATA,
            $iv,
            $tag,
            '',
            16
        );

        if ($ciphertext === false) {
            throw new \RuntimeException('WPMgr Agent: AES-256-GCM encryption failed.');
        }

        return base64_encode($iv . $tag . $ciphertext);
    }

    /**
     * Decrypt an envelope produced by encrypt().
     *
     * @param string $envelope Base64-encoded iv||tag||ciphertext.
     * @return string Recovered plaintext.
     * @throws \RuntimeException On malformed input or authentication failure.
     */
    public function decrypt(string $envelope): string
    {
        $raw = base64_decode($envelope, true);
        if ($raw === false || strlen($raw) < 28) {
            throw new \RuntimeException('WPMgr Agent: malformed ciphertext envelope.');
        }

        $iv         = substr($raw, 0, 12);
        $tag        = substr($raw, 12, 16);
        $ciphertext = substr($raw, 28);

        $key = $this->masterKey();

        $plaintext = openssl_decrypt(
            $ciphertext,
            'aes-256-gcm',
            $key,
            OPENSSL_RAW_DATA,
            $iv,
            $tag
        );

        if ($plaintext === false) {
            // GCM tag mismatch => tampered or wrong key. Do not leak details.
            throw new \RuntimeException('WPMgr Agent: ciphertext authentication failed.');
        }

        return $plaintext;
    }

    /**
     * Persist the control-plane Ed25519 public key (raw 32 bytes), encrypted.
     *
     * @param string $rawPublicKey 32-byte raw Ed25519 public key.
     * @return void
     */
    public function storeControlPlanePublicKey(string $rawPublicKey): void
    {
        update_option(self::OPTION_CP_PUBLIC_KEY, $this->encrypt($rawPublicKey), false);
    }

    /**
     * Retrieve and decrypt the control-plane Ed25519 public key.
     *
     * @return string|null Raw 32-byte public key, or null if not provisioned.
     */
    public function getControlPlanePublicKey(): ?string
    {
        $stored = get_option(self::OPTION_CP_PUBLIC_KEY);
        if (!is_string($stored) || $stored === '') {
            return null;
        }

        return $this->decrypt($stored);
    }

    /**
     * Generate this site's Ed25519 keypair and store it encrypted.
     *
     * @return string The raw 32-byte site public key (for sharing upstream).
     */
    public function generateSiteKeypair(): string
    {
        $keypair   = sodium_crypto_sign_keypair();
        $publicKey = sodium_crypto_sign_publickey($keypair);

        update_option(self::OPTION_SITE_KEYPAIR, $this->encrypt($keypair), false);

        // Wipe the in-memory keypair as soon as it is persisted.
        sodium_memzero($keypair);

        return $publicKey;
    }

    /**
     * Retrieve and decrypt this site's Ed25519 keypair (secret||public, 64+32).
     *
     * @return string|null Raw sodium keypair string, or null if absent.
     */
    public function getSiteKeypair(): ?string
    {
        $stored = get_option(self::OPTION_SITE_KEYPAIR);
        if (!is_string($stored) || $stored === '') {
            return null;
        }

        return $this->decrypt($stored);
    }

    /**
     * Load (or lazily create) the 32-byte AES master key from a file outside
     * the web root.
     *
     * Resolution order:
     *   1. WPMGR_AGENT_KEY_FILE constant (recommended; set in wp-config.php).
     *   2. A derived path one level above ABSPATH.
     *
     * @return string 32 raw bytes.
     * @throws \RuntimeException If the key cannot be read or created.
     */
    private function masterKey(): string
    {
        $path = $this->keyFilePath();

        if (is_readable($path)) {
            $key = file_get_contents($path);
            if ($key === false || strlen($key) !== self::KEY_BYTES) {
                throw new \RuntimeException('WPMgr Agent: master key file is invalid.');
            }

            return $key;
        }

        // First run: generate and persist a fresh master key with tight perms.
        $key = random_bytes(self::KEY_BYTES);

        $dir = dirname($path);
        if (!is_dir($dir)) {
            throw new \RuntimeException('WPMgr Agent: master key directory is missing.');
        }

        if (file_put_contents($path, $key, LOCK_EX) === false) {
            throw new \RuntimeException('WPMgr Agent: unable to write master key file.');
        }

        @chmod($path, 0600);

        return $key;
    }

    /**
     * Resolve the absolute path to the master key file (outside web root).
     *
     * @return string
     */
    private function keyFilePath(): string
    {
        if (defined('WPMGR_AGENT_KEY_FILE') && is_string(WPMGR_AGENT_KEY_FILE) && WPMGR_AGENT_KEY_FILE !== '') {
            return WPMGR_AGENT_KEY_FILE;
        }

        // Fall back to one directory above ABSPATH, which is outside the
        // document root on the vast majority of WordPress installs.
        $base = defined('ABSPATH') ? (string) ABSPATH : sys_get_temp_dir() . '/';

        return rtrim(dirname(rtrim($base, '/\\')), '/\\') . '/.wpmgr-agent-master.key';
    }
}
