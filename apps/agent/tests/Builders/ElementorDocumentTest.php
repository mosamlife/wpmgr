<?php
/**
 * ElementorDocument: the tree prechecks, the target guards, the save through
 * Elementor's document save inside the write scope, and the read-back.
 *
 * Elementor is FakeElementorApi with a stand-in document whose save() runs a
 * closure. Every hook the write scope and the side-effect recorder install is
 * captured through add_filter() and fired by hand from inside that closure,
 * as core would fire it during a real save. Rows live in FakeBuilderWpdb and
 * are read back through the same SQL reads production uses.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Commands\AbilityRunCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorDocument
 */
final class ElementorDocumentTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    private const TARGET = 41;

    private const KIT = 4;

    private const OTHER = 77;

    private const PRINCIPAL = 2;

    private const REQUEST = '11111111-2222-4333-8444-777777777777';

    /** Locales tried, in order, for one whose numeric name is not "C". */
    private const LOCALES = ['de_DE.UTF-8', 'de_DE.utf8', 'C.UTF-8', 'en_US.UTF-8', 'en_US.utf8'];

    /** @var list<array{0:string,1:callable,2:int,3:int}> Captured add_filter calls: tag, callback, priority, accepted args. */
    private array $hooks = [];

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<string,mixed> What get_post() answers for the target. */
    private array $post = [];

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
        $this->hooks   = [];
        $this->options = [];

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
        Functions\when('get_post')->alias(fn ($id) => (int) $id === self::TARGET && $this->post !== [] ? (object) $this->post : null);
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        Functions\when('wc_get_page_id')->justReturn(-1);

        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
        $this->wpdb      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;
        unset($GLOBALS['_wp_switched_stack']);
        $this->locale = setlocale(LC_NUMERIC, '0');

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->wpdb->addPost(self::TARGET, self::postRow('page', 'draft'));
        $this->wpdb->addMeta(1, self::TARGET, ElementorDocument::KEY_EDIT_MODE, 'builder');
        $this->wpdb->addPost(self::KIT, self::postRow('elementor_library', 'publish'));
        $this->wpdb->addMeta(2, self::KIT, '_elementor_page_settings', 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->wpdb->addMeta(3, self::KIT, '_elementor_data', '[]');
        $this->post = ['ID' => self::TARGET, 'post_author' => (string) self::PRINCIPAL, 'post_parent' => 0];
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

    public function test_saves_through_document_save_never_raw_meta(): void
    {
        Functions\expect('update_post_meta')->never();
        Functions\expect('add_post_meta')->never();
        Functions\expect('update_metadata')->never();
        Functions\expect('add_metadata')->never();
        $tree     = self::tree();
        $document = $this->api->addDocument(self::TARGET);

        $result = (new ElementorDocument($this->api))->save(self::TARGET, $tree, self::KIT);

        $this->assertTrue($result['ok']);
        $this->assertSame([['elements' => $tree]], $document->saves, 'one save, through the document, with the built tree');
        $this->assertSame([], $this->wpdb->queries === [] ? [] : array_filter($this->wpdb->queries, static fn ($q) => !str_starts_with($q['sql'], 'SELECT')), 'only reads reach the database');
    }

    public function test_settings_never_passed(): void
    {
        $document = $this->api->addDocument(self::TARGET);
        (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);

        $this->assertCount(1, $document->saves);
        $this->assertSame(['elements'], array_keys($document->saves[0]));
    }

    public function test_unknown_type_refused(): void
    {
        $doc = new ElementorDocument($this->api);

        $widget                                         = self::tree();
        $widget[0]['elements'][1]['elements'][0]['widgetType'] = 'wpmgr-not-registered';
        $this->assertSame(
            ['code' => 'page_has_unknown_elements', 'detail' => 'elements[0].elements[1].elements[0]: widget type not registered'],
            $doc->precheckTree($widget)
        );

        $elType = self::tree();
        $elType[] = ['id' => 'a000001', 'elType' => 'wpmgr-not-an-eltype', 'settings' => [], 'elements' => [], 'isInner' => false];
        $this->assertSame(
            ['code' => 'page_has_unknown_elements', 'detail' => 'elements[1]: element type not registered'],
            $doc->precheckTree($elType)
        );

        // Sections are refused where only containers are registered.
        $this->api->elementTypes = ['container'];
        $sections                = [['id' => 'a000002', 'elType' => 'section', 'settings' => ['structure' => '10'], 'elements' => [], 'isInner' => false]];
        $this->assertSame('page_has_unknown_elements', $doc->precheckTree($sections)['code'] ?? null);

        // The type check comes before Elementor builds anything.
        foreach ($this->api->calls as [$method]) {
            $this->assertNotSame('createElementInstance', $method);
        }

        $this->api->elementTypes = ['section', 'column', 'container'];
        $this->assertNull($doc->precheckTree(self::tree()), 'the golden tree passes');
    }

    public function test_kses_change_refused(): void
    {
        $doc = new ElementorDocument($this->api);

        // Elementor's own sanitiser changes one string.
        $this->api->kses = static function (array $data): array {
            $data[0]['elements'][0]['elements'][0]['settings']['title'] = 'Left';

            return $data;
        };
        $this->assertSame(
            ['code' => 'sanitiser_changed_new_content', 'detail' => 'Elementor\'s sanitiser would change the new content'],
            $doc->precheckTree(self::tree())
        );

        // A sanitiser that cannot be asked refuses: the document asks the API
        // and nothing else, even where WordPress's sanitiser would leave the
        // tree as it is.
        $this->api->kses = static fn (array $data): ?array => null;
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        $this->assertSame(
            ['code' => 'sanitiser_changed_new_content', 'detail' => 'Elementor\'s sanitiser could not be asked'],
            $doc->precheckTree(self::tree())
        );

        // The API's answer, unchanged, passes.
        $this->api->kses = null;
        $this->assertNull($doc->precheckTree(self::tree()), 'unchanged by the sanitiser');

        // Without Elementor nothing can be asked.
        $this->api->loaded = false;
        $this->assertSame('page_has_unknown_elements', $doc->precheckTree(self::tree())['code'] ?? null);
    }

    public function test_dry_run_difference_refused(): void
    {
        $doc = new ElementorDocument($this->api);

        // Elementor would store an untouched nested setting differently.
        $this->api->elementFactory = static fn (array $node): object => new class ($node) {
            /** @param array<string, mixed> $node Node. */
            public function __construct(private array $node)
            {
            }

            /** @return array<string, mixed> */
            public function get_data_for_save(): array
            {
                $data = $this->node;
                if (isset($data['elements'][1])) {
                    $data['elements'][1]['settings']['width']['size'] = '60';
                }

                return $data;
            }
        };
        $this->assertSame(
            ['code' => 'builder_would_change_layout', 'detail' => 'elements[0]: Elementor would store the element differently'],
            $doc->precheckTree(self::tree())
        );

        // A write while building, even of nothing that differs, refuses.
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
        $this->assertSame('side_effect_detected', $doc->precheckTree(self::tree())['code'] ?? null);
        $this->assertSame([], $this->hooks, 'the recorder is disarmed after the dry run');

        $this->api->elementFactory = null;
        $this->assertNull($doc->precheckTree(self::tree()));
    }

    public function test_locale_restored_after_refused_save(): void
    {
        $locale   = $this->otherLocale();
        $document = $this->api->addDocument(self::TARGET);
        $document->onSave = static fn (): bool => false;

        $result = (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);

        $this->assertSame('builder_save_refused', $result['code'] ?? null);
        $this->assertSame($locale, setlocale(LC_NUMERIC, '0'), 'the save left "C"; the wrapper put the locale back');
    }

    public function test_throwable_becomes_builder_crashed(): void
    {
        $locale   = $this->otherLocale();
        $document = $this->api->addDocument(self::TARGET);
        $document->onSave = static function (): bool {
            echo 'Warning: Undefined array key "widgetType"';
            throw new \RuntimeException('invalid prop');
        };

        $result = (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);

        $this->assertFalse($result['ok']);
        $this->assertSame('builder_crashed', $result['code'] ?? null);
        $this->assertStringNotContainsString('invalid prop', (string) ($result['detail'] ?? ''));
        $this->assertSame($locale, setlocale(LC_NUMERIC, '0'));
        $this->assertSame([], $this->hooks, 'the scope is disarmed after a throw');
        $this->expectOutputString('');
    }

    public function test_false_becomes_builder_save_refused(): void
    {
        foreach (['false' => false, 'null' => null, 'one' => 1, 'text' => 'true'] as $name => $answer) {
            $document = $this->api->addDocument(self::TARGET);
            $document->onSave = static fn () => $answer;
            $result = (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);
            $this->assertSame(['ok' => false, 'code' => 'builder_save_refused'], ['ok' => $result['ok'], 'code' => $result['code'] ?? null], $name);
        }

        $this->assertSame('builder_save_refused', (new ElementorDocument($this->api))->save(self::OTHER, self::tree(), self::KIT)['code'] ?? null, 'no document');
    }

    public function test_kit_write_detected(): void
    {
        $document = $this->api->addDocument(self::TARGET);
        $document->onSave = function (): bool {
            // A raw write to the kit, which no hook sees.
            $this->wpdb->addMeta(++$this->metaId, self::KIT, '_elementor_page_settings', 'a:0:{}');

            return true;
        };

        $result = (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);

        $this->assertSame(['ok' => false, 'code' => 'side_effect_detected', 'detail' => 'the active kit changed during the save'], array_diff_key($result, ['scope' => 1]));
    }

    public function test_other_post_write_during_save_detected(): void
    {
        $document = $this->api->addDocument(self::TARGET);
        $document->onSave = function (): bool {
            $this->fire('wp_insert_post', self::TARGET, (object) ['ID' => self::TARGET, 'post_type' => 'page', 'post_parent' => 0], true);
            $this->fire('wp_insert_post', 50, (object) ['ID' => 50, 'post_type' => 'revision', 'post_parent' => self::TARGET], false);
            $this->fire('added_post_meta', 7, self::TARGET, '_elementor_data', '[]');
            $this->fire('added_post_meta', 8, 50, '_elementor_data', '[]');

            return true;
        };
        $clean = (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);
        $this->assertTrue($clean['ok'], 'the target, its new revision and their meta are the save\'s own');
        $this->assertSame([50], $clean['scope']['revision_ids'] ?? null);

        $document->onSave = function (): bool {
            $this->fire('wp_insert_post', self::OTHER, (object) ['ID' => self::OTHER, 'post_type' => 'page', 'post_parent' => 0], true);
            $this->fire('added_post_meta', 9, self::OTHER, '_thumbnail_id', '5');
            $this->fire('updated_option', 'blogname', 'a', 'b');

            return true;
        };
        $dirty = (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);
        $this->assertSame('side_effect_detected', $dirty['code'] ?? null);
        $this->assertSame(['option_written', 'other_post_meta_written', 'other_post_written'], $dirty['scope']['violations'] ?? null);
        $this->assertSame('the save wrote outside the page: option_written, other_post_meta_written, other_post_written', $dirty['detail'] ?? null);
    }

    public function test_save_options_follow_the_version(): void
    {
        $document = $this->api->addDocument(self::TARGET);
        $document->onSave = function (): bool {
            $this->fire('added_option', '_elementor_assets_data', []);
            $this->fire('deleted_option', '_transient__elementor_editor_unsaved_' . self::TARGET);

            return true;
        };
        $doc = new ElementorDocument($this->api);

        $this->api->version = '3.20.4';
        $this->assertTrue($doc->save(self::TARGET, self::tree(), self::KIT)['ok'], '3.20.x writes its asset cache option');

        foreach (['3.35.9', '4.3.4', null] as $version) {
            $this->api->version = $version;
            $result             = $doc->save(self::TARGET, self::tree(), self::KIT);
            $this->assertSame(['option_written'], $result['scope']['violations'] ?? null, (string) $version);
        }

        // Another post's transient is never the target's.
        $this->api->version = '4.3.4';
        $document->onSave   = function (): bool {
            $this->fire('deleted_option', '_transient__elementor_editor_unsaved_999');

            return true;
        };
        $this->assertSame('side_effect_detected', $doc->save(self::TARGET, self::tree(), self::KIT)['code'] ?? null);
    }

    public function test_whole_tree_verify_catches_changed_untouched_node(): void
    {
        $tree = self::tree();
        $doc  = new ElementorDocument($this->api);
        $this->storeCreated(json_encode($tree, JSON_THROW_ON_ERROR));
        $this->assertNull($doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'));

        // A filter changed a node nobody asked to change.
        $changed = $tree;
        $changed[0]['elements'][1]['elements'][0]['settings']['editor'] = '<p>Right column</p><p>added</p>';
        $this->resetRows();
        $this->storeCreated(json_encode($changed, JSON_THROW_ON_ERROR));
        $this->assertSame('tree_differs', $doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'));

        // A dropped node.
        $dropped = $tree;
        unset($dropped[0]['elements'][1]);
        $dropped[0]['elements'] = array_values($dropped[0]['elements']);
        $this->resetRows();
        $this->storeCreated(json_encode($dropped, JSON_THROW_ON_ERROR));
        $this->assertSame('tree_differs', $doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'));
    }

    public function test_verify_accepts_the_string_scalar_form_only_before_3_35_9(): void
    {
        $tree   = self::tree();
        $doc    = new ElementorDocument($this->api);
        $stored = str_replace(['"isInner":true', '"isInner":false'], ['"isInner":"1"', '"isInner":""'], json_encode($tree, JSON_THROW_ON_ERROR));
        $this->storeCreated($stored);

        $this->api->version = '3.20.4';
        $this->assertNull($doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'));
        foreach (['3.35.9', '4.3.4', null] as $version) {
            $this->api->version = $version;
            $this->assertSame('tree_differs', $doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'), (string) $version);
        }
    }

    public function test_verify_checks_every_stored_row(): void
    {
        $tree = self::tree();
        $doc  = new ElementorDocument($this->api);
        $json = json_encode($tree, JSON_THROW_ON_ERROR);

        $cases = [
            'not a draft'      => ['not_draft', fn () => $this->wpdb->addPost(self::TARGET, self::postRow('page', 'publish'))],
            'other type'       => ['post_type', fn () => $this->wpdb->addPost(self::TARGET, self::postRow('post', 'draft'))],
            'other author'     => ['author', fn () => $this->post['post_author'] = '3'],
            'has a parent'     => ['parent', fn () => $this->post['post_parent'] = 9],
            'page settings'    => ['page_settings', fn () => $this->wpdb->addMeta(++$this->metaId, self::TARGET, '_elementor_page_settings', 'a:0:{}')],
            'other template'   => ['template_type', fn () => $this->wpdb->addMeta(++$this->metaId, self::TARGET, '_elementor_template_type', 'wp-post')],
            'second data row'  => ['data_missing', fn () => $this->wpdb->addMeta(++$this->metaId, self::TARGET, '_elementor_data', $json)],
            'other marker'     => ['marker', fn () => $this->wpdb->addMeta(++$this->metaId, self::TARGET, ElementorDocument::MARKER_KEY, 'x')],
        ];
        foreach ($cases as $name => [$token, $plant]) {
            $this->resetRows();
            $this->post = ['ID' => self::TARGET, 'post_author' => (string) self::PRINCIPAL, 'post_parent' => 0];
            $this->storeCreated($json);
            $plant();
            $this->assertSame($token, $doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'), $name);
        }

        $this->resetRows();
        $this->storeCreated('{"not":"a tree"');
        $this->assertSame('data_not_json', $doc->verifyCreated(self::TARGET, $tree, self::PRINCIPAL, self::REQUEST, 'page'));
        $this->assertSame(AbilityRunCommand::META_CREATED_BY, ElementorDocument::MARKER_KEY);
    }

    public function test_target_guards_refuse_kit_front_posts_page_and_non_draft(): void
    {
        $doc   = new ElementorDocument($this->api);
        $facts = ['active_kit_id' => self::KIT];
        $this->api->addDocument(self::TARGET);
        $this->assertNull($doc->targetProblem(self::TARGET, $facts), 'WPMgr\'s own Elementor draft');

        // Each reserved page, alone.
        $this->options = ['page_on_front' => (string) self::TARGET];
        $this->assertSame('front_page', $doc->targetProblem(self::TARGET, $facts));
        $this->options = ['page_for_posts' => self::TARGET];
        $this->assertSame('posts_page', $doc->targetProblem(self::TARGET, $facts));
        $this->options = [];
        Functions\when('wc_get_page_id')->alias(static fn ($page) => $page === 'shop' ? self::TARGET : -1);
        $this->assertSame('shop_page', $doc->targetProblem(self::TARGET, $facts));
        Functions\when('wc_get_page_id')->justReturn(-1);
        $this->assertSame('active_kit', $doc->targetProblem(self::TARGET, ['active_kit_id' => self::TARGET]));
        $this->api->activeKitId = self::TARGET;
        $this->assertSame('active_kit', $doc->targetProblem(self::TARGET, $facts), 'the kit Elementor names now');
        $this->api->activeKitId = self::KIT;

        // Not a draft; not an Elementor page.
        $this->wpdb->addPost(self::TARGET, self::postRow('page', 'publish'));
        $this->assertSame('not_draft', $doc->targetProblem(self::TARGET, $facts));
        $this->resetRows(false);
        $this->assertSame('not_builder_page', $doc->targetProblem(self::TARGET, $facts));
        $this->wpdb->addMeta(++$this->metaId, self::TARGET, ElementorDocument::KEY_EDIT_MODE, 'builder');

        // Elementor's own answers.
        $this->api->addDocument(self::TARGET, 'kit');
        $this->assertSame('document_type', $doc->targetProblem(self::TARGET, $facts));
        $this->api->addDocument(self::TARGET, 'wp-post', false);
        $this->assertSame('not_editable', $doc->targetProblem(self::TARGET, $facts));
        unset($this->api->documents[self::TARGET]);
        $this->assertSame('document_missing', $doc->targetProblem(self::TARGET, $facts));
        $this->assertSame('post_missing', $doc->targetProblem(self::OTHER, $facts));
    }

    public function test_scope_armed_only_during_save(): void
    {
        $seen     = [];
        $document = $this->api->addDocument(self::TARGET);
        $document->onSave = function () use (&$seen): bool {
            $seen = array_column($this->hooks, 0);

            return true;
        };
        $this->assertSame([], $this->hooks, 'nothing installed before the save');

        (new ElementorDocument($this->api))->save(self::TARGET, self::tree(), self::KIT);

        foreach (['map_meta_cap', 'user_has_cap', 'wp_insert_post', 'added_post_meta', 'added_option', 'pre_http_request'] as $tag) {
            $this->assertContains($tag, $seen, $tag . ' is armed during the save');
        }
        $this->assertSame([], $this->hooks, 'nothing left installed after the save');
    }

    /**
     * The golden containers tree for two columns.
     *
     * @return list<array<string, mixed>>
     */
    private static function tree(): array
    {
        $fixture = json_decode((string) file_get_contents(self::FIXTURE), true, 512, JSON_THROW_ON_ERROR);
        foreach ($fixture['cases'] as $case) {
            if ($case['name'] === 'columns') {
                return $case['tree'];
            }
        }
        throw new \LogicException('the columns golden is missing');
    }

    /**
     * The stored rows of a page this request created, with $data as its tree.
     */
    private function storeCreated(string $data): void
    {
        $this->wpdb->addMeta(++$this->metaId, self::TARGET, ElementorDocument::MARKER_KEY, self::REQUEST);
        $this->wpdb->addMeta(++$this->metaId, self::TARGET, ElementorDocument::KEY_TEMPLATE_TYPE, 'wp-page');
        $this->wpdb->addMeta(++$this->metaId, self::TARGET, ElementorDocument::KEY_DATA, $data);
        $this->wpdb->addMeta(++$this->metaId, self::TARGET, '_elementor_version', '3.35.9');
    }

    /**
     * Start the rows again: the target a draft page, the kit, and the
     * target's edit mode row unless $editMode is false.
     */
    private function resetRows(bool $editMode = true): void
    {
        $this->wpdb      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->wpdb->addPost(self::TARGET, self::postRow('page', 'draft'));
        $this->wpdb->addPost(self::KIT, self::postRow('elementor_library', 'publish'));
        if ($editMode) {
            $this->wpdb->addMeta(1, self::TARGET, ElementorDocument::KEY_EDIT_MODE, 'builder');
        }
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
     * Switch the numeric locale to one whose name is not "C" and return it.
     */
    private function otherLocale(): string
    {
        foreach (self::LOCALES as $candidate) {
            $set = setlocale(LC_NUMERIC, $candidate);
            if (is_string($set) && $set !== 'C' && $set !== 'POSIX') {
                return $set;
            }
        }
        $this->fail('no locale other than C is installed');
    }

    /**
     * @return array<string, string>
     */
    private static function postRow(string $type, string $status): array
    {
        return [
            'post_type'         => $type,
            'post_status'       => $status,
            'post_title'        => 'Page',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-09 07:00:00',
        ];
    }
}
