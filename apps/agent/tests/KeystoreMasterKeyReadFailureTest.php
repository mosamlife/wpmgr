<?php
/**
 * A failed read never changes which master key the site uses.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\KeystoreReadFailed;
use WPMgr\Agent\Plugin;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/** @covers \WPMgr\Agent\Keystore */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class KeystoreMasterKeyReadFailureTest extends TestCase
{
    /** @var array<string,mixed> Options shared with the database double. */
    private array $rows = [];

    /** @var array<string,bool> Missing option records. */
    private array $missing = [];

    /** @var list<string> Option API writes. */
    private array $optionWrites = [];

    private FakeKeyRowWpdb $wpdb;

    /** @var mixed Previous database handle. */
    private $previousWpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->previousWpdb = $GLOBALS['wpdb'] ?? null;
        $this->wpdb = new FakeKeyRowWpdb($this->rows);
        $GLOBALS['wpdb'] = $this->wpdb;
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->optionGet($name, $default));
        Functions\when('update_option')->alias(function ($name, $value): bool {
            if ($name === Keystore::OPTION_MASTER_KEY_SOURCE && $this->wpdb->beforeInsert !== null) {
                ($this->wpdb->beforeInsert)($name);
            }
            $this->optionWrites[] = $name;
            $this->rows[$name] = $value;
            unset($this->missing[$name]);
            return true;
        });
        Functions\when('add_option')->alias(function ($name, $value): bool {
            if (!isset($this->missing[$name]) && $this->optionGet($name) !== false) {
                return false;
            }
            if ($this->wpdb->beforeInsert !== null) {
                ($this->wpdb->beforeInsert)($name);
            }
            $this->optionWrites[] = $name;
            $this->rows[$name] = $value;
            unset($this->missing[$name]);
            return true;
        });
        Functions\when('delete_option')->alias(function ($name): bool {
            unset($this->rows[$name]);
            return true;
        });
        Functions\when('wp_cache_get')->alias(fn ($key) => $key === 'notoptions' ? $this->missing : false);
        Functions\when('wp_cache_set')->alias(function ($key, $value): bool {
            if ($key === 'notoptions') {
                $this->missing = $value;
            }
            return true;
        });
        Functions\when('wp_cache_delete')->justReturn(true);
        Functions\when('wp_json_encode')->alias(static fn ($value) => json_encode($value));
        Functions\when('is_readable')->justReturn(false);
        Functions\when('is_writable')->justReturn(false);
    }

    protected function tear_down(): void
    {
        $GLOBALS['wpdb'] = $this->previousWpdb;
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * @param string $name Option name.
     * @param mixed $default Default value.
     * @return mixed
     */
    private function optionGet(string $name, $default = false)
    {
        if (!in_array($name, [Keystore::OPTION_MASTER_KEY_SOURCE, Keystore::OPTION_DB_MASTER_KEY], true)) {
            return $this->rows[$name] ?? $default;
        }
        if (isset($this->missing[$name])) {
            return $default;
        }
        $row = $this->wpdb->get_row($this->wpdb->prepare(
            'SELECT COUNT(*) AS stored_rows, MAX(option_value) AS stored_value FROM wp_options WHERE option_name = %s',
            $name
        ), ARRAY_A);
        if ($row === null || $row['stored_rows'] === '0') {
            $this->missing[$name] = true;
            return $default;
        }
        return $this->rows[$name];
    }

    /** @return array<string,array{string}> */
    public static function operations(): array
    {
        return ['encrypt' => ['encrypt'], 'decrypt' => ['decrypt'], 'probe' => ['probe'], 'setup' => ['setup']];
    }

    /** @return array<string,string> Plaintext by stored item. */
    private function seed(): array
    {
        $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'db'];
        $this->rows[Keystore::OPTION_DB_MASTER_KEY] = base64_encode(random_bytes(32));
        $keystore = new Keystore();
        $keystore->generateSiteKeypair();
        $keystore->createAgeIdentity(random_bytes(32));
        $keystore->storeControlPlanePublicKey(random_bytes(32));
        $keystore->storeEmailSecret('mail-secret');
        $keystore->store_connection_secrets(['primary' => 'connection-secret']);
        $plain = [];
        foreach ([Keystore::OPTION_SITE_KEYPAIR, Keystore::OPTION_AGE_IDENTITY, Keystore::OPTION_CP_PUBLIC_KEY, Keystore::OPTION_EMAIL_SECRET, Keystore::OPTION_EMAIL_CONN_SECRETS] as $name) {
            $plain[$name] = $keystore->decrypt($this->rows[$name]);
        }
        $this->optionWrites = [];
        $this->wpdb->insertedByName = [];
        define('AUTH_KEY', str_repeat('a', 64));
        return $plain;
    }

    /** @param array<string,string> $plain Stored plaintexts. */
    private function assertReadable(array $plain): void
    {
        foreach ($plain as $name => $expected) {
            $this->assertSame($expected, (new Keystore())->decrypt($this->rows[$name]));
        }
        $this->assertSame(Keystore::PROBE_OK, (new Keystore())->probe()['state']);
    }

    private function setupKeystore(): bool
    {
        $class = new \ReflectionClass(Plugin::class);
        $plugin = $class->newInstanceWithoutConstructor();
        $class->getProperty('keystore')->setValue($plugin, new Keystore());
        return (bool) $class->getMethod('setupKeystore')->invoke($plugin);
    }

    #[DataProvider('operations')]
    public function test_a_failed_marker_read_keeps_the_stored_master_key(string $operation): void
    {
        $plain = $this->seed();
        $before = $this->rows;
        $this->wpdb->failSelectsByName[Keystore::OPTION_MASTER_KEY_SOURCE] = 1;
        if ($operation === 'probe') {
            $this->assertSame(Keystore::PROBE_KEY_UNAVAILABLE, (new Keystore())->probe()['state']);
        } elseif ($operation === 'setup') {
            $this->assertFalse($this->setupKeystore());
        } else {
            try {
                $keystore = new Keystore();
                if ($operation === 'encrypt') {
                    $keystore->encrypt('payload');
                } else {
                    $keystore->decrypt($this->rows[Keystore::OPTION_EMAIL_SECRET]);
                }
                $this->fail('A failed marker read must throw KeystoreReadFailed.');
            } catch (KeystoreReadFailed $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
        $this->assertSame(1, $this->wpdb->failedSelects);
        foreach ($before as $name => $value) {
            $this->assertSame($value, $this->rows[$name]);
        }
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_MASTER_KEY_SOURCE));
        $this->assertSame(0, $this->wpdb->insertedFor(Keystore::OPTION_DB_MASTER_KEY));
        $this->assertReadable($plain);
        if ($operation === 'setup') {
            $this->assertTrue($this->setupKeystore());
        }
    }

    public function test_failed_master_key_reads_never_create_a_key(): void
    {
        $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'db'];
        $this->rows[Keystore::OPTION_DB_MASTER_KEY] = base64_encode(random_bytes(32));
        $before = $this->rows;
        $this->wpdb->failSelectsByName = [Keystore::OPTION_MASTER_KEY_SOURCE => 1, Keystore::OPTION_DB_MASTER_KEY => 1];
        try {
            (new Keystore())->encrypt('payload');
            $this->fail('A failed marker read must throw KeystoreReadFailed.');
        } catch (KeystoreReadFailed $e) {
            $this->assertSame($before, $this->rows);
        }
        $this->assertSame([], $this->optionWrites);
        $this->assertSame(0, $this->wpdb->inserted);
        $this->assertSame(1, $this->wpdb->failedSelects);
        $this->assertSame(1, $this->wpdb->failSelectsByName[Keystore::OPTION_DB_MASTER_KEY]);
        $this->wpdb->failSelectsByName = [];
        $keystore = new Keystore();
        $this->assertSame('payload', $keystore->decrypt($keystore->encrypt('payload')));
    }

    public function test_a_failed_database_key_read_never_creates_a_key(): void
    {
        $this->wpdb->failSelectsByName[Keystore::OPTION_DB_MASTER_KEY] = 1;
        try {
            (new Keystore())->encrypt('payload');
            $this->fail('A failed database-key read must throw KeystoreReadFailed.');
        } catch (KeystoreReadFailed $e) {
            $this->assertSame([], $this->rows);
        }
        $this->assertSame(0, $this->wpdb->inserted);
        $this->assertSame([], $this->optionWrites);
    }

    public function test_a_fresh_install_creates_one_marker_and_one_database_key(): void
    {
        $envelope = (new Keystore())->encrypt('payload');
        $this->assertSame('payload', (new Keystore())->decrypt($envelope));
        $this->assertSame(['source' => 'db'], $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_MASTER_KEY_SOURCE));
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_DB_MASTER_KEY));
        $this->assertSame([], $this->optionWrites);
    }

    public function test_two_creators_use_one_marker_and_one_database_key(): void
    {
        $other = null;
        $this->wpdb->beforeInsert = function () use (&$other): void {
            if ($other !== null) {
                return;
            }
            $other = '';
            $other = (new Keystore())->encrypt('other');
        };
        $mine = (new Keystore())->encrypt('mine');
        $this->assertSame('other', (new Keystore())->decrypt($other));
        $this->assertSame('mine', (new Keystore())->decrypt($mine));
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_MASTER_KEY_SOURCE));
        $this->assertSame(1, $this->wpdb->insertedFor(Keystore::OPTION_DB_MASTER_KEY));
    }

    public function test_pinning_honours_the_marker_stored_first(): void
    {
        define('AUTH_KEY', str_repeat('a', 64));
        $winner = base64_encode(random_bytes(32));
        $this->wpdb->beforeInsert = function (string $name) use ($winner): void {
            if ($name === Keystore::OPTION_MASTER_KEY_SOURCE) {
                $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'db'];
                $this->rows[Keystore::OPTION_DB_MASTER_KEY] = $winner;
            }
        };
        $envelope = (new Keystore())->encrypt('payload');
        $this->assertSame(['source' => 'db'], $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $this->assertSame($winner, $this->rows[Keystore::OPTION_DB_MASTER_KEY]);
        $this->assertSame('payload', (new Keystore())->decrypt($envelope));
    }

    public function test_database_creation_honours_the_key_stored_first(): void
    {
        $winner = base64_encode(random_bytes(32));
        $this->wpdb->beforeInsert = function (string $name) use ($winner): void {
            if ($name === Keystore::OPTION_DB_MASTER_KEY) {
                $this->rows[$name] = $winner;
            }
        };
        $envelope = (new Keystore())->encrypt('payload');
        $this->assertSame($winner, $this->rows[Keystore::OPTION_DB_MASTER_KEY]);
        $this->assertSame('payload', (new Keystore())->decrypt($envelope));
    }

    /** @return array<string,array{mixed}> */
    public static function unpinnedMarkers(): array
    {
        return [
            'empty' => [''],
            'scalar' => ['unassigned'],
            'array without source' => [['path' => '/unused']],
            'non-string source' => [['source' => false]],
        ];
    }

    /** @param mixed $marker Stored option value. */
    #[DataProvider('unpinnedMarkers')]
    public function test_discovery_accepts_empty_and_invalid_markers($marker): void
    {
        $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE] = $marker;
        $envelope = (new Keystore())->encrypt('payload');
        $this->assertSame(['source' => 'db'], $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $this->assertSame('payload', (new Keystore())->decrypt($envelope));
    }

    /** @return array<string,array{string}> */
    public static function invalidDatabaseKeys(): array
    {
        return ['empty' => [''], 'invalid' => ['invalid-key'], 'short' => [base64_encode('short')]];
    }

    #[DataProvider('invalidDatabaseKeys')]
    public function test_an_existing_invalid_database_key_is_not_replaced(string $stored): void
    {
        $this->rows[Keystore::OPTION_DB_MASTER_KEY] = $stored;
        try {
            (new Keystore())->encrypt('payload');
            $this->fail('An invalid stored key must remain unavailable.');
        } catch (\RuntimeException $e) {
            $this->assertSame($stored, $this->rows[Keystore::OPTION_DB_MASTER_KEY]);
            $this->assertArrayNotHasKey(Keystore::OPTION_MASTER_KEY_SOURCE, $this->rows);
        }
    }

    /** @return array<string,array{string}> */
    public static function createdRows(): array
    {
        return [
            'marker' => [Keystore::OPTION_MASTER_KEY_SOURCE],
            'database key' => [Keystore::OPTION_DB_MASTER_KEY],
            'backup key' => [Keystore::OPTION_AGE_IDENTITY],
            'site keypair' => [Keystore::OPTION_SITE_KEYPAIR],
        ];
    }

    #[DataProvider('createdRows')]
    public function test_a_failed_read_back_preserves_the_created_row(string $name): void
    {
        if (in_array($name, [Keystore::OPTION_AGE_IDENTITY, Keystore::OPTION_SITE_KEYPAIR], true)) {
            $this->rows[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'db'];
            $this->rows[Keystore::OPTION_DB_MASTER_KEY] = base64_encode(random_bytes(32));
        }
        $this->wpdb->beforeInsert = function (string $inserted) use ($name): void {
            if ($inserted === $name) {
                $this->wpdb->failSelectsByName[$name] = 1;
            }
        };
        try {
            $this->create($name);
            $this->fail('A failed read-back must throw KeystoreReadFailed.');
        } catch (KeystoreReadFailed $e) {
            $this->assertArrayHasKey($name, $this->rows);
            $this->assertSame(1, $this->wpdb->insertedFor($name));
        }
        $stored = $this->rows[$name];
        $this->wpdb->beforeInsert = null;
        $this->missing = [];
        $result = $this->create($name);
        $this->assertSame($stored, $this->rows[$name]);
        $this->assertSame(1, $this->wpdb->insertedFor($name));
        if ($name === Keystore::OPTION_SITE_KEYPAIR) {
            $this->assertSame(sodium_crypto_sign_publickey((new Keystore())->getSiteKeypair()), $result);
        } elseif ($name === Keystore::OPTION_AGE_IDENTITY) {
            $this->assertSame((new Keystore())->getAgeIdentity(), $result);
        } else {
            $this->assertSame('payload', (new Keystore())->decrypt($result));
        }
    }

    private function create(string $name): string
    {
        $keystore = new Keystore();
        if ($name === Keystore::OPTION_SITE_KEYPAIR) {
            return $keystore->generateSiteKeypair();
        }
        if ($name === Keystore::OPTION_AGE_IDENTITY) {
            return $keystore->createAgeIdentity(random_bytes(32));
        }
        return $keystore->encrypt('payload');
    }
}
