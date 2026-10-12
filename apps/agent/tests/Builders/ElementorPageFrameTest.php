<?php
/**
 * The page frame of a builder page: the frame an outline version 3 page asks
 * for, the page template that frame is stored as, the template written as a
 * plain row with the insert and never through the document's settings save,
 * the refusal when the site does not offer the template for the post type,
 * and the template kept by later updates of the post row.
 *
 * The harness is BuilderPageCreateTest's: FakeElementorApi behind the real
 * ElementorAdapter, rows in FakeBuilderWpdb read back through the SQL reads
 * production uses. The stand-in document save also updates the post row,
 * and every update of the post row here treats the page template as core's
 * post save does: a template other than the default that the theme does not
 * offer for the post type is put back to "default".
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\AbilityWriteScope;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\DocumentDescriptor;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\ElementorStyleCache;
use WPMgr\Agent\Abilities\Builders\NativeDocument;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageCreate
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorDocument
 */
final class ElementorPageFrameTest extends TestCase
{
    private const CONTAINERS = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    /** The shared page-create case table, and the rule of its rows about the site's theme. */
    private const CASES = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

    private const RULE_FRAME_FITS = 'page_frame_fits_theme';

    /** The id the next inserted post gets. */
    private const NEW_ID = 41;

    /** The revision Elementor's save makes of the new draft. */
    private const REVISION = 90;

    private const KIT = 4;

    private const OTHER = 77;

    private const PRINCIPAL = 2;

    private const TITLE = 'Our services & more';

    private const TEMPLATE = '_wp_page_template';

    /** The request whose snapshot an undo test takes. */
    private const UNDO_REQUEST = '7a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d';

    /** The theme's list once Elementor has added its templates to it. */
    private const WITH_ELEMENTOR = [
        'elementor_canvas'        => 'Elementor Canvas',
        'elementor_header_footer' => 'Elementor Full Width',
        'elementor_theme'         => 'Theme',
        'page-wide.php'           => 'Wide',
    ];

    /** @var list<array{0:string,1:callable,2:int,3:int}> Captured hooks: tag, callback, priority, accepted args. */
    private array $hooks = [];

    /** @var array<int,array<string,string>> Post rows by id, as stored. */
    private array $rows = [];

    /** @var array<int,int> Post author by id. */
    private array $authors = [];

    /** @var list<array<string,mixed>> wp_insert_post() arguments, in order. */
    private array $inserts = [];

    /** @var array<string,mixed> The ledger row as the updates left it. */
    private array $ledgerRow = [];

    /** @var array<string,array<string,string>> The page templates the theme offers, by post type. */
    private array $offered = [];

    /** @var list<array{0:mixed,1:string}> The theme's page-template list reads: post, post type. */
    private array $templateReads = [];

    private FakeBuilderWpdb $wpdb;

    private FakeElementorApi $api;

    private int $metaId = 100;

    /** @var list<array{0:int,1:string,2:mixed}> update_post_meta() calls: post, key, the value as passed. */
    private array $metaCalls = [];

    /** @var (\Closure(): void)|null Runs inside update_post_meta() after its write, as a listener on it would. */
    private ?\Closure $afterMetaWrite = null;

    /** @var (\Closure(): void)|null Runs inside clean_post_cache() after a restore's commit, as a listener on it would. */
    private ?\Closure $duringUndo = null;

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
        $offered = function ($post, $postType): array {
            $this->templateReads[] = [$post, (string) $postType];

            return $this->offered[(string) $postType] ?? [];
        };
        $theme = new class ($offered) {
            /** @var \Closure */
            private $offered;

            public function __construct(\Closure $offered)
            {
                $this->offered = $offered;
            }

            /** @return array<string,string> */
            public function get_page_templates($post = null, $post_type = 'page'): array
            {
                return ($this->offered)($post, $post_type);
            }
        };
        Functions\when('add_filter')->alias($capture);
        Functions\when('add_action')->alias($capture);
        Functions\when('remove_filter')->alias($release);
        Functions\when('remove_action')->alias($release);
        Functions\when('get_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('get_userdata')->justReturn(false);
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wc_get_page_id')->justReturn(-1);
        Functions\when('wp_slash')->alias($slash);
        Functions\when('wp_get_theme')->justReturn($theme);
        Functions\when('get_post_meta')->justReturn('');
        Functions\when('get_post')->alias(function ($id) {
            $id = (int) $id;
            if (!isset($this->rows[$id])) {
                return null;
            }

            return (object) (['ID' => $id, 'post_author' => (string) ($this->authors[$id] ?? 0), 'post_parent' => 0] + $this->rows[$id]);
        });
        Functions\when('wp_insert_post')->alias(function (array $data, $wpError = false) {
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
                // As core's meta write: unslashed, an array serialized.
                $this->wpdb->addMeta(++$this->metaId, $id, (string) $key, is_array($value) ? serialize(self::unslash($value)) : stripslashes((string) $value));
            }

            return $id;
        });
        Functions\when('wp_trash_post')->alias(function ($id) {
            $id = (int) $id;
            if (isset($this->rows[$id])) {
                $this->rows[$id]['post_status'] = 'trash';
                $this->wpdb->addPost($id, $this->rows[$id]);
            }

            return true;
        });
        Functions\when('wp_get_post_autosave')->justReturn(false);
        Functions\when('wp_get_post_revisions')->justReturn([]);
        Functions\when('wp_check_post_lock')->justReturn(false);

        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
        unset($GLOBALS['_wp_switched_stack']);
        $this->locale = setlocale(LC_NUMERIC, '0');

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->fresh();
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

    public function testTemplateMetaPlainAndKept(): void
    {
        // Never a meta call of its own: the row goes in with the insert.
        Functions\expect('update_post_meta')->never();
        Functions\expect('add_post_meta')->never();
        Functions\expect('update_metadata')->never();
        $this->offered = ['page' => self::WITH_ELEMENTOR, 'post' => self::WITH_ELEMENTOR];

        foreach ([
            ['full_width', 'page', 'elementor_header_footer'],
            ['canvas', 'page', 'elementor_canvas'],
            ['theme', 'page', ''],
            ['full_width', 'post', 'elementor_header_footer'],
        ] as [$frame, $postType, $template]) {
            $this->fresh();
            $spec  = $this->v3Spec($frame, $postType);
            $built = $this->precheck($spec);
            $this->assertArrayNotHasKey('refusal', $built, $frame . ': ' . (string) json_encode($built['refusal'] ?? null));
            $document = $this->elementorSaves($postType === 'page' ? 'wp-page' : 'wp-post');

            $result = $this->write($spec, $built);

            $this->assertTrue($result['ok'] ?? null, $frame . ': ' . (string) ($result['detail'] ?? ''));
            $this->assertSame([
                '_wpmgr_created_by_request' => self::request(),
                '_elementor_edit_mode'      => 'builder',
                '_elementor_template_type'  => $postType === 'page' ? 'wp-page' : 'wp-post',
                self::TEMPLATE              => $template,
            ], $this->inserts[0]['meta_input'] ?? null, $frame . ': the template is a plain row of the insert');
            $this->assertSame([['elements' => $built['tree']]], $document->saves, $frame . ': the document save gets the tree, never page settings');
            $this->assertSame([$template], $this->stored(self::TEMPLATE), $frame . ': kept through the post-row update of the save');
            $this->assertSame([], $this->stored(ElementorDocument::KEY_PAGE_SETTINGS), $frame . ': no page settings row');

            // A later update of the post row keeps it.
            $this->rowUpdate(self::NEW_ID);
            $this->assertSame([$template], $this->stored(self::TEMPLATE), $frame . ': kept by a later post-row update');
        }
    }

    public function testTemplateUnavailableRefused(): void
    {
        // The theme's own list, without Elementor's templates.
        $this->offered = ['page' => ['page-wide.php' => 'Wide']];
        foreach (['full_width', 'canvas'] as $frame) {
            $built = $this->precheck($this->v3Spec($frame));
            $this->assertSame('page_template_unavailable', $built['refusal']['code'] ?? null, $frame);
            $this->assertNull($built['doc'], $frame);
        }
        $this->assertSame([[null, 'page'], [null, 'page']], $this->templateReads, 'the list is the one for the post type, no post yet');
        $this->assertSame([], $this->inserts);

        // Offered for a page only: a post asks its own type's list.
        $this->offered       = ['page' => self::WITH_ELEMENTOR];
        $this->templateReads = [];
        $built               = $this->precheck($this->v3Spec('full_width', 'post'));
        $this->assertSame('page_template_unavailable', $built['refusal']['code'] ?? null);
        $this->assertSame([[null, 'post']], $this->templateReads);

        // The theme's own template is never refused, and needs no list.
        $this->offered       = [];
        $this->templateReads = [];
        $built               = $this->precheck($this->v3Spec('theme'));
        $this->assertArrayNotHasKey('refusal', $built, (string) json_encode($built['refusal'] ?? null));
        $this->assertSame('', $built['doc']->meta[self::TEMPLATE] ?? null);
        $this->assertSame([], $this->templateReads);
    }

    public function testTemplatePutBackByTheSaveIsVerifyMismatch(): void
    {
        $this->offered = ['page' => self::WITH_ELEMENTOR];
        $spec          = $this->v3Spec('full_width');
        $built         = $this->precheck($spec);
        $this->elementorSaves();
        // The theme stops offering the template before the save updates the post row.
        $this->offered = ['page' => []];

        $result = $this->write($spec, $built);

        $this->assertSame(['default'], $this->stored(self::TEMPLATE), 'precondition: the update put the template back');
        $this->assertSame(['ok' => false, 'code' => 'verify_mismatch', 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result));
        $this->assertSame('the created draft is not what was built: page_template', $result['detail'] ?? null);
        $this->assertSame('trash', $this->rows[self::NEW_ID]['post_status']);
    }

    public function testFrameDefaultsToTheHeroAndMapsInOnePlace(): void
    {
        $hero  = ['type' => 'section', 'anchor' => 'top', 'children' => [['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => []]]];
        $plain = ['type' => 'section', 'anchor' => 'about', 'children' => [['type' => 'heading', 'level' => 2, 'text' => 'About']]];
        $v3    = ['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => 'T', 'outline_version' => 3];

        $this->assertSame('full_width', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero, $plain]], false));
        $this->assertSame('full_width', BuilderPageCreate::pageFrame($v3 + ['outline' => json_decode((string) json_encode([$hero]))], false), 'decoded objects');
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$plain]], false));
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$plain, $hero]], false), 'only the opening block can be the hero');
        $this->assertSame('canvas', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero], 'page_frame' => 'canvas'], false));
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero], 'page_frame' => 'theme'], false));
        $this->assertNull(BuilderPageCreate::pageFrame(['outline' => [$hero]], false), 'an outline before version 3 has no frame');
        foreach ([
            'frame without version 3' => ['outline' => [], 'page_frame' => 'canvas'],
            'version 2 named'         => ['outline' => [], 'outline_version' => 2, 'page_frame' => 'canvas'],
            'unknown frame'           => $v3 + ['outline' => [], 'page_frame' => 'wide'],
            'frame not a string'      => $v3 + ['outline' => [], 'page_frame' => ['canvas']],
            'version as text'         => ['outline' => [], 'outline_version' => '3', 'page_frame' => 'canvas'],
        ] as $name => $spec) {
            $this->assertFalse(BuilderPageCreate::pageFrame($spec, false), $name);
        }

        $templates = [];
        foreach (OutlineV3Schema::PAGE_FRAMES as $frame) {
            $templates[$frame] = ElementorDocument::pageTemplate($frame);
        }
        $this->assertSame(['theme' => '', 'full_width' => 'elementor_header_footer', 'canvas' => 'elementor_canvas'], $templates);
        $this->assertNull(ElementorDocument::pageTemplate('wide'));

        // A version 2 page gets no template row and reads no list.
        $this->offered = [];
        $built         = $this->precheck(['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => self::TITLE, 'outline' => self::outline()]);
        $this->assertArrayNotHasKey('refusal', $built);
        $this->assertArrayNotHasKey(self::TEMPLATE, $built['doc']->meta);
        $this->assertSame([], $this->templateReads);

        // A frame the precheck cannot place is bad input.
        $built = $this->precheck(['page_frame' => 'wide'] + $this->v3Spec('canvas'));
        $this->assertSame('bad_input', $built['refusal']['code'] ?? null);
    }

    public function testSaveScopeAdmitsOwnWritesAndTripsOnOthers(): void
    {
        $this->offered = ['page' => self::WITH_ELEMENTOR];
        $cache         = ElementorStyleCache::PREFIX . 'widget';
        $before        = ['state' => true, 'children' => [self::OTHER => ['state' => true]]];
        $own           = ['state' => true, 'children' => [self::OTHER => ['state' => true], self::NEW_ID => ['state' => true]]];
        $other         = ['state' => true, 'children' => [self::OTHER => ['state' => false], self::NEW_ID => ['state' => true]]];

        $cases = [
            'own template and own cache entry' => [null, function () use ($cache, $before, $own): void {
                $this->fire('updated_post_meta', 500, self::NEW_ID, self::TEMPLATE, 'elementor_header_footer');
                $this->fire('updated_option', $cache, $before, $own);
            }],
            'another post\'s template'         => ['side_effect_detected', function (): void {
                $this->fire('updated_post_meta', 501, self::OTHER, self::TEMPLATE, 'elementor_canvas');
            }],
            'another post\'s cache entry'      => ['side_effect_detected', function () use ($cache, $before, $other): void {
                $this->fire('updated_option', $cache, $before, $other);
            }],
        ];
        foreach ($cases as $name => [$code, $during]) {
            $this->fresh();
            $spec  = $this->v3Spec('full_width');
            $built = $this->precheck($spec);
            $this->elementorSaves('wp-page', $during);

            $result = $this->write($spec, $built);

            if ($code === null) {
                $this->assertTrue($result['ok'] ?? null, $name . ': ' . (string) ($result['detail'] ?? ''));
                $this->assertSame(['elementor_header_footer'], $this->stored(self::TEMPLATE), $name);
            } else {
                $this->assertSame(['ok' => false, 'code' => $code, 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result), $name);
            }
        }
    }

    public function testBlockThemeBuildsCanvasOnly(): void
    {
        Functions\when('wp_is_block_theme')->justReturn(true);
        $this->offered = ['page' => self::WITH_ELEMENTOR];

        // Without a page_frame a block theme gives canvas, even to a page that
        // opens with a hero; a classic theme keeps the hero rule.
        $hero = ['type' => 'section', 'anchor' => 'top', 'children' => [['type' => 'pattern', 'pattern' => 'hero', 'variant' => 'centered', 'children' => []]]];
        $v3   = ['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => 'T', 'outline_version' => 3];
        $this->assertSame('canvas', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero]], true));
        $this->assertSame('full_width', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero]], false));
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero], 'page_frame' => 'theme'], true), 'a sent frame is the frame judged');
        $this->assertFalse(BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero], 'page_frame' => 'wide'], true));
        $this->assertNull(BuilderPageCreate::pageFrame(['outline' => [$hero]], true), 'an outline before version 3 has no frame');
        $this->assertSame(['canvas'], BuilderPageCreate::pageFrames(true));
        $this->assertSame(OutlineV3Schema::PAGE_FRAMES, BuilderPageCreate::pageFrames(false));

        $noFrame = array_diff_key($this->v3Spec('canvas'), ['page_frame' => 1]);
        foreach (['no page_frame' => $noFrame, 'canvas sent' => $this->v3Spec('canvas')] as $name => $spec) {
            $built = $this->precheck($spec);
            $this->assertArrayNotHasKey('refusal', $built, $name . ': ' . (string) json_encode($built['refusal'] ?? null));
            $this->assertSame('elementor_canvas', $built['doc']->meta[self::TEMPLATE] ?? null, $name);
            $this->assertSame(['block_theme' => true, 'page_frame' => 'canvas', 'active_breakpoints' => ['mobile', 'tablet']], $built['builder_facts'] ?? null, $name);
        }

        // theme or full_width sent: refused before any template list is read.
        foreach (['theme', 'full_width'] as $frame) {
            $this->templateReads = [];
            $built               = $this->precheck($this->v3Spec($frame));
            $refusal             = $built['refusal'] ?? [];
            $this->assertSame('page_frame_unavailable', $refusal['code'] ?? null, $frame);
            $this->assertSame([[
                'path'    => 'input',
                'key'     => 'page_frame',
                'reason'  => BuilderPageCreate::REASON_FRAME_BLOCK_THEME,
                'allowed' => ['canvas'],
            ]], $refusal['failures'] ?? null, $frame);
            $this->assertStringStartsWith('the page frame cannot be built with the site\'s theme.', (string) ($refusal['detail'] ?? ''), $frame);
            $this->assertNull($built['doc'], $frame);
            $this->assertSame([], $this->templateReads, $frame);
        }

        // A version 2 page is unchanged: no frame and no template row.
        $built = $this->precheck(['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => self::TITLE, 'outline' => self::outline()]);
        $this->assertArrayNotHasKey('refusal', $built);
        $this->assertArrayNotHasKey(self::TEMPLATE, $built['doc']->meta);
        $this->assertSame(['block_theme' => true, 'page_frame' => null], $built['builder_facts'] ?? null);
        $this->assertSame([], $this->inserts);
    }

    public function testClassicThemeFramesAreUnchanged(): void
    {
        $this->offered = ['page' => self::WITH_ELEMENTOR];
        foreach (OutlineV3Schema::PAGE_FRAMES as $frame) {
            $built = $this->precheck($this->v3Spec($frame));
            $this->assertArrayNotHasKey('refusal', $built, $frame . ': ' . (string) json_encode($built['refusal'] ?? null));
            $this->assertSame(['block_theme' => false, 'page_frame' => $frame, 'active_breakpoints' => ['mobile', 'tablet']], $built['builder_facts'] ?? null, $frame);
        }
        $built = $this->precheck(array_diff_key($this->v3Spec('canvas'), ['page_frame' => 1]));
        $this->assertSame(['block_theme' => false, 'page_frame' => 'theme', 'active_breakpoints' => ['mobile', 'tablet']], $built['builder_facts'] ?? null, 'no hero, no frame: theme');
        $built = $this->precheck(['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => self::TITLE, 'outline' => self::outline()]);
        $this->assertSame(['block_theme' => false, 'page_frame' => null], $built['builder_facts'] ?? null, 'version 2');
    }

    public function testFrameThatNoLongerFitsIsRefusedAtExecution(): void
    {
        $this->offered = ['page' => self::WITH_ELEMENTOR];
        $spec          = $this->v3Spec('full_width');
        $built         = $this->precheck($spec);
        $this->assertSame(['block_theme' => false, 'page_frame' => 'full_width', 'active_breakpoints' => ['mobile', 'tablet']], $built['builder_facts'] ?? null);

        // The site moves to a block theme before the write. The write's
        // pipeline builds again from the same input before any insert.
        Functions\when('wp_is_block_theme')->justReturn(true);
        $again = $this->precheck($spec);

        $this->assertSame('page_frame_unavailable', $again['refusal']['code'] ?? null);
        $this->assertNull($again['doc']);
        $this->assertSame([], $this->inserts);
    }

    public function testBlockThemeCanvasSaveScopeAdmitsOwnWritesAndTripsOnOthers(): void
    {
        Functions\when('wp_is_block_theme')->justReturn(true);
        $this->offered = ['page' => self::WITH_ELEMENTOR];
        $cases         = [
            'own canvas template'      => [null, function (): void {
                $this->fire('updated_post_meta', 500, self::NEW_ID, self::TEMPLATE, 'elementor_canvas');
            }],
            'another post\'s template' => ['side_effect_detected', function (): void {
                $this->fire('updated_post_meta', 501, self::OTHER, self::TEMPLATE, 'elementor_canvas');
            }],
        ];
        foreach ($cases as $name => [$code, $during]) {
            $this->fresh();
            $spec  = array_diff_key($this->v3Spec('canvas'), ['page_frame' => 1]);
            $built = $this->precheck($spec);
            $this->assertSame('canvas', $built['builder_facts']['page_frame'] ?? null, $name);
            $this->elementorSaves('wp-page', $during);

            $result = $this->write($spec, $built);

            if ($code === null) {
                $this->assertTrue($result['ok'] ?? null, $name . ': ' . (string) ($result['detail'] ?? ''));
                $this->assertSame(['elementor_canvas'], $this->stored(self::TEMPLATE), $name);
            } else {
                $this->assertSame(['ok' => false, 'code' => $code, 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result), $name);
            }
        }
    }

    public function testSharedRowsOfTheSiteTheme(): void
    {
        $doc   = json_decode((string) file_get_contents(self::CASES), true, 512, JSON_THROW_ON_ERROR);
        $names = [];
        foreach ($doc['cases'] as $case) {
            if ($case['rule'] !== self::RULE_FRAME_FITS) {
                continue;
            }
            $name  = $case['name'];
            $block = $case['site']['block_theme'] ?? null;
            $this->assertIsBool($block, $name . ': the row states the site\'s theme');
            $spec    = json_decode($case['input'], true, 512, JSON_THROW_ON_ERROR);
            $frame   = BuilderPageCreate::pageFrame($spec, $block);
            $refusal = is_string($frame) ? BuilderPageCreate::frameRefusal($frame, $block) : null;
            $this->assertIsString($frame, $name);
            $this->assertSame($case['agent'], $refusal['code'] ?? 'ok', $name);
            if ($refusal === null) {
                $this->assertContains($frame, BuilderPageCreate::pageFrames($block), $name);
                $this->assertTrue(!$block || $frame === 'canvas', $name . ': a block theme builds canvas');
            } else {
                // The precheck answers the row's code before anything is built.
                Functions\when('wp_is_block_theme')->justReturn($block);
                $built = $this->precheck($spec);
                $this->assertSame($case['agent'], $built['refusal']['code'] ?? null, $name);
            }
            $names[] = $name;
        }
        foreach (['block-theme-without-a-frame', 'block-theme-canvas', 'block-theme-theme-frame', 'block-theme-full-width-frame'] as $must) {
            $this->assertContains($must, $names, 'the table holds the row ' . $must);
        }
    }

    /**
     * A full_width page that opens with a hero is created with its title
     * hidden: the whole page-settings array {hide_title: "yes"} is one plain
     * row of the insert, never a meta call of its own and never the
     * document's save, and the read-back requires exactly those bytes.
     */
    public function testHiddenTitleWrittenOnceOnCreate(): void
    {
        $hidden = [ElementorDocument::SETTING_HIDE_TITLE => ElementorDocument::HIDE_TITLE];
        $this->assertSame(['hide_title' => 'yes'], $hidden, 'Elementor\'s own setting name and value');
        $this->assertSame($hidden, ElementorDocument::createdPageSettings('full_width', true));
        foreach ([['full_width', false], ['theme', true], ['theme', false], ['canvas', true], ['wide', true]] as [$frame, $hero]) {
            $this->assertNull(ElementorDocument::createdPageSettings($frame, $hero), $frame . ($hero ? ' with' : ' without') . ' a hero');
        }

        Functions\expect('update_post_meta')->never();
        Functions\expect('add_post_meta')->never();
        Functions\expect('update_metadata')->never();
        $this->offered = ['page' => self::WITH_ELEMENTOR];
        foreach ([
            'kept'                => [null, null],
            'changed by the save' => ['verify_mismatch', 'a:1:{s:10:"hide_title";s:0:"";}'],
            'removed by the save' => ['verify_mismatch', null],
        ] as $name => [$code, $rewrite]) {
            $this->fresh();
            $spec  = $this->v3Spec('full_width');
            $built = $this->withHiddenTitle($this->precheck($spec));
            $during = $code === null ? null : function () use ($rewrite): void {
                $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => self::NEW_ID, 'meta_key' => ElementorDocument::KEY_PAGE_SETTINGS]);
                if ($rewrite !== null) {
                    $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_PAGE_SETTINGS, $rewrite);
                }
            };
            $document = $this->elementorSaves('wp-page', $during);

            $result = $this->write($spec, $built);

            if ($code !== null) {
                $this->assertSame(['ok' => false, 'code' => $code, 'post_id' => self::NEW_ID, 'trashed' => true], self::summary($result), $name);
                continue;
            }
            $this->assertTrue($result['ok'] ?? null, $name . ': ' . (string) ($result['detail'] ?? ''));
            $this->assertSame($hidden, $this->inserts[0]['meta_input'][ElementorDocument::KEY_PAGE_SETTINGS] ?? null, 'a plain row of the insert');
            $this->assertSame(['a:1:{s:10:"hide_title";s:3:"yes";}'], $this->stored(ElementorDocument::KEY_PAGE_SETTINGS), 'one row, the whole array');
            $this->assertSame([['elements' => $built['tree']]], $document->saves, 'the document save gets the tree, never page settings');
        }
    }

    /**
     * On a change the whole page-settings array is read, one key set, and the
     * whole array written back in one meta write: every other key is kept,
     * the same value again writes nothing, and a row that is not one plain
     * array is refused unwritten.
     */
    public function testHideTitleMergeKeepsOtherKeys(): void
    {
        $this->coreMetaWrites();
        $others = ['background_background' => 'classic', 'background_color' => '#1F2937', 'padding' => ['unit' => 'px', 'top' => '0', 'isLinked' => true], 'template' => 'elementor_header_footer'];
        foreach ([
            'appended'     => [[serialize($others)], $others + ['hide_title' => 'yes']],
            'set in place' => [[serialize(['hide_title' => '', 'background_color' => '#1F2937'])], ['hide_title' => 'yes', 'background_color' => '#1F2937']],
            'no row yet'   => [[], ['hide_title' => 'yes']],
        ] as $name => [$rows, $after]) {
            $document = $this->storedPage($rows);

            $result = $this->hideTitle();

            $this->assertSame(['ok' => true, 'written' => true], self::outcomeOf($result), $name . ': ' . (string) ($result['detail'] ?? ''));
            $this->assertSame([serialize($after)], $this->stored(ElementorDocument::KEY_PAGE_SETTINGS), $name . ': the whole array, every other key kept');
            $this->assertCount(1, $this->metaCalls, $name . ': one write of the whole array');
            $this->assertSame([self::NEW_ID, ElementorDocument::KEY_PAGE_SETTINGS], array_slice($this->metaCalls[0], 0, 2), $name);
            $this->assertSame([], $document->saves, $name . ': never the document\'s save');

            $this->assertSame(['ok' => true, 'written' => false], self::outcomeOf($this->hideTitle()), $name . ': the same value again');
            $this->assertCount(1, $this->metaCalls, $name . ': writes nothing');
        }

        foreach ([
            'a string'       => ['s:3:"yes";'],
            'not serialized' => ['hide_title=yes'],
            'an object'      => ['a:1:{s:1:"a";O:8:"stdClass":0:{}}'],
            'two rows'       => [serialize(['a' => '1']), serialize(['b' => '2'])],
            'not canonical'  => ['a:1:{s:1:"a";d:0.1000000000000000055511151231257827;}'],
        ] as $name => $rows) {
            $document = $this->storedPage($rows);

            $result = $this->hideTitle();

            $this->assertSame(['ok' => false, 'written' => null], self::outcomeOf($result), $name);
            $this->assertSame(ElementorDocument::CODE_REFUSED, $result['code'] ?? null, $name);
            $this->assertSame([], $this->metaCalls, $name . ': nothing written');
            $this->assertSame($rows, $this->stored(ElementorDocument::KEY_PAGE_SETTINGS), $name . ': the rows as they were');
            $this->assertSame([], $document->saves, $name);
        }
    }

    /**
     * The whole array goes to the meta write slashed, so core's unslash
     * leaves backslashes and quotes as they were, and the byte read-back
     * refuses a row that does not hold exactly what was written.
     */
    public function testPageSettingsBytesSlashed(): void
    {
        $this->coreMetaWrites();
        $before = ['custom_css' => 'selector .a::before { content: "\201C"; }', 'note' => 'Tom\'s "best" \\ page'];
        $this->storedPage([serialize($before)]);

        $result = $this->hideTitle();

        $this->assertSame(['ok' => true, 'written' => true], self::outcomeOf($result), (string) ($result['detail'] ?? ''));
        $this->assertSame([serialize($before + ['hide_title' => 'yes'])], $this->stored(ElementorDocument::KEY_PAGE_SETTINGS), 'backslashes and quotes byte for byte');
        $this->assertSame(addslashes($before['custom_css']), $this->metaCalls[0][2]['custom_css'] ?? null, 'passed to the meta write slashed');

        foreach ([
            'changed after the write' => function (): void {
                $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => self::NEW_ID, 'meta_key' => ElementorDocument::KEY_PAGE_SETTINGS]);
                $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_PAGE_SETTINGS, serialize(['hide_title' => 'yes']));
            },
            'a second row added'      => function (): void {
                $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_PAGE_SETTINGS, serialize(['hide_title' => 'yes']));
            },
        ] as $name => $after) {
            $this->storedPage([serialize($before)]);
            $this->afterMetaWrite = $after;

            $result = $this->hideTitle();

            $this->assertSame(['ok' => false, 'written' => null], self::outcomeOf($result), $name);
            $this->assertSame(ElementorDocument::CODE_REFUSED, $result['code'] ?? null, $name);
        }
        $this->storedPage([serialize($before)]);
        $this->afterMetaWrite = null;
        Functions\when('update_post_meta')->justReturn(true);
        $this->assertSame(ElementorDocument::CODE_REFUSED, $this->hideTitle()['code'] ?? null, 'a write that reports success but stores nothing');
    }

    /**
     * The page-settings write runs inside the post's write scope and under
     * the kit tripwire, as the tree's save does: its own row and the post's
     * own style-cache entry pass; another post's meta, another post's cache
     * entry and a change to the kit are side_effect_detected.
     */
    public function testPageSettingsWriteScopeAdmitsOwnAndTripsOnOthers(): void
    {
        $this->coreMetaWrites();
        $cache  = ElementorStyleCache::PREFIX . 'widget';
        $before = ['state' => true, 'children' => [self::OTHER => ['state' => true]]];
        $own    = ['state' => true, 'children' => [self::OTHER => ['state' => true], self::NEW_ID => ['state' => true]]];
        $other  = ['state' => true, 'children' => [self::OTHER => ['state' => false], self::NEW_ID => ['state' => true]]];
        foreach ([
            'own row and own cache entry' => [null, function () use ($cache, $before, $own): void {
                $this->fire('updated_option', $cache, $before, $own);
            }],
            'another post\'s page settings' => [ElementorDocument::CODE_SIDE_EFFECT, function (): void {
                $this->fire('updated_post_meta', 601, self::OTHER, ElementorDocument::KEY_PAGE_SETTINGS, ['hide_title' => 'yes']);
            }],
            'another post\'s cache entry' => [ElementorDocument::CODE_SIDE_EFFECT, function () use ($cache, $before, $other): void {
                $this->fire('updated_option', $cache, $before, $other);
            }],
            'the kit changed'             => [ElementorDocument::CODE_SIDE_EFFECT, function (): void {
                $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => self::KIT, 'meta_key' => ElementorDocument::KEY_PAGE_SETTINGS]);
                $this->wpdb->addMeta(++$this->metaId, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:0:{}');
            }],
        ] as $name => [$code, $during]) {
            $this->storedPage([serialize(['background_color' => '#1F2937'])]);
            $this->afterMetaWrite = $during;

            $result = $this->hideTitle();

            if ($code === null) {
                $this->assertSame(['ok' => true, 'written' => true], self::outcomeOf($result), $name . ': ' . (string) ($result['detail'] ?? ''));
                $this->assertSame([ElementorDocument::KEY_PAGE_SETTINGS], $result['scope']['target_meta_keys'] ?? null, $name);
                continue;
            }
            $this->assertSame(['ok' => false, 'written' => null], self::outcomeOf($result), $name);
            $this->assertSame($code, $result['code'] ?? null, $name . ': ' . (string) ($result['detail'] ?? ''));
        }
    }

    /**
     * Undo puts the page template and the page settings back byte for byte.
     * A change that wrote both (the hidden title through setPageSetting(),
     * the template as a frame change stores it) is undone, by a person's
     * undo and by the automatic restore, to exactly each key's rows from
     * before; a key the change added has no row again. A change that wrote
     * neither leaves both rows as they are, meta ids included. No descriptor
     * may list core's page template key or a key of the document as derived,
     * since a restore deletes a derived key instead of putting it back.
     */
    public function testUndoRestoresTemplateAndPageSettingsBytes(): void
    {
        $this->undoWiring();
        $settings = serialize(['background_color' => '#1F2937', 'custom_note' => "a\\b \"quoted\" caf\u{e9}"]);
        foreach (['rows before' => [[$settings], 'elementor_header_footer'], 'no rows before' => [[], null]] as $name => [$rows, $template]) {
            $this->storedUndoPage($rows, $template);
            $before   = $this->frameRows();
            $snapshot = $this->snapshotOf(self::NEW_ID);
            $this->frameChange();
            $this->assertNotSame($before[ElementorDocument::KEY_PAGE_SETTINGS], $this->frameRows()[ElementorDocument::KEY_PAGE_SETTINGS], $name . ': the change wrote the page settings');
            $this->assertNotSame($before[self::TEMPLATE], $this->frameRows()[self::TEMPLATE], $name . ': the change wrote the template');
            $changed = BuilderDocumentRestore::changes(self::NEW_ID, $snapshot, []);
            $this->assertSame([ElementorDocument::KEY_PAGE_SETTINGS, self::TEMPLATE], array_column($changed['changed_keys'], 'key'), $name);

            $adapter = new ElementorAdapter($this->api);
            $this->assertNull(BuilderDocumentRestore::scoped(self::NEW_ID, $snapshot, $changed['changed_keys'], $changed['changed_fields'], $adapter->descriptor(), $adapter), $name);
            $this->assertSame($before, $this->frameRows(), $name . ': each key holds exactly its rows from before the change');
        }

        // The automatic restore inside the call that wrote.
        $this->storedUndoPage([$settings], 'elementor_header_footer');
        $before   = $this->frameRows();
        $adapter  = new ElementorAdapter($this->api);
        $beforeFp = (string) BuilderDocumentFingerprint::ofPost(self::NEW_ID, $adapter->descriptor()->exactKeys);
        $snapshot = $this->snapshotOf(self::NEW_ID);
        $this->frameChange();
        $this->assertNull(BuilderDocumentRestore::full(self::NEW_ID, $snapshot, $adapter->descriptor(), $adapter, $beforeFp));
        $this->assertSame($before, $this->frameRows(), 'the automatic restore puts both keys back');

        // A change that wrote neither key.
        $this->storedUndoPage([$settings], 'elementor_header_footer');
        $kept     = $this->frameRowsWithIds();
        $snapshot = $this->snapshotOf(self::NEW_ID);
        update_post_meta(self::NEW_ID, ElementorDocument::KEY_DATA, '[]');
        $changed = BuilderDocumentRestore::changes(self::NEW_ID, $snapshot, []);
        $this->assertSame([ElementorDocument::KEY_DATA], array_column($changed['changed_keys'], 'key'));
        $adapter = new ElementorAdapter($this->api);
        $this->assertNull(BuilderDocumentRestore::scoped(self::NEW_ID, $snapshot, $changed['changed_keys'], $changed['changed_fields'], $adapter->descriptor(), $adapter));
        $this->assertSame($kept, $this->frameRowsWithIds(), 'a key the change did not write is neither deleted nor rewritten');

        // No descriptor can make a restore delete either key.
        $this->assertSame(['_elementor_css', '_elementor_element_cache', '_elementor_page_assets'], $adapter->descriptor()->derivedKeys, 'the adapter\'s caches stay derived');
        foreach ([
            'core\'s page template' => [ElementorDocument::DESCRIPTOR_KEYS, [], [self::TEMPLATE], []],
            'the page settings'     => [ElementorDocument::DESCRIPTOR_KEYS, [], [ElementorDocument::KEY_PAGE_SETTINGS], []],
            'a flag key'            => [[ElementorDocument::KEY_DATA], [], [ElementorDocument::KEY_EDIT_MODE], [ElementorDocument::KEY_EDIT_MODE]],
            'a key under a prefix'  => [[], ['_elementor_'], ['_elementor_css'], []],
        ] as $name => [$exact, $prefixes, $derived, $flags]) {
            try {
                new DocumentDescriptor($exact, $prefixes, $derived, $flags);
                $this->fail($name . ': a descriptor that makes a restore delete a key it puts back was built');
            } catch (\InvalidArgumentException $e) {
                $this->assertStringContainsString('derived', $e->getMessage(), $name);
            }
        }
    }

    /**
     * The undo's own writes stay inside the page's write scope, the one
     * save() runs under: the restored rows and the page's own style-cache
     * entry pass; another post's page settings, the kit's page settings or
     * another post's cache entry written during the undo trip it.
     */
    public function testUndoWritesPassTheWriteScopeAndOthersTrip(): void
    {
        $this->undoWiring();
        $cache  = ElementorStyleCache::PREFIX . 'widget';
        $before = ['state' => true, 'children' => [self::OTHER => ['state' => true]]];
        $own    = ['state' => true, 'children' => [self::OTHER => ['state' => true], self::NEW_ID => ['state' => true]]];
        $other  = ['state' => true, 'children' => [self::OTHER => ['state' => false], self::NEW_ID => ['state' => true]]];
        foreach ([
            'own rows and own cache entry'   => [[], function () use ($cache, $before, $own): void {
                $this->fire('updated_option', $cache, $before, $own);
            }],
            'another post\'s page settings' => [[AbilityWriteScope::V_OTHER_META], function (): void {
                $this->fire('updated_post_meta', 601, self::OTHER, ElementorDocument::KEY_PAGE_SETTINGS, ['hide_title' => 'yes']);
            }],
            'the kit\'s page settings'      => [[AbilityWriteScope::V_OTHER_META], function (): void {
                $this->fire('updated_post_meta', 2, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, ['system_colors' => []]);
            }],
            'another post\'s cache entry'   => [[AbilityWriteScope::V_OPTION], function () use ($cache, $before, $other): void {
                $this->fire('updated_option', $cache, $before, $other);
            }],
        ] as $name => [$violations, $during]) {
            $this->storedUndoPage([serialize(['background_color' => '#1F2937'])], 'elementor_header_footer');
            $was      = $this->frameRows();
            $snapshot = $this->snapshotOf(self::NEW_ID);
            $this->frameChange();
            $changed          = BuilderDocumentRestore::changes(self::NEW_ID, $snapshot, []);
            $adapter          = new ElementorAdapter($this->api);
            $scope            = new AbilityWriteScope(self::NEW_ID, (new ElementorDocument($this->api))->saveOptionPatterns(), ElementorDocument::saveOptionChecks());
            $this->duringUndo = $during;
            $problem          = $scope->run(static fn () => BuilderDocumentRestore::scoped(self::NEW_ID, $snapshot, $changed['changed_keys'], $changed['changed_fields'], $adapter->descriptor(), $adapter));
            $this->duringUndo = null;
            $outcome          = $scope->outcome();

            $this->assertNull($problem, $name);
            $this->assertSame($was, $this->frameRows(), $name . ': both keys are back');
            $this->assertSame($violations === [], $outcome['clean'], $name . ': ' . implode(', ', $outcome['violations']));
            $this->assertSame($violations, $outcome['violations'], $name);
        }
    }

    /**
     * The stubs an undo needs: snapshots stored as options, cache drops, and
     * clean_post_cache() running duringUndo as a listener on it would.
     */
    private function undoWiring(): void
    {
        $this->coreMetaWrites();
        Functions\when('add_option')->alias(fn ($name, $value = '', $unused = '', $autoload = null): bool => $this->wpdb->addOptionLikeCore((string) $name, $value, $unused, $autoload));
        Functions\when('delete_option')->alias(fn ($name): bool => $this->wpdb->deleteOptionLikeCore((string) $name));
        Functions\when('wp_cache_delete')->justReturn(true);
        Functions\when('clean_post_cache')->alias(function (): void {
            if ($this->duringUndo !== null) {
                ($this->duringUndo)();
            }
        });
    }

    /**
     * Start again with a stored Elementor page: a tree, $settings as its
     * page-settings rows and, unless null, $template as its page template.
     *
     * @param list<string> $settings Stored page-settings rows.
     */
    private function storedUndoPage(array $settings, ?string $template): void
    {
        $this->storedPage($settings);
        $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_DATA, '[{"id":"a1b2c3d","elType":"container","settings":[],"elements":[]}]');
        if ($template !== null) {
            $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, self::TEMPLATE, $template);
        }
    }

    /** A change that writes both keys: the hidden title, then a canvas template. */
    private function frameChange(): void
    {
        $result = $this->hideTitle();
        $this->assertSame(['ok' => true, 'written' => true], self::outcomeOf($result), (string) ($result['detail'] ?? ''));
        update_post_meta(self::NEW_ID, self::TEMPLATE, 'elementor_canvas');
    }

    /**
     * The page's stored rows of the page settings and the page template.
     *
     * @return array<string, list<string|null>>
     */
    private function frameRows(): array
    {
        return [ElementorDocument::KEY_PAGE_SETTINGS => $this->stored(ElementorDocument::KEY_PAGE_SETTINGS), self::TEMPLATE => $this->stored(self::TEMPLATE)];
    }

    /**
     * The page's stored rows of the page settings and the page template,
     * meta ids included.
     *
     * @return list<array<string, mixed>>
     */
    private function frameRowsWithIds(): array
    {
        return array_values(array_filter($this->wpdb->metaRowsOf(self::NEW_ID), static fn (array $r): bool => in_array($r['meta_key'], [ElementorDocument::KEY_PAGE_SETTINGS, self::TEMPLATE], true)));
    }

    /**
     * Takes, loads and decodes the post's snapshot, as page-edit and its
     * undo do.
     *
     * @return array<string, mixed>
     */
    private function snapshotOf(int $postId): array
    {
        $taken = BuilderDocumentSnapshot::take($postId, self::UNDO_REQUEST);
        $this->assertTrue($taken['ok'], (string) ($taken['code'] ?? ''));
        $loaded  = BuilderDocumentSnapshot::load(self::UNDO_REQUEST);
        $decoded = BuilderDocumentSnapshot::decode((string) ($loaded['json'] ?? ''), self::UNDO_REQUEST, $postId);
        $this->assertIsArray($decoded);

        return $decoded;
    }

    /**
     * update_post_meta() as core runs it: the value unslashed, an array
     * serialized, the key's rows replaced by one, and added_post_meta or
     * updated_post_meta fired; false when the bytes would not change.
     * afterMetaWrite then runs, as a listener on the write would.
     */
    private function coreMetaWrites(): void
    {
        Functions\when('update_post_meta')->alias(function ($postId, $key, $value) {
            $this->metaCalls[] = [(int) $postId, (string) $key, $value];
            $value  = self::unslash($value);
            $bytes  = is_array($value) ? serialize($value) : (string) $value;
            $read   = BuilderDocumentFingerprint::read((int) $postId, [(string) $key]);
            $before = $read === null ? [] : ($read['rows'][(string) $key] ?? []);
            if ($before === [$bytes]) {
                return false;
            }
            $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => (int) $postId, 'meta_key' => (string) $key]);
            $this->wpdb->addMeta(++$this->metaId, (int) $postId, (string) $key, $bytes);
            $this->fire($before === [] ? 'added_post_meta' : 'updated_post_meta', $this->metaId, (int) $postId, (string) $key, $value);
            if ($this->afterMetaWrite !== null) {
                ($this->afterMetaWrite)();
            }

            return $before === [] ? $this->metaId : true;
        });
    }

    /**
     * Start again with a stored Elementor page holding $rows as its
     * page-settings rows; answers its stand-in document.
     *
     * @param list<string> $rows Stored page-settings rows.
     */
    private function storedPage(array $rows): object
    {
        $this->fresh();
        $this->metaCalls          = [];
        $this->rows[self::NEW_ID] = [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => self::TITLE,
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-09 07:00:00',
        ];
        $this->wpdb->addPost(self::NEW_ID, $this->rows[self::NEW_ID]);
        foreach ($rows as $row) {
            $this->wpdb->addMeta(++$this->metaId, self::NEW_ID, ElementorDocument::KEY_PAGE_SETTINGS, $row);
        }

        return $this->api->addDocument(self::NEW_ID, 'wp-page');
    }

    /**
     * @return array<string,mixed>
     */
    private function hideTitle(): array
    {
        return (new ElementorDocument($this->api))->setPageSetting(self::NEW_ID, ElementorDocument::SETTING_HIDE_TITLE, ElementorDocument::HIDE_TITLE, self::KIT);
    }

    /**
     * precheck()'s answer with the hidden-title page settings in the
     * document's rows, as precheck() builds them for a page that opens with
     * a hero.
     *
     * @param array<string,mixed> $built precheck() answer.
     * @return array<string,mixed>
     */
    private function withHiddenTitle(array $built): array
    {
        $this->assertArrayNotHasKey('refusal', $built, (string) json_encode($built['refusal'] ?? null));
        $doc           = $built['doc'];
        $built['doc']  = new NativeDocument($doc->tree, $doc->meta + [ElementorDocument::KEY_PAGE_SETTINGS => ElementorDocument::createdPageSettings('full_width', true)]);

        return $built;
    }

    /**
     * @param array<string,mixed> $result setPageSetting() answer.
     * @return array{ok:mixed,written:mixed}
     */
    private static function outcomeOf(array $result): array
    {
        return ['ok' => $result['ok'] ?? null, 'written' => $result['written'] ?? null];
    }

    /**
     * Core's wp_unslash(), over arrays too.
     */
    private static function unslash(mixed $value): mixed
    {
        if (is_array($value)) {
            return array_map([self::class, 'unslash'], $value);
        }

        return is_string($value) ? stripslashes($value) : $value;
    }

    /**
     * A version 3 spec: one section holding the blocks of a plain outline, and a design.
     *
     * @return array<string,mixed>
     */
    private function v3Spec(string $frame, string $postType = 'page'): array
    {
        return [
            'post_type'        => $postType,
            'editor'           => 'builder:elementor',
            'title'            => self::TITLE,
            'elementor_format' => 'classic',
            'outline'          => [['type' => 'section', 'anchor' => 'intro', 'children' => self::outline()]],
            'outline_version'  => 3,
            'design'           => [
                'colors'  => ['ground' => '#F6F5F2', 'surface' => '#FFFFFF', 'ink' => '#15171C', 'muted' => '#5A606C', 'accent' => '#0D7A63', 'on_accent' => '#FFFFFF'],
                'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
                'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
                'radius'  => 'md',
                'density' => 'normal',
            ],
            'page_frame'       => $frame,
        ];
    }

    /**
     * @param array<string,mixed> $spec Spec.
     * @return array<string,mixed>
     */
    private function precheck(array $spec): array
    {
        $adapter = new ElementorAdapter($this->api, self::PRINCIPAL);

        return BuilderPageCreate::precheck($spec, $adapter, $adapter->facts(), [], self::request());
    }

    /**
     * @param array<string,mixed> $spec  Spec.
     * @param array<string,mixed> $built precheck() answer.
     * @return array<string,mixed>
     */
    private function write(array $spec, array $built): array
    {
        $ledger = function (array $fields): bool {
            $this->ledgerRow = array_merge($this->ledgerRow, $fields);

            return true;
        };

        return BuilderPageCreate::write(self::PRINCIPAL, $spec, new ElementorAdapter($this->api, self::PRINCIPAL), self::request(), $built, $ledger);
    }

    /**
     * A stand-in Elementor document for the next draft whose save stores
     * the tree as Elementor's does on a draft, updates the post row, and
     * runs $during inside the save.
     */
    private function elementorSaves(string $name = 'wp-page', ?callable $during = null): object
    {
        $document         = $this->api->addDocument(self::NEW_ID, $name);
        $document->onSave = function ($data) use ($during): bool {
            $json = json_encode($data['elements'], JSON_THROW_ON_ERROR);
            $this->rows[self::NEW_ID]['post_modified_gmt'] = '2026-10-09 07:00:05';
            $this->wpdb->addPost(self::NEW_ID, $this->rows[self::NEW_ID]);
            $this->fire('wp_insert_post', self::NEW_ID, (object) ['ID' => self::NEW_ID, 'post_type' => $this->rows[self::NEW_ID]['post_type'], 'post_parent' => 0], true);
            $this->rowUpdate(self::NEW_ID);
            $this->fire('wp_insert_post', self::REVISION, (object) ['ID' => self::REVISION, 'post_type' => 'revision', 'post_parent' => self::NEW_ID], false);
            foreach ([[self::NEW_ID, ElementorDocument::KEY_DATA, $json], [self::NEW_ID, '_elementor_version', '3.35.9'], [self::REVISION, ElementorDocument::KEY_DATA, $json]] as [$id, $key, $value]) {
                $this->wpdb->addMeta(++$this->metaId, $id, $key, $value);
                $this->fire('added_post_meta', $this->metaId, $id, $key, $value);
            }
            if ($during !== null) {
                $during();
            }

            return true;
        };

        return $document;
    }

    /**
     * An update of the post row, treating its page template as core's post
     * save does: a template other than the default that the theme does not
     * offer for the post type is put back to "default".
     */
    private function rowUpdate(int $postId): void
    {
        $template = $this->stored(self::TEMPLATE)[0] ?? '';
        if ($template === '' || $template === 'default') {
            return;
        }
        if (isset(($this->offered[$this->rows[$postId]['post_type']] ?? [])[$template])) {
            return;
        }
        $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => $postId, 'meta_key' => self::TEMPLATE]);
        $this->wpdb->insert($this->wpdb->postmeta, ['post_id' => $postId, 'meta_key' => self::TEMPLATE, 'meta_value' => 'default']);
        $this->fire('updated_post_meta', ++$this->metaId, $postId, self::TEMPLATE, 'default');
    }

    /**
     * The new draft's stored rows of one key, read with production's SQL.
     *
     * @return list<string|null>
     */
    private function stored(string $key): array
    {
        $read = BuilderDocumentFingerprint::read(self::NEW_ID, [$key]);

        return $read === null ? [] : ($read['rows'][$key] ?? []);
    }

    /**
     * Start again: only the kit, no draft, no ledger row.
     */
    private function fresh(): void
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
        $this->wpdb->addMeta(2, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->wpdb->addMeta(3, self::KIT, ElementorDocument::KEY_DATA, '[]');
        $this->rows      = [];
        $this->authors   = [];
        $this->inserts   = [];
        $this->ledgerRow = [];
    }

    private function fire(string $tag, mixed ...$args): void
    {
        $rows = array_values(array_filter($this->hooks, static fn ($r) => $r[0] === $tag));
        usort($rows, static fn ($a, $b) => $a[2] <=> $b[2]);
        foreach ($rows as [, $callback, , $accepted]) {
            $callback(...array_slice($args, 0, $accepted));
        }
    }

    private static function request(): string
    {
        $fixture = json_decode((string) file_get_contents(self::CONTAINERS), true, 512, JSON_THROW_ON_ERROR);

        return (string) $fixture['request_id'];
    }

    /**
     * The golden "heading" outline, as page-create validates it.
     *
     * @return list<array<string,mixed>>
     */
    private static function outline(): array
    {
        $fixture = json_decode((string) file_get_contents(self::CONTAINERS), true, 512, JSON_THROW_ON_ERROR);
        foreach ($fixture['cases'] as $case) {
            if ($case['name'] !== 'heading') {
                continue;
            }
            $valid = PageCreateBuilder::validate((object) [
                'post_type' => 'page',
                'editor'    => PageCreateBuilder::EDITOR_BLOCKS,
                'title'     => self::TITLE,
                'outline'   => json_decode((string) json_encode($case['input']), false, 512, JSON_THROW_ON_ERROR),
            ]);
            if (!isset($valid['spec'])) {
                throw new \LogicException('golden case does not validate');
            }

            return $valid['spec']['outline'];
        }
        throw new \LogicException('golden case missing');
    }

    /**
     * @param array<string,mixed> $result
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
