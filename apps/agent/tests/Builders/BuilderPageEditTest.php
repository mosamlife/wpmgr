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
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\BuilderPageEdit;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\ElementorStyleCache;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\NativeDocument;
use WPMgr\Agent\Abilities\Builders\OutlineV3;
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

    /** A designed page: a section that links to a pricing section. */
    private const DESIGNED_PAGE = '[{"type":"section","anchor":"top","children":[{"type":"paragraph","text":"Welcome."},{"type":"buttons","buttons":[{"text":"See plans","url":"#plans"}]}]},'
        . '{"type":"section","anchor":"plans","tone":"surface","children":[{"type":"pattern","pattern":"pricing","variant":"three-plans","plans":['
        . '{"name":"Starter","price":"$19","period":"per month","features":["One site","Email support"],"cta":{"text":"Choose Starter","url":"https://example.com/start"}},'
        . '{"name":"Growth","price":"$29","period":"per month","features":["Five sites","Chat support"],"cta":{"text":"Choose Growth","url":"https://example.com/grow"}},'
        . '{"name":"Scale","price":"$99","period":"per month","features":["Fifty sites","Phone support"],"cta":{"text":"Choose Scale","url":"https://example.com/scale"}}]}]}]';

    /** A section a designed page is given: a paragraph and a testimonials pattern. */
    private const NEW_SECTION = '[{"type":"section","anchor":"press","children":[{"type":"paragraph","text":"Where we were mentioned."},'
        . '{"type":"pattern","pattern":"testimonials","variant":"featured","testimonials":[{"quote":"It just works.","name":"Ana","role":"Founder"},{"quote":"Saved us hours.","name":"Ben"}]}]}]';

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

    private function nativeCatalogue(): void
    {
        $this->api->coreWidgets = ['heading', 'counter'];
        $this->api->widgetTypes[] = 'counter';
        foreach ($this->api->coreWidgets as $type) {
            $this->api->widgetCatalogue[] = ['name' => $type, 'title' => ucfirst($type), 'categories' => ['basic'], 'pro' => false, 'atomic' => false];
            $this->api->widgetControls[$type] = ['title' => ['type' => 'text', 'label' => 'Title'], 'image' => ['type' => 'media']];
        }
    }

    /** Real plan/precheck answers are the cross-language verifier oracle. */
    public function test_native_precheck_answers_golden(): void
    {
        $tree = $this->pageWithNativeCounter();
        RealNativeControls::install($this->api);
        $cases = [
            'set_native' => [['op' => 'set_native', 'ref' => 'ccccccc', 'settings' => ['title' => 'New counter']]],
            'add_native' => [['op' => 'add_native', 'into' => self::RIGHT, 'widget' => 'button', 'settings' => ['text' => 'New button']]],
            'duplicate' => [['op' => 'duplicate', 'ref' => 'ccccccc', 'after' => 'ccccccc']],
            'remove' => [['op' => 'remove', 'ref' => 'ccccccc']],
            'move' => [['op' => 'move', 'ref' => 'ccccccc', 'into' => $tree[0]['elements'][0]['id'], 'position' => 'first']],
            'no_native_op' => [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Legacy title']],
            'merge_delete' => [['op' => 'set_native', 'ref' => 'ccccccc', 'settings' => ['title' => null, 'ending_number' => '25']]],
            'mapped_set_native' => [['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native heading']]],
            'reused_anchor' => [
                ['op' => 'add_native', 'after' => 'ccccccc', 'widget' => 'counter', 'settings' => ['title' => 'First']],
                ['op' => 'add_native', 'after' => 'ccccccc', 'widget' => 'counter', 'settings' => ['title' => 'Second']],
            ],
            'remove_ancestor' => [['op' => 'remove', 'ref' => self::RIGHT]],
        ];
        $rows = [];
        foreach ($cases as $name => $ops) {
            $this->storePage($tree);
            $input = $this->input($ops);
            $plan = $this->plan($input);
            $this->assertArrayNotHasKey('refusal', $plan, $name . ': ' . json_encode($plan['refusal'] ?? null));
            $answer = BuilderPageEdit::precheckAnswer($plan, self::EDIT, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']));
            $this->assertTrue($answer['ok'], $name);
            $rows[] = ['name' => $name, 'request_id' => self::EDIT, 'entry_sha256' => self::ENTRY_SHA, 'input' => $input,
                'base_fingerprint' => $answer['base_fingerprint'], 'preview' => $answer['preview'],
                'tree_json' => json_encode($plan['preview']['tree'], JSON_THROW_ON_ERROR),
                'preview_digest' => $answer['preview_digest'], 'precheck_digest' => $answer['precheck_digest']];
        }
        $bytes = json_encode($rows, JSON_PRETTY_PRINT | JSON_THROW_ON_ERROR) . "\n";
        $path = __DIR__ . '/../fixtures/ability-run/native-precheck-answers.json';
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertSame(strlen($bytes), file_put_contents($path, $bytes));
        }
        $this->assertFileExists($path, 'Generate with WPMGR_WRITE_FIXTURES=1');
        $this->assertSame(file_get_contents($path), $bytes);
    }

    public function test_native_stored_shape_matches_elementor_factory(): void
    {
        RealNativeControls::install($this->api);
        $normalise = static function (array $node) use (&$normalise): array {
            foreach ($node['elements'] ?? [] as $i => $child) {
                $node['elements'][$i] = $normalise($child);
            }
            if (($node['elType'] ?? '') === 'widget') {
                $type = $node['widgetType'];
                unset($node['isInner'], $node['widgetType']);
                $node['widgetType'] = $type;
            }
            return $node;
        };
        $this->api->elementFactory = static fn ($node) => new class ($normalise($node)) {
            public function __construct(private array $node) {}
            public function get_data_for_save(): array { return $this->node; }
        };
        $input = $this->input([
            ['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native title']],
            ['op' => 'add_native', 'into' => self::RIGHT, 'widget' => 'button', 'settings' => ['text' => 'New button']],
        ]);
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $added = $plan['preview']['results'][1]['result_node'];
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'widgetType'], array_keys($added));
        $this->assertSame($added, $plan['preview']['tree'][0]['elements'][1]['elements'][1]);
        $set = $plan['preview']['results'][0]['result_node'];
        $expected = self::page()[0]['elements'][0]['elements'][0];
        $expected['settings']['title'] = 'Native title';
        $this->assertSame($expected, $set);
        $this->api->storingDocument(self::TARGET, $this->rows);
        $r = $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);
        $this->assertTrue($r['ok'] ?? false, json_encode($r));
    }

    public function test_native_designed_ancestor_and_host_rules_use_compiler_shapes(): void
    {
        $this->storeDesigned();
        $tree = self::designedPage();
        RealNativeControls::install($this->api);
        $this->api->coreWidgets[] = 'text-editor';
        $this->api->widgetControls['text-editor'] = ['editor' => ['type' => 'wysiwyg']];
        $this->api->widgetCatalogue[] = ['name' => 'text-editor', 'title' => 'Text', 'categories' => ['basic'], 'pro' => false, 'atomic' => false];
        $plain = ['id' => 'eeeeeee', 'elType' => 'container', 'settings' => [], 'elements' => [
            ['id' => 'fffffff', 'elType' => 'widget', 'settings' => ['title' => 'Outside'], 'elements' => [], 'widgetType' => 'counter']]];
        $tree[] = $plain;
        $this->storePage($tree);
        $section = $tree[0]['id'];
        $source = $tree[0]['elements'][0]['id'];
        $pattern = $tree[1]['elements'][0];
        $findWidget = static function (array $node) use (&$findWidget): ?string {
            if ($node['elType'] === 'widget') { return $node['id']; }
            foreach ($node['elements'] as $child) {
                $id = $findWidget($child);
                if ($id !== null) { return $id; }
            }
            return null;
        };
        $patternSource = $findWidget($pattern);
        foreach ([
            ['op' => 'set_native', 'ref' => $source, 'settings' => ['editor' => 'Changed']],
            ['op' => 'duplicate', 'ref' => $source, 'into' => 'eeeeeee'],
            ['op' => 'duplicate', 'ref' => $patternSource, 'into' => 'eeeeeee'],
            ['op' => 'add_native', 'into' => $section, 'widget' => 'counter', 'settings' => []],
            ['op' => 'add_native', 'into' => $pattern['id'], 'widget' => 'counter', 'settings' => []],
            ['op' => 'duplicate', 'ref' => 'fffffff', 'into' => $section],
            ['op' => 'move', 'ref' => 'fffffff', 'into' => $section],
            ['op' => 'move', 'ref' => 'fffffff', 'into' => $pattern['id']],
        ] as $op) {
            $plan = $this->plan($this->input([$op]));
            $this->assertSame('native_designed_node', $plan['refusal']['code'] ?? null, json_encode($plan));
        }
        $plan = $this->plan($this->input([['op' => 'set_native', 'ref' => 'fffffff', 'settings' => ['title' => 'Outside edit']]]));
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan));
    }

    public function test_native_plan_reads_one_breakpoint_list_for_validation_and_digest(): void
    {
        $this->nativeCatalogue();
        $this->api->widgetControls['heading']['color'] = ['type' => 'color', 'responsive' => true];
        $this->api->activeBreakpoints = ['tablet', 'mobile'];
        $this->api->calls = [];
        $input = $this->input([['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['color_tablet' => '#ABCDEF']]]);
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $calls = array_filter($this->api->calls, static fn ($call) => $call[0] === 'activeBreakpoints');
        $this->assertCount(1, $calls);
        $this->assertSame(['mobile', 'tablet'], $plan['preview']['digest_inputs']['active_breakpoints']);
        $this->api->activeBreakpoints = null;
        $input = $this->input([['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'New']]]);
        $plan = $this->plan($input);
        $this->assertSame('native_settings_invalid', $plan['refusal']['code'] ?? null);
    }

    public function test_media_resolver_preserves_outline_order_then_sorts_native_ids(): void
    {
        $asked = [];
        $resolve = static function (array $ids) use (&$asked): array {
            $asked = $ids;
            return ['facts' => array_map(static fn ($id) => ['id' => $id, 'url' => 'https://example.com/' . $id . '.jpg',
                'filename' => $id . '.jpg', 'mime' => 'image/jpeg', 'width' => 100, 'height' => 100, 'modified_gmt' => '2026-10-13 00:00:00'], $ids)];
        };
        $ops = [['op' => 'insert', 'after' => self::TEXT, 'outline' => [
            ['type' => 'image', 'attachment_id' => 9, 'alt' => ''],
            ['type' => 'image', 'attachment_id' => 7, 'alt' => '']]]];
        $plan = BuilderPageEdit::plan($this->input($ops), $this->entry(), [self::TARGET], self::EDIT, $resolve, $this->seam());
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $this->assertSame([9, 7], $asked);
        $this->nativeCatalogue();
        $this->api->widgetControls['counter']['second_image'] = ['type' => 'media'];
        $ops[] = ['op' => 'add_native', 'into' => self::RIGHT, 'widget' => 'counter', 'settings' => [
            'image' => ['id' => '4', 'url' => 'https://example.com/4.jpg'],
            'second_image' => ['id' => '2', 'url' => 'https://example.com/2.jpg']]];
        $plan = BuilderPageEdit::plan($this->input($ops), $this->entry(), [self::TARGET], self::EDIT, $resolve, $this->seam());
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $this->assertSame([9, 7, 2, 4], $asked);
    }

    public function test_invalid_utf8_title_refuses_precheck_answer(): void
    {
        $this->nativeCatalogue();
        $this->rows->update($this->rows->posts, ['post_title' => "\xB1"], ['ID' => self::TARGET]);
        foreach ([[['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native']]], self::ops()] as $ops) {
            $plan = $this->plan($this->input($ops));
            $this->assertSame('preview_unreadable', $plan['refusal']['code'] ?? null);
            $this->assertStringContainsString('UTF-8', $plan['refusal']['detail']);
        }
    }

    public function test_repeated_id_precheck_reports_the_page_identity_problem(): void
    {
        $tree = self::page();
        $tree[0]['elements'][1]['elements'][0]['id'] = self::HEADING;
        $this->storePage($tree);
        $plan = $this->plan($this->input([['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'New']]));
        $this->assertSame('data_unreadable', $plan['refusal']['code'] ?? null);
        $this->assertSame('an element id appears more than once on the page', $plan['refusal']['detail'] ?? null);
    }

    public function test_invalid_core_type_projects_locked_and_other_text_prechecks(): void
    {
        $tree = self::page();
        foreach (['bad_name', 'Bad-name', str_repeat('a', 65)] as $i => $type) {
            $this->api->widgetTypes[] = $type;
            $this->api->coreWidgets[] = $type;
            $this->api->widgetCatalogue[] = ['name' => $type, 'title' => 'Core', 'categories' => ['basic'], 'pro' => false, 'atomic' => false];
            $tree[0]['elements'][1]['elements'][] = ['id' => 'ffffff' . $i, 'elType' => 'widget', 'settings' => [], 'elements' => [], 'widgetType' => $type];
        }
        $this->storePage($tree);
        $nodes = (new ElementorAdapter($this->api))->project($tree)->nodes();
        foreach (array_slice($nodes, -3) as $node) { $this->assertSame('locked', $node['kind']); }
        $plan = $this->plan($this->input([['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'New']]));
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
    }

    public function test_real_size_counter_and_button_native_operations_precheck_and_write(): void
    {
        RealNativeControls::install($this->api);
        $normalise = static function (array $node) use (&$normalise): array {
            foreach ($node['elements'] ?? [] as $i => $child) { $node['elements'][$i] = $normalise($child); }
            if ($node['elType'] === 'widget') {
                $type = $node['widgetType']; unset($node['isInner'], $node['widgetType']); $node['widgetType'] = $type;
            }
            return $node;
        };
        $this->api->elementFactory = static fn ($node) => new class ($normalise($node)) {
            public function __construct(private array $node) {}
            public function get_data_for_save(): array { return $this->node; }
        };
        foreach (['counter' => 'title', 'button' => 'text'] as $type => $key) {
            foreach ([['op' => 'set_native', 'ref' => 'ccccccc', 'settings' => [$key => 'New']],
                ['op' => 'add_native', 'into' => self::RIGHT, 'widget' => $type, 'settings' => [$key => 'New']],
                ['op' => 'duplicate', 'ref' => 'ccccccc', 'into' => self::RIGHT]] as $op) {
                $tree = self::page();
                $tree[0]['elements'][0]['elements'][] = ['id' => 'ccccccc', 'elType' => 'widget', 'settings' => [$key => 'Stored'], 'elements' => [], 'widgetType' => $type];
                $this->storePage($tree);
                $input = $this->input([$op]);
                $plan = $this->plan($input);
                $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
                $this->api->storingDocument(self::TARGET, $this->rows);
                $r = $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);
                $this->assertTrue($r['ok'] ?? false, json_encode($r));
            }
        }
    }

    public function test_native_preview_binds_live_schema_and_write_uses_existing_snapshot_path(): void
    {
        $this->nativeCatalogue();
        $ops = [['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native title']],
            ['op' => 'add_native', 'into' => self::RIGHT, 'widget' => 'counter', 'settings' => ['title' => 'Counter']]];
        $input = $this->input($ops);
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $preview = $plan['preview'];
        $this->assertSame(self::page(), $preview['before_tree']);
        $this->assertSame(hash('sha256', json_encode(self::page())), $preview['before_tree_sha256']);
        $this->assertSame($ops, array_column($preview['results'], 'op'));
        $this->assertSame(['before_tree_sha256', 'widgets', 'active_breakpoints', 'builder_version', 'media'], array_keys($preview['digest_inputs']));
        $this->assertSame($preview['before_tree_sha256'], $preview['digest_inputs']['before_tree_sha256']);
        $this->assertSame(hash('sha256', json_encode(['wpmgr.page_edit.native.v1', 'elementor', '3.35.9', self::TARGET, $this->currentFp(), json_encode($preview['tree']), json_encode($preview['digest_inputs'])])), $plan['preview_digest']);
        $this->assertSame($plan['preview_digest'], $this->plan($input)['preview_digest']);
        $this->api->storingDocument(self::TARGET, $this->rows);
        $r = $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);
        $this->assertTrue($r['ok'] ?? false, json_encode($r));
        $this->assertArrayHasKey('snapshot_sha256', $r);
        $this->assertSame([], $this->wpdb->claims);
    }

    public function test_native_schema_change_refuses_approved_write(): void
    {
        $this->nativeCatalogue();
        $input = $this->input([['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native title']]]);
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan));
        $this->api->widgetControls['heading']['title']['default'] = 'Changed default';
        $this->api->storingDocument(self::TARGET, $this->rows);
        $r = $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);
        $this->assertSame('preview_changed', $r['code'] ?? null);
        $this->assertSame([], $this->api->documents[self::TARGET]->saves);
        $this->assertSame([], $this->rows->optionRows());
    }

    public function test_native_media_resolution_and_exact_site_url(): void
    {
        $this->nativeCatalogue();
        $fact = ['id' => 7, 'url' => 'https://example.com/uploads/a.jpg', 'filename' => 'a.jpg', 'mime' => 'image/jpeg', 'width' => 100, 'height' => 100, 'modified_gmt' => '2026-10-13 00:00:00'];
        $resolve = static fn (array $ids): array => ['facts' => $ids === [7] ? [$fact] : []];
        $ops = [['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['image' => ['id' => '7', 'url' => $fact['url']]]]];
        $input = $this->input($ops);
        $plan = BuilderPageEdit::plan($input, $this->entry(), [self::TARGET], self::EDIT, $resolve, $this->seam());
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $this->assertSame('7', $plan['preview']['digest_inputs']['media'][0]['id']);
        $this->assertSame($fact['url'], $plan['preview']['digest_inputs']['media'][0]['url']);
        $ops[0]['settings']['image']['url'] = 'https://example.com/uploads/wrong.jpg';
        $r = BuilderPageEdit::plan($this->input($ops), $this->entry(), [self::TARGET], self::EDIT, $resolve, $this->seam());
        $this->assertSame('native_settings_invalid', $r['refusal']['code'] ?? null);
        $this->assertNotEmpty($r['refusal']['failures']);
        $this->assertArrayHasKey('schema', $r['refusal']);
    }

    public function test_native_precheck_answer_size_cap(): void
    {
        $plan = ['base_fingerprint' => str_repeat('a', 64), 'preview_digest' => str_repeat('b', 64), 'builder_facts' => null,
            'preview' => ['digest_inputs' => [], 'title' => str_repeat('x', 4194304)]];
        $r = BuilderPageEdit::precheckAnswer($plan, self::EDIT, str_repeat('c', 64));
        $this->assertSame('native_preview_too_large', $r['code'] ?? null);
    }

    public function test_native_write_detects_changed_carry_over_and_restores(): void
    {
        $this->nativeCatalogue();
        $input = $this->input([['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native title']]]);
        $before = $this->currentFp();
        $r = $this->approvedWrite($input, static function (array $tree): array {
            $tree[0]['elements'][0]['elements'][0]['settings']['title_color'] = '#000000';
            return $tree;
        });
        $this->assertSame(['verify_mismatch', true], [$r['code'] ?? null, $r['restored'] ?? null], json_encode($r));
        $this->assertSame($before, $this->currentFp());
    }

    public function test_native_breakpoint_change_on_reused_adapter_changes_digest(): void
    {
        $this->nativeCatalogue();
        $seam = $this->seam();
        $input = $this->input([['op' => 'set_native', 'ref' => self::HEADING, 'settings' => ['title' => 'Native title']]]);
        $first = BuilderPageEdit::plan($input, $this->entry(), [self::TARGET], self::EDIT, self::noMedia(), $seam);
        $this->assertArrayNotHasKey('refusal', $first, json_encode($first['refusal'] ?? null));
        $this->api->activeBreakpoints = ['mobile'];
        $second = BuilderPageEdit::plan($input, $this->entry(), [self::TARGET], self::EDIT, self::noMedia(), $seam);
        $this->assertSame(['mobile'], $second['preview']['digest_inputs']['active_breakpoints'] ?? null);
        $this->assertNotSame($first['preview_digest'], $second['preview_digest'] ?? null);
    }

    public function test_native_duplicate_media_is_resolved_and_must_match_the_site(): void
    {
        $this->nativeCatalogue();
        $fact = ['id' => 7, 'url' => 'https://example.com/uploads/a.jpg', 'filename' => 'a.jpg', 'mime' => 'image/jpeg', 'width' => 100, 'height' => 100, 'modified_gmt' => '2026-10-13 00:00:00'];
        $asked = [];
        $resolve = static function (array $ids) use ($fact, &$asked): array {
            $asked = $ids;
            return ['facts' => [$fact]];
        };
        $tree = self::page();
        $tree[0]['elements'][0]['elements'][0]['settings']['image'] = ['id' => 7, 'url' => $fact['url'], 'alt' => 'Stored alt', 'source' => 'library', 'size' => 'large'];
        $this->storePage($tree);
        $ops = [['op' => 'duplicate', 'ref' => self::HEADING, 'into' => self::RIGHT]];
        $r = BuilderPageEdit::plan($this->input($ops), $this->entry(), [self::TARGET], self::EDIT, $resolve, $this->seam());
        $this->assertArrayNotHasKey('refusal', $r, json_encode($r['refusal'] ?? null));
        $this->assertSame([7], $asked);
        $this->assertSame($tree[0]['elements'][0]['elements'][0]['settings']['image'], $r['preview']['tree'][0]['elements'][1]['elements'][1]['settings']['image']);
        $tree[0]['elements'][0]['elements'][0]['settings']['image']['url'] = 'https://example.com/wrong.jpg';
        $this->storePage($tree);
        $r = BuilderPageEdit::plan($this->input($ops), $this->entry(), [self::TARGET], self::EDIT, $resolve, $this->seam());
        $this->assertSame('native_settings_invalid', $r['refusal']['code'] ?? null);
        $this->assertSame('media', $r['refusal']['schema']['controls']['image']['type'] ?? null);
    }

    private function pageWithNativeCounter(): array
    {
        $this->nativeCatalogue();
        $tree = self::page();
        $tree[0]['elements'][1]['elements'][] = ['id' => 'ccccccc', 'elType' => 'widget', 'widgetType' => 'counter', 'isInner' => false, 'settings' => ['title' => 'Counter'], 'elements' => []];
        $this->storePage($tree);
        return $tree;
    }

    public function test_ancestor_move_keeps_base_commit_legacy_golden(): void
    {
        $tree = $this->pageWithNativeCounter();
        RealNativeControls::install($this->api);
        $input = $this->input([
            ['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Golden title'],
            ['op' => 'insert', 'after' => self::TEXT, 'outline' => [['type' => 'paragraph', 'text' => 'Golden paragraph']]],
            ['op' => 'move', 'ref' => self::RIGHT, 'before' => $tree[0]['elements'][0]['id']],
        ]);
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $answer = BuilderPageEdit::precheckAnswer($plan, self::EDIT, str_repeat('f', 64));
        $bytes = json_encode($answer, JSON_THROW_ON_ERROR);
        $path = __DIR__ . '/../fixtures/ability-run/native-legacy-precheck.json';
        if (getenv('WPMGR_RECORD_BASE_GOLDEN') === '1') {
            file_put_contents($path, $bytes . "\n");
        }
        $this->assertSame(trim(file_get_contents($path)), $bytes);
        $this->assertArrayNotHasKey('digest_inputs', $answer['preview']);
    }

    public function test_non_native_calls_keep_legacy_answer_with_native_nodes_as_anchors(): void
    {
        $tree = $this->pageWithNativeCounter();
        $ops = [['op' => 'insert', 'after' => 'ccccccc', 'outline' => [['type' => 'paragraph', 'text' => 'Legacy insert']]]];
        $input = $this->input($ops);
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $parsed = \WPMgr\Agent\Abilities\Builders\PageEditValidator::parse($input);
        $legacy = \WPMgr\Agent\Abilities\Builders\LayoutOps::apply($tree, $parsed['input']['operations'], new IdSeed(self::EDIT), [], true);
        $this->assertSame($legacy['changes'], $plan['preview']['changes']);
        $this->assertSame(json_encode(ElementorClassicMapper::project($legacy['tree'])->toArray(500)), json_encode($plan['preview']['after_outline']));
        $this->assertSame(BuilderPageEdit::previewDigest('elementor', '3.35.9', self::TARGET, $this->currentFp(), json_encode($legacy['tree'])), $plan['preview_digest']);
        $this->assertArrayNotHasKey('digest_inputs', $plan['preview']);
    }

    public function test_structural_native_subtree_changes_include_before_tree_without_schema(): void
    {
        $tree = $this->pageWithNativeCounter();
        $plan = $this->plan($this->input([['op' => 'remove', 'ref' => self::RIGHT]]));
        $this->assertArrayNotHasKey('refusal', $plan, json_encode($plan['refusal'] ?? null));
        $this->assertSame($tree, $plan['preview']['before_tree'] ?? null);
        $this->assertSame([], (array) $plan['preview']['digest_inputs']['widgets']);
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

    public function test_a_stylesheet_that_was_not_regenerated_is_a_warning_on_an_applied_edit(): void
    {
        $this->api->regenerated = ['status' => 'file', 'path' => sys_get_temp_dir() . '/wpmgr-no-such-dir/post-41.css'];

        $r = $this->approvedWrite($this->input(self::ops()));

        $this->assertSame([true, 'applied'], [$r['ok'] ?? null, $r['outcome'] ?? null], (string) json_encode($r));
        $this->assertSame(['css_not_regenerated'], array_column($r['warnings'] ?? [], 'code'));
        $this->assertSame([[self::TARGET]], $this->api->callsTo('regeneratePostCss'), 'regenerated once');
        $this->assertSame([], $this->api->callsTo('deletePostCss'), 'nothing was put back');
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

    public function test_editor_open_is_retryable_other_conflicts_are_not(): void
    {
        $input = $this->input(self::ops());

        // Someone else holds the edit lock: worth asking again once it lapses.
        $this->locks[self::TARGET] = 9;
        $open = $this->plan($input)['refusal'] ?? [];
        $this->assertSame(['conflict', 'editor_open', true], [$open['code'] ?? null, $open['detail'] ?? null, $open['retryable'] ?? null], (string) json_encode($open));
        $this->assertSame(['ok', 'outcome', 'code', 'detail', 'retryable'], array_keys($open), 'the detail is the one token, nothing from the site');

        // An autosave waits on a person, and a page that moved on needs a new read.
        $this->locks                   = [];
        $this->autosaves[self::TARGET] = 9;
        $autosave = $this->plan($input)['refusal'] ?? [];
        $this->assertSame(['conflict', 'autosave_pending', false], [$autosave['code'] ?? null, $autosave['detail'] ?? null, $autosave['retryable'] ?? null]);
        $this->autosaves = [];
        $stale = $this->plan($this->input(self::ops(), str_repeat('c', 64)))['refusal'] ?? [];
        $this->assertSame(['conflict', 'changed_since_read', false], [$stale['code'] ?? null, $stale['detail'] ?? null, $stale['retryable'] ?? null]);

        // The lock taken between the approval and the write.
        $plan = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $this->api->storingDocument(self::TARGET, $this->rows);
        $before                    = $this->rows->metaRowsOf(self::TARGET);
        $post                      = $this->rows->postRow(self::TARGET);
        $this->locks[self::TARGET] = 9;
        $r = $this->write($input, $this->digest($input)($plan['base_fingerprint'], $plan['preview_digest']), $plan['preview_digest']);
        $this->assertSame(['conflict', 'editor_open', true, false], [$r['code'] ?? null, $r['detail'] ?? null, $r['retryable'] ?? null, $r['ok'] ?? null], (string) json_encode($r));
        $this->assertSame([], $this->api->documents[self::TARGET]->saves, 'Elementor was never asked to save');
        $this->assertSame($before, $this->rows->metaRowsOf(self::TARGET), 'nothing written');
        $this->assertSame($post, $this->rows->postRow(self::TARGET));
        $this->assertNull(AbilityLedger::get(self::EDIT), 'no ledger row');
        $this->assertSame([], $this->rows->optionRows(), 'no snapshot');
        $this->assertSame([], $this->wpdb->claims, 'the target claim is released');
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

    public function test_a_sqlite_site_takes_the_edit_without_reading_the_catalogue(): void
    {
        // The SQLite database integration's driver, whose catalogue cannot
        // name a table's storage engine: SQLite has one, with transactions.
        $sqlite          = SqliteEngineWpdb::asDriver($this->rows);
        $this->wpdb      = $sqlite;
        $GLOBALS['wpdb'] = $sqlite;
        $this->assertInstanceOf(SqliteEngineWpdb::DRIVER_CLASS, $sqlite);

        $input = $this->input(self::ops());
        $r     = $this->approvedWrite($input);
        $this->assertSame([true, 'applied'], [$r['ok'] ?? null, $r['outcome'] ?? null], (string) json_encode($r));
        $this->assertSame(0, $sqlite->catalogueReads, 'information_schema is never read on SQLite');
        $this->assertSame('completed', AbilityLedger::get(self::EDIT)['phase'] ?? null);
        $this->assertSame([], $this->wpdb->claims, 'the claim is released');

        // The same unreadable catalogue behind any other database class
        // still refuses before anything is written.
        $this->storePage(self::page());
        $this->rows->failOn = 'information_schema';
        $this->assertSame(['builder_not_available', 'table_engine_unreadable'], self::codeOf($this->plan($this->input(self::ops()))));
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
    // -------------------------------------------------------------------------
    // A designed page, end to end through plan() and the write
    // -------------------------------------------------------------------------

    public function test_a_designed_section_insert_is_planned_written_and_held_to_the_designed_allowlist(): void
    {
        $this->storeDesigned();
        $input = $this->designedInput([['op' => 'insert', 'after' => $this->nodeOf('section', 'top')['ref'], 'outline' => json_decode(self::NEW_SECTION, true)]], true);
        $plan  = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $made = $plan['preview']['tree'][1];
        $this->assertSame(['press', LeafPolicy::SECTION_LABEL], [$made['settings']['_element_id'] ?? null, $made['settings']['_title'] ?? null]);
        $this->assertArrayHasKey('padding_tablet', $made['settings'], 'compiled for the tablet breakpoint the site has active');

        $r = $this->approvedWrite($input);
        $this->assertSame([true, 'applied'], [$r['ok'] ?? null, $r['outcome'] ?? null], (string) json_encode($r));

        // Held to the designed allowlist for this site, not to anything wider.
        $before = $this->snapshotOf(self::EDIT);
        $tree   = $plan['preview']['tree'];
        $this->assertNull($this->adapter()->verifyEdited(self::TARGET, new NativeDocument($tree), $before));
        $extra                              = $tree;
        $extra[1]['settings']['custom_css'] = 'section{}';
        $this->storeData($extra);
        $this->assertSame('leaf_policy', $this->adapter()->verifyEdited(self::TARGET, new NativeDocument($extra), $before), 'a key no designed tree holds');
        $this->storeData($tree);
        $this->api->activeBreakpoints = ['mobile'];
        $this->assertSame('leaf_policy', $this->adapter()->verifyEdited(self::TARGET, new NativeDocument($tree), $before), 'a tablet override, on a site with no tablet breakpoint');
    }

    public function test_set_text_on_a_pricing_plans_price_is_planned_and_written(): void
    {
        $this->storeDesigned();
        $input = $this->designedInput([['op' => 'set_text', 'ref' => $this->nodeOf('pattern')['ref'], 'field' => 'plans.1.price', 'text' => '$59']], false);
        $plan  = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $this->assertSame(str_replace('"$29"', '"$59"', (string) json_encode(self::designedPage())), json_encode($plan['preview']['tree']), 'only the second plan\'s price differs');
        $this->assertSame(['set_text'], array_column($plan['preview']['changes'], 'op'));

        $r = $this->approvedWrite($input);
        $this->assertSame([true, 'applied', 1], [$r['ok'] ?? null, $r['outcome'] ?? null, $r['changes_applied'] ?? null], (string) json_encode($r));
    }

    /**
     * A plain button of a designed page takes the address of a section of the
     * page, as the grammar lets it; the written link keeps its other members,
     * and the edited page is held to the designed allowlist for the site.
     */
    public function test_set_text_of_a_button_url_to_an_anchor_is_planned_and_written_on_a_designed_page(): void
    {
        $this->storeDesigned();
        $button = $this->nodeOf('buttons');
        $input  = $this->designedInput([['op' => 'set_text', 'ref' => $button['ref'], 'field' => 'url', 'text' => '#top']], false);
        $plan   = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $this->assertSame(str_replace('"url":"#plans"', '"url":"#top"', (string) json_encode(self::designedPage())), json_encode($plan['preview']['tree']), 'only the button\'s link differs');
        $this->assertSame([['url' => '#plans'], ['url' => '#top']], [$plan['preview']['changes'][0]['before'], $plan['preview']['changes'][0]['after']]);

        $r = $this->approvedWrite($input);
        $this->assertSame([true, 'applied', 1], [$r['ok'] ?? null, $r['outcome'] ?? null, $r['changes_applied'] ?? null], (string) json_encode($r));

        // A link to a section the page does not have is still the validator's to refuse, and a bad shape is still bad_input.
        $this->assertSame(['link_anchor_missing', 'link_leads_nowhere'], self::codeOf($this->plan($this->designedInput([['op' => 'set_text', 'ref' => $button['ref'], 'field' => 'url', 'text' => '#nowhere']], false))));
        $this->assertSame('bad_input', $this->plan($this->designedInput([['op' => 'set_text', 'ref' => $button['ref'], 'field' => 'url', 'text' => '#Not_An_Anchor']], false))['refusal']['code'] ?? null);
    }

    /**
     * A link is one value of the designed allowlist: what an edit wrote into a
     * link is checked as the whole link, however many of its members changed.
     * So a plain button's address and a plan's button address are written on a
     * designed page, a path on the site and a section of the page alike.
     */
    public function test_set_text_of_a_path_on_the_site_is_written_to_a_button_of_a_designed_page(): void
    {
        $this->storeDesigned();
        $input = $this->designedInput([['op' => 'set_text', 'ref' => $this->nodeOf('buttons')['ref'], 'field' => 'url', 'text' => '/start']], false);
        $r     = $this->approvedWrite($input);
        $this->assertSame([true, 'applied', 1], [$r['ok'] ?? null, $r['outcome'] ?? null, $r['changes_applied'] ?? null], (string) json_encode($r));
        $this->assertStringContainsString('"url":"\/start","is_external":"","nofollow":""', (string) ($this->rowsOf(ElementorDocument::KEY_DATA)[0] ?? ''));
    }

    public function test_set_text_of_a_plans_button_address_is_written_on_a_designed_page(): void
    {
        $this->storeDesigned();
        $input = $this->designedInput([['op' => 'set_text', 'ref' => $this->nodeOf('pattern')['ref'], 'field' => 'plans.0.cta.url', 'text' => '#top']], false);
        $r     = $this->approvedWrite($input);
        $this->assertSame([true, 'applied', 1], [$r['ok'] ?? null, $r['outcome'] ?? null, $r['changes_applied'] ?? null], (string) json_encode($r));
        $this->assertStringContainsString('"url":"#top","is_external":"","nofollow":""', (string) ($this->rowsOf(ElementorDocument::KEY_DATA)[0] ?? ''));
    }

    /**
     * The precheck of a designed page names the breakpoints its tree is a
     * function of, as page-create's does; the answer of a plain page names
     * nothing, and the digests do not hold the facts.
     */
    public function test_the_precheck_names_the_active_breakpoints_for_a_designed_page_and_nothing_for_a_plain_one(): void
    {
        $plain = $this->plan($this->input(self::ops()));
        $this->assertArrayNotHasKey('refusal', $plain, (string) json_encode($plain['refusal'] ?? null));
        $this->assertNull($plain['builder_facts'] ?? null, 'a plain page has no fact');
        $this->assertArrayNotHasKey('builder_facts', BuilderPageEdit::precheckAnswer($plain, self::EDIT, str_repeat('0', 64)), 'a plain page\'s answer is as it was');

        $this->api->activeBreakpoints = ['mobile'];
        $this->storeDesigned();
        $input = $this->designedInput([['op' => 'set_text', 'ref' => $this->nodeOf('pattern')['ref'], 'field' => 'plans.1.price', 'text' => '$59']], false);
        $plan  = $this->plan($input);
        $this->assertArrayNotHasKey('refusal', $plan, (string) json_encode($plan['refusal'] ?? null));
        $this->assertSame(['active_breakpoints' => ['mobile']], $plan['builder_facts'] ?? null, 'the breakpoints the site has active');
        $this->assertSame(['active_breakpoints' => ['mobile']], BuilderPageEdit::precheckAnswer($plan, self::EDIT, str_repeat('0', 64))['builder_facts'] ?? null, 'the answer carries them beside the preview');

        $this->api->activeBreakpoints = [];
        $none = $this->plan($input);
        $this->assertSame(['active_breakpoints' => []], $none['builder_facts'] ?? null, 'none active is a list, not a missing fact');
        $this->assertSame($plan['preview_digest'], $none['preview_digest'], 'the facts are beside the preview, not in its digest');
    }

    /** A page is designed when it holds a designed section before the edit or after it. */
    public function test_a_page_is_designed_when_the_edit_finds_or_leaves_a_designed_section(): void
    {
        $plain    = self::page();
        $designed = self::designedPage();
        $adapter  = $this->adapter();
        $this->assertNull($adapter->editFacts($plain, $plain));
        $want = ['active_breakpoints' => ['mobile', 'tablet']];
        $this->assertSame($want, $adapter->editFacts($designed, $designed));
        $this->assertSame($want, $adapter->editFacts($designed, $plain), 'an edit that takes the last section away still found a designed page');
        $this->assertSame($want, $adapter->editFacts($plain, $designed), 'an edit that leaves a designed section is a designed page');
    }

    public function test_a_hero_inserted_below_a_section_is_refused_and_one_above_it_is_planned(): void
    {
        $this->storeDesigned();
        $hero = [['type' => 'section', 'anchor' => 'welcome', 'children' => [['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => [['type' => 'heading', 'level' => 1, 'text' => 'Ship faster']]]]]];

        $below = $this->plan($this->designedInput([['op' => 'insert', 'after' => $this->nodeOf('section', 'top')['ref'], 'outline' => $hero]], true));
        $this->assertSame(['hero_misplaced', 'hero_not_first'], self::codeOf($below));

        $above = $this->plan($this->designedInput([['op' => 'insert', 'before' => $this->nodeOf('section', 'top')['ref'], 'outline' => $hero]], true));
        $this->assertArrayNotHasKey('refusal', $above, (string) json_encode($above['refusal'] ?? null));
        $this->assertSame([], $this->rows->optionRows(), 'a precheck stores nothing');
    }

    public function test_an_inserted_background_that_is_not_an_image_is_refused_naming_its_operation(): void
    {
        $this->storeDesigned();
        $mime = 'application/pdf';
        Functions\when('get_post')->alias(static fn ($id) => (int) $id === 77 ? (object) ['ID' => 77, 'post_type' => 'attachment'] : null);
        Functions\when('current_user_can')->justReturn(true);
        Functions\when('get_post_mime_type')->alias(static function () use (&$mime) {
            return $mime;
        });
        $section = [['type' => 'section', 'anchor' => 'press', 'background' => ['attachment_id' => 77], 'children' => [['type' => 'paragraph', 'text' => 'Where we were mentioned.']]]];
        $input   = $this->designedInput([
            ['op' => 'set_text', 'ref' => $this->nodeOf('pattern')['ref'], 'field' => 'plans.0.price', 'text' => '$9'],
            ['op' => 'insert', 'after' => $this->nodeOf('section', 'plans')['ref'], 'outline' => $section],
        ], true);

        $plan = $this->plan($input);
        $this->assertSame(['background_not_image', 1], [$plan['refusal']['code'] ?? null, $plan['refusal']['op_index'] ?? null], (string) json_encode($plan));
        $this->assertSame(['operations[1].outline[0].background'], array_column($plan['refusal']['failures'] ?? [], 'path'));

        // An image passes this check, and is then resolved as every image is.
        $mime = 'image/png';
        $this->assertSame('image_not_available', $this->plan($input)['refusal']['code'] ?? null);
    }

    public function test_an_inserted_logo_whose_alt_is_not_the_librarys_is_refused_naming_its_operation(): void
    {
        $this->storeDesigned();
        Functions\when('get_post_meta')->alias(static fn ($id, $key = '', $single = false) => $key === BuilderPageCreate::ALT_META_KEY ? 'Logo ' . $id : '');
        $media = static fn (array $ids): array => ['facts' => array_map(static fn (int $id): array => [
            'id'           => $id,
            'url'          => 'https://example.com/wp-content/uploads/2026/10/logo-' . $id . '.png',
            'filename'     => 'logo-' . $id . '.png',
            'mime'         => 'image/png',
            'width'        => 320,
            'height'       => 120,
            'modified_gmt' => '2026-10-01 09:00:00',
        ], $ids)];
        $input = fn (string $alt): string => $this->designedInput([['op' => 'insert', 'after' => $this->nodeOf('section', 'plans')['ref'], 'outline' => [[
            'type'     => 'section',
            'anchor'   => 'press',
            'children' => [['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => [
                ['attachment_id' => 81, 'alt' => 'Logo 81'],
                ['attachment_id' => 82, 'alt' => $alt],
                ['attachment_id' => 83, 'alt' => 'Logo 83'],
            ]]],
        ]]]], true);

        $plan = BuilderPageEdit::plan($input('Acme'), $this->entry(), [self::TARGET], self::EDIT, $media, $this->seam());
        $this->assertSame(['image_alt_from_library', 0], [$plan['refusal']['code'] ?? null, $plan['refusal']['op_index'] ?? null], (string) json_encode($plan));
        $this->assertSame(['operations[0].outline[0].children[0].logos[1]'], array_column($plan['refusal']['failures'] ?? [], 'path'));

        $same = BuilderPageEdit::plan($input('Logo 82'), $this->entry(), [self::TARGET], self::EDIT, $media, $this->seam());
        $this->assertArrayNotHasKey('refusal', $same, (string) json_encode($same['refusal'] ?? null));
    }

    // -------------------------------------------------------------------------
    // The put-back after a failed edit runs in the page's write scope
    // -------------------------------------------------------------------------

    public function test_a_failed_edit_of_a_viewed_draft_is_put_back_inside_the_write_scope(): void
    {
        $baseFp  = $this->currentFp();
        $name    = ElementorStyleCache::PREFIX . 'local';
        $other   = ['state' => true, 'meta' => '6ac9bb9c3d4e5'];
        $viewed  = ['state' => false, 'children' => [self::OTHER => $other, self::TARGET => ['state' => true, 'meta' => []]]];
        $cleared = ['state' => false, 'children' => [self::OTHER => $other]];
        $armed   = null;
        // The put-back's regeneration, the one after Elementor's CSS was deleted,
        // clears the draft's own style-cache entry, rewrites its CSS row and drops
        // its element cache.
        $this->api->onRegenerate = function () use ($name, $viewed, $cleared, &$armed): void {
            if ($this->api->callsTo('deletePostCss') === []) {
                return;
            }
            $armed = array_column($this->hooks, 0);
            $this->fire('updated_option', $name, $viewed, $cleared);
            $this->fire('updated_post_meta', 990, self::TARGET, '_elementor_css', ['status' => 'file']);
            $this->fire('deleted_post_meta', [991], self::TARGET, ElementorDocument::KEY_ELEMENT_CACHE, '');
        };
        $r = $this->approvedWrite($this->input(self::ops()), self::untouchedChange());
        $this->api->onRegenerate = null;

        $this->assertSame(['verify_mismatch', true], [$r['code'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
        $this->assertIsArray($armed, 'the put-back regenerated the stylesheet');
        $this->assertContains('updated_option', $armed, 'the put-back runs inside the write scope');
        $this->assertContains('updated_post_meta', $armed, 'the put-back runs inside the write scope');
        $this->assertSame($baseFp, $this->currentFp(), 'the page is back');
        $this->assertSame([], $this->hooks, 'the write scope is disarmed');
    }

    public function test_a_put_back_that_writes_outside_the_page_is_side_effect_detected(): void
    {
        $baseFp                  = $this->currentFp();
        $this->api->onRegenerate = function (): void {
            if ($this->api->callsTo('deletePostCss') !== []) {
                $this->fire('updated_post_meta', 992, self::OTHER, '_elementor_css', ['status' => 'file']);
            }
        };
        $r = $this->approvedWrite($this->input(self::ops()), self::untouchedChange());
        $this->api->onRegenerate = null;

        $this->assertSame([false, 'side_effect_detected', true], [$r['ok'] ?? null, $r['code'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
        $this->assertStringContainsString('verify_mismatch', (string) $r['detail'], 'the answer still names what failed');
        $this->assertSame($baseFp, $this->currentFp(), 'the page is back');
        $this->assertSame(['failed', true], [AbilityLedger::get(self::EDIT)['phase'] ?? null, AbilityLedger::get(self::EDIT)['restored'] ?? null]);
        $this->assertSame([], $this->hooks, 'the write scope is disarmed');
    }

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

    /** A save filter that changes a node the edit did not touch. */
    private static function untouchedChange(): \Closure
    {
        return static function (array $elements): array {
            $elements[0]['elements'][1]['elements'][0]['settings']['editor'] = '<p>Changed by a filter</p>';

            return $elements;
        };
    }

    /**
     * @return array<string,mixed>
     */
    private static function design(): array
    {
        return [
            'colors'  => ['ground' => '#FFFFFF', 'surface' => '#F6F5F2', 'ink' => '#15171C', 'muted' => '#5A606C', 'accent' => '#0D7A63', 'on_accent' => '#FAFAF7'],
            'fonts'   => ['heading' => 'Manrope', 'body' => 'Work Sans'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ];
    }

    /**
     * DESIGNED_PAGE compiled as page-create compiles it, for a site with the
     * mobile and tablet breakpoints active.
     *
     * @return list<array<string,mixed>>
     */
    private static function designedPage(): array
    {
        $outline = OutlineV3::validate(json_decode(self::DESIGNED_PAGE, false));
        self::assertArrayHasKey('outline', $outline, (string) ($outline['detail'] ?? ''));
        $compiler = new ElementorSectionCompiler(DesignTokens::fromDesign(self::design()), ['mobile', 'tablet'], []);
        $mapped   = ElementorClassicMapper::map(['outline' => $outline['outline']], new IdSeed('b7e2a4c1-0000-4000-8000-000000000001'), [], true, $compiler);
        self::assertArrayHasKey('tree', $mapped, (string) ($mapped['detail'] ?? ''));

        return $mapped['tree'];
    }

    /** The designed page as the target, with every widget a designed page uses registered. */
    private function storeDesigned(): void
    {
        $this->storePage(self::designedPage());
        $this->api->widgetTypes = array_values(array_unique(array_merge($this->api->widgetTypes, ['icon-list', 'icon-box', 'icon', 'testimonial', 'nested-accordion', 'accordion', 'counter', 'image-box'])));
    }

    /**
     * @param list<array<string,mixed>> $ops    Operations.
     * @param bool                      $design Whether the input carries the page's design.
     */
    private function designedInput(array $ops, bool $design): string
    {
        $input = ['post_id' => self::TARGET, 'base_fingerprint' => $this->currentFp(), 'operations' => $ops];
        if ($design) {
            $input['design'] = self::design();
        }

        return (string) json_encode($input);
    }

    /**
     * The first projection node of $kind on the designed page, with $anchor when given.
     *
     * @return array<string,mixed>
     */
    private function nodeOf(string $kind, ?string $anchor = null): array
    {
        foreach (ElementorClassicMapper::project(self::designedPage())->nodes() as $node) {
            if ($node['kind'] === $kind && ($anchor === null || ($node['anchor'] ?? null) === $anchor)) {
                return $node;
            }
        }
        $this->fail('no ' . $kind . ' node ' . (string) $anchor);
    }

    private function adapter(): ElementorAdapter
    {
        return new ElementorAdapter($this->api, self::PRINCIPAL);
    }

    /**
     * The snapshot $requestId stored, decoded as the write decodes it.
     *
     * @return array<string,mixed>
     */
    private function snapshotOf(string $requestId): array
    {
        $loaded  = BuilderDocumentSnapshot::load($requestId);
        $decoded = BuilderDocumentSnapshot::decode((string) ($loaded['json'] ?? ''), $requestId, self::TARGET);
        $this->assertIsArray($decoded);

        return $decoded;
    }

    /**
     * Replaces the target's element data row with $tree.
     *
     * @param list<array<string,mixed>> $tree The tree.
     */
    private function storeData(array $tree): void
    {
        $this->rows->delete($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => ElementorDocument::KEY_DATA]);
        $this->rows->insert($this->rows->postmeta, ['post_id' => self::TARGET, 'meta_key' => ElementorDocument::KEY_DATA, 'meta_value' => (string) json_encode($tree)]);
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
