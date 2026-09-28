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

    /** Replacement step for unreadable email credentials. */
    private const EMAIL_REMEDY = ' Save the email settings again in WPMgr to replace the email credentials.';

    /** What an unreadable backup key means once putting back an earlier key is ruled out. */
    private const BACKUP_KEY_RESET = ' Backups of this site cannot run until the backup key is reset.'
        . ' A way to reset it from WPMgr is coming in an update.';

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

        update_option(Plugin::OPTION_KEYSTORE_ERROR, self::keyUnavailableNotice($probe), false);
        update_option(Plugin::OPTION_KEYSTORE_ERROR_KIND, self::KIND_KEY_UNAVAILABLE, false);
    }

    /**
     * Whether the backup key may be read, or created when none is stored,
     * under the key this site loads now.
     *
     * True when the stored backup key opens. Also true when no backup key is
     * stored and the current key is shown to be the live one: every stored
     * item opens, or the site keypair opens under it. Never true for a backup
     * key that is stored but does not open, and never true while the key
     * cannot be loaded.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return bool
     */
    public static function backupKeyUsable(array $probe): bool
    {
        $backupKey = $probe['items']['age_identity'] ?? '';
        if ($backupKey === Keystore::ITEM_OK) {
            return true;
        }
        if ($backupKey !== Keystore::ITEM_ABSENT) {
            return false;
        }
        if ($probe['state'] === Keystore::PROBE_OK) {
            return true;
        }

        return $probe['state'] === Keystore::PROBE_UNREADABLE
            && ($probe['items']['site_keypair'] ?? '') === Keystore::ITEM_OK;
    }

    /**
     * Whether a failure to read the backup key is explained by the keystore:
     * the key cannot be loaded, or the stored backup key does not open.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return bool
     */
    public static function backupKeyUnreadable(array $probe): bool
    {
        return $probe['state'] === Keystore::PROBE_KEY_UNAVAILABLE
            || ($probe['items']['age_identity'] ?? '') === Keystore::ITEM_UNREADABLE;
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
     * The advice depends on what still opens:
     *   - Something stored opens under the current key: the current key is
     *     the live one, so putting back an earlier key would break what opens
     *     now. Each unreadable item gets its own replacement step instead.
     *   - Nothing stored opens: putting back the earlier key is the fix.
     *   - No key source is recorded: there is no earlier key to name, so each
     *     unreadable item gets its own replacement step.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return string Plain text.
     */
    public static function unreadableNotice(array $probe): string
    {
        $unreadable = $probe['unreadable'];
        $source     = $probe['key_source'];
        $backupKey  = in_array('age_identity', $unreadable, true);
        $connection = array_intersect(['site_keypair', 'cp_public_key'], $unreadable) !== [];
        $email      = array_intersect(['email_secret', 'email_connection_secrets'], $unreadable) !== [];
        $what       = self::describe($backupKey, $connection, $email);

        $text = ucfirst($what) . ' saved on this site cannot be opened with its current encryption key.';

        if (self::somethingOpens($probe) || $source === '') {
            $text .= ' ' . self::replaceCause($probe, $what, $backupKey && !$connection && !$email);
            if ($connection) {
                $text .= ' Reconnect this site in WPMgr to replace its connection keys.';
            }
            if ($email) {
                $text .= self::EMAIL_REMEDY;
            }
            if ($backupKey) {
                $text .= self::BACKUP_KEY_RESET;
                if (!self::chunksEncrypted()) {
                    $text .= ' Backups already taken do not need this key and are not affected.';
                }
            }

            return $text;
        }

        $text .= ' ' . self::likelyCause($source) . ' ' . self::remedy($source);
        if ($connection) {
            $text .= ' If you cannot, reconnect this site in WPMgr to replace its connection keys.';
        }
        if ($email) {
            $text .= self::EMAIL_REMEDY;
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
     * Notice body for "items are stored, but the key cannot be loaded": what
     * is wrong with the pinned source and how to put it back.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return string Plain text.
     */
    public static function keyUnavailableNotice(array $probe): string
    {
        $missing = self::missingExtensionsNotice();
        if ($missing !== '') {
            return $missing;
        }

        return self::keyUnavailableCause($probe['key_source'])
            . ', so none of the keys saved on this site can be opened. '
            . self::keyUnavailableRemedy($probe['key_source']) . self::INACTIVE_SUFFIX;
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
            return 'Backup not started: this site cannot load its encryption key. '
                . self::keyUnavailableCause($probe['key_source']) . '. '
                . self::keyUnavailableRemedy($probe['key_source']);
        }

        if (($probe['items']['age_identity'] ?? '') === Keystore::ITEM_UNREADABLE) {
            $text = 'Backup not started: this site cannot read its backup key.';
            if (self::somethingOpens($probe) || $probe['key_source'] === '') {
                $text .= ' ' . self::replaceCause($probe, 'the backup key', true) . self::BACKUP_KEY_RESET;
            } else {
                $text .= ' ' . self::likelyCause($probe['key_source']) . ' ' . self::remedy($probe['key_source']);
            }
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
        $missing = self::missingExtensionsNotice();
        if ($missing !== '') {
            return $missing;
        }

        $message = $e->getMessage();

        // (b) Pinned-source drift: every pinned-source failure the Keystore
        // throws names the source and contains the word "pinned".
        if (strpos($message, 'pinned') !== false) {
            return 'WPMgr Agent could not re-establish its encryption key: ' . self::stripPrefix($message) . '.'
                . self::INACTIVE_SUFFIX;
        }

        // (c) First-time establishment failure.
        return 'WPMgr Agent could not establish its encryption key. Define WPMGR_AGENT_KEY_FILE '
            . 'in wp-config.php pointing to a writable path, ensure your wp-config.php secret salts '
            . '(AUTH_KEY, ...) are set, or (if WPMGR_AGENT_DISABLE_DB_KEY is defined) remove that '
            . 'constant so the database-stored fallback key can be used.' . self::INACTIVE_SUFFIX;
    }

    /**
     * The missing-crypto-extension notice, or '' when sodium and openssl are
     * both loaded.
     *
     * @return string Plain text.
     */
    private static function missingExtensionsNotice(): string
    {
        $missingExtensions = [];
        if (!extension_loaded('sodium')) {
            $missingExtensions[] = 'sodium';
        }
        if (!extension_loaded('openssl')) {
            $missingExtensions[] = 'openssl';
        }
        if ($missingExtensions === []) {
            return '';
        }
        $plural = count($missingExtensions) > 1;

        return 'WPMgr Agent requires the PHP ' . implode(' and ', $missingExtensions)
            . ' extension' . ($plural ? 's' : '') . ', which ' . ($plural ? 'are' : 'is')
            . ' not available on this server. Ask your host to enable '
            . ($plural ? 'them' : 'it') . '.' . self::INACTIVE_SUFFIX;
    }

    /**
     * Whether at least one stored item opens under the current key, which
     * shows the current key is the one this site is using now.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe Keystore::probe() result.
     * @return bool
     */
    private static function somethingOpens(array $probe): bool
    {
        return in_array(Keystore::ITEM_OK, $probe['items'], true);
    }

    /**
     * Cause sentence when the unreadable items have to be replaced rather
     * than recovered by putting back an earlier key.
     *
     * @param array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string} $probe    Keystore::probe() result.
     * @param string                                                                                                    $what     The unreadable items in plain words.
     * @param bool                                                                                                      $singular Whether $what is a single item.
     * @return string
     */
    private static function replaceCause(array $probe, string $what, bool $singular): string
    {
        $source = $probe['key_source'];
        if (!self::somethingOpens($probe)) {
            return 'No earlier encryption key is recorded for this site, so ' . $what . ' cannot be recovered and '
                . ($singular ? 'has' : 'have') . ' to be replaced. Most often this happens when the plugin was '
                . 'deleted and installed again, or the site was moved to another host.';
        }

        if ($source === 'salts') {
            $previous = "the site's previous security keys in wp-config.php, before the site was moved to "
                . 'another host or those keys were changed';
            $keep     = ' Do not put back the previous security keys: what opens now would stop opening.';
        } elseif ($source === 'file' || $source === 'constant') {
            $previous = "the site's previous encryption key file, before the site was moved to another host "
                . 'or that file was replaced';
            $keep     = ' Do not put back the previous key file: what opens now would stop opening.';
        } else {
            $previous = "the site's previous encryption key, before the site was moved to another host";
            $keep     = '';
        }

        return 'Other keys saved on this site do open with the current key, so ' . $what . ' '
            . ($singular ? 'was' : 'were') . ' most likely saved under ' . $previous . '.' . $keep;
    }

    /**
     * What is wrong with the pinned source when the key cannot be loaded, as
     * a sentence without its final full stop.
     *
     * @param string $keySource Pinned source name.
     * @return string
     */
    private static function keyUnavailableCause(string $keySource): string
    {
        switch ($keySource) {
            case 'salts':
                return 'The security keys in wp-config.php that the key is made from are missing or no longer usable';
            case 'constant':
                return 'The key file named by WPMGR_AGENT_KEY_FILE in wp-config.php is missing, or that constant was removed';
            case 'file':
                return 'The file the key is kept in is missing or damaged';
            case 'db':
                return 'The key stored in the database is missing or damaged';
            case 'unknown':
                return 'The record of where the key is kept is damaged';
            default:
                return 'The key could not be loaded';
        }
    }

    /**
     * How to put the pinned source back when the key cannot be loaded.
     *
     * @param string $keySource Pinned source name.
     * @return string
     */
    private static function keyUnavailableRemedy(string $keySource): string
    {
        switch ($keySource) {
            case 'salts':
                return 'To fix it, put back the previous values of the security keys in wp-config.php: '
                    . self::SALT_NAMES . '.';
            case 'constant':
                return 'To fix it, put back that constant and the key file it names.';
            case 'file':
                return 'To fix it, put back the key file this site used before.';
            case 'db':
            case 'unknown':
                return 'To fix it, restore the database this site used before.';
            default:
                return 'To fix it, try again later, and if it keeps failing ask your host to check the PHP error log.';
        }
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
            return 'To fix it, put back the previous values of the security keys in wp-config.php: '
                . self::SALT_NAMES . '.';
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
