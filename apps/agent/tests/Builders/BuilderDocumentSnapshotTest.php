<?php
/**
 * BuilderDocumentSnapshot: the page snapshot taken at the SQL layer before a
 * builder write, stored as one option, loaded and hashed over the stored
 * bytes, and swept after the undo window.
 *
 * Rows live in FakeBuilderWpdb; add_option() and delete_option() run against
 * its options table as core runs them.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot
 */
final class BuilderDocumentSnapshotTest extends TestCase
{
    private const POST_ID = 418;

    private const REQUEST_ID = '0b5e6f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b';

    private const NOW = 1760000000;

    private FakeBuilderWpdb $db;

    private int $now = self::NOW;

    /** add_option() calls, as [name, value]. */
    private array $added = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->db        = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->db;
        $this->now       = self::NOW;
        $this->added     = [];

        Functions\when('time')->alias(fn (): int => $this->now);
        Functions\when('add_option')->alias(function ($name, $value = '', $unused = '', $autoload = null): bool {
            $this->added[] = [$name, $value];

            return $this->db->addOptionLikeCore((string) $name, $value, $unused, $autoload);
        });
        Functions\when('delete_option')->alias(fn ($name): bool => $this->db->deleteOptionLikeCore((string) $name));
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // What a snapshot holds
    // -------------------------------------------------------------------------

    public function test_reads_raw_rows_in_meta_id_order(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        // Storage order is not meta_id order.
        $this->db->addMeta(30, self::POST_ID, '_elementor_data', '[{"id":"stored"}]');
        $this->db->addMeta(10, self::POST_ID, '_elementor_edit_mode', 'builder');
        $this->db->addMeta(20, self::POST_ID, '_thumbnail_id', '77');

        // Every cache and API answers something else: the snapshot reads none of them.
        Functions\when('get_post_meta')->justReturn(['cached']);
        Functions\when('get_metadata')->justReturn(['cached']);
        Functions\when('get_metadata_raw')->justReturn(['cached']);
        Functions\when('wp_cache_get')->justReturn(['_elementor_data' => ['cached']]);
        Functions\when('get_post')->justReturn((object) ['post_title' => 'cached', 'post_content' => 'cached']);
        Functions\when('get_option')->justReturn('cached');

        $result = BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID);

        $this->assertTrue($result['ok'], (string) ($result['code'] ?? ''));
        $snapshot = $this->snapshot();
        $this->assertSame(
            [['_elementor_edit_mode', 'builder'], ['_thumbnail_id', '77'], ['_elementor_data', '[{"id":"stored"}]']],
            $snapshot['meta']
        );
        $this->assertSame(self::post()['post_title'], $snapshot['post']['post_title']);
        $this->assertSame('SELECT meta_key, meta_value FROM %i WHERE post_id = %d ORDER BY meta_id ASC', $this->sqlOf('SELECT meta_key')[0]);
    }

    public function test_every_meta_key_but_edit_lock(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $rows = [
            [1, '_edit_lock', '1760000000:3'],
            [2, '_edit_last', '3'],
            [3, '_elementor_data', '[]'],
            [4, '_elementor_css', 'a:0:{}'],
            [5, '_thumbnail_id', '77'],
            [6, '_wp_page_template', 'elementor_canvas'],
            [7, 'seo_plugin_title', 'Title'],
            [8, 'custom_field', 'value'],
            // Only the exact key is the lock: these are other rows.
            [9, '_EDIT_LOCK', 'kept'],
            [10, '_edit_lock ', 'kept'],
        ];
        foreach ($rows as [$id, $key, $value]) {
            $this->db->addMeta($id, self::POST_ID, $key, $value);
        }
        $this->db->addMeta(11, self::POST_ID + 1, 'another_post', 'not this one');

        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID)['ok']);

        $want = [];
        foreach (array_slice($rows, 1) as [, $key, $value]) {
            $want[] = [$key, $value];
        }
        $this->assertSame($want, $this->snapshot()['meta']);
    }

    public function test_multi_row_key_kept_with_count_and_order(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(14, self::POST_ID, '_wpmgr_multi', 'third');
        $this->db->addMeta(11, self::POST_ID, '_wpmgr_multi', 'first');
        $this->db->addMeta(12, self::POST_ID, '_elementor_data', '[]');
        $this->db->addMeta(13, self::POST_ID, '_wpmgr_multi', 'second');
        $this->db->addMeta(15, self::POST_ID, '_wpmgr_multi', 'first');

        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID)['ok']);

        $this->assertSame(
            [['_wpmgr_multi', 'first'], ['_elementor_data', '[]'], ['_wpmgr_multi', 'second'], ['_wpmgr_multi', 'third'], ['_wpmgr_multi', 'first']],
            $this->snapshot()['meta'],
            'four rows of one key, a repeated value among them, each where meta_id puts it'
        );
    }

    public function test_bytes_survive_serialized_json_and_escaped_slash(): void
    {
        $post = [
            'post_title'   => 'Café "quoted" & <b>bold</b>',
            'post_content' => '<p>It\'s \"escaped\" and https:\/\/example.com\/a</p>' . "\n" . 'é ü 日本',
            'post_excerpt' => 'A \\ backslash',
            'post_name'    => 'caf%c3%a9',
            'post_parent'  => '12',
            'menu_order'   => '-3',
            'post_author'  => '7',
        ] + self::post();
        $this->db->addPost(self::POST_ID, $post);
        $corpus = [
            '_elementor_page_settings' => serialize(['hide_title' => 'yes', 'title' => 'Café "x"']),
            '_elementor_data'          => '[{"id":"a1b2c3d","settings":{"title":"Say \"hi\" caf\u00e9 é","link":{"url":"https:\/\/example.com\/a?b=1&c=2"}}}]',
            '_wpmgr_url'               => 'https:\/\/example.com\/path',
            '_wpmgr_empty'             => '',
            '_wpmgr_nul'               => "a\0b\0",
            '_wpmgr_not_utf8'          => "\xff\xfe\x00\x80 latin-1 caf\xe9",
            '_wpmgr_backslashes'       => 'C:\\path\\to \\\\ \\u00e9',
            '_wpmgr_newlines'          => "one\r\ntwo\n",
            '_wpmgr_null'              => null,
        ];
        $id = 100;
        foreach ($corpus as $key => $value) {
            $this->db->addMeta(++$id, self::POST_ID, $key, $value);
        }

        foreach ([false, true] as $native) {
            $this->db->nativeInts = $native;
            $requestId            = $native ? '1b5e6f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b' : self::REQUEST_ID;
            $result               = BuilderDocumentSnapshot::take(self::POST_ID, $requestId);
            $this->assertTrue($result['ok'], (string) ($result['code'] ?? ''));

            $stored = $this->storedText($requestId);
            $this->assertMatchesRegularExpression('/^[\x20-\x7e]+$/', $stored, 'the stored text is printable ASCII whatever the bytes');

            $snapshot = $this->snapshot($requestId);
            $this->assertSame(array_map(static fn ($k, $v): array => [$k, $v], array_keys($corpus), $corpus), $snapshot['meta'], 'every byte of every row, and NULL as null');
            foreach (BuilderDocumentSnapshot::RESTORE_POST_COLUMNS as $column) {
                $this->assertSame((string) $this->db->postRow(self::POST_ID)[$column], $snapshot['post'][$column], $column . ($native ? ' (native ints)' : ''));
            }
            $this->assertSame(['12', '-3', '7'], [$snapshot['post']['post_parent'], $snapshot['post']['menu_order'], $snapshot['post']['post_author']]);
        }
    }

    public function test_restore_columns_cover_every_fingerprinted_column(): void
    {
        $fingerprinted = array_merge(BuilderDocumentFingerprint::POST_FIELDS, BuilderDocumentFingerprint::PLACEMENT_FIELDS);
        $this->assertSame([], array_values(array_diff($fingerprinted, BuilderDocumentSnapshot::RESTORE_POST_COLUMNS)));
        $this->assertSame($fingerprinted, array_slice(BuilderDocumentSnapshot::RESTORE_POST_COLUMNS, 0, count($fingerprinted)));
        $this->assertSame(BuilderDocumentSnapshot::RESTORE_POST_COLUMNS, array_values(array_unique(BuilderDocumentSnapshot::RESTORE_POST_COLUMNS)));

        $this->db->addPost(self::POST_ID, self::post());
        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID)['ok']);
        $this->assertSame(
            'SELECT ' . implode(', ', BuilderDocumentSnapshot::RESTORE_POST_COLUMNS) . ' FROM %i WHERE ID = %d',
            $this->sqlOf('SELECT post_type')[0],
            'the posts read names exactly the restore columns, in order'
        );
        $this->assertSame(BuilderDocumentSnapshot::RESTORE_POST_COLUMNS, array_keys($this->snapshot()['post']));
    }

    public function test_revisions_are_the_posts_own_in_id_order(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addPost(530, ['post_type' => 'revision', 'post_parent' => (string) self::POST_ID] + self::post());
        $this->db->addPost(510, ['post_type' => 'revision', 'post_parent' => (string) self::POST_ID, 'post_name' => self::POST_ID . '-autosave-v1'] + self::post());
        $this->db->addPost(520, ['post_type' => 'revision', 'post_parent' => '999'] + self::post());
        $this->db->addPost(540, ['post_type' => 'attachment', 'post_parent' => (string) self::POST_ID] + self::post());

        foreach ([false, true] as $native) {
            $this->db->nativeInts = $native;
            $requestId            = $native ? '1b5e6f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b' : self::REQUEST_ID;
            $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, $requestId)['ok']);
            $this->assertSame([510, 530], $this->snapshot($requestId)['revisions'], $native ? 'native ints' : 'text');
        }
    }

    // -------------------------------------------------------------------------
    // Stored, read back and hashed
    // -------------------------------------------------------------------------

    public function test_stored_bytes_read_back_and_hashed(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, '_elementor_data', '[]');
        // get_option() answers something else: the read-back never consults it.
        Functions\when('get_option')->justReturn('cached');

        $result = BuilderDocumentSnapshot::take(self::POST_ID, strtoupper(self::REQUEST_ID));

        $this->assertTrue($result['ok']);
        $rows = $this->db->optionRows();
        $this->assertCount(1, $rows);
        $this->assertSame(BuilderDocumentSnapshot::OPTION_PREFIX . self::REQUEST_ID, $rows[0]['option_name'], 'named for the request id in lowercase');
        $this->assertSame('off', $rows[0]['autoload'], 'never autoloaded');
        $this->assertIsString($rows[0]['option_value']);
        $this->assertSame(hash('sha256', $rows[0]['option_value']), $result['sha256'] ?? null, 'the hash of the stored bytes');
        $this->assertSame(strlen($rows[0]['option_value']), $result['bytes'] ?? null);
        $this->assertSame([[BuilderDocumentSnapshot::OPTION_PREFIX . self::REQUEST_ID, $rows[0]['option_value']]], $this->added, 'stored as the one string it encoded');
        $this->assertStringStartsWith('{"version":1,"request_id":"' . self::REQUEST_ID . '","post_id":' . self::POST_ID . ',"taken_at":' . self::NOW . ',"post":{"post_type":', $rows[0]['option_value']);
        $this->assertContains('SELECT option_name, option_value FROM %i WHERE option_name = %s', $this->sqlOf('SELECT option_name'), 'read back with SQL');

        $loaded = BuilderDocumentSnapshot::load(self::REQUEST_ID);
        $this->assertSame(['json' => $rows[0]['option_value'], 'sha256' => $result['sha256']], $loaded);

        // load() hashes what is stored now, so a changed row shows.
        $this->db->setOptionValue(BuilderDocumentSnapshot::OPTION_PREFIX . self::REQUEST_ID, $rows[0]['option_value'] . ' ');
        $changed = BuilderDocumentSnapshot::load(self::REQUEST_ID);
        $this->assertSame(hash('sha256', $rows[0]['option_value'] . ' '), $changed['sha256'] ?? null);
        $this->assertNotSame($result['sha256'], $changed['sha256'] ?? null);
    }

    public function test_failed_add_option_is_snapshot_failed(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, '_elementor_data', '[]');
        $name = BuilderDocumentSnapshot::OPTION_PREFIX . self::REQUEST_ID;

        // A row already under the name: add_option() refuses, and that row stays as it is.
        $this->db->addOption($name, 'an earlier snapshot');
        $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID));
        $this->assertSame([[1, $name, 'an earlier snapshot']], $this->optionTable());
        $this->db->deleteOptionLikeCore($name);

        // The insert fails.
        $this->db->failOnStatement = 'INSERT INTO wp_options';
        $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID));
        $this->assertSame([], $this->optionTable(), 'nothing stored');

        // Stored, but not the bytes encoded: removed.
        $this->db->optionWriteFilter = static fn (string $n, string $v): string => str_replace('"post_id"', '"post_ID"', $v);
        $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID));
        $this->assertSame([], $this->optionTable(), 'a row that is not the encoded bytes is removed');
        $this->db->optionWriteFilter = null;

        // Stored, but the read-back fails: removed.
        $this->db->failOnStatement = 'SELECT option_name, option_value';
        $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID));
        $this->assertSame([], $this->optionTable(), 'a row that could not be read back is removed');

        // Then it works.
        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID)['ok']);
    }

    public function test_a_missing_post_or_a_failed_read_stores_nothing(): void
    {
        $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID), 'no such post');

        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, '_elementor_data', '[]');
        $failing = [
            'SELECT COALESCE(SUM(LENGTH(meta_value)), 0)',
            'SELECT post_type',
            'SELECT meta_key, meta_value',
            'SELECT ID FROM',
        ];
        foreach ($failing as $statement) {
            $this->db->failOnStatement = $statement;
            $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID), $statement);
            $this->assertSame('', $this->db->failOnStatement, 'precondition: ' . $statement . ' ran and failed');
        }
        $this->assertSame([], $this->added, 'add_option() is never reached');

        // A column that is not stored text, or an integer column that is not an integer.
        foreach (['post_title' => null, 'post_author' => '01', 'menu_order' => '1.5'] as $column => $value) {
            $this->db->addPost(self::POST_ID, [$column => $value] + self::post());
            $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID), $column);
        }
        $this->db->addPost(self::POST_ID, self::post());

        // A meta key json_encode cannot carry.
        $this->db->addMeta(2, self::POST_ID, "caf\xe9", 'v');
        $this->assertSame(['ok' => false, 'code' => 'snapshot_failed'], BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID));
        $this->assertSame([], $this->optionTable());
    }

    public function test_cap_refuses_and_stores_nothing(): void
    {
        $max = BuilderContract::MAX_SNAPSHOT_BYTES;
        $this->db->addPost(self::POST_ID, self::post());

        // The stored text grows by exactly four bytes for every three 'A's
        // (base64 "QUFB", nothing to escape) and by one for each letter of
        // the key, so find the key and value that land on the cap exactly.
        $this->db->addMeta(1, self::POST_ID, 'k', 'AAA');
        $base = BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(1));
        $this->assertTrue($base['ok']);
        $pad   = ($max - (int) $base['bytes']) % 4;
        $value = 3 + 3 * intdiv($max - (int) $base['bytes'] - $pad, 4);

        $this->db = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->db;
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, 'k' . str_repeat('x', $pad), str_repeat('A', $value));
        $atCap = BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(2));
        $this->assertTrue($atCap['ok'], (string) ($atCap['code'] ?? ''));
        $this->assertSame($max, $atCap['bytes'], 'a text of exactly MAX_SNAPSHOT_BYTES is stored');

        // One byte more is refused, and nothing is stored.
        $this->db = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->db;
        $this->added     = [];
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, 'k' . str_repeat('x', $pad + 1), str_repeat('A', $value));
        $this->assertSame(['ok' => false, 'code' => 'snapshot_too_large'], BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(3)));
        $this->assertSame([], $this->added, 'add_option() is never called');
        $this->assertSame([], $this->optionTable());

        // Meta values whose base64 alone is over the cap are refused before
        // any row is read.
        $this->db = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->db;
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, 'big', str_repeat('A', intdiv(3 * $max, 4) + 1));
        $this->db->addMeta(2, self::POST_ID, '_edit_lock', str_repeat('9', 1000));
        $this->assertSame(['ok' => false, 'code' => 'snapshot_too_large'], BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(4)));
        $this->assertSame([], $this->sqlOf('SELECT meta_key'), 'the rows were never loaded');
        $this->assertSame([], $this->added);
        $this->assertSame([], $this->optionTable());
    }

    // -------------------------------------------------------------------------
    // load() and decode()
    // -------------------------------------------------------------------------

    public function test_load_tells_missing_from_unreadable(): void
    {
        $this->assertSame(['code' => 'snapshot_missing'], BuilderDocumentSnapshot::load(self::REQUEST_ID));

        // Another row the collation would match is not the snapshot.
        $this->db->addOption(strtoupper(BuilderDocumentSnapshot::OPTION_PREFIX . self::REQUEST_ID), '{"version":1}');
        $this->assertSame(['code' => 'snapshot_missing'], BuilderDocumentSnapshot::load(self::REQUEST_ID));

        $this->db->failOnStatement = 'SELECT option_name, option_value';
        $this->assertSame(['code' => 'snapshot_unreadable'], BuilderDocumentSnapshot::load(self::REQUEST_ID));
    }

    public function test_decode_accepts_only_a_snapshot_of_this_request_and_post(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->db->addMeta(1, self::POST_ID, '_elementor_data', '[]');
        $this->db->addPost(530, ['post_type' => 'revision', 'post_parent' => (string) self::POST_ID] + self::post());
        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID)['ok']);
        $json = $this->storedText(self::REQUEST_ID);
        $doc  = json_decode($json, true, 512, JSON_THROW_ON_ERROR);

        $this->assertNotNull(BuilderDocumentSnapshot::decode($json, strtoupper(self::REQUEST_ID), self::POST_ID));
        $this->assertNull(BuilderDocumentSnapshot::decode($json, self::uuid(9), self::POST_ID), 'another request');
        $this->assertNull(BuilderDocumentSnapshot::decode($json, self::REQUEST_ID, self::POST_ID + 1), 'another post');

        $bad = [
            'version 2'          => ['version' => 2] + $doc,
            'members reordered'  => ['request_id' => $doc['request_id']] + $doc,
            'extra member'       => $doc + ['extra' => 1],
            'column missing'     => array_replace($doc, ['post' => array_slice($doc['post'], 1, null, true)]),
            'not base64'         => array_replace_recursive($doc, ['post' => ['post_title' => '!!!']]),
            'not canonical'      => array_replace_recursive($doc, ['post' => ['post_type' => rtrim((string) $doc['post']['post_type'], '=')]]),
            'author not integer' => array_replace_recursive($doc, ['post' => ['post_author' => base64_encode('7a')]]),
            'edit lock row'      => array_replace($doc, ['meta' => [['_edit_lock', base64_encode('1:1')]]]),
            'row of three'       => array_replace($doc, ['meta' => [['k', base64_encode('v'), 'x']]]),
            'key not a string'   => array_replace($doc, ['meta' => [[1, base64_encode('v')]]]),
            'revisions falling'  => array_replace($doc, ['revisions' => [530, 510]]),
            'revision as text'   => array_replace($doc, ['revisions' => ['530']]),
            'taken_at negative'  => array_replace($doc, ['taken_at' => -1]),
        ];
        foreach ($bad as $label => $case) {
            $this->assertNull(BuilderDocumentSnapshot::decode(json_encode($case, JSON_THROW_ON_ERROR), self::REQUEST_ID, self::POST_ID), $label);
        }
        $this->assertNull(BuilderDocumentSnapshot::decode('{"meta":[[["deeper"]]]}', self::REQUEST_ID, self::POST_ID), 'nested deeper than a snapshot');
        $this->assertNull(BuilderDocumentSnapshot::decode('not json', self::REQUEST_ID, self::POST_ID));
    }

    public function test_invalid_arguments_throw(): void
    {
        foreach (['not-a-uuid', self::REQUEST_ID . "\n", '', '0b5e6f3a1c2d4e5f8a9b0c1d2e3f4a5b'] as $requestId) {
            foreach (['take', 'load'] as $method) {
                try {
                    $method === 'take' ? BuilderDocumentSnapshot::take(self::POST_ID, $requestId) : BuilderDocumentSnapshot::load($requestId);
                    $this->fail($method . ' accepted request id ' . json_encode($requestId));
                } catch (\InvalidArgumentException $e) {
                    $this->assertSame('the request id must be a UUID', $e->getMessage());
                }
            }
        }
        $this->expectException(\InvalidArgumentException::class);
        BuilderDocumentSnapshot::take(0, self::REQUEST_ID);
    }

    // -------------------------------------------------------------------------
    // sweep()
    // -------------------------------------------------------------------------

    public function test_sweep_deletes_only_expired_snapshots(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $ages = [
            'expired'      => BuilderDocumentSnapshot::RETENTION_SECONDS + 1,
            'long expired' => 90 * 86400,
            'at the limit' => BuilderDocumentSnapshot::RETENTION_SECONDS,
            'fresh'        => 3600,
            'in an hour'   => -3600,
        ];
        $ids = [];
        $n   = 10;
        foreach ($ages as $label => $age) {
            $ids[$label] = self::uuid(++$n);
            $this->now   = self::NOW - $age;
            $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, $ids[$label])['ok'], $label);
        }
        $this->now = self::NOW;

        // Rows a sweep must never delete, each with an expired head.
        $expiredHead = static fn (string $id): string => '{"version":1,"request_id":"' . $id . '","post_id":' . self::POST_ID . ',"taken_at":' . (self::NOW - 90 * 86400) . ',"post":{}}';
        $keep        = [
            'wpmgr_ability_ledger_' . self::uuid(40)                                    => $expiredHead(self::uuid(40)),
            'wpmgr_abilityXsnapX' . self::uuid(41)                                      => $expiredHead(self::uuid(41)),
            'my_' . BuilderDocumentSnapshot::OPTION_PREFIX . self::uuid(42)             => $expiredHead(self::uuid(42)),
            strtoupper(BuilderDocumentSnapshot::OPTION_PREFIX) . self::uuid(43)        => $expiredHead(self::uuid(43)),
            BuilderDocumentSnapshot::OPTION_PREFIX . self::uuid(44)                     => $expiredHead(self::uuid(45)),
            BuilderDocumentSnapshot::OPTION_PREFIX . 'not-a-request-id'                 => $expiredHead(self::uuid(46)),
            BuilderDocumentSnapshot::OPTION_PREFIX . self::uuid(47)                     => 'not a snapshot',
            BuilderDocumentSnapshot::OPTION_PREFIX . strtoupper(self::uuid(48))         => $expiredHead(strtoupper(self::uuid(48))),
            BuilderDocumentSnapshot::OPTION_PREFIX . self::uuid(49)                     => str_replace('"version":1', '"version":2', $expiredHead(self::uuid(49))),
        ];
        foreach ($keep as $name => $value) {
            $this->db->addOption($name, $value);
        }

        $this->assertSame(2, BuilderDocumentSnapshot::sweep());

        $left = array_column($this->db->optionRows(), 'option_name');
        $this->assertNotContains(BuilderDocumentSnapshot::OPTION_PREFIX . $ids['expired'], $left);
        $this->assertNotContains(BuilderDocumentSnapshot::OPTION_PREFIX . $ids['long expired'], $left);
        foreach (['at the limit', 'fresh', 'in an hour'] as $label) {
            $this->assertContains(BuilderDocumentSnapshot::OPTION_PREFIX . $ids[$label], $left, $label . ' must be kept');
        }
        foreach (array_keys($keep) as $name) {
            $this->assertContains($name, $left, $name . ' must be kept');
        }
        $this->assertSame(['wpmgr\\_ability\\_snap\\_%'], $this->db->likePatterns, 'one anchored pattern with its wildcards escaped');
    }

    public function test_sweep_bounded(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->now = self::NOW - 30 * 86400;
        for ($i = 1; $i <= 25; ++$i) {
            $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::uuid($i))['ok']);
        }
        $this->now = self::NOW;

        $this->assertSame(0, BuilderDocumentSnapshot::sweep(0));
        $this->assertSame([], $this->db->likePatterns, 'a sweep of nothing reads nothing');

        $this->assertSame(20, BuilderDocumentSnapshot::sweep());
        $left = array_column($this->db->optionRows(), 'option_name');
        $this->assertSame(array_map(static fn (int $i): string => BuilderDocumentSnapshot::OPTION_PREFIX . self::uuid($i), range(21, 25)), $left, 'the oldest go first');

        $this->assertSame(3, BuilderDocumentSnapshot::sweep(3));
        $this->assertSame(2, BuilderDocumentSnapshot::sweep());
        $this->assertSame(0, BuilderDocumentSnapshot::sweep());
        $this->assertSame([], $this->db->optionRows());

        $heads = array_values(array_filter($this->db->queries, static fn (array $q): bool => str_starts_with($q['sql'], 'SELECT option_id')));
        $this->assertSame([160, 'wp_options', 'wpmgr\\_ability\\_snap\\_%', BuilderDocumentSnapshot::SWEEP_WINDOW], $heads[0]['args'], 'a bounded read of the heads only');
    }

    public function test_the_scheduled_sweep_deletes_expired_copies_and_is_scheduled_once_hourly(): void
    {
        $this->db->addPost(self::POST_ID, self::post());
        $this->now = self::NOW - 30 * 86400;
        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(1))['ok']);
        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(2))['ok']);
        $this->now = self::NOW;
        $this->assertTrue(BuilderDocumentSnapshot::take(self::POST_ID, self::uuid(3))['ok']);

        // The cron event, as wp-cron runs it: no argument.
        BuilderDocumentSnapshot::sweepScheduled();
        $this->assertSame([BuilderDocumentSnapshot::OPTION_PREFIX . self::uuid(3)], array_column($this->db->optionRows(), 'option_name'), 'the expired copies go, the fresh one stays');

        $events = [];
        Functions\when('wp_next_scheduled')->alias(static function ($hook) use (&$events) {
            return isset($events[$hook]) ? $events[$hook][0] : false;
        });
        Functions\when('wp_schedule_event')->alias(static function ($timestamp, $recurrence, $hook) use (&$events): bool {
            $events[$hook] = [$timestamp, $recurrence];

            return true;
        });
        BuilderDocumentSnapshot::scheduleSweep(self::NOW);
        BuilderDocumentSnapshot::scheduleSweep(self::NOW + 60);
        $this->assertSame([BuilderDocumentSnapshot::HOOK_SWEEP => [self::NOW + 3600, 'hourly']], $events, 'scheduled once, hourly');
        $this->assertSame('wpmgr_builder_snapshot_sweep', BuilderDocumentSnapshot::HOOK_SWEEP);

        // A scheduler that throws never reaches the page-edit write that called it.
        Functions\when('wp_next_scheduled')->alias(static function (): void {
            throw new \RuntimeException('cron unavailable');
        });
        BuilderDocumentSnapshot::scheduleSweep(self::NOW);
    }

    public function test_sweep_never_throws_on_a_failed_read(): void
    {
        $this->db->failOnStatement = 'SELECT option_id';
        $this->assertSame(0, BuilderDocumentSnapshot::sweep());
        unset($GLOBALS['wpdb']);
        $this->assertSame(0, BuilderDocumentSnapshot::sweep());
        $GLOBALS['wpdb'] = $this->db;
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * A posts row with every fingerprinted column set.
     *
     * @return array<string,string>
     */
    private static function post(): array
    {
        return [
            'post_type'             => 'page',
            'post_status'           => 'draft',
            'post_title'            => 'Stored title',
            'post_content'          => 'Stored content',
            'post_excerpt'          => '',
            'post_name'             => '',
            'post_password'         => '',
            'post_modified_gmt'     => '2026-10-09 10:00:00',
            'post_parent'           => '0',
            'menu_order'            => '0',
            'post_modified'         => '2026-10-09 15:30:00',
            'post_content_filtered' => '',
            'post_author'           => '3',
            'post_date'             => '2026-10-08 15:30:00',
            'post_date_gmt'         => '2026-10-08 10:00:00',
        ];
    }

    /** A request id that differs from the others by its last digits only. */
    private static function uuid(int $n): string
    {
        return sprintf('0b5e6f3a-1c2d-4e5f-8a9b-%012x', $n);
    }

    /**
     * The decoded snapshot stored for a request.
     *
     * @return array{request_id:string,post_id:int,taken_at:int,post:array<string,string>,meta:list<array{0:string,1:string|null}>,revisions:list<int>}
     */
    private function snapshot(string $requestId = self::REQUEST_ID): array
    {
        $doc = BuilderDocumentSnapshot::decode($this->storedText($requestId), $requestId, self::POST_ID);
        $this->assertNotNull($doc, 'the stored text decodes as a snapshot of this request and post');

        return $doc;
    }

    /** The stored text under a request's name, from the table itself. */
    private function storedText(string $requestId): string
    {
        foreach ($this->db->optionRows() as $row) {
            if ($row['option_name'] === BuilderDocumentSnapshot::OPTION_PREFIX . strtolower($requestId)) {
                $this->assertIsString($row['option_value']);

                return $row['option_value'];
            }
        }
        $this->fail('no snapshot stored for ' . $requestId);
    }

    /**
     * The options table as [option_id, option_name, option_value] rows.
     *
     * @return list<array{0:int,1:string,2:string|null}>
     */
    private function optionTable(): array
    {
        return array_map(static fn (array $r): array => [$r['option_id'], $r['option_name'], $r['option_value']], $this->db->optionRows());
    }

    /**
     * The statements run that start with a prefix.
     *
     * @return list<string>
     */
    private function sqlOf(string $prefix): array
    {
        return array_values(array_filter(array_column($this->db->queries, 'sql'), static fn (string $sql): bool => str_starts_with($sql, $prefix)));
    }
}
