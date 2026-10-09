<?php
/**
 * BuilderDocumentRestore: the full restore after a failed write and a
 * person's scoped undo, both at the SQL layer, plus the per-key digest.
 *
 * Rows live in FakeBuilderWpdb. Every snapshot is taken by
 * BuilderDocumentSnapshot::take() and read back through load() and decode(),
 * as the write and the undo will. The meta API runs against the same rows as
 * core runs it (unslash, maybe_serialize, every row of a key), so a restore
 * that went through it would change bytes here as it would on a site.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   builder-key-digest.json  rows (base64), their digest and its sha256
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Actions;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\DocumentDescriptor;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorAdapter
 */
final class BuilderDocumentRestoreTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/builder-key-digest.json';

    private const POST_ID = 418;

    private const REQUEST_ID = '6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f';

    private const NOW = 1760000000;

    /** The digest of a key with no row. */
    private const ABSENT = '[0,[]]';

    private FakeBuilderWpdb $db;

    private FakeElementorApi $api;

    private ElementorAdapter $adapter;

    private DocumentDescriptor $descriptor;

    /** @var list<array<int, mixed>> Cache drops, style clears and hook writes, in order. */
    private array $trace = [];

    /** @var list<array{0:string,1:list<mixed>}> Calls into the meta API. */
    private array $metaApi = [];

    /** @var (\Closure(int):void)|null Runs inside clean_post_cache(), as a hooked plugin would. */
    private ?\Closure $onCleanPostCache = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->db               = new FakeBuilderWpdb();
        $GLOBALS['wpdb']        = $this->db;
        $this->trace            = [];
        $this->metaApi          = [];
        $this->onCleanPostCache = null;
        $this->api              = new FakeElementorApi();
        $this->adapter          = new ElementorAdapter($this->api, 1);
        $this->descriptor       = $this->adapter->descriptor();

        Functions\when('time')->justReturn(self::NOW);
        Functions\when('add_option')->alias(fn ($name, $value = '', $unused = '', $autoload = null): bool => $this->db->addOptionLikeCore((string) $name, $value, $unused, $autoload));
        Functions\when('delete_option')->alias(fn ($name): bool => $this->db->deleteOptionLikeCore((string) $name));
        Functions\when('wp_cache_delete')->alias(function ($key, $group = ''): bool {
            $this->trace[] = ['wp_cache_delete', $key, $group, $this->db->inTransaction()];

            return true;
        });
        Functions\when('clean_post_cache')->alias(function ($postId): void {
            $this->trace[] = ['clean_post_cache', $postId, $this->db->inTransaction()];
            if ($this->onCleanPostCache !== null) {
                ($this->onCleanPostCache)((int) $postId);
            }
        });
        Actions\expectDone(ElementorAdapter::HOOK_STYLES_CLEAR)->zeroOrMoreTimes()->whenHappen(function (...$args): void {
            $this->trace[] = ['styles_clear', $args];
        });
        $this->stubMetaApiLikeCore();
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // full(): the restore inside the call that wrote
    // -------------------------------------------------------------------------

    /**
     * @dataProvider drivers
     */
    public function test_full_restore_is_byte_exact(bool $nativeInts): void
    {
        $this->db->nativeInts = $nativeInts;
        $this->seedCorpus();
        $rowsBefore = $this->rows();
        $postBefore = $this->db->postRow(self::POST_ID);
        $snapshot   = $this->snapshot();
        $beforeFp   = $this->fp();

        $this->failedSave();
        $this->assertNotSame($rowsBefore, $this->rows(), 'precondition: the save changed the rows');
        $this->assertNotSame($beforeFp, $this->fp(), 'precondition: the save changed the page');

        $this->assertNull(BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));

        // Keys, values, row counts and order, byte for byte; NULL stays NULL.
        $this->assertSame($rowsBefore, $this->rows());
        $this->assertSame($postBefore, $this->db->postRow(self::POST_ID));
        $this->assertSame($beforeFp, $this->fp());
        $this->assertSame([], $this->metaApi, 'the restore never goes through the meta API');
        // The revision the failed save made is left, as the target's own.
        $this->assertSame('revision', $this->db->postRow(901)['post_type'] ?? null);
        $this->assertSame([['_other_post', 'kept']], array_map(static fn (array $r): array => [$r['meta_key'], $r['meta_value']], $this->db->metaRowsOf(500)));
    }

    /**
     * @return array<string, array{0: bool}>
     */
    public static function drivers(): array
    {
        return ['text columns' => [false], 'native ints' => [true]];
    }

    public function test_full_restore_deletes_keys_added_by_the_save(): void
    {
        $this->seedCorpus();
        $rowsBefore = $this->rows();
        $snapshot   = $this->snapshot();
        $beforeFp   = $this->fp();

        // Keys the save added, a second row on a document key, and a derived
        // cache the save wrote.
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_elementor_controls_usage', 'meta_value' => 'a:0:{}']);
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => 'seo_plugin_score', 'meta_value' => '91']);
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_elementor_edit_mode', 'meta_value' => 'builder']);
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_elementor_page_assets', 'meta_value' => 'a:0:{}']);

        $this->assertNull(BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));

        $keys = array_column($this->rows(), 0);
        foreach (['_elementor_controls_usage', 'seo_plugin_score', '_elementor_page_assets'] as $added) {
            $this->assertNotContains($added, $keys, $added . ' was added by the save and must be gone');
        }
        $this->assertSame(1, array_count_values($keys)['_elementor_edit_mode']);
        $this->assertSame($rowsBefore, $this->rows());
    }

    public function test_full_restore_reruns_fp_and_reports_mismatch(): void
    {
        // A plugin hooked on the post cache writes the document after the
        // restore: the fingerprint read afterwards is not the one before.
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        $this->onCleanPostCache = function (int $postId): void {
            $this->db->insert('wp_postmeta', ['post_id' => $postId, 'meta_key' => '_elementor_data', 'meta_value' => '[]']);
        };
        $this->assertSame(BuilderDocumentRestore::CODE_MISMATCH, BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));

        // A snapshot that is not the page the fingerprint was read from.
        $this->fresh();
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->failedSave();
        $afterFp = $this->fp();
        $this->assertSame(BuilderDocumentRestore::CODE_MISMATCH, BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $afterFp));

        // The same restore with the right fingerprint passes.
        $this->fresh();
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        $this->assertNull(BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));
    }

    public function test_full_restore_invalidates_caches_and_css(): void
    {
        // The page was rendered before the edit: Elementor's caches exist.
        $this->seedCorpus();
        $this->db->addMeta(40, self::POST_ID, '_elementor_css', serialize(['time' => 1759990000, 'status' => 'file']));
        $this->db->addMeta(41, self::POST_ID, '_elementor_element_cache', serialize(['timeout' => 1760086400, 'value' => '<div>old</div>']));
        $this->db->addMeta(42, self::POST_ID, '_elementor_page_assets', serialize(['styles' => ['widget-heading']]));
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_elementor_element_cache', 'meta_value' => serialize(['timeout' => 1760090000, 'value' => '<div>new</div>'])]);

        $this->assertNull(BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));

        $keys = array_column($this->rows(), 0);
        foreach ($this->descriptor->derivedKeys as $derived) {
            $this->assertNotContains($derived, $keys, $derived . ' is a cache the builder rebuilds and must be gone');
        }
        $this->assertSame([[self::POST_ID]], $this->api->callsTo('deletePostCss'));
        $this->assertSame([
            ['wp_cache_delete', self::POST_ID, 'post_meta', false],
            ['wp_cache_delete', self::POST_ID, 'posts', false],
            ['clean_post_cache', self::POST_ID, false],
            ['styles_clear', [['local', self::POST_ID]]],
        ], $this->trace, 'the caches drop after the commit, then the builder invalidates its own');
    }

    public function test_full_restore_keeps_the_edit_lock(): void
    {
        $this->seedCorpus();
        $this->db->addMeta(3, self::POST_ID, '_edit_lock', '1759999000:3');
        // Only the exact key is the lock: this row is the post's own and comes back.
        $this->db->addMeta(43, self::POST_ID, '_EDIT_LOCK', 'kept');
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        // A person opened the editor while the save ran.
        $this->db->delete('wp_postmeta', ['meta_id' => 3]);
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_edit_lock', 'meta_value' => '1760000000:5']);
        $lock = $this->rowsWithIds('_edit_lock');

        $this->assertNull(BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));

        $this->assertSame($lock, $this->rowsWithIds('_edit_lock'), 'the lock row is neither restored nor deleted');
        $this->assertSame([['_EDIT_LOCK', 'kept']], array_values(array_filter($this->rows(), static fn (array $r): bool => $r[0] === '_EDIT_LOCK')));
    }

    // -------------------------------------------------------------------------
    // scoped(): a person's undo of one change
    // -------------------------------------------------------------------------

    public function test_scoped_restores_only_changed_keys(): void
    {
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->edit();
        [$keys, $fields] = $this->changeOfEdit($snapshot);

        // After the change, a person sets a featured image (meta only, no save).
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_thumbnail_id', 'meta_value' => '88']);
        $thumbnail = $this->rowsWithIds('_thumbnail_id');
        $untouched = $this->rowsWithIds('_elementor_page_settings');

        $this->assertNull(BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, $fields, $this->descriptor, $this->adapter));

        $this->assertSame(self::snapshotValues($snapshot, '_elementor_data'), $this->values('_elementor_data'));
        $this->assertSame(self::snapshotValues($snapshot, '_wpmgr_multi'), $this->values('_wpmgr_multi'));
        $this->assertSame($thumbnail, $this->rowsWithIds('_thumbnail_id'), 'the featured image set after the change survives the undo');
        $this->assertSame($untouched, $this->rowsWithIds('_elementor_page_settings'), 'a key the change did not write is not rewritten');
        foreach (['post_content', 'post_modified', 'post_modified_gmt'] as $field) {
            $this->assertSame($snapshot['post'][$field], (string) $this->db->postRow(self::POST_ID)[$field], $field);
        }
        $this->assertSame('Later title', $this->db->postRow(self::POST_ID)['post_title'], 'a column the change did not write is kept');
        $this->assertSame([], $this->metaApi);
        $this->assertSame([[self::POST_ID]], $this->api->callsTo('deletePostCss'));
    }

    public function test_scoped_guard_refuses_when_a_key_moved_on(): void
    {
        $cases = [
            'a later edit rewrote the document' => function (): void {
                $this->replaceRows('_elementor_data', ['[{"id":"later01","elType":"container","elements":[]}]']);
            },
            'a later save added a row to a key the change wrote' => function (): void {
                $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_wpmgr_multi', 'meta_value' => 'fourth']);
            },
            'a later save moved a column the change wrote' => function (): void {
                $this->db->update('wp_posts', ['post_modified_gmt' => '2026-10-09 12:00:00'], ['ID' => self::POST_ID]);
            },
        ];
        foreach ($cases as $label => $later) {
            $this->fresh();
            $this->seedCorpus();
            $snapshot = $this->snapshot();
            $this->edit();
            [$keys, $fields] = $this->changeOfEdit($snapshot);
            $later();
            $rowsBefore = $this->db->metaRowsOf(self::POST_ID);
            $postBefore = $this->db->postRow(self::POST_ID);

            $this->assertSame(
                ['code' => 'conflict', 'detail' => 'changed_after_this_change'],
                BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, $fields, $this->descriptor, $this->adapter),
                $label
            );
            $this->assertSame($rowsBefore, $this->db->metaRowsOf(self::POST_ID), $label . ': nothing written, not even a meta_id');
            $this->assertSame($postBefore, $this->db->postRow(self::POST_ID), $label);
            $this->assertSame([], $this->trace, $label . ': no cache dropped, no style cleared');
            $this->assertSame([], $this->api->callsTo('deletePostCss'), $label);
            $this->assertSame(['START TRANSACTION', 'ROLLBACK'], $this->transactionStatements(), $label);
        }
    }

    public function test_scoped_ignores_and_deletes_derived_keys(): void
    {
        $this->seedCorpus();
        $this->db->addMeta(40, self::POST_ID, '_elementor_css', serialize(['time' => 1759990000, 'status' => 'file']));
        $snapshot = $this->snapshot();
        // The change's save deleted the CSS meta, as Elementor's save does.
        $this->edit();
        $this->replaceRows('_elementor_css', []);
        [$keys, $fields] = $this->changeOfEdit($snapshot);
        $keys[]          = $this->keyChange('_elementor_css', $snapshot);
        $this->assertSame(hash('sha256', self::ABSENT), end($keys)['after_sha256']);

        // A preview after the change regenerated both caches.
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_elementor_css', 'meta_value' => serialize(['time' => 1760001000, 'status' => 'file'])]);
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_elementor_element_cache', 'meta_value' => 'a:0:{}']);

        $this->assertNull(BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, $fields, $this->descriptor, $this->adapter));

        $this->assertSame([], $this->values('_elementor_css'), 'a derived key is deleted, never restored');
        $this->assertSame([], $this->values('_elementor_element_cache'));
        $this->assertSame(self::snapshotValues($snapshot, '_elementor_data'), $this->values('_elementor_data'));
    }

    public function test_scoped_conflict_names_nothing_from_the_site(): void
    {
        $canaryKey   = '_canary_key_q7Zx';
        $canaryValue = 'CANARY-VALUE-3f9a <b>"site"</b>';
        $this->seedCorpus();
        $this->db->addMeta(44, self::POST_ID, $canaryKey, $canaryValue);
        $snapshot = $this->snapshot();
        $this->replaceRows($canaryKey, [$canaryValue . ' changed']);
        $keys = [$this->keyChange($canaryKey, $snapshot)];
        $this->replaceRows($canaryKey, [$canaryValue . ' changed again']);

        $result = BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, [], $this->descriptor, $this->adapter);

        $this->assertSame(['code' => 'conflict', 'detail' => 'changed_after_this_change'], $result);
        $text = (string) json_encode($result);
        foreach ([$canaryKey, 'CANARY', 'site', (string) self::POST_ID, $keys[0]['after_sha256']] as $needle) {
            $this->assertStringNotContainsString($needle, $text);
        }
    }

    public function test_scoped_rows_match_byte_for_byte_never_by_collation(): void
    {
        $this->seedCorpus();
        $this->db->addMeta(45, self::POST_ID, 'Feature', 'upper');
        $this->db->addMeta(46, self::POST_ID, 'feature', 'lower');
        $this->db->addMeta(47, self::POST_ID, 'feature ', 'padded');
        $this->db->addMeta(3, self::POST_ID, '_edit_lock', '1759999000:3');
        $snapshot = $this->snapshot();
        $this->replaceRows('Feature', ['upper changed']);
        $keys = [$this->keyChange('Feature', $snapshot), $this->keyChange('_edit_lock', $snapshot)];
        $lower  = $this->rowsWithIds('feature');
        $padded = $this->rowsWithIds('feature ');
        $lock   = $this->rowsWithIds('_edit_lock');

        $this->assertNull(BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, [], $this->descriptor, $this->adapter));

        $this->assertSame(['upper'], $this->values('Feature'));
        $this->assertSame($lower, $this->rowsWithIds('feature'), 'a key the collation matches is another key');
        $this->assertSame($padded, $this->rowsWithIds('feature '));
        $this->assertSame($lock, $this->rowsWithIds('_edit_lock'), 'the edit lock is never restored, even when a change names it');
    }

    public function test_scoped_deletes_a_key_the_change_added_and_puts_null_back(): void
    {
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => '_wpmgr_added', 'meta_value' => 'new']);
        $this->replaceRows('_wpmgr_null', ['not null now']);
        $keys = [$this->keyChange('_wpmgr_added', $snapshot), $this->keyChange('_wpmgr_null', $snapshot)];
        $this->assertSame(hash('sha256', self::ABSENT), $keys[0]['before_sha256']);

        $this->assertNull(BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, [], $this->descriptor, $this->adapter));

        $this->assertSame([], $this->values('_wpmgr_added'));
        $this->assertSame([null], $this->values('_wpmgr_null'), 'a NULL meta_value comes back as NULL, not as an empty string');
    }

    public function test_scoped_read_back_reports_restore_mismatch(): void
    {
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->edit();
        [$keys, $fields] = $this->changeOfEdit($snapshot);
        $this->onCleanPostCache = function (int $postId): void {
            $this->db->insert('wp_postmeta', ['post_id' => $postId, 'meta_key' => '_elementor_data', 'meta_value' => '[]']);
        };

        $this->assertSame(
            ['code' => 'restore_mismatch', 'detail' => 'read_back_differs'],
            BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, $fields, $this->descriptor, $this->adapter)
        );
    }

    public function test_scoped_snapshot_disagreeing_with_the_change_writes_nothing(): void
    {
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->edit();
        [$keys, $fields] = $this->changeOfEdit($snapshot);
        $before          = count($this->db->queries);

        $badKey                     = $keys;
        $badKey[0]['before_sha256'] = hash('sha256', 'not the snapshot');
        $badField                     = $fields;
        $badField[0]['before_sha256'] = hash('sha256', 'not the snapshot');
        foreach ([[$badKey, $fields], [$keys, $badField]] as [$k, $f]) {
            $this->assertSame(
                ['code' => 'restore_mismatch', 'detail' => 'snapshot_disagrees'],
                BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $k, $f, $this->descriptor, $this->adapter)
            );
        }
        $this->assertCount($before, $this->db->queries, 'refused before any statement');
    }

    // -------------------------------------------------------------------------
    // Both modes
    // -------------------------------------------------------------------------

    public function test_rollback_on_db_error_and_reports(): void
    {
        // full(): the third insert fails, after every delete and two inserts.
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        $rowsBefore = $this->db->metaRowsOf(self::POST_ID);
        $postBefore = $this->db->postRow(self::POST_ID);
        $this->db->failOnStatement      = 'INSERT INTO wp_postmeta';
        $this->db->failOnStatementAfter = 2;

        $this->assertSame(BuilderDocumentRestore::CODE_MISMATCH, BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));
        $this->assertSame($rowsBefore, $this->db->metaRowsOf(self::POST_ID), 'rolled back: the rows the failed write left, meta_ids and all');
        $this->assertSame($postBefore, $this->db->postRow(self::POST_ID));
        $this->assertSame(['START TRANSACTION', 'ROLLBACK'], $this->transactionStatements());
        $this->assertGreaterThan(0, count(array_filter($this->db->queries, static fn (array $q): bool => $q['sql'] === 'DELETE FROM wp_postmeta')), 'precondition: rows were deleted before the failure');
        $this->assertSame([], $this->trace, 'no cache dropped after a rollback');
        $this->assertSame([], $this->api->callsTo('deletePostCss'));

        // scoped(): the posts update fails after the meta writes.
        $this->fresh();
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->edit();
        [$keys, $fields] = $this->changeOfEdit($snapshot);
        $rowsBefore      = $this->db->metaRowsOf(self::POST_ID);
        $postBefore      = $this->db->postRow(self::POST_ID);
        $this->db->failOnStatement = 'UPDATE wp_posts';

        $this->assertSame(
            ['code' => 'restore_mismatch', 'detail' => 'write_failed'],
            BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, $keys, $fields, $this->descriptor, $this->adapter)
        );
        $this->assertSame($rowsBefore, $this->db->metaRowsOf(self::POST_ID));
        $this->assertSame($postBefore, $this->db->postRow(self::POST_ID));
        $this->assertSame(['START TRANSACTION', 'ROLLBACK'], $this->transactionStatements());

        // A locking read that fails ends the restore before any write.
        $this->fresh();
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        $rowsBefore = $this->db->metaRowsOf(self::POST_ID);
        $this->db->failOnStatement = 'SELECT meta_id, meta_key, meta_value FROM %i WHERE post_id = %d ORDER BY meta_id ASC FOR UPDATE';
        $this->assertSame(BuilderDocumentRestore::CODE_MISMATCH, BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));
        $this->assertSame($rowsBefore, $this->db->metaRowsOf(self::POST_ID));
    }

    public function test_writes_run_in_one_transaction_after_locking_reads(): void
    {
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $beforeFp = $this->fp();
        $this->failedSave();
        $start = count($this->db->queries);

        $this->assertNull(BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, $beforeFp));

        $sql = array_column(array_slice($this->db->queries, $start), 'sql');
        $this->assertSame('START TRANSACTION', $sql[0]);
        $this->assertStringEndsWith('FOR UPDATE', $sql[1]);
        $this->assertStringEndsWith('FOR UPDATE', $sql[2]);
        $commit = array_search('COMMIT', $sql, true);
        $this->assertIsInt($commit);
        foreach (array_slice($sql, 3, $commit - 3) as $statement) {
            $this->assertMatchesRegularExpression('/^(DELETE FROM|INSERT INTO) wp_postmeta$|^UPDATE wp_posts$/', $statement);
        }
        $this->assertNotContains('UPDATE wp_postmeta', $sql);
    }

    public function test_malformed_arguments_throw_before_any_statement(): void
    {
        $this->seedCorpus();
        $snapshot = $this->snapshot();
        $this->edit();
        [$keys, $fields] = $this->changeOfEdit($snapshot);
        $start           = count($this->db->queries);
        $sha             = hash('sha256', 'x');

        $other            = $snapshot;
        $other['post_id'] = self::POST_ID + 1;
        $noColumn         = $snapshot;
        unset($noColumn['post']['post_title']);
        $lockRow           = $snapshot;
        $lockRow['meta'][] = ['_edit_lock', '1:1'];
        $calls             = [
            'snapshot of another post' => fn () => BuilderDocumentRestore::full(self::POST_ID, $other, $this->descriptor, $this->adapter, $sha),
            'snapshot missing a column' => fn () => BuilderDocumentRestore::full(self::POST_ID, $noColumn, $this->descriptor, $this->adapter, $sha),
            'snapshot holding the lock' => fn () => BuilderDocumentRestore::full(self::POST_ID, $lockRow, $this->descriptor, $this->adapter, $sha),
            'fingerprint not a sha256' => fn () => BuilderDocumentRestore::full(self::POST_ID, $snapshot, $this->descriptor, $this->adapter, strtoupper($sha)),
            'key change without a member' => fn () => BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, [['key' => '_elementor_data', 'before_sha256' => $sha]], [], $this->descriptor, $this->adapter),
            'key change with an extra member' => fn () => BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, [['key' => '_elementor_data', 'before_sha256' => $sha, 'after_sha256' => $sha, 'value' => 'x']], [], $this->descriptor, $this->adapter),
            'key named twice' => fn () => BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, [$keys[0], $keys[0]], [], $this->descriptor, $this->adapter),
            'hash not lowercase hex' => fn () => BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, [['key' => 'k', 'before_sha256' => $sha, 'after_sha256' => 'abc']], [], $this->descriptor, $this->adapter),
            'field a snapshot does not keep' => fn () => BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, [], [['field' => 'guid', 'before_sha256' => $sha, 'after_sha256' => $sha]], $this->descriptor, $this->adapter),
            'field named twice' => fn () => BuilderDocumentRestore::scoped(self::POST_ID, $snapshot, [], [$fields[0], $fields[0]], $this->descriptor, $this->adapter),
        ];
        foreach ($calls as $label => $call) {
            try {
                $call();
                $this->fail($label . ': no exception');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $label);
            }
        }
        $this->assertCount($start, $this->db->queries, 'nothing ran');
    }

    public function test_elementor_after_restore_drops_post_css_and_clears_local_styles(): void
    {
        $this->adapter->afterRestore(self::POST_ID);
        $this->assertSame([[self::POST_ID]], $this->api->callsTo('deletePostCss'));
        $this->assertSame([['styles_clear', [['local', self::POST_ID]]]], $this->trace);

        // Not a post: nothing asked, nothing fired.
        $this->trace = [];
        $this->adapter->afterRestore(0);
        $this->assertSame([[self::POST_ID]], $this->api->callsTo('deletePostCss'));
        $this->assertSame([], $this->trace);
    }

    public function test_elementor_after_restore_survives_a_throwing_listener(): void
    {
        Monkey\tearDown();
        Monkey\setUp();
        $this->api     = new FakeElementorApi();
        $this->adapter = new ElementorAdapter($this->api, 1);
        Actions\expectDone(ElementorAdapter::HOOK_STYLES_CLEAR)->once()->whenHappen(static function (): void {
            throw new \RuntimeException('listener failed');
        });

        $this->adapter->afterRestore(self::POST_ID);
        $this->assertSame([[self::POST_ID]], $this->api->callsTo('deletePostCss'));
    }

    // -------------------------------------------------------------------------
    // rowsDigest(): the one digest of a key's rows
    // -------------------------------------------------------------------------

    public function test_rows_digest_formula(): void
    {
        $this->assertSame(self::ABSENT, BuilderDocumentRestore::rowsDigest([]));
        $this->assertSame('[1,["' . hash('sha256', '') . '"]]', BuilderDocumentRestore::rowsDigest(['']));
        $this->assertSame('[1,[null]]', BuilderDocumentRestore::rowsDigest([null]));
        $this->assertSame('[2,["' . hash('sha256', 'b') . '","' . hash('sha256', 'a') . '"]]', BuilderDocumentRestore::rowsDigest(['b', 'a']));
        $this->assertSame(hash('sha256', self::ABSENT), BuilderDocumentRestore::rowsSha256([]));
        $this->assertNotSame(BuilderDocumentRestore::rowsDigest(['a', 'b']), BuilderDocumentRestore::rowsDigest(['b', 'a']), 'row order counts');
        $this->assertNotSame(BuilderDocumentRestore::rowsDigest(['']), BuilderDocumentRestore::rowsDigest([null]), 'NULL is not an empty row');

        foreach ([[1 => 'a'], ['a', 7], [['a']]] as $bad) {
            try {
                BuilderDocumentRestore::rowsDigest($bad);
                $this->fail('accepted ' . json_encode($bad));
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_key_digest_fixture_is_what_the_code_produces(): void
    {
        $want = self::digestFixtureText();
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::FIXTURE, $want), 'could not write builder-key-digest.json');
        }
        $got = file_get_contents(self::FIXTURE);
        $this->assertIsString($got, 'builder-key-digest.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $this->assertSame($want, $got, 'builder-key-digest.json differs from what the agent produces now; regenerate with WPMGR_WRITE_FIXTURES=1');

        // Replay: the digests must hold on their own.
        $doc = json_decode($got, true, 512, JSON_THROW_ON_ERROR);
        $this->assertGreaterThanOrEqual(6, count($doc['cases']));
        foreach ($doc['cases'] as $case) {
            $rows = array_map(static fn (?string $b64): ?string => $b64 === null ? null : (string) base64_decode($b64, true), $case['rows_b64']);
            $this->assertSame($case['digest'], BuilderDocumentRestore::rowsDigest($rows), $case['name']);
            $this->assertSame($case['sha256'], hash('sha256', $case['digest']), $case['name']);
        }
        $this->assertCount(count($doc['cases']), array_unique(array_column($doc['cases'], 'sha256')), 'every case has its own digest');
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /** A clean slate for the next scenario in the same test. */
    private function fresh(): void
    {
        $this->tear_down();
        $this->set_up();
    }

    /**
     * The W2.9 corpus: a serialized array, JSON with an escaped quote and a
     * non-ASCII character, an escaped-slash URL, a key with three rows, an
     * empty row, a NULL row and a NUL byte, stored out of meta_id order.
     */
    private function seedCorpus(): void
    {
        $this->db->addPost(self::POST_ID, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => 'Caf' . "\u{e9}" . ' "menu"',
            'post_content'      => '<p>Plain text of the page</p>',
            'post_excerpt'      => 'Summary \\ with a backslash',
            'post_name'         => 'cafe-menu',
            'post_password'     => '',
            'post_modified_gmt' => '2026-10-09 08:00:00',
            'post_modified'     => '2026-10-09 13:30:00',
            'post_parent'       => '12',
            'menu_order'        => '-3',
            'post_author'       => '7',
            'post_date'         => '2026-10-09 13:00:00',
            'post_date_gmt'     => '2026-10-09 07:30:00',
        ]);
        $data = '[{"id":"a1b2c3d","elType":"widget","widgetType":"heading","settings":{"title":"Say \\"hi\\" at the caf' . "\u{e9}" . '","link":{"url":"https:\\/\\/example.com\\/menu"}},"elements":[]}]';
        $this->db->addMeta(20, self::POST_ID, '_elementor_data', $data);
        $this->db->addMeta(10, self::POST_ID, '_elementor_edit_mode', 'builder');
        $this->db->addMeta(11, self::POST_ID, '_elementor_template_type', 'wp-page');
        $this->db->addMeta(12, self::POST_ID, '_elementor_page_settings', serialize(['hide_title' => 'yes', 'template' => 'elementor_canvas']));
        $this->db->addMeta(13, self::POST_ID, '_elementor_version', '3.35.9');
        $this->db->addMeta(31, self::POST_ID, '_wpmgr_multi', 'second');
        $this->db->addMeta(30, self::POST_ID, '_wpmgr_multi', 'first');
        $this->db->addMeta(32, self::POST_ID, '_wpmgr_multi', 'first');
        $this->db->addMeta(14, self::POST_ID, '_wpmgr_empty', '');
        $this->db->addMeta(15, self::POST_ID, '_wpmgr_null', null);
        $this->db->addMeta(16, self::POST_ID, '_wpmgr_bytes', "a\0b\\c\\\\d");
        $this->db->addMeta(17, self::POST_ID, '_thumbnail_id', '77');
        $this->db->addMeta(18, 500, '_other_post', 'kept');
    }

    /** A save that failed after it changed rows, columns and made a revision. */
    private function failedSave(): void
    {
        $this->replaceRows('_elementor_data', ['[{"id":"broken","elType":"container","elements":[]}]']);
        $this->replaceRows('_elementor_page_settings', [serialize(['hide_title' => 'no'])]);
        $this->replaceRows('_wpmgr_multi', ['first', 'fourth']);
        $this->replaceRows('_thumbnail_id', []);
        $this->replaceRows('_wpmgr_null', ['']);
        $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => 'seo_plugin_score', 'meta_value' => '64']);
        $this->db->update('wp_posts', [
            'post_title'        => 'Broken',
            'post_content'      => '<p>broken</p>',
            'post_excerpt'      => '',
            'post_modified_gmt' => '2026-10-09 09:00:00',
            'post_modified'     => '2026-10-09 14:30:00',
            'menu_order'        => '0',
        ], ['ID' => self::POST_ID]);
        $this->db->addPost(901, ['post_type' => 'revision', 'post_status' => 'inherit', 'post_parent' => (string) self::POST_ID, 'post_title' => 'Broken', 'post_content' => '', 'post_modified_gmt' => '2026-10-09 09:00:00']);
    }

    /** A change that applied: the document, a multi-row key and three columns. */
    private function edit(): void
    {
        $this->replaceRows('_elementor_data', ['[{"id":"e1f2a3b","elType":"widget","widgetType":"heading","settings":{"title":"Edited"},"elements":[]}]']);
        $this->replaceRows('_wpmgr_multi', ['first', 'second', 'third']);
        $this->db->update('wp_posts', [
            'post_content'      => '<p>Edited</p>',
            'post_modified_gmt' => '2026-10-09 09:00:00',
            'post_modified'     => '2026-10-09 14:30:00',
        ], ['ID' => self::POST_ID]);
        // A column the change did not write moves on afterwards.
        $this->db->update('wp_posts', ['post_title' => 'Later title'], ['ID' => self::POST_ID]);
    }

    /**
     * The keys and columns edit() wrote, with their hashes, as the write
     * records them: before from the snapshot, after from the stored rows.
     *
     * @param array<string, mixed> $snapshot Decoded snapshot.
     * @return array{0: list<array<string, string>>, 1: list<array<string, string>>}
     */
    private function changeOfEdit(array $snapshot): array
    {
        $keys   = [$this->keyChange('_elementor_data', $snapshot), $this->keyChange('_wpmgr_multi', $snapshot)];
        $fields = [];
        foreach (['post_content', 'post_modified_gmt', 'post_modified'] as $field) {
            $fields[] = [
                'field'         => $field,
                'before_sha256' => hash('sha256', $snapshot['post'][$field]),
                'after_sha256'  => hash('sha256', (string) $this->db->postRow(self::POST_ID)[$field]),
            ];
        }

        return [$keys, $fields];
    }

    /**
     * @param string               $key      Meta key.
     * @param array<string, mixed> $snapshot Decoded snapshot.
     * @return array<string, string>
     */
    private function keyChange(string $key, array $snapshot): array
    {
        return [
            'key'           => $key,
            'before_sha256' => BuilderDocumentRestore::rowsSha256(self::snapshotValues($snapshot, $key)),
            'after_sha256'  => BuilderDocumentRestore::rowsSha256($this->values($key)),
        ];
    }

    /**
     * Replaces every row of one exact key with new rows, as a save would.
     *
     * @param list<string|null> $values New rows, in order.
     */
    private function replaceRows(string $key, array $values): void
    {
        foreach ($this->rowsWithIds($key) as $row) {
            $this->db->delete('wp_postmeta', ['meta_id' => $row['meta_id']]);
        }
        foreach ($values as $value) {
            $this->db->insert('wp_postmeta', ['post_id' => self::POST_ID, 'meta_key' => $key, 'meta_value' => $value]);
        }
    }

    /**
     * Takes, stores, loads and decodes the post's snapshot, as the write and
     * the undo do.
     *
     * @return array<string, mixed>
     */
    private function snapshot(): array
    {
        $taken = BuilderDocumentSnapshot::take(self::POST_ID, self::REQUEST_ID);
        $this->assertTrue($taken['ok'], (string) ($taken['code'] ?? ''));
        $loaded = BuilderDocumentSnapshot::load(self::REQUEST_ID);
        $this->assertSame($taken['sha256'], $loaded['sha256'] ?? null);
        $decoded = BuilderDocumentSnapshot::decode((string) $loaded['json'], self::REQUEST_ID, self::POST_ID);
        $this->assertIsArray($decoded);
        // A second snapshot under the same request is refused; each scenario takes one.
        $this->db->deleteOptionLikeCore(BuilderDocumentSnapshot::OPTION_PREFIX . self::REQUEST_ID);

        return $decoded;
    }

    private function fp(): string
    {
        return (string) BuilderDocumentFingerprint::ofPost(self::POST_ID, $this->descriptor->exactKeys);
    }

    /**
     * The post's rows in meta_id order as [key, value].
     *
     * @return list<array{0: string, 1: string|null}>
     */
    private function rows(): array
    {
        return array_map(static fn (array $r): array => [$r['meta_key'], $r['meta_value']], $this->db->metaRowsOf(self::POST_ID));
    }

    /**
     * The stored rows of one exact key, meta_id included.
     *
     * @return list<array{meta_id: int, meta_key: string, meta_value: string|null}>
     */
    private function rowsWithIds(string $key): array
    {
        return array_values(array_filter($this->db->metaRowsOf(self::POST_ID), static fn (array $r): bool => $r['meta_key'] === $key));
    }

    /**
     * @return list<string|null>
     */
    private function values(string $key): array
    {
        return array_column($this->rowsWithIds($key), 'meta_value');
    }

    /**
     * @param array<string, mixed> $snapshot Decoded snapshot.
     * @return list<string|null>
     */
    private static function snapshotValues(array $snapshot, string $key): array
    {
        $values = [];
        foreach ($snapshot['meta'] as [$k, $v]) {
            if ($k === $key) {
                $values[] = $v;
            }
        }

        return $values;
    }

    /**
     * START TRANSACTION, COMMIT and ROLLBACK, in the order they ran.
     *
     * @return list<string>
     */
    private function transactionStatements(): array
    {
        return array_values(array_filter(
            array_column($this->db->queries, 'sql'),
            static fn (string $sql): bool => in_array($sql, ['START TRANSACTION', 'COMMIT', 'ROLLBACK'], true)
        ));
    }

    /**
     * The meta API as core runs it against these rows: the value unslashed
     * and passed through maybe_serialize(), every row of a key (as the
     * collation matches it) updated to the one value, and hooks never fired.
     * Each call is recorded.
     */
    private function stubMetaApiLikeCore(): void
    {
        $maybeSerialize = static function ($value) {
            if (is_array($value) || is_object($value) || (is_string($value) && self::isSerialized($value))) {
                return serialize($value);
            }

            return $value;
        };
        $prepare = static fn ($value) => $maybeSerialize(wp_unslash($value));
        $matching = fn (int $postId, string $key): array => array_values(array_filter(
            $this->db->metaRowsOf($postId),
            static fn (array $r): bool => strtolower(rtrim($r['meta_key'], ' ')) === strtolower(rtrim($key, ' '))
        ));
        $add = function (int $postId, string $key, $value) use ($prepare) {
            $this->db->insert('wp_postmeta', ['post_id' => $postId, 'meta_key' => wp_unslash($key), 'meta_value' => $prepare($value)]);

            return $this->db->insert_id;
        };
        $update = function (int $postId, string $key, $value) use ($prepare, $matching, $add) {
            $rows = $matching($postId, $key);
            if ($rows === []) {
                return $add($postId, $key, $value);
            }
            foreach ($rows as $row) {
                $this->db->delete('wp_postmeta', ['meta_id' => $row['meta_id']]);
            }
            foreach ($rows as $row) {
                $this->db->insert('wp_postmeta', ['post_id' => $postId, 'meta_key' => $row['meta_key'], 'meta_value' => $prepare($value)]);
            }

            return true;
        };
        $delete = function (int $postId, string $key) use ($matching): bool {
            $rows = $matching($postId, $key);
            foreach ($rows as $row) {
                $this->db->delete('wp_postmeta', ['meta_id' => $row['meta_id']]);
            }

            return $rows !== [];
        };
        $record = function (string $name, array $args): void {
            $this->metaApi[] = [$name, $args];
        };

        Functions\when('maybe_serialize')->alias($maybeSerialize);
        Functions\when('add_metadata')->alias(function ($type, $id, $key, $value) use ($record, $add) {
            $record('add_metadata', func_get_args());

            return $add((int) $id, (string) $key, $value);
        });
        Functions\when('update_metadata')->alias(function ($type, $id, $key, $value) use ($record, $update) {
            $record('update_metadata', func_get_args());

            return $update((int) $id, (string) $key, $value);
        });
        Functions\when('delete_metadata')->alias(function ($type, $id, $key) use ($record, $delete): bool {
            $record('delete_metadata', func_get_args());

            return $delete((int) $id, (string) $key);
        });
        Functions\when('add_post_meta')->alias(function ($id, $key, $value) use ($record, $add) {
            $record('add_post_meta', func_get_args());

            return $add((int) $id, (string) $key, $value);
        });
        Functions\when('update_post_meta')->alias(function ($id, $key, $value) use ($record, $update) {
            $record('update_post_meta', func_get_args());

            return $update((int) $id, (string) $key, $value);
        });
        Functions\when('delete_post_meta')->alias(function ($id, $key) use ($record, $delete): bool {
            $record('delete_post_meta', func_get_args());

            return $delete((int) $id, (string) $key);
        });
    }

    /**
     * Core's is_serialized() for a string, non-strict, as maybe_serialize()
     * asks it.
     */
    private static function isSerialized(string $data): bool
    {
        $data = trim($data);
        if ($data === 'N;') {
            return true;
        }
        if (strlen($data) < 4 || $data[1] !== ':') {
            return false;
        }

        return preg_match('/^(?:[aOC]:\d+:|s:\d+:"|[bid]:[0-9.E+-]+;)/', $data) === 1;
    }

    /**
     * The key digest fixture text.
     */
    private static function digestFixtureText(): string
    {
        $cases = [
            'absent'                    => [],
            'one-empty-row'             => [''],
            'null-row'                  => [null],
            'serialized-page-settings'  => [serialize(['hide_title' => 'yes', 'template' => 'elementor_canvas'])],
            'json-escapes-and-utf8'     => ['[{"title":"Say \\"hi\\" at the caf' . "\u{e9}" . '","url":"https:\\/\\/example.com\\/menu"}]'],
            'three-rows-in-meta-id-order' => ['first', 'second', 'first'],
            'same-rows-other-order'     => ['second', 'first', 'first'],
            'nul-byte-and-backslashes'  => ["a\0b\\c\\\\d"],
        ];
        $out = [];
        foreach ($cases as $name => $rows) {
            $digest = BuilderDocumentRestore::rowsDigest($rows);
            $out[]  = [
                'name'     => $name,
                'rows_b64' => array_map(static fn (?string $row): ?string => $row === null ? null : base64_encode($row), $rows),
                'digest'   => $digest,
                'sha256'   => hash('sha256', $digest),
            ];
        }
        $doc = [
            'note'  => 'Generated by the agent (tests/Builders/BuilderDocumentRestoreTest.php). Regenerate with WPMGR_WRITE_FIXTURES=1. '
                . 'The digest of one meta key\'s rows on a post: json_encode, default flags, of [row_count, [sha256(row), ...]] with the rows in meta_id order, '
                . 'each sha256 lowercase hex over the stored bytes, and null in place of the hash for a row whose meta_value is NULL. '
                . 'A key with no row is [0,[]], which differs from one empty row. The hash a builder page change records for a key (before_sha256, after_sha256) '
                . 'is the sha256, lowercase hex, of that digest text. rows_b64 holds each stored row, base64, in meta_id order; null is a NULL row.',
            'cases' => $out,
        ];

        return json_encode($doc, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) . "\n";
    }
}
