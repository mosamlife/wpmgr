<?php
/**
 * wpmgr/page-create with editor builder:elementor, driven through the real
 * signed Router dispatch: the catalogue entry's builders_enabled, the builder
 * registry, the precheck and its digests, the write and its replay, the
 * digest re-check, the trash on a failed verify, and undo of a builder draft.
 *
 * Elementor is FakeElementorApi behind the real ElementorAdapter, handed to
 * the command through its test-only adapter seam. Every inserted post gets a
 * stand-in Elementor document whose save() stores the tree the way
 * Elementor's save does on a draft: the post row moves, one revision is
 * inserted, and the tree and version rows are added, each firing the hook
 * core fires. Post and meta rows live in FakeBuilderWpdb and are read back
 * through the same SQL reads production uses; the ledger's option rows use
 * the same wpdb.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Commands\AbilityRunCommand;
use WPMgr\Agent\Commands\ContentEditingEnableCommand;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AbilityRunCommand
 * @covers \WPMgr\Agent\Abilities\PageCreateBuilder
 * @covers \WPMgr\Agent\Abilities\OwnAbilities
 */
final class ElementorPageCreateRouterTest extends TestCase
{
    private const CONTAINERS = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    private const SECTIONS = __DIR__ . '/../fixtures/ability-run/elementor-classic-sections.json';

    /** The goldens' request id: the node ids in their trees derive from it. */
    private const REQ_GOLDEN = '11111111-2222-4333-8444-777777777777';

    private const REQ_B = '99999999-2222-4333-8444-555555555555';

    private const KIT = 4;

    private const TITLE = 'Our services';

    private string $keyFile;

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;

    private FakeElementorApi $api;

    private FakeBuilderWpdb $rows;

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<int,object> */
    private array $posts = [];

    /** @var array<int,array<string,mixed>> */
    private array $meta = [];

    /** @var array<string,object> */
    private array $roles = [];

    /** @var array<int,object> */
    private array $users = [];

    private int $currentUser = 0;

    private int $nextId = 100;

    private int $metaId = 1000;

    /** @var list<array{0:string,1:callable,2:int,3:int}> Captured hooks: tag, callback, priority, accepted args. */
    private array $hooks = [];

    /** @var array<int,list<object>> Revisions by parent id. */
    private array $revisions = [];

    /** @var array<int,true> Posts with an autosave. */
    private array $autosaves = [];

    /** @var array<int,int> Edit lock holder by post id. */
    private array $locks = [];

    /** @var list<int> Ids wp_insert_post created, in order. */
    private array $inserted = [];

    /** @var (\Closure(list<mixed>): list<mixed>)|null Changes the tree Elementor stores, as a filter on its save would. */
    private ?\Closure $alterOnSave = null;

    /** @var string|false */
    private $locale = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-bfc-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
        $this->locale = setlocale(LC_NUMERIC, '0');

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
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('add_option')->alias(function ($name, $value = '') {
            if (array_key_exists($name, $this->options)) {
                return false;
            }
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('delete_option')->alias(function ($name) {
            $had = array_key_exists($name, $this->options);
            unset($this->options[$name]);

            return $had;
        });
        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('register_rest_route')->justReturn(true);
        Functions\when('wc_get_page_id')->justReturn(-1);
        $GLOBALS['wp_version'] = '6.4.2';

        // Roles and users, as PageCreateWriteTest has them.
        Functions\when('get_role')->alias(fn ($r) => $this->roles[$r] ?? null);
        Functions\when('add_role')->alias(function ($r, $label, $caps) {
            if (isset($this->roles[$r])) {
                return null;
            }
            $this->roles[$r] = (object) ['name' => $r, 'capabilities' => $caps];

            return $this->roles[$r];
        });
        Functions\when('get_userdata')->alias(fn ($id) => $this->users[(int) $id] ?? false);
        Functions\when('get_user_by')->alias(function ($field, $value) {
            foreach ($this->users as $u) {
                if ($field === 'login' && $u->user_login === $value) {
                    return $u;
                }
            }

            return false;
        });
        Functions\when('wp_insert_user')->alias(function (array $data) {
            $id               = $this->nextId++;
            $this->users[$id] = (object) ['ID' => $id, 'user_login' => $data['user_login'], 'roles' => [$data['role']], 'caps' => [$data['role'] => true]];

            return $id;
        });
        Functions\when('update_user_meta')->justReturn(true);
        Functions\when('wp_set_current_user')->alias(function ($id) {
            $this->currentUser = (int) $id;

            return null;
        });
        Functions\when('current_user_can')->alias(function ($cap, ...$args) {
            $u = $this->users[$this->currentUser] ?? null;
            if ($u === null) {
                return false;
            }
            if ($cap === 'read_post') {
                return true;
            }
            foreach ($u->roles as $r) {
                if (!empty($this->roles[$r]->capabilities[$cap])) {
                    return true;
                }
            }

            return false;
        });

        // Posts: the post objects core hands out, and the raw rows.
        $slash = static function ($value) use (&$slash) {
            if (is_array($value)) {
                return array_map($slash, $value);
            }

            return is_string($value) ? addslashes($value) : $value;
        };
        Functions\when('wp_slash')->alias($slash);
        Functions\when('wp_kses_post')->returnArg();
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('get_post')->alias(fn ($id) => $this->posts[(int) $id] ?? null);
        Functions\when('get_post_meta')->alias(fn ($id, $key = '', $single = false) => $this->meta[(int) $id][$key] ?? '');
        Functions\when('wp_insert_post')->alias(fn (array $data, $wpError = false) => $this->insertPost($data));
        Functions\when('wp_trash_post')->alias(function ($id) {
            $id = (int) $id;
            if (!isset($this->posts[$id])) {
                return false;
            }
            $this->posts[$id]->post_status = 'trash';
            $this->syncRow($id);

            return $this->posts[$id];
        });
        Functions\when('wp_get_post_autosave')->alias(fn ($id, $user = 0) => isset($this->autosaves[(int) $id]) ? (object) ['ID' => 500, 'post_type' => 'revision'] : false);
        Functions\when('wp_get_post_revisions')->alias(function ($id, $args = null) {
            $out = [];
            foreach ($this->revisions[(int) $id] ?? [] as $revision) {
                $out[$revision->ID] = $revision;
            }

            return $out;
        });
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $this->rows      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb($this->rows);
        $this->rows->addPost(self::KIT, [
            'post_type'         => 'elementor_library',
            'post_status'       => 'publish',
            'post_title'        => 'Kit',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-01 07:00:00',
        ]);
        $this->rows->addMeta(2, self::KIT, '_elementor_page_settings', 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->rows->addMeta(3, self::KIT, '_elementor_data', '[]');

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->resetShieldStash();

        $api          = $this->api;
        $this->router = new Router(
            new Connector($keystore, new Settings()),
            [new AbilityRunCommand(static fn (): array => ['elementor' => new ElementorAdapter($api)]), new ContentEditingEnableCommand()]
        );
    }

    protected function tear_down(): void
    {
        $this->resetShieldStash();
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        if (is_string($this->locale)) {
            setlocale(LC_NUMERIC, $this->locale);
        }
        unset($GLOBALS['wpdb'], $GLOBALS['wp_version']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_builder_editor_refused_when_not_in_builders_enabled(): void
    {
        $this->enable();
        $input = $this->input('columns');

        foreach (['no builders_enabled' => null, 'another builder' => ['bricks'], 'empty list' => []] as $why => $enabled) {
            $pre = $this->precheck(self::REQ_GOLDEN, $input, $enabled);
            $this->assertSame('builder_not_enabled', $pre['code'] ?? null, $why . ': ' . json_encode($pre));
            $write = $this->write(self::REQ_GOLDEN, $input, str_repeat('a', 64), str_repeat('b', 64), $enabled);
            $this->assertSame('builder_not_enabled', $write['code'] ?? null, $why . ' (write)');
        }
        foreach (['an object' => ['elementor' => true], 'a number in the list' => ['elementor', 7], 'an id twice' => ['elementor', 'elementor']] as $why => $enabled) {
            $pre = $this->precheck(self::REQ_GOLDEN, $input, $enabled);
            $this->assertSame('bad_input', $pre['code'] ?? null, $why . ': ' . json_encode($pre));
        }

        $this->assertSame([], $this->inserted, 'nothing was created');
        $this->assertNull(AbilityLedger::get(self::REQ_GOLDEN), 'no ledger row');
        $this->assertSame([], $this->api->calls, 'Elementor was never asked');
    }

    public function test_unknown_builder_id_refused(): void
    {
        $this->enable();
        $evil = $this->input('columns', ['editor' => 'builder:evil']);
        $pre  = $this->precheck(self::REQ_GOLDEN, $evil, ['elementor', 'evil']);
        $this->assertSame('bad_input', $pre['code'] ?? null, 'an id outside the agent\'s set is refused even when the entry lists it: ' . json_encode($pre));
        $write = $this->write(self::REQ_GOLDEN, $evil, str_repeat('a', 64), str_repeat('b', 64), ['elementor', 'evil']);
        $this->assertSame('bad_input', $write['code'] ?? null);

        $beaver = $this->precheck(self::REQ_GOLDEN, $this->input('columns', ['editor' => 'builder:beaver']), ['elementor', 'beaver']);
        $this->assertSame(['builder_not_available', 'not_compiled'], [$beaver['code'] ?? null, $beaver['detail'] ?? null], 'a known builder that is not compiled in');

        foreach (['builder:', 'builder:Elementor', 'builder:elementor ', "builder:elementor\n", 'elementor'] as $editor) {
            $r = $this->precheck(self::REQ_GOLDEN, $this->input('columns', ['editor' => $editor]), ['elementor']);
            $this->assertSame('bad_input', $r['code'] ?? null, json_encode($editor));
        }
        $this->assertSame([], $this->inserted);
        $this->assertNull(AbilityLedger::get(self::REQ_GOLDEN));
    }

    public function test_precheck_returns_tree_preview_and_digests(): void
    {
        $this->enable();
        $input = $this->input('columns');
        $pre   = $this->precheck(self::REQ_GOLDEN, $input);

        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $this->assertSame('prechecked', $pre['outcome']);
        $this->assertSame(['post_type', 'editor', 'format', 'elementor_version', 'layout', 'status', 'title', 'tree'], array_keys($pre['preview']));
        $this->assertSame(
            ['post_type' => 'page', 'editor' => 'builder:elementor', 'format' => 'classic', 'elementor_version' => '3.35.9', 'layout' => 'containers', 'status' => 'draft', 'title' => self::TITLE],
            array_diff_key($pre['preview'], ['tree' => 1])
        );
        $golden = self::golden(self::CONTAINERS, 'columns')['tree'];
        $this->assertSame($golden, $pre['preview']['tree'], 'the tree is the golden Elementor stored for this outline and request');
        $this->assertSame(
            hash('sha256', (string) json_encode(['builder:elementor', 'classic', '3.35.9', 'page', 'draft', self::TITLE, json_encode($golden)])),
            $pre['preview_digest']
        );
        $this->assertSame(PageCreateBuilder::baseFingerprint('page', []), $pre['base_fingerprint']);
        $entry = $this->entry(['elementor']);
        $this->assertSame(
            hash('sha256', (string) json_encode([hash('sha256', $entry), hash('sha256', $input), $pre['base_fingerprint'], $pre['preview_digest']])),
            $pre['precheck_digest'],
            'the precheck digest binds the entry, the input bytes, the base and the preview'
        );

        // Sections when the site has the container layout off.
        $this->api->experiments['container'] = false;
        $sections                            = $this->precheck(self::REQ_GOLDEN, $input);
        $this->assertSame('sections', $sections['preview']['layout'] ?? null);
        $this->assertSame(self::golden(self::SECTIONS, 'columns')['tree'], $sections['preview']['tree'] ?? null);

        $this->assertSame([], $this->inserted, 'a precheck creates nothing');
        $this->assertNull(AbilityLedger::get(self::REQ_GOLDEN));
        $this->assertSame(0, $this->currentUser, 'the user is switched back to 0');
        $this->assertSame([], $this->hooks, 'nothing is left hooked');
    }

    public function test_write_creates_elementor_draft_and_verifies(): void
    {
        $uid = (int) $this->enable()['user_id'];
        $pre = $this->precheck(self::REQ_GOLDEN, $this->input('columns'));
        $r   = $this->write(self::REQ_GOLDEN, $this->input('columns'), $pre['precheck_digest'], $pre['preview_digest']);

        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame('created', $r['outcome']);
        $id = (int) $r['post_id'];
        $this->assertSame([$id], $this->inserted);
        $this->assertSame(['builder:elementor', 'classic', '3.35.9', $pre['preview_digest']], [$r['editor'], $r['format'], $r['elementor_version'], $r['preview_digest']]);
        $this->assertSame(['tree_equal' => true, 'status' => 'draft'], $r['verify']);

        $post = $this->posts[$id];
        $this->assertSame(['draft', 'page', $uid, self::TITLE, ''], [$post->post_status, $post->post_type, $post->post_author, $post->post_title, $post->post_content]);
        $stored = BuilderDocumentFingerprint::read($id, array_merge(ElementorDocument::DESCRIPTOR_KEYS, [ElementorDocument::MARKER_KEY]));
        $this->assertSame([self::REQ_GOLDEN], $stored['rows'][ElementorDocument::MARKER_KEY]);
        $this->assertSame(['builder'], $stored['rows'][ElementorDocument::KEY_EDIT_MODE]);
        $this->assertSame(['wp-page'], $stored['rows'][ElementorDocument::KEY_TEMPLATE_TYPE]);
        $this->assertArrayNotHasKey(ElementorDocument::KEY_PAGE_SETTINGS, $stored['rows']);
        $this->assertSame($pre['preview']['tree'], json_decode($stored['rows'][ElementorDocument::KEY_DATA][0], true), 'Elementor stored the approved tree');
        $this->assertCount(1, $this->api->documents[$id]->saves, 'one save, through Elementor\'s document');

        $ledger = AbilityLedger::get(self::REQ_GOLDEN);
        $this->assertSame('completed', $ledger['phase']);
        $this->assertSame($id, $ledger['created_post_id']);
        $this->assertSame(['elementor', '3.35.9', 'classic'], [$ledger['builder'], $ledger['builder_version'], $ledger['format']]);
        $this->assertSame(array_map(static fn ($o) => $o->ID, $this->revisions[$id]), $ledger['own_revision_ids']);
        $this->assertSame(BuilderDocumentFingerprint::ofPost($id, ElementorDocument::DESCRIPTOR_KEYS), $ledger['after_fp']);
        $this->assertSame($r['after_fp'], $ledger['after_fp']);
        $this->assertSame('available', $ledger['undo_state']);
        $this->assertFalse(AbilityLedger::inflight(self::REQ_GOLDEN), 'the claim is released');
        $this->assertSame(0, $this->currentUser);
        $this->assertSame([], $this->hooks, 'the write scope is disarmed');
    }

    public function test_replayed_write_is_idempotent(): void
    {
        $this->enable();
        $pre   = $this->precheck(self::REQ_GOLDEN, $this->input('columns'));
        $first = $this->write(self::REQ_GOLDEN, $this->input('columns'), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('created', $first['outcome'] ?? null, (string) json_encode($first));

        $again = $this->write(self::REQ_GOLDEN, $this->input('columns'), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertTrue($again['ok']);
        $this->assertSame('already_applied', $again['outcome']);
        $this->assertSame($first['post_id'], $again['post_id']);
        $this->assertSame($first, $again['result'], 'the replay answers the recorded result');
        $this->assertCount(1, $this->inserted, 'no second draft');
        $this->assertCount(1, $this->api->documents[(int) $first['post_id']]->saves, 'no second save');
    }

    public function test_preview_changed_when_tree_differs(): void
    {
        $this->enable();
        $input = $this->input('columns');
        $pre   = $this->precheck(self::REQ_GOLDEN, $input);
        $this->assertSame('containers', $pre['preview']['layout'] ?? null);

        // The site turns the container layout off after the approval: the
        // write would build sections, not the approved containers.
        $this->api->experiments['container'] = false;
        $r = $this->write(self::REQ_GOLDEN, $input, $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('preview_changed', $r['code'] ?? null, (string) json_encode($r));
        $this->assertSame([], $this->inserted, 'nothing was created');
        $this->assertNull(AbilityLedger::get(self::REQ_GOLDEN), 'no ledger row');

        // Approved and written with nothing changed in between, the same
        // input is created: the write rebuilds exactly what was approved.
        $fresh = $this->precheck(self::REQ_B, $input);
        $ok    = $this->write(self::REQ_B, $input, $fresh['precheck_digest'], $fresh['preview_digest']);
        $this->assertSame('created', $ok['outcome'] ?? null, (string) json_encode($ok));
        $this->assertSame('sections', $fresh['preview']['layout'] ?? null);
    }

    public function test_verify_failure_trashes(): void
    {
        $this->enable();
        $pre = $this->precheck(self::REQ_GOLDEN, $this->input('columns'));

        // Something on the site changes a node while Elementor saves.
        $this->alterOnSave = static function (array $tree): array {
            $tree[0]['settings']['flex_direction'] = 'column';

            return $tree;
        };
        $r = $this->write(self::REQ_GOLDEN, $this->input('columns'), $pre['precheck_digest'], $pre['preview_digest']);

        $this->assertFalse($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame('verify_mismatch', $r['code']);
        $this->assertTrue($r['trashed']);
        $id = (int) $r['post_id'];
        $this->assertSame([$id], $this->inserted);
        $this->assertSame('trash', $this->posts[$id]->post_status);
        $ledger = AbilityLedger::get(self::REQ_GOLDEN);
        $this->assertSame(['failed', 'trashed'], [$ledger['phase'], $ledger['undo_state']]);
        $this->assertFalse(AbilityLedger::inflight(self::REQ_GOLDEN));
    }

    public function test_undo_trashes_builder_draft_with_own_revisions(): void
    {
        $id = $this->createDraft();
        $this->assertNotSame([], $this->revisions[$id] ?? [], 'precondition: Elementor\'s save made revisions of the draft');

        $r = $this->revert(self::REQ_GOLDEN);
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame('reverted', $r['outcome']);
        $this->assertSame($id, $r['post_id']);
        $this->assertSame('trash', $this->posts[$id]->post_status);
        $this->assertSame('trashed', AbilityLedger::get(self::REQ_GOLDEN)['undo_state']);

        $again = $this->revert(self::REQ_GOLDEN);
        $this->assertSame('already_reverted', $again['outcome'] ?? null);
    }

    public function test_undo_refused_after_human_edit(): void
    {
        $id = $this->createDraft();

        // A person saved the page in the editor: a revision this request
        // did not make.
        $this->revisions[$id][] = (object) ['ID' => 777];
        $r                      = $this->revert(self::REQ_GOLDEN);
        $this->assertSame('created_post_touched', $r['code'] ?? null, (string) json_encode($r));
        $this->assertSame('draft', $this->posts[$id]->post_status, 'not trashed');
        array_pop($this->revisions[$id]);

        // A person changed the page's rows without a revision.
        $this->posts[$id]->post_title = 'Changed by a person';
        $this->syncRow($id);
        $r = $this->revert(self::REQ_GOLDEN);
        $this->assertSame('created_post_touched', $r['code'] ?? null, (string) json_encode($r));
        $this->assertSame('draft', $this->posts[$id]->post_status, 'not trashed');

        // An autosave or an open editor refuses too.
        $this->posts[$id]->post_title = self::TITLE;
        $this->syncRow($id);
        $this->autosaves[$id] = true;
        $this->assertSame('created_post_touched', $this->revert(self::REQ_GOLDEN)['code'] ?? null);
        unset($this->autosaves[$id]);
        $this->locks[$id] = 9;
        $this->assertSame('created_post_touched', $this->revert(self::REQ_GOLDEN)['code'] ?? null);
        unset($this->locks[$id]);

        $this->assertSame('available',AbilityLedger::get(self::REQ_GOLDEN)['undo_state']);
        $this->assertSame('reverted', $this->revert(self::REQ_GOLDEN)['outcome'] ?? null, 'unchanged again, the draft can be undone');
    }

    public function test_elementor_format_rejected_for_block_editor(): void
    {
        $this->enable();
        foreach ([PageCreateBuilder::EDITOR_BLOCKS, PageCreateBuilder::EDITOR_CLASSIC] as $editor) {
            foreach (['site_default', 'classic', 'atomic'] as $format) {
                $r = $this->precheck(self::REQ_GOLDEN, $this->input('columns', ['editor' => $editor, 'elementor_format' => $format]));
                $this->assertSame('bad_input', $r['code'] ?? null, $editor . ' with ' . $format . ': ' . json_encode($r));
            }
        }
        foreach (['fancy', '', 'Classic', null, 1] as $format) {
            $r = $this->precheck(self::REQ_GOLDEN, $this->input('columns', ['elementor_format' => $format]));
            $this->assertSame('bad_input', $r['code'] ?? null, json_encode($format));
        }

        $atomic = $this->precheck(self::REQ_GOLDEN, $this->input('columns', ['elementor_format' => 'atomic']));
        $this->assertSame(['builder_not_available', 'atomic_unavailable'], [$atomic['code'] ?? null, $atomic['detail'] ?? null]);
        foreach (['site_default', 'classic'] as $format) {
            $r = $this->precheck(self::REQ_GOLDEN, $this->input('columns', ['elementor_format' => $format]));
            $this->assertSame('classic', $r['preview']['format'] ?? null, $format . ': ' . json_encode($r));
        }

        // The block editor's own answer keeps its bytes: no elementor_format.
        $spec = PageCreateBuilder::validate((object) ['post_type' => 'page', 'editor' => PageCreateBuilder::EDITOR_BLOCKS, 'title' => 'T', 'outline' => [(object) ['type' => 'paragraph', 'text' => 'x']]]);
        $this->assertSame(['post_type', 'editor', 'title', 'outline'], array_keys($spec['spec'] ?? []));
        $this->assertSame([], $this->inserted);
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Create the golden columns page as REQ_GOLDEN and answer its post id.
     */
    private function createDraft(): int
    {
        $this->enable();
        $pre = $this->precheck(self::REQ_GOLDEN, $this->input('columns'));
        $r   = $this->write(self::REQ_GOLDEN, $this->input('columns'), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('created', $r['outcome'] ?? null, (string) json_encode($r));

        return (int) $r['post_id'];
    }

    /**
     * Core's insert: the post object, its raw row, its meta rows, and a
     * stand-in Elementor document whose save() stores the tree as
     * Elementor's does on a draft.
     *
     * @param array<string,mixed> $data wp_insert_post() arguments.
     */
    private function insertPost(array $data): int
    {
        $id = $this->nextId++;
        $this->posts[$id] = (object) [
            'ID'                => $id,
            'post_type'         => (string) $data['post_type'],
            'post_status'       => (string) $data['post_status'],
            'post_author'       => (int) $data['post_author'],
            'post_title'        => stripslashes((string) $data['post_title']),
            'post_content'      => stripslashes((string) $data['post_content']),
            'post_parent'       => 0,
            'post_date'         => '2026-10-09 10:00:00',
            'post_date_gmt'     => '0000-00-00 00:00:00',
            'post_modified'     => '2026-10-09 10:00:00',
            'post_modified_gmt' => '0000-00-00 00:00:00',
        ];
        $this->syncRow($id);
        foreach ((array) ($data['meta_input'] ?? []) as $key => $value) {
            $this->meta[$id][(string) $key] = stripslashes((string) $value);
            $this->rows->addMeta(++$this->metaId, $id, (string) $key, stripslashes((string) $value));
        }
        $this->inserted[] = $id;

        $document         = $this->api->addDocument($id, 'wp-page');
        $document->onSave = function ($data) use ($id): bool {
            $tree = $data['elements'];
            if ($this->alterOnSave !== null) {
                $tree = ($this->alterOnSave)($tree);
            }
            $json     = json_encode($tree, JSON_THROW_ON_ERROR);
            $revision = $this->nextId++;
            $this->posts[$id]->post_modified_gmt = '2026-10-09 07:00:05';
            $this->syncRow($id);
            $this->fire('wp_insert_post', $id, (object) ['ID' => $id, 'post_type' => 'page', 'post_parent' => 0], true);
            $this->fire('wp_insert_post', $revision, (object) ['ID' => $revision, 'post_type' => 'revision', 'post_parent' => $id], false);
            $this->revisions[$id][] = (object) ['ID' => $revision];
            foreach ([[$id, ElementorDocument::KEY_DATA, $json], [$id, '_elementor_version', '3.35.9'], [$revision, ElementorDocument::KEY_DATA, $json]] as [$on, $key, $value]) {
                $this->rows->addMeta(++$this->metaId, $on, $key, $value);
                $this->fire('added_post_meta', $this->metaId, $on, $key, $value);
            }

            return true;
        };

        return $id;
    }

    /** Copy a post object's columns into its raw row. */
    private function syncRow(int $id): void
    {
        $p = $this->posts[$id];
        $this->rows->addPost($id, [
            'post_type'         => (string) $p->post_type,
            'post_status'       => (string) $p->post_status,
            'post_title'        => (string) $p->post_title,
            'post_content'      => (string) $p->post_content,
            'post_modified_gmt' => (string) $p->post_modified_gmt,
        ]);
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
     * A golden case of $file by name.
     *
     * @return array{input: list<mixed>, tree: list<mixed>}
     */
    private static function golden(string $file, string $name): array
    {
        $fixture = json_decode((string) file_get_contents($file), true, 512, JSON_THROW_ON_ERROR);
        self::assertSame(self::REQ_GOLDEN, $fixture['request_id']);
        foreach ($fixture['cases'] as $case) {
            if ($case['name'] === $name) {
                return ['input' => $case['input'], 'tree' => $case['tree']];
            }
        }
        throw new \LogicException('golden case missing: ' . $name);
    }

    /**
     * The page-create input text for a golden case's outline.
     *
     * @param array<string,mixed> $overrides Top-level fields to set; null removes nothing, it is sent as null.
     */
    private function input(string $case, array $overrides = []): string
    {
        return (string) json_encode(array_merge([
            'post_type' => 'page',
            'editor'    => PageCreateBuilder::EDITOR_BUILDER_ELEMENTOR,
            'title'     => self::TITLE,
            'outline'   => self::golden(self::CONTAINERS, $case)['input'],
        ], $overrides), JSON_UNESCAPED_UNICODE);
    }

    /**
     * Go's page-create entry, with limits.builders_enabled set to $enabled
     * (left out when null).
     *
     * @param mixed $enabled The builders_enabled value.
     */
    private function entry(mixed $enabled = ['elementor']): string
    {
        $raw  = file_get_contents(__DIR__ . '/../fixtures/ability-run/page-create.json');
        $base = json_decode((string) json_decode((string) $raw, true)['entry'], true);
        self::assertIsArray($base);
        $limits = (array) $base['limits'];
        if ($enabled !== null) {
            $limits['builders_enabled'] = $enabled;
        }
        $base['limits'] = $limits;

        return (string) json_encode($base);
    }

    /**
     * @return array<string,mixed>
     */
    private function enable(): array
    {
        return $this->post('content_editing_enable', '{}', null);
    }

    /**
     * @param mixed $enabled The entry's builders_enabled.
     * @return array<string,mixed>
     */
    private function precheck(string $rid, string $input, mixed $enabled = ['elementor']): array
    {
        return $this->callP($this->p('precheck', $rid, $input, null, $enabled));
    }

    /**
     * @param mixed $enabled The entry's builders_enabled.
     * @return array<string,mixed>
     */
    private function write(string $rid, string $input, string $pre, string $prev, mixed $enabled = ['elementor']): array
    {
        return $this->callP($this->p('write', $rid, $input, ['precheck_digest' => $pre, 'preview_digest' => $prev], $enabled));
    }

    /**
     * @return array<string,mixed>
     */
    private function revert(string $rid): array
    {
        $entry = $this->entry();

        return $this->callP((string) json_encode(['mode' => 'revert', 'request_id' => $rid, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry)]));
    }

    /**
     * @param array<string,string>|null $expected
     * @param mixed                     $enabled
     */
    private function p(string $mode, string $rid, string $input, ?array $expected, mixed $enabled): string
    {
        $entry = $this->entry($enabled);
        $p     = ['mode' => $mode, 'request_id' => $rid, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input];
        if ($expected !== null) {
            $p['expected'] = $expected;
        }

        return (string) json_encode($p);
    }

    /**
     * @return array<string,mixed>
     */
    private function callP(string $p): array
    {
        return $this->post('ability_run', (string) json_encode(['p' => $p]), hash('sha256', $p));
    }

    /**
     * @return array<string,mixed>
     */
    private function post(string $cmd, string $body, ?string $pd): array
    {
        $request = new \WP_REST_Request('POST', '/wpmgr/v1/command/' . $cmd);
        $request->set_url_params(['command' => $cmd]);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Accept', 'application/json');
        $request->set_header('Authorization', 'Bearer ' . $this->mintToken($cmd, $pd));
        $request->set_body($body);

        $this->assertTrue($this->router->authorizeCommand($request, $cmd), 'the signed request was not authorized');
        $response = $this->router->handleCommand($request);
        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertIsArray($response->data);

        return $response->data;
    }

    private function mintToken(string $cmd, ?string $pd): string
    {
        $claims = ['aud' => $this->siteId, 'cmd' => $cmd, 'jti' => bin2hex(random_bytes(8)), 'exp' => time() + 30];
        if ($pd !== null) {
            $claims['pd'] = $pd;
        }
        $segments   = [
            self::b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])),
            self::b64((string) json_encode($claims)),
        ];
        $segments[] = self::b64(sodium_crypto_sign_detached(implode('.', $segments), $this->cpSecret));

        return implode('.', $segments);
    }

    private static function b64(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    private function resetShieldStash(): void
    {
        $prop = new ReflectionProperty(AuthHeaderShield::class, 'stashedBearer');
        $prop->setValue(null, null);
    }

    /**
     * One wpdb for the ledger's option rows and the builder's post and
     * meta rows: the post and meta reads go to FakeBuilderWpdb, the option
     * reads and writes to this test's options.
     */
    private function wpdb(FakeBuilderWpdb $rows): object
    {
        $test = $this;

        return new class ($rows, $test) {
            public string $prefix     = 'wp_';
            public string $options    = 'wp_options';
            public string $posts      = 'wp_posts';
            public string $postmeta   = 'wp_postmeta';
            public string $last_error = '';

            public function __construct(private FakeBuilderWpdb $rows, private ElementorPageCreateRouterTest $t)
            {
            }

            public function prepare(string $q, ...$args): string
            {
                return $this->rows->prepare($q, ...$args);
            }

            public function get_row(string $q, string $output = 'OBJECT', int $y = 0)
            {
                return $this->rows->get_row($q, $output, $y);
            }

            public function get_results(string $q, string $output = 'OBJECT'): ?array
            {
                return $this->rows->get_results($q, $output);
            }

            public function get_var(string $q): ?string
            {
                [$sql, $args] = self::parse($q);

                return str_starts_with($sql, 'SELECT option_value FROM wp_options') ? $this->t->rawOption((string) $args[0]) : null;
            }

            public function query(string $q): int
            {
                [$sql, $args] = self::parse($q);
                if (str_starts_with($sql, 'INSERT IGNORE INTO wp_options')) {
                    return $this->t->optionInsert((string) $args[0], (string) $args[1]);
                }
                if (str_starts_with($sql, 'UPDATE wp_options SET option_value = %s WHERE option_name = %s AND option_value = %s')) {
                    return $this->t->optionSwap((string) $args[1], (string) $args[2], (string) $args[0]);
                }
                if (str_starts_with($sql, 'DELETE FROM wp_options WHERE option_name = %s AND option_value = %s')) {
                    return $this->t->optionSwap((string) $args[0], (string) $args[1], null);
                }
                // The token replay table's prune, as PageCreateWriteTest has it.
                return 0;
            }

            /**
             * The token replay record, as PageCreateWriteTest has it.
             *
             * @param array<string,mixed> $row Row.
             */
            public function insert(string $table, array $row, $format = null): int
            {
                return 1;
            }

            /** @return array{0:string,1:list<mixed>} */
            private static function parse(string $q): array
            {
                $d = json_decode($q, true, 512, JSON_THROW_ON_ERROR);

                return [(string) preg_replace('/\s+/', ' ', trim((string) $d['sql'])), array_values((array) $d['args'])];
            }
        };
    }

    /** Fake-wpdb hook: the raw value of an options row, or null. */
    public function rawOption(string $name): ?string
    {
        return array_key_exists($name, $this->options) ? (string) $this->options[$name] : null;
    }

    /** Fake-wpdb hook: INSERT IGNORE on the unique option_name. */
    public function optionInsert(string $name, string $value): int
    {
        if (array_key_exists($name, $this->options)) {
            return 0;
        }
        $this->options[$name] = $value;

        return 1;
    }

    /** Fake-wpdb hook: replace (or delete, when $new is null) a row holding $old. */
    public function optionSwap(string $name, string $old, ?string $new): int
    {
        if (!array_key_exists($name, $this->options) || (string) $this->options[$name] !== $old) {
            return 0;
        }
        if ($new === null) {
            unset($this->options[$name]);
        } else {
            $this->options[$name] = $new;
        }

        return 1;
    }
}
