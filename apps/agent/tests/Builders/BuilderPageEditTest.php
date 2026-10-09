<?php
/**
 * BuilderPageEdit: the wpmgr/page-edit plan and preview, and the write that
 * snapshots the page, saves it through Elementor, reads it back and puts it
 * back from the snapshot when anything is off.
 *
 * Rows live in FakeBuilderWpdb behind EngineWpdb (which adds the ledger's
 * claim rows) and are read through the SQL reads production uses; the
 * ledger rows are this test's options, the snapshot rows FakeBuilderWpdb's
 * options table. Elementor is FakeElementorApi behind the real
 * ElementorAdapter, handed over as the compiled set. Saves go into a
 * stand-in document that stores the elements as Elementor's save does.
 * Hooks the write scope installs are captured and fired by hand.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\BuilderPageEdit;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\OwnAbilities;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageEdit
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore
 */
final class BuilderPageEditTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    private const TARGET = 41;

    private const OTHER = 42;

    private const KIT = 4;

    private const PRINCIPAL = 2;

    /** The page-create request that created TARGET. */
    private const CREATE = '11111111-2222-4333-8444-777777777777';

    /** The page-edit request. */
    private const EDIT = '22222222-3333-4444-8555-888888888888';

    private const ENTRY_SHA = 'e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0';

    /** Ids in the golden two-column page. */
    private const HEADING = '52982f9';

    private const TEXT = '6cbe98a';

    private const RIGHT = '3d3406f';

    /** A colour a person set on the heading: a setting WPMgr never writes. */
    private const PERSON_COLOR = '#c0392b';

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<int,int> */
    private array $autosaves = [];

    /** @var array<int,int> */
    private array $locks = [];

    private FakeBuilderWpdb $rows;

    private EngineWpdb $wpdb;

    private FakeElementorApi $api;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->hooks     = [];
        $this->options   = [];
        $this->autosaves = [];
        $this->locks     = [];

        $capture = function ($tag, $cb, $prio = 10, $args = 1) {
            $this->hooks[] = [(string) $tag, $cb, (int) $prio, (int) $args];

            return true;
        };
        $release = function ($tag, $cb, $prio = 10) {
            foreach ($this->hooks as $i => [$t, $c, $p]) {
                if ($t === $tag && $c === $cb && $p === (int) $prio) {
                    unset($this->hooks[$i]);
                    $this->hooks = array_values($this->hooks);

                    return true;
                }
            }

            return false;
        };
        $snap = static fn ($name): bool => str_starts_with((string) $name, BuilderDocumentSnapshot::OPTION_PREFIX);
        Functions\when('add_filter')->alias($capture);
        Functions\when('add_action')->alias($capture);
        Functions\when('remove_filter')->alias($release);
        Functions\when('remove_action')->alias($release);
        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('add_option')->alias(function ($name, $value = '', $unused = '', $autoload = null) use ($snap): bool {
            if ($snap($name)) {
                return $this->rows->addOptionLikeCore((string) $name, $value, $unused, $autoload);
            }
            if (array_key_exists($name, $this->options)) {
                return false;
            }
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('delete_option')->alias(function ($name) use ($snap): bool {
            if ($snap($name)) {
                return $this->rows->deleteOptionLikeCore((string) $name);
            }
            $had = array_key_exists($name, $this->options);
            unset($this->options[$name]);

            return $had;
        });
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('get_userdata')->justReturn(false);
        Functions\when('get_post_meta')->justReturn('');
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_cache_delete')->justReturn(true);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wc_get_page_id')->justReturn(-1);
        Functions\when('wp_get_post_autosave')->alias(function ($id, $user = 0) {
            $by = $this->autosaves[(int) $id] ?? null;

            return $by !== null && ($user === 0 || $user === $by) ? (object) ['ID' => 500, 'post_type' => 'revision'] : false;
        });
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->storePage(self::page());
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_precheck_preview_and_digest_deterministic(): void
    {
        $input = $this->input(self::ops());
        $one   = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $one, (string) json_encode($one['refusal'] ?? null));
        $two = $this->plan($input);

        $this->assertSame(json_encode($one['preview']), json_encode($two['preview']), 'the same request, input and page give the same preview bytes');
        $this->assertSame($one['preview_digest'], $two['preview_digest']);
        $this->assertSame(['post_id', 'builder', 'builder_version', 'format', 'title', 'changes', 'after_outline', 'tree_sha256', 'tree'], array_keys($one['preview']));
        $preview = $one['preview'];
        $this->assertSame([self::TARGET, 'elementor', '3.35.9', 'classic', 'Spring'], [$preview['post_id'], $preview['builder'], $preview['builder_version'], $preview['format'], $preview['title']]);
        $treeJson = (string) json_encode($preview['tree']);
        $this->assertSame(hash('sha256', $treeJson), $preview['tree_sha256']);
        $this->assertSame($this->currentFp(), $one['base_fingerprint']);
        $this->assertSame(
            hash('sha256', (string) json_encode(['wpmgr.page_edit.v1', 'elementor', '3.35.9', self::TARGET, $this->currentFp(), $treeJson])),
            $one['preview_digest'],
            'the digest binds the builder, its version, the post, the base and the edited tree'
        );
        $this->assertSame(json_encode(ElementorClassicMapper::project($preview['tree'])->toArray(BuilderContract::MAX_STRUCTURE_NODES)), json_encode($preview['after_outline']));
        $this->assertSame(['set_text', 'insert'], array_column($preview['changes'], 'op'));
        $this->assertSame('Summer sale', $preview['tree'][0]['elements'][0]['elements'][0]['settings']['title']);
        $this->assertSame(self::PERSON_COLOR, $preview['tree'][0]['elements'][0]['elements'][0]['settings']['title_color'], 'a setting a person made stays');
        $this->assertCount(2, $preview['tree'][0]['elements'][1]['elements'], 'the new paragraph sits after the text');

        // Another request makes other node ids, so other bytes.
        $other = BuilderPageEdit::plan($input, $this->entry(), [self::TARGET], '33333333-3333-4444-8555-888888888888', self::noMedia(), $this->seam());
        $this->assertNotSame($one['preview_digest'], $other['preview_digest'] ?? null);
        $this->assertSame([], $this->rows->optionRows(), 'a precheck stores nothing');
    }

    public function test_write_digest_matches_precheck(): void
    {
        $input = $this->input(self::ops());
        $plan  = $this->plan($input);
        $pre   = $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']);

        foreach (['preview' => [$pre, str_repeat('b', 64)], 'precheck' => [str_repeat('a', 64), $plan['preview_digest']]] as $why => [$expPre, $expPrev]) {
            $before = $this->rows->metaRowsOf(self::TARGET);
            $r      = $this->write($input, $expPre, $expPrev);
            $this->assertSame(['preview_changed', false], [$r['code'] ?? null, $r['ok'] ?? null], $why);
            $this->assertSame($before, $this->rows->metaRowsOf(self::TARGET), $why . ': nothing changed');
            $this->assertNull(AbilityLedger::get(self::EDIT), $why . ': no ledger row');
            $this->assertSame([], $this->rows->optionRows(), $why . ': no snapshot');
        }

        $this->api->storingDocument(self::TARGET, $this->rows);
        $r = $this->write($input, $pre, $plan['preview_digest']);
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame(['applied', 'write', OwnAbilities::NAME_PAGE_EDIT, self::TARGET, 2, $plan['preview_digest']], [$r['outcome'], $r['mode'], $r['ability'], $r['post_id'], $r['changes_applied'], $r['preview_digest']]);
        $stored = BuilderDocumentFingerprint::read(self::TARGET, [ElementorDocument::KEY_DATA]);
        $this->assertSame($plan['preview']['tree'], json_decode($stored['rows'][ElementorDocument::KEY_DATA][0], true), 'Elementor stored the approved tree');
        $this->assertSame($this->currentFp(), $r['after_fp']);
        $this->assertSame($plan['base_fingerprint'], $r['before_fp']);
        $this->assertSame([], $this->wpdb->claims, 'the target claim is released');
    }

    public function test_snapshot_before_any_effect(): void
    {
        $input = $this->input(self::ops());
        $this->rows->queries = [];
        $r = $this->approvedWrite($input);
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));

        $statements = array_column($this->rows->queries, 'sql');
        $snapshot   = array_search('INSERT INTO wp_options', $statements, true);
        $firstMeta  = null;
        foreach ($statements as $i => $sql) {
            if (str_contains($sql, 'wp_postmeta') && (str_starts_with($sql, 'INSERT') || str_starts_with($sql, 'DELETE') || str_starts_with($sql, 'UPDATE'))) {
                $firstMeta = $i;
                break;
            }
        }
        $this->assertIsInt($snapshot, 'the snapshot row was written: ' . json_encode($statements));
        $this->assertIsInt($firstMeta, 'the save wrote the page');
        $this->assertLessThan($firstMeta, $snapshot, 'the snapshot is stored before any meta write');
        $this->assertSame('snapshot_taken', $this->ledgerPhases()[1] ?? null, 'the ledger holds the snapshot before the save');
    }

    public function test_snapshot_failure_changes_nothing(): void
    {
        $input  = $this->input(self::ops());
        $before = $this->rows->metaRowsOf(self::TARGET);
        $post   = $this->rows->postRow(self::TARGET);

        // The stored bytes are not the bytes encoded: snapshot_failed.
        $this->rows->optionWriteFilter = static fn (string $name, string $value): string => $value . ' ';
        $r                             = $this->approvedWrite($input);
        $this->assertSame(['snapshot_failed', false], [$r['code'] ?? null, $r['ok'] ?? null], (string) json_encode($r));
        $this->assertSame([], $this->api->documents[self::TARGET]->saves, 'Elementor was never asked to save');
        $this->assertSame($before, $this->rows->metaRowsOf(self::TARGET));
        $this->assertSame($post, $this->rows->postRow(self::TARGET));
        $this->assertSame('failed', AbilityLedger::get(self::EDIT)['phase'] ?? null);
        $this->assertSame($r, AbilityLedger::get(self::EDIT)['result'] ?? null);

        // Over the cap: snapshot_too_large, nothing stored.
        $this->options = array_intersect_key($this->options, ['wpmgr_ability_ledger_' . self::CREATE => 1]);
        $this->rows->optionWriteFilter = null;
        $this->rows->addMeta(900, self::TARGET, 'big', str_repeat('x', BuilderContract::MAX_SNAPSHOT_BYTES));
        $big = $this->approvedWrite($this->input(self::ops()));
        $this->assertSame('snapshot_too_large', $big['code'] ?? null, (string) json_encode($big));
        $this->assertSame([], $this->api->documents[self::TARGET]->saves);
        $this->assertSame([], $this->rows->optionRows());
    }

    public function test_verify_failure_restores_byte_exact(): void
    {
        $this->storePage(self::page(), [['_wpmgr_note', 'a:1:{s:1:"a";s:3:"\\"é";}'], ['_multi', 'one'], ['_multi', 'two']]);
        $input  = $this->input(self::ops());
        $before = self::pairs($this->rows->metaRowsOf(self::TARGET));
        $post   = $this->rows->postRow(self::TARGET);
        $baseFp = $this->currentFp();

        // A filter on the save changes a node the edit did not touch.
        $r = $this->approvedWrite($input, static function (array $elements): array {
            $elements[0]['elements'][1]['elements'][0]['settings']['editor'] = '<p>Changed by a filter</p>';

            return $elements;
        });
        $this->assertSame(['verify_mismatch', true, false], [$r['code'] ?? null, $r['restored'] ?? null, $r['ok'] ?? null], (string) json_encode($r));
        $this->assertStringContainsString('tree_differs', (string) $r['detail']);
        $this->assertSame($before, self::pairs($this->rows->metaRowsOf(self::TARGET)), 'every row is back, byte for byte, in order');
        $this->assertSame($post, $this->rows->postRow(self::TARGET));
        $this->assertSame($baseFp, $this->currentFp());
        $row = AbilityLedger::get(self::EDIT);
        $this->assertSame(['failed', true], [$row['phase'] ?? null, $row['restored'] ?? null]);
        $this->assertSame($r, $row['result']);
    }

    public function test_side_effect_restores(): void
    {
        $input  = $this->input(self::ops());
        $before = self::pairs($this->rows->metaRowsOf(self::TARGET));
        $r      = $this->approvedWrite($input, null, function (): void {
            $this->fire('added_post_meta', 77, self::OTHER, '_thumbnail_id', '5');
        });
        $this->assertSame(['side_effect_detected', true], [$r['code'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
        $this->assertSame($before, self::pairs($this->rows->metaRowsOf(self::TARGET)));
        $this->assertSame([], $this->hooks, 'the write scope is disarmed');
    }

    public function test_save_throw_restores(): void
    {
        $input  = $this->input(self::ops());
        $before = self::pairs($this->rows->metaRowsOf(self::TARGET));
        $post   = $this->rows->postRow(self::TARGET);
        $r      = $this->approvedWrite($input, null, function (): void {
            // Elementor wrote half the page, then failed.
            $this->rows->insert($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => '_elementor_css', 'meta_value' => 'a:0:{}']);
            $this->rows->update($this->rows->posts, ['post_content' => 'half'], ['ID' => self::TARGET]);
            throw new \RuntimeException('Elementor failed');
        });
        $this->assertSame(['builder_crashed', true], [$r['code'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
        $this->assertSame($before, self::pairs($this->rows->metaRowsOf(self::TARGET)), 'the half-written rows are gone');
        $this->assertSame($post, $this->rows->postRow(self::TARGET));
        $this->assertSame([[self::TARGET]], $this->api->callsTo('deletePostCss'), 'the page\'s generated CSS is dropped');
    }

    public function test_changed_keys_and_fields_recorded_with_hashes(): void
    {
        $this->storePage(self::page(), [['_plugin_cache', 'stale']]);
        $input  = $this->input(self::ops());
        $data0 = $this->rowsOf(ElementorDocument::KEY_DATA);
        $r     = $this->approvedWrite($input, null, function (): void {
            // As core and Elementor do on a save: the meta API's hook for the
            // tree, a new revision, the post's text and time, and a plugin
            // that drops its cache row.
            $this->fire('wp_insert_post', 60, (object) ['ID' => 60, 'post_type' => 'revision', 'post_parent' => self::TARGET], false);
            $this->fire('updated_post_meta', 4, self::TARGET, ElementorDocument::KEY_DATA, '[]');
            $this->rows->update($this->rows->posts, ['post_content' => 'Summer sale Right column New words', 'post_modified_gmt' => '2026-10-09 08:00:00'], ['ID' => self::TARGET]);
            $this->rows->delete($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => '_plugin_cache']);
        });
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));

        $row  = AbilityLedger::get(self::EDIT);
        $keys = [];
        foreach ($row['changed_keys'] as $k) {
            $keys[$k['key']] = [$k['before_sha256'], $k['after_sha256']];
        }
        $this->assertSame([ElementorDocument::KEY_DATA, '_plugin_cache'], array_keys($keys), 'the tree, and the row deleted outside the meta API');
        $this->assertSame([BuilderDocumentRestore::rowsSha256($data0), BuilderDocumentRestore::rowsSha256($this->rowsOf(ElementorDocument::KEY_DATA))], $keys[ElementorDocument::KEY_DATA]);
        $this->assertSame([BuilderDocumentRestore::rowsSha256(['stale']), BuilderDocumentRestore::rowsSha256([])], $keys['_plugin_cache'], 'a deletion counts');
        $this->assertSame(['post_content', 'post_modified_gmt'], array_column($row['changed_fields'], 'field'));
        $this->assertSame([hash('sha256', 'Left column Right column'), hash('sha256', 'Summer sale Right column New words')], [$row['changed_fields'][0]['before_sha256'], $row['changed_fields'][0]['after_sha256']]);
        $this->assertSame([60], $row['own_revision_ids']);
        $this->assertSame(['completed', 'available', 'elementor', '3.35.9', 'classic'], [$row['phase'], $row['undo_state'], $row['builder'], $row['builder_version'], $row['format']]);
        $this->assertSame([$r['before_fp'], $r['after_fp']], [$row['before_fp'], $row['after_fp']]);
        $this->assertCount(2, $row['touched_refs']);

        // The hashes are the ones a person's undo checks and restores by.
        $loaded   = BuilderDocumentSnapshot::load(self::EDIT);
        $snapshot = BuilderDocumentSnapshot::decode((string) $loaded['json'], self::EDIT, self::TARGET);
        $undo     = BuilderDocumentRestore::scoped(self::TARGET, (array) $snapshot, $row['changed_keys'], $row['changed_fields'], (new ElementorAdapter($this->api, self::PRINCIPAL))->descriptor(), new ElementorAdapter($this->api, self::PRINCIPAL));
        $this->assertNull($undo, (string) json_encode($undo));
        $this->assertSame($r['before_fp'], $this->currentFp(), 'the undo puts the page back to its fingerprint before the edit');
    }

    public function test_ledger_result_carries_snapshot_hash(): void
    {
        $r = $this->approvedWrite($this->input(self::ops()));
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $stored = $this->rows->optionRows();
        $this->assertCount(1, $stored);
        $this->assertSame(BuilderDocumentSnapshot::OPTION_PREFIX . self::EDIT, $stored[0]['option_name']);
        $hash = hash('sha256', (string) $stored[0]['option_value']);
        $row  = AbilityLedger::get(self::EDIT);
        $this->assertSame([$hash, $hash, $hash], [$r['snapshot_sha256'], $row['snapshot_sha256'], $row['result']['snapshot_sha256'] ?? null], 'the answer, the row and the stored result all carry the hash of the stored bytes');
        $this->assertSame($r, $row['result']);
        $this->assertSame(strlen((string) $stored[0]['option_value']), $row['snapshot_bytes']);
    }

    public function test_replay_is_idempotent(): void
    {
        $input = $this->input(self::ops());
        $plan  = $this->plan($input);
        $pre   = $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']);
        $this->api->storingDocument(self::TARGET, $this->rows);
        $first = $this->write($input, $pre, $plan['preview_digest']);
        $this->assertTrue($first['ok'] ?? null, (string) json_encode($first));
        $rows = $this->rows->metaRowsOf(self::TARGET);

        // The page moved on, so the same request plans other bytes; a write
        // without the command's replay check refuses before changing anything.
        $again = $this->write($input, $pre, $plan['preview_digest']);
        $this->assertSame('conflict', $again['code'] ?? null, (string) json_encode($again));
        $this->assertSame($rows, $this->rows->metaRowsOf(self::TARGET));
        $this->assertSame($first, AbilityLedger::get(self::EDIT)['result'], 'the recorded result stands');
    }

    public function test_stale_fingerprint_is_conflict(): void
    {
        $stale = $this->input(self::ops(), str_repeat('c', 64));
        $plan  = $this->plan($stale);
        $this->assertSame(['conflict', 'changed_since_read'], [$plan['refusal']['code'] ?? null, $plan['refusal']['detail'] ?? null]);

        // A page changed between the read and the write.
        $input = $this->input(self::ops());
        $ok    = $this->plan($input);
        $pre   = $this->digest($input)($ok['base_fingerprint'], $ok['preview_digest']);
        $this->rows->update($this->rows->posts, ['post_title' => 'Spring!'], ['ID' => self::TARGET]);
        $before = $this->rows->metaRowsOf(self::TARGET);
        $r      = $this->write($input, $pre, $ok['preview_digest']);
        $this->assertSame(['conflict', 'changed_since_read'], [$r['code'] ?? null, $r['detail'] ?? null]);
        $this->assertSame($before, $this->rows->metaRowsOf(self::TARGET));
        $this->assertNull(AbilityLedger::get(self::EDIT));

        // Someone has the page open, or an autosave of it.
        $this->rows->update($this->rows->posts, ['post_title' => 'Spring'], ['ID' => self::TARGET]);
        $this->locks[self::TARGET] = 9;
        $this->assertSame(['conflict', 'editor_open'], self::codeOf($this->plan($input)));
        $this->locks                   = [];
        $this->autosaves[self::TARGET] = 9;
        $this->assertSame(['conflict', 'autosave_pending'], self::codeOf($this->plan($input)));
    }

    public function test_page_changed_before_the_snapshot_is_conflict(): void
    {
        $input = $this->input(self::ops());
        $this->wpdb->beforeSnapshotRead = function (): void {
            $this->rows->update($this->rows->posts, ['post_title' => 'Saved by a person'], ['ID' => self::TARGET]);
        };
        $before = $this->rows->metaRowsOf(self::TARGET);
        $r      = $this->approvedWrite($input);
        $this->assertSame(['conflict', 'changed_since_read'], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame([], $this->api->documents[self::TARGET]->saves, 'Elementor was never asked to save');
        $this->assertSame($before, $this->rows->metaRowsOf(self::TARGET));
        $this->assertSame('Saved by a person', $this->rows->postRow(self::TARGET)['post_title'], 'the person\'s save stands');
        $this->assertSame('failed', AbilityLedger::get(self::EDIT)['phase'] ?? null);
    }

    public function test_page_saved_after_the_snapshot_is_conflict(): void
    {
        $input = $this->input(self::ops());
        $plan  = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $this->api->storingDocument(self::TARGET, $this->rows);

        // Another client's Elementor save lands just after the copy is taken.
        $theirs = self::page();
        $theirs[0]['elements'][0]['elements'][0]['settings']['title'] = 'Saved by a person';
        $landed = false;
        Functions\when('update_option')->alias(function ($name, $value) use ($theirs, &$landed) {
            $this->options[$name] = $value;
            if (!$landed && $name === 'wpmgr_ability_ledger_' . self::EDIT && is_array($value) && ($value['phase'] ?? null) === 'snapshot_taken') {
                $landed = true;
                $this->rows->delete($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => ElementorDocument::KEY_DATA]);
                $this->rows->insert($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => ElementorDocument::KEY_DATA, 'meta_value' => (string) json_encode($theirs)]);
            }

            return true;
        });
        $r = $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);

        $this->assertTrue($landed, 'the other save landed after the snapshot');
        $this->assertSame(['conflict', 'changed_since_read', false], [$r['code'] ?? null, $r['detail'] ?? null, $r['ok'] ?? null], (string) json_encode($r));
        $this->assertSame([], $this->api->documents[self::TARGET]->saves, 'Elementor was never asked to save');
        $this->assertSame([(string) json_encode($theirs)], $this->rowsOf(ElementorDocument::KEY_DATA), 'the other client\'s save stands');
        $row = AbilityLedger::get(self::EDIT);
        $this->assertSame('failed', $row['phase'] ?? null);
        $this->assertArrayNotHasKey('restored', $row, 'nothing was put back over the other save');
        $this->assertSame($r, $row['result']);
        $this->assertSame([], $this->wpdb->claims, 'the target claim is released');
    }

    public function test_tables_without_transactions_refuse_before_any_write(): void
    {
        $input = $this->input(self::ops());
        $plan  = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $pre = $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']);

        $cases = [
            'postmeta on MyISAM'          => ['wp_postmeta', 'MyISAM', 'tables_not_transactional'],
            'posts on Aria'               => ['wp_posts', 'Aria', 'tables_not_transactional'],
            'posts listed with no engine' => ['wp_posts', null, 'tables_not_transactional'],
            'postmeta not listed'         => ['wp_postmeta', false, 'table_engine_unreadable'],
            'the catalogue read fails'    => ['', null, 'table_engine_unreadable'],
        ];
        foreach ($cases as $why => [$table, $engine, $detail]) {
            // A new request on the site: a new database handle, read afresh.
            $this->storePage(self::page());
            if ($table === '') {
                $this->rows->failOn = 'information_schema';
            } elseif ($engine === false) {
                unset($this->rows->tableEngines[$table]);
            } else {
                $this->rows->tableEngines[$table] = $engine;
            }
            $this->assertSame(['builder_not_available', $detail], self::codeOf($this->plan($input)), $why . ': precheck');

            $before = $this->rows->metaRowsOf(self::TARGET);
            $post   = $this->rows->postRow(self::TARGET);
            $this->api->storingDocument(self::TARGET, $this->rows);
            $r = $this->write($input, $pre, $plan['preview_digest']);
            $this->assertSame(['builder_not_available', $detail, false], [$r['code'] ?? null, $r['detail'] ?? null, $r['ok'] ?? null], $why . ': write');
            $this->assertSame([], $this->api->documents[self::TARGET]->saves, $why . ': Elementor was never asked to save');
            $this->assertSame($before, $this->rows->metaRowsOf(self::TARGET), $why);
            $this->assertSame($post, $this->rows->postRow(self::TARGET), $why);
            $this->assertNull(AbilityLedger::get(self::EDIT), $why . ': no ledger row');
            $this->assertSame([], $this->rows->optionRows(), $why . ': no snapshot');
            $this->assertSame([], $this->wpdb->claims, $why . ': the claim is released');
        }
    }

    public function test_table_engines_read_once_per_request(): void
    {
        $input               = $this->input(self::ops());
        $this->rows->queries = [];
        $this->assertArrayNotHasKey('refusal', $this->plan($input));
        $this->assertArrayNotHasKey('refusal', $this->plan($input));
        $reads = array_values(array_filter($this->rows->queries, static fn (array $q): bool => $q['sql'] === FakeBuilderWpdb::ENGINES_SQL));
        $this->assertCount(1, $reads, 'information_schema is read once per request');
        $this->assertSame([$this->rows->posts, $this->rows->postmeta], $reads[0]['args'], 'for the posts and postmeta tables, through prepare()');

        // Within the request the answer stands; a new request reads again.
        $this->rows->tableEngines['wp_postmeta'] = 'MyISAM';
        $this->assertArrayNotHasKey('refusal', $this->plan($input));
        $this->storePage(self::page());
        $this->rows->tableEngines['wp_postmeta'] = 'MyISAM';
        $this->assertSame(['builder_not_available', 'tables_not_transactional'], self::codeOf($this->plan($input)));
    }

    public function test_ineligible_target_single_code(): void
    {
        $input = $this->input(self::ops());
        $cases = [
            'not in the signed list' => [fn () => null, [], 'not_in_signed_list'],
            'another post listed'    => [fn () => null, [self::OTHER], 'not_in_signed_list'],
            'no marker'              => [fn () => $this->rows->delete($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => DraftEligibility::MARKER_KEY]), [self::TARGET], 'no_marker'],
            'the creation is trashed' => [fn () => $this->options['wpmgr_ability_ledger_' . self::CREATE]['undo_state'] = 'trashed', [self::TARGET], 'trashed'],
            'another post\'s marker'  => [fn () => $this->options['wpmgr_ability_ledger_' . self::CREATE]['created_post_id'] = self::OTHER, [self::TARGET], 'ledger_other_post'],
            'published'              => [fn () => $this->rows->update($this->rows->posts, ['post_status' => 'publish'], ['ID' => self::TARGET]), [self::TARGET], 'not_draft'],
            'not an Elementor page'  => [fn () => $this->rows->delete($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => ElementorDocument::KEY_EDIT_MODE]), [self::TARGET], 'not_builder_page'],
            'the front page'         => [fn () => $this->options['page_on_front'] = self::TARGET, [self::TARGET], 'front_page'],
            'the active kit'         => [fn () => $this->api->activeKitId = self::TARGET, [self::TARGET], 'active_kit'],
        ];
        foreach ($cases as $why => [$arrange, $allowed, $detail]) {
            $this->storePage(self::page());
            $this->api->activeKitId = self::KIT;
            unset($this->options['page_on_front']);
            $arrange();
            $plan = BuilderPageEdit::plan($input, $this->entry(), $allowed, self::EDIT, self::noMedia(), $this->seam());
            $this->assertSame(['target_not_eligible', $detail], self::codeOf($plan), $why);
            $this->assertSame(['ok', 'outcome', 'code', 'detail', 'retryable'], array_keys($plan['refusal']), $why . ': the detail is the one token, nothing from the site');
        }
        $missing = BuilderPageEdit::plan((string) json_encode(['post_id' => 999, 'base_fingerprint' => str_repeat('a', 64), 'operations' => self::ops()]), $this->entry(), [999], self::EDIT, self::noMedia(), $this->seam());
        $this->assertSame(['target_not_eligible', 'missing'], self::codeOf($missing));
    }

    public function test_dispatch_list_is_checked_again_at_write(): void
    {
        $input = $this->input(self::ops());
        $plan  = $this->plan($input);
        $pre   = $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']);
        $rows  = $this->rows->metaRowsOf(self::TARGET);

        // The control plane names no draft at dispatch (the creation was
        // undone after the approval): the write refuses, nothing changes.
        $r = BuilderPageEdit::write($input, $this->entry(), [], self::EDIT, self::ENTRY_SHA, $pre, $plan['preview_digest'], $this->digest($input), self::noMedia(), $this->seam());
        $this->assertSame(['target_not_eligible', 'not_in_signed_list'], [$r['code'] ?? null, $r['detail'] ?? null]);
        $this->assertSame($rows, $this->rows->metaRowsOf(self::TARGET));
        $this->assertNull(AbilityLedger::get(self::EDIT));
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Two operations on the golden page: the heading's text, and a new
     * paragraph after the text.
     *
     * @return list<array<string,mixed>>
     */
    private static function ops(): array
    {
        return [
            ['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Summer sale'],
            ['op' => 'insert', 'after' => self::TEXT, 'outline' => [['type' => 'paragraph', 'text' => 'New words']]],
        ];
    }

    /**
     * The golden two-column page, with a colour a person set on its heading.
     *
     * @return list<array<string,mixed>>
     */
    private static function page(): array
    {
        $fixture = json_decode((string) file_get_contents(self::FIXTURE), true, 512, JSON_THROW_ON_ERROR);
        foreach ($fixture['cases'] as $case) {
            if ($case['name'] === 'columns') {
                $tree = $case['tree'];
                $tree[0]['elements'][0]['elements'][0]['settings']['title_color'] = self::PERSON_COLOR;

                return $tree;
            }
        }
        throw new \LogicException('the columns golden is missing');
    }

    /**
     * Fresh rows: the target, WPMgr's Elementor draft holding $tree and
     * created by CREATE, then the active kit; $extra are more target rows.
     *
     * @param list<array<string,mixed>>         $tree  The stored tree.
     * @param list<array{0:string,1:string}>    $extra More target rows.
     */
    private function storePage(array $tree, array $extra = []): void
    {
        $this->rows      = new FakeBuilderWpdb();
        $this->wpdb      = new EngineWpdb($this->rows);
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->rows->addPost(self::TARGET, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => 'Spring',
            'post_content'      => 'Left column Right column',
            'post_author'       => (string) self::PRINCIPAL,
            'post_modified_gmt' => '2026-10-09 07:00:00',
        ]);
        $this->rows->addMeta(1, self::TARGET, ElementorDocument::MARKER_KEY, self::CREATE);
        $this->rows->addMeta(2, self::TARGET, ElementorDocument::KEY_EDIT_MODE, ElementorDocument::EDIT_MODE_BUILDER);
        $this->rows->addMeta(3, self::TARGET, ElementorDocument::KEY_TEMPLATE_TYPE, ElementorDocument::TEMPLATE_PAGE);
        $this->rows->addMeta(4, self::TARGET, ElementorDocument::KEY_DATA, (string) json_encode($tree));
        $this->rows->addMeta(5, self::TARGET, '_elementor_version', '3.35.9');
        $this->rows->addPost(self::KIT, ['post_type' => 'elementor_library', 'post_status' => 'publish', 'post_title' => 'Kit', 'post_content' => '', 'post_modified_gmt' => '2026-10-09 07:00:00']);
        $this->rows->addMeta(6, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->rows->addMeta(7, self::KIT, ElementorDocument::KEY_DATA, '[]');
        $id = 100;
        foreach ($extra as [$key, $value]) {
            $this->rows->addMeta(++$id, self::TARGET, $key, $value);
        }
        $this->options = [
            'wpmgr_ability_ledger_' . self::CREATE => [
                'request_id'      => self::CREATE,
                'ability'         => OwnAbilities::NAME_PAGE_CREATE,
                'phase'           => 'completed',
                'created_post_id' => self::TARGET,
                'undo_state'      => 'available',
                'builder'         => 'elementor',
            ],
        ];
        $this->api->documents = [];
        $this->api->addDocument(self::TARGET);
    }

    /** The page-edit entry, decoded as the command decodes it. */
    private function entry(): object
    {
        return (object) json_decode((string) json_encode([
            'name'          => OwnAbilities::NAME_PAGE_EDIT,
            'source'        => 'wpmgr',
            'class'         => 'write',
            'status'        => 'admitted',
            'enabled'       => true,
            'approval_mode' => 'per_call',
            'snapshot'      => 'builder_document',
            'limits'        => ['builders_enabled' => ['elementor']],
        ]), false);
    }

    /**
     * @param list<array<string,mixed>> $ops Operations.
     */
    private function input(array $ops, ?string $fp = null): string
    {
        return (string) json_encode(['post_id' => self::TARGET, 'base_fingerprint' => $fp ?? $this->currentFp(), 'operations' => $ops]);
    }

    private function currentFp(): string
    {
        return (string) BuilderDocumentFingerprint::ofPost(self::TARGET, ElementorDocument::DESCRIPTOR_KEYS);
    }

    /**
     * @return array<string, ElementorAdapter>
     */
    private function seam(): array
    {
        return ['elementor' => new ElementorAdapter($this->api, self::PRINCIPAL)];
    }

    /**
     * @return array<string,mixed>
     */
    private function plan(string $input): array
    {
        return BuilderPageEdit::plan($input, $this->entry(), [self::TARGET], self::EDIT, self::noMedia(), $this->seam());
    }

    /** The command's precheck digest for this input. */
    private function digest(string $input): \Closure
    {
        $inputSha = hash('sha256', $input);

        return static fn (string $base, string $preview): string => hash('sha256', (string) json_encode([self::ENTRY_SHA, $inputSha, $base, $preview]));
    }

    /**
     * @return array<string,mixed>
     */
    private function write(string $input, string $expPre, string $expPrev): array
    {
        return BuilderPageEdit::write($input, $this->entry(), [self::TARGET], self::EDIT, self::ENTRY_SHA, $expPre, $expPrev, $this->digest($input), self::noMedia(), $this->seam());
    }

    /**
     * Precheck, then write with the approved digests into a document that
     * stores the elements as Elementor does, through $filter, after $during.
     *
     * @param (\Closure(list<mixed>): list<mixed>)|null $filter Changes the saved elements.
     * @param (\Closure(): void)|null                  $during Runs inside the save, before the rows are stored.
     * @return array<string,mixed>
     */
    private function approvedWrite(string $input, ?\Closure $filter = null, ?\Closure $during = null): array
    {
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $document = $this->api->storingDocument(self::TARGET, $this->rows, $filter);
        if ($during !== null) {
            $store            = $document->onSave;
            $document->onSave = static function ($data) use ($store, $during) {
                $during();

                return $store($data);
            };
        }
        $this->watchLedger();

        return $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);
    }

    /** @var list<string> */
    private array $phases = [];

    /** Records in $phases the phase of every ledger update of EDIT, in order. */
    private function watchLedger(): void
    {
        $this->phases = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            if ($name === 'wpmgr_ability_ledger_' . self::EDIT && is_array($value)) {
                $this->phases[] = (string) ($value['phase'] ?? '');
            }

            return true;
        });
    }

    /**
     * The ledger phases of EDIT, the created row's first.
     *
     * @return list<string>
     */
    private function ledgerPhases(): array
    {
        return array_merge(['started'], $this->phases);
    }

    /**
     * One key's stored rows of the target, in meta_id order.
     *
     * @return list<string|null>
     */
    private function rowsOf(string $key): array
    {
        $out = [];
        foreach ($this->rows->metaRowsOf(self::TARGET) as $row) {
            if ($row['meta_key'] === $key) {
                $out[] = $row['meta_value'];
            }
        }

        return $out;
    }

    /**
     * Rows as [key, value] pairs, meta_ids left out (a restore inserts new ones).
     *
     * @param list<array{meta_id:int,meta_key:string,meta_value:string|null}> $rows Rows.
     * @return list<array{0:string,1:string|null}>
     */
    private static function pairs(array $rows): array
    {
        return array_map(static fn (array $r): array => [$r['meta_key'], $r['meta_value']], $rows);
    }

    /**
     * @param array<string,mixed> $plan A plan() answer.
     * @return array{0:mixed,1:mixed}
     */
    private static function codeOf(array $plan): array
    {
        return [$plan['refusal']['code'] ?? null, $plan['refusal']['detail'] ?? null];
    }

    /** A media resolver that is never needed: the operations add no image. */
    private static function noMedia(): \Closure
    {
        return static fn (array $ids): array => $ids === [] ? ['facts' => []] : ['refusal' => ['ok' => false, 'code' => 'image_not_available', 'detail' => 'none here']];
    }

    /**
     * Run the captured callbacks for $tag in priority order.
     *
     * @param mixed ...$args The hook's arguments.
     */
    private function fire(string $tag, mixed ...$args): void
    {
        $rows = array_values(array_filter($this->hooks, static fn ($r) => $r[0] === $tag));
        usort($rows, static fn ($a, $b) => $a[2] <=> $b[2]);
        foreach ($rows as [, $callback, , $accepted]) {
            $callback(...array_slice($args, 0, $accepted));
        }
    }
}
