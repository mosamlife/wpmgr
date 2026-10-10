<?php
/**
 * ElementorDocument on the edit path: the edit target checks, the precheck
 * of an edited tree, and the whole-tree read-back against the page's
 * snapshot.
 *
 * Rows live in FakeBuilderWpdb and are read through the SQL reads production
 * uses. Every edit is planned by ElementorAdapter::planEdit() over the stored
 * row, as the page-edit call plans it. Snapshots are taken by
 * BuilderDocumentSnapshot::take() and read back through load() and decode().
 * Saves go through ElementorDocument::save() into a stand-in document that
 * stores the elements as Elementor's save does, optionally through a filter
 * that changes them first. Hooks the write scope and the side-effect
 * recorder install are captured through add_filter() and fired by hand.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorDocument
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorAdapter
 */
final class ElementorEditDocumentTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    private const TARGET = 41;

    private const KIT = 4;

    private const PRINCIPAL = 2;

    private const PERSON = 9;

    private const CREATE = '11111111-2222-4333-8444-777777777777';

    private const EDIT = '22222222-3333-4444-8555-888888888888';

    /** Ids in the golden two-column page. */
    private const ROW = '605cdc2';

    private const LEFT = 'c87900b';

    private const HEADING = '52982f9';

    private const RIGHT = '3d3406f';

    private const TEXT = '6cbe98a';

    /** A colour a person set on the heading: a setting WPMgr never writes. */
    private const PERSON_COLOR = '#c0392b';

    /** @var list<array{0:string,1:callable,2:int,3:int}> Captured add_filter calls: tag, callback, priority, accepted args. */
    private array $hooks = [];

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<int,int> Post id => the user whose autosave of it exists. */
    private array $autosaves = [];

    /** @var array<int,int> Post id => the user holding its edit lock. */
    private array $locks = [];

    private FakeBuilderWpdb $wpdb;

    private FakeElementorApi $api;

    private int $metaId = 100;

    private mixed $savedWpdb = null;

    private bool $hadWpdb = false;

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
        Functions\when('add_filter')->alias($capture);
        Functions\when('add_action')->alias($capture);
        Functions\when('remove_filter')->alias($release);
        Functions\when('remove_action')->alias($release);
        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wc_get_page_id')->justReturn(-1);
        Functions\when('add_option')->alias(fn ($name, $value = '', $unused = '', $autoload = null): bool => $this->wpdb->addOptionLikeCore((string) $name, $value, $unused, $autoload));
        Functions\when('delete_option')->alias(fn ($name): bool => $this->wpdb->deleteOptionLikeCore((string) $name));
        // As core: user 0 (the int) asks for an autosave by anyone, any other
        // id for that user's own.
        Functions\when('wp_get_post_autosave')->alias(function ($id, $user = 0) {
            $by = $this->autosaves[(int) $id] ?? null;

            return $by !== null && ($user === 0 || $user === $by) ? (object) ['ID' => 500, 'post_type' => 'revision', 'post_author' => (string) $by] : false;
        });
        // As core: the user holding the lock, never the current user.
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);

        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
        unset($GLOBALS['_wp_switched_stack']);

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->storePage(self::page());
        $this->api->addDocument(self::TARGET);
    }

    protected function tear_down(): void
    {
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->savedWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // editTargetProblem()
    // -------------------------------------------------------------------------

    public function test_autosave_by_anyone_refuses(): void
    {
        $doc   = new ElementorDocument($this->api);
        $facts = ['active_kit_id' => self::KIT];
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts), 'WPMgr\'s own draft, nobody in it');

        // A person's autosave, not the service user's: found only when the
        // question is about anyone.
        $this->autosaves[self::TARGET] = self::PERSON;
        $this->assertSame('autosave_pending', $doc->editTargetProblem(self::TARGET, $facts));
        $this->autosaves = [];

        // Elementor's own answer.
        $document                = $this->api->documents[self::TARGET];
        $document->newerAutosave = (object) ['ID' => 501];
        $this->assertSame('autosave_pending', $doc->editTargetProblem(self::TARGET, $facts));
        $document->newerAutosave = static function (): never {
            throw new \RuntimeException('the autosave could not be read');
        };
        $this->assertSame('autosave_unreadable', $doc->editTargetProblem(self::TARGET, $facts));
        $document->newerAutosave = null;
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts), 'no newer autosave');

        // A document that cannot be asked leaves WordPress's answer.
        $this->api->documents[self::TARGET] = new class () {
            public function get_name(): string
            {
                return 'wp-page';
            }

            public function is_editable_by_current_user(): bool
            {
                return true;
            }
        };
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts));
        $this->autosaves[self::TARGET] = self::PERSON;
        $this->assertSame('autosave_pending', $doc->editTargetProblem(self::TARGET, $facts));
    }

    public function test_edit_lock_refuses(): void
    {
        $doc   = new ElementorDocument($this->api);
        $facts = ['active_kit_id' => self::KIT];

        $this->locks[self::TARGET] = self::PERSON;
        $this->assertSame('editor_open', $doc->editTargetProblem(self::TARGET, $facts));
        $this->locks = [];
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts));
    }

    public function test_reserved_pages_refused(): void
    {
        $doc   = new ElementorDocument($this->api);
        $facts = ['active_kit_id' => self::KIT];
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts));

        // The active kit, as the facts name it and as Elementor names it now.
        $this->assertSame('active_kit', $doc->editTargetProblem(self::TARGET, ['active_kit_id' => self::TARGET]));
        $this->api->activeKitId = self::TARGET;
        $this->assertSame('active_kit', $doc->editTargetProblem(self::TARGET, $facts));
        $this->api->activeKitId = self::KIT;

        // The front page, the posts page and the shop page, each alone.
        $this->options = ['page_on_front' => (string) self::TARGET];
        $this->assertSame('front_page', $doc->editTargetProblem(self::TARGET, $facts));
        $this->options = ['page_for_posts' => self::TARGET];
        $this->assertSame('posts_page', $doc->editTargetProblem(self::TARGET, $facts));
        $this->options = [];
        Functions\when('wc_get_page_id')->alias(static fn ($page) => $page === 'shop' ? self::TARGET : -1);
        $this->assertSame('shop_page', $doc->editTargetProblem(self::TARGET, $facts));
        Functions\when('wc_get_page_id')->justReturn(-1);
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts));

        // Drafts only.
        $this->wpdb->update($this->wpdb->posts, ['post_status' => 'publish'], ['ID' => self::TARGET]);
        $this->assertSame('not_draft', $doc->editTargetProblem(self::TARGET, $facts));
        $this->wpdb->update($this->wpdb->posts, ['post_status' => 'draft'], ['ID' => self::TARGET]);

        // Pages and posts only, whatever Elementor calls the document.
        $this->wpdb->update($this->wpdb->posts, ['post_type' => 'post'], ['ID' => self::TARGET]);
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts));
        $this->wpdb->update($this->wpdb->posts, ['post_type' => 'product'], ['ID' => self::TARGET]);
        $this->api->addDocument(self::TARGET, 'wp-post');
        $this->assertSame('post_type', $doc->editTargetProblem(self::TARGET, $facts));
    }

    public function test_data_unreadable_refuses(): void
    {
        $doc   = new ElementorDocument($this->api);
        $facts = ['active_kit_id' => self::KIT];
        $deep  = str_repeat('[', 65) . str_repeat(']', 65);

        foreach ([
            'no row'     => [],
            'two rows'   => ['[]', '[]'],
            'an object'  => ['{"id":"a1b2c3d"}'],
            'not JSON'   => ['[{"id":'],
            'too deep'   => [$deep],
        ] as $name => $rows) {
            $this->replaceRows(ElementorDocument::KEY_DATA, $rows);
            $this->assertSame('data_unreadable', $doc->editTargetProblem(self::TARGET, $facts), $name);
        }
        $this->replaceRows(ElementorDocument::KEY_DATA, [(string) json_encode(self::page())]);
        $this->assertNull($doc->editTargetProblem(self::TARGET, $facts));
    }

    // -------------------------------------------------------------------------
    // precheckEditTree()
    // -------------------------------------------------------------------------

    public function test_admin_only_content_refuses(): void
    {
        // An administrator's script, in an HTML widget the edit does not touch.
        $page                             = self::page();
        $page[0]['elements'][1]['elements'][] = ['id' => 'aa11bb2', 'elType' => 'widget', 'settings' => ['html' => '<script>track()</script><p>Hours</p>'], 'elements' => [], 'widgetType' => 'html'];
        $this->api->widgetTypes[]         = 'html';
        $this->api->kses                  = static fn (array $data): array => self::stripScripts($data);
        $this->storePage($page);
        $doc  = new ElementorDocument($this->api);
        $plan = $this->planned([self::setText(self::HEADING, 'Summer sale')]);

        $this->assertSame(
            ['code' => 'page_has_admin_only_content', 'detail' => 'Elementor\'s sanitiser would change content already on the page'],
            $doc->precheckEditTree($page, $plan['doc']->tree, $plan['touched'])
        );
        $this->assertSame([], $this->api->callsTo('createElementInstance'), 'nothing is built for a page that is refused');

        // The same edit on the page without that widget passes.
        $this->storePage(self::page());
        $plan = $this->planned([self::setText(self::HEADING, 'Summer sale')]);
        $this->assertNull($doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched']));

        // New content the sanitiser would change is the new content's refusal.
        $this->api->kses = static fn (array $data): array => self::replaced($data, 'Summer sale', 'Summer');
        $this->assertSame(
            ['code' => 'sanitiser_changed_new_content', 'detail' => 'Elementor\'s sanitiser would change the new content'],
            $doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched'])
        );
        $this->api->kses = static fn (array $data): ?array => null;
        $this->assertSame(
            ['code' => 'sanitiser_changed_new_content', 'detail' => 'Elementor\'s sanitiser could not be asked'],
            $doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched'])
        );
    }

    public function test_unknown_widget_on_page_refuses(): void
    {
        // An add-on's widget, its plugin since deactivated, the edit not touching it.
        $page                                 = self::page();
        $page[0]['elements'][1]['elements'][] = ['id' => 'cc33dd4', 'elType' => 'widget', 'settings' => ['testimonial_content' => 'Great'], 'elements' => [], 'widgetType' => 'testimonial'];
        $this->storePage($page);
        $doc  = new ElementorDocument($this->api);
        $plan = $this->planned([self::setText(self::HEADING, 'Summer sale')]);

        $this->assertSame(
            ['code' => 'page_has_unknown_elements', 'detail' => 'elements[0].elements[1].elements[1]: widget type not registered'],
            $doc->precheckEditTree($page, $plan['doc']->tree, $plan['touched'])
        );
        $this->assertSame([], $this->api->callsTo('createElementInstance'), 'the types are known before anything is built');

        // A new node of a type the site does not register.
        $this->storePage(self::page());
        $plan                   = $this->planned([['op' => 'insert', 'after' => self::TEXT, 'outline' => [['type' => 'separator']]]]);
        $this->api->widgetTypes = ['heading', 'text-editor', 'button', 'image', 'spacer'];
        $this->assertSame(
            ['code' => 'page_has_unknown_elements', 'detail' => 'after the edit, elements[0].elements[1].elements[1]: widget type not registered'],
            $doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched'])
        );
    }

    public function test_dry_run_only_on_touched_nodes(): void
    {
        $doc  = new ElementorDocument($this->api);
        $plan = $this->planned([
            self::setText(self::HEADING, 'Summer sale'),
            ['op' => 'insert', 'after' => self::TEXT, 'outline' => [['type' => 'paragraph', 'text' => 'Open daily']]],
        ]);
        $made = $plan['touched'][1] ?? '';
        $this->assertSame([self::HEADING, $made], $plan['touched']);

        // Elementor would store every untouched node differently; none is built.
        $built                     = [];
        $untouched                 = [self::ROW, self::LEFT, self::RIGHT, self::TEXT];
        $this->api->elementFactory = static function (array $node) use (&$built, $untouched): object {
            $built[] = $node['id'];

            return new class ($node, $untouched) {
                /**
                 * @param array<string, mixed> $node      Node.
                 * @param list<string>         $untouched Ids the edit did not touch.
                 */
                public function __construct(private array $node, private array $untouched)
                {
                }

                /** @return array<string, mixed> */
                public function get_data_for_save(): array
                {
                    $data = $this->node;
                    if (in_array($data['id'], $this->untouched, true)) {
                        $data['settings']['_margin'] = ['unit' => 'px', 'top' => '1'];
                    }

                    return $data;
                }
            };
        };
        $this->assertNull($doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched']));
        $this->assertSame([self::HEADING, $made], $built, 'the touched nodes, each once, in page order');

        // A touched node Elementor would store differently refuses.
        $this->api->elementFactory = static fn (array $node): object => new class ($node, $made) {
            /** @param array<string, mixed> $node Node. */
            public function __construct(private array $node, private string $made)
            {
            }

            /** @return array<string, mixed> */
            public function get_data_for_save(): array
            {
                $data = $this->node;
                if ($data['id'] === $this->made) {
                    $data['settings']['editor'] = '<p>Open daily</p>' . "\n";
                }

                return $data;
            }
        };
        $this->assertSame(
            ['code' => 'builder_would_change_layout', 'detail' => 'elements[0].elements[1].elements[1]: Elementor would store the element differently'],
            $doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched'])
        );

        // A write while building refuses, and the recorder is disarmed after.
        $this->api->elementFactory = fn (array $node): object => new class ($node, fn () => $this->fire('updated_option', 'elementor_cache', 'a', 'b')) {
            /** @param array<string, mixed> $node Node. */
            public function __construct(private array $node, private \Closure $write)
            {
            }

            /** @return array<string, mixed> */
            public function get_data_for_save(): array
            {
                ($this->write)();

                return $this->node;
            }
        };
        $this->assertSame('side_effect_detected', $doc->precheckEditTree(self::page(), $plan['doc']->tree, $plan['touched'])['code'] ?? null);
        $this->assertSame([], $this->hooks);

        // An edit that makes and changes nothing builds nothing.
        $this->api->calls = [];
        $removed          = $this->planned([['op' => 'remove', 'ref' => self::TEXT]]);
        $this->assertSame([], $removed['touched']);
        $this->assertNull($doc->precheckEditTree(self::page(), $removed['doc']->tree, $removed['touched']));
        $this->assertSame([], $this->api->callsTo('createElementInstance'));
    }

    // -------------------------------------------------------------------------
    // verifyEdited()
    // -------------------------------------------------------------------------

    public function test_verify_catches_changed_untouched_node(): void
    {
        $ops = [self::setText(self::HEADING, 'Summer sale')];

        // A filter on the saved data changes the paragraph nobody asked to change.
        [$plan, $before] = $this->savedEdit($ops, static function (array $elements): array {
            $elements[0]['elements'][1]['elements'][0]['settings']['editor'] = '<p>Right column</p><p>added</p>';

            return $elements;
        });
        $doc = new ElementorDocument($this->api);
        $this->assertSame('tree_differs', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));
        $this->assertSame('tree_differs', (new ElementorAdapter($this->api, self::PRINCIPAL))->verifyEdited(self::TARGET, $plan['doc'], $before), 'the adapter delegates');

        // One dropped untouched node.
        [$plan, $before] = $this->savedEdit($ops, static function (array $elements): array {
            array_pop($elements[0]['elements']);

            return $elements;
        });
        $this->assertSame('tree_differs', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));

        // The same save without the filter is what was planned.
        [$plan, $before] = $this->savedEdit($ops);
        $this->assertNull($doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));
        $this->assertNull((new ElementorAdapter($this->api, self::PRINCIPAL))->verifyEdited(self::TARGET, $plan['doc'], $before));
    }

    public function test_verify_page_settings_byte_identical(): void
    {
        $settings  = 'a:2:{s:10:"hide_title";s:3:"yes";s:8:"template";s:7:"default";}';
        $reordered = 'a:2:{s:8:"template";s:7:"default";s:10:"hide_title";s:3:"yes";}';
        $this->assertEquals(unserialize($settings), unserialize($reordered), 'precondition: the same settings in other bytes');
        $ops = [self::setText(self::HEADING, 'Summer sale')];
        $doc = new ElementorDocument($this->api);

        [$plan, $before] = $this->savedEdit($ops, null, [[ElementorDocument::KEY_PAGE_SETTINGS, $settings]]);
        $this->assertNull($doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before), 'the row the snapshot holds');

        foreach ([
            'other bytes' => [$reordered],
            'removed'     => [],
            'a second row' => [$settings, $settings],
        ] as $name => $rows) {
            $this->replaceRows(ElementorDocument::KEY_PAGE_SETTINGS, $rows);
            $this->assertSame('page_settings', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before), $name);
        }

        // A page that had none gets one.
        [$plan, $before] = $this->savedEdit($ops);
        $this->replaceRows(ElementorDocument::KEY_PAGE_SETTINGS, ['a:0:{}']);
        $this->assertSame('page_settings', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));
    }

    public function test_verify_accepts_string_scalars_only_before_3_35_9(): void
    {
        [$plan, $before] = $this->savedEdit([self::setText(self::HEADING, 'Summer sale')]);
        $doc             = new ElementorDocument($this->api);
        $stored          = str_replace(['"isInner":true', '"isInner":false'], ['"isInner":"1"', '"isInner":""'], (string) json_encode($plan['doc']->tree));
        $this->assertNotSame((string) json_encode($plan['doc']->tree), $stored, 'precondition: the string form differs');
        $this->replaceRows(ElementorDocument::KEY_DATA, [$stored]);

        $this->api->version = '3.20.4';
        $this->assertNull($doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));
        foreach (['3.35.9', '4.3.4', null] as $version) {
            $this->api->version = $version;
            $this->assertSame('tree_differs', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before), (string) $version);
        }
    }

    public function test_verify_checks_the_post_and_its_rows(): void
    {
        $ops   = [self::setText(self::HEADING, 'Summer sale')];
        $doc   = new ElementorDocument($this->api);
        $cases = [
            'published'       => ['not_draft', fn () => $this->wpdb->update($this->wpdb->posts, ['post_status' => 'publish'], ['ID' => self::TARGET])],
            'other type'      => ['post_type', fn () => $this->wpdb->update($this->wpdb->posts, ['post_type' => 'post'], ['ID' => self::TARGET])],
            'other author'    => ['author', fn () => $this->wpdb->update($this->wpdb->posts, ['post_author' => (string) self::PERSON], ['ID' => self::TARGET])],
            'moved'           => ['parent', fn () => $this->wpdb->update($this->wpdb->posts, ['post_parent' => '12'], ['ID' => self::TARGET])],
            'renamed'         => ['post_name', fn () => $this->wpdb->update($this->wpdb->posts, ['post_name' => 'spring'], ['ID' => self::TARGET])],
            'edit mode'       => ['edit_mode', fn () => $this->replaceRows(ElementorDocument::KEY_EDIT_MODE, [''])],
            'document type'   => ['template_type', fn () => $this->replaceRows(ElementorDocument::KEY_TEMPLATE_TYPE, ['wp-post'])],
            'no data'         => ['data_missing', fn () => $this->replaceRows(ElementorDocument::KEY_DATA, [])],
            'data not JSON'   => ['data_not_json', fn () => $this->replaceRows(ElementorDocument::KEY_DATA, ['[{"id":'])],
        ];
        foreach ($cases as $name => [$token, $plant]) {
            [$plan, $before] = $this->savedEdit($ops);
            $this->assertNull($doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before), $name . ': before the plant');
            $plant();
            $this->assertSame($token, $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before), $name);
        }

        // A snapshot of another post, or without its tree, is not this page's.
        [$plan, $before] = $this->savedEdit($ops);
        $this->assertSame('snapshot_unreadable', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, ['post_id' => 77] + $before));
        $noTree         = $before;
        $noTree['meta'] = array_values(array_filter($before['meta'], static fn (array $pair): bool => $pair[0] !== ElementorDocument::KEY_DATA));
        $this->assertSame('snapshot_unreadable', $doc->verifyEdited(self::TARGET, $plan['doc']->tree, $noTree));
        $this->assertNull($doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));
    }

    public function test_verify_holds_what_the_edit_wrote_to_the_rules(): void
    {
        // The person's colour stays on the changed heading, and an HTML widget
        // no edit touched stays on the page: neither is WPMgr's writing.
        $page                                 = self::page();
        $page[0]['elements'][1]['elements'][] = ['id' => 'aa11bb2', 'elType' => 'widget', 'settings' => ['html' => '<p>Hours</p>', '_css_classes' => 'hours'], 'elements' => [], 'widgetType' => 'html'];
        $this->api->widgetTypes[]             = 'html';
        $ops                                  = [
            self::setText(self::HEADING, 'Summer sale'),
            ['op' => 'insert', 'after' => self::TEXT, 'outline' => [['type' => 'paragraph', 'text' => 'Open daily']]],
        ];
        [$plan, $before] = $this->savedEdit($ops, null, [], $page);
        $doc             = new ElementorDocument($this->api);
        $this->assertSame(self::PERSON_COLOR, $plan['doc']->tree[0]['elements'][0]['elements'][0]['settings']['title_color'] ?? null);
        $this->assertNull($doc->verifyEdited(self::TARGET, $plan['doc']->tree, $before));

        // A new node holding a key WPMgr never writes.
        $tree = $plan['doc']->tree;
        $tree[0]['elements'][1]['elements'][1]['settings']['_css_classes'] = 'promo';
        $this->replaceRows(ElementorDocument::KEY_DATA, [(string) json_encode($tree)]);
        $this->assertSame('leaf_policy', $doc->verifyEdited(self::TARGET, $tree, $before));

        // A changed node whose new text holds Elementor's dynamic-tag marker.
        $tree = $plan['doc']->tree;
        $tree[0]['elements'][0]['elements'][0]['settings']['title'] = 'Sale [elementor-tag id="1"]';
        $this->replaceRows(ElementorDocument::KEY_DATA, [(string) json_encode($tree)]);
        $this->assertSame('leaf_policy', $doc->verifyEdited(self::TARGET, $tree, $before));

        // A changed node with a setting WPMgr never writes, set by the edit.
        $tree = $plan['doc']->tree;
        $tree[0]['elements'][0]['elements'][0]['settings']['custom_css'] = 'h3{}';
        $this->replaceRows(ElementorDocument::KEY_DATA, [(string) json_encode($tree)]);
        $this->assertSame('leaf_policy', $doc->verifyEdited(self::TARGET, $tree, $before));
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * The golden two-column page as page-create built it, with a colour a
     * person later set on its heading.
     *
     * @return list<array<string, mixed>>
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
     * Fresh rows: the target a WPMgr Elementor draft holding $tree, then the
     * active kit; $extra are more [key, value] rows of the target.
     *
     * @param list<array<string, mixed>>        $tree  The stored tree.
     * @param list<array{0: string, 1: string}> $extra More target rows.
     */
    private function storePage(array $tree, array $extra = []): void
    {
        $this->wpdb      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->wpdb->addPost(self::TARGET, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => 'Spring',
            'post_content'      => 'Left column Right column',
            'post_author'       => (string) self::PRINCIPAL,
            'post_modified_gmt' => '2026-10-09 07:00:00',
        ]);
        $this->wpdb->addMeta(1, self::TARGET, ElementorDocument::MARKER_KEY, self::CREATE);
        $this->wpdb->addMeta(2, self::TARGET, ElementorDocument::KEY_EDIT_MODE, ElementorDocument::EDIT_MODE_BUILDER);
        $this->wpdb->addMeta(3, self::TARGET, ElementorDocument::KEY_TEMPLATE_TYPE, ElementorDocument::TEMPLATE_PAGE);
        $this->wpdb->addMeta(4, self::TARGET, ElementorDocument::KEY_DATA, (string) json_encode($tree));
        $this->wpdb->addMeta(5, self::TARGET, '_elementor_version', '3.35.9');
        $this->wpdb->addPost(self::KIT, ['post_type' => 'elementor_library', 'post_status' => 'publish', 'post_title' => 'Kit', 'post_content' => '', 'post_modified_gmt' => '2026-10-09 07:00:00']);
        $this->wpdb->addMeta(6, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->wpdb->addMeta(7, self::KIT, ElementorDocument::KEY_DATA, '[]');
        foreach ($extra as [$key, $value]) {
            $this->wpdb->addMeta(++$this->metaId, self::TARGET, $key, $value);
        }
    }

    /**
     * The edit planned over the stored row, as page-edit plans it.
     *
     * @param list<array<string, mixed>> $operations Operations as the AI sends them.
     * @return array<string, mixed> planEdit()'s answer: doc, changes, touched, new_count.
     */
    private function planned(array $operations): array
    {
        $parsed = PageEditValidator::parse((string) json_encode(['post_id' => self::TARGET, 'base_fingerprint' => str_repeat('a', 64), 'operations' => $operations]));
        $this->assertArrayHasKey('input', $parsed, (string) json_encode($parsed));
        $plan = (new ElementorAdapter($this->api, self::PRINCIPAL))->planEdit(self::TARGET, $parsed['input']['operations'], new IdSeed(self::EDIT), []);
        $this->assertArrayHasKey('doc', $plan, (string) json_encode($plan));

        return $plan;
    }

    /**
     * Store $page, plan $operations, take the snapshot, and save the planned
     * tree through ElementorDocument::save() into a document that stores it
     * as Elementor does, through $filter when given.
     *
     * @param list<array<string, mixed>>             $operations Operations.
     * @param (\Closure(list<mixed>): list<mixed>)|null $filter     Changes the saved elements.
     * @param list<array{0: string, 1: string}>      $extra      More target rows.
     * @param list<array<string, mixed>>|null        $page       The stored page; the golden page when null.
     * @return array{0: array<string, mixed>, 1: array<string, mixed>} The plan and the decoded snapshot.
     */
    private function savedEdit(array $operations, ?\Closure $filter = null, array $extra = [], ?array $page = null): array
    {
        $this->storePage($page ?? self::page(), $extra);
        $plan   = $this->planned($operations);
        $before = $this->snapshot();
        $this->api->storingDocument(self::TARGET, $this->wpdb, $filter);
        $saved = (new ElementorDocument($this->api))->save(self::TARGET, $plan['doc']->tree, self::KIT);
        $this->assertTrue($saved['ok'], (string) json_encode($saved));

        return [$plan, $before];
    }

    /**
     * Takes, stores, loads and decodes the target's snapshot, as the write does.
     *
     * @return array<string, mixed>
     */
    private function snapshot(): array
    {
        $taken = BuilderDocumentSnapshot::take(self::TARGET, self::EDIT);
        $this->assertTrue($taken['ok'], (string) ($taken['code'] ?? ''));
        $loaded  = BuilderDocumentSnapshot::load(self::EDIT);
        $decoded = BuilderDocumentSnapshot::decode((string) ($loaded['json'] ?? ''), self::EDIT, self::TARGET);
        $this->assertIsArray($decoded);
        $this->wpdb->deleteOptionLikeCore(BuilderDocumentSnapshot::OPTION_PREFIX . self::EDIT);

        return $decoded;
    }

    /**
     * Replaces every row of one exact key of the target with new rows.
     *
     * @param list<string> $values New rows, in order.
     */
    private function replaceRows(string $key, array $values): void
    {
        $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => self::TARGET, 'meta_key' => $key]);
        foreach ($values as $value) {
            $this->wpdb->insert($this->wpdb->postmeta, ['post_id' => self::TARGET, 'meta_key' => $key, 'meta_value' => $value]);
        }
    }

    /**
     * @return array<string, string>
     */
    private static function setText(string $ref, string $text): array
    {
        return ['op' => 'set_text', 'ref' => $ref, 'field' => 'text', 'text' => $text];
    }

    /**
     * Every string with script elements removed, as the sanitiser does for a
     * user without unfiltered_html.
     *
     * @param array<mixed> $data Data.
     * @return array<mixed>
     */
    private static function stripScripts(array $data): array
    {
        foreach ($data as $key => $value) {
            if (is_array($value)) {
                $data[$key] = self::stripScripts($value);
            } elseif (is_string($value)) {
                $data[$key] = (string) preg_replace('#<script\b[^>]*>.*?</script>#is', '', $value);
            }
        }

        return $data;
    }

    /**
     * Every string equal to $from replaced by $to.
     *
     * @param array<mixed> $data Data.
     * @return array<mixed>
     */
    private static function replaced(array $data, string $from, string $to): array
    {
        foreach ($data as $key => $value) {
            if (is_array($value)) {
                $data[$key] = self::replaced($value, $from, $to);
            } elseif ($value === $from) {
                $data[$key] = $to;
            }
        }

        return $data;
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
