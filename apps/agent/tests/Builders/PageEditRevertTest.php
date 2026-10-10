<?php
/**
 * A person's undo of a wpmgr/page-edit change, driven through the real signed
 * Router: the snapshot-hash echo, the post from the ledger only, the undo of
 * only what the change wrote, newest first, and the refusals that write
 * nothing.
 *
 * Each edit runs as production runs it: precheck, then write with the
 * approved digests. Elementor is FakeElementorApi behind the real
 * ElementorAdapter, handed to the command through its test-only adapter
 * seam; the draft's document stores the elements as Elementor's save does.
 * Post, meta and snapshot rows live in FakeBuilderWpdb behind EngineWpdb,
 * which keeps the ledger's claim rows; the ledger rows are this test's
 * options, which get_option() answers from a per-request cache as core does.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
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
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageEdit
 */
final class PageEditRevertTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    /** The draft WPMgr created, and edits. */
    private const DRAFT = 120;

    /** Another draft WPMgr created. */
    private const OTHER = 121;

    private const KIT = 4;

    /** The page-create requests that created DRAFT and OTHER. */
    private const CREATE = '11111111-2222-4333-8444-777777777777';

    private const CREATE_OTHER = '11111111-2222-4333-8444-888888888888';

    /** Two page-edit requests on DRAFT, in order. */
    private const EDIT = '44444444-2222-4333-8444-666666666666';

    private const EDIT2 = '55555555-2222-4333-8444-666666666666';

    /** Element data a forged copy of the page would put back. */
    private const FORGED_DATA = '[{"id":"0000001","elType":"widget","widgetType":"html","settings":{"html":"<script>1<\/script>"},"elements":[]}]';

    /** Ids in the golden two-column page. */
    private const HEADING = '52982f9';

    private const TEXT = '6cbe98a';

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

    /** @var array<int,int> Post id => the user with an autosave of it. */
    private array $autosaves = [];

    /** @var array<int,int> Post id => the user holding its edit lock. */
    private array $locks = [];

    /** The fingerprint of DRAFT as its creation left it. */
    private string $createdFp = '';

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    /** @var (\Closure(string, mixed): bool)|null True for an update_option() the site's database fails. */
    private ?\Closure $refuseOption = null;

    /**
     * @var array<string,mixed> This request's options cache, as core keeps it
     *      without a persistent object cache: get_option() answers a name it
     *      has read or written in this request from here, so a write another
     *      request makes to $options meanwhile is not seen until
     *      wp_cache_delete($name, 'options'). Emptied when a request starts.
     */
    private array $optionCache = [];

    /** @var array<string,true> Names this request found missing, as core's notoptions. */
    private array $notOptions = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-bfe-revert-' . bin2hex(random_bytes(8)) . '.key';
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
        $this->refuseOption = null;
        $this->optionCache  = [];
        $this->notOptions   = [];
        // The options API as core runs it against this request's cache.
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
            // Core compares with what get_option() answers, cached or not.
            if ($value === \get_option($name)) {
                return false;
            }
            if ($this->refuseOption !== null && ($this->refuseOption)($name, $value)) {
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
            // Core asks get_option(), so this request's cache, whether the
            // option exists.
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
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('wp_kses_post')->returnArg();
        Functions\when('get_post_meta')->justReturn('');
        Functions\when('wp_get_post_autosave')->alias(function ($id, $user = 0) {
            $by = $this->autosaves[(int) $id] ?? null;

            return $by !== null && ($user === 0 || $user === $by) ? (object) ['ID' => 500, 'post_type' => 'revision'] : false;
        });
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);
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

        $this->rows      = new FakeBuilderWpdb();
        $this->wpdb      = new EngineWpdb($this->rows);
        $GLOBALS['wpdb'] = $this->wpdb;
        $this->addDraft(self::DRAFT, self::CREATE, 11, 'Our services');
        $this->addDraft(self::OTHER, self::CREATE_OTHER, 21, 'Our team');
        $this->rows->addPost(self::KIT, ['post_type' => 'elementor_library', 'post_status' => 'publish', 'post_title' => 'Kit', 'post_content' => '', 'post_modified_gmt' => '2026-10-01 07:00:00']);
        $this->rows->addMeta(2, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->createdFp = $this->fp();
        $this->options['wpmgr_ability_ledger_' . self::CREATE]['after_fp'] = $this->createdFp;

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->api->storingDocument(self::DRAFT, $this->rows);
        $this->api->storingDocument(self::OTHER, $this->rows);
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

    public function test_hash_must_agree_three_ways(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $hash    = $applied['snapshot_sha256'];
        $option  = BuilderDocumentSnapshot::OPTION_PREFIX . self::EDIT;
        $ledger  = 'wpmgr_ability_ledger_' . self::EDIT;
        $bytes   = $this->snapshotBytes(self::EDIT);
        $row     = $this->options[$ledger];
        $this->assertSame([$hash, $hash], [$row['snapshot_sha256'], hash('sha256', $bytes)], 'the write answered the hash of the stored bytes');

        $cases = [
            'another signed hash'      => [fn () => null, str_repeat('a', 64)],
            'the stored bytes changed' => [fn () => $this->rows->setOptionValue($option, $this->forgedSnapshot($bytes)), $hash],
            'the ledger hash changed'  => [function () use ($ledger): void {
                $this->options[$ledger]['snapshot_sha256'] = hash('sha256', 'another copy');
            }, $hash],
            'the snapshot is gone'     => [fn () => $this->rows->deleteOptionLikeCore($option), $hash],
        ];
        $after = $this->state(self::DRAFT);
        foreach ($cases as $why => [$arrange, $signed]) {
            $arrange();
            $this->assertTamperedAndNothingWritten($why, $signed);

            // Back to the copy and the record the write left.
            if ($this->optionValue($option) === null) {
                $this->rows->addOption($option, $bytes);
            }
            $this->rows->setOptionValue($option, $bytes);
            $this->options[$ledger] = $row;
        }
        $this->assertSame($after, $this->state(self::DRAFT));

        // All three agree: the undo runs.
        $r = $this->undo(self::EDIT, $hash);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
    }

    public function test_copy_and_record_rewritten_together_refused(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $option  = BuilderDocumentSnapshot::OPTION_PREFIX . self::EDIT;
        $ledger  = 'wpmgr_ability_ledger_' . self::EDIT;

        // Both site-side records rewritten together: a well-formed copy that
        // would put other content on the page, a ledger row naming its hash,
        // and the edited key's before-hash naming its rows. Only the hash the
        // control plane signed still names the copy the write kept.
        $forged = $this->forgedSnapshot($this->snapshotBytes(self::EDIT));
        $this->rows->setOptionValue($option, $forged);
        $this->options[$ledger]['snapshot_sha256'] = hash('sha256', $forged);
        foreach ($this->options[$ledger]['changed_keys'] as $i => $k) {
            if ($k['key'] === ElementorDocument::KEY_DATA) {
                $this->options[$ledger]['changed_keys'][$i]['before_sha256'] = BuilderDocumentRestore::rowsSha256([self::FORGED_DATA]);
            }
        }
        $this->assertNotNull(BuilderDocumentSnapshot::decode($forged, self::EDIT, self::DRAFT), 'the forged copy is a well-formed snapshot of this request and post');
        $this->assertSame(hash('sha256', $forged), $this->options[$ledger]['snapshot_sha256'], 'the ledger row and the stored copy agree with each other');

        $this->assertTamperedAndNothingWritten('the copy and its record rewritten together', $applied['snapshot_sha256']);
        $this->assertNotContains(self::FORGED_DATA, $this->byKey(self::DRAFT)[ElementorDocument::KEY_DATA]);
    }

    public function test_undo_keeps_later_featured_image(): void
    {
        $this->enable();
        $before = $this->byKey(self::DRAFT);
        $post   = $this->rows->postRow(self::DRAFT);
        $baseFp = $this->fp();
        $other  = $this->rowsOfPost(self::OTHER);

        $applied = $this->edit(self::EDIT, self::ops());
        $this->assertNotSame($before, $this->byKey(self::DRAFT), 'the edit changed the page');
        // A person sets the featured image afterwards: one meta row, no save.
        $this->rows->insert($this->rows->postmeta, ['post_id' => self::DRAFT, 'meta_key' => '_thumbnail_id', 'meta_value' => '77']);

        $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['ok' => true, 'outcome' => 'reverted', 'mode' => 'revert', 'request_id' => self::EDIT, 'post_id' => self::DRAFT, 'restored' => true], $r);
        $this->assertSame(['77'], $this->byKey(self::DRAFT)['_thumbnail_id'] ?? null, 'the featured image set after the edit stays');
        $want = $before + ['_thumbnail_id' => ['77']];
        ksort($want, SORT_STRING);
        $this->assertSame($want, $this->byKey(self::DRAFT), 'every key the edit wrote holds its bytes from before the edit; a key it added is gone');
        $this->assertSame($post, $this->rows->postRow(self::DRAFT));
        $this->assertSame($baseFp, $this->fp());
        $this->assertSame($other, $this->rowsOfPost(self::OTHER), 'no other post is touched');

        $row = $this->options['wpmgr_ability_ledger_' . self::EDIT];
        $this->assertSame(['restored', $baseFp], [$row['undo_state'], $row['restored_fp']]);
        $this->assertIsInt($row['reverted_at']);
        $this->assertSame([[self::DRAFT]], $this->api->callsTo('deletePostCss'), 'the page\'s generated CSS is dropped');
        $this->assertSame([], $this->wpdb->claims, 'the target claim is released');
        $this->assertSame(0, $this->currentUser, 'the user is switched back to 0');

        $ledger = $this->callP((string) json_encode(['mode' => 'ledger', 'request_id' => self::EDIT]));
        $this->assertSame([true, 'restored'], [$ledger['found'] ?? null, $ledger['undo_state'] ?? null]);
    }

    public function test_newest_first(): void
    {
        $this->enable();
        $before = $this->byKey(self::DRAFT);
        $first  = $this->edit(self::EDIT, self::ops());
        $mid    = $this->byKey(self::DRAFT);
        $midFp  = $this->fp();
        $second = $this->edit(self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);
        $this->assertNotSame($first['snapshot_sha256'], $second['snapshot_sha256']);

        // The older edit first: its key moved on since, so nothing is written.
        $state = $this->state(self::DRAFT);
        $older = $this->undo(self::EDIT, $first['snapshot_sha256']);
        $this->assertSame(['conflict', 'changed_after_this_change', false], [$older['code'] ?? null, $older['detail'] ?? null, $older['ok'] ?? null], (string) json_encode($older));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');
        $this->assertSame('available', $this->options['wpmgr_ability_ledger_' . self::EDIT]['undo_state'], 'the older edit can still be undone');

        // Newest first: each puts back the page as it was before it.
        $newer = $this->undo(self::EDIT2, $second['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$newer['outcome'] ?? null, $newer['restored'] ?? null], (string) json_encode($newer));
        $this->assertSame($mid, $this->byKey(self::DRAFT));
        $this->assertSame($midFp, $this->fp());

        $older = $this->undo(self::EDIT, $first['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$older['outcome'] ?? null, $older['restored'] ?? null], (string) json_encode($older));
        $this->assertSame($before, $this->byKey(self::DRAFT));
        $this->assertSame($this->createdFp, $this->fp(), 'the page is back to what its creation left');
    }

    public function test_post_id_from_ledger_only(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $hash    = $applied['snapshot_sha256'];
        $draft   = $this->rowsOfPost(self::DRAFT);
        $other   = $this->rowsOfPost(self::OTHER);

        // An undo takes no input: naming a post is refused.
        $named = $this->callP($this->pRevert(self::EDIT, (string) json_encode(['snapshot_sha256' => $hash]), (string) json_encode(['post_id' => self::OTHER])));
        $this->assertSame('bad_input', $named['code'] ?? null, (string) json_encode($named));
        $this->assertSame([$draft, $other], [$this->rowsOfPost(self::DRAFT), $this->rowsOfPost(self::OTHER)], 'nothing written');

        // A ledger row rewritten to name the other post: the snapshot is not of it.
        $ledger = 'wpmgr_ability_ledger_' . self::EDIT;
        $row    = $this->options[$ledger];
        $this->options[$ledger]['target_post_id'] = self::OTHER;
        $moved = $this->undo(self::EDIT, $hash);
        $this->assertSame('snapshot_tampered', $moved['code'] ?? null, (string) json_encode($moved));
        $this->assertSame([$draft, $other], [$this->rowsOfPost(self::DRAFT), $this->rowsOfPost(self::OTHER)], 'nothing written');
        $this->options[$ledger] = $row;

        // A draft list naming the other post changes nothing: the undo is of
        // the post the ledger names.
        $r = $this->callP($this->pRevert(self::EDIT, (string) json_encode(['snapshot_sha256' => $hash]), '{}', [self::OTHER]));
        $this->assertSame(['reverted', self::DRAFT], [$r['outcome'] ?? null, $r['post_id'] ?? null], (string) json_encode($r));
        $this->assertSame($other, $this->rowsOfPost(self::OTHER), 'the other draft is untouched');
        $this->assertSame($this->createdFp, $this->fp());
    }

    public function test_published_target_refused(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $cases   = [
            'publish' => ['refused_published', 'published'],
            'future'  => ['refused_published', 'scheduled'],
            'private' => ['refused_published', 'private'],
            'pending' => ['target_not_draft', 'pending review'],
            'trash'   => ['target_not_draft', 'in the trash'],
        ];
        foreach ($cases as $status => [$code, $why]) {
            $this->rows->update($this->rows->posts, ['post_status' => $status], ['ID' => self::DRAFT]);
            $state = $this->state(self::DRAFT);
            $r     = $this->undo(self::EDIT, $applied['snapshot_sha256']);
            $this->assertSame([$code, false], [$r['code'] ?? null, $r['ok'] ?? null], $why . ': ' . json_encode($r));
            $this->assertSame($state, $this->state(self::DRAFT), $why . ': nothing written');
            $this->assertSame([], $this->wpdb->claims, $why . ': the target claim is released');
        }

        // Gone altogether.
        $this->rows->update($this->rows->posts, ['post_status' => 'draft'], ['ID' => self::DRAFT]);
        $posts = new ReflectionProperty(FakeBuilderWpdb::class, 'postRows');
        $kept  = $posts->getValue($this->rows);
        $gone  = $kept;
        unset($gone[self::DRAFT]);
        $posts->setValue($this->rows, $gone);
        $missing = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame('target_not_draft', $missing['code'] ?? null, (string) json_encode($missing));
        $posts->setValue($this->rows, $kept);

        // A draft again: the undo runs.
        $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
    }

    public function test_open_or_autosaved_target_refused(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $state   = $this->state(self::DRAFT);
        $cases   = [
            'someone has the page open'          => [fn () => $this->locks[self::DRAFT] = 9, 'editor_open'],
            'someone has an autosave of it'      => [fn () => $this->autosaves[self::DRAFT] = 9, 'autosave_pending'],
            'Elementor reports a newer autosave' => [fn () => $this->api->documents[self::DRAFT]->newerAutosave = (object) ['ID' => 501], 'autosave_pending'],
        ];
        foreach ($cases as $why => [$arrange, $detail]) {
            $arrange();
            $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
            $this->assertSame(['conflict', $detail], [$r['code'] ?? null, $r['detail'] ?? null], $why . ': ' . json_encode($r));
            $this->assertSame($state, $this->state(self::DRAFT), $why . ': nothing written');
            $this->locks     = [];
            $this->autosaves = [];
            $this->api->documents[self::DRAFT]->newerAutosave = false;
        }

        $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
    }

    public function test_editor_open_is_retryable_other_conflicts_are_not(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $state   = $this->state(self::DRAFT);

        // Someone else holds the edit lock: worth asking again once it lapses.
        $this->locks[self::DRAFT] = 9;
        $open = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['conflict', 'editor_open', true, false], [$open['code'] ?? null, $open['detail'] ?? null, $open['retryable'] ?? null, $open['ok'] ?? null], (string) json_encode($open));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written, no ledger change');
        $this->assertSame([], $this->wpdb->claims, 'the target claim is released');
        $this->assertSame([], $this->api->callsTo('deletePostCss'));
        $this->locks = [];

        // An autosave waits on a person.
        $this->autosaves[self::DRAFT] = 9;
        $autosave = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['conflict', 'autosave_pending', false], [$autosave['code'] ?? null, $autosave['detail'] ?? null, $autosave['retryable'] ?? null]);
        $this->autosaves = [];

        // A later edit moved the page on: asking again cannot help.
        $this->edit(self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);
        $moved = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['conflict', 'changed_after_this_change', false], [$moved['code'] ?? null, $moved['detail'] ?? null, $moved['retryable'] ?? null]);
    }

    public function test_replay_already_reverted(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $first   = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$first['outcome'] ?? null, $first['restored'] ?? null], (string) json_encode($first));
        $state = $this->state(self::DRAFT);

        $again = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['ok' => true, 'outcome' => 'already_reverted', 'mode' => 'revert', 'request_id' => self::EDIT, 'post_id' => self::DRAFT, 'restored' => true], $again);
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written again');
        $this->assertCount(1, $this->api->callsTo('deletePostCss'), 'the caches are dropped once');
    }

    public function test_an_undo_the_ledger_cannot_record_is_never_done_and_the_next_undo_finishes_it(): void
    {
        $this->enable();
        $before  = $this->byKey(self::DRAFT);
        $post    = $this->rows->postRow(self::DRAFT);
        $baseFp  = $this->fp();
        $applied = $this->edit(self::EDIT, self::ops());
        $hash    = $applied['snapshot_sha256'];
        $ledger  = 'wpmgr_ability_ledger_' . self::EDIT;

        // The page goes back, then the ledger cannot take the undo's record.
        $this->refuseOption = static fn (string $name, $value): bool => $name === $ledger && is_array($value) && ($value['undo_state'] ?? null) === 'restored';
        $r = $this->undo(self::EDIT, $hash);
        $this->assertSame([false, 'data_unreadable', true], [$r['ok'] ?? null, $r['code'] ?? null, $r['restored'] ?? null], 'never answered as done: ' . json_encode($r));
        $this->assertSame($before, $this->byKey(self::DRAFT), 'the page is back');
        $this->assertSame($post, $this->rows->postRow(self::DRAFT));
        $this->assertSame('restoring', $this->options[$ledger]['undo_state'], 'the row says an undo started and may have put the page back');
        $this->assertSame([], $this->wpdb->claims, 'the target claim is released');

        // The next undo finishes it, writes nothing again, and records it.
        $this->refuseOption = null;
        $rows  = $this->rowsOfPost(self::DRAFT);
        $again = $this->undo(self::EDIT, $hash);
        $this->assertSame(['ok' => true, 'outcome' => 'reverted', 'mode' => 'revert', 'request_id' => self::EDIT, 'post_id' => self::DRAFT, 'restored' => true], $again);
        $this->assertSame($rows, $this->rowsOfPost(self::DRAFT), 'nothing written again');
        $row = $this->options[$ledger];
        $this->assertSame(['restored', $baseFp], [$row['undo_state'], $row['restored_fp']]);
        $this->assertIsInt($row['reverted_at']);

        $last = $this->undo(self::EDIT, $hash);
        $this->assertSame(['ok' => true, 'outcome' => 'already_reverted', 'mode' => 'revert', 'request_id' => self::EDIT, 'post_id' => self::DRAFT, 'restored' => true], $last);
    }

    public function test_an_undo_that_cannot_record_its_start_writes_nothing(): void
    {
        $this->enable();
        $before  = $this->byKey(self::DRAFT);
        $applied = $this->edit(self::EDIT, self::ops());
        $ledger  = 'wpmgr_ability_ledger_' . self::EDIT;
        $state   = $this->state(self::DRAFT);

        $this->refuseOption = static fn (string $name): bool => $name === $ledger;
        $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame([false, 'data_unreadable'], [$r['ok'] ?? null, $r['code'] ?? null], (string) json_encode($r));
        $this->assertArrayNotHasKey('restored', $r);
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written, the row still available');
        $this->assertSame([], $this->api->callsTo('deletePostCss'));

        $this->refuseOption = null;
        $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
        $this->assertSame($before, $this->byKey(self::DRAFT));
    }

    public function test_an_undo_left_restoring_runs_again_or_refuses_a_page_that_moved_on(): void
    {
        $this->enable();
        $before  = $this->byKey(self::DRAFT);
        $applied = $this->edit(self::EDIT, self::ops());
        $ledger  = 'wpmgr_ability_ledger_' . self::EDIT;

        // An undo marked its start and its restore never landed: the page
        // still holds the edit, and the next undo puts it back.
        $this->options[$ledger]['undo_state'] = 'restoring';
        $r = $this->undo(self::EDIT, $applied['snapshot_sha256']);
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
        $this->assertSame($before, $this->byKey(self::DRAFT));
        $this->assertSame('restored', $this->options[$ledger]['undo_state']);

        // Another edit, whose undo marked its start, and someone saved the
        // page since: it holds neither what the edit left nor what it found.
        // Nothing is written and the row is left as it was.
        $second = $this->edit(self::EDIT2, [['op' => 'set_text', 'ref' => self::HEADING, 'field' => 'text', 'text' => 'Autumn prices']]);
        $ledger = 'wpmgr_ability_ledger_' . self::EDIT2;
        $this->options[$ledger]['undo_state'] = 'restoring';
        foreach ($this->rows->metaRowsOf(self::DRAFT) as $meta) {
            if ($meta['meta_key'] === ElementorDocument::KEY_DATA) {
                $this->rows->delete($this->rows->postmeta, ['meta_id' => $meta['meta_id']]);
                $this->rows->insert($this->rows->postmeta, ['post_id' => self::DRAFT, 'meta_key' => ElementorDocument::KEY_DATA, 'meta_value' => str_replace('Autumn', 'Winter', (string) $meta['meta_value'])]);
            }
        }
        $state = $this->state(self::DRAFT);
        $r     = $this->undo(self::EDIT2, $second['snapshot_sha256']);
        $this->assertSame(['conflict', 'changed_after_this_change'], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written, the row still restoring');
    }

    public function test_an_undo_finished_while_this_one_waited_for_the_post_answers_already_reverted(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $hash    = $applied['snapshot_sha256'];
        $ledger  = 'wpmgr_ability_ledger_' . self::EDIT;
        $done    = $this->undo(self::EDIT, $hash);
        $this->assertSame('reverted', $done['outcome'] ?? null, (string) json_encode($done));
        $finished = $this->options[$ledger];
        $state    = $this->state(self::DRAFT);

        // This undo read the row before the other one recorded its undo, and
        // takes the claim on the post after it.
        $this->options[$ledger]['undo_state'] = 'available';
        $this->wpdb->afterClaim = function (string $name) use ($ledger, $finished): void {
            if ($name === 'wpmgr_ability_target_' . self::DRAFT) {
                $this->options[$ledger] = $finished;
                $this->wpdb->afterClaim = null;
            }
        };
        $r = $this->undo(self::EDIT, $hash);
        $this->assertSame(['ok' => true, 'outcome' => 'already_reverted', 'mode' => 'revert', 'request_id' => self::EDIT, 'post_id' => self::DRAFT, 'restored' => true], $r);
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written: the row still says restored');
    }

    public function test_revert_parameters_and_ledger_rows(): void
    {
        $this->enable();
        $applied = $this->edit(self::EDIT, self::ops());
        $hash    = $applied['snapshot_sha256'];
        $state   = $this->state(self::DRAFT);

        $shapes = [
            'no revert member'     => null,
            'not an object'        => '"' . $hash . '"',
            'a list'               => '["' . $hash . '"]',
            'another member too'   => '{"snapshot_sha256":"' . $hash . '","chain":[]}',
            'upper-case hex'       => '{"snapshot_sha256":"' . strtoupper($hash) . '"}',
            'short'                => '{"snapshot_sha256":"' . substr($hash, 1) . '"}',
            'a trailing newline'   => '{"snapshot_sha256":"' . $hash . '\n"}',
            'not a string'         => '{"snapshot_sha256":1}',
        ];
        foreach ($shapes as $why => $revertJson) {
            $r = $this->callP($this->pRevert(self::EDIT, $revertJson));
            $this->assertSame('bad_params', $r['code'] ?? null, $why . ': ' . json_encode($r));
        }
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');

        // No ledger row; another ability's row; a change that did not complete.
        $this->assertSame('nothing_to_revert', $this->undo('99999999-2222-4333-8444-666666666666', $hash)['code'] ?? null);
        $this->assertSame('ledger_ability_mismatch', $this->undo(self::CREATE, $hash)['code'] ?? null);
        $ledger = 'wpmgr_ability_ledger_' . self::EDIT;
        $row    = $this->options[$ledger];
        foreach (['failed phase' => ['phase' => 'failed'], 'undo not open' => ['undo_state' => 'none'], 'no changed keys' => ['changed_keys' => null]] as $why => $fields) {
            $this->options[$ledger] = array_merge($row, $fields);
            $r                      = $this->undo(self::EDIT, $hash);
            $this->assertSame('not_revertible', $r['code'] ?? null, $why . ': ' . json_encode($r));
        }
        $this->options[$ledger] = $row;
        $this->assertSame($state, $this->state(self::DRAFT), 'nothing written');

        // An undo stays available on a disabled entry.
        $r = $this->callP($this->pRevert(self::EDIT, (string) json_encode(['snapshot_sha256' => $hash]), '{}', null, false));
        $this->assertSame(['reverted', true], [$r['outcome'] ?? null, $r['restored'] ?? null], (string) json_encode($r));
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
     * A WPMgr Elementor draft holding the golden page, created by $create.
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
        $this->options['wpmgr_ability_ledger_' . $create] = [
            'request_id'      => $create,
            'ability'         => OwnAbilities::NAME_PAGE_CREATE,
            'phase'           => 'completed',
            'created_post_id' => $postId,
            'undo_state'      => 'available',
            'builder'         => 'elementor',
        ];
    }

    /**
     * Precheck, then write with the approved digests, through the Router.
     *
     * @param list<array<string,mixed>> $ops Operations on DRAFT.
     * @return array<string,mixed> The write's answer.
     */
    private function edit(string $requestId, array $ops): array
    {
        $input = (string) json_encode(['post_id' => self::DRAFT, 'base_fingerprint' => $this->fp(), 'operations' => $ops]);
        $pre   = $this->callP($this->p('precheck', $requestId, $input));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $r = $this->callP($this->p('write', $requestId, $input, $pre));
        $this->assertSame([true, 'applied'], [$r['ok'] ?? null, $r['outcome'] ?? null], (string) json_encode($r));

        return $r;
    }

    /**
     * A person's undo as the control plane sends it.
     *
     * @return array<string,mixed>
     */
    private function undo(string $requestId, string $hash): array
    {
        return $this->callP($this->pRevert($requestId, (string) json_encode(['snapshot_sha256' => $hash])));
    }

    /** The page-edit entry as the control plane sends it. */
    private function entry(bool $enabled = true): string
    {
        return (string) json_encode([
            'name'          => OwnAbilities::NAME_PAGE_EDIT,
            'source'        => 'wpmgr',
            'class'         => 'write',
            'status'        => 'admitted',
            'enabled'       => $enabled,
            'approval_mode' => 'per_call',
            'snapshot'      => 'builder_document',
            'limits'        => ['builders_enabled' => ['elementor']],
        ]);
    }

    /**
     * p for a precheck or a write of DRAFT, as the control plane builds it.
     *
     * @param array<string,mixed>|null $expected The precheck answer, for a write.
     */
    private function p(string $mode, string $requestId, string $input, ?array $expected = null): string
    {
        $entry = $this->entry();
        $p     = ['mode' => $mode, 'request_id' => $requestId, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input, 'allowed_draft_ids' => [self::DRAFT]];
        if ($expected !== null) {
            $p['expected'] = ['precheck_digest' => $expected['precheck_digest'], 'preview_digest' => $expected['preview_digest']];
        }

        return (string) json_encode($p);
    }

    /**
     * p for a revert: $revertJson is p.revert's exact text (null leaves it out).
     *
     * @param list<int>|null $allowed A draft list; null leaves it out.
     */
    private function pRevert(string $requestId, ?string $revertJson, string $input = '{}', ?array $allowed = null, bool $enabled = true): string
    {
        $entry = $this->entry($enabled);
        $p     = ['mode' => 'revert', 'request_id' => $requestId, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input];
        if ($allowed !== null) {
            $p['allowed_draft_ids'] = $allowed;
        }
        $text = (string) json_encode($p);

        return $revertJson === null ? $text : substr($text, 0, -1) . ',"revert":' . $revertJson . '}';
    }

    /** DRAFT's builder_document_v1 fingerprint now. */
    private function fp(): string
    {
        return (string) BuilderDocumentFingerprint::ofPost(self::DRAFT, ElementorDocument::DESCRIPTOR_KEYS);
    }

    /**
     * Everything an undo could write for a post: its meta rows as stored,
     * its posts row, and every ledger row and snapshot.
     *
     * @return array<string,mixed>
     */
    private function state(int $postId): array
    {
        $ledger = array_filter($this->options, static fn ($k): bool => str_starts_with((string) $k, 'wpmgr_ability_ledger_'), ARRAY_FILTER_USE_KEY);

        return [
            'meta'      => $this->rows->metaRowsOf($postId),
            'post'      => $this->rows->postRow($postId),
            'ledger'    => $ledger,
            'snapshots' => array_map(static fn (array $r): array => [$r['option_name'], $r['option_value']], $this->rows->optionRows()),
        ];
    }

    /**
     * A post's meta rows as stored and its posts row.
     *
     * @return array<string,mixed>
     */
    private function rowsOfPost(int $postId): array
    {
        return ['meta' => $this->rows->metaRowsOf($postId), 'post' => $this->rows->postRow($postId)];
    }

    /**
     * A post's stored meta values by key, keys sorted, each key's rows in
     * meta_id order.
     *
     * @return array<string, list<string|null>>
     */
    private function byKey(int $postId): array
    {
        $out = [];
        foreach ($this->rows->metaRowsOf($postId) as $row) {
            $out[$row['meta_key']][] = $row['meta_value'];
        }
        ksort($out, SORT_STRING);

        return $out;
    }

    /** The stored snapshot text of a request. */
    private function snapshotBytes(string $requestId): string
    {
        $value = $this->optionValue(BuilderDocumentSnapshot::OPTION_PREFIX . $requestId);
        $this->assertIsString($value, 'the snapshot is stored');

        return $value;
    }

    private function optionValue(string $name): ?string
    {
        foreach ($this->rows->optionRows() as $row) {
            if ($row['option_name'] === $name) {
                return $row['option_value'];
            }
        }

        return null;
    }

    /**
     * The stored snapshot text with DRAFT's _elementor_data replaced by
     * FORGED_DATA: still a well-formed snapshot of this request and post.
     */
    private function forgedSnapshot(string $bytes): string
    {
        $doc = json_decode($bytes, true, 512, JSON_THROW_ON_ERROR);
        foreach ($doc['meta'] as $i => [$key]) {
            if ($key === ElementorDocument::KEY_DATA) {
                $doc['meta'][$i][1] = base64_encode(self::FORGED_DATA);
            }
        }

        return (string) json_encode($doc);
    }

    /**
     * An undo of EDIT with $signed is snapshot_tampered, and writes nothing:
     * no row, no ledger change, no cache dropped, no claim left.
     */
    private function assertTamperedAndNothingWritten(string $why, string $signed): void
    {
        $state = $this->state(self::DRAFT);
        $r     = $this->undo(self::EDIT, $signed);
        $this->assertSame(['snapshot_tampered', false], [$r['code'] ?? null, $r['ok'] ?? null], $why . ': ' . json_encode($r));
        $this->assertSame($state, $this->state(self::DRAFT), $why . ': nothing written');
        $this->assertSame([], $this->wpdb->claims, $why . ': the claims are released');
        $this->assertSame([], $this->api->callsTo('deletePostCss'), $why . ': no cache of the page dropped');
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
}
