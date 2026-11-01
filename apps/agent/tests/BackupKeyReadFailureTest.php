<?php
/**
 * GH #1004. A failed database read of the backup-key row must never be taken
 * for "no key is stored", so it can never replace the site's backup key.
 *
 * The option API is modelled the way WordPress core behaves: a SELECT of the
 * row that fails records the option as missing in 'notoptions' and returns
 * false; update_option() of a value get_option() reports missing goes to
 * add_option(), which skips its existence check when 'notoptions' lists the
 * option and writes with ON DUPLICATE KEY UPDATE, replacing the row. The
 * database is FakeKeyRowWpdb over the same rows, and one SELECT of the row can
 * fail the way a MySQL error does.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\KeystoreReadFailed;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Keystore
 * @covers \WPMgr\Agent\Support\AgeIdentity
 * @covers \WPMgr\Agent\Commands\BackupCommand
 * @covers \WPMgr\Agent\Commands\MetadataCommand
 */
final class BackupKeyReadFailureTest extends TestCase
{
    private const READ_FAILED_DETAIL = 'Backup not started: this site could not read its backup key from its database just now. Nothing was changed. Try again in a few minutes.';

    /** @var array<string,mixed> The options table. */
    private array $rows = [];

    /** @var array<string,bool> Core's 'notoptions' cache. */
    private array $notoptions = [];

    /** @var array<string,mixed> Core's per-option value cache. */
    private array $valueCache = [];

    private FakeKeyRowWpdb $wpdb;

    private string $keyFile = '';

    /** @var mixed */
    private $previousWpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-1004-' . bin2hex(random_bytes(8)) . '.key';
        file_put_contents($this->keyFile, random_bytes(32));
        $this->rows       = [Keystore::OPTION_MASTER_KEY_SOURCE => ['source' => 'file', 'path' => $this->keyFile]];
        $this->notoptions = [];
        $this->valueCache = [];

        $this->previousWpdb = $GLOBALS['wpdb'] ?? null;
        $this->wpdb         = new FakeKeyRowWpdb($this->rows);
        $GLOBALS['wpdb']    = $this->wpdb;

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->coreGetOption((string) $name, $default));
        Functions\when('update_option')->alias(fn ($name, $value) => $this->coreUpdateOption((string) $name, $value));
        Functions\when('add_option')->alias(fn ($name, $value = '') => $this->coreAddOption((string) $name, $value));
        Functions\when('delete_option')->alias(function ($name): bool {
            unset($this->rows[$name], $this->valueCache[$name]);

            return true;
        });
        Functions\when('wp_cache_get')->alias(fn ($key, $group = '') => $key === 'notoptions' && $group === 'options' ? $this->notoptions : false);
        Functions\when('wp_cache_set')->alias(function ($key, $value, $group = ''): bool {
            if ($key === 'notoptions' && $group === 'options' && is_array($value)) {
                $this->notoptions = $value;
            }

            return true;
        });
        Functions\when('wp_cache_delete')->alias(function ($key, $group = ''): bool {
            if ($group === 'options') {
                unset($this->valueCache[$key]);
            }

            return true;
        });
    }

    protected function tear_down(): void
    {
        $GLOBALS['wpdb'] = $this->previousWpdb;
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * get_option() as core runs it: 'notoptions' first, then the value cache,
     * then a SELECT of the row. A SELECT that fails, or finds no row, records
     * the option in 'notoptions' and returns the default.
     *
     * @param mixed $default
     * @return mixed
     */
    private function coreGetOption(string $name, $default = false)
    {
        if ($name !== Keystore::OPTION_AGE_IDENTITY) {
            return $this->rows[$name] ?? $default;
        }
        if (isset($this->notoptions[$name])) {
            return $default;
        }
        if (array_key_exists($name, $this->valueCache)) {
            return $this->valueCache[$name];
        }
        $this->wpdb->statements[] = 'core get_option SELECT';
        if ($this->wpdb->failSelects > 0) {
            --$this->wpdb->failSelects;
            ++$this->wpdb->failedSelects;
            $this->notoptions[$name] = true;

            return $default;
        }
        if (!array_key_exists($name, $this->rows)) {
            $this->notoptions[$name] = true;

            return $default;
        }
        $this->valueCache[$name] = $this->rows[$name];

        return $this->rows[$name];
    }

    /** @param mixed $value */
    private function coreUpdateOption(string $name, $value): bool
    {
        if ($name !== Keystore::OPTION_AGE_IDENTITY) {
            $this->rows[$name] = $value;

            return true;
        }
        if ($this->coreGetOption($name) === false) {
            return $this->coreAddOption($name, $value);
        }
        $this->rows[$name]       = $value;
        $this->valueCache[$name] = $value;

        return true;
    }

    /**
     * add_option() as core runs it: the existence check is skipped when
     * 'notoptions' lists the option, and the write is an upsert.
     *
     * @param mixed $value
     */
    private function coreAddOption(string $name, $value): bool
    {
        if ($name !== Keystore::OPTION_AGE_IDENTITY) {
            if (array_key_exists($name, $this->rows)) {
                return false;
            }
            $this->rows[$name] = $value;

            return true;
        }
        if (!isset($this->notoptions[$name]) && $this->coreGetOption($name) !== false) {
            return false;
        }
        if ($this->wpdb->beforeInsert !== null) {
            ($this->wpdb->beforeInsert)();
        }
        // INSERT ... ON DUPLICATE KEY UPDATE: replaces an existing row.
        $this->rows[$name]       = $value;
        $this->valueCache[$name] = $value;
        unset($this->notoptions[$name]);

        return true;
    }

    /** Store a site keypair and a backup key under the current key; return the backup key's recipient. */
    private function seedHealthyKeystore(): string
    {
        $keystore = new Keystore();
        $keystore->generateSiteKeypair();
        $pair = (new AgeCrypto())->generateIdentity();
        $this->rows[Keystore::OPTION_AGE_IDENTITY] = $keystore->encrypt($pair['secret']);

        return $pair['recipient'];
    }

    /** The recipient of the backup key the table holds now. */
    private function storedRecipient(): string
    {
        $stored = $this->rows[Keystore::OPTION_AGE_IDENTITY] ?? null;
        $this->assertIsString($stored, 'A backup key must be stored.');

        return (new AgeCrypto())->recipientForSecret((new Keystore())->decrypt($stored));
    }

    /** @return array<string,mixed> */
    private function backupParams(string $recipient): array
    {
        return [
            'snapshot_id'       => '11111111-2222-3333-4444-555555555555',
            'kind'              => 'files',
            'age_recipient'     => $recipient,
            'chunk_bytes'       => 4 << 20,
            'presign_endpoint'  => 'https://cp.example/agent/v1/backups/x/presign',
            'manifest_endpoint' => 'https://cp.example/agent/v1/backups/x/manifest',
        ];
    }

    private function stubInventory(): void
    {
        Functions\when('get_bloginfo')->justReturn('6.5.2');
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_site_transient')->justReturn(false);
        Functions\when('get_core_updates')->justReturn([]);
    }

    public function test_a_failed_read_throws_keystore_read_failed(): void
    {
        $this->seedHealthyKeystore();
        $sealed                  = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->wpdb->failSelects = 1;

        try {
            (new Keystore())->getAgeIdentity();
            $this->fail('A failed read must throw.');
        } catch (KeystoreReadFailed $e) {
            $this->assertSame(1, $this->wpdb->failedSelects);
        }
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY]);
    }

    /** (a) The backup command's recipient check. */
    public function test_backup_refuses_with_keystore_read_failed_and_keeps_the_key(): void
    {
        $recipient               = $this->seedHealthyKeystore();
        $sealed                  = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->wpdb->failSelects = 1;

        $res = (new BackupCommand(new AgeIdentity(new Keystore())))->execute([], $this->backupParams($recipient));

        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the backup key failed.');
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(0, $this->wpdb->inserted);
        $this->assertFalse($res['ok']);
        $this->assertSame('keystore_read_failed', $res['code'] ?? null);
        $this->assertSame(self::READ_FAILED_DETAIL, $res['detail']);
    }

    /** The mismatch refusal gains a stable code and keeps its detail byte for byte. */
    public function test_backup_mismatch_carries_the_age_recipient_mismatch_code(): void
    {
        $this->seedHealthyKeystore();
        $sealed = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $other  = (new AgeCrypto())->generateIdentity()['recipient'];

        $res = (new BackupCommand(new AgeIdentity(new Keystore())))->execute([], $this->backupParams($other));

        $this->assertFalse($res['ok']);
        $this->assertSame('age_recipient_mismatch', $res['code'] ?? null);
        $this->assertSame('age recipient mismatch', $res['detail']);
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY]);
    }

    /** (b) The metadata push, when the backup key's own read fails. */
    public function test_metadata_push_sends_no_recipient_when_the_key_read_fails(): void
    {
        $this->seedHealthyKeystore();
        $sealed = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->stubInventory();
        // The keystore probe reads through get_option(), served from the
        // value cache; the backup key's own read is the one that fails.
        $this->valueCache[Keystore::OPTION_AGE_IDENTITY] = $sealed;
        $this->wpdb->failSelects                         = 1;

        $data = (new MetadataCommand(new AgeIdentity(new Keystore())))->collect();

        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the backup key failed.');
        $this->assertArrayNotHasKey('age_recipient', $data);
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(0, $this->wpdb->inserted);
    }

    /** (b) The metadata push, when the keystore probe's get_option() read fails first. */
    public function test_metadata_push_keeps_the_key_when_the_probe_read_fails(): void
    {
        $recipient = $this->seedHealthyKeystore();
        $sealed    = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->stubInventory();
        $this->wpdb->failSelects = 1;

        $data = (new MetadataCommand(new AgeIdentity(new Keystore())))->collect();

        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the backup key failed.');
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame($recipient, $data['age_recipient'] ?? null, 'The push carries the stored key, never a new one.');
        $this->assertSame(0, $this->wpdb->inserted);
    }

    /** (c) Admin setup: a failed read leaves the key, and records no notice. */
    public function test_admin_setup_does_nothing_when_the_key_read_fails(): void
    {
        $this->seedHealthyKeystore();
        $sealed = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        // An earlier failed read left the option marked missing (a persistent
        // object cache keeps it), and this request's read fails as well.
        $this->notoptions[Keystore::OPTION_AGE_IDENTITY] = true;
        $this->wpdb->failSelects                         = 1;

        $ready = $this->runSetupKeystore();

        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the backup key failed.');
        $this->assertFalse($ready);
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(0, $this->wpdb->inserted);
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR, $this->rows);
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR_KIND, $this->rows);
    }

    /** A 'notoptions' record that the option is missing, with a healthy database: the stored key is used. */
    public function test_a_stale_notoptions_record_never_replaces_the_stored_key(): void
    {
        $recipient = $this->seedHealthyKeystore();
        $sealed    = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->notoptions[Keystore::OPTION_AGE_IDENTITY] = true;

        $ensured = (new AgeIdentity(new Keystore()))->ensureRecipient();
        $ready   = $this->runSetupKeystore();

        $this->assertSame($recipient, $ensured);
        $this->assertTrue($ready);
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(0, $this->wpdb->inserted);
        $this->assertArrayNotHasKey(Keystore::OPTION_AGE_IDENTITY, $this->notoptions, 'The stale missing record is dropped once the row is found.');
    }

    /** Over-fire guard: a row that is really absent gets exactly one key. */
    public function test_a_really_absent_row_creates_exactly_one_key(): void
    {
        (new Keystore())->generateSiteKeypair();

        $first  = (new AgeIdentity(new Keystore()))->ensureRecipient();
        $second = (new AgeIdentity(new Keystore()))->ensureRecipient();

        $this->assertSame(1, $this->wpdb->inserted, 'Exactly one key is created.');
        $this->assertStringStartsWith('age1', $first);
        $this->assertSame($first, $second);
        $this->assertSame($first, $this->storedRecipient());
        $this->assertArrayNotHasKey(Keystore::OPTION_AGE_IDENTITY, $this->notoptions);
    }

    /** Two creators racing on an absent row settle on one key: the first one stored. */
    public function test_two_creators_settle_on_one_key(): void
    {
        (new Keystore())->generateSiteKeypair();

        $other = null;
        $this->wpdb->beforeInsert = function () use (&$other): void {
            if ($other !== null) {
                return;
            }
            $other = '';
            // Another request creates its key between this one's read and its write.
            $other = (new AgeIdentity(new Keystore()))->ensureRecipient();
        };

        $mine = (new AgeIdentity(new Keystore()))->ensureRecipient();

        $this->assertIsString($other);
        $this->assertStringStartsWith('age1', $other);
        $this->assertSame($other, $mine, 'Both creators must end on the same key.');
        $this->assertSame($other, $this->storedRecipient(), 'The first key stored is the one kept.');
        $this->assertSame(1, $this->wpdb->inserted);
    }

    /** Run Plugin::setupKeystore() over a fresh Keystore, without booting the plugin. */
    private function runSetupKeystore(): bool
    {
        $class  = new \ReflectionClass(Plugin::class);
        $plugin = $class->newInstanceWithoutConstructor();
        $class->getProperty('keystore')->setValue($plugin, new Keystore());

        return (bool) $class->getMethod('setupKeystore')->invoke($plugin);
    }
}
