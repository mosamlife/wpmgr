<?php
/**
 * Designed-page edit golden: what the wpmgr/page-edit precheck answers for a
 * designed Elementor page (outline version 3), recorded for a spread of
 * operations in one shared fixture and asserted equal here. A twin of the
 * edit check in another language replays the same file.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   page-edit-designed-previews.json  the pages, the projection the AI reads
 *                                     for each, and for every operation case
 *                                     its input, the site facts it ran under
 *                                     and the precheck's answer
 *
 * Every page is the tree BuilderPageCreate::precheck() builds for a designed
 * input. Every case runs through BuilderPageEdit::plan(), the code the
 * wpmgr/page-edit command's precheck runs after it has read the signed
 * parameters, over the real ElementorAdapter and a stand-in for Elementor's
 * runtime; the command's own envelope is added the way the command adds it.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\BuilderPageEdit;
use WPMgr\Agent\Abilities\Builders\BuilderPageStructure;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageEdit
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageStructure
 */
final class PageEditDesignedGoldenTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/page-edit-designed-previews.json';

    /** The Router-driven page-edit golden, whose recorded digests the envelope is checked against. */
    private const ROUTER_FIXTURE = __DIR__ . '/../fixtures/ability-run/page-edit-preview.json';

    private const TARGET = 41;

    private const KIT = 4;

    private const PRINCIPAL = 2;

    /** The page-create request that created the pages. */
    private const CREATE = '11111111-2222-4333-8444-777777777777';

    /** The page-edit request of every case. */
    private const EDIT = '22222222-3333-4444-8555-888888888888';

    private const BOTH = ['mobile', 'tablet'];

    /** A base fingerprint no page has. */
    private const STALE = 'abababababababababababababababababababababababababababababababab';

    private const DESIGN = [
        'colors'  => ['ground' => '#F6F5F2', 'surface' => '#FFFFFF', 'ink' => '#15171C', 'muted' => '#5A606C', 'accent' => '#0D7A63', 'on_accent' => '#FFFFFF'],
        'fonts'   => ['heading' => 'Manrope', 'body' => 'Source Sans 3'],
        'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
        'radius'  => 'md',
        'density' => 'normal',
    ];

    /** The alt text each image has in the media library, by attachment id. */
    private const LIBRARY_ALT = [
        42  => 'Our team at work',
        43  => 'A quiet street',
        44  => 'Press clippings',
        101 => 'Acme logo', 102 => 'Birchwood logo', 103 => 'Cobalt logo', 104 => 'Dunmore logo', 105 => 'Everly logo',
        106 => 'Fernhill logo', 107 => 'Glenn logo',
        201 => 'Dana Whitfield', 202 => 'Marcus Lee',
    ];

    /** The type of every attachment the cases name; an id not listed is no attachment. */
    private const MIMES = [
        42  => 'image/jpeg', 43 => 'image/jpeg', 44 => 'image/png', 77 => 'application/pdf',
        101 => 'image/jpeg', 102 => 'image/jpeg', 103 => 'image/jpeg', 104 => 'image/jpeg', 105 => 'image/png',
        106 => 'image/webp', 107 => 'image/jpeg',
        201 => 'image/jpeg', 202 => 'image/jpeg',
    ];

    private const EXTENSION_OF = ['image/jpeg' => 'jpg', 'image/png' => 'png', 'image/webp' => 'webp', 'image/gif' => 'gif', 'image/avif' => 'avif'];

    private const WIDGETS = ['heading', 'text-editor', 'button', 'image', 'divider', 'spacer', 'icon-box', 'counter', 'testimonial', 'accordion', 'nested-accordion', 'icon-list', 'icon', 'image-box', 'tabs'];

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    /** @var array<string,mixed> */
    private array $options = [];

    private FakeBuilderWpdb $rows;

    private EngineWpdb $wpdb;

    private FakeElementorApi $api;

    /** @var array<int,string> */
    private array $libraryAlt = self::LIBRARY_ALT;

    /** @var array<int,string> */
    private array $mimes = self::MIMES;

    /**
     * The compiled pages, by name: {title, create_input, create_facts, tree}.
     *
     * @var array<string,array<string,mixed>>
     */
    private array $pages = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->hooks      = [];
        $this->options    = [];
        $this->libraryAlt = self::LIBRARY_ALT;
        $this->mimes      = self::MIMES;

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
        $theme = new class () {
            /** @return array<string,string> */
            public function get_page_templates($post = null, $post_type = 'page'): array
            {
                return ['elementor_header_footer' => 'Elementor Full Width', 'elementor_canvas' => 'Elementor Canvas'];
            }
        };
        $snap = static fn ($name): bool => str_starts_with((string) $name, \WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot::OPTION_PREFIX);
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
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_cache_delete')->justReturn(true);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wc_get_page_id')->justReturn(-1);
        Functions\when('wp_get_post_autosave')->justReturn(false);
        Functions\when('wp_check_post_lock')->justReturn(false);
        Functions\when('wp_slash')->alias($slash);
        Functions\when('wp_get_theme')->justReturn($theme);
        Functions\when('wp_is_block_theme')->justReturn(false);
        Functions\when('get_post_meta')->alias(fn ($id, $key = '', $single = false) => $key === BuilderPageCreate::ALT_META_KEY ? ($this->libraryAlt[(int) $id] ?? '') : '');
        Functions\when('get_post')->alias(fn ($id) => isset($this->mimes[(int) $id]) ? (object) ['ID' => (int) $id, 'post_type' => 'attachment'] : null);
        Functions\when('current_user_can')->justReturn(true);
        Functions\when('get_post_mime_type')->alias(fn ($id) => $this->mimes[(int) $id] ?? false);

        $this->api               = new FakeElementorApi();
        $this->api->activeKitId  = self::KIT;
        $this->api->widgetTypes  = self::WIDGETS;
        $this->api->fontFamilies = ['Manrope', 'Inter', 'Lora', 'Roboto', 'Source Sans 3', 'Playfair Display'];
        $this->usePage('landing', [], 'x');
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // The fixture
    // -------------------------------------------------------------------------

    public function test_fixture_is_what_the_code_produces(): void
    {
        $want = $this->fixtureText();
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::FIXTURE, $want), 'could not write page-edit-designed-previews.json');
        }
        $this->assertFileExists(self::FIXTURE, 'page-edit-designed-previews.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $got = file_get_contents(self::FIXTURE);
        $this->assertIsString($got);
        $this->assertSame($want, $got, 'page-edit-designed-previews.json differs from what the precheck answers now; regenerate with WPMGR_WRITE_FIXTURES=1');
    }

    /**
     * Each case replays from what the file records alone: the stored page, the
     * input, the facts and the images, not the generator's memory of them.
     */
    public function test_every_case_replays_from_what_it_records(): void
    {
        $doc = $this->fixture();
        $this->assertNotEmpty($doc['cases'], 'the golden holds no case');
        $this->libraryAlt = array_map('strval', $doc['library_alt']);
        $this->mimes      = array_map('strval', $doc['attachments']);
        foreach ($doc['cases'] as $case) {
            $page = $doc['pages'][$case['page']];
            $this->usePage($case['page'], $page['before_tree'], $page['title']);
            $before = $this->fingerprint();
            $this->assertSame($page['base_fingerprint'], $before, $case['name'] . ': the stored page has the recorded fingerprint');
            $media  = $case['media'];
            $answer = $this->precheck($doc['entry'], $case['input'], $case['facts'], static fn (array $ids): array => ['facts' => $media], $ids);
            if ($media !== []) {
                $this->assertSame(array_column($media, 'id'), $ids, $case['name'] . ': the precheck asks for the images the file records');
            }
            // The file spells an empty object as [] once decoded to arrays; the byte comparison above holds the difference.
            $this->assertSame($case['answer'], json_decode((string) json_encode($answer), true), $case['name'] . ' answer');
            // A precheck writes nothing.
            $this->assertSame($before, $this->fingerprint(), $case['name'] . ': the page is as it was');
            $this->assertSame([], $this->rows->optionRows(), $case['name'] . ': a precheck stores nothing');
            $this->assertSame([], $this->hooks, $case['name'] . ': a precheck arms no write scope');
        }
    }

    /** Every case answers what it was written to answer. */
    public function test_every_case_answers_what_it_expects(): void
    {
        $seen = [];
        foreach ($this->fixture()['cases'] as $case) {
            $got = ($case['answer']['ok'] ?? null) === true ? 'prechecked' : $this->expectKey($case['answer']);
            $this->assertSame($case['expect'], $got, $case['name']);
            $seen[$case['name']] = true;
        }
        $this->assertCount(count($this->fixture()['cases']), $seen, 'case names are unique');
    }

    /**
     * Every answer binds what it carries, from the file alone: the tree's
     * bytes, the preview digest over them and the precheck digest over that.
     */
    public function test_every_success_binds_its_tree_and_digests(): void
    {
        $doc = $this->fixture();
        $n   = 0;
        foreach ($doc['cases'] as $case) {
            $a = $case['answer'];
            if (($a['ok'] ?? null) !== true) {
                continue;
            }
            ++$n;
            $pv        = $a['preview'];
            $treeJson  = (string) json_encode($pv['tree']);
            $this->assertSame(hash('sha256', $treeJson), $pv['tree_sha256'], $case['name'] . ' tree_sha256');
            $digest = BuilderPageEdit::previewDigest($pv['builder'], $pv['builder_version'], $pv['post_id'], $a['base_fingerprint'], $treeJson);
            $this->assertSame($digest, $a['preview_digest'], $case['name'] . ' preview_digest');
            $this->assertSame(self::precheckDigest($doc['entry_sha256'], hash('sha256', $case['input']), $a['base_fingerprint'], $digest), $a['precheck_digest'], $case['name'] . ' precheck_digest');
            $this->assertSame($doc['pages'][$case['page']]['base_fingerprint'], $a['base_fingerprint'], $case['name'] . ' base_fingerprint');
        }
        $this->assertGreaterThan(20, $n, 'the golden holds the successes it exists for');
    }

    /**
     * The envelope formula of this test is the command's: the digest the
     * Router-driven golden recorded recomputes from its own fields.
     */
    public function test_the_precheck_digest_formula_is_the_commands(): void
    {
        $raw = file_get_contents(self::ROUTER_FIXTURE);
        $this->assertIsString($raw);
        $fx = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
        $this->assertSame(
            $fx['precheck_digest'],
            self::precheckDigest($fx['entry_sha256'], hash('sha256', $fx['input']), $fx['base_fingerprint'], $fx['preview_digest'])
        );
    }

    public function test_the_golden_holds_no_float(): void
    {
        $doc = $this->fixture();
        foreach ($doc['pages'] as $name => $page) {
            $this->assertSame([], self::floatPaths($page), $name . ': a number written as a float depends on the host float settings');
        }
        foreach ($doc['cases'] as $case) {
            $this->assertSame([], self::floatPaths($case['answer']), $case['name'] . ': a number written as a float depends on the host float settings');
        }
    }

    // -------------------------------------------------------------------------
    // The pages, and the projection the AI reads
    // -------------------------------------------------------------------------

    /** A stored page is what page-create's precheck builds for the input the file records. */
    public function test_every_page_is_what_the_page_create_precheck_builds(): void
    {
        foreach ($this->fixture()['pages'] as $name => $page) {
            if (!isset($page['create_input'])) {
                continue;
            }
            $this->assertSame($page['before_tree'], $this->createTree($page['create_input'], $page['create_facts']), $name);
        }
    }

    /** The projection the AI reads names every kind of node a designed page has, and offers paths only where a pattern does. */
    public function test_the_projection_names_sections_patterns_and_locked_nodes(): void
    {
        $doc   = $this->fixture();
        $kinds = [];
        $offer = [];
        foreach ($doc['pages'] as $page) {
            foreach ($page['structure']['nodes'] as $node) {
                $kinds[$node['kind']] = true;
                if ($node['kind'] === 'pattern') {
                    $offer[$node['variant'] === 'background' || $node['variant'] === 'band' || $node['variant'] === 'row' ? $node['variant'] : 'items'][] = $node['editable'];
                }
            }
            $this->assertSame(self::FORMAT_HEAD, array_keys(array_diff_key($page['structure'], ['nodes' => 1])), 'the head of the answer');
        }
        foreach (['section', 'pattern', 'locked', 'heading', 'paragraph', 'buttons'] as $kind) {
            $this->assertArrayHasKey($kind, $kinds, 'the golden projects a ' . $kind);
        }
        // The hero, the call to action and the logos offer no path.
        foreach (['background', 'band', 'row'] as $name) {
            $this->assertNotEmpty($offer[$name] ?? [], $name);
            $this->assertSame([], array_values(array_unique(array_merge(...$offer[$name]))), $name . ' offers no path');
        }
    }

    /**
     * Every path every pattern of the landing page offers is set by one
     * successful case at least; no other path is set.
     */
    public function test_the_golden_sets_every_path_every_pattern_offers(): void
    {
        $doc     = $this->fixture();
        $offered = [];
        foreach ($doc['pages']['landing']['structure']['nodes'] as $node) {
            if ($node['kind'] === 'pattern') {
                foreach ($node['editable'] as $path) {
                    $offered[$node['ref'] . ' ' . $path] = true;
                }
            }
        }
        $this->assertGreaterThan(30, count($offered), 'the landing page offers the paths of five patterns');
        $set = [];
        foreach ($doc['cases'] as $case) {
            if ($case['page'] !== 'landing' || ($case['answer']['ok'] ?? null) !== true) {
                continue;
            }
            foreach ((array) json_decode($case['input'], true)['operations'] as $op) {
                if ($op['op'] === 'set_text' && isset($offered[$op['ref'] . ' ' . $op['field']])) {
                    $set[$op['ref'] . ' ' . $op['field']] = true;
                }
            }
        }
        $this->assertSame([], array_keys(array_diff_key($offered, $set)), 'a path no successful case sets');
        // Each pattern's own words, so a renamed path cannot pass by naming nothing.
        $kinds = [];
        foreach (array_keys($set) as $key) {
            $kinds[preg_replace('/\.[0-9]+(\.|$)/', '.N$1', substr($key, strpos($key, ' ') + 1))] = true;
        }
        foreach (['features.N.title', 'features.N.text', 'testimonials.N.quote', 'testimonials.N.name', 'testimonials.N.role', 'plans.N.name', 'plans.N.price',
            'plans.N.period', 'plans.N.features.N', 'plans.N.cta.text', 'plans.N.cta.url', 'faq.N.question', 'faq.N.answer', 'stats.N.value', 'stats.N.suffix', 'stats.N.label'] as $kind) {
            $this->assertArrayHasKey($kind, $kinds, $kind);
        }
    }

    /** The golden holds each operation, each place an insert can stand, and the refusals the designed rules give. */
    public function test_the_golden_covers_the_operations_and_the_refusals(): void
    {
        $ops     = [];
        $places  = [];
        $refused = [];
        foreach ($this->fixture()['cases'] as $case) {
            $input = json_decode($case['input'], true);
            foreach ((array) ($input['operations'] ?? []) as $op) {
                $ops[$op['op']] = true;
                if ($op['op'] === 'insert') {
                    $places[array_key_exists('after', $op) ? 'after' : (array_key_exists('before', $op) ? 'before' : 'into')] = true;
                }
            }
            if (($case['answer']['ok'] ?? null) === false) {
                $refused[$this->expectKey($case['answer'])] = true;
            }
        }
        foreach (['set_text', 'insert', 'replace', 'move', 'remove'] as $op) {
            $this->assertArrayHasKey($op, $ops, $op);
        }
        foreach (['after', 'before', 'into'] as $place) {
            $this->assertArrayHasKey($place, $places, $place);
        }
        foreach ([
            'design_not_ready', 'design_contrast_low', 'hero_misplaced/hero_not_first', 'layout_invalid/second_hero',
            'node_not_editable/section_at_top_only', 'node_not_editable/locked', 'node_not_editable/holds_locked', 'node_not_editable/field_not_offered',
            'anchor_duplicate/anchor_twice', 'anchor_in_use/section_linked', 'link_anchor_missing/link_leads_nowhere', 'ops_invalid/ref_gone',
            'background_not_image', 'image_alt_from_library', 'conflict/changed_since_read', 'bad_input',
        ] as $want) {
            $found = false;
            foreach (array_keys($refused) as $key) {
                $found = $found || $key === $want || strncmp($key, $want . '/', strlen($want) + 1) === 0;
            }
            $this->assertTrue($found, 'a refusal ' . $want . ' in ' . implode(', ', array_keys($refused)));
        }
    }

    // -------------------------------------------------------------------------
    // The input of the golden
    // -------------------------------------------------------------------------

    /**
     * The compiled pages, by name.
     *
     * @return array<string,array{title:string,create_input:string,create_facts:array<string,mixed>}>
     */
    private static function pageInputs(): array
    {
        $landing = [
            ['type' => 'section', 'anchor' => 'top', 'width' => 'full', 'tone' => 'inverse', 'background' => ['attachment_id' => 42, 'overlay' => 'strong', 'focal' => 'center'], 'spacing' => 'xl', 'align' => 'center', 'min_height' => 'screen', 'children' => [
                ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'background', 'children' => [
                    ['type' => 'heading', 'level' => 1, 'text' => 'Websites that earn their keep'],
                    ['type' => 'paragraph', 'text' => 'We design, build and look after sites for small businesses.'],
                    ['type' => 'buttons', 'buttons' => [['text' => 'See pricing', 'url' => '#pricing', 'style' => 'fill'], ['text' => 'Talk to us', 'url' => '/contact', 'style' => 'outline']]],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'clients', 'tone' => 'surface', 'spacing' => 'md', 'children' => [
                ['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => [
                    ['attachment_id' => 101, 'alt' => 'Acme logo'], ['attachment_id' => 102, 'alt' => 'Birchwood logo'], ['attachment_id' => 103, 'alt' => 'Cobalt logo'],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'features', 'tone' => 'ground', 'children' => [
                ['type' => 'heading', 'level' => 2, 'text' => 'What you get'],
                ['type' => 'pattern', 'pattern' => 'features', 'variant' => 'split', 'features' => [
                    ['icon' => 'bolt', 'title' => 'Fast setup', 'text' => 'Go live in an afternoon, not a quarter.'],
                    ['icon' => 'shield', 'title' => 'Safe by default', 'text' => 'Every change is reviewed before it goes out.'],
                    ['icon' => 'chart', 'title' => 'Clear numbers', 'text' => 'See what each page earns, week by week.'],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'numbers', 'tone' => 'surface', 'children' => [
                ['type' => 'pattern', 'pattern' => 'stats', 'variant' => 'counters', 'stats' => [
                    ['value' => 1200, 'suffix' => '+', 'label' => 'sites launched'],
                    ['value' => 98, 'suffix' => '%', 'label' => 'would recommend us'],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'stories', 'tone' => 'ground', 'children' => [
                ['type' => 'heading', 'level' => 2, 'text' => 'What customers say'],
                ['type' => 'pattern', 'pattern' => 'testimonials', 'variant' => 'featured', 'testimonials' => [
                    ['quote' => 'The new site paid for itself in a month.', 'name' => 'Dana Whitfield', 'role' => 'Owner, Whitfield Bakery', 'attachment_id' => 201],
                    ['quote' => 'Updates that used to take a week now take an hour.', 'name' => 'Marcus Lee', 'role' => 'Operations lead'],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'pricing', 'tone' => 'surface', 'spacing' => 'xl', 'children' => [
                ['type' => 'heading', 'level' => 2, 'text' => 'Simple pricing'],
                ['type' => 'pattern', 'pattern' => 'pricing', 'variant' => 'three-plans', 'plans' => [
                    ['name' => 'Starter', 'price' => '19', 'period' => 'month', 'features' => ['1 site', 'Email support'], 'cta' => ['text' => 'Choose Starter', 'url' => '#signup'], 'highlighted' => false],
                    ['name' => 'Team', 'price' => '49', 'features' => ['5 sites'], 'cta' => ['text' => 'Choose Team', 'url' => '/contact'], 'highlighted' => true],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'faq', 'tone' => 'ground', 'children' => [
                ['type' => 'heading', 'level' => 2, 'text' => 'Questions we hear'],
                ['type' => 'pattern', 'pattern' => 'faq', 'variant' => 'accordion', 'faq' => [
                    ['question' => 'How long does a launch take?', 'answer' => 'Most pages are live within a week of approval.'],
                    ['question' => 'Can I edit the page myself?', 'answer' => 'Yes. Every page opens in the visual editor.'],
                ]],
            ]],
            ['type' => 'section', 'anchor' => 'signup', 'tone' => 'accent', 'align' => 'center', 'children' => [
                ['type' => 'pattern', 'pattern' => 'cta', 'variant' => 'band', 'children' => [
                    ['type' => 'heading', 'level' => 2, 'text' => 'Ready when you are'],
                    ['type' => 'paragraph', 'text' => 'Start a trial today and keep everything if you stay.'],
                    ['type' => 'buttons', 'buttons' => [['text' => 'Start free', 'url' => '/signup', 'style' => 'fill'], ['text' => 'Book a call', 'url' => 'https://example.com/call', 'style' => 'text']]],
                ]],
            ]],
        ];
        $small = [
            ['type' => 'section', 'anchor' => 'top', 'children' => [
                ['type' => 'paragraph', 'text' => 'Welcome.'],
                ['type' => 'buttons', 'buttons' => [['text' => 'See plans', 'url' => '#plans']]],
            ]],
            ['type' => 'section', 'anchor' => 'plans', 'tone' => 'surface', 'children' => [
                ['type' => 'pattern', 'pattern' => 'pricing', 'variant' => 'three-plans', 'plans' => [
                    ['name' => 'Starter', 'price' => '$19', 'period' => 'per month', 'features' => ['One site', 'Email support'], 'cta' => ['text' => 'Choose Starter', 'url' => 'https://example.com/start']],
                    ['name' => 'Growth', 'price' => '$29', 'features' => ['Five sites'], 'cta' => ['text' => 'Choose Growth', 'url' => 'https://example.com/grow']],
                ]],
            ]],
        ];
        $page = static fn (string $title, array $outline): array => [
            'title'        => $title,
            'create_input' => (string) json_encode([
                'post_type' => 'page', 'editor' => 'builder:elementor', 'elementor_format' => 'classic', 'title' => $title,
                'outline_version' => 3, 'design' => self::DESIGN, 'outline' => $outline, 'page_frame' => 'full_width',
            ], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR),
            'create_facts' => ['elementor_version' => '3.35.9', 'active_breakpoints' => self::BOTH],
        ];

        return ['landing' => $page('Launch page', $landing), 'small' => $page('Pricing', $small)];
    }

    /** The head of a page-structure answer, in its order. */
    private const FORMAT_HEAD = ['post_id', 'builder', 'builder_version', 'format', 'status', 'editable', 'open_state', 'element_cache_active', 'base_fingerprint', 'node_count', 'truncated'];

    /**
     * The tree page-create's precheck builds for an input on a site with these facts.
     *
     * @param string              $input The page-create input text.
     * @param array<string,mixed> $facts elementor_version, active_breakpoints.
     * @return list<array<string,mixed>>
     */
    private function createTree(string $input, array $facts): array
    {
        $this->api->version           = $facts['elementor_version'];
        $this->api->activeBreakpoints = $facts['active_breakpoints'];
        $checked                      = PageCreateBuilder::validate(json_decode($input, false, 32));
        $this->assertArrayHasKey('spec', $checked, 'the input is valid: ' . json_encode($checked));
        $media   = array_map(fn (int $id): array => self::mediaFact($id, $this->mimes[$id] ?? 'image/jpeg'), PageCreateBuilder::mediaIds($checked['spec']));
        $adapter = new ElementorAdapter($this->api, self::PRINCIPAL);
        $answer  = BuilderPageCreate::precheck($checked['spec'], $adapter, $adapter->facts(), $media, self::CREATE);
        $this->assertArrayNotHasKey('refusal', $answer, 'the page compiles: ' . json_encode($answer['refusal'] ?? null));

        return $answer['tree'];
    }

    /**
     * What the site resolves an attachment id to.
     *
     * @return array<string,mixed>
     */
    private static function mediaFact(int $id, string $mime): array
    {
        return [
            'id'           => $id,
            'url'          => sprintf('https://example.com/wp-content/uploads/2026/10/img-%d.%s', $id, self::EXTENSION_OF[$mime] ?? 'jpg'),
            'filename'     => sprintf('img-%d.%s', $id, self::EXTENSION_OF[$mime] ?? 'jpg'),
            'mime'         => $mime,
            'width'        => 1600,
            'height'       => 900,
            'modified_gmt' => '2026-10-01 09:00:00',
        ];
    }

    /** The sections a section insert is given. */
    private const GUARANTEE = [
        ['type' => 'section', 'anchor' => 'guarantee', 'tone' => 'accent', 'align' => 'center', 'children' => [
            ['type' => 'heading', 'level' => 2, 'text' => 'Our promise'],
            ['type' => 'paragraph', 'text' => 'Cancel in the first month and we refund every cent.'],
            ['type' => 'pattern', 'pattern' => 'features', 'variant' => 'grid', 'features' => [
                ['icon' => 'shield', 'title' => 'Refunds', 'text' => 'Every cent, no questions.'],
                ['icon' => 'bolt', 'title' => 'Speed', 'text' => 'Answers within a day.'],
                ['icon' => 'users', 'title' => 'People', 'text' => 'A named contact.'],
            ]],
            ['type' => 'buttons', 'buttons' => [['text' => 'Read the terms', 'url' => '#faq', 'style' => 'text']]],
        ]],
    ];

    private const PRESS_AND_PARTNERS = [
        ['type' => 'section', 'anchor' => 'press', 'tone' => 'inverse', 'background' => ['attachment_id' => 44, 'overlay' => 'soft', 'focal' => 'top'], 'children' => [
            ['type' => 'heading', 'level' => 2, 'text' => 'As seen in'],
            ['type' => 'paragraph', 'text' => 'Where we were mentioned.'],
        ]],
        ['type' => 'section', 'anchor' => 'partners', 'tone' => 'surface', 'children' => [
            ['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => [
                ['attachment_id' => 104, 'alt' => 'Dunmore logo'], ['attachment_id' => 105, 'alt' => 'Everly logo'], ['attachment_id' => 106, 'alt' => 'Fernhill logo'],
            ]],
        ]],
    ];

    private const HERO = [
        ['type' => 'section', 'anchor' => 'welcome', 'tone' => 'inverse', 'align' => 'center', 'children' => [
            ['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => [
                ['type' => 'heading', 'level' => 1, 'text' => 'Ship faster'],
                ['type' => 'paragraph', 'text' => 'Pages that stay on brand.'],
                ['type' => 'buttons', 'buttons' => [['text' => 'See plans', 'url' => '#plans', 'style' => 'fill']]],
            ]],
        ]],
    ];

    /**
     * The operation cases. A case names its page, its operations, whether the
     * input carries the page's design (or a design of its own), the active
     * breakpoints of the site and what it must answer.
     *
     * @return list<array<string,mixed>>
     */
    private function rows(): array
    {
        $rows = [];
        $add  = static function (string $name, string $note, string $page, array $ops, string $expect, $design = false, array $extra = []) use (&$rows): void {
            $rows[] = ['name' => $name, 'note' => $note, 'page' => $page, 'ops' => $ops, 'expect' => $expect, 'design' => $design] + $extra;
        };
        $L   = fn (array $where): string => $this->ref('landing', $where);
        $S   = fn (array $where): string => $this->ref('small', $where);
        $sec = fn (string $page, string $anchor): string => $this->ref($page, ['kind' => 'section', 'anchor' => $anchor]);
        $pat = fn (string $page, string $pattern): string => $this->ref($page, ['kind' => 'pattern', 'pattern' => $pattern]);

        // Every path every pattern of the landing page offers, one per node per call.
        foreach ($this->pathCalls() as $n => $ops) {
            $add(sprintf('landing-set-text-paths-%02d', $n + 1), 'Changes one text of each item pattern, by pattern path.', 'landing', $ops, 'prechecked');
        }
        $add('landing-set-text-blocks-in-sections', 'Changes a hero heading, paragraph and button, a section heading and a call to action button, by plain field.', 'landing', [
            ['op' => 'set_text', 'ref' => $L(['kind' => 'heading', 'level' => 1]), 'field' => 'text', 'text' => 'Websites that pay their way'],
            ['op' => 'set_text', 'ref' => $L(['kind' => 'paragraph']), 'field' => 'text', 'text' => 'We build, launch and look after sites.'],
            ['op' => 'set_text', 'ref' => $L(['kind' => 'buttons']), 'field' => 'url', 'text' => '/features'],
            ['op' => 'set_text', 'ref' => $L(['kind' => 'heading', 'parent' => $sec('landing', 'features')]), 'field' => 'text', 'text' => 'Everything you get'],
            ['op' => 'set_text', 'ref' => $L(['kind' => 'heading', 'parent' => $pat('landing', 'cta')]), 'field' => 'text', 'text' => 'Ready, when you are'],
        ], 'prechecked');
        $add('landing-insert-section-after-pricing', 'A designed section with a features pattern is compiled with the page design and placed after a section.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => self::GUARANTEE],
        ], 'prechecked', true);
        $add('landing-insert-sections-with-a-background-and-logos', 'Two sections, one on a background picture and one holding logos, are placed before a section; the images are resolved.', 'landing', [
            ['op' => 'insert', 'before' => $sec('landing', 'faq'), 'outline' => self::PRESS_AND_PARTNERS],
        ], 'prechecked', true);
        $add('landing-insert-section-on-a-mobile-only-site', 'The same section on a site with only the mobile breakpoint: no tablet value is written.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => self::GUARANTEE],
        ], 'prechecked', true, ['breakpoints' => ['mobile']]);
        $add('landing-insert-section-on-a-site-with-no-breakpoint', 'The same section on a site that has neither breakpoint active.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => self::GUARANTEE],
        ], 'prechecked', true, ['breakpoints' => []]);
        $add('landing-insert-blocks-into-a-section', 'A paragraph and buttons placed last in a section take its tone.', 'landing', [
            ['op' => 'insert', 'into' => $sec('landing', 'features'), 'position' => 'last', 'outline' => [
                ['type' => 'paragraph', 'text' => 'And much more.'],
                ['type' => 'buttons', 'buttons' => [['text' => 'See the plans', 'url' => '#pricing', 'style' => 'outline']]],
            ]],
        ], 'prechecked', true);
        $add('landing-replace-the-faq-pattern', 'A faq pattern replaced by one with three questions.', 'landing', [
            ['op' => 'replace', 'ref' => $pat('landing', 'faq'), 'outline' => [
                ['type' => 'pattern', 'pattern' => 'faq', 'variant' => 'accordion', 'faq' => [
                    ['question' => 'How long does a launch take?', 'answer' => 'About a week.'],
                    ['question' => 'Can I edit it myself?', 'answer' => 'Yes, in the visual editor.'],
                    ['question' => 'Can I cancel?', 'answer' => 'At any time.'],
                ]],
            ]],
        ], 'prechecked', true);
        $add('landing-replace-the-pricing-pattern', 'A pricing pattern replaced by one with three plans.', 'landing', [
            ['op' => 'replace', 'ref' => $pat('landing', 'pricing'), 'outline' => [
                ['type' => 'pattern', 'pattern' => 'pricing', 'variant' => 'three-plans', 'plans' => [
                    ['name' => 'Solo', 'price' => '9', 'period' => 'month', 'features' => ['1 site'], 'cta' => ['text' => 'Choose Solo', 'url' => '#signup']],
                    ['name' => 'Team', 'price' => '49', 'period' => 'month', 'features' => ['5 sites', 'Priority support'], 'cta' => ['text' => 'Choose Team', 'url' => '#signup'], 'highlighted' => true],
                    ['name' => 'Business', 'price' => '99', 'period' => 'month', 'features' => ['25 sites', 'Phone support', 'Daily reports'], 'cta' => ['text' => 'Choose Business', 'url' => '#signup']],
                ]],
            ]],
        ], 'prechecked', true);
        $add('landing-replace-a-section-with-the-same-anchor', 'A section replaced by a new one that keeps its anchor.', 'landing', [
            ['op' => 'replace', 'ref' => $sec('landing', 'numbers'), 'outline' => [
                ['type' => 'section', 'anchor' => 'numbers', 'tone' => 'inverse', 'children' => [
                    ['type' => 'pattern', 'pattern' => 'stats', 'variant' => 'counters', 'stats' => [
                        ['value' => 1500, 'suffix' => '+', 'label' => 'sites launched'],
                        ['value' => 97, 'suffix' => '%', 'label' => 'would recommend us'],
                        ['value' => 24, 'suffix' => 'h', 'label' => 'average reply time'],
                    ]],
                ]],
            ]],
        ], 'prechecked', true);
        $add('landing-move-a-section', 'A section moved after another.', 'landing', [
            ['op' => 'move', 'ref' => $sec('landing', 'clients'), 'after' => $sec('landing', 'features')],
        ], 'prechecked');
        $add('landing-move-a-pattern-into-another-section', 'A pattern moved from its section to stand before the pattern of another.', 'landing', [
            ['op' => 'move', 'ref' => $pat('landing', 'stats'), 'before' => $pat('landing', 'faq')],
        ], 'prechecked');
        $add('landing-remove-a-section', 'A section nothing links to is removed.', 'landing', [
            ['op' => 'remove', 'ref' => $sec('landing', 'clients')],
        ], 'prechecked');
        $add('landing-a-batch-of-every-kind', 'A text, an insert, a move and a remove in one call.', 'landing', [
            ['op' => 'set_text', 'ref' => $pat('landing', 'pricing'), 'field' => 'plans.1.price', 'text' => '$59'],
            ['op' => 'insert', 'after' => $sec('landing', 'faq'), 'outline' => self::GUARANTEE],
            ['op' => 'move', 'ref' => $sec('landing', 'numbers'), 'before' => $sec('landing', 'features')],
            ['op' => 'remove', 'ref' => $sec('landing', 'clients')],
        ], 'prechecked', true);

        $add('small-insert-a-hero-before-the-first-section', 'A hero section placed before the first section of a page that has none.', 'small', [
            ['op' => 'insert', 'before' => $sec('small', 'top'), 'outline' => self::HERO],
        ], 'prechecked', true);
        $add('small-set-text-links', 'A button given a path on the site, and a plan\'s button the address of a section the page has.', 'small', [
            ['op' => 'set_text', 'ref' => $S(['kind' => 'buttons']), 'field' => 'url', 'text' => '/start'],
            ['op' => 'set_text', 'ref' => $pat('small', 'pricing'), 'field' => 'plans.0.cta.url', 'text' => '#top'],
        ], 'prechecked');

        // Refusals.
        $add('refuse-a-section-without-the-design', 'A section sent with no design: the page is compiled with the design of the page, and the input has none.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => self::GUARANTEE],
        ], 'design_not_ready');
        $add('refuse-a-design-with-low-contrast', 'A design whose ink is too close to the ground.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => self::GUARANTEE],
        ], 'design_contrast_low', array_replace_recursive(self::DESIGN, ['colors' => ['ink' => '#EEEEEE']]));
        $add('refuse-a-hero-below-a-section', 'A hero placed after the first section.', 'small', [
            ['op' => 'insert', 'after' => $sec('small', 'top'), 'outline' => self::HERO],
        ], 'hero_misplaced/hero_not_first', true);
        $add('refuse-a-second-hero', 'A hero placed first on a page that has one.', 'landing', [
            ['op' => 'insert', 'before' => $sec('landing', 'top'), 'outline' => self::HERO],
        ], 'layout_invalid/second_hero', true);
        $add('refuse-a-section-inside-a-section', 'A section inserted into a section.', 'landing', [
            ['op' => 'insert', 'into' => $sec('landing', 'features'), 'outline' => self::GUARANTEE],
        ], 'node_not_editable/section_at_top_only', true);
        $add('refuse-an-anchor-the-page-has', 'A section whose anchor is a section already on the page.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => [
                ['type' => 'section', 'anchor' => 'faq', 'children' => [['type' => 'paragraph', 'text' => 'Again.']]],
            ]],
        ], 'anchor_duplicate/anchor_twice', true);
        $add('refuse-removing-a-section-a-link-leads-to', 'A section removed while a button links to its anchor.', 'small', [
            ['op' => 'remove', 'ref' => $sec('small', 'plans')],
        ], 'anchor_in_use/section_linked');
        $add('refuse-a-link-that-leads-nowhere', 'A button given the address of a section the page does not have.', 'landing', [
            ['op' => 'set_text', 'ref' => $L(['kind' => 'buttons']), 'field' => 'url', 'text' => '#nowhere'],
        ], 'link_anchor_missing/link_leads_nowhere');
        $add('refuse-a-background-that-is-not-an-image', 'A section on a background that is a PDF.', 'landing', [
            ['op' => 'set_text', 'ref' => $pat('landing', 'pricing'), 'field' => 'plans.0.price', 'text' => '$9'],
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => [
                ['type' => 'section', 'anchor' => 'press', 'background' => ['attachment_id' => 77], 'children' => [['type' => 'paragraph', 'text' => 'Where we were mentioned.']]],
            ]],
        ], 'background_not_image', true);
        $add('refuse-a-logo-whose-alt-is-not-the-librarys', 'A logo given alt text other than the one saved with its image.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => [
                ['type' => 'section', 'anchor' => 'partners', 'children' => [
                    ['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => [
                        ['attachment_id' => 104, 'alt' => 'Dunmore logo'], ['attachment_id' => 105, 'alt' => 'Another name'], ['attachment_id' => 106, 'alt' => 'Fernhill logo'],
                    ]],
                ]],
            ]],
        ], 'image_alt_from_library', true);
        $add('refuse-a-path-the-pattern-does-not-offer', 'A pricing path past the last plan.', 'landing', [
            ['op' => 'set_text', 'ref' => $pat('landing', 'pricing'), 'field' => 'plans.7.price', 'text' => '$1'],
        ], 'node_not_editable/field_not_offered');
        $add('refuse-a-path-on-a-hero', 'A hero offers no path.', 'landing', [
            ['op' => 'set_text', 'ref' => $pat('landing', 'hero'), 'field' => 'features.0.title', 'text' => 'Nothing'],
        ], 'node_not_editable/field_not_offered');
        $add('refuse-a-stale-base-fingerprint', 'An input read from a page that has since changed.', 'landing', [
            ['op' => 'remove', 'ref' => $sec('landing', 'clients')],
        ], 'conflict/changed_since_read', false, ['base' => self::STALE]);
        $add('refuse-a-pattern-with-too-few-items', 'A features pattern with two items.', 'landing', [
            ['op' => 'insert', 'after' => $sec('landing', 'pricing'), 'outline' => [
                ['type' => 'section', 'anchor' => 'guarantee', 'children' => [
                    ['type' => 'pattern', 'pattern' => 'features', 'variant' => 'grid', 'features' => [
                        ['icon' => 'bolt', 'title' => 'One', 'text' => 'First.'], ['icon' => 'shield', 'title' => 'Two', 'text' => 'Second.'],
                    ]],
                ]],
            ]],
        ], 'bad_input', true);
        $add('refuse-an-edit-inside-a-removed-section', 'A text set inside a section the same call removes.', 'landing', [
            ['op' => 'remove', 'ref' => $sec('landing', 'faq')],
            ['op' => 'set_text', 'ref' => $pat('landing', 'faq'), 'field' => 'faq.0.question', 'text' => 'Gone?'],
        ], 'ops_invalid/ref_gone');

        // The page whose pricing pattern a person restructured in Elementor.
        $locked = $this->ref('restructured', ['kind' => 'locked']);
        $add('restructured-insert-after-a-locked-pattern', 'A locked node may be an anchor.', 'restructured', [
            ['op' => 'insert', 'after' => $locked, 'outline' => [['type' => 'paragraph', 'text' => 'Prices include tax.']]],
        ], 'prechecked', true);
        $add('refuse-removing-a-locked-pattern', 'A restructured pattern is locked and never the target of an operation.', 'restructured', [
            ['op' => 'remove', 'ref' => $locked],
        ], 'node_not_editable/locked');
        $add('refuse-replacing-a-section-that-holds-a-locked-pattern', 'A locked node never goes with its section.', 'restructured', [
            ['op' => 'replace', 'ref' => $this->ref('restructured', ['kind' => 'section', 'anchor' => 'plans']), 'outline' => [
                ['type' => 'section', 'anchor' => 'plans', 'children' => [['type' => 'paragraph', 'text' => 'Moved.']]],
            ]],
        ], 'node_not_editable/holds_locked', true);

        return $rows;
    }

    /**
     * The calls that set every path of the landing page: call n sets the n-th
     * path of every item pattern that has one, so a node is named once a call.
     *
     * @return list<list<array<string,mixed>>>
     */
    private function pathCalls(): array
    {
        $byPattern = [];
        foreach (ElementorClassicMapper::project($this->pages['landing']['tree'])->nodes() as $node) {
            if ($node['kind'] === 'pattern' && $node['editable'] !== []) {
                $byPattern[(string) $node['pattern']] = [(string) $node['ref'], array_values($node['editable'])];
            }
        }
        $calls = [];
        foreach ($byPattern as [$ref, $paths]) {
            foreach ($paths as $n => $path) {
                $calls[$n][] = ['op' => 'set_text', 'ref' => $ref, 'field' => $path, 'text' => self::textFor($path, $n)];
            }
        }

        return array_values($calls);
    }

    /** A new text for a path, in the form the path stores: digits for a count, a link, a button text, or words. */
    private static function textFor(string $path, int $n): string
    {
        if (substr($path, -6) === '.value') {
            return (string) (1000 + 7 * $n);
        }
        if (substr($path, -7) === '.suffix') {
            return ['+', '%', 'k', 'x'][$n % 4];
        }
        if (substr($path, -4) === '.url') {
            return $n % 2 === 0 ? 'https://example.com/offer/' . $n : '#faq';
        }
        if (substr($path, -9) === '.cta.text') {
            return 'Choose plan ' . $n;
        }

        return 'Edited ' . $path . ($n % 3 === 0 ? ' with Tom & Jerry\'s "best" caf' . "\u{00E9}" : '');
    }

    /**
     * The ref of the first projection node of a page that has these members.
     *
     * @param array<string,mixed> $where Members of the projection node.
     */
    private function ref(string $page, array $where): string
    {
        $tree = $this->pageTree($page);
        foreach (ElementorClassicMapper::project($tree)->nodes() as $node) {
            $all = true;
            foreach ($where as $key => $value) {
                $all = $all && ($node[$key] ?? null) === $value;
            }
            if ($all) {
                return (string) $node['ref'];
            }
        }
        $this->fail('no node ' . json_encode($where) . ' on ' . $page);
    }

    /**
     * A page's tree: the compiled one, or the one a person restructured.
     *
     * @return list<array<string,mixed>>
     */
    private function pageTree(string $page): array
    {
        if ($page === 'restructured') {
            // The pricing card's list and second heading change places: no pattern has that shape.
            $tree = $this->pages['small']['tree'];
            $card = &$tree[1]['elements'][0]['elements'][0]['elements'];
            [$card[1], $card[2]] = [$card[2], $card[1]];
            unset($card);

            return $tree;
        }

        return $this->pages[$page]['tree'];
    }

    // -------------------------------------------------------------------------
    // Running a case
    // -------------------------------------------------------------------------

    /** The catalogue entry the command is given. */
    private function entry(): string
    {
        return (string) json_encode([
            'name'          => OwnAbilities::NAME_PAGE_EDIT,
            'source'        => 'wpmgr',
            'class'         => 'write',
            'status'        => 'admitted',
            'enabled'       => true,
            'approval_mode' => 'per_call',
            'snapshot'      => 'builder_document',
            'limits'        => ['builders_enabled' => ['elementor']],
        ]);
    }

    private static function precheckDigest(string $entrySha, string $inputSha, string $baseFp, string $previewDigest): string
    {
        return hash('sha256', (string) json_encode([$entrySha, $inputSha, $baseFp, $previewDigest]));
    }

    /**
     * Fresh rows: the target, WPMgr's Elementor draft holding $tree and
     * created by CREATE, then the active kit.
     *
     * @param list<array<string,mixed>> $tree  The stored tree.
     */
    private function usePage(string $name, array $tree, string $title): void
    {
        $this->rows      = new FakeBuilderWpdb();
        $this->wpdb      = new EngineWpdb($this->rows);
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->rows->addPost(self::TARGET, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => $title,
            'post_content'      => $name,
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

    private function fingerprint(): string
    {
        return (string) BuilderDocumentFingerprint::ofPost(self::TARGET, ElementorDocument::DESCRIPTOR_KEYS);
    }

    /**
     * The projection the AI reads for the stored page: wpmgr/page-structure's answer.
     *
     * @return array<string,mixed>
     */
    private function structure(): array
    {
        $read = BuilderPageStructure::read(self::TARGET, null, BuilderContract::MAX_STRUCTURE_NODES, [self::TARGET], new ElementorAdapter($this->api, self::PRINCIPAL));
        $this->assertArrayHasKey('output', $read, json_encode($read));

        return $read['output'];
    }

    /**
     * The precheck of an input on the stored page, as the command answers it.
     *
     * @param string                            $entry   The entry text.
     * @param string                            $input   The input text.
     * @param array<string,mixed>               $facts   The site's facts.
     * @param callable(list<int>): array<mixed> $media   Resolves attachment ids.
     * @param list<int>|null                    $asked   Receives the ids the precheck asked for.
     * @return array<string,mixed>
     */
    private function precheck(string $entry, string $input, array $facts, callable $media, ?array &$asked = null): array
    {
        $this->api->version           = $facts['elementor_version'];
        $this->api->activeBreakpoints = $facts['active_breakpoints'];
        $asked                        = [];
        $resolver                     = static function (array $ids) use ($media, &$asked): array {
            $asked = array_map('intval', array_values($ids));

            return $media($ids);
        };
        $plan = BuilderPageEdit::plan($input, (object) json_decode($entry, false), [self::TARGET], self::EDIT, $resolver, ['elementor' => new ElementorAdapter($this->api, self::PRINCIPAL)]);
        if (isset($plan['refusal'])) {
            return $plan['refusal'];
        }

        return [
            'ok'               => true,
            'outcome'          => 'prechecked',
            'mode'             => 'precheck',
            'ability'          => OwnAbilities::NAME_PAGE_EDIT,
            'request_id'       => self::EDIT,
            'valid'            => true,
            'base_fingerprint' => $plan['base_fingerprint'],
            'preview_digest'   => $plan['preview_digest'],
            'precheck_digest'  => self::precheckDigest(hash('sha256', $entry), hash('sha256', $input), $plan['base_fingerprint'], $plan['preview_digest']),
            'preview'          => $plan['preview'],
        ];
    }

    /** The code of a refusal, with its detail when that is one of WPMgr's fixed words. */
    private function expectKey(array $answer): string
    {
        $code   = (string) ($answer['code'] ?? '?');
        $detail = (string) ($answer['detail'] ?? '');

        return $detail !== '' && strlen($detail) <= 40 && strpos($detail, ' ') === false ? $code . '/' . $detail : $code;
    }

    /**
     * One case, run.
     *
     * @param array<string,mixed> $row The case row.
     * @return array<string,mixed>
     */
    private function record(array $row, string $entry): array
    {
        $page = $this->pages[$row['page']];
        $tree = $this->pageTree($row['page']);
        $this->usePage($row['page'], $tree, $page['title']);
        $facts  = ['elementor_version' => '3.35.9', 'active_breakpoints' => $row['breakpoints'] ?? self::BOTH];
        $design = $row['design'] === true ? self::DESIGN : ($row['design'] === false ? null : $row['design']);
        $body   = ['post_id' => self::TARGET, 'base_fingerprint' => $row['base'] ?? $this->fingerprint(), 'operations' => $row['ops']];
        if ($design !== null) {
            $body['design'] = $design;
        }
        $input    = (string) json_encode($body, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
        $resolved = [];
        $answer   = $this->precheck($entry, $input, $facts, function (array $ids) use (&$resolved): array {
            $resolved = [];
            foreach ($ids as $id) {
                $mime = $this->mimes[(int) $id] ?? null;
                if ($mime === null || strncmp($mime, 'image/', 6) !== 0) {
                    return ['refusal' => ['ok' => false, 'outcome' => 'refused', 'code' => 'image_not_available', 'detail' => 'an image could not be resolved', 'retryable' => false]];
                }
                $resolved[] = self::mediaFact((int) $id, $mime);
            }

            return ['facts' => $resolved];
        });

        return [
            'name'       => $row['name'],
            'note'       => $row['note'],
            'page'       => $row['page'],
            'expect'     => $row['expect'],
            'request_id' => self::EDIT,
            'facts'      => $facts,
            'media'      => $resolved,
            'input'      => $input,
            'answer'     => $answer,
        ];
    }

    /**
     * The whole fixture.
     *
     * @return array<string,mixed>
     */
    private function document(): array
    {
        $entry = $this->entry();
        $pages = [];
        foreach (self::pageInputs() as $name => $page) {
            $tree = $this->createTree($page['create_input'], $page['create_facts']);
            $this->pages[$name] = ['title' => $page['title'], 'tree' => $tree];
        }
        $this->pages['restructured'] = ['title' => 'Pricing, restructured', 'tree' => []];
        foreach (self::pageInputs() as $name => $page) {
            $this->usePage($name, $this->pages[$name]['tree'], $page['title']);
            $pages[$name] = [
                'title'            => $page['title'],
                'post_id'          => self::TARGET,
                'create_input'     => $page['create_input'],
                'create_facts'     => $page['create_facts'],
                'base_fingerprint' => $this->fingerprint(),
                'before_tree'      => $this->pages[$name]['tree'],
                'structure'        => $this->structure(),
            ];
        }
        // The restructured page is the small one with a pricing card whose parts changed places.
        $restructured = $this->pageTree('restructured');
        $this->usePage('restructured', $restructured, 'Pricing, restructured');
        $pages['restructured'] = [
            'title'            => 'Pricing, restructured',
            'post_id'          => self::TARGET,
            'derived_from'     => 'small',
            'base_fingerprint' => $this->fingerprint(),
            'before_tree'      => $restructured,
            'structure'        => $this->structure(),
        ];

        $cases    = [];
        $mismatch = [];
        foreach ($this->rows() as $row) {
            $case = $this->record($row, $entry);
            $got  = ($case['answer']['ok'] ?? null) === true ? 'prechecked' : $this->expectKey($case['answer']);
            if ($got !== $row['expect']) {
                $mismatch[$row['name']] = ['wanted' => $row['expect'], 'got' => $got, 'answer' => ($case['answer']['ok'] ?? null) === true ? 'prechecked' : $case['answer']];
            }
            $cases[] = $case;
        }
        $this->assertSame([], $mismatch, 'cases that did not answer what they were written for');

        return [
            'note'         => 'Golden answers of the wpmgr/page-edit precheck for designed pages (outline version 3), regenerated by PageEditDesignedGoldenTest with WPMGR_WRITE_FIXTURES=1 and never edited by hand. '
                . '"pages" holds each page the cases edit: the tree page-create\'s precheck built for "create_input" on a site with "create_facts" (the "restructured" page is "derived_from" another with one pricing card\'s parts moved, so no pattern reads from it), '
                . 'the fingerprint of its stored rows and "structure", the answer wpmgr/page-structure gives for it, which is the projection the AI reads. '
                . 'Each case holds "input", the exact JSON text of the page-edit input with the page\'s design as its last member when the case carries one; "facts", what the site gave the compile of any new section: the Elementor release and the breakpoints Elementor has active; '
                . '"media", the images the precheck asked the site for, in the order asked; and "answer", exactly what the command answers: for a precheck {ok, outcome, mode, ability, request_id, valid, base_fingerprint, preview_digest, precheck_digest, preview} where preview is {post_id, builder, builder_version, format, title, changes, after_outline, tree_sha256, tree}, '
                . 'and for a refusal {ok, outcome, code, detail, retryable} with op_index, refs or failures where the refusal names them. '
                . '"expect" is the answer the case was written for: prechecked, or the refusal\'s code and detail. '
                . 'The precheck digest binds "entry_sha256", the sha256 of "entry", the sha256 of the input, the base fingerprint and the preview digest; the preview digest binds the tree\'s bytes (json_encode with default flags). '
                . '"library_alt" is the alt text each image has in the media library and "attachments" the type of each attachment the cases name. A number is a JSON integer or text, and the file holds no float.',
            'entry'        => $entry,
            'entry_sha256' => hash('sha256', $entry),
            'library_alt'  => (object) array_map('strval', self::LIBRARY_ALT),
            'attachments'  => (object) array_map('strval', self::MIMES),
            'pages'        => $pages,
            'cases'        => $cases,
        ];
    }

    /**
     * The fixture as text: the file pretty-printed, each tree as a list with
     * one top-level section to a line, so a change to one section is a change
     * to one line.
     */
    private function fixtureText(): string
    {
        $flags       = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR;
        $doc         = $this->document();
        $this->lines = [];
        foreach (array_keys($doc['pages']) as $name) {
            $doc['pages'][$name]              = $this->lineOut($doc['pages'][$name], 'before_tree');
            $doc['pages'][$name]['structure'] = $this->lineOut($doc['pages'][$name]['structure'], 'nodes');
        }
        foreach (array_keys($doc['cases']) as $i) {
            if (isset($doc['cases'][$i]['answer']['preview']['tree'])) {
                $preview                                  = $doc['cases'][$i]['answer']['preview'];
                $preview['after_outline']                 = $this->lineOut($preview['after_outline'], 'nodes');
                $doc['cases'][$i]['answer']['preview']    = $this->lineOut($preview, 'tree');
            }
        }
        $text = json_encode($doc, JSON_PRETTY_PRINT | $flags);
        foreach ($this->lines as $token => $items) {
            $text = str_replace('"' . $token . '"', "[\n            " . implode(",\n            ", $items) . "\n        ]", $text);
        }

        return $text . "\n";
    }

    /** @var array<string,list<string>> */
    private array $lines = [];

    /**
     * A holder with its list at $key replaced by a token, and the list kept
     * as one line to an item for fixtureText() to put back.
     *
     * @param array<string,mixed> $holder The holder.
     * @param string              $key    The member that holds the list.
     * @return array<string,mixed>
     */
    private function lineOut(array $holder, string $key): array
    {
        $token               = '@@list' . count($this->lines) . '@@';
        $this->lines[$token] = array_map(static fn (array $item): string => (string) json_encode($item, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR), $holder[$key]);
        $holder[$key]        = $token;

        return $holder;
    }

    /**
     * The fixture, read.
     *
     * @return array<string,mixed>
     */
    private function fixture(): array
    {
        $this->assertFileExists(self::FIXTURE, 'page-edit-designed-previews.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $raw = file_get_contents(self::FIXTURE);
        $this->assertIsString($raw);

        return json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
    }

    /**
     * Paths in a value where a number is a float.
     *
     * @return list<string>
     */
    private static function floatPaths(mixed $value, string $path = ''): array
    {
        if (is_float($value)) {
            return [$path];
        }
        $found = [];
        if (is_array($value)) {
            foreach ($value as $key => $child) {
                array_push($found, ...self::floatPaths($child, $path . '/' . $key));
            }
        }

        return $found;
    }
}
