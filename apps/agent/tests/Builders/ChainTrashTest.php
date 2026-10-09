<?php
/**
 * A person's undo of a wpmgr/page-create builder draft after WPMgr's later
 * wpmgr/page-edit changes to it, driven through the real signed Router: the
 * undo's signed parameters name those changes (p.revert.chain), and the draft
 * is moved to the trash only when their ledger rows account for every
 * difference between the draft as created and the draft now.
 *
 * Each edit and each edit's undo runs as production runs it, through the
 * Router. Elementor is FakeElementorApi behind the real ElementorAdapter; a
 * draft's document saves as Elementor's does on a draft: the post row moves
 * on, core keeps one revision of it (the hook core fires is fired, so the
 * write scope records it), and the tree and version rows are stored. Post,
 * meta, revision and snapshot rows live in FakeBuilderWpdb behind
 * EngineWpdb, which keeps the ledger's claim rows; the ledger rows are this
 * test's options.
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
use WPMgr\Agent\Abilities\Builders\BuilderPageCreate;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
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
 */
final class ChainTrashTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    /** The draft WPMgr created, and edits. */
    private const DRAFT = 120;

    /** Another draft WPMgr created. */
    private const OTHER = 121;

    /** A block-editor draft WPMgr created. */
    private const BLOCKS = 122;

    private const KIT = 4;

    /** The page-create requests that created DRAFT, OTHER and BLOCKS. */
    private const CREATE = '11111111-2222-4333-8444-777777777777';

    private const CREATE_OTHER = '11111111-2222-4333-8444-888888888888';

    private const CREATE_BLOCKS = '11111111-2222-4333-8444-999999999999';

    /** Page-edit requests on DRAFT, in the order they are made. */
    private const EDIT = '44444444-2222-4333-8444-666666666666';

    private const EDIT2 = '55555555-2222-4333-8444-666666666666';

    /** A page-edit request on OTHER. */
    private const EDIT_OTHER = '66666666-2222-4333-8444-666666666666';

    /** Ids in the golden two-column page. */
    private const HEADING = '52982f9';

    private const TEXT = '6cbe98a';

    /** What an undo with no chain answers when the draft changed since it was created. */
    private const SAID_EDITED = 'someone edited this draft after it was created';

    /** What an undo with no chain answers for a revision its creation did not make. */
    private const SAID_REVISION = 'this draft has revisions its creation did not make; open it in WordPress';

    private string $keyFile;

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;

    private FakeElementorApi $api;

    private FakeBuilderWpdb $rows;

    private EngineWpdb $wpdb;

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<string,object> */
    private array $roles = [];

    /** @var array<int,object> */
    private array $users = [];

    private int $currentUser = 0;

    private int $nextId = 300;

    /** The next revision id a save makes. */
    private int $nextRevision = 400;

    /** Minutes past the hour of the next save's modified time. */
    private int $clock = 0;

    /** @var array<int,list<int>> Post id => its revisions, in the order they were made. */
    private array $revisions = [];

    /** @var array<int,int> Post id => the user with an autosave of it. */
    private array $autosaves = [];

    /** @var array<int,int> Post id => the user holding its edit lock. */
    private array $locks = [];

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-bfe-chain-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
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
        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        Functions\when('get_site_option')->alias(static fn ($name, $default = false) => $default);
        Functions\when('get_current_blog_id')->justReturn(1);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('register_rest_route')->justReturn(true);
        Functions\when('wc_get_page_id')->justReturn(-1);
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_cache_delete')->justReturn(true);
        Functions\when('wp_kses_post')->returnArg();
        $GLOBALS['wp_version'] = '6.4.2';

        // Posts as core hands them out, from the same rows the SQL reads see.
        Functions\when('get_post')->alias(function ($id) {
            $row = $this->rows->postRow((int) $id);

            return $row === null ? null : (object) (['ID' => (int) $id] + $row);
        });
        Functions\when('get_post_meta')->alias(function ($id, $key = '', $single = false) {
            $values = [];
            foreach ($this->rows->metaRowsOf((int) $id) as $row) {
                if ($row['meta_key'] === $key) {
                    $values[] = $row['meta_value'];
                }
            }

            return $single ? ($values[0] ?? '') : $values;
        });
        Functions\when('wp_trash_post')->alias(function ($id) {
            $this->rows->update($this->rows->posts, ['post_status' => 'trash'], ['ID' => (int) $id]);

            return (object) ['ID' => (int) $id];
        });
        Functions\when('wp_get_post_revisions')->alias(function ($id, $args = null) {
            $out = [];
            foreach ($this->revisions[(int) $id] ?? [] as $revision) {
                $out[$revision] = (object) ['ID' => $revision, 'post_type' => 'revision', 'post_parent' => (int) $id];
            }

            return $out;
        });
        Functions\when('wp_get_post_autosave')->alias(function ($id, $user = 0) {
            $by = $this->autosaves[(int) $id] ?? null;

            return $by !== null && ($user === 0 || $user === $by) ? (object) ['ID' => 500, 'post_type' => 'revision'] : false;
        });
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);

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
            foreach ($u->roles as $r) {
                if (!empty($this->roles[$r]->capabilities[$cap])) {
                    return true;
                }
            }

            return false;
        });

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->rows             = new FakeBuilderWpdb();
        $this->wpdb             = new EngineWpdb($this->rows);
        $GLOBALS['wpdb']        = $this->wpdb;
        $this->addDraft(self::DRAFT, self::CREATE, 11, 'Our services');
        $this->addDraft(self::OTHER, self::CREATE_OTHER, 21, 'Our team');
        $this->rows->addPost(self::KIT, ['post_type' => 'elementor_library', 'post_status' => 'publish', 'post_title' => 'Kit', 'post_content' => '', 'post_modified_gmt' => '2026-10-01 07:00:00']);
        $this->rows->addMeta(2, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');

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
        unset($GLOBALS['wpdb'], $GLOBALS['wp_version']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_trash_after_two_ai_edits(): void
    {
        $this->enable();
        $this->edit(self::DRAFT, self::EDIT, self::ops());
        $this->edit(self::DRAFT, self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);
        $this->assertCount(3, $this->revisions[self::DRAFT], 'precondition: the creation and each edit made one revision');
        $this->assertNotSame($this->createdFp(self::CREATE), $this->fp(self::DRAFT), 'precondition: the edits changed the draft');

        // Without the chain the undo refuses, as it did before chains.
        $state = $this->state(self::DRAFT);
        $r     = $this->trash(self::CREATE, null);
        $this->assertSame(['created_post_touched', self::SAID_EDITED], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');

        // The chain out of order does not lead from the creation to the draft now.
        $r = $this->trash(self::CREATE, [self::EDIT2, self::EDIT]);
        $this->assertSame(['created_post_touched', BuilderPageCreate::CHAIN_BROKEN], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');

        $r = $this->trash(self::CREATE, [self::EDIT, self::EDIT2]);
        $this->assertSame(['ok' => true, 'outcome' => 'reverted', 'mode' => 'revert', 'request_id' => self::CREATE, 'post_id' => self::DRAFT, 'trashed' => true], $r);
        $this->assertSame('trash', $this->rows->postRow(self::DRAFT)['post_status']);
        $this->assertSame('trashed', $this->options['wpmgr_ability_ledger_' . self::CREATE]['undo_state']);
        $this->assertSame('draft', $this->rows->postRow(self::OTHER)['post_status'], 'no other draft is touched');
        $this->assertSame([], $this->wpdb->claims, 'the claims are released');
        $this->assertSame(0, $this->currentUser, 'the user is switched back to 0');

        $again = $this->trash(self::CREATE, [self::EDIT, self::EDIT2]);
        $this->assertSame('already_reverted', $again['outcome'] ?? null, (string) json_encode($again));
    }

    public function test_trash_after_edit_and_its_undo(): void
    {
        $this->enable();
        $applied = $this->edit(self::DRAFT, self::EDIT, self::ops());
        $undone  = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$undone['outcome'] ?? null, $undone['restored'] ?? null], (string) json_encode($undone));
        $this->assertSame($this->createdFp(self::CREATE), $this->fp(self::DRAFT), 'precondition: the undo put the draft back as created');
        $this->assertCount(2, $this->revisions[self::DRAFT], 'precondition: the edit\'s revision stays after its undo');

        // Without the chain the edit's revision is not the creation's.
        $state = $this->state(self::DRAFT);
        $r     = $this->trash(self::CREATE, null);
        $this->assertSame(['created_post_touched', self::SAID_REVISION], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');

        // An undone change counts for its revision, not for the fingerprint;
        // a later live change chains from the creation.
        $second = $this->edit(self::DRAFT, self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);
        $this->assertSame($this->createdFp(self::CREATE), $second['before_fp']);
        $r = $this->trash(self::CREATE, [self::EDIT, self::EDIT2]);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['trashed'] ?? null], (string) json_encode($r));
        $this->assertSame('trash', $this->rows->postRow(self::DRAFT)['post_status']);
    }

    public function test_person_edit_breaks_chain(): void
    {
        $this->enable();
        $this->edit(self::DRAFT, self::EDIT, self::ops());
        $this->edit(self::DRAFT, self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);
        $chain = [self::EDIT, self::EDIT2];
        $saved = $this->rows->postRow(self::DRAFT);

        // A person saved the page in Elementor: its rows moved on, with a revision.
        $this->personSaves(self::DRAFT);
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, $chain, 'a person saved the page');
        $this->undoPersonSave(self::DRAFT, $saved);

        // A person's rows changed with no revision, and a person's revision with no change.
        $this->rows->update($this->rows->posts, ['post_title' => 'Changed by a person'], ['ID' => self::DRAFT]);
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, $chain, 'a person changed the title');
        $this->rows->update($this->rows->posts, ['post_title' => $saved['post_title']], ['ID' => self::DRAFT]);
        $this->revisions[self::DRAFT][] = 999;
        $this->assertRefusedAndKept(BuilderPageCreate::FOREIGN_REVISION, $chain, 'a person\'s revision');
        array_pop($this->revisions[self::DRAFT]);

        // Someone has an autosave of it, or has it open.
        $this->autosaves[self::DRAFT] = 9;
        $this->assertRefusedAndKept(BuilderPageCreate::AUTOSAVE, $chain, 'an autosave');
        $this->autosaves = [];
        $this->locks[self::DRAFT] = 9;
        $this->assertRefusedAndKept(BuilderPageCreate::LOCKED, $chain, 'an open editor');
        $this->locks = [];

        // Nothing but WPMgr's changes again: the draft goes to the trash.
        $r = $this->trash(self::CREATE, $chain);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['trashed'] ?? null], (string) json_encode($r));
    }

    public function test_omitted_edit_breaks_chain(): void
    {
        $this->enable();
        $this->edit(self::DRAFT, self::EDIT, self::ops());
        $this->edit(self::DRAFT, self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);

        // The control plane names one of the two changes.
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, [self::EDIT], 'only the first change');
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, [self::EDIT2], 'only the second change');

        $r = $this->trash(self::CREATE, [self::EDIT, self::EDIT2]);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['trashed'] ?? null], (string) json_encode($r));
    }

    public function test_edit_of_another_post_refused(): void
    {
        $this->enable();
        $this->edit(self::DRAFT, self::EDIT, self::ops());
        $this->edit(self::OTHER, self::EDIT_OTHER, self::ops());
        $ledger = 'wpmgr_ability_ledger_' . self::EDIT;
        $row    = $this->options[$ledger];

        // A change of another draft, a request with no row, a request of
        // another ability, each named next to the real change.
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, [self::EDIT, self::EDIT_OTHER], 'a change of another draft');
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, [self::EDIT, '77777777-2222-4333-8444-666666666666'], 'a request with no ledger row');
        $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, [self::EDIT, self::CREATE_OTHER], 'a page-create request');

        // The real change's row, rewritten: each is refused.
        $rewrites = [
            'another post'           => ['target_post_id' => self::OTHER],
            'the post id as text'    => ['target_post_id' => (string) self::DRAFT],
            'another ability'        => ['ability' => OwnAbilities::NAME_PAGE_CREATE],
            'not completed'          => ['phase' => 'failed'],
            'another builder'        => ['builder' => 'beaver'],
            'undo not open'          => ['undo_state' => 'none'],
            'no before fingerprint'  => ['before_fp' => ''],
            'no after fingerprint'   => ['after_fp' => null],
            'revisions not ids'      => ['own_revision_ids' => ['401']],
            'revisions not a list'   => ['own_revision_ids' => [5 => 401]],
        ];
        foreach ($rewrites as $why => $fields) {
            $this->options[$ledger] = array_merge($row, $fields);
            $this->assertRefusedAndKept(BuilderPageCreate::CHAIN_BROKEN, [self::EDIT], $why);
        }
        $this->options[$ledger] = $row;

        // The other draft's own creation and change trash it, and only it.
        $r = $this->trash(self::CREATE_OTHER, [self::EDIT_OTHER]);
        $this->assertSame(['reverted', self::OTHER], [$r['outcome'] ?? null, $r['post_id'] ?? null], (string) json_encode($r));
        $this->assertSame(['draft', 'trash'], [$this->rows->postRow(self::DRAFT)['post_status'], $this->rows->postRow(self::OTHER)['post_status']]);

        // A block-editor draft has no page-edit changes: a chain is refused.
        $this->addBlockDraft();
        $state = $this->state(self::BLOCKS);
        $r     = $this->trash(self::CREATE_BLOCKS, [self::EDIT]);
        $this->assertSame(['created_post_touched', BuilderPageCreate::CHAIN_BROKEN], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame($state, $this->state(self::BLOCKS), 'nothing written');
        $r = $this->trash(self::CREATE_BLOCKS, null);
        $this->assertSame(['reverted', self::BLOCKS], [$r['outcome'] ?? null, $r['post_id'] ?? null], 'without a chain it is trashed as before: ' . json_encode($r));
    }

    public function test_empty_chain_unchanged_behaviour(): void
    {
        $this->enable();
        $forms = ['no p.revert' => null, 'an empty p.revert' => '{}', 'an empty chain' => '{"chain":[]}'];

        // Each state answers the same for every form of "no chain", in the
        // words an undo answered before chains.
        $states = [
            'a revision the creation did not make' => [fn () => $this->revisions[self::DRAFT][] = 999, self::SAID_REVISION],
            'the rows changed'                     => [fn () => $this->rows->update($this->rows->posts, ['post_title' => 'Changed by a person'], ['ID' => self::DRAFT]), self::SAID_EDITED],
            'an autosave'                          => [fn () => $this->autosaves[self::DRAFT] = 9, 'someone has unsaved changes to this draft; open it in WordPress'],
            'an open editor'                       => [fn () => $this->locks[self::DRAFT] = 9, 'someone is editing this draft right now'],
        ];
        $saved     = $this->rows->postRow(self::DRAFT);
        $revisions = $this->revisions;
        foreach ($states as $why => [$arrange, $detail]) {
            $arrange();
            foreach ($forms as $form => $revertJson) {
                $state = $this->state(self::DRAFT);
                $r     = $this->callP($this->pTrash(self::CREATE, $revertJson));
                $this->assertSame(['ok' => false, 'outcome' => 'refused', 'code' => 'created_post_touched', 'detail' => $detail, 'retryable' => false], $r, $why . ', ' . $form);
                $this->assertSame($state, $this->state(self::DRAFT), $why . ', ' . $form . ': nothing written');
            }
            $this->rows->addPost(self::DRAFT, $saved);
            $this->revisions = $revisions;
            $this->autosaves = [];
            $this->locks     = [];
        }

        // Untouched since it was created: trashed, then already undone.
        $r = $this->callP($this->pTrash(self::CREATE, '{"chain":[]}'));
        $this->assertSame(['ok' => true, 'outcome' => 'reverted', 'mode' => 'revert', 'request_id' => self::CREATE, 'post_id' => self::DRAFT, 'trashed' => true], $r);
        foreach ($forms as $form => $revertJson) {
            $again = $this->callP($this->pTrash(self::CREATE, $revertJson));
            $this->assertSame('already_reverted', $again['outcome'] ?? null, $form);
        }
    }

    public function test_chain_parameters(): void
    {
        $this->enable();
        $this->edit(self::DRAFT, self::EDIT, self::ops());
        $ids   = array_map(static fn (int $i): string => sprintf('%08x-2222-4333-8444-666666666666', $i), range(1, BuilderPageCreate::MAX_CHAIN + 1));
        $state = $this->state(self::DRAFT);

        $shapes = [
            'not an object'          => '"' . self::EDIT . '"',
            'a list'                 => '["' . self::EDIT . '"]',
            'another member'         => '{"snapshot_sha256":"' . str_repeat('a', 64) . '"}',
            'another member too'     => '{"chain":["' . self::EDIT . '"],"post_id":121}',
            'chain null'             => '{"chain":null}',
            'chain an object'        => '{"chain":{"0":"' . self::EDIT . '"}}',
            'chain a string'         => '{"chain":"' . self::EDIT . '"}',
            'an id not a string'     => '{"chain":[120]}',
            'an upper-case id'       => '{"chain":["ABCDEF12-2222-4333-8444-666666666666"]}',
            'an id with a newline'   => '{"chain":["' . self::EDIT . '\n"]}',
            'an id twice'            => '{"chain":["' . self::EDIT . '","' . self::EDIT . '"]}',
            'one id too many'        => (string) json_encode(['chain' => $ids]),
        ];
        foreach ($shapes as $why => $revertJson) {
            $r = $this->callP($this->pTrash(self::CREATE, $revertJson));
            $this->assertSame('bad_params', $r['code'] ?? null, $why . ': ' . json_encode($r));
        }
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');

        // The most a chain names is read, and refused only for what it names.
        $most = (string) json_encode(['chain' => array_slice($ids, 0, BuilderPageCreate::MAX_CHAIN)]);
        $r    = $this->callP($this->pTrash(self::CREATE, $most));
        $this->assertSame(['created_post_touched', BuilderPageCreate::CHAIN_BROKEN], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));

        // An undo takes no input, with a chain too.
        $r = $this->callP($this->pTrash(self::CREATE, '{"chain":["' . self::EDIT . '"]}', (string) json_encode(['post_id' => self::OTHER])));
        $this->assertSame('bad_input', $r['code'] ?? null, (string) json_encode($r));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * The golden two-column page.
     *
     * @return list<array<string,mixed>>
     */
    private static function page(): array
    {
        $fixture = json_decode((string) file_get_contents(self::FIXTURE), true, 512, JSON_THROW_ON_ERROR);

        return array_values(array_filter($fixture['cases'], static fn (array $c): bool => $c['name'] === 'columns'))[0]['tree'];
    }

    /**
     * The heading's text, and a new paragraph after the text.
     *
     * @return list<array<string,mixed>>
     */
    private static function ops(): array
    {
        return [
            ['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Summer sale & more'],
            ['op' => 'insert', 'after' => self::TEXT, 'outline' => [['type' => 'paragraph', 'text' => 'Open every day']]],
        ];
    }

    /**
     * A WPMgr Elementor draft holding the golden page as $create's save left
     * it, with the one revision that save made, its saving document, and its
     * completed page-create ledger row as BuilderPageCreate::write() records
     * it.
     */
    private function addDraft(int $postId, string $create, int $firstMetaId, string $title): void
    {
        $this->rows->addPost($postId, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => $title,
            'post_content'      => 'Left column Right column',
            'post_modified_gmt' => '2026-10-09 07:00:05',
        ]);
        $this->rows->addMeta($firstMetaId, $postId, DraftEligibility::MARKER_KEY, $create);
        $this->rows->addMeta($firstMetaId + 1, $postId, ElementorDocument::KEY_EDIT_MODE, 'builder');
        $this->rows->addMeta($firstMetaId + 2, $postId, ElementorDocument::KEY_TEMPLATE_TYPE, 'wp-page');
        $this->rows->addMeta($firstMetaId + 3, $postId, ElementorDocument::KEY_DATA, (string) json_encode(self::page()));
        $revision = $this->addRevision($postId);
        $this->savingDocument($postId);
        $this->options['wpmgr_ability_ledger_' . $create] = [
            'request_id'       => $create,
            'ability'          => OwnAbilities::NAME_PAGE_CREATE,
            'snapshot'         => 'created_post_trash',
            'phase'            => 'completed',
            'created_post_id'  => $postId,
            'after_fp'         => $this->fp($postId),
            'undo_state'       => 'available',
            'builder'          => 'elementor',
            'builder_version'  => '3.35.9',
            'format'           => 'classic',
            'own_revision_ids' => [$revision],
        ];
    }

    /**
     * A WPMgr block-editor draft, untouched since its creation.
     */
    private function addBlockDraft(): void
    {
        $this->rows->addPost(self::BLOCKS, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => 'Plain page',
            'post_content'      => '<!-- wp:paragraph --><p>Hi</p><!-- /wp:paragraph -->',
            'post_author'       => (string) $this->currentPrincipal(),
            'post_date'         => '2026-10-09 10:00:00',
            'post_date_gmt'     => '2026-10-09 08:00:00',
            'post_modified'     => '2026-10-09 10:00:00',
            'post_modified_gmt' => '2026-10-09 08:00:00',
        ]);
        $this->rows->addMeta(31, self::BLOCKS, AbilityRunCommand::META_CREATED_BY, self::CREATE_BLOCKS);
        $this->options['wpmgr_ability_ledger_' . self::CREATE_BLOCKS] = [
            'request_id'      => self::CREATE_BLOCKS,
            'ability'         => OwnAbilities::NAME_PAGE_CREATE,
            'snapshot'        => 'created_post_trash',
            'phase'           => 'post_created',
            'created_post_id' => self::BLOCKS,
            'after_fp'        => '',
            'undo_state'      => 'none',
        ];
    }

    /** The service user content_editing_enable created. */
    private function currentPrincipal(): int
    {
        foreach ($this->users as $id => $user) {
            return (int) $id;
        }

        return 0;
    }

    /**
     * A revision of $postId, as core stores one: answered by the revisions
     * read and by the snapshot's SQL read.
     */
    private function addRevision(int $postId): int
    {
        $revision = $this->nextRevision++;
        $this->rows->addPost($revision, ['post_type' => 'revision', 'post_status' => 'inherit', 'post_parent' => (string) $postId, 'post_title' => '', 'post_content' => '']);
        $this->revisions[$postId][] = $revision;

        return $revision;
    }

    /**
     * Give $postId a stand-in Elementor document whose save() stores the
     * elements as Elementor's does on a draft: the post row's modified time
     * moves on, core inserts one revision (firing the hook core fires), and
     * every _elementor_data and _elementor_version row is replaced by one.
     */
    private function savingDocument(int $postId): void
    {
        $document         = $this->api->addDocument($postId);
        $document->onSave = function ($data) use ($postId): bool {
            $elements = is_array($data) && is_array($data['elements'] ?? null) ? $data['elements'] : [];
            $this->rows->update($this->rows->posts, ['post_modified_gmt' => sprintf('2026-10-09 08:%02d:00', ++$this->clock)], ['ID' => $postId]);
            $revision = $this->addRevision($postId);
            $this->fire('wp_insert_post', $revision, (object) ['ID' => $revision, 'post_type' => 'revision', 'post_parent' => $postId], false);
            foreach ([ElementorDocument::KEY_DATA => (string) json_encode($elements), '_elementor_version' => '3.35.9'] as $key => $value) {
                $this->rows->delete($this->rows->postmeta, ['post_id' => $postId, 'meta_key' => $key]);
                $this->rows->insert($this->rows->postmeta, ['post_id' => $postId, 'meta_key' => $key, 'meta_value' => $value]);
            }

            return true;
        };
    }

    /**
     * A person saves $postId in the editor: new data, a new modified time
     * and a revision no ledger row names.
     */
    private function personSaves(int $postId): void
    {
        $this->rows->update($this->rows->posts, ['post_modified_gmt' => '2026-10-09 09:30:00'], ['ID' => $postId]);
        $this->revisions[$postId][] = 998;
    }

    /**
     * Put back $postId's row as $saved and drop the person's revision.
     *
     * @param array<string,mixed> $saved The posts row before the person's save.
     */
    private function undoPersonSave(int $postId, array $saved): void
    {
        $this->rows->addPost($postId, $saved);
        $this->revisions[$postId] = array_values(array_diff($this->revisions[$postId], [998]));
    }

    /**
     * Precheck, then write with the approved digests, through the Router.
     *
     * @param list<array<string,mixed>> $ops Operations on $postId.
     * @return array<string,mixed> The write's answer.
     */
    private function edit(int $postId, string $requestId, array $ops): array
    {
        $input = (string) json_encode(['post_id' => $postId, 'base_fingerprint' => $this->fp($postId), 'operations' => $ops]);
        $pre   = $this->callP($this->pEdit('precheck', $postId, $requestId, $input));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $r = $this->callP($this->pEdit('write', $postId, $requestId, $input, $pre));
        $this->assertSame([true, 'applied'], [$r['ok'] ?? null, $r['outcome'] ?? null], (string) json_encode($r));

        return $r;
    }

    /**
     * A person's undo of a page-edit change, as the control plane sends it.
     *
     * @return array<string,mixed>
     */
    private function undo(string $requestId, string $hash): array
    {
        $entry = $this->editEntry();
        $p     = (string) json_encode(['mode' => 'revert', 'request_id' => $requestId, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => '{}']);

        return $this->callP(substr($p, 0, -1) . ',"revert":' . (string) json_encode(['snapshot_sha256' => $hash]) . '}');
    }

    /**
     * A person's undo of a page-create request; $chain null sends no p.revert.
     *
     * @param list<string>|null $chain The page-edit requests p.revert.chain names.
     * @return array<string,mixed>
     */
    private function trash(string $requestId, ?array $chain): array
    {
        return $this->callP($this->pTrash($requestId, $chain === null ? null : (string) json_encode(['chain' => $chain])));
    }

    /**
     * An undo of $chain that is created_post_touched with $detail and writes
     * nothing: no row, no ledger change, no snapshot, no claim left.
     *
     * @param list<string> $chain The chain.
     */
    private function assertRefusedAndKept(string $detail, array $chain, string $why): void
    {
        $state = $this->state(self::DRAFT);
        $r     = $this->trash(self::CREATE, $chain);
        $this->assertSame(['ok' => false, 'outcome' => 'refused', 'code' => 'created_post_touched', 'detail' => $detail, 'retryable' => false], $r, $why);
        $this->assertSame($state, $this->state(self::DRAFT), $why . ': nothing written');
        $this->assertSame([], $this->wpdb->claims, $why . ': the claims are released');
    }

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

    /** Go's page-create entry, with Elementor enabled. */
    private function createEntry(): string
    {
        $raw  = file_get_contents(__DIR__ . '/../fixtures/ability-run/page-create.json');
        $base = json_decode((string) json_decode((string) $raw, true)['entry'], true);
        self::assertIsArray($base);
        $base['limits'] = ['builders_enabled' => ['elementor']] + (array) $base['limits'];

        return (string) json_encode($base);
    }

    /**
     * p for a precheck or a write of $postId, as the control plane builds it.
     *
     * @param array<string,mixed>|null $expected The precheck answer, for a write.
     */
    private function pEdit(string $mode, int $postId, string $requestId, string $input, ?array $expected = null): string
    {
        $entry = $this->editEntry();
        $p     = ['mode' => $mode, 'request_id' => $requestId, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input, 'allowed_draft_ids' => [$postId]];
        if ($expected !== null) {
            $p['expected'] = ['precheck_digest' => $expected['precheck_digest'], 'preview_digest' => $expected['preview_digest']];
        }

        return (string) json_encode($p);
    }

    /**
     * p for a page-create undo: $revertJson is p.revert's exact text (null leaves it out).
     */
    private function pTrash(string $requestId, ?string $revertJson, string $input = '{}'): string
    {
        $entry = $this->createEntry();
        $text  = (string) json_encode(['mode' => 'revert', 'request_id' => $requestId, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input]);

        return $revertJson === null ? $text : substr($text, 0, -1) . ',"revert":' . $revertJson . '}';
    }

    /** A post's builder_document_v1 fingerprint now. */
    private function fp(int $postId): string
    {
        return (string) BuilderDocumentFingerprint::ofPost($postId, ElementorDocument::DESCRIPTOR_KEYS);
    }

    /** The fingerprint a page-create request's ledger row recorded. */
    private function createdFp(string $create): string
    {
        return (string) $this->options['wpmgr_ability_ledger_' . $create]['after_fp'];
    }

    /**
     * Everything an undo could write for a post: its meta rows as stored,
     * its posts row, its revisions, and every ledger row and snapshot.
     *
     * @return array<string,mixed>
     */
    private function state(int $postId): array
    {
        $ledger = array_filter($this->options, static fn ($k): bool => str_starts_with((string) $k, 'wpmgr_ability_ledger_'), ARRAY_FILTER_USE_KEY);

        return [
            'meta'      => $this->rows->metaRowsOf($postId),
            'post'      => $this->rows->postRow($postId),
            'revisions' => $this->revisions[$postId] ?? [],
            'ledger'    => $ledger,
            'snapshots' => array_map(static fn (array $r): array => [$r['option_name'], $r['option_value']], $this->rows->optionRows()),
        ];
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
     * @return array<string,mixed>
     */
    private function enable(): array
    {
        return $this->post('content_editing_enable', '{}', null);
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
}
