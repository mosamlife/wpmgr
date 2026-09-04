<?php
/**
 * Keystore: AES-256-GCM encrypted-at-rest storage of cryptographic material.
 *
 * Stores:
 *   - The control-plane Ed25519 PUBLIC key (used to verify inbound JWTs).
 *   - This site's own Ed25519 keypair (generated on activation; the public
 *     half is shared with the control plane, the secret half signs responses).
 *
 * The master key is acquired from a portable, deterministic source (in
 * priority order):
 *
 *   1. The WPMGR_AGENT_KEY_FILE constant (define it in wp-config.php).
 *   2. Derivation from the wp-config.php secret salts (AUTH_KEY, ...) via
 *      HKDF-SHA256. This is the preferred default: the salts live outside the
 *      database and are present on virtually every real install, so no file
 *      write is needed and the key is identical across requests.
 *   3. A key file created atomically (O_EXCL) at the first writable
 *      candidate location (uploads, then wp-content, then — for backward
 *      compatibility with pre-existing installs only — one level above
 *      ABSPATH), 0600-permissioned, with web-root locations hardened by
 *      index.php + .htaccess. A candidate whose file already exists adopts
 *      the existing key instead of overwriting it (self-heal), and a
 *      partial write (disk full/quota mid-write) is rolled back rather than
 *      pinned. There is deliberately no system-temp-directory candidate: on
 *      shared (non-CageFS) hosting /tmp is often readable/writable by other
 *      tenants, who could pre-plant a symlink at the predictable path and
 *      capture the key.
 *   4. Last resort: a random key stored in a dedicated wp_option, for hosts
 *      where neither the salts nor any candidate directory is usable (for
 *      example some CloudLinux/CageFS shared-hosting configurations that
 *      restrict writes above the account's own webroot while also shipping
 *      wp-config.php without real secret salts). Claimed atomically via
 *      add_option()'s UNIQUE(option_name) semantics, so two concurrent
 *      first-establishment requests converge on one winning key rather than
 *      each encrypting under a different one. This key lives in the
 *      database alongside the ciphertext it protects, so it is strictly
 *      weaker than tiers 1-3 — defining WPMGR_AGENT_KEY_FILE or setting real
 *      wp-config.php salts is the recommended defense-in-depth upgrade. It
 *      can be disabled outright with the WPMGR_AGENT_DISABLE_DB_KEY constant.
 *
 * To stay deterministic (the keystore must decrypt what it earlier encrypted),
 * the chosen source is pinned in a wp-option marker the first time a key is
 * established, so later requests never silently switch sources.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

use WPMgr\Agent\Email\EmailKeystoreInterface;
use WPMgr\Agent\Support\SecureMemory;

/**
 * Encrypted keystore backed by wp-options + a file-based master key.
 */
final class Keystore implements EmailKeystoreInterface
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
     * Option name holding the per-site email provider secret (SMTP password,
     * API key, or AWS Secret Access Key), AES-256-GCM-encrypted at rest.
     * The plaintext secret travels only in the signed JWT-protected
     * sync_email_config body and is immediately encrypted upon receipt.
     */
    public const OPTION_EMAIL_SECRET = 'wpmgr_agent_email_secret';

    /**
     * Option name holding the per-connection secrets map, AES-256-GCM-encrypted.
     * Stores a JSON object {connection_key: plaintext_secret} for all named
     * connections; replaced atomically on every sync_email_config push.
     */
    public const OPTION_EMAIL_CONN_SECRETS = 'wpmgr_agent_email_conn_secrets';

    /**
     * Option pinning which master-key source this install uses, so decrypt
     * always re-derives/reads the exact same key. Value is one of:
     *   ['source' => 'constant']
     *   ['source' => 'salts']
     *   ['source' => 'file', 'path' => '/abs/path']
     *   ['source' => 'db']
     */
    public const OPTION_MASTER_KEY_SOURCE = 'wpmgr_agent_master_key_source';

    /**
     * Option holding the last-resort master key itself (base64-encoded raw
     * bytes), used only when no portable file/salt source is available. Kept
     * non-autoloaded (see keyFromDatabase()) so it never inflates alloptions.
     */
    public const OPTION_DB_MASTER_KEY = 'wpmgr_agent_db_master_key';

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
     * Minimum combined length (bytes) the concatenated salts (including the
     * domain-separation labels) must reach before we trust them as keying
     * material. Eight default WP salts are 64+ chars each; we require well
     * above any single-salt fluke.
     */
    private const SALT_MIN_COMBINED_LENGTH = 96;

    /**
     * Minimum combined length (bytes) of the raw salt VALUES ALONE (excluding
     * the "NAME=" labels and separators) that we trust as keying material.
     * A pure superset of SALT_MIN_COMBINED_LENGTH: it accepts installs whose
     * wp-config.php carries only one or two real salts (a common outcome of a
     * migrated or minimally-generated wp-config.php) rather than requiring
     * the full default set of eight.
     */
    private const SALT_MIN_VALUE_BYTES = 32;

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
        SecureMemory::wipe($keypair);

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
     * Persist the per-site email provider secret (SMTP password / API key /
     * AWS secret access key), AES-256-GCM-encrypted under the master key.
     * Passing an empty string removes any stored secret.
     *
     * @param string $secret Raw plaintext secret.
     * @return void
     */
    public function storeEmailSecret(string $secret): void
    {
        if ($secret === '') {
            delete_option(self::OPTION_EMAIL_SECRET);
            return;
        }
        update_option(self::OPTION_EMAIL_SECRET, $this->encrypt($secret), false);
    }

    /**
     * Retrieve and decrypt the per-site email provider secret.
     * Returns an empty string when no secret has been stored.
     *
     * @return string Decrypted secret, or '' when absent.
     */
    public function get_email_secret(): string
    {
        $stored = get_option(self::OPTION_EMAIL_SECRET);
        if (!is_string($stored) || $stored === '') {
            return '';
        }
        try {
            return $this->decrypt($stored);
        } catch (\Throwable $e) {
            return '';
        }
    }

    /**
     * Persist the per-connection secrets map, AES-256-GCM-encrypted.
     *
     * The map is keyed by connection_key (slug) with plaintext-secret values.
     * Stored as an AES-encrypted JSON blob under OPTION_EMAIL_CONN_SECRETS;
     * replaced atomically on every sync. Passing an empty array removes the option.
     *
     * @param array<string,string> $secrets Map of connection_key => plaintext secret.
     * @return void
     */
    public function store_connection_secrets( array $secrets ): void
    {
        if ( $secrets === [] ) {
            delete_option( self::OPTION_EMAIL_CONN_SECRETS );
            return;
        }
        $json      = (string) wp_json_encode( $secrets );
        $encrypted = $this->encrypt( $json );
        update_option( self::OPTION_EMAIL_CONN_SECRETS, $encrypted, false );
    }

    /**
     * Retrieve the decrypted plaintext secret for a named connection.
     * Returns an empty string when no secret is stored for that key.
     *
     * @param string $connection_key Operator-chosen connection slug.
     * @return string Decrypted secret, or '' when absent.
     */
    public function get_connection_secret( string $connection_key ): string
    {
        if ( $connection_key === '' ) {
            return '';
        }
        $stored = get_option( self::OPTION_EMAIL_CONN_SECRETS );
        if ( ! is_string( $stored ) || $stored === '' ) {
            return '';
        }
        try {
            $json = $this->decrypt( $stored );
        } catch ( \Throwable $e ) {
            return '';
        }
        $map = json_decode( $json, true );
        if ( ! is_array( $map ) ) {
            return '';
        }
        return isset( $map[ $connection_key ] ) && is_string( $map[ $connection_key ] )
            ? $map[ $connection_key ]
            : '';
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
     * Resolution honours the pinned source marker (if any) so decrypt always
     * re-derives/reads the identical key, then falls back to source discovery
     * on first use:
     *
     *   1. WPMGR_AGENT_KEY_FILE constant.
     *   2. Derivation from wp-config secret salts (HKDF-SHA256). Preferred.
     *   3. A 0600 key file at the first writable candidate location.
     *   4. A random key stored in wp_options (last resort; see
     *      keyFromDatabase()). The key itself lives in the database for this
     *      tier only — tiers 1-3 never store the key material there.
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
     * @throws \RuntimeException If the key cannot be established, or if a
     *                            previously-pinned source has become
     *                            unavailable.
     */
    private function resolveMasterKey(): string
    {
        $pinned = $this->pinnedSource();

        // Honour an already-pinned source so we never silently switch keys.
        // Every case below either returns the ORIGINAL key or throws; none is
        // allowed to fall through to fresh discovery, which would derive a
        // different key and permanently orphan everything already encrypted
        // under this install's real key (site keypair, age backup identity,
        // stored control-plane public key, ...).
        if ($pinned !== null) {
            switch ($pinned['source']) {
                case 'constant':
                    $key = $this->keyFromConstant();
                    if ($key !== null) {
                        return $key;
                    }
                    throw new \RuntimeException(
                        'WPMgr Agent: pinned WPMGR_AGENT_KEY_FILE master key is no longer available '
                        . '(the constant was removed or its file is missing).'
                    );
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
                case 'db':
                    $key = $this->readDatabaseKey();
                    if ($key !== null) {
                        return $key;
                    }
                    throw new \RuntimeException(
                        'WPMgr Agent: pinned database-stored master key is missing or corrupt.'
                    );
                default:
                    // An unrecognised pinned source (corrupt marker, hand
                    // edit, or a foreign/future value) fails closed like
                    // every recognised case above: silently re-discovering
                    // could mint or derive a DIFFERENT key and re-key an
                    // install that already has ciphertext under the
                    // original one.
                    throw new \RuntimeException(
                        'WPMgr Agent: master-key source marker is corrupt or unrecognised.'
                    );
            }
        }

        // First run (or unpinned legacy install): discover a source in order.

        // 1. Explicit constant.
        $key = $this->keyFromConstant();
        if ($key !== null) {
            $this->pinSource(['source' => 'constant']);
            return $key;
        }

        // 1b. Re-adopt any key file this install already has (current write
        // candidates AND the pre-#257 legacy default) with priority over
        // deriving/writing a new one. This matters even outside a true first
        // run: if OPTION_MASTER_KEY_SOURCE is ever lost/corrupted (and this
        // is not caught by the pinned-source honour block above, which only
        // runs when a marker exists), re-discovery must not derive a
        // DIFFERENT key from newly-available salts, or mint a brand-new file
        // key, while an existing valid key file is sitting right there —
        // either would silently orphan everything already encrypted under it.
        foreach ($this->existingKeyFilePaths() as $existing) {
            $key = $this->readKeyFile($existing);
            if ($key !== null) {
                $this->pinSource(['source' => 'file', 'path' => $existing]);
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

        // 4. Last resort: a random key stored in wp_options. Opt out with
        // WPMGR_AGENT_DISABLE_DB_KEY if your threat model requires the key to
        // never live in the database (you must then guarantee tier 1-3
        // availability yourself).
        $key = $this->keyFromDatabase();
        if ($key !== null) {
            $this->pinSource(['source' => 'db']);
            return $key;
        }

        throw new \RuntimeException(
            'WPMgr Agent: unable to establish a master key. Define WPMGR_AGENT_KEY_FILE '
            . 'to a writable path, ensure wp-config.php secret salts are set, or (if '
            . 'WPMGR_AGENT_DISABLE_DB_KEY is defined) remove that constant so the '
            . 'database-stored fallback key can be used.'
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
     * salts via HKDF-SHA256. Returns null if no usable salts are present.
     *
     * A single placeholder or missing salt does NOT poison the whole
     * derivation: that individual salt is skipped and the remaining real
     * salts are used. A migrated, cloned, or minimally-generated wp-config.php
     * commonly carries only some of the eight default salts (WordPress treats
     * them as optional and self-heals missing ones into wp_options via
     * wp_salt() — but this method only reads the constants, by design, so it
     * stays independent of database state).
     *
     * @return string|null 32 raw bytes, or null if no salts are usable.
     */
    private function keyFromSalts(): ?string
    {
        $ikm        = '';
        $valueBytes = 0;
        foreach (self::SALT_CONSTANTS as $name) {
            if (!defined($name)) {
                continue;
            }
            $value = constant($name);
            if (!is_string($value) || $value === '') {
                continue;
            }
            if (in_array($value, self::SALT_PLACEHOLDERS, true)) {
                // A placeholder salt carries no entropy: skip only this one
                // and keep deriving from whatever real salts remain.
                continue;
            }
            // Domain-separate each salt so reordering/concatenation is unambiguous.
            $ikm        .= $name . '=' . $value . "\n";
            $valueBytes += strlen($value);
        }

        // Accept if EITHER the combined material (incl. labels) reaches the
        // original threshold OR the raw salt values alone reach the looser
        // threshold. This is a pure superset of the original single gate:
        // every input that used to pass still passes identically.
        if (strlen($ikm) < self::SALT_MIN_COMBINED_LENGTH && $valueBytes < self::SALT_MIN_VALUE_BYTES) {
            return null;
        }

        // HKDF-SHA256 with a fixed info string yields a stable 32-byte key.
        $key = hash_hkdf('sha256', $ikm, self::KEY_BYTES, self::HKDF_INFO, '');
        SecureMemory::wipe($ikm);

        return $key;
    }

    /**
     * Last-resort master key: a random 32-byte key stored (base64-encoded)
     * in a dedicated, non-autoloaded wp_option. Used only when the constant,
     * salts, and every candidate directory in writeKeyFileToFirstWritable()
     * are all unusable — the situation on some CloudLinux/CageFS shared-
     * hosting configurations, where writes above the account webroot are
     * blocked by open_basedir and wp-config.php ships without real secret
     * salts.
     *
     * This key lives in the database alongside the ciphertext it protects,
     * so it is intentionally NOT derived from wp_salt(): salts are the thing
     * an operator is advised to rotate after a compromise, and a salt-linked
     * key would orphan the age backup identity (the only key that can
     * decrypt prior backups) on every rotation. A dedicated key that never
     * rotates on its own is strictly more stable here. Defining
     * WPMGR_AGENT_KEY_FILE or setting real wp-config.php salts remains the
     * recommended defense-in-depth upgrade over this tier.
     *
     * First establishment is claimed ATOMICALLY via add_option(), which
     * INSERTs and returns false if the option row already exists (the
     * wp_options table enforces UNIQUE(option_name)). Two requests racing to
     * establish the key for the first time (for example an admin_init retry
     * overlapping an in-flight Enroll POST) therefore cannot both "win": the
     * loser's freshly generated bytes are discarded and it adopts whatever
     * the winner actually persisted, so every request ends up encrypting
     * under the SAME key.
     *
     * @return string|null 32 raw bytes, or null when disabled via
     *                      WPMGR_AGENT_DISABLE_DB_KEY.
     */
    private function keyFromDatabase(): ?string
    {
        if (defined('WPMGR_AGENT_DISABLE_DB_KEY') && WPMGR_AGENT_DISABLE_DB_KEY === true) {
            return null;
        }

        $existing = $this->readDatabaseKey();
        if ($existing !== null) {
            return $existing;
        }

        $key = random_bytes(self::KEY_BYTES);
        if (add_option(self::OPTION_DB_MASTER_KEY, base64_encode($key), '', false)) {
            return $key;
        }

        // Lost the race: another request's add_option() already won.
        // Discard this key and adopt whichever key actually got persisted.
        SecureMemory::wipe($key);

        return $this->readDatabaseKey();
    }

    /**
     * Read the stored database-fallback master key, if present and valid.
     * Deliberately ignores WPMGR_AGENT_DISABLE_DB_KEY: once a key has been
     * pinned to this source it must always be re-readable, regardless of
     * whether the opt-out constant is defined later — disabling the tier only
     * blocks NEW installs from adopting it.
     *
     * @return string|null 32 raw bytes, or null if absent/corrupt.
     */
    private function readDatabaseKey(): ?string
    {
        $stored = get_option(self::OPTION_DB_MASTER_KEY);
        if (!is_string($stored) || $stored === '') {
            return null;
        }

        $raw = base64_decode($stored, true);
        if ($raw === false || strlen($raw) !== self::KEY_BYTES) {
            // Corrupt/foreign value: treat as absent rather than using it.
            return null;
        }

        return $raw;
    }

    /**
     * Read a 32-byte master key from a file, or null if it is absent/invalid.
     *
     * @param string $path Absolute file path.
     * @return string|null 32 raw bytes, or null.
     */
    private function readKeyFile(string $path): ?string
    {
        // Probes are @-suppressed: on open_basedir/CageFS hosts, checking a
        // candidate outside the allowed path list emits a PHP warning that
        // would otherwise corrupt any output generated during this request
        // (e.g. the enrollment JSON response).
        if (!@is_readable($path) || !@is_file($path)) {
            return null;
        }
        $key = @file_get_contents($path);
        if ($key === false || strlen($key) !== self::KEY_BYTES) {
            return null;
        }

        return $key;
    }

    /**
     * Atomically create a fresh 32-byte key file at $path with 0600 perms, or
     * adopt whatever key is already there.
     *
     * Uses 'xb' (O_CREAT|O_EXCL) so the create itself fails if the file
     * already exists, instead of the previous blind file_put_contents()
     * overwrite — two requests racing to establish a NEW key here can no
     * longer clobber each other, and a lost/corrupted OPTION_MASTER_KEY_SOURCE
     * pin re-adopts the pre-existing key on this path (self-heal, idempotent)
     * rather than minting a different one and orphaning everything already
     * encrypted under the original. If the existing file turns out to be
     * missing/invalid/short, this candidate is simply unusable — the caller
     * moves on to the next one.
     *
     * A short write (the byte count fwrite() actually persisted is less than
     * KEY_BYTES — for example a disk-full or quota condition mid-write) is
     * never pinned: the partial file is deleted and this candidate fails,
     * rather than silently persisting a key that would read back short (and
     * therefore be rejected as invalid) on every later request.
     *
     * @param string $path Absolute file path (its directory must already exist).
     * @return string|null The (possibly pre-existing, adopted) 32-byte key, or
     *                      null if the candidate is unusable.
     */
    private function createKeyFile(string $path): ?string
    {
        $dir = dirname($path);
        // Probes are @-suppressed: candidates outside open_basedir/CageFS's
        // allowed path list otherwise emit a PHP warning that can corrupt
        // any output generated during this request.
        if (!@is_dir($dir) || !@is_writable($dir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            return null;
        }

        $handle = @fopen($path, 'xb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- headless agent; WP_Filesystem never initialized; O_EXCL atomic create has no WP_Filesystem equivalent
        if ($handle === false) {
            // Someone else already created this file — a concurrent writer,
            // or a pre-existing key from an earlier request. Never overwrite
            // it: adopt it if valid, otherwise this candidate is unusable.
            return $this->readKeyFile($path);
        }

        $key     = random_bytes(self::KEY_BYTES);
        $written = fwrite($handle, $key); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- headless agent; WP_Filesystem never initialized; pairs with the fopen() above
        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- headless agent; WP_Filesystem never initialized; pairs with the fopen() above

        if ($written !== self::KEY_BYTES) {
            // Partial write: never pin a key that was not fully persisted.
            SecureMemory::wipe($key);
            wp_delete_file($path);
            return null;
        }
        @chmod($path, 0600); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0600); WP_Filesystem would coerce to wider FS_CHMOD_FILE

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

            // Probes are @-suppressed: on open_basedir/CageFS hosts, checking
            // a candidate outside the allowed path list emits a PHP warning
            // that would otherwise corrupt any output generated during this
            // request (e.g. the enrollment JSON response).
            if (!@is_dir($dir) && !@mkdir($dir, 0700, true) && !@is_dir($dir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on secret/scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
                continue;
            }
            if (!@is_writable($dir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
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
     * Ordered candidate directories for the fallback key file. Uploads-first:
     * on CloudLinux/CageFS and similar managed hosts, the uploads directory is
     * near-universally writable, while a location one level above ABSPATH
     * (the old default) commonly sits outside the account's open_basedir and
     * fails silently. That legacy location is kept LAST, purely so an install
     * that already has a key there is still discovered by
     * existingKeyFilePaths() before any candidate here is tried — see
     * resolveMasterKey() step 1b.
     *
     * Deliberately does NOT include sys_get_temp_dir(): on shared (non-CageFS)
     * hosting, /tmp is frequently readable/writable by every tenant on the
     * box, and the candidate path would be a function of ABSPATH alone — a
     * co-tenant could predict it, pre-plant a symlink, and capture the master
     * key the moment this install writes to it, or simply own the directory
     * first. The database tier (see keyFromDatabase()) already covers hosts
     * where uploads and wp-content are both unwritable, without stepping
     * outside the account's own boundary.
     *
     * @return list<array{dir:string,in_webroot:bool,filename:string}>
     */
    private function candidateKeyDirs(): array
    {
        $candidates = [];

        // a. uploads base dir /wpmgr-agent (inside webroot -> hardened).
        $uploadBase = $this->uploadsBaseDir();
        if ($uploadBase !== null) {
            $candidates[] = [
                'dir'        => rtrim($uploadBase, '/\\') . '/wpmgr-agent',
                'in_webroot' => true,
                'filename'   => 'master.key',
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

        // c. Backward-compat only: one level above ABSPATH. Kept as a WRITE
        // candidate (last resort before the database tier) as well as a READ
        // candidate (existingKeyFilePaths()) so a pre-existing install here
        // keeps working, but new installs no longer prefer it — it is
        // frequently outside open_basedir on managed hosts.
        if (defined('ABSPATH') && is_string(ABSPATH) && ABSPATH !== '') {
            $candidates[] = [
                'dir'        => rtrim(dirname(rtrim((string) ABSPATH, '/\\')), '/\\'),
                'in_webroot' => false,
                'filename'   => '.wpmgr-agent-master.key',
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
        if (!@file_exists($index)) {
            @file_put_contents($index, "<?php\n// Silence is golden.\n", LOCK_EX);
        }

        $htaccess = $dir . '/.htaccess';
        if (!@file_exists($htaccess)) {
            $rules = "# Apache 2.2\n<IfModule !mod_authz_core.c>\nDeny from all\n</IfModule>\n"
                . "# Apache 2.4\n<IfModule mod_authz_core.c>\nRequire all denied\n</IfModule>\n";
            @file_put_contents($htaccess, $rules, LOCK_EX);
        }
    }

    /**
     * Every key-file path this install might already have a key at, in the
     * same priority order as candidateKeyDirs() writes to them, checked
     * BEFORE deriving/writing a new key (resolveMasterKey() step 1b). Covers
     * both the current write candidates (uploads, wp-content) and the
     * pre-#257 legacy default (one level above ABSPATH) — the legacy path is
     * never dropped from this READ set, so an older install that already has
     * a key there keeps working even though new installs no longer WRITE
     * there first.
     *
     * @return list<string>
     */
    private function existingKeyFilePaths(): array
    {
        $paths = [];

        $uploadBase = $this->uploadsBaseDir();
        if ($uploadBase !== null) {
            $paths[] = rtrim($uploadBase, '/\\') . '/wpmgr-agent/master.key';
        }

        if (defined('WP_CONTENT_DIR') && is_string(WP_CONTENT_DIR) && WP_CONTENT_DIR !== '') {
            $paths[] = rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-agent/master.key';
        }

        // The pre-#257 default: dirname(ABSPATH)/.wpmgr-agent-master.key.
        if (defined('ABSPATH') && is_string(ABSPATH) && ABSPATH !== '') {
            $paths[] = rtrim(dirname(rtrim((string) ABSPATH, '/\\')), '/\\') . '/.wpmgr-agent-master.key';
        }

        return $paths;
    }
}
