<?php
/**
 * The round trip of a designed page, driven through the real signed Router:
 * page-create writes the landing page of the shared version 3 table, a
 * page-structure read gives the pricing pattern's paths, page-edit sets the
 * text of plans.1.price, a second read shows only that text changed, and an
 * Undo puts the page, its page-template row and its page-settings row back
 * byte for byte.
 *
 * Elementor is FakeElementorApi behind the real ElementorAdapter, handed to
 * the command through its test-only adapter seam. The stand-in document saves
 * the elements as Elementor's save does: the post row's modified time moves,
 * the element data row is replaced, and the hooks of a post and meta write
 * fire. Post, meta and snapshot rows live in FakeBuilderWpdb behind
 * EngineWpdb; the ledger rows are this test's options.
 *
 * The side-effect check runs on every write of the trip: the writes the page
 * owns (its save, its regenerated stylesheet, its style-cache entry) pass,
 * and any other write during the edit or during the Undo trips it.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\ElementorStyleCache;
use WPMgr\Agent\Abilities\OwnAbilities;
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
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageCreate
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageStructure
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageEdit
 */
final class PageCreateEditRoundTripTest extends TestCase
{
    /** The accept/refuse table of an outline version 3 input. */
    private const CASES = __DIR__ . '/../fixtures/ability-run/page-create-v3-cases.json';

    /** The row of the table that holds every pattern once. */
    private const ROW = 'v3-full-landing-page';

    private const CREATE = '11111111-2222-4333-8444-777777777777';

    private const EDIT = '44444444-2222-4333-8444-666666666666';

    private const READ = '33333333-2222-4333-8444-666666666666';

    private const KIT = 4;

    /** Another post of the site, whose pages and style-cache entry the trip must not touch. */
    private const OTHER = 77;

    private const TEMPLATE = '_wp_page_template';

    /** The path of the second plan's price, and the price before and after. */
    private const PATH = 'plans.1.price';

    private const PRICE = '49';

    private const NEW_PRICE = '59';

    /** The keys a restore drops and Elementor regenerates, never puts back. */
    private const DERIVED = ['_elementor_css', '_elementor_element_cache', '_elementor_page_assets'];

    private string $keyFile;

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;

    private FakeElementorApi $api;

    private FakeBuilderWpdb $rows;

    private EngineWpdb $wpdb;

    /** @var array<string,mixed> */
    private array $options = [];

    /**
     * @var array<string,mixed> This request's options cache, as core keeps it
     *      without a persistent object cache. Emptied when a request starts.
     */
    private array $optionCache = [];

    /** @var array<string,true> Names this request found missing, as core's notoptions. */
    private array $notOptions = [];

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

    /** @var array<int,array{mime:string,image:bool}> Media library attachments by id. */
    private array $attachments = [];

    /** @var string|false */
    private $locale = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-roundtrip-' . bin2hex(random_bytes(8)) . '.key';
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
        $snap = static fn ($name): bool => str_starts_with((string) $name, BuilderDocumentSnapshot::OPTION_PREFIX);
        Functions\when('add_filter')->alias($capture);
        Functions\when('add_action')->alias($capture);
        Functions\when('remove_filter')->alias($release);
        Functions\when('remove_action')->alias($release);

        // The options API as core runs it against this request's cache; the
        // snapshots are rows of the options table, as production stores them.
        Functions\when('get_option')->alias(function ($name, $default = false) {
            $name = (string) $name;
            if (array_key_exists($name, $this->optionCache)) {
                return $this->optionCache[$name];
            }
            if (isset($this->notOptions[$name])) {
                return $default;
            }
            if (!array_key_exists($name, $this->options)) {
                $this->notOptions[$name] = true;

                return $default;
            }

            return $this->optionCache[$name] = $this->options[$name];
        });
        Functions\when('update_option')->alias(function ($name, $value) {
            $name = (string) $name;
            if ($value === \get_option($name)) {
                return false;
            }
            $this->options[$name]     = $value;
            $this->optionCache[$name] = $value;
            unset($this->notOptions[$name]);

            return true;
        });
        Functions\when('add_option')->alias(function ($name, $value = '', $unused = '', $autoload = null) use ($snap): bool {
            if ($snap($name)) {
                return $this->rows->addOptionLikeCore((string) $name, $value, $unused, $autoload);
            }
            $name = (string) $name;
            if (!isset($this->notOptions[$name]) && \get_option($name) !== false) {
                return false;
            }
            $this->options[$name]     = $value;
            $this->optionCache[$name] = $value;
            unset($this->notOptions[$name]);

            return true;
        });
        Functions\when('delete_option')->alias(function ($name) use ($snap): bool {
            if ($snap($name)) {
                return $this->rows->deleteOptionLikeCore((string) $name);
            }
            $name = (string) $name;
            $had  = array_key_exists($name, $this->options);
            unset($this->options[$name], $this->optionCache[$name]);
            if ($had) {
                $this->notOptions[$name] = true;
            }

            return $had;
        });
        Functions\when('wp_cache_delete')->alias(function ($key, $group = '') {
            if ($group === 'options') {
                unset($this->optionCache[(string) $key]);
            }

            return true;
        });
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('register_rest_route')->justReturn(true);
        Functions\when('wc_get_page_id')->justReturn(-1);
        $GLOBALS['wp_version'] = '6.4.2';

        // Roles and users, as the other router tests have them.
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
        Functions\when('wp_get_post_autosave')->justReturn(false);
        Functions\when('wp_get_post_revisions')->alias(function ($id, $args = null) {
            $out = [];
            foreach ($this->revisions[(int) $id] ?? [] as $revision) {
                $out[$revision->ID] = $revision;
            }

            return $out;
        });
        Functions\when('wp_check_post_lock')->justReturn(false);

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $this->rows      = new FakeBuilderWpdb();
        $this->wpdb      = new EngineWpdb($this->rows);
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->rows->addPost(self::KIT, [
            'post_type'         => 'elementor_library',
            'post_status'       => 'publish',
            'post_title'        => 'Kit',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-01 07:00:00',
        ]);
        $this->rows->addMeta(2, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->rows->addMeta(3, self::KIT, ElementorDocument::KEY_DATA, '[]');

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

    // -------------------------------------------------------------------------
    // The round trip
    // -------------------------------------------------------------------------

    public function testCreateReadSetTextReadUndoPutsTheWholePageBack(): void
    {
        $id      = $this->createLanding();
        $created = $this->pageBytes($id);

        // The page opens with a hero on a full-width frame: the template and the
        // hidden title are rows of their own, and the Undo has them to put back.
        $this->assertSame(
            [ElementorDocument::KEY_PAGE_SETTINGS => [ElementorDocument::pageSettingsBytes(['hide_title' => 'yes'])], self::TEMPLATE => [ElementorDocument::pageTemplate('full_width')]],
            $this->frameRows($id),
            'precondition: the created page holds one page-settings row and one template row'
        );

        // Read: the pricing pattern gives its paths and the site's text.
        $first   = $this->structure($id);
        $pricing = $this->patternOf($first, 'pricing');
        $this->assertSame('three-plans', $pricing['variant']);
        $this->assertContains(self::PATH, $pricing['editable']);
        $this->assertSame(self::PRICE, $pricing['from_the_site'][self::PATH] ?? null);
        $this->assertSame($this->fp($id), $first['base_fingerprint']);

        // Edit: the second plan's price, and nothing else.
        $applied = $this->edit($id, $first['base_fingerprint'], $pricing['ref']);
        $this->assertSame([true, 'applied', 1], [$applied['ok'] ?? null, $applied['outcome'] ?? null, $applied['changes_applied'] ?? null], (string) json_encode($applied));
        $edited = $this->pageBytes($id);
        $this->assertNotSame($created['meta'][ElementorDocument::KEY_DATA], $edited['meta'][ElementorDocument::KEY_DATA], 'the edit wrote the element data');
        $this->assertSame(self::keysWithoutData($created['meta']), self::keysWithoutData($edited['meta']), 'every other row of the page is as the edit found it');
        $this->assertSame($this->frameRows($id), [ElementorDocument::KEY_PAGE_SETTINGS => [ElementorDocument::pageSettingsBytes(['hide_title' => 'yes'])], self::TEMPLATE => [ElementorDocument::pageTemplate('full_width')]], 'the edit left the frame alone');
        $this->assertSame(
            [self::NEW_PRICE],
            self::leafChanges(json_decode((string) $created['meta'][ElementorDocument::KEY_DATA][0], true), json_decode((string) $edited['meta'][ElementorDocument::KEY_DATA][0], true)),
            'one text of the stored tree differs: the price, from ' . self::PRICE . ' to ' . self::NEW_PRICE
        );

        // Read again: the same page, with that one price.
        $second = $this->structure($id);
        $this->assertSame($this->fp($id), $second['base_fingerprint']);
        $this->assertNotSame($first['base_fingerprint'], $second['base_fingerprint']);
        $this->assertSame(
            self::withPrice($first['nodes'], self::NEW_PRICE),
            $second['nodes'],
            'every node and path is as before, and the second plan\'s price is the new one'
        );
        $this->assertSame(self::NEW_PRICE, $this->patternOf($second, 'pricing')['from_the_site'][self::PATH] ?? null);

        // Undo: the page, its template row and its page-settings row, byte for byte.
        $rowsWithIds = $this->frameRowsWithIds($id);
        $undone      = $this->undo($applied['snapshot_sha256']);
        $this->assertSame(['ok' => true, 'outcome' => 'reverted', 'mode' => 'revert', 'request_id' => self::EDIT, 'post_id' => $id, 'restored' => true], $undone);
        $this->assertSame($created, $this->pageBytes($id), 'the page is as the create left it, every row and column');
        $this->assertSame($this->frameRows($id), [ElementorDocument::KEY_PAGE_SETTINGS => [ElementorDocument::pageSettingsBytes(['hide_title' => 'yes'])], self::TEMPLATE => [ElementorDocument::pageTemplate('full_width')]], 'the page-settings row and the template row');
        $this->assertSame($rowsWithIds, $this->frameRowsWithIds($id), 'neither frame row was deleted or rewritten: the same meta ids');
        $this->assertSame($first['base_fingerprint'], $this->fp($id));
        $this->assertSame($first, $this->structure($id), 'the structure reads as it did before the edit');
        $this->assertSame('restored', $this->options['wpmgr_ability_ledger_' . self::EDIT]['undo_state'] ?? null);
        $this->assertSame([], $this->wpdb->claims, 'the claims are released');
        $this->assertSame([], $this->hooks, 'the write scope is disarmed');
    }

    // -------------------------------------------------------------------------
    // The create step
    // -------------------------------------------------------------------------

    /**
     * Create the landing page of the shared table as CREATE, precheck then
     * write, and answer its post id.
     */
    private function createLanding(): int
    {
        $this->enable();
        $this->site();
        $input = $this->landing();
        $pre   = $this->callP($this->pCreate('precheck', $input, null));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $r = $this->callP($this->pCreate('write', $input, ['precheck_digest' => $pre['precheck_digest'], 'preview_digest' => $pre['preview_digest']]));
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame('created', $r['outcome'] ?? null);
        $this->assertSame(['tree_equal' => true, 'status' => 'draft'], $r['verify'] ?? null);

        return (int) $r['post_id'];
    }

    /** The exact input text of the landing page row. */
    private function landing(): string
    {
        $doc = json_decode((string) file_get_contents(self::CASES), true, 512, JSON_THROW_ON_ERROR);
        foreach ($doc['cases'] as $case) {
            if ($case['name'] === self::ROW) {
                return (string) $case['input'];
            }
        }
        throw new \LogicException('no such row: ' . self::ROW);
    }

    /**
     * The site a designed page is built on: the images the landing page
     * names, the widgets its patterns use, and a theme that offers Elementor's
     * page templates.
     */
    private function site(): void
    {
        // Posts created from here on take ids clear of the attachments' (up to 203).
        $this->nextId           = 5000;
        $this->api->widgetTypes = ['heading', 'text-editor', 'button', 'image', 'divider', 'spacer', 'icon-box', 'counter', 'testimonial', 'accordion', 'icon-list', 'image-box', 'tabs'];
        $theme                  = new class () {
            /** @return array<string,string> */
            public function get_page_templates($post = null, $post_type = 'page'): array
            {
                return ['elementor_header_footer' => 'Elementor Full Width', 'elementor_canvas' => 'Elementor Canvas'];
            }
        };
        Functions\when('wp_get_theme')->justReturn($theme);
        Functions\when('wp_attachment_is_image')->alias(fn ($id) => (bool) ($this->attachments[(int) $id]['image'] ?? false));
        Functions\when('get_post_mime_type')->alias(fn ($id) => $this->attachments[(int) $id]['mime'] ?? false);
        Functions\when('get_post_status')->alias(fn ($id) => isset($this->attachments[(int) $id]) ? 'publish' : false);
        Functions\when('wp_get_attachment_image_src')->alias(fn ($id, $size = 'thumbnail') => [sprintf('https://example.com/wp-content/uploads/2026/10/img-%d.jpg', (int) $id), 1024, 576]);
        Functions\when('get_attached_file')->alias(fn ($id) => sprintf('/srv/wp-content/uploads/2026/10/img-%d.jpg', (int) $id));
        Functions\when('wp_get_attachment_metadata')->justReturn(['width' => 1600, 'height' => 900]);
        Functions\when('wp_basename')->alias(static fn ($path, $suffix = '') => basename((string) $path, (string) $suffix));

        $logos = [101 => 'Acme logo', 102 => 'Birchwood logo', 103 => 'Cobalt logo', 104 => 'Dunmore logo', 105 => 'Everly logo', 106 => 'Fernhill logo'];
        foreach ([42, 101, 102, 103, 104, 105, 106, 201, 202, 203] as $id) {
            $this->attachments[$id] = ['mime' => 'image/jpeg', 'image' => true];
            $this->posts[$id]       = (object) [
                'ID'                => $id,
                'post_type'         => 'attachment',
                'post_status'       => 'inherit',
                'post_password'     => '',
                'post_parent'       => 0,
                'post_modified_gmt' => '2026-10-01 09:00:00',
            ];
            $this->meta[$id]['_wp_attachment_image_alt'] = $logos[$id] ?? '';
        }
    }

    /**
     * Core's insert: the post object, its raw row, its meta rows, and a
     * stand-in Elementor document for the new draft.
     *
     * @param array<string,mixed> $data wp_insert_post() arguments.
     */
    private function insertPost(array $data): int
    {
        $id               = $this->nextId++;
        $this->posts[$id] = (object) [
            'ID'                => $id,
            'post_type'         => (string) $data['post_type'],
            'post_status'       => (string) $data['post_status'],
            'post_author'       => (int) $data['post_author'],
            'post_title'        => stripslashes((string) $data['post_title']),
            'post_content'      => stripslashes((string) $data['post_content']),
            'post_excerpt'      => '',
            'post_name'         => '',
            'post_password'     => '',
            'post_parent'       => 0,
            'menu_order'        => 0,
            'post_date'         => '2026-10-09 10:00:00',
            'post_date_gmt'     => '0000-00-00 00:00:00',
            'post_modified'     => '2026-10-09 10:00:00',
            'post_modified_gmt' => '0000-00-00 00:00:00',
        ];
        $this->syncRow($id);
        $unslash = static function ($value) use (&$unslash) {
            if (is_array($value)) {
                return array_map($unslash, $value);
            }

            return is_string($value) ? stripslashes($value) : $value;
        };
        foreach ((array) ($data['meta_input'] ?? []) as $key => $value) {
            // As core stores a meta value: an array is serialized, a string kept.
            $value                          = $unslash($value);
            $this->meta[$id][(string) $key] = is_array($value) ? $value : (string) $value;
            $this->rows->addMeta(++$this->metaId, $id, (string) $key, is_array($value) ? ElementorDocument::pageSettingsBytes($value) : (string) $value);
        }
        $document         = $this->api->addDocument($id, 'wp-page');
        $document->onSave = fn ($data): bool => $this->elementorSave($id, $data);

        return $id;
    }

    /**
     * What Elementor's save of a draft does with the elements it is given:
     * the post row's modified time moves, the element data and version rows
     * are written (replaced when the page has them), and the first save also
     * makes its revision.
     *
     * @param int   $id   The draft.
     * @param mixed $data The save data.
     */
    private function elementorSave(int $id, $data): bool
    {
        $json   = json_encode($data['elements'], JSON_THROW_ON_ERROR);
        $update = $this->storedValues($id, ElementorDocument::KEY_DATA) !== [];
        $this->posts[$id]->post_modified_gmt = $update ? '2026-10-09 08:15:00' : '2026-10-09 07:00:05';
        $this->syncRow($id);
        $this->fire('wp_insert_post', $id, (object) ['ID' => $id, 'post_type' => 'page', 'post_parent' => 0], true);
        if (!$update) {
            $revision = $this->nextId++;
            $this->fire('wp_insert_post', $revision, (object) ['ID' => $revision, 'post_type' => 'revision', 'post_parent' => $id], false);
            $this->revisions[$id][] = (object) ['ID' => $revision];
            $this->metaWrite($revision, ElementorDocument::KEY_DATA, $json);
        }
        $this->metaWrite($id, ElementorDocument::KEY_DATA, $json);
        $this->metaWrite($id, '_elementor_version', '3.35.9');

        return true;
    }

    /**
     * update_post_meta() as core runs it: the key's rows replaced by one, and
     * added_post_meta or updated_post_meta fired.
     */
    private function metaWrite(int $postId, string $key, string $value): void
    {
        $had = $this->storedValues($postId, $key) !== [];
        $this->wpdb->delete($this->wpdb->postmeta, ['post_id' => $postId, 'meta_key' => $key]);
        $this->rows->addMeta(++$this->metaId, $postId, $key, $value);
        $this->fire($had ? 'updated_post_meta' : 'added_post_meta', $this->metaId, $postId, $key, $value);
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
            'post_excerpt'      => (string) $p->post_excerpt,
            'post_name'         => (string) $p->post_name,
            'post_password'     => (string) $p->post_password,
            'post_modified_gmt' => (string) $p->post_modified_gmt,
            'post_parent'       => (string) $p->post_parent,
            'menu_order'        => (string) $p->menu_order,
        ]);
    }

    // -------------------------------------------------------------------------
    // The read and edit steps
    // -------------------------------------------------------------------------

    /**
     * The page-structure answer of a draft the signed list names.
     *
     * @return array<string,mixed> The answer's output.
     */
    private function structure(int $id): array
    {
        $entry = (string) json_encode([
            'name'          => OwnAbilities::NAME_PAGE_STRUCTURE,
            'source'        => 'wpmgr',
            'class'         => 'read',
            'status'        => 'admitted',
            'enabled'       => true,
            'approval_mode' => 'none',
            'snapshot'      => 'none',
            'limits'        => ['builders_enabled' => ['elementor']],
        ]);
        $p = (string) json_encode([
            'mode'              => 'read',
            'request_id'        => self::READ,
            'entry'             => $entry,
            'entry_sha256'      => hash('sha256', $entry),
            'input'             => (string) json_encode(['post_id' => $id]),
            'allowed_draft_ids' => [$id],
        ]);
        $r = $this->callP($p);
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));

        // As the control plane receives it: the JSON of the answer.
        $out = json_decode((string) json_encode($r['output']), true, 512, JSON_THROW_ON_ERROR);
        $this->assertIsArray($out);

        return $out;
    }

    /**
     * The one pattern of a structure answer.
     *
     * @param array<string,mixed> $structure A structure answer's output.
     * @return array<string,mixed>
     */
    private function patternOf(array $structure, string $pattern): array
    {
        foreach ($structure['nodes'] as $node) {
            if (($node['kind'] ?? null) === 'pattern' && str_contains((string) ($node['variant'] ?? ''), 'plans') === ($pattern === 'pricing')) {
                if ($pattern !== 'pricing' || isset($node['from_the_site'][self::PATH])) {
                    return $node;
                }
            }
        }
        throw new \LogicException('no ' . $pattern . ' pattern in the structure');
    }

    /**
     * The page-edit input that sets the second plan's price.
     *
     * @return array<string,mixed>
     */
    private function editInput(int $id, string $baseFp, string $ref): array
    {
        return ['post_id' => $id, 'base_fingerprint' => $baseFp, 'operations' => [['op' => 'set_text', 'ref' => $ref, 'field' => self::PATH, 'text' => self::NEW_PRICE]]];
    }

    /**
     * Precheck, then write with the approved digests: the write's answer.
     *
     * @return array<string,mixed>
     */
    private function edit(int $id, string $baseFp, string $ref): array
    {
        $input = (string) json_encode($this->editInput($id, $baseFp, $ref));
        $pre   = $this->callP($this->pEdit('precheck', $id, $input, null));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));

        return $this->callP($this->pEdit('write', $id, $input, $pre));
    }

    /**
     * A person's undo of the edit, as the control plane sends it.
     *
     * @return array<string,mixed>
     */
    private function undo(string $hash): array
    {
        $entry = $this->editEntry();
        $p     = (string) json_encode(['mode' => 'revert', 'request_id' => self::EDIT, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => '{}']);

        return $this->callP(substr($p, 0, -1) . ',"revert":' . json_encode(['snapshot_sha256' => $hash]) . '}');
    }

    // -------------------------------------------------------------------------
    // The signed parameters
    // -------------------------------------------------------------------------

    /** The page-edit entry as the control plane sends it. */
    private function editEntry(): string
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

    /**
     * p for a precheck or a write of the edit on $id.
     *
     * @param array<string,mixed>|null $expected The precheck answer, for a write.
     */
    private function pEdit(string $mode, int $id, string $input, ?array $expected): string
    {
        $entry = $this->editEntry();
        $p     = ['mode' => $mode, 'request_id' => self::EDIT, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input, 'allowed_draft_ids' => [$id]];
        if ($expected !== null) {
            $p['expected'] = ['precheck_digest' => $expected['precheck_digest'], 'preview_digest' => $expected['preview_digest']];
        }

        return (string) json_encode($p);
    }

    /**
     * p for the page-create precheck or write: Go's page-create entry with
     * Elementor enabled.
     *
     * @param array<string,string>|null $expected The digests, for a write.
     */
    private function pCreate(string $mode, string $input, ?array $expected): string
    {
        $raw  = file_get_contents(__DIR__ . '/../fixtures/ability-run/page-create.json');
        $base = json_decode((string) json_decode((string) $raw, true)['entry'], true);
        self::assertIsArray($base);
        $limits                     = (array) $base['limits'];
        $limits['builders_enabled'] = ['elementor'];
        $base['limits']             = $limits;
        $entry                      = (string) json_encode($base);
        $p                          = ['mode' => $mode, 'request_id' => self::CREATE, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input];
        if ($expected !== null) {
            $p['expected'] = $expected;
        }

        return (string) json_encode($p);
    }

    /** @return array<string,mixed> */
    private function enable(): array
    {
        return $this->post('content_editing_enable', '{}', null);
    }

    /** @return array<string,mixed> */
    private function callP(string $p): array
    {
        return $this->post('ability_run', (string) json_encode(['p' => $p]), hash('sha256', $p));
    }

    /** @return array<string,mixed> */
    private function post(string $cmd, string $body, ?string $pd): array
    {
        // A new request: nothing of the options is cached yet.
        $this->optionCache = [];
        $this->notOptions  = [];
        $request           = new \WP_REST_Request('POST', '/wpmgr/v1/command/' . $cmd);
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

    // -------------------------------------------------------------------------
    // What the page holds
    // -------------------------------------------------------------------------

    /** The page's builder_document_v1 fingerprint now. */
    private function fp(int $id): string
    {
        return (string) BuilderDocumentFingerprint::ofPost($id, ElementorDocument::DESCRIPTOR_KEYS);
    }

    /**
     * A post's posts row as stored, and its meta values by key, keys sorted,
     * each key's rows in meta_id order.
     *
     * @return array{post: array<string,string|int|null>|null, meta: array<string,list<string|null>>}
     */
    private function pageBytes(int $id): array
    {
        $meta = [];
        foreach ($this->rows->metaRowsOf($id) as $row) {
            $meta[$row['meta_key']][] = $row['meta_value'];
        }
        ksort($meta, SORT_STRING);

        return ['post' => $this->rows->postRow($id), 'meta' => $meta];
    }

    /**
     * The stored rows of the page settings and the page template.
     *
     * @return array<string,list<string|null>>
     */
    private function frameRows(int $id): array
    {
        return [ElementorDocument::KEY_PAGE_SETTINGS => $this->storedValues($id, ElementorDocument::KEY_PAGE_SETTINGS), self::TEMPLATE => $this->storedValues($id, self::TEMPLATE)];
    }

    /**
     * The same rows with their meta ids.
     *
     * @return list<array<string,mixed>>
     */
    private function frameRowsWithIds(int $id): array
    {
        return array_values(array_filter($this->rows->metaRowsOf($id), static fn (array $r): bool => in_array($r['meta_key'], [ElementorDocument::KEY_PAGE_SETTINGS, self::TEMPLATE], true)));
    }

    /**
     * One key's stored values for a post.
     *
     * @return list<string|null>
     */
    private function storedValues(int $id, string $key): array
    {
        $out = [];
        foreach ($this->rows->metaRowsOf($id) as $row) {
            if ($row['meta_key'] === $key) {
                $out[] = $row['meta_value'];
            }
        }

        return $out;
    }

    /**
     * Meta values by key, less the element data.
     *
     * @param array<string,list<string|null>> $meta A page's meta values by key.
     * @return array<string,list<string|null>>
     */
    private static function keysWithoutData(array $meta): array
    {
        unset($meta[ElementorDocument::KEY_DATA]);

        return $meta;
    }

    /**
     * The values at which two trees differ, in tree order.
     *
     * @param mixed $before A tree.
     * @param mixed $after  The same tree after an edit.
     * @return list<mixed> The after-values of every leaf that differs.
     */
    private static function leafChanges(mixed $before, mixed $after): array
    {
        if (is_array($before) && is_array($after) && array_keys($before) === array_keys($after)) {
            $out = [];
            foreach ($before as $key => $value) {
                array_push($out, ...self::leafChanges($value, $after[$key]));
            }

            return $out;
        }

        return $before === $after ? [] : [$after];
    }

    /**
     * A structure's nodes with the second plan's price set.
     *
     * @param list<array<string,mixed>> $nodes Nodes.
     * @return list<array<string,mixed>>
     */
    private static function withPrice(array $nodes, string $price): array
    {
        foreach ($nodes as $i => $node) {
            if (isset($node['from_the_site'][self::PATH])) {
                $nodes[$i]['from_the_site'][self::PATH] = $price;
            }
        }

        return $nodes;
    }

    /**
     * Run the callbacks hooked on $tag, lowest priority first, each with the
     * number of arguments it accepts.
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
