<?php
/**
 * Tests for the plain-words keystore notice and backup refusal copy. Each
 * case feeds a Keystore::probe()-shaped result, so the salts-pinned states a
 * host move leaves behind are covered without defining the process-wide salt
 * constants.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Backup\EncryptAndUpload;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Support\KeystoreHealth;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\KeystoreHealth
 */
final class KeystoreHealthTest extends TestCase
{
    /**
     * A probe result with the given per-item states. Items not named are
     * absent; the state follows from the items unless given.
     *
     * @param array<string,string> $items     Item name => ok|unreadable.
     * @param string               $keySource Pinned source name.
     * @param string               $state     Overrides the derived state.
     * @param string               $detail    Probe detail.
     * @return array{state:string,key_source:string,items:array<string,string>,unreadable:list<string>,detail:string}
     */
    private function probe(array $items, string $keySource, string $state = '', string $detail = ''): array
    {
        $all = [
            'site_keypair'             => Keystore::ITEM_ABSENT,
            'cp_public_key'            => Keystore::ITEM_ABSENT,
            'age_identity'             => Keystore::ITEM_ABSENT,
            'email_secret'             => Keystore::ITEM_ABSENT,
            'email_connection_secrets' => Keystore::ITEM_ABSENT,
        ];
        $unreadable = [];
        foreach ($all as $name => $absent) {
            if (isset($items[$name])) {
                $all[$name] = $items[$name];
                if ($items[$name] === Keystore::ITEM_UNREADABLE) {
                    $unreadable[] = $name;
                }
            }
        }
        if ($state === '') {
            $state = $unreadable === [] ? Keystore::PROBE_OK : Keystore::PROBE_UNREADABLE;
        }

        return [
            'state'      => $state,
            'key_source' => $keySource,
            'items'      => $all,
            'unreadable' => $unreadable,
            'detail'     => $detail,
        ];
    }

    /**
     * GH #753, the reporter's state: a salts-pinned site moved host and was
     * reconnected, so the connection keys open under the new security keys
     * and the backup key does not. Putting back the previous security keys
     * would break the connection keys, so the notice must not advise it.
     */
    public function test_split_state_under_salts_never_advises_putting_back_the_security_keys(): void
    {
        $probe = $this->probe([
            'site_keypair'  => Keystore::ITEM_OK,
            'cp_public_key' => Keystore::ITEM_OK,
            'age_identity'  => Keystore::ITEM_UNREADABLE,
        ], 'salts');

        $notice  = KeystoreHealth::unreadableNotice($probe);
        $refusal = KeystoreHealth::backupRefusal($probe);

        $this->assertStringStartsWith('The backup key saved on this site cannot be opened with its current encryption key.', $notice);
        $this->assertStringContainsString(
            "the backup key was most likely saved under the site's previous security keys in wp-config.php",
            $notice
        );
        $this->assertStringContainsString('Do not put back the previous security keys', $notice);
        $this->assertStringContainsString('Backups of this site cannot run until the backup key is reset.', $notice);
        $this->assertStringContainsString('A way to reset it from WPMgr is coming in an update.', $notice);
        $this->assertStringStartsWith('Backup not started: this site cannot read its backup key.', $refusal);
        $this->assertStringContainsString('Backups of this site cannot run until the backup key is reset.', $refusal);
        foreach ([$notice, $refusal] as $text) {
            $this->assertStringNotContainsString('To fix it, put back', $text);
            $this->assertStringNotContainsString('AUTH_KEY', $text);
            $this->assertStringNotContainsString('WPMGR_AGENT_KEY_FILE', $text);
            if (!EncryptAndUpload::ENCRYPT_CHUNKS) {
                $this->assertStringContainsString('not affected', $text);
            }
        }
    }

    /** When nothing stored opens under the salts, putting them back is the primary advice. */
    public function test_nothing_opens_under_salts_advises_putting_back_the_security_keys(): void
    {
        $probe = $this->probe([
            'site_keypair' => Keystore::ITEM_UNREADABLE,
            'age_identity' => Keystore::ITEM_UNREADABLE,
            'email_secret' => Keystore::ITEM_UNREADABLE,
        ], 'salts');

        $notice  = KeystoreHealth::unreadableNotice($probe);
        $refusal = KeystoreHealth::backupRefusal($probe);

        $this->assertStringStartsWith(
            'The backup key, the connection keys and the email credentials saved on this site cannot be opened',
            $notice
        );
        $this->assertStringContainsString(
            'To fix it, put back the previous values of the security keys in wp-config.php: AUTH_KEY, SECURE_AUTH_KEY, '
            . 'LOGGED_IN_KEY, NONCE_KEY, AUTH_SALT, SECURE_AUTH_SALT, LOGGED_IN_SALT and NONCE_SALT.',
            $notice
        );
        $this->assertStringContainsString('If you cannot, reconnect this site in WPMgr to replace its connection keys.', $notice);
        $this->assertStringContainsString('Save the email settings again in WPMgr to replace the email credentials.', $notice);
        $this->assertStringContainsString('Until the backup key can be read, backups of this site will fail.', $notice);
        $this->assertStringNotContainsString('Do not put back', $notice);
        $this->assertStringContainsString('To fix it, put back the previous values of the security keys', $refusal);
    }

    /**
     * Split state where the connection and email items are the unreadable
     * ones: each gets its own replacement step, never "put back the key".
     */
    public function test_split_state_gives_each_unreadable_item_its_replacement_step(): void
    {
        $probe = $this->probe([
            'site_keypair'  => Keystore::ITEM_OK,
            'cp_public_key' => Keystore::ITEM_UNREADABLE,
            'email_secret'  => Keystore::ITEM_UNREADABLE,
        ], 'salts');

        $notice = KeystoreHealth::unreadableNotice($probe);

        $this->assertStringStartsWith('The connection keys and the email credentials saved on this site', $notice);
        $this->assertStringContainsString('the connection keys and the email credentials were most likely saved under', $notice);
        $this->assertStringContainsString('Reconnect this site in WPMgr to replace its connection keys.', $notice);
        $this->assertStringContainsString('Save the email settings again in WPMgr to replace the email credentials.', $notice);
        $this->assertStringNotContainsString('To fix it, put back', $notice);
        $this->assertStringNotContainsString('backup key', $notice);
    }

    /**
     * No key source is recorded (an unpinned install where no key was
     * found): the copy names no earlier key to restore and never uses the
     * database-copy wording.
     */
    public function test_no_recorded_source_asks_for_replacement_not_a_database_restore(): void
    {
        $probe = $this->probe([
            'age_identity' => Keystore::ITEM_UNREADABLE,
            'email_secret' => Keystore::ITEM_UNREADABLE,
        ], '');

        $notice  = KeystoreHealth::unreadableNotice($probe);
        $refusal = KeystoreHealth::backupRefusal($probe);

        $this->assertStringContainsString('No earlier encryption key is recorded for this site', $notice);
        $this->assertStringContainsString('the backup key and the email credentials cannot be recovered and have to be replaced', $notice);
        $this->assertStringContainsString('Save the email settings again in WPMgr to replace the email credentials.', $notice);
        $this->assertStringContainsString('Backups of this site cannot run until the backup key is reset.', $notice);
        $this->assertStringContainsString('the backup key cannot be recovered and has to be replaced', $refusal);
        foreach ([$notice, $refusal] as $text) {
            $this->assertStringNotContainsString('database', $text);
            $this->assertStringNotContainsString('copied from another site', $text);
            $this->assertStringNotContainsString('To fix it, put back', $text);
        }
    }

    /**
     * The key-unavailable notice and refusal name what is wrong and give a
     * remedy, without the "WPMgr Agent:" prefix the keystore puts on its
     * messages and without parentheses.
     */
    public function test_key_unavailable_copy_has_a_remedy_and_no_repeated_prefix_or_parentheses(): void
    {
        $details = [
            'salts'    => 'WPMgr Agent: pinned salt-derived master key is no longer available (wp-config salts changed or were removed).',
            'constant' => 'WPMgr Agent: pinned WPMGR_AGENT_KEY_FILE master key is no longer available (the constant was removed or its file is missing).',
            'file'     => 'WPMgr Agent: pinned master key file is missing or invalid.',
            'db'       => 'WPMgr Agent: pinned database-stored master key is missing or corrupt.',
            'unknown'  => 'WPMgr Agent: master-key source marker is corrupt or unrecognised.',
        ];
        foreach ($details as $source => $detail) {
            $probe   = $this->probe(['site_keypair' => Keystore::ITEM_UNREADABLE], $source, Keystore::PROBE_KEY_UNAVAILABLE, $detail);
            $notice  = KeystoreHealth::keyUnavailableNotice($probe);
            $refusal = KeystoreHealth::backupRefusal($probe);

            $this->assertStringContainsString('To fix it,', $notice, $source);
            $this->assertStringContainsString('To fix it,', $refusal, $source);
            $this->assertStringStartsWith('Backup not started: this site cannot load its encryption key. ', $refusal, $source);
            foreach ([$notice, $refusal] as $text) {
                $this->assertStringNotContainsString('WPMgr Agent', $text, $source);
                $this->assertStringNotContainsString('(', $text, $source);
                $this->assertStringNotContainsString('..', $text, $source);
            }
        }
    }

    /**
     * An absent backup key may be created only when the current key is shown
     * to be the live one; a stored backup key that does not open never.
     */
    public function test_backup_key_usable_only_under_a_live_key(): void
    {
        $this->assertTrue(KeystoreHealth::backupKeyUsable($this->probe([], 'salts')));
        $this->assertTrue(KeystoreHealth::backupKeyUsable($this->probe(['age_identity' => Keystore::ITEM_OK], 'salts')));
        $this->assertTrue(KeystoreHealth::backupKeyUsable($this->probe([
            'site_keypair' => Keystore::ITEM_OK,
            'email_secret' => Keystore::ITEM_UNREADABLE,
        ], 'salts')));
        $this->assertFalse(KeystoreHealth::backupKeyUsable($this->probe([
            'site_keypair'  => Keystore::ITEM_UNREADABLE,
            'cp_public_key' => Keystore::ITEM_OK,
        ], 'salts')));
        $this->assertFalse(KeystoreHealth::backupKeyUsable($this->probe([
            'site_keypair' => Keystore::ITEM_OK,
            'age_identity' => Keystore::ITEM_UNREADABLE,
        ], 'salts')));
        $this->assertFalse(KeystoreHealth::backupKeyUsable($this->probe(
            ['site_keypair' => Keystore::ITEM_UNREADABLE],
            'file',
            Keystore::PROBE_KEY_UNAVAILABLE
        )));
    }
}
