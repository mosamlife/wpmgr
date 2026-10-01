<?php
/**
 * E2: content_editing_enable and ability_run write/revert for
 * wpmgr/page-create, driven through the real signed Router dispatch.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\ServicePrincipal;
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
 * @covers \WPMgr\Agent\Commands\ContentEditingEnableCommand
 * @covers \WPMgr\Agent\Abilities\ServicePrincipal
 * @covers \WPMgr\Agent\Abilities\AbilityLedger
 * @covers \WPMgr\Agent\Abilities\PageCreateBuilder
 */
final class PageCreateWriteTest extends TestCase
{
    private const REQ_A = '11111111-2222-4333-8444-555555555555';
    private const REQ_B = '99999999-2222-4333-8444-555555555555';

    private string $keyFile;

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

    /** @var list<string> Caps a hostile filter grants to everyone. */
    private array $filterGrants = [];

    /** @var list<array{0:string,1:mixed,2:int}> */
    private array $filters = [];

    /** Whether the fake wp_insert_post alters content (a sanitiser plugin). */
    private bool $mangleOnInsert = false;

    private bool $blockEditor = true;

    /** @var array<int,int> post id => author of an autosave revision. */
    private array $autosaves = [];

    /** @var array<int,int> post id => number of revisions. */
    private array $revisions = [];

    /** @var array<int,int> post id => user holding the edit lock. */
    private array $locks = [];

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-e2-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

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
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('register_rest_route')->justReturn(true);
        Functions\when('add_filter')->alias(function ($name, $cb, $prio = 10) {
            $this->filters[] = [(string) $name, $cb, (int) $prio];
            return true;
        });
        Functions\when('remove_filter')->justReturn(true);
        $GLOBALS['wp_version'] = '6.4.2';

        // Roles and users.
        Functions\when('get_role')->alias(fn ($r) => $this->roles[$r] ?? null);
        Functions\when('add_role')->alias(function ($r, $label, $caps) {
            if (isset($this->roles[$r])) {
                return null;
            }
            $o               = new \stdClass();
            $o->name         = $r;
            $o->capabilities = $caps;
            $this->roles[$r] = $o;
            return $o;
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
            $id              = $this->nextId++;
            $u               = new \stdClass();
            $u->ID           = $id;
            $u->user_login   = $data['user_login'];
            $u->roles        = [$data['role']];
            $u->caps         = [$data['role'] => true];
            $this->users[$id] = $u;
            return $id;
        });
        Functions\when('update_user_meta')->justReturn(true);
        Functions\when('wp_set_current_user')->alias(function ($id) {
            $this->currentUser = (int) $id;
            return null;
        });
        Functions\when('current_user_can')->alias(function ($cap) {
            if (in_array($cap, $this->filterGrants, true)) {
                return true;
            }
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

        // Posts.
        Functions\when('wp_slash')->alias(fn ($v) => is_array($v) ? array_map(fn ($x) => is_string($x) ? addslashes($x) : $x, $v) : (is_string($v) ? addslashes($v) : $v));
        Functions\when('wp_kses_post')->returnArg();
        Functions\when('use_block_editor_for_post_type')->alias(fn () => $this->blockEditor);
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('get_post')->alias(fn ($id) => $this->posts[(int) $id] ?? null);
        Functions\when('get_post_meta')->alias(fn ($id, $key = '', $single = false) => $this->meta[(int) $id][$key] ?? '');
        Functions\when('wp_insert_post')->alias(function (array $data, $wpError = false) {
            $id                  = $this->nextId++;
            $p                   = new \stdClass();
            $p->ID               = $id;
            $p->post_type        = $data['post_type'];
            $p->post_status      = $data['post_status'];
            $p->post_author      = (int) $data['post_author'];
            $p->post_title       = stripslashes($data['post_title']);
            $p->post_content     = stripslashes($data['post_content']) . ($this->mangleOnInsert ? ' ' : '');
            $p->post_excerpt     = '';
            $p->post_name        = '';
            $p->post_password    = '';
            $p->post_modified_gmt = '2026-10-01 10:00:00';
            $this->posts[$id]    = $p;
            foreach ((array) ($data['meta_input'] ?? []) as $k => $v) {
                $this->meta[$id][$k] = stripslashes((string) $v);
            }
            return $id;
        });
        Functions\when('wp_get_post_autosave')->alias(function ($id, $user = 0) {
            if (!isset($this->autosaves[(int) $id])) {
                return false;
            }
            // Core: user id int 0 means any user's autosave.
            if ($user !== 0 && $this->autosaves[(int) $id] !== (int) $user) {
                return false;
            }
            $r            = new \stdClass();
            $r->post_type = 'revision';
            return $r;
        });
        Functions\when('wp_get_post_revisions')->alias(fn ($id, $args = null) => array_fill(0, $this->revisions[(int) $id] ?? 0, new \stdClass()));
        Functions\when('wp_check_post_lock')->alias(fn ($id) => $this->locks[(int) $id] ?? false);
        Functions\when('wp_trash_post')->alias(function ($id) {
            if (!isset($this->posts[(int) $id])) {
                return false;
            }
            $this->posts[(int) $id]->post_status = 'trash';
            return $this->posts[(int) $id];
        });

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $test            = $this;
        $GLOBALS['wpdb'] = new class ($test) {
            public string $prefix = 'wp_';
            public string $options = 'wp_options';
            public string $last_error = '';

            /** @var PageCreateWriteTest */
            private $t;

            public function __construct($t)
            {
                $this->t = $t;
            }

            public function prepare(string $q, ...$args): string
            {
                return (string) json_encode([$q, $args]);
            }

            public function get_var(string $q): ?string
            {
                [$sql, $args] = json_decode($q, true) ?? [$q, []];
                if (is_string($sql) && str_starts_with($sql, 'SELECT option_id FROM')) {
                    return $this->t->optionExists((string) $args[0]) ? '1' : null;
                }
                return null;
            }

            public function query(string $q): int
            {
                [$sql, $args] = json_decode($q, true) ?? [$q, []];
                if (is_string($sql) && str_starts_with($sql, 'INSERT IGNORE INTO')) {
                    return $this->t->insertIgnore((string) $args[0], (string) $args[1]);
                }
                return 0;
            }

            /** @param array<string,mixed> $row */
            public function insert(string $t, array $row, $f = null): int
            {
                return 1;
            }
        };
        $this->resetShieldStash();

        $this->router = new Router(
            new Connector($keystore, new Settings()),
            [new AbilityRunCommand(), new ContentEditingEnableCommand()]
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

    /** Fake-wpdb hook: does an options row exist. */
    public function optionExists(string $name): bool
    {
        return array_key_exists($name, $this->options);
    }

    /** Fake-wpdb hook: INSERT IGNORE semantics on the unique option_name. */
    public function insertIgnore(string $name, string $value): int
    {
        if (array_key_exists($name, $this->options)) {
            return 0;
        }
        $this->options[$name] = $value;
        return 1;
    }

    // -------------------------------------------------------------------------
    // Enable and the principal
    // -------------------------------------------------------------------------

    public function test_enable_creates_role_and_user_with_exact_caps_and_is_idempotent(): void
    {
        $r = $this->enable();
        $this->assertTrue($r['ok'], (string) json_encode($r));
        $this->assertSame('enabled', $r['outcome']);
        $id = (int) $r['user_id'];
        $this->assertGreaterThan(0, $id);

        $caps = array_keys(array_filter($this->roles[ServicePrincipal::ROLE]->capabilities));
        sort($caps);
        $this->assertSame(
            ['edit_others_pages', 'edit_others_posts', 'edit_pages', 'edit_posts', 'edit_published_pages', 'edit_published_posts', 'read'],
            $caps
        );
        foreach (['unfiltered_html', 'manage_options', 'edit_users', 'install_plugins', 'edit_files', 'upload_files', 'publish_posts', 'publish_pages', 'delete_posts', 'delete_pages'] as $bad) {
            $this->assertArrayNotHasKey($bad, $this->roles[ServicePrincipal::ROLE]->capabilities);
        }
        $this->assertSame([ServicePrincipal::ROLE], $this->users[$id]->roles);

        $again = $this->enable();
        $this->assertTrue($again['ok']);
        $this->assertSame('already_enabled', $again['outcome']);
        $this->assertSame($id, $again['user_id']);
        $this->assertCount(1, $this->users, 'a second enable must not create a second user');
    }

    public function test_enable_refuses_params(): void
    {
        $r = $this->post('content_editing_enable', (string) json_encode(['role' => 'administrator']), null);
        $this->assertFalse($r['ok']);
        $this->assertSame('bad_params', $r['code']);
        $this->assertSame([], $this->users);
    }

    public function test_the_service_user_cannot_authenticate(): void
    {
        $id   = (int) $this->enable()['user_id'];
        $user = $this->users[$id];

        $this->assertInstanceOf(\WP_Error::class, ServicePrincipal::refuseAuthenticate($user, 'wpmgr-content-agent', 'x'));
        $this->assertInstanceOf(\WP_Error::class, ServicePrincipal::refuseAuthenticate(null, 'WPMGR-content-agent', 'x'), 'refused by login name before any password check');
        $this->assertInstanceOf(\WP_Error::class, ServicePrincipal::refuseAuthenticateUser($user, 'x'));
        $this->assertFalse(ServicePrincipal::refuseAppPasswords(true, $user));
        $this->assertFalse(ServicePrincipal::refusePasswordReset(true, $id));

        $other     = new \stdClass();
        $other->ID = 1;
        $other->user_login = 'admin';
        $this->assertSame($other, ServicePrincipal::refuseAuthenticate($other, 'admin', 'x'), 'other users are untouched');

        ServicePrincipal::register();
        $hooked = array_map(static fn ($f) => $f[0] . '@' . $f[2], $this->filters);
        $this->assertContains('authenticate@' . PHP_INT_MAX, $hooked);
        $this->assertContains('wp_authenticate_user@' . PHP_INT_MAX, $hooked);
    }

    // -------------------------------------------------------------------------
    // Write
    // -------------------------------------------------------------------------

    public function test_write_without_enable_is_refused(): void
    {
        $r = $this->write(self::REQ_A, $this->input(), str_repeat('a', 64), str_repeat('b', 64));
        $this->assertFalse($r['ok']);
        $this->assertSame('content_editing_not_enabled', $r['code']);
        $this->assertSame([], $this->posts);
    }

    public function test_write_creates_a_draft_with_exactly_the_built_content(): void
    {
        $uid = (int) $this->enable()['user_id'];
        $pre = $this->precheck(self::REQ_A, $this->input());
        $this->assertTrue($pre['ok'], (string) json_encode($pre));

        $r = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertTrue($r['ok'], (string) json_encode($r));
        $this->assertSame('created', $r['outcome']);

        $post     = $this->posts[$r['post_id']];
        $expected = "<!-- wp:heading -->\n<h2 class=\"wp-block-heading\">Our hours &amp; prices</h2>\n<!-- /wp:heading -->\n\n"
            . "<!-- wp:paragraph -->\n<p>Open daily. Café \"Ünïcode\" it's fine.</p>\n<!-- /wp:paragraph -->\n\n"
            . "<!-- wp:list {\"ordered\":true} -->\n<ol class=\"wp-block-list\"><!-- wp:list-item -->\n<li>One</li>\n<!-- /wp:list-item --><!-- wp:list-item -->\n<li>Two</li>\n<!-- /wp:list-item --></ol>\n<!-- /wp:list -->";
        $this->assertSame($expected, $post->post_content);
        $this->assertSame($pre['preview']['content'], $post->post_content);
        $this->assertSame('draft', $post->post_status);
        $this->assertSame('page', $post->post_type);
        $this->assertSame($uid, $post->post_author);
        $this->assertSame('About us', $post->post_title);
        $this->assertSame(self::REQ_A, $this->meta[$r['post_id']][AbilityRunCommand::META_CREATED_BY]);
        $this->assertSame(0, $this->currentUser, 'the user is switched back to 0');

        $ledger = AbilityLedger::get(self::REQ_A);
        $this->assertSame('completed', $ledger['phase']);
        $this->assertSame($r['post_id'], $ledger['created_post_id']);
        $this->assertFalse(AbilityLedger::inflight(self::REQ_A), 'the claim is released');
    }

    public function test_classic_editor_builds_plain_html(): void
    {
        $this->enable();
        $this->blockEditor = false;
        $input             = $this->input('wordpress_classic');
        $pre               = $this->precheck(self::REQ_A, $input);
        $r                 = $this->write(self::REQ_A, $input, $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertTrue($r['ok'], (string) json_encode($r));
        $this->assertSame(
            "<h2>Our hours &amp; prices</h2>\n\n<p>Open daily. Café \"Ünïcode\" it's fine.</p>\n\n<ol>\n<li>One</li>\n<li>Two</li>\n</ol>",
            $this->posts[$r['post_id']]->post_content
        );

        $wrongEditor = $this->precheck(self::REQ_B, $this->input('wordpress_blocks'));
        $this->assertSame('editor_unavailable', $wrongEditor['code']);
    }

    public function test_a_replay_returns_already_applied_with_no_duplicate(): void
    {
        $this->enable();
        $pre   = $this->precheck(self::REQ_A, $this->input());
        $first = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('created', $first['outcome']);

        $replay = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);

        $this->assertTrue($replay['ok'], (string) json_encode($replay));
        $this->assertSame('already_applied', $replay['outcome']);
        $this->assertSame($first['post_id'], $replay['post_id']);
        $this->assertCount(1, $this->posts, 'a replay must not create a second draft');
    }

    public function test_forbidden_sequences_and_markup_in_text_are_refused(): void
    {
        $this->enable();
        $bad = [
            '<b>bold</b>', '[gallery]', '{{ x }}', '{% if %}', 'a <!-- c', 'x > y', "two\nlines",
            "rtl\u{202E}override", '&amp;', '&#60;script', 'tick`',
        ];
        foreach ($bad as $text) {
            $input = (string) json_encode([
                'post_type' => 'page', 'editor' => 'wordpress_blocks', 'title' => 'T',
                'outline'   => [['type' => 'paragraph', 'text' => $text]],
            ]);
            $r = $this->precheck(self::REQ_A, $input);
            $this->assertFalse($r['ok'], 'must refuse: ' . $text);
            $this->assertSame('create_content_invalid', $r['code'], $text);
        }
        $unknownNode = (string) json_encode([
            'post_type' => 'page', 'editor' => 'wordpress_blocks', 'title' => 'T',
            'outline'   => [['type' => 'html', 'text' => 'x']],
        ]);
        $this->assertSame('create_content_invalid', $this->precheck(self::REQ_A, $unknownNode)['code']);
        $badTitle = (string) json_encode([
            'post_type' => 'page', 'editor' => 'wordpress_blocks', 'title' => '<script>',
            'outline'   => [['type' => 'paragraph', 'text' => 'x']],
        ]);
        $this->assertSame('create_content_invalid', $this->precheck(self::REQ_A, $badTitle)['code']);
        $this->assertSame([], $this->posts);
    }

    public function test_a_pd_mismatch_is_refused(): void
    {
        $this->enable();
        $pre = $this->precheck(self::REQ_A, $this->input());
        $p   = $this->p('write', self::REQ_A, $this->input(), ['precheck_digest' => $pre['precheck_digest'], 'preview_digest' => $pre['preview_digest']]);

        $r = $this->call($p, hash('sha256', $p . ' '));

        $this->assertSame('token_params_mismatch', $r['code']);
        $this->assertSame([], $this->posts);
    }

    public function test_a_digest_that_does_not_match_the_build_is_refused(): void
    {
        $this->enable();
        $pre = $this->precheck(self::REQ_A, $this->input());

        $r = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], str_repeat('0', 64));

        $this->assertSame('preview_changed', $r['code']);
        $this->assertSame([], $this->posts);
        $this->assertNull(AbilityLedger::get(self::REQ_A));
    }

    public function test_a_stored_content_mismatch_trashes_the_draft(): void
    {
        $this->enable();
        $pre                  = $this->precheck(self::REQ_A, $this->input());
        $this->mangleOnInsert = true;

        $r = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);

        $this->assertSame('verify_mismatch', $r['code']);
        $this->assertTrue($r['trashed']);
        $this->assertSame('trash', $this->posts[$r['post_id']]->post_status);
    }

    public function test_a_write_entry_needs_per_call_approval_and_a_snapshot(): void
    {
        $this->enable();
        $noApproval = $this->write(self::REQ_A, $this->input(), str_repeat('a', 64), str_repeat('b', 64), ['approval_mode' => 'none']);
        $this->assertSame('entry_approval_invalid', $noApproval['code']);
        $noSnapshot = $this->write(self::REQ_A, $this->input(), str_repeat('a', 64), str_repeat('b', 64), ['snapshot' => 'none']);
        $this->assertSame('snapshot_strategy_invalid', $noSnapshot['code']);
        $readClass = $this->write(self::REQ_A, $this->input(), str_repeat('a', 64), str_repeat('b', 64), ['class' => 'read']);
        $this->assertSame('mode_class_mismatch', $readClass['code']);
    }

    public function test_cap_drift_is_refused(): void
    {
        $this->enable();
        $pre = $this->precheck(self::REQ_A, $this->input());

        $this->roles[ServicePrincipal::ROLE]->capabilities['publish_pages'] = true;
        $r = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('principal_capabilities_drifted', $r['code']);
        unset($this->roles[ServicePrincipal::ROLE]->capabilities['publish_pages']);

        $this->filterGrants = ['unfiltered_html'];
        $r2 = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('principal_capabilities_drifted', $r2['code'], 'a filter-granted forbidden cap is drift too');
        $this->assertSame(0, $this->currentUser);
        $this->assertSame([], $this->posts);
    }

    public function test_a_second_claim_on_the_same_key_fails(): void
    {
        $this->assertTrue(AbilityLedger::claimRequest(self::REQ_A));
        $this->assertFalse(AbilityLedger::claimRequest(self::REQ_A));
        $this->assertTrue(AbilityLedger::inflight(self::REQ_A));
        $this->assertTrue(AbilityLedger::claimTarget(42));
        $this->assertFalse(AbilityLedger::claimTarget(42));
        AbilityLedger::releaseRequest(self::REQ_A);
        $this->assertTrue(AbilityLedger::claimRequest(self::REQ_A), 'a released claim can be taken again');
    }

    public function test_a_write_while_the_request_is_claimed_is_refused(): void
    {
        $this->enable();
        $pre = $this->precheck(self::REQ_A, $this->input());
        $this->assertTrue(AbilityLedger::claimRequest(self::REQ_A));

        $r = $this->write(self::REQ_A, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);

        $this->assertSame('request_in_flight', $r['code']);
        $this->assertSame([], $this->posts);
    }

    // -------------------------------------------------------------------------
    // Revert
    // -------------------------------------------------------------------------

    public function test_revert_trashes_the_draft(): void
    {
        $id = $this->createOne(self::REQ_A);

        $r = $this->revert(self::REQ_A);

        $this->assertTrue($r['ok'], (string) json_encode($r));
        $this->assertSame('reverted', $r['outcome']);
        $this->assertSame('trash', $this->posts[$id]->post_status);
        $this->assertSame('already_reverted', $this->revert(self::REQ_A)['outcome']);
        $this->assertSame(0, $this->currentUser);
    }

    public function test_revert_of_a_draft_changed_after_creation_is_refused(): void
    {
        $id                             = $this->createOne(self::REQ_A);
        $this->posts[$id]->post_content .= ' edited by a person';

        $r = $this->revert(self::REQ_A);

        $this->assertSame('conflict', $r['code']);
        $this->assertSame('draft', $this->posts[$id]->post_status);
    }

    public function test_revert_of_a_draft_with_another_users_autosave_is_refused(): void
    {
        $id                   = $this->createOne(self::REQ_A);
        $this->autosaves[$id] = 1;

        $r = $this->revert(self::REQ_A);

        $this->assertFalse($r['ok']);
        $this->assertSame('created_post_touched', $r['code'], (string) json_encode($r));
        $this->assertSame('draft', $this->posts[$id]->post_status);
        $this->assertSame(0, $this->currentUser);
    }

    public function test_revert_of_a_draft_with_a_revision_is_refused(): void
    {
        $id                   = $this->createOne(self::REQ_A);
        $this->revisions[$id] = 1;

        $this->assertSame('created_post_touched', $this->revert(self::REQ_A)['code']);
        $this->assertSame('draft', $this->posts[$id]->post_status);
    }

    public function test_revert_of_a_draft_someone_has_open_is_refused(): void
    {
        $id               = $this->createOne(self::REQ_A);
        $this->locks[$id] = 1;

        $this->assertSame('created_post_touched', $this->revert(self::REQ_A)['code']);
        $this->assertSame('draft', $this->posts[$id]->post_status);

        unset($this->locks[$id]);
        $this->assertSame('reverted', $this->revert(self::REQ_A)['outcome'], 'once untouched again, undo proceeds');
        $this->assertSame('trash', $this->posts[$id]->post_status);
    }

    public function test_the_service_user_is_never_the_current_user_of_a_request(): void
    {
        $id = (int) $this->enable()['user_id'];

        $this->assertSame(0, ServicePrincipal::refuseCurrentUser($id));
        $this->assertSame(0, ServicePrincipal::refuseCurrentUser((string) $id));
        $this->assertSame(1, ServicePrincipal::refuseCurrentUser(1), 'other users are untouched');
        $this->assertFalse(ServicePrincipal::refuseCurrentUser(false), 'no user stays no user');

        ServicePrincipal::register();
        $hooked = array_map(static fn ($f) => $f[0] . '@' . $f[2], $this->filters);
        $this->assertContains('determine_current_user@' . PHP_INT_MAX, $hooked);

        // The engine's in-process switch does not go through the filter, so a
        // write and a revert still run as the principal past the live-drift check.
        $post = $this->createOne(self::REQ_A);
        $this->assertSame((string) $id, (string) $this->posts[$post]->post_author);
        $this->assertSame('reverted', $this->revert(self::REQ_A)['outcome']);
        $this->assertSame(0, $this->currentUser);
    }

    public function test_revert_of_a_published_post_is_refused(): void
    {
        $id                            = $this->createOne(self::REQ_A);
        $this->posts[$id]->post_status = 'publish';

        $this->assertSame('created_post_published', $this->revert(self::REQ_A)['code']);
        $this->assertSame('publish', $this->posts[$id]->post_status);
    }

    public function test_revert_takes_the_id_only_from_the_ledger(): void
    {
        $id = $this->createOne(self::REQ_A);
        // An unrelated draft the attacker would like trashed, even marked as ours.
        $this->posts[7] = clone $this->posts[$id];
        $this->posts[7]->ID = 7;
        $this->meta[7][AbilityRunCommand::META_CREATED_BY] = self::REQ_B;

        $withInput = $this->revert(self::REQ_A, (string) json_encode(['post_id' => 7]));
        $this->assertSame('bad_input', $withInput['code']);

        $noRow = $this->revert(self::REQ_B);
        $this->assertSame('nothing_to_revert', $noRow['code'], 'post meta is never a source of the id');

        $this->assertSame('draft', $this->posts[7]->post_status);
        $this->assertSame('draft', $this->posts[$id]->post_status);
    }

    public function test_ledger_mode_reports_the_row(): void
    {
        $id = $this->createOne(self::REQ_A);
        $p  = (string) json_encode(['mode' => 'ledger', 'request_id' => self::REQ_A]);
        $r  = $this->callP($p);

        $this->assertTrue($r['found']);
        $this->assertSame('completed', $r['phase']);
        $this->assertSame($id, $r['created_post_id']);
        $this->assertFalse($r['inflight']);
    }

    public function test_go_fixture_replays_precheck_write_and_revert_byte_for_byte(): void
    {
        $f            = self::fixture();
        $this->siteId = (string) $f['site_id'];
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;
        $this->enable();

        // Precheck: Go's exact p, never re-encoded; pd is over these bytes.
        $this->assertSame(hash('sha256', (string) $f['entry']), $f['entry_sha256'], 'the fixture entry hashes to its entry_sha256');
        $this->assertStringContainsString('"entry_sha256":"' . $f['entry_sha256'] . '"', (string) $f['precheck']['p']);
        $pre = $this->callP((string) $f['precheck']['p']);
        $this->assertTrue($pre['ok'] ?? false, (string) json_encode($pre));
        $this->assertSame('prechecked', $pre['outcome']);

        // The stand-in digests are refused, and nothing is created.
        $standIn = (string) $f['write']['p'];
        $stale   = $this->callP($standIn);
        $this->assertFalse($stale['ok'] ?? true);
        $this->assertSame('preview_changed', $stale['code'] ?? null, (string) json_encode($stale));
        $this->assertSame([], $this->posts);

        // Write: substitute the real digests as plain text, keeping Go's encoding.
        $oldPre  = (string) $f['write']['expected']['precheck_digest'];
        $oldPrev = (string) $f['write']['expected']['preview_digest'];
        $this->assertSame(1, substr_count($standIn, $oldPre));
        $this->assertSame(1, substr_count($standIn, $oldPrev));
        $writeP = str_replace([$oldPre, $oldPrev], [(string) $pre['precheck_digest'], (string) $pre['preview_digest']], $standIn);
        $w      = $this->callP($writeP);
        $this->assertTrue($w['ok'] ?? false, (string) json_encode($w));
        $this->assertSame('created', $w['outcome']);
        $id = (int) $w['post_id'];
        $this->assertSame('draft', $this->posts[$id]->post_status);
        $this->assertSame('page', $this->posts[$id]->post_type);
        $this->assertSame('Café launch 🚀', $this->posts[$id]->post_title);

        // Revert: Go's exact p; the post id comes from the ledger.
        $r = $this->callP((string) $f['revert']['p']);
        $this->assertTrue($r['ok'] ?? false, (string) json_encode($r));
        $this->assertSame('trash', $this->posts[$id]->post_status);
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function createOne(string $rid): int
    {
        $this->enable();
        $pre = $this->precheck($rid, $this->input());
        $r   = $this->write($rid, $this->input(), $pre['precheck_digest'], $pre['preview_digest']);
        $this->assertSame('created', $r['outcome'], (string) json_encode($r));

        return (int) $r['post_id'];
    }

    private function input(string $editor = 'wordpress_blocks'): string
    {
        return (string) json_encode([
            'post_type' => 'page',
            'editor'    => $editor,
            'title'     => 'About us',
            'outline'   => [
                ['type' => 'heading', 'level' => 2, 'text' => 'Our hours & prices'],
                ['type' => 'paragraph', 'text' => 'Open daily. Café "Ünïcode" it\'s fine.'],
                ['type' => 'list', 'ordered' => true, 'items' => ['One', 'Two']],
            ],
        ], JSON_UNESCAPED_UNICODE);
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
    private function precheck(string $rid, string $input): array
    {
        return $this->callP($this->p('precheck', $rid, $input));
    }

    /**
     * @param array<string,mixed> $entryOverrides
     * @return array<string,mixed>
     */
    private function write(string $rid, string $input, string $pre, string $prev, array $entryOverrides = []): array
    {
        return $this->callP($this->p('write', $rid, $input, ['precheck_digest' => $pre, 'preview_digest' => $prev], $entryOverrides));
    }

    /**
     * @return array<string,mixed>
     */
    private function revert(string $rid, ?string $input = null): array
    {
        $entry = $this->entry();
        $p     = ['mode' => 'revert', 'request_id' => $rid, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry)];
        if ($input !== null) {
            $p['input'] = $input;
        }

        return $this->callP((string) json_encode($p));
    }

    /**
     * @param array<string,mixed> $overrides
     */
    private function entry(array $overrides = []): string
    {
        // Built from Go's own entry, so the field names are the wire's names.
        $base = json_decode((string) self::fixture()['entry'], true);
        self::assertIsArray($base);

        return (string) json_encode(array_merge($base, $overrides));
    }

    /**
     * The golden fixture Go writes (TestPageCreateFixture). Never hand-edited.
     *
     * @return array<string,mixed>
     */
    private static function fixture(): array
    {
        $raw = file_get_contents(__DIR__ . '/fixtures/ability-run/page-create.json');
        self::assertIsString($raw, 'the Go page-create fixture is missing');
        $f = json_decode($raw, true);
        self::assertIsArray($f, 'the Go page-create fixture is not JSON');

        return $f;
    }

    /**
     * @param array<string,string>|null $expected
     * @param array<string,mixed>       $entryOverrides
     */
    private function p(string $mode, string $rid, string $input, ?array $expected = null, array $entryOverrides = []): string
    {
        $entry = $this->entry($entryOverrides);
        $p     = [
            'mode'         => $mode,
            'request_id'   => $rid,
            'entry'        => $entry,
            'entry_sha256' => hash('sha256', $entry),
            'input'        => $input,
        ];
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
        return $this->call($p, hash('sha256', $p));
    }

    /**
     * @return array<string,mixed>
     */
    private function call(string $p, ?string $pd): array
    {
        return $this->post('ability_run', (string) json_encode(['p' => $p]), $pd);
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
        $claims = [
            'aud' => $this->siteId,
            'cmd' => $cmd,
            'jti' => bin2hex(random_bytes(8)),
            'exp' => time() + 30,
        ];
        if ($pd !== null) {
            $claims['pd'] = $pd;
        }
        $segments   = [
            $this->b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])),
            $this->b64((string) json_encode($claims)),
        ];
        $segments[] = $this->b64(sodium_crypto_sign_detached(implode('.', $segments), $this->cpSecret));

        return implode('.', $segments);
    }

    private function b64(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    private function resetShieldStash(): void
    {
        $prop = new ReflectionProperty(AuthHeaderShield::class, 'stashedBearer');
        $prop->setValue(null, null);
    }
}
