<?php
/**
 * KeystoreHealth: turns a Keystore::probe() result into the admin notice, the
 * metadata status and the backup refusal text.
 *
 * Every message here is plain text (no markup); callers escape at output.
 * Nothing here ever includes key material, a key-check value or a file path.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

use WPMgr\Agent\Backup\EncryptAndUpload;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Plugin;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Plain-words reporting for the encrypted keystore's health.
 */
final class KeystoreHealth
{
    /** Notice kind: stored keys exist that the current key cannot open. */
    public const KIND_UNREADABLE = 'unreadable';

    /** Notice kind: stored keys exist, but the master key cannot be loaded. */
    public const KIND_KEY_UNAVAILABLE = 'key_unavailable';

    /** Notice kind: first-time setup failed. */
    public const KIND_SETUP = 'setup';

    /** Appended to every notice that leaves the agent unable to work. */
    private const INACTIVE_SUFFIX = ' The plugin is active but inactive until this is resolved.';

    /** The eight wp-config.php security keys, in the order wp-config.php lists them. */
    private const SALT_NAMES = 'AUTH_KEY, SECURE_AUTH_KEY, LOGGED_IN_KEY, NONCE_KEY, AUTH_SALT, '
        . 'SECURE_AUTH_SALT, LOGGED_IN_SALT and NONCE_SALT';

    /**
     * The status block the metadata payload carries: overall state, the
     * pinned source NAME (never its path), and absent/ok/unreadable per item.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>}
     */
    public static function status(array $probe): array
    {
        return [
            'state'      => $probe['state'],
            'key_source' => $probe['key_source'],
            'items'      => $probe['items'],
            'unreadable' => $probe['unreadable'],
        ];
    }

    /**
     * Record the admin notice when the probe is not healthy. Never clears a
     * notice: clearing is left to Plugin's keystore setup, which runs on
     * activation and on every admin load while a notice is set.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return void
     */
    public static function flag(array $probe): void
    {
        if ($probe['state'] === Keystore::PROBE_OK || !function_exists('update_option')) {
            return;
        }

        if ($probe['state'] === Keystore::PROBE_UNREADABLE) {
            update_option(Plugin::OPTION_KEYSTORE_ERROR, self::unreadableNotice($probe), false);
            update_option(Plugin::OPTION_KEYSTORE_ERROR_KIND, self::KIND_UNREADABLE, false);
            return;
        }

        update_option(
            Plugin::OPTION_KEYSTORE_ERROR,
            self::setupFailureNotice(new \RuntimeException($probe['detail'])),
            false
        );
        update_option(Plugin::OPTION_KEYSTORE_ERROR_KIND, self::KIND_KEY_UNAVAILABLE, false);
    }

    /**
     * The bold headline shown in front of the stored notice.
     *
     * @param mixed $kind Stored notice kind; anything unrecognised reads as setup.
     * @return string
     */
    public static function headline($kind): string
    {
        if ($kind === self::KIND_UNREADABLE) {
            return 'WPMgr Agent cannot read its saved keys.';
        }
        if ($kind === self::KIND_KEY_UNAVAILABLE) {
            return 'WPMgr Agent cannot load its encryption key.';
        }

        return 'WPMgr Agent: setup incomplete.';
    }

    /**
     * Notice body for "the key loads, but stored items do not open under it".
     *
     * Names what cannot be read, gives the most common cause (not a
     * certainty: a damaged item looks the same), and says what to do. It
     * says backups already taken are unaffected only when backup chunks are
     * stored unencrypted, which is read from EncryptAndUpload, not assumed.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return string Plain text.
     */
    public static function unreadableNotice(array $probe): string
    {
        $unreadable = $probe['unreadable'];
        $backupKey  = in_array('age_identity', $unreadable, true);
        $connection = array_intersect(['site_keypair', 'cp_public_key'], $unreadable) !== [];
        $email      = array_intersect(['email_secret', 'email_connection_secrets'], $unreadable) !== [];

        $text = ucfirst(self::describe($backupKey, $connection, $email))
            . ' saved on this site cannot be opened with its current encryption key. '
            . self::likelyCause($probe['key_source']) . ' ' . self::remedy($probe['key_source']);

        if ($connection) {
            $text .= ' If you cannot, reconnect this site in WPMgr to replace its connection keys.';
        }
        if ($email) {
            $text .= ' Save the email settings again in WPMgr to replace the email credentials.';
        }
        if ($backupKey) {
            $text .= ' Until the backup key can be read, backups of this site will fail.';
            if (!self::chunksEncrypted()) {
                $text .= ' Backups already taken do not need this key and are not affected.';
            }
        }

        return $text;
    }

    /**
     * Detail for a backup the agent refused because its backup key cannot be
     * read. Reaches the dashboard as the failed snapshot's error.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return string Plain text.
     */
    public static function backupRefusal(array $probe): string
    {
        if ($probe['state'] === Keystore::PROBE_KEY_UNAVAILABLE) {
            return 'Backup not started: this site cannot load its encryption key ('
                . self::stripPrefix($probe['detail']) . ').';
        }

        if (($probe['items']['age_identity'] ?? '') === Keystore::ITEM_UNREADABLE) {
            $text = 'Backup not started: this site cannot read its backup key. '
                . self::likelyCause($probe['key_source']) . ' ' . self::remedy($probe['key_source']);
            if (!self::chunksEncrypted()) {
                $text .= ' Backups already taken are not affected.';
            }

            return $text;
        }

        return 'Backup not started: this site could not read its backup key.';
    }

    /**
     * Notice for a keystore setup that threw. Distinguishes three situations
     * rather than one fixed opaque message (GH #257):
     *
     *   (a) A required crypto PHP extension (sodium or openssl) is missing.
     *   (b) A previously-pinned master-key source has become unavailable; the
     *       Keystore's own message names which one.
     *   (c) First-time establishment failed: every source was unusable.
     *
     * @param \Throwable $e The exception caught while setting up the keystore.
     * @return string Plain text.
     */
    public static function setupFailureNotice(\Throwable $e): string
    {
        // (a) Missing crypto extension. Checked independently of $e's content
        // (a missing extension typically surfaces as an "undefined function"
        // \Error rather than a message that names the extension).
        $missingExtensions = [];
        if (!extension_loaded('sodium')) {
            $missingExtensions[] = 'sodium';
        }
        if (!extension_loaded('openssl')) {
            $missingExtensions[] = 'openssl';
        }
        if ($missingExtensions !== []) {
            $plural = count($missingExtensions) > 1;
            return 'WPMgr Agent requires the PHP ' . implode(' and ', $missingExtensions)
                . ' extension' . ($plural ? 's' : '') . ', which ' . ($plural ? 'are' : 'is')
                . ' not available on this server. Ask your host to enable '
                . ($plural ? 'them' : 'it') . '.' . self::INACTIVE_SUFFIX;
        }

        $message = $e->getMessage();

        // (b) Pinned-source drift: every pinned-source failure the Keystore
        // throws names the source and contains the word "pinned".
        if (strpos($message, 'pinned') !== false) {
            return 'WPMgr Agent could not re-establish its encryption key: ' . $message . self::INACTIVE_SUFFIX;
        }

        // (c) First-time establishment failure.
        return 'WPMgr Agent could not establish its encryption key. Define WPMGR_AGENT_KEY_FILE '
            . 'in wp-config.php pointing to a writable path, ensure your wp-config.php secret salts '
            . '(AUTH_KEY, ...) are set, or (if WPMGR_AGENT_DISABLE_DB_KEY is defined) remove that '
            . 'constant so the database-stored fallback key can be used.' . self::INACTIVE_SUFFIX;
    }

    /**
     * Name the unreadable things in plain words: "the backup key", "the
     * connection keys and the email credentials", ...
     *
     * @param bool $backupKey  The age identity is unreadable.
     * @param bool $connection A connection key is unreadable.
     * @param bool $email      An email credential is unreadable.
     * @return string
     */
    private static function describe(bool $backupKey, bool $connection, bool $email): string
    {
        $parts = [];
        if ($backupKey) {
            $parts[] = 'the backup key';
        }
        if ($connection) {
            $parts[] = 'the connection keys';
        }
        if ($email) {
            $parts[] = 'the email credentials';
        }
        if ($parts === []) {
            return 'the keys';
        }
        $last = array_pop($parts);

        return $parts === [] ? $last : implode(', ', $parts) . ' and ' . $last;
    }

    /**
     * The most common cause for the pinned source, stated as likely, not certain.
     *
     * @param string $keySource Pinned source name.
     * @return string
     */
    private static function likelyCause(string $keySource): string
    {
        if ($keySource === 'salts') {
            return 'Most often this happens when the site was moved to another host, '
                . 'or the security keys in wp-config.php were changed.';
        }
        if ($keySource === 'file' || $keySource === 'constant') {
            return 'Most often this happens when the site was moved to another host, '
                . 'or its encryption key file was replaced.';
        }

        return 'Most often this happens when the site was moved to another host, '
            . 'or its database was copied from another site.';
    }

    /**
     * What to do for the pinned source.
     *
     * @param string $keySource Pinned source name.
     * @return string
     */
    private static function remedy(string $keySource): string
    {
        if ($keySource === 'salts') {
            return 'To fix it, put back the previous values of the security keys in wp-config.php ('
                . self::SALT_NAMES . ').';
        }
        if ($keySource === 'file' || $keySource === 'constant') {
            return 'To fix it, put back the encryption key file this site used before.';
        }

        return 'To fix it, restore the database this site used before.';
    }

    /**
     * Drop the "WPMgr Agent: " prefix the Keystore puts on its messages.
     *
     * @param string $message Keystore exception message.
     * @return string
     */
    private static function stripPrefix(string $message): string
    {
        $prefix = 'WPMgr Agent: ';
        $text   = strpos($message, $prefix) === 0 ? substr($message, strlen($prefix)) : $message;

        return rtrim($text, '.');
    }

    /**
     * Whether backup chunks are stored encrypted to the backup key. When they
     * are not, a backup key that cannot be read does not affect backups
     * already taken.
     *
     * @return bool
     */
    private static function chunksEncrypted(): bool
    {
        return (bool) EncryptAndUpload::ENCRYPT_CHUNKS;
    }
}
