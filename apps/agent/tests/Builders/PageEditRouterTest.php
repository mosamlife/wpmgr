<?php
/**
 * wpmgr/page-edit driven through the real signed Router dispatch: the
 * catalogue entry and its snapshot strategy, p.allowed_draft_ids, the
 * precheck and its digests, the write, its replay and the ledger mode.
 *
 * Elementor is FakeElementorApi behind the real ElementorAdapter, handed to
 * the command through its test-only adapter seam; the draft's document
 * stores the elements as Elementor's save does. Post, meta and snapshot rows
 * live in FakeBuilderWpdb behind EngineWpdb, which keeps the ledger's claim
 * rows; the ledger rows are this test's options.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   page-edit-preview.json  the input, the page before, the precheck's
 *                           preview, its digests and the tree's bytes
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
 * @covers \WPMgr\Agent\Abilities\OwnAbilities
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageEdit
 */
final class PageEditRouterTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    private const PREVIEW_FIXTURE = __DIR__ . '/../fixtures/ability-run/page-edit-preview.json';

    /** The draft WPMgr created. */
    private const DRAFT = 120;

    private const KIT = 4;

    /** The page-create request that created DRAFT. */
    private const CREATE = '11111111-2222-4333-8444-777777777777';

    /** The page-edit request. */
    private const EDIT = '44444444-2222-4333-8444-666666666666';

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

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-bfe-edit-' . bin2hex(random_bytes(8)) . '.key';
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
        Functions\when('get_post_meta')->justReturn('');
        Functions\when('wp_get_post_autosave')->justReturn(false);
        Functions\when('wp_check_post_lock')->justReturn(false);
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
        $this->rows->addPost(self::DRAFT, [
            'post_type'         => 'page',
            'post_status'       => 'draft',
            'post_title'        => 'Our services',
            'post_content'      => 'Left column Right column',
            'post_modified_gmt' => '2026-10-09 07:00:05',
        ]);
        $this->rows->addMeta(11, self::DRAFT, DraftEligibility::MARKER_KEY, self::CREATE);
        $this->rows->addMeta(12, self::DRAFT, ElementorDocument::KEY_EDIT_MODE, 'builder');
        $this->rows->addMeta(13, self::DRAFT, ElementorDocument::KEY_TEMPLATE_TYPE, 'wp-page');
        $this->rows->addMeta(14, self::DRAFT, ElementorDocument::KEY_DATA, (string) json_encode(self::page()));
        $this->rows->addPost(self::KIT, ['post_type' => 'elementor_library', 'post_status' => 'publish', 'post_title' => 'Kit', 'post_content' => '', 'post_modified_gmt' => '2026-10-01 07:00:00']);
        $this->rows->addMeta(2, self::KIT, ElementorDocument::KEY_PAGE_SETTINGS, 'a:1:{s:13:"system_colors";a:0:{}}');
        $this->options['wpmgr_ability_ledger_' . self::CREATE] = [
            'request_id'      => self::CREATE,
            'ability'         => OwnAbilities::NAME_PAGE_CREATE,
            'phase'           => 'completed',
            'created_post_id' => self::DRAFT,
            'undo_state'      => 'available',
            'builder'         => 'elementor',
        ];

        $this->api              = new FakeElementorApi();
        $this->api->activeKitId = self::KIT;
        $this->api->storingDocument(self::DRAFT, $this->rows);
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

    public function test_precheck_write_and_ledger_through_the_router(): void
    {
        $this->enable();
        $input = $this->input();
        $pre   = $this->callP($this->p('precheck', $input, [self::DRAFT]));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $this->assertSame(['prechecked', 'precheck', OwnAbilities::NAME_PAGE_EDIT, self::EDIT, true], [$pre['outcome'], $pre['mode'], $pre['ability'], $pre['request_id'], $pre['valid']]);
        $fp = BuilderDocumentFingerprint::ofPost(self::DRAFT, ElementorDocument::DESCRIPTOR_KEYS);
        $this->assertSame($fp, $pre['base_fingerprint']);
        $entry = $this->entry();
        $this->assertSame(
            hash('sha256', (string) json_encode([hash('sha256', $entry), hash('sha256', $input), $pre['base_fingerprint'], $pre['preview_digest']])),
            $pre['precheck_digest'],
            'the precheck digest binds the entry, the input bytes, the base and the preview'
        );
        $this->assertSame([], $this->rows->optionRows(), 'a precheck stores nothing');
        $this->assertNull(AbilityLedger::get(self::EDIT));

        $r = $this->callP($this->p('write', $input, [self::DRAFT], $pre));
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame(['applied', self::DRAFT, $fp, $pre['preview_digest']], [$r['outcome'], $r['post_id'], $r['before_fp'], $r['preview_digest']]);
        $stored = BuilderDocumentFingerprint::read(self::DRAFT, [ElementorDocument::KEY_DATA]);
        $this->assertSame($pre['preview']['tree'], json_decode($stored['rows'][ElementorDocument::KEY_DATA][0], true));
        $this->assertSame(0, $this->currentUser, 'the user is switched back to 0');
        $this->assertSame([], $this->wpdb->claims, 'both claims are released');

        $ledger = $this->callP((string) json_encode(['mode' => 'ledger', 'request_id' => self::EDIT]));
        $this->assertSame([true, 'completed', self::DRAFT, 'available'], [$ledger['found'], $ledger['phase'], $ledger['target_post_id'], $ledger['undo_state']]);
        $this->assertSame($r, $ledger['result'], 'a lost reply is recovered whole, its snapshot hash included');
        $this->assertSame(hash('sha256', (string) $this->rows->optionRows()[0]['option_value']), $ledger['result']['snapshot_sha256']);

        // The replay answers the recorded result and saves nothing again.
        $again = $this->callP($this->p('write', $input, [self::DRAFT], $pre));
        $this->assertSame(['already_applied', 'completed', self::DRAFT], [$again['outcome'] ?? null, $again['phase'] ?? null, $again['post_id'] ?? null], (string) json_encode($again));
        $this->assertSame($r, $again['result']);
        $this->assertCount(1, $this->api->documents[self::DRAFT]->saves, 'one save');
    }

    public function test_a_write_schedules_the_hourly_sweep_of_expired_copies(): void
    {
        $events = [];
        Functions\when('wp_next_scheduled')->alias(static function ($hook) use (&$events) {
            return isset($events[$hook]) ? $events[$hook][0] : false;
        });
        Functions\when('wp_schedule_event')->alias(static function ($timestamp, $recurrence, $hook) use (&$events): bool {
            $events[$hook] = [$timestamp, $recurrence];

            return true;
        });
        $this->enable();
        $input = $this->input();
        $pre   = $this->callP($this->p('precheck', $input, [self::DRAFT]));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $this->assertSame([], $events, 'a precheck schedules nothing');

        $r = $this->callP($this->p('write', $input, [self::DRAFT], $pre));
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame([BuilderDocumentSnapshot::HOOK_SWEEP], array_keys($events));
        $this->assertSame('hourly', $events[BuilderDocumentSnapshot::HOOK_SWEEP][1]);
    }

    public function test_write_needs_the_draft_named_at_dispatch(): void
    {
        $this->enable();
        $input = $this->input();
        $pre   = $this->callP($this->p('precheck', $input, [self::DRAFT]));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));

        // The control plane names no draft at dispatch: the creation was
        // undone between the approval and the write.
        $r = $this->callP($this->p('write', $input, [], $pre));
        $this->assertSame(['target_not_eligible', 'not_in_signed_list'], [$r['code'] ?? null, $r['detail'] ?? null], (string) json_encode($r));
        $this->assertSame([], $this->api->documents[self::DRAFT]->saves);
        $this->assertNull(AbilityLedger::get(self::EDIT));

        $bad = $this->callP($this->p('write', $input, null, $pre, '[' . self::DRAFT . ',' . self::KIT . ']'));
        $this->assertSame('bad_params', $bad['code'] ?? null);
    }

    public function test_entry_must_carry_the_builder_document_strategy(): void
    {
        $this->enable();
        $input = $this->input();
        foreach (['created_post_trash', 'post_fields', 'none'] as $strategy) {
            foreach (['precheck', 'write'] as $mode) {
                $r = $this->callP($this->p($mode, $input, [self::DRAFT], ['precheck_digest' => str_repeat('a', 64), 'preview_digest' => str_repeat('b', 64)], null, $strategy));
                $this->assertSame('snapshot_strategy_invalid', $r['code'] ?? null, $strategy . ' ' . $mode . ': ' . json_encode($r));
            }
        }
        $revert = $this->callP($this->p('revert', '{}', null));
        $this->assertSame('bad_params', $revert['code'] ?? null, 'an undo needs the signed snapshot hash');
        $read = $this->callP($this->p('read', $input, [self::DRAFT]));
        $this->assertSame('mode_class_mismatch', $read['code'] ?? null);
        $this->assertSame([], $this->api->documents[self::DRAFT]->saves);
    }

    public function test_preview_fixture_is_what_the_agent_answers(): void
    {
        $this->enable();
        $input = $this->input();
        $pre   = $this->callP($this->p('precheck', $input, [self::DRAFT]));
        $this->assertTrue($pre['ok'] ?? null, (string) json_encode($pre));
        $entry = $this->entry();
        $want  = json_encode([
            'note'             => 'wpmgr/page-edit precheck through the signed Router: the entry, the input, the page before, the preview, its digests, and the canonical bytes of the edited tree the preview digest is over. Generated by PageEditRouterTest with WPMGR_WRITE_FIXTURES=1; never hand-edit.',
            'request_id'       => self::EDIT,
            'entry'            => $entry,
            'entry_sha256'     => hash('sha256', $entry),
            'input'            => $input,
            'before_tree'      => self::page(),
            'base_fingerprint' => $pre['base_fingerprint'],
            'preview'          => $pre['preview'],
            'tree_json'        => json_encode($pre['preview']['tree']),
            'preview_digest'   => $pre['preview_digest'],
            'precheck_digest'  => $pre['precheck_digest'],
        ], JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) . "\n";
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::PREVIEW_FIXTURE, $want), 'could not write page-edit-preview.json');
        }
        $got = file_get_contents(self::PREVIEW_FIXTURE);
        $this->assertIsString($got, 'page-edit-preview.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $this->assertSame($want, $got, 'page-edit-preview.json differs from what the agent answers now; regenerate with WPMGR_WRITE_FIXTURES=1');

        $doc = json_decode($got, true, 512, JSON_THROW_ON_ERROR);
        $this->assertSame(hash('sha256', $doc['tree_json']), $doc['preview']['tree_sha256']);
        $this->assertSame(
            hash('sha256', (string) json_encode(['wpmgr.page_edit.v1', $doc['preview']['builder'], $doc['preview']['builder_version'], $doc['preview']['post_id'], $doc['base_fingerprint'], $doc['tree_json']])),
            $doc['preview_digest'],
            'the preview digest recomputes from the fixture alone'
        );
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

    /** The input: the heading's text, and a new paragraph after the text. */
    private function input(): string
    {
        return (string) json_encode([
            'post_id'          => self::DRAFT,
            'base_fingerprint' => BuilderDocumentFingerprint::ofPost(self::DRAFT, ElementorDocument::DESCRIPTOR_KEYS),
            'operations'       => [
                ['op' => 'set_text', 'ref' => '52982f9', 'field' => 'text', 'text' => 'Summer sale & more'],
                ['op' => 'insert', 'after' => '6cbe98a', 'outline' => [['type' => 'paragraph', 'text' => 'Open every day']]],
            ],
        ]);
    }

    /** The page-edit entry as the control plane sends it. */
    private function entry(string $snapshot = 'builder_document'): string
    {
        return (string) json_encode([
            'name'          => OwnAbilities::NAME_PAGE_EDIT,
            'source'        => 'wpmgr',
            'class'         => 'write',
            'status'        => 'admitted',
            'enabled'       => true,
            'approval_mode' => 'per_call',
            'snapshot'      => $snapshot,
            'limits'        => ['builders_enabled' => ['elementor']],
        ]);
    }

    /**
     * p as the control plane builds it.
     *
     * @param list<int>|null           $allowed  The signed list; null leaves it out.
     * @param array<string,mixed>|null $expected The precheck answer (or its two digests) for a write.
     */
    private function p(string $mode, string $input, ?array $allowed, ?array $expected = null, ?string $idsJson = null, string $snapshot = 'builder_document'): string
    {
        $entry = $this->entry($snapshot);
        $p     = ['mode' => $mode, 'request_id' => self::EDIT, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input];
        if ($expected !== null && $mode === 'write') {
            $p['expected'] = ['precheck_digest' => $expected['precheck_digest'], 'preview_digest' => $expected['preview_digest']];
        }
        if ($allowed !== null) {
            $p['allowed_draft_ids'] = $allowed;
        }
        $text = (string) json_encode($p);

        return $idsJson === null ? $text : substr($text, 0, -1) . ',"allowed_draft_ids":' . $idsJson . '}';
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
