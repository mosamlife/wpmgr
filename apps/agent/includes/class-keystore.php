<?php
/**
 * Keystore: AES-256-GCM encrypted-at-rest storage of cryptographic material.
 *
 * Stores:
 *   - The control-plane Ed25519 PUBLIC key (used to verify inbound JWTs).
 *   - This site's own Ed25519 keypair (generated on activation; the public
 *     half is shared with the control plane, the secret half signs responses).
 *
 * The AES-256-GCM master key never touches the database. It is acquired from a
 * portable, deterministic source (in priority order):
 *
 *   1. The WPMGR_AGENT_KEY_FILE constant (define it in wp-config.php).
 *   2. Derivation from the wp-config.php secret salts (AUTH_KEY, ...) via
 *      HKDF-SHA256. This is the preferred default: the salts live outside the
 *      database and are present on virtually every real install, so no file
 *      write is needed and the key is identical across requests.
 *   3. A 0600 key file written to the first writable candidate location, with
 *      web-root locations hardened by index.php + .htaccess.
 *
 * To stay deterministic (the keystore must decrypt what it earlier encrypted),
 * the chosen source is pinned in a wp-option marker the first time a key is
 * established, so later requests never silently switch sources.
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

    /**
     * Option name holding the site's age X25519 identity (raw 32-byte secret
     * scalar), encrypted at rest. This is the PRIVATE backup-decryption key; it
     * NEVER leaves the keystore and is NEVER transmitted to the control plane.
     */
    public const OPTION_AGE_IDENTITY = 'wpmgr_agent_age_identity';

    /**
     * Option pinning which master-key source this install uses, so decrypt
     * always re-derives/reads the exact same key. Value is one of:
     *   ['source' => 'constant']
     *   ['source' => 'salts']
     *   ['source' => 'file', 'path' => '/abs/path']
     */
    public const OPTION_MASTER_KEY_SOURCE = 'wpmgr_agent_master_key_source';

    /** Length in bytes of an AES-256 key. */
    private const KEY_BYTES = 32;

    /** Fixed HKDF info/context string for salt-derived master keys (versioned). */
    private const HKDF_INFO = 'wpmgr-agent-master-v1';

    /**
     * WordPress secret-salt constants used (in this order) as HKDF input keying
     * material. Order is fixed so derivation is stable across requests.
     *
     * @var list<string>
     */
    private const SALT_CONSTANTS = [
        'AUTH_KEY',
        'SECURE_AUTH_KEY',
        'LOGGED_IN_KEY',
        'NONCE_KEY',
        'AUTH_SALT',
        'SECURE_AUTH_SALT',
        'LOGGED_IN_SALT',
        'NONCE_SALT',
    ];

    /**
     * Well-known placeholder values shipped in wp-config-sample.php. If a salt
     * matches one of these it carries no entropy and must be rejected.
     *
     * @var list<string>
     */
    private const SALT_PLACEHOLDERS = [
        'put your unique phrase here',
    ];

    /**
     * Minimum combined length (bytes) the concatenated salts must reach before
     * we trust them as keying material. Eight default WP salts are 64+ chars
     * each; we require well above any single-salt fluke.
     */
    private const SALT_MIN_COMBINED_LENGTH = 96;

    /** Cached resolved master key for the lifetime of this request. */
    private ?string $cachedKey = null;

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
     * Clear the keys that bind this agent to a specific control-plane
     * enrollment: the CP's Ed25519 public key, and this site's Ed25519
     * keypair. Used by the admin "Disconnect" flow so a fresh enrollment
     * (potentially against a different CP) generates a new identity.
     *
     * Intentionally does NOT touch the age identity (OPTION_AGE_IDENTITY) —
     * deleting it would orphan ciphertext from any prior backups, making
     * them undecryptable. The operator can wipe it manually if they want a
     * true clean slate.
     *
     * @return void
     */
    public function clearSiteIdentity(): void
    {
        delete_option(self::OPTION_CP_PUBLIC_KEY);
        delete_option(self::OPTION_SITE_KEYPAIR);
    }

    /**
     * Persist the site's age X25519 secret scalar (raw 32 bytes), encrypted.
     *
     * The secret is the ONLY key that can decrypt this site's backups. It is
     * stored AES-256-GCM-encrypted under the master key, exactly like the
     * Ed25519 keypair, and is never logged or transmitted.
     *
     * @param string $rawSecret Raw 32-byte X25519 scalar.
     * @return void
     */
    public function storeAgeIdentity(string $rawSecret): void
    {
        update_option(self::OPTION_AGE_IDENTITY, $this->encrypt($rawSecret), false);
    }

    /**
     * Retrieve and decrypt the site's age X25519 secret scalar.
     *
     * @return string|null Raw 32-byte X25519 scalar, or null if not provisioned.
     */
    public function getAgeIdentity(): ?string
    {
        $stored = get_option(self::OPTION_AGE_IDENTITY);
        if (!is_string($stored) || $stored === '') {
            return null;
        }

        return $this->decrypt($stored);
    }

    /**
     * Whether an age identity has been provisioned for this site.
     *
     * @return bool
     */
    public function hasAgeIdentity(): bool
    {
        $stored = get_option(self::OPTION_AGE_IDENTITY);

        return is_string($stored) && $stored !== '';
    }

    /**
     * Resolve the 32-byte AES master key for this install.
     *
     * The key never lives in the database. Resolution honours the pinned source
     * marker (if any) so decrypt always re-derives/reads the identical key, then
     * falls back to source discovery on first use:
     *
     *   1. WPMGR_AGENT_KEY_FILE constant.
     *   2. Derivation from wp-config secret salts (HKDF-SHA256). Preferred.
     *   3. A 0600 key file at the first writable candidate location.
     *
     * @return string 32 raw bytes.
     * @throws \RuntimeException If no portable key source can be established.
     */
    private function masterKey(): string
    {
        if ($this->cachedKey !== null) {
            return $this->cachedKey;
        }

        $key = $this->resolveMasterKey();
        if (strlen($key) !== self::KEY_BYTES) {
            throw new \RuntimeException('WPMgr Agent: derived master key has the wrong length.');
        }

        $this->cachedKey = $key;

        return $key;
    }

    /**
     * Establish the master key for this install, pinning the source on first
     * use and honouring an already-pinned source thereafter.
     *
     * @return string 32 raw bytes.
     * @throws \RuntimeException If the key cannot be established.
     */
    private function resolveMasterKey(): string
    {
        $pinned = $this->pinnedSource();

        // Honour an already-pinned source so we never silently switch keys.
        if ($pinned !== null) {
            switch ($pinned['source']) {
                case 'constant':
                    $key = $this->keyFromConstant();
                    if ($key !== null) {
                        return $key;
                    }
                    break;
                case 'salts':
                    $key = $this->keyFromSalts();
                    if ($key !== null) {
                        return $key;
                    }
                    throw new \RuntimeException(
                        'WPMgr Agent: pinned salt-derived master key is no longer available '
                        . '(wp-config salts changed or were removed).'
                    );
                case 'file':
                    $path = isset($pinned['path']) && is_string($pinned['path']) ? $pinned['path'] : '';
                    $key  = $path !== '' ? $this->readKeyFile($path) : null;
                    if ($key !== null) {
                        return $key;
                    }
                    throw new \RuntimeException('WPMgr Agent: pinned master key file is missing or invalid.');
            }
            // 'constant' fell through (constant undefined now): continue to
            // re-discover, but keep backward-compat file reads below.
        }

        // First run (or unpinned legacy install): discover a source in order.

        // 1. Explicit constant.
        $key = $this->keyFromConstant();
        if ($key !== null) {
            $this->pinSource(['source' => 'constant']);
            return $key;
        }

        // 1b. Backward-compat: reuse any key file written by a prior version
        // (incl. the old dirname(ABSPATH) path) before generating anew.
        foreach ($this->legacyKeyFilePaths() as $legacy) {
            $key = $this->readKeyFile($legacy);
            if ($key !== null) {
                $this->pinSource(['source' => 'file', 'path' => $legacy]);
                return $key;
            }
        }

        // 2. Preferred portable default: derive from wp-config salts.
        $key = $this->keyFromSalts();
        if ($key !== null) {
            $this->pinSource(['source' => 'salts']);
            return $key;
        }

        // 3. Fallback: write a key file to the first writable candidate.
        $written = $this->writeKeyFileToFirstWritable();
        if ($written !== null) {
            $this->pinSource(['source' => 'file', 'path' => $written['path']]);
            return $written['key'];
        }

        throw new \RuntimeException(
            'WPMgr Agent: unable to establish a master key. Define WPMGR_AGENT_KEY_FILE '
            . 'to a writable path, or ensure wp-config.php secret salts are set.'
        );
    }

    /**
     * Read the pinned master-key source marker, if present and well-formed.
     *
     * @return array{source:string,path?:string}|null
     */
    private function pinnedSource(): ?array
    {
        $stored = get_option(self::OPTION_MASTER_KEY_SOURCE);
        if (!is_array($stored) || !isset($stored['source']) || !is_string($stored['source'])) {
            return null;
        }

        $marker = ['source' => $stored['source']];
        if (isset($stored['path']) && is_string($stored['path'])) {
            $marker['path'] = $stored['path'];
        }

        return $marker;
    }

    /**
     * Pin the master-key source so later requests resolve the same key. Only
     * the source/path (never the key itself) is persisted.
     *
     * @param array{source:string,path?:string} $marker Source descriptor.
     * @return void
     */
    private function pinSource(array $marker): void
    {
        update_option(self::OPTION_MASTER_KEY_SOURCE, $marker, false);
    }

    /**
     * Obtain a 32-byte key from the WPMGR_AGENT_KEY_FILE constant path, reading
     * it if present or creating it 0600 if its directory is writable.
     *
     * @return string|null 32 raw bytes, or null if the constant is undefined or
     *                      the file is unusable.
     */
    private function keyFromConstant(): ?string
    {
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            return null;
        }
        $path = WPMGR_AGENT_KEY_FILE;
        if (!is_string($path) || trim($path) === '') {
            return null;
        }

        $existing = $this->readKeyFile($path);
        if ($existing !== null) {
            return $existing;
        }

        // Not yet created: try to create it (without web-root hardening — the
        // admin chose this path explicitly and is responsible for its location).
        return $this->createKeyFile($path);
    }

    /**
     * Deterministically derive a 32-byte master key from the wp-config secret
     * salts via HKDF-SHA256. Returns null if the salts are absent, placeholder,
     * or carry insufficient entropy/length.
     *
     * @return string|null 32 raw bytes, or null if salts are unusable.
     */
    private function keyFromSalts(): ?string
    {
        $ikm = '';
        foreach (self::SALT_CONSTANTS as $name) {
            if (!defined($name)) {
                continue;
            }
            $value = constant($name);
            if (!is_string($value) || $value === '') {
                continue;
            }
            if (in_array($value, self::SALT_PLACEHOLDERS, true)) {
                // A placeholder poisons the whole derivation: bail out.
                return null;
            }
            // Domain-separate each salt so reordering/concatenation is unambiguous.
            $ikm .= $name . '=' . $value . "\n";
        }

        if (strlen($ikm) < self::SALT_MIN_COMBINED_LENGTH) {
            return null;
        }

        // HKDF-SHA256 with a fixed info string yields a stable 32-byte key.
        $key = hash_hkdf('sha256', $ikm, self::KEY_BYTES, self::HKDF_INFO, '');
        sodium_memzero($ikm);

        return $key;
    }

    /**
     * Read a 32-byte master key from a file, or null if it is absent/invalid.
     *
     * @param string $path Absolute file path.
     * @return string|null 32 raw bytes, or null.
     */
    private function readKeyFile(string $path): ?string
    {
        if (!is_readable($path) || !is_file($path)) {
            return null;
        }
        $key = file_get_contents($path);
        if ($key === false || strlen($key) !== self::KEY_BYTES) {
            return null;
        }

        return $key;
    }

    /**
     * Generate and write a fresh 32-byte key to $path with 0600 perms.
     *
     * @param string $path Absolute file path (its directory must already exist).
     * @return string|null The written key, or null if the write failed.
     */
    private function createKeyFile(string $path): ?string
    {
        $dir = dirname($path);
        if (!is_dir($dir) || !is_writable($dir)) {
            return null;
        }

        $key = random_bytes(self::KEY_BYTES);
        if (file_put_contents($path, $key, LOCK_EX) === false) {
            sodium_memzero($key);
            return null;
        }
        @chmod($path, 0600);

        return $key;
    }

    /**
     * Try each candidate key-file location and write to the first whose
     * directory we can create/write. Web-root locations are hardened with
     * index.php + .htaccess so the key cannot be served.
     *
     * @return array{path:string,key:string}|null The chosen path + key, or null.
     */
    private function writeKeyFileToFirstWritable(): ?array
    {
        foreach ($this->candidateKeyDirs() as $candidate) {
            $dir       = $candidate['dir'];
            $inWebroot = $candidate['in_webroot'];

            if (!is_dir($dir) && !@mkdir($dir, 0700, true) && !is_dir($dir)) {
                continue;
            }
            if (!is_writable($dir)) {
                continue;
            }

            if ($inWebroot) {
                $this->hardenDirectory($dir);
            }

            $path = rtrim($dir, '/\\') . '/' . ($candidate['filename']);
            $key  = $this->createKeyFile($path);
            if ($key !== null) {
                return ['path' => $path, 'key' => $key];
            }
        }

        return null;
    }

    /**
     * Ordered candidate directories for the fallback key file.
     *
     * @return list<array{dir:string,in_webroot:bool,filename:string}>
     */
    private function candidateKeyDirs(): array
    {
        $candidates = [];

        // a. One level above ABSPATH (outside webroot on most installs).
        if (defined('ABSPATH') && is_string(ABSPATH) && ABSPATH !== '') {
            $candidates[] = [
                'dir'        => rtrim(dirname(rtrim((string) ABSPATH, '/\\')), '/\\'),
                'in_webroot' => false,
                'filename'   => '.wpmgr-agent-master.key',
            ];
        }

        // b. wp-content/wpmgr-agent (inside webroot -> hardened).
        if (defined('WP_CONTENT_DIR') && is_string(WP_CONTENT_DIR) && WP_CONTENT_DIR !== '') {
            $candidates[] = [
                'dir'        => rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-agent',
                'in_webroot' => true,
                'filename'   => 'master.key',
            ];
        }

        // c. uploads base dir /wpmgr-agent (inside webroot -> hardened).
        $uploadBase = $this->uploadsBaseDir();
        if ($uploadBase !== null) {
            $candidates[] = [
                'dir'        => rtrim($uploadBase, '/\\') . '/wpmgr-agent',
                'in_webroot' => true,
                'filename'   => 'master.key',
            ];
        }

        return $candidates;
    }

    /**
     * Resolve the WordPress uploads base directory, or null if unavailable.
     *
     * @return string|null
     */
    private function uploadsBaseDir(): ?string
    {
        if (!function_exists('wp_upload_dir')) {
            return null;
        }
        $info = wp_upload_dir();
        if (is_array($info) && isset($info['basedir']) && is_string($info['basedir']) && $info['basedir'] !== '') {
            return $info['basedir'];
        }

        return null;
    }

    /**
     * Drop index.php + .htaccess into a web-root directory so its contents
     * (the key file) cannot be served over HTTP.
     *
     * @param string $dir Absolute directory path.
     * @return void
     */
    private function hardenDirectory(string $dir): void
    {
        $dir   = rtrim($dir, '/\\');
        $index = $dir . '/index.php';
        if (!file_exists($index)) {
            @file_put_contents($index, "<?php\n// Silence is golden.\n", LOCK_EX);
        }

        $htaccess = $dir . '/.htaccess';
        if (!file_exists($htaccess)) {
            $rules = "# Apache 2.2\n<IfModule !mod_authz_core.c>\nDeny from all\n</IfModule>\n"
                . "# Apache 2.4\n<IfModule mod_authz_core.c>\nRequire all denied\n</IfModule>\n";
            @file_put_contents($htaccess, $rules, LOCK_EX);
        }
    }

    /**
     * Legacy key-file paths to check for backward compatibility before
     * generating a new key (so existing installs keep decrypting).
     *
     * @return list<string>
     */
    private function legacyKeyFilePaths(): array
    {
        $paths = [];

        // The pre-fix default: dirname(ABSPATH)/.wpmgr-agent-master.key.
        if (defined('ABSPATH') && is_string(ABSPATH) && ABSPATH !== '') {
            $paths[] = rtrim(dirname(rtrim((string) ABSPATH, '/\\')), '/\\') . '/.wpmgr-agent-master.key';
        }

        return $paths;
    }
}
