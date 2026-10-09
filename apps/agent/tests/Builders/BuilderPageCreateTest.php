<?php
/**
 * BuilderPageCreate: the builder precheck and its digests, the write of an
 * Elementor draft, the trash on every failure after the insert, and the undo
 * guard of a builder draft.
 *
 * Elementor is FakeElementorApi behind the real ElementorAdapter, so the
 * mapper, the tree prechecks, the write scope and the read-back are the
 * production classes. The stand-in document's save() stores the tree the way
 * Elementor's save does on a draft: the post row is updated, one revision is
 * inserted, and the tree and version rows are added, each firing the hook
 * core fires. Rows live in FakeBuilderWpdb and are read back through the same
 * SQL reads production uses.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageCreate
 */
final class BuilderPageCreateTest extends TestCase
{
    private const CONTAINERS = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    private const SECTIONS = __DIR__ . '/../fixtures/ability-run/elementor-classic-sections.json';

    /** The id the next inserted post gets. */
    private const NEW_ID = 41;

    /** The revision Elementor's save makes of the new draft. */
    private const REVISION = 90;

    private const KIT = 4;

    private const OTHER = 77;

    private const PRINCIPAL = 2;

    private const TITLE = 'Our services & more';

    /** @var list<array{0:string,1:callable,2:int,3:int}> Captured add_filter calls: tag, callback, priority, accepted args. */
    private array $hooks = [];

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<int,array<string,string>> Post rows by id, as stored. */
    private array $rows = [];

    /** @var array<int,int> Post author by id. */
    private array $authors = [];

    /** @var array<int,string> Library alt text by attachment id. */
    private array $alts = [];

    /** @var list<array<string,mixed>> wp_insert_post() arguments, in order. */
    private array $inserts = [];

    /** @var list<string> What happened, in order. */
    private array $events = [];

    /** @var array<string,mixed> The ledger row as the updates left it. */
    private array $ledgerRow = [];

    private bool $ledgerOk = true;

    /** @var array<int,list<object>> Revisions by parent id. */
    private array $revisions = [];

    /** @var array<int,true> Posts with an autosave. */
    private array $autosaves = [];

    /** @var array<int,int> Edit lock holder by post id. */
    private array $locks = [];

    private FakeBuilderWpdb $wpdb;

    private FakeElementorApi $api;

    private int $metaId = 100;

    /** @var mixed */
    private $savedWpdb;

    private bool $hadWpdb = false;

    /** @var string|false */
    private $locale = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

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
        $slash = static function ($value) use (&$slash) {
            if (is_array($value)) {
                return array_map($slash, $value);
            }

            return is_string($value) ? addslashes($value) : $value;
        };
        Functions\when('add_filter')->alias($capture);
        Functions\when('add_action')->alias($capture);
        Functions\when('remove_filter')->alias($release);
        Functions\when('remove_action')->alias($release);
        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('get_userdata')->justReturn(false);
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wc_get_page_id')->justReturn(-1);
        Functions\when('wp_slash')->alias($slash);
        Functions\when('get_post_meta')->alias(fn ($id, $key = '', $single = false) => $key === BuilderPageCreate::ALT_META_KEY ? ($this->alts[(int) $id] ?? '') : '');
        Functions\when('get_post')->alias(function ($id) {
            $id = (int) $id;
            if (!isset($this->rows[$id])) {
                return null;
            }

            return (object) (['ID' => $id, 'post_author' => (string) ($this->authors[$id] ?? 0), 'post_parent' => 0] + $this->rows[$id]);
        });
        Functions\when('wp_insert_post')->alias(function (array $data, $wpError = false) {
            $this->events[]  = 'insert';
            $this->inserts[] = $data;
            $id              = self::NEW_ID;
            $this->rows[$id] = [
                'post_type'         => (string) $data['post_type'],
                'post_status'       => (string) $data['post_status'],
                'post_title'        => stripslashes((string) $data['post_title']),
                'post_content'      => stripslashes((string) $data['post_content']),
                'post_modified_gmt' => '2026-10-09 07:00:00',
            ];
            $this->authors[$id] = (int) $data['post_author'];
            $this->wpdb->addPost($id, $this->rows[$id]);
            foreach ((array) ($data['meta_input'] ?? []) as $key => $value) {
                $this->wpdb->addMeta(++$this->metaId, $id, (string) $key, stripslashes((string) $value));
            }

            return $id;
        });
        Functions\when('wp_trash_post')->alias(function ($id) {
            $id             = (int) $id;
            $this->events[] = 'trash';
            if (isset($this->rows[$id])) {
                $this->rows[$id]['post_status'] = 'trash';
                $this->wpdb->addPost($id, $this->rows[$id]);
            }

            return true;
        });
        Functions\when('wp_get_post_autosave')->alias(fn ($id, $user = 0) => isset($this->autosaves[(int) $id]) ? (object) ['ID' => 500] : false);
        Functions\when('wp_get_post_revisions')->alias(function ($id, $args = null) {
            $out = [];
            foreach ($this->revisions[(int) $id] ?? [] as $revision) {
                $out[$revision->ID] = $revision;
            }

            return $out;
        });
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);

        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
        $this->wpdb      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;
        unset($GLOBALS['_wp_switched_stack']);
        $this->locale = setlocale(LC_NUMERIC, '0');

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->wpdb->addPost(self::KIT, [
            'post_type'         => 'elementor_library',
            'post_status'       => 'publish',
            'post_title'        => 'Kit',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-01 07:00:00',
        ]);
        $this->wpdb->addMeta(2, self::KIT, '_elementor_page_settings', 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->wpdb->addMeta(3, self::KIT, '_elementor_data', '[]');
    }

    protected function tear_down(): void
    {
        if (is_string($this->locale)) {
            setlocale(LC_NUMERIC, $this->locale);
        }
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->savedWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_precheck_preview_and_digest_are_deterministic(): void
    {
        [$outline, $golden, $request] = self::golden(self::CONTAINERS, 'columns');
        $spec                         = self::spec($outline);

        $first  = $this->precheck($spec, $request);
        $second = $this->precheck($spec, $request);

        $this->assertArrayNotHasKey('refusal', $first, json_encode($first['refusal'] ?? null));
        $this->assertSame(['post_type', 'editor', 'format', 'elementor_version', 'layout', 'status', 'title', 'tree'], array_keys($first['preview']));
        $this->assertSame(
            ['post_type' => 'page', 'editor' => 'builder:elementor', 'format' => 'classic', 'elementor_version' => '3.35.9', 'layout' => 'containers', 'status' => 'draft', 'title' => self::TITLE],
            array_diff_key($first['preview'], ['tree' => 1])
        );
        $this->assertSame($golden, $first['preview']['tree'], 'the tree is the golden Elementor stored for this outline and request');
        $this->assertSame($first['preview'], $second['preview']);
        $this->assertSame($first['preview_digest'], $second['preview_digest']);
        $this->assertSame(
            hash('sha256', (string) json_encode(['builder:elementor', 'classic', '3.35.9', 'page', 'draft', self::TITLE, json_encode($golden)])),
            $first['preview_digest'],
            'preview_digest = sha256(json_encode([editor, format, elementor_version, post_type, "draft", title, tree_json]))'
        );
        $this->assertSame(PageCreateBuilder::baseFingerprint('page', []), $first['base_fingerprint']);

        // The site default and an explicit classic build the same page.
        foreach (['site_default', 'classic'] as $format) {
            $this->assertSame($first['preview_digest'], $this->precheck($spec + ['elementor_format' => $format], $request)['preview_digest'], $format);
        }

        // Another request, another version or another title is another digest.
        $this->assertNotSame($first['preview_digest'], $this->precheck($spec, '99999999-2222-4333-8444-777777777777')['preview_digest']);
        $this->assertNotSame($first['preview_digest'], $this->precheck(['title' => 'Other'] + $spec, $request)['preview_digest']);
        $this->api->version = '3.35.10';
        $this->assertNotSame($first['preview_digest'], $this->precheck($spec, $request)['preview_digest']);

        // Containers off: sections, as the sections golden.
        $this->api->version                   = '3.35.9';
        $this->api->experiments['container'] = false;
        [$outline, $golden, $request]         = self::golden(self::SECTIONS, 'columns');
        $sections                             = $this->precheck(self::spec($outline), $request);
        $this->assertSame('sections', $sections['preview']['layout'] ?? null);
        $this->assertSame($golden, $sections['preview']['tree'] ?? null);
        $this->assertSame([], $this->hooks, 'the precheck leaves nothing installed');
    }

    public function test_precheck_and_write_digests_match(): void
    {
        [$outline, $golden, $request, $media] = self::golden(self::CONTAINERS, 'mixed-top-level');
        $spec                                 = self::spec($outline);
        $facts                                = [self::mediaFact(7, $media[7]['url'])];

        // The precheck call, then the write call's re-check: separate
        // requests to the agent, so nothing is shared but the request id.
        $approved = $this->precheck($spec, $request, $facts);
        $recheck  = $this->precheck($spec, $request, $facts);
        $this->assertArrayNotHasKey('refusal', $approved, json_encode($approved['refusal'] ?? null));
        $this->assertSame($golden, $approved['preview']['tree']);
        $this->assertSame($approved['preview_digest'], $recheck['preview_digest'], 'the write builds exactly what was approved');
        $this->assertSame($approved['base_fingerprint'], $recheck['base_fingerprint']);

        $this->elementorSaves();
        $result = $this->write($spec, $request, $recheck);

        $this->assertTrue($result['ok'] ?? null, (string) ($result['detail'] ?? ''));
        $this->assertSame($approved['preview_digest'], $result['preview_digest']);
        $stored = BuilderDocumentFingerprint::read(self::NEW_ID, [ElementorDocument::KEY_DATA]);
        $this->assertSame($approved['preview']['tree'], json_decode($stored['rows'][ElementorDocument::KEY_DATA][0] ?? '', true), 'the stored tree is the approved one');
    }

    public function test_alt_read_from_library(): void
    {
        [$outline, $golden, $request, $media] = self::golden(self::CONTAINERS, 'image');
        $facts                                = [self::mediaFact(5, $media[5]['url'])];
        $this->alts[5]                        = 'Library alt text';
        $asked                                = [];
        Functions\when('get_post_meta')->alias(function ($id, $key = '', $single = false) use (&$asked) {
            $asked[] = [$id, $key, $single];

            return $key === BuilderPageCreate::ALT_META_KEY ? ($this->alts[(int) $id] ?? '') : '';
        });

        $ok = $this->precheck(self::spec($outline), $request, $facts);
        $this->assertArrayNotHasKey('refusal', $ok);
        $this->assertSame([[5, '_wp_attachment_image_alt', true]], $asked, 'the alt is read from the media library, one value');
        $this->assertSame($golden, $ok['preview']['tree']);
        $this->assertSame($facts, $ok['preview']['media'], 'the preview carries the image facts as resolved');
        $this->assertSame(PageCreateBuilder::baseFingerprint('page', $facts), $ok['base_fingerprint'], 'every image fact is bound');

        // The page shows the library alt, so an alt that differs is refused.
        $this->alts[5] = 'Changed in the library';
        $refused       = $this->precheck(self::spec($outline), $request, $facts);
        $this->assertSame('image_alt_from_library', $refused['refusal']['code'] ?? null);
        $this->assertSame('', $refused['preview_digest']);

        // No alt in the library is the empty alt.
        unset($this->alts[5]);
        $outline[0]['alt'] = '';
        $this->assertArrayNotHasKey('refusal', $this->precheck(self::spec($outline), $request, $facts));
    }

    public function test_atomic_refused_in_a1(): void
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'heading');
        $this->api->experiments['e_atomic_elements'] = true;

        $refused = $this->precheck(self::spec($outline) + ['elementor_format' => 'atomic'], $request);

        $this->assertSame(['code' => 'builder_not_available', 'detail' => 'atomic_unavailable'], ['code' => $refused['refusal']['code'] ?? null, 'detail' => $refused['refusal']['detail'] ?? null]);
        $this->assertFalse($refused['refusal']['ok']);
        $this->assertNull($refused['doc']);
        foreach ($this->api->calls as [$method]) {
            $this->assertNotSame('createElementInstance', $method, 'nothing is built for a refused format');
        }

        $bogus = $this->precheck(self::spec($outline) + ['elementor_format' => 'v4'], $request);
        $this->assertSame('bad_input', $bogus['refusal']['code'] ?? null);

        // Elementor's own refusals come through unchanged.
        $this->api->loaded = false;
        $this->assertSame('elementor_missing', $this->precheck(self::spec($outline), $request)['refusal']['detail'] ?? null);
    }

    public function test_insert_carries_marker_edit_mode_and_template_type(): void
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'heading');
        foreach (['page' => 'wp-page', 'post' => 'wp-post'] as $postType => $template) {
            $this->inserts = [];
            $this->rows    = [];
            $this->resetWpdb();
            $spec  = ['post_type' => $postType] + self::spec($outline);
            $built = $this->precheck($spec, $request);
            $this->elementorSaves($postType === 'page' ? 'wp-page' : 'wp-post');

            $result = $this->write($spec, $request, $built);

            $this->assertTrue($result['ok'] ?? null, $postType . ': ' . (string) ($result['detail'] ?? ''));
            $this->assertSame([[
                'post_type'    => $postType,
                'post_status'  => 'draft',
                'post_author'  => self::PRINCIPAL,
                'post_title'   => 'Our services &amp; more',
                'post_content' => '',
                'meta_input'   => [
                    '_wpmgr_created_by_request' => $request,
                    '_elementor_edit_mode'      => 'builder',
                    '_elementor_template_type'  => $template,
                ],
            ]], $this->inserts, $postType);
        }
    }

    public function test_ledger_post_created_before_save(): void
    {
        Functions\expect('update_post_meta')->never();
        Functions\expect('add_post_meta')->never();
        Functions\expect('update_metadata')->never();
        [$outline, , $request] = self::golden(self::CONTAINERS, 'columns');
        $spec                  = self::spec($outline);
        $built                 = $this->precheck($spec, $request);
        $document              = $this->elementorSaves();

        $result = $this->write($spec, $request, $built);

        $this->assertTrue($result['ok'] ?? null, (string) ($result['detail'] ?? ''));
        $this->assertSame(['insert', 'ledger:post_created', 'save', 'ledger:completed'], $this->events);
        $this->assertSame(self::NEW_ID, $this->ledgerRow['created_post_id'] ?? null);
        $this->assertSame([['elements' => $built['tree']]], $document->saves, 'the tree is stored by Elementor\'s own save, once');
    }

    public function test_verify_failure_trashes_draft(): void
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'columns');
        $spec                  = self::spec($outline);
        $built                 = $this->precheck($spec, $request);
        // A filter on the save drops a node nobody asked to change.
        $this->elementorSaves('wp-page', static function (array $tree): array {
            array_pop($tree[0]['elements']);

            return $tree;
        });

        $result = $this->write($spec, $request, $built);

        $this->assertSame(['ok' => false, 'code' => 'verify_mismatch', 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result));
        $this->assertSame('the created draft is not what was built: tree_differs', $result['detail']);
        $this->assertSame('trash', $this->rows[self::NEW_ID]['post_status']);
        $this->assertSame(['phase' => 'failed', 'undo_state' => 'trashed'], array_intersect_key($this->ledgerRow, ['phase' => 1, 'undo_state' => 1]));
        $this->assertSame($result, $this->ledgerRow['result']);
    }

    public function test_side_effect_trashes_draft(): void
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'heading');
        $spec                  = self::spec($outline);
        $built                 = $this->precheck($spec, $request);
        $document              = $this->elementorSaves();
        $store                 = $document->onSave;
        $document->onSave      = function ($data) use ($store) {
            $this->fire('added_post_meta', 9, self::OTHER, '_thumbnail_id', '5');

            return $store($data);
        };

        $result = $this->write($spec, $request, $built);

        $this->assertSame(['ok' => false, 'code' => 'side_effect_detected', 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result));
        $this->assertSame('trash', $this->rows[self::NEW_ID]['post_status']);
        $this->assertSame('failed', $this->ledgerRow['phase'] ?? null);
        $this->assertArrayNotHasKey('after_fp', $this->ledgerRow);
    }

    public function test_save_refused_trashes_draft(): void
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'heading');
        $spec                  = self::spec($outline);
        $cases                 = [
            'refused' => ['builder_save_refused', static fn (): bool => false],
            'crashed' => ['builder_crashed', static function (): bool {
                throw new \RuntimeException('invalid prop');
            }],
        ];
        foreach ($cases as $name => [$code, $onSave]) {
            $this->rows      = [];
            $this->ledgerRow = [];
            $this->resetWpdb();
            $built            = $this->precheck($spec, $request);
            $document         = $this->api->addDocument(self::NEW_ID);
            $document->onSave = $onSave;

            $result = $this->write($spec, $request, $built);

            $this->assertSame(['ok' => false, 'code' => $code, 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result), $name);
            $this->assertSame('trash', $this->rows[self::NEW_ID]['post_status'], $name);
            $this->assertSame('trashed', $this->ledgerRow['undo_state'] ?? null, $name);
        }

        // Elementor will not let the service user edit the new page.
        $this->rows = [];
        $this->resetWpdb();
        $built    = $this->precheck($spec, $request);
        $document = $this->api->addDocument(self::NEW_ID, 'wp-page', false);
        $result   = $this->write($spec, $request, $built);
        $this->assertSame(['ok' => false, 'code' => 'builder_not_available', 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result));
        $this->assertSame('role_excluded', $result['detail']);
        $this->assertSame([], $document->saves, 'nothing is saved on a page the user may not edit');
    }

    public function test_after_fp_is_builder_document_v1(): void
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'columns');
        $spec                  = self::spec($outline);
        $built                 = $this->precheck($spec, $request);
        $this->elementorSaves();

        $result = $this->write($spec, $request, $built);

        $expected = BuilderDocumentFingerprint::ofPost(self::NEW_ID, ElementorDocument::DESCRIPTOR_KEYS);
        $this->assertIsString($expected);
        $this->assertSame($expected, $result['after_fp'] ?? null);
        $this->assertNotSame(PageCreateBuilder::documentFingerprint((object) $this->rows[self::NEW_ID]), $result['after_fp']);
        $this->assertSame([
            'ok'                => true,
            'outcome'           => 'created',
            'mode'              => 'write',
            'ability'           => 'wpmgr/page-create',
            'request_id'        => $request,
            'post_id'           => self::NEW_ID,
            'post_type'         => 'page',
            'editor'            => 'builder:elementor',
            'format'            => 'classic',
            'elementor_version' => '3.35.9',
            'status'            => 'draft',
            'preview_digest'    => $built['preview_digest'],
            'after_fp'          => $expected,
            'verify'            => ['tree_equal' => true, 'status' => 'draft'],
        ], $result);
        $this->assertSame([
            'phase'            => 'completed',
            'after_fp'         => $expected,
            'undo_state'       => 'available',
            'builder'          => 'elementor',
            'builder_version'  => '3.35.9',
            'format'           => 'classic',
            'own_revision_ids' => [self::REVISION],
            'result'           => $result,
        ], array_diff_key($this->ledgerRow, ['created_post_id' => 1]));
    }

    public function test_revert_guard_allows_own_revisions_only(): void
    {
        $row = $this->createdDraft();
        $this->assertNull(BuilderPageCreate::revertProblem(self::NEW_ID, $row), 'the draft as created, with its own revision');

        // A revision the creation did not make.
        $this->revisions[self::NEW_ID][] = (object) ['ID' => 91];
        $this->assertSame('this draft has revisions its creation did not make; open it in WordPress', BuilderPageCreate::revertProblem(self::NEW_ID, $row));
        array_pop($this->revisions[self::NEW_ID]);
        $this->assertNull(BuilderPageCreate::revertProblem(self::NEW_ID, $row));

        // An autosave, an edit lock.
        $this->autosaves[self::NEW_ID] = true;
        $this->assertSame('someone has unsaved changes to this draft; open it in WordPress', BuilderPageCreate::revertProblem(self::NEW_ID, $row));
        unset($this->autosaves[self::NEW_ID]);
        $this->locks[self::NEW_ID] = 3;
        $this->assertSame('someone is editing this draft right now', BuilderPageCreate::revertProblem(self::NEW_ID, $row));
        unset($this->locks[self::NEW_ID]);

        // The recorded revisions must be a list of post ids.
        foreach ([null, 'x', [0], ['90'], ['a' => 90]] as $own) {
            $this->assertSame('the created draft has no record of its own revisions', BuilderPageCreate::revertProblem(self::NEW_ID, ['own_revision_ids' => $own] + $row), var_export($own, true));
        }
        $this->assertNull(BuilderPageCreate::revertProblem(self::NEW_ID, $row));
    }

    public function test_revert_guard_refuses_changed_fingerprint(): void
    {
        $row = $this->createdDraft();
        $this->assertNull(BuilderPageCreate::revertProblem(self::NEW_ID, $row));

        // A person saved the page in Elementor: the tree row changed.
        $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_DATA, '[]');
        $this->assertSame('someone edited this draft after it was created', BuilderPageCreate::revertProblem(self::NEW_ID, $row));

        // Page settings appeared.
        $this->resetWpdb();
        $row = $this->createdDraft();
        $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_PAGE_SETTINGS, 'a:0:{}');
        $this->assertSame('someone edited this draft after it was created', BuilderPageCreate::revertProblem(self::NEW_ID, $row));

        // Without a builder this agent checks, or a recorded fingerprint, nothing is trashed.
        $this->assertSame('this draft was built with a page builder this agent cannot check', BuilderPageCreate::revertProblem(self::NEW_ID, ['builder' => 'beaver'] + $row));
        $this->assertSame('the created draft has no recorded fingerprint', BuilderPageCreate::revertProblem(self::NEW_ID, ['after_fp' => ''] + $row));
        $this->assertSame('the created draft no longer exists', BuilderPageCreate::revertProblem(self::OTHER, $row));
    }

    /**
     * Precheck $spec as the service user's request, through a fresh adapter.
     *
     * @param array<string,mixed>       $spec       Spec.
     * @param string                    $request    Request id.
     * @param list<array<string,mixed>> $mediaFacts Resolved image facts.
     * @return array<string,mixed>
     */
    private function precheck(array $spec, string $request, array $mediaFacts = []): array
    {
        $adapter = new ElementorAdapter($this->api, self::PRINCIPAL);

        return BuilderPageCreate::precheck($spec, $adapter, $adapter->facts(), $mediaFacts, $request);
    }

    /**
     * Write $built through a fresh adapter, with a ledger that records every
     * update.
     *
     * @param array<string,mixed> $spec    Spec.
     * @param string              $request Request id.
     * @param array<string,mixed> $built   precheck() answer.
     * @return array<string,mixed>
     */
    private function write(array $spec, string $request, array $built): array
    {
        $ledger = function (array $fields): bool {
            $this->events[]  = 'ledger:' . (string) ($fields['phase'] ?? '');
            $this->ledgerRow = array_merge($this->ledgerRow, $fields);

            return $this->ledgerOk;
        };

        return BuilderPageCreate::write(self::PRINCIPAL, $spec, new ElementorAdapter($this->api, self::PRINCIPAL), $request, $built, $ledger);
    }

    /**
     * A created draft, and its ledger row as the write left it.
     *
     * @return array<string,mixed>
     */
    private function createdDraft(): array
    {
        [$outline, , $request] = self::golden(self::CONTAINERS, 'columns');
        $spec                  = self::spec($outline);
        $this->rows            = [];
        $this->ledgerRow       = [];
        $built                 = $this->precheck($spec, $request);
        $this->elementorSaves();
        $result = $this->write($spec, $request, $built);
        $this->assertTrue($result['ok'] ?? null, (string) ($result['detail'] ?? ''));

        return $this->ledgerRow;
    }

    /**
     * Give the next draft a stand-in Elementor document whose save() stores
     * the tree as Elementor's does on a draft. $alter changes the tree first,
     * as a filter on the save would.
     *
     * @param string        $name  What get_name() answers.
     * @param callable|null $alter Tree => stored tree.
     * @return object The document.
     */
    private function elementorSaves(string $name = 'wp-page', ?callable $alter = null): object
    {
        $document         = $this->api->addDocument(self::NEW_ID, $name);
        $document->onSave = function ($data) use ($alter): bool {
            $this->events[] = 'save';
            $tree           = $data['elements'];
            if ($alter !== null) {
                $tree = $alter($tree);
            }
            $json = json_encode($tree, JSON_THROW_ON_ERROR);
            $this->rows[self::NEW_ID]['post_modified_gmt'] = '2026-10-09 07:00:05';
            $this->wpdb->addPost(self::NEW_ID, $this->rows[self::NEW_ID]);
            $this->fire('wp_insert_post', self::NEW_ID, (object) ['ID' => self::NEW_ID, 'post_type' => 'page', 'post_parent' => 0], true);
            $this->fire('wp_insert_post', self::REVISION, (object) ['ID' => self::REVISION, 'post_type' => 'revision', 'post_parent' => self::NEW_ID], false);
            $this->revisions[self::NEW_ID] = [(object) ['ID' => self::REVISION]];
            foreach ([[self::NEW_ID, ElementorDocument::KEY_DATA, $json], [self::NEW_ID, '_elementor_version', '3.35.9'], [self::REVISION, ElementorDocument::KEY_DATA, $json]] as [$id, $key, $value]) {
                $this->wpdb->addMeta(++$this->metaId, $id, $key, $value);
                $this->fire('added_post_meta', $this->metaId, $id, $key, $value);
            }

            return true;
        };

        return $document;
    }

    /**
     * Start the rows again: only the kit.
     */
    private function resetWpdb(): void
    {
        $this->wpdb      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->wpdb->addPost(self::KIT, [
            'post_type'         => 'elementor_library',
            'post_status'       => 'publish',
            'post_title'        => 'Kit',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-01 07:00:00',
        ]);
        $this->wpdb->addMeta(2, self::KIT, '_elementor_page_settings', 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->wpdb->addMeta(3, self::KIT, '_elementor_data', '[]');
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

    /**
     * A golden case: its outline as page-create validates it, its tree, the
     * request id and the media.
     *
     * @return array{0: list<array<string,mixed>>, 1: list<array<string,mixed>>, 2: string, 3: array<int,array<string,string>>}
     */
    private static function golden(string $file, string $name): array
    {
        $fixture = json_decode((string) file_get_contents($file), true, 512, JSON_THROW_ON_ERROR);
        foreach ($fixture['cases'] as $case) {
            if ($case['name'] !== $name) {
                continue;
            }
            // The block editor's rules accept every layout node; only the outline is kept.
            $valid = PageCreateBuilder::validate((object) [
                'post_type' => 'page',
                'editor'    => PageCreateBuilder::EDITOR_BLOCKS,
                'title'     => self::TITLE,
                'outline'   => json_decode((string) json_encode($case['input']), false, 512, JSON_THROW_ON_ERROR),
            ]);
            if (!isset($valid['spec'])) {
                throw new \LogicException('golden case does not validate: ' . $name);
            }

            return [$valid['spec']['outline'], $case['tree'], $fixture['request_id'], $fixture['media']];
        }
        throw new \LogicException('golden case missing: ' . $name);
    }

    /**
     * A validated builder spec for $outline.
     *
     * @param list<array<string,mixed>> $outline Outline.
     * @return array<string,mixed>
     */
    private static function spec(array $outline): array
    {
        return ['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => self::TITLE, 'outline' => $outline];
    }

    /**
     * A resolved image fact row, as the command's media resolution answers.
     *
     * @return array<string,mixed>
     */
    private static function mediaFact(int $id, string $url): array
    {
        return ['id' => $id, 'url' => $url, 'filename' => 'van.png', 'mime' => 'image/png', 'width' => 1600, 'height' => 1000, 'modified_gmt' => '2026-10-01 09:00:00'];
    }

    /**
     * The fields a failure answer is compared on.
     *
     * @param array<string,mixed> $result Answer.
     * @return array<string,mixed>
     */
    private static function summary(array $result): array
    {
        return [
            'ok'      => $result['ok'] ?? null,
            'code'    => $result['code'] ?? null,
            'post_id' => $result['post_id'] ?? null,
            'trashed' => $result['trashed'] ?? null,
        ];
    }
}
