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
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\ElementorStyleCache;
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

    /** The id the next inserted post gets. */
    private const NEW_ID = 41;

    /** The revision Elementor's save makes of the new draft. */
    private const REVISION = 90;

    private const KIT = 4;

    private const OTHER = 77;

    private const PRINCIPAL = 2;

    private const TITLE = 'Our services & more';

    private const TEMPLATE = '_wp_page_template';

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
                $this->wpdb->addMeta(++$this->metaId, $id, (string) $key, stripslashes((string) $value));
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

        $this->assertSame('full_width', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero, $plain]]));
        $this->assertSame('full_width', BuilderPageCreate::pageFrame($v3 + ['outline' => json_decode((string) json_encode([$hero]))]), 'decoded objects');
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$plain]]));
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$plain, $hero]]), 'only the opening block can be the hero');
        $this->assertSame('canvas', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero], 'page_frame' => 'canvas']));
        $this->assertSame('theme', BuilderPageCreate::pageFrame($v3 + ['outline' => [$hero], 'page_frame' => 'theme']));
        $this->assertNull(BuilderPageCreate::pageFrame(['outline' => [$hero]]), 'an outline before version 3 has no frame');
        foreach ([
            'frame without version 3' => ['outline' => [], 'page_frame' => 'canvas'],
            'version 2 named'         => ['outline' => [], 'outline_version' => 2, 'page_frame' => 'canvas'],
            'unknown frame'           => $v3 + ['outline' => [], 'page_frame' => 'wide'],
            'frame not a string'      => $v3 + ['outline' => [], 'page_frame' => ['canvas']],
            'version as text'         => ['outline' => [], 'outline_version' => '3', 'page_frame' => 'canvas'],
        ] as $name => $spec) {
            $this->assertFalse(BuilderPageCreate::pageFrame($spec), $name);
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

    /**
     * A version 3 spec over a plain outline the classic mapper builds today.
     *
     * @return array<string,mixed>
     */
    private function v3Spec(string $frame, string $postType = 'page'): array
    {
        return [
            'post_type'       => $postType,
            'editor'          => 'builder:elementor',
            'title'           => self::TITLE,
            'outline'         => self::outline(),
            'outline_version' => 3,
            'page_frame'      => $frame,
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
