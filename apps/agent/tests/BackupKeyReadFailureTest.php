<?php
/**
 * GH #1004. The keystore reads the backup-key row directly, so a read that
 * fails is never taken for an absent key, and creates the key only with an
 * insert that cannot replace an existing row.
 *
 * The database is FakeKeyRowWpdb, and a stand-in for the option functions
 * reads and writes the same rows. One SELECT of the row can fail the way a
 * MySQL error does. The metadata push cases live in MetadataCommandTest, next
 * to the inventory stubs that push needs.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\KeystoreReadFailed;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Keystore
 * @covers \WPMgr\Agent\Support\AgeIdentity
 * @covers \WPMgr\Agent\Commands\BackupCommand
 */
final class BackupKeyReadFailureTest extends TestCase
{
    private const READ_FAILED_DETAIL = 'Backup not started: this site could not read its backup key from its database just now. Nothing was changed. Try again in a few minutes.';

    /** @var array<string,mixed> The options table. */
    private array $rows = [];

    /** @var array<string,bool> The options cache's record of missing options. */
    private array $notoptions = [];

    /** @var array<string,mixed> Core's per-option value cache. */
    private array $valueCache = [];

    private FakeKeyRowWpdb $wpdb;

    /** @var list<string> Options whose reads go through the stand-in's database read. */
    private array $modelled = [Keystore::OPTION_AGE_IDENTITY];

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
        $this->modelled   = [Keystore::OPTION_AGE_IDENTITY];

        $this->previousWpdb = $GLOBALS['wpdb'] ?? null;
        $this->wpdb         = new FakeKeyRowWpdb($this->rows);
        $GLOBALS['wpdb']    = $this->wpdb;

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->optionGet((string) $name, $default));
        Functions\when('update_option')->alias(fn ($name, $value) => $this->optionUpdate((string) $name, $value));
        Functions\when('add_option')->alias(fn ($name, $value = '') => $this->optionAdd((string) $name, $value));
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
     * Stand-in for get_option() over the test's rows and caches.
     *
     * @param mixed $default
     * @return mixed
     */
    private function optionGet(string $name, $default = false)
    {
        if (!in_array($name, $this->modelled, true)) {
            return $this->rows[$name] ?? $default;
        }
        if (isset($this->notoptions[$name])) {
            return $default;
        }
        if (array_key_exists($name, $this->valueCache)) {
            return $this->valueCache[$name];
        }
        $this->wpdb->statements[] = 'get_option SELECT';
        if ($this->wpdb->failSelects > 0 || ($this->wpdb->failSelectsByName[$name] ?? 0) > 0) {
            if (($this->wpdb->failSelectsByName[$name] ?? 0) > 0) {
                --$this->wpdb->failSelectsByName[$name];
            } else {
                --$this->wpdb->failSelects;
            }
            $this->wpdb->last_error = 'Lock wait timeout exceeded; try restarting transaction';
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
    private function optionUpdate(string $name, $value): bool
    {
        if (!in_array($name, $this->modelled, true)) {
            $this->rows[$name] = $value;

            return true;
        }
        if ($this->optionGet($name) === false) {
            return $this->optionAdd($name, $value);
        }
        $this->rows[$name]       = $value;
        $this->valueCache[$name] = $value;

        return true;
    }

    /**
     * Stand-in for add_option() over the test's rows and caches.
     *
     * @param mixed $value
     */
    private function optionAdd(string $name, $value): bool
    {
        if (!in_array($name, $this->modelled, true)) {
            if (array_key_exists($name, $this->rows)) {
                return false;
            }
            $this->rows[$name] = $value;

            return true;
        }
        if (!isset($this->notoptions[$name]) && $this->optionGet($name) !== false) {
            return false;
        }
        if ($this->wpdb->beforeInsert !== null) {
            ($this->wpdb->beforeInsert)();
        }
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

    public function test_a_failed_read_throws_keystore_read_failed(): void
    {
        $this->seedHealthyKeystore();
        $sealed                  = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->wpdb->failSelectsByName[Keystore::OPTION_AGE_IDENTITY] = 1;

        try {
            (new Keystore())->getAgeIdentity();
            $this->fail('A failed read must throw.');
        } catch (KeystoreReadFailed $e) {
            $this->assertSame(1, $this->wpdb->failedSelects);
        }
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY]);
    }

    /** (a) The backup command's recipient check. */
    public function test_a_failed_read_during_a_backup_refuses_and_leaves_the_key_unchanged(): void
    {
        $recipient               = $this->seedHealthyKeystore();
        $sealed                  = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->wpdb->failSelectsByName[Keystore::OPTION_AGE_IDENTITY] = 1;

        $res = (new BackupCommand(new AgeIdentity(new Keystore())))->execute([], $this->backupParams($recipient));

        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the backup key failed.');
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_AGE_IDENTITY));
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

    /** (c) Admin setup: a failed read leaves the key, and records no notice. */
    public function test_a_failed_read_records_no_notice_and_leaves_the_key_unchanged(): void
    {
        $this->seedHealthyKeystore();
        $sealed = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        // The options cache reports the key missing, and this request's read
        // of the row fails.
        $this->notoptions[Keystore::OPTION_AGE_IDENTITY] = true;
        $this->wpdb->failSelectsByName[Keystore::OPTION_AGE_IDENTITY] = 1;

        $ready = $this->runSetupKeystore();

        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the backup key failed.');
        $this->assertFalse($ready);
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_AGE_IDENTITY));
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR, $this->rows);
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR_KIND, $this->rows);
    }

    /** With a healthy database, the stored key is used whatever the options cache holds for it. */
    public function test_a_healthy_read_uses_the_stored_key_whatever_the_cache_holds(): void
    {
        $recipient = $this->seedHealthyKeystore();
        $sealed    = $this->rows[Keystore::OPTION_AGE_IDENTITY];
        $this->notoptions[Keystore::OPTION_AGE_IDENTITY] = true;

        $ensured = (new AgeIdentity(new Keystore()))->ensureRecipient();
        $ready   = $this->runSetupKeystore();

        $this->assertSame($recipient, $ensured);
        $this->assertTrue($ready);
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The backup key must not be replaced.');
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_AGE_IDENTITY));
        $this->assertArrayNotHasKey(Keystore::OPTION_AGE_IDENTITY, $this->notoptions, 'A cached missing record is dropped once the row is found.');
    }

    /** Over-fire guard: a row that is really absent gets exactly one key. */
    public function test_a_really_absent_row_creates_exactly_one_key(): void
    {
        (new Keystore())->generateSiteKeypair();

        $first  = (new AgeIdentity(new Keystore()))->ensureRecipient();
        $second = (new AgeIdentity(new Keystore()))->ensureRecipient();

        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_AGE_IDENTITY), 'Exactly one key is created.');
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
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_AGE_IDENTITY));
    }

    /**
     * The key row is stored by another request before this one's insert, so
     * the insert fails: the stored key is the one used, and the error state of
     * the database handle is restored afterwards.
     */
    public function test_a_key_stored_first_by_another_request_is_the_one_used(): void
    {
        (new Keystore())->generateSiteKeypair();
        $this->wpdb->insertSuppressed = [];
        $winner = (new AgeCrypto())->generateIdentity();
        $sealed = (new Keystore())->encrypt($winner['secret']);
        $this->wpdb->beforeInsert = function () use ($sealed): void {
            $this->rows[Keystore::OPTION_AGE_IDENTITY] = $sealed;
        };

        $recipient = (new AgeIdentity(new Keystore()))->ensureRecipient();

        $this->assertSame(1, $this->wpdb->duplicateInserts, 'Precondition: the insert failed on the existing row.');
        $this->assertSame($winner['recipient'], $recipient, 'The stored key is the one used.');
        $this->assertSame($sealed, $this->rows[Keystore::OPTION_AGE_IDENTITY], 'The stored key must not be replaced.');
        $this->assertSame([true], $this->wpdb->insertSuppressed, 'The insert runs with errors suppressed.');
        $this->assertFalse($this->wpdb->suppressErrors, 'The previous error state is restored.');
    }

    /** A failed read never replaces the site keypair, and records no notice. */
    public function test_a_failed_read_never_replaces_the_site_keypair(): void
    {
        $this->seedHealthyKeystore();
        $keypair          = $this->rows[Keystore::OPTION_SITE_KEYPAIR];
        $this->modelled[] = Keystore::OPTION_SITE_KEYPAIR;
        $this->wpdb->insertedByName = [];
        $this->wpdb->failSelectsByName[Keystore::OPTION_SITE_KEYPAIR] = 1;

        $this->runSetupKeystore();

        $this->assertSame($keypair, $this->rows[Keystore::OPTION_SITE_KEYPAIR], 'The site keypair must not be replaced.');
        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the site keypair failed.');
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_SITE_KEYPAIR));
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR, $this->rows);
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR_KIND, $this->rows);
    }

    /** When a read of the site keypair fails, the stored keypair is the one used. */
    public function test_the_stored_site_keypair_is_used_when_a_read_fails(): void
    {
        $this->seedHealthyKeystore();
        $keypair          = $this->rows[Keystore::OPTION_SITE_KEYPAIR];
        $expected         = sodium_crypto_sign_publickey((new Keystore())->decrypt($keypair));
        $this->modelled[] = Keystore::OPTION_SITE_KEYPAIR;
        $this->wpdb->insertedByName = [];
        $this->wpdb->failSelectsByName[Keystore::OPTION_SITE_KEYPAIR] = 1;

        $public = (new Signer(new Keystore()))->agentPublicKey();

        $this->assertSame($keypair, $this->rows[Keystore::OPTION_SITE_KEYPAIR], 'The site keypair must not be replaced.');
        $this->assertSame(1, $this->wpdb->failedSelects, 'Precondition: the read of the site keypair failed.');
        $this->assertSame($expected, $public, 'The stored keypair is the one used.');
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_SITE_KEYPAIR));
    }

    /** Over-fire guard: a site keypair row that is really absent gets exactly one keypair. */
    public function test_a_really_absent_keypair_row_creates_exactly_one_keypair(): void
    {
        $this->modelled[] = Keystore::OPTION_SITE_KEYPAIR;

        $ready = $this->runSetupKeystore();

        $this->assertTrue($ready);
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_SITE_KEYPAIR), 'Exactly one keypair is created.');
        $keypair = (new Keystore())->getSiteKeypair();
        $this->assertIsString($keypair);
        $this->assertSame(SODIUM_CRYPTO_SIGN_KEYPAIRBYTES, strlen($keypair));
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_AGE_IDENTITY));
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
