<?php
/**
 * AbilityRunCommand tests, driven through the real request path: a signed
 * Ed25519 command token carrying the pd claim, Router::authorizeCommand() and
 * Router::handleCommand().
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Abilities\AbilityDenylist;
use WPMgr\Agent\Abilities\AbilityGuards;
use WPMgr\Agent\Abilities\AbilityInterception;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Commands\AbilityRunCommand;
use WPMgr\Agent\Commands\CommandEffect;
use WPMgr\Agent\Commands\CommandRepeatability;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AbilityRunCommand
 * @covers \WPMgr\Agent\Abilities\OwnAbilities
 * @covers \WPMgr\Agent\Abilities\AbilityDenylist
 * @covers \WPMgr\Agent\Abilities\AbilityGuards
 */
final class AbilityRunCommandTest extends TestCase
{
    private const REQ_ID = '11111111-2222-4333-8444-555555555555';

    private string $keyFile;

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<int,object> */
    private array $posts = [];

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;


    /** @var list<array{0:string,1:callable,2:int}> Captured add_filter calls. */
    private array $filters = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-ability-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options   = [];
        $this->posts     = [];
        $this->filters   = [];

        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('current_user_can')->justReturn(true);
        Functions\when('register_rest_route')->justReturn(true);
        $GLOBALS['wp_version'] = '6.4.2';
        Functions\when('get_post')->alias(fn ($id) => $this->posts[(int) $id] ?? null);
        Functions\when('add_filter')->alias(function ($name, $cb, $prio = 10) {
            $this->filters[] = [(string) $name, $cb, (int) $prio];
            $this->syncWpFilter();
            return true;
        });
        Functions\when('remove_filter')->alias(function ($name, $cb, $prio = 10) {
            foreach ($this->filters as $i => $row) {
                if ($row[0] === $name && $row[1] === $cb && $row[2] === $prio) {
                    unset($this->filters[$i]);
                }
            }
            $this->syncWpFilter();
            return true;
        });

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;
        $GLOBALS['wpdb'] = new class {
            public string $prefix = 'wp_';
            public string $options = 'wp_options';
            public string $last_error = '';

            public function prepare(string $q, ...$args): string
            {
                return $q;
            }

            public function get_var(string $q): ?string
            {
                return null;
            }

            public function query(string $q): int
            {
                return 0;
            }

            /** @param array<string,mixed> $row */
            public function insert(string $t, array $row, $f = null): int
            {
                return 1;
            }
        };
        $this->resetShieldStash();

        $this->router = new Router(new Connector($keystore, new Settings()), [new AbilityRunCommand()]);
    }

    protected function tear_down(): void
    {
        $this->resetShieldStash();
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb'], $GLOBALS['wp_version'], $GLOBALS['wp_filter']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Identity
    // -------------------------------------------------------------------------

    public function test_declares_write_and_unsafe_to_repeat(): void
    {
        $c = new AbilityRunCommand();
        $this->assertSame('ability_run', $c->name());
        $this->assertSame(CommandEffect::Write, $c->effect());
        $this->assertSame(CommandRepeatability::Unsafe, $c->repeatability());
    }

    // -------------------------------------------------------------------------
    // R2: the digest is over bytes
    // -------------------------------------------------------------------------

    public function test_digest_mismatch_is_refused_and_nothing_runs(): void
    {
        Functions\expect('get_post')->never();
        $p = $this->p('read', OwnAbilities::NAME_CONTENT, ['post_id' => 5]);

        $r = $this->call($p, hash('sha256', $p . ' '));

        $this->assertFalse($r['ok']);
        $this->assertSame('token_params_mismatch', $r['code']);
    }

    public function test_a_token_without_pd_is_refused(): void
    {
        $r = $this->call($this->p('read', OwnAbilities::NAME_FACTS), null);

        $this->assertSame('token_params_mismatch', $r['code']);
    }

    public function test_the_digest_covers_the_exact_bytes_not_the_meaning(): void
    {
        $p          = $this->p('read', OwnAbilities::NAME_FACTS);
        $reencoded  = (string) json_encode(json_decode($p), JSON_PRETTY_PRINT);
        $this->assertNotSame($p, $reencoded);

        $r = $this->call($reencoded, hash('sha256', $p));

        $this->assertSame('token_params_mismatch', $r['code'], 'semantically equal bytes must still not verify');
    }

    public function test_empty_object_and_empty_array_stay_distinct(): void
    {
        $asArray  = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{"plugin_slugs":[]}'));
        $asObject = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{"plugin_slugs":{}}'));
        $inputArr = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '[]'));
        $inputObj = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{}'));

        $this->assertTrue($asArray['ok'], 'an empty list is a list');
        $this->assertSame('bad_input', $asObject['code'], 'an empty object is not a list');
        $this->assertSame('bad_input', $inputArr['code'], 'input must be an object, and [] is not one');
        $this->assertTrue($inputObj['ok']);
    }

    public function test_entry_hash_mismatch_is_refused(): void
    {
        $entry = $this->entry(OwnAbilities::NAME_FACTS);
        $p     = (string) json_encode([
            'mode' => 'read', 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry . ' '), 'input' => '{}',
        ]);

        $r = $this->callP($p);

        $this->assertSame('integration_entry_changed', $r['code']);
    }

    public function test_body_must_be_exactly_p(): void
    {
        $p = $this->p('read', OwnAbilities::NAME_FACTS);
        $r = $this->post((string) json_encode(['p' => $p, 'extra' => 1]), hash('sha256', $p));

        $this->assertSame('bad_params', $r['code']);
    }

    // -------------------------------------------------------------------------
    // Scope and denylist
    // -------------------------------------------------------------------------

    public function test_a_vendor_ability_is_refused_even_when_the_entry_says_admitted(): void
    {
        $r = $this->callP($this->p('read', 'acme-builder/get-page', [], '{}', ['status' => 'admitted', 'source' => 'vendor']));

        $this->assertFalse($r['ok']);
        $this->assertSame('ability_not_runnable_yet', $r['code']);
    }

    public function test_a_core_ability_is_refused_in_this_slice(): void
    {
        $r = $this->callP($this->p('read', 'core/get-site-info', [], '{}', ['source' => 'core', 'status' => 'admitted']));

        $this->assertSame('ability_not_runnable_yet', $r['code']);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function deniedNames(): array
    {
        return [
            'known code exec'       => ['bricks/execute-php'],
            'pattern php'           => ['acme/run-php'],
            'own namespace, denied' => ['wpmgr/execute-php'],
            'shell segment'         => ['acme/shell'],
            'sql prefix'            => ['acme/sql-console'],
            'install'               => ['acme/plugin-install'],
        ];
    }

    /**
     * @dataProvider deniedNames
     */
    public function test_a_denylisted_name_is_refused_before_scope(string $name): void
    {
        $r = $this->callP($this->p('read', $name, [], '{}', ['status' => 'admitted']));

        $this->assertFalse($r['ok']);
        $this->assertSame('ability_denied', $r['code']);
    }

    public function test_the_denylist_does_not_over_fire_on_own_or_common_names(): void
    {
        foreach (OwnAbilities::names() as $name) {
            $this->assertFalse(AbilityDenylist::denies($name), $name);
        }
        foreach (['core/get-site-info', 'acme/get-page', 'acme/list-posts', 'yoast/get-title'] as $name) {
            $this->assertFalse(AbilityDenylist::denies($name), $name);
        }
        $this->assertTrue(AbilityDenylist::denies(''));
        $this->assertTrue(AbilityDenylist::denies(null));
    }

    public function test_an_unknown_wpmgr_name_is_not_run(): void
    {
        $r = $this->callP($this->p('read', 'wpmgr/made-up'));

        $this->assertSame('ability_unknown', $r['code']);
    }

    public function test_write_and_revert_modes_refuse_a_read_ability(): void
    {
        foreach (['write', 'revert'] as $mode) {
            $this->assertSame('bad_request_id', $this->callP($this->p($mode, OwnAbilities::NAME_FACTS))['code']);
            $this->assertSame(
                'mode_class_mismatch',
                $this->callP($this->p($mode, OwnAbilities::NAME_FACTS, ['request_id' => self::REQ_ID]))['code']
            );
        }
        $this->assertSame('bad_mode', $this->callP($this->p('bogus', OwnAbilities::NAME_FACTS))['code']);
    }

    public function test_a_disabled_entry_is_refused(): void
    {
        $r = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{}', ['enabled' => false]));

        $this->assertSame('ability_disabled', $r['code']);
    }

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function notAdmittedStatuses(): array
    {
        return [
            'detect_only'           => [['status' => 'detect_only']],
            'awaiting_vendor_tools' => [['status' => 'awaiting_vendor_tools']],
            'null'                  => [['status' => null]],
        ];
    }

    /**
     * @dataProvider notAdmittedStatuses
     *
     * @param array<string,mixed> $override Entry override.
     */
    public function test_an_entry_that_is_not_admitted_is_refused(array $override): void
    {
        $r = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{}', $override));

        $this->assertSame('ability_not_admitted', $r['code']);
    }

    public function test_an_entry_without_status_is_refused(): void
    {
        $r = $this->callP($this->pWithout('status', OwnAbilities::NAME_FACTS));

        $this->assertSame('ability_not_admitted', $r['code']);
    }

    public function test_an_entry_without_source_is_refused_not_defaulted(): void
    {
        $r = $this->callP($this->pWithout('source', OwnAbilities::NAME_FACTS));

        $this->assertSame('entry_source_mismatch', $r['code']);
        $r = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{}', ['source' => null]));
        $this->assertSame('entry_source_mismatch', $r['code']);
    }

    /**
     * The cross-language fixture the Go control plane writes
     * (apps/api/internal/agentcmd/testdata/ability_run_fixture.json, from
     * BuildAbilityRunParams). The agent recomputes every digest from the
     * bytes it decodes, then runs the exact body through the command.
     */
    public function test_the_go_fixture_digests_match_and_the_command_accepts_it(): void
    {
        $path = dirname(__DIR__, 2) . '/api/internal/agentcmd/testdata/ability_run_fixture.json';
        $this->assertFileExists($path, 'the Go fixture is missing, so this test proves nothing');
        $fx = json_decode((string) file_get_contents($path), true);

        $body = json_decode($fx['body'], true);
        $this->assertSame(['p'], array_keys($body));
        $p = $body['p'];
        $this->assertSame($fx['pd'], hash('sha256', $p));

        $req = json_decode($p, false);
        $this->assertSame($fx['entry_sha256'], hash('sha256', $req->entry));
        $this->assertSame($fx['input_sha256'], hash('sha256', $req->input));
        $this->assertStringContainsString("\u{2014}", $req->entry);
        $this->assertStringContainsString("\u{2028}", $req->entry);
        $this->assertStringContainsString('<b>&amp;', $req->entry);

        $r = $this->post($fx['body'], $fx['pd']);
        $this->assertTrue($r['ok'] ?? false, 'the command refused the Go-built body: ' . json_encode($r));
        $this->assertSame($fx['precheck_digest'], $r['precheck_digest']);
    }

    /**
     * A read-mode `p` whose entry lacks one key entirely.
     */
    private function pWithout(string $key, string $name): string
    {
        $fields = json_decode($this->entry($name), true);
        unset($fields[$key]);
        $fields['limits'] = new \stdClass();
        $entry = (string) json_encode($fields);

        return (string) json_encode([
            'mode'         => 'read',
            'entry'        => $entry,
            'entry_sha256' => hash('sha256', $entry),
            'input'        => '{}',
        ]);
    }

    public function test_read_mode_needs_a_read_class_entry(): void
    {
        $r = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{}', ['class' => 'write']));

        $this->assertSame('mode_class_mismatch', $r['code']);
    }

    // -------------------------------------------------------------------------
    // Own abilities: output shape
    // -------------------------------------------------------------------------

    public function test_site_facts_shape(): void
    {
        $GLOBALS['wp_version'] = '6.9.1';
        $this->options['template']      = 'twentytwentyfive';
        $this->options['stylesheet']    = 'child-theme';
        $this->options['active_plugins'] = ['acme-builder/acme.php', 'hello.php'];

        $r = $this->callP($this->p('read', OwnAbilities::NAME_FACTS, [], '{"plugin_slugs":["acme-builder","other"],"theme_slugs":["child-theme"]}'));

        $this->assertTrue($r['ok']);
        $this->assertSame('completed', $r['outcome']);
        $o = $r['output'];
        $this->assertSame('6.9.1', $o['wp_version']);
        $this->assertSame(PHP_VERSION, $o['php_version']);
        $this->assertSame(['template' => 'twentytwentyfive', 'stylesheet' => 'child-theme'], $o['active_theme']);
        $this->assertSame(['acme-builder', 'hello'], $o['active_plugins']);
        $this->assertSame(['plugin:acme-builder', 'theme:child-theme'], $o['builder_hints']);
        $this->assertFalse($o['abilities_api']['filters_71']);
        $this->assertArrayHasKey('multisite', $o);
    }

    public function test_content_read_returns_capped_text_under_from_the_site(): void
    {
        $this->seedPost(7, '<p>Hello <b>world</b></p><script>alert(1)</script>' . str_repeat('x', 600), 'page', 'publish', 'Title');

        $r = $this->callP($this->p('read', OwnAbilities::NAME_CONTENT, [], '{"post_id":7,"max_bytes":256}'));

        $this->assertTrue($r['ok']);
        $o = $r['output'];
        $this->assertSame(7, $o['post_id']);
        $this->assertSame('page', $o['post_type']);
        $this->assertTrue($o['truncated']);
        $this->assertSame(256, strlen($o['from_the_site']['text']));
        $this->assertStringStartsWith('Hello world', $o['from_the_site']['text']);
        $this->assertSame('Title', $o['from_the_site']['title']);
        $this->assertGreaterThan(256, $o['text_bytes']);
        $this->assertArrayNotHasKey('text', $o, 'site text must live only under from_the_site');
    }

    public function test_content_read_never_splits_a_multibyte_character(): void
    {
        $this->seedPost(8, str_repeat("\u{20AC}", 200));

        $r = $this->callP($this->p('read', OwnAbilities::NAME_CONTENT, [], '{"post_id":8,"max_bytes":256}'));

        $this->assertSame(1, preg_match('//u', $r['output']['from_the_site']['text']));
        $this->assertLessThanOrEqual(256, strlen($r['output']['from_the_site']['text']));
    }

    public function test_content_read_refuses_everything_that_is_not_public_with_one_answer(): void
    {
        $this->seedPost(20, 'x', 'page', 'draft');
        $this->seedPost(21, 'x', 'page', 'private');
        $this->seedPost(22, 'x', 'page', 'publish', 'T', 'secret');
        $this->seedPost(23, 'x', 'shop_order', 'publish');
        $this->seedPost(24, 'x', 'page', 'trash');

        $answers = [];
        foreach ([20, 21, 22, 23, 24, 999] as $id) {
            $r = $this->callP($this->p('read', OwnAbilities::NAME_CONTENT, [], '{"post_id":' . $id . '}'));
            $this->assertFalse($r['ok'], (string) $id);
            $answers[] = $r['code'] . '|' . $r['detail'];
        }
        $this->assertSame(['post_not_readable|no published, unprotected post or page with that id'], array_values(array_unique($answers)));
    }

    public function test_content_read_validates_its_arguments(): void
    {
        foreach (['{}', '{"post_id":"7"}', '{"post_id":0}', '{"post_id":7,"max_bytes":1}', '{"post_id":7,"x":1}', '{"post_id":7.5}'] as $input) {
            $this->assertSame('bad_input', $this->callP($this->p('read', OwnAbilities::NAME_CONTENT, [], $input))['code'], $input);
        }
    }

    public function test_abilities_api_absent_inventory_still_works(): void
    {
        $this->assertFalse(function_exists('wp_get_abilities'), 'this process must not have the abilities API');

        $r = $this->callP($this->p('read', OwnAbilities::NAME_INVENTORY));

        $this->assertTrue($r['ok']);
        $o = $r['output'];
        $this->assertFalse($o['api_present']);
        $this->assertFalse($o['truncated']);
        $names = array_column($o['abilities'], 'name');
        $this->assertSame(OwnAbilities::names(), $names);
        foreach ($o['abilities'] as $row) {
            $this->assertSame('wpmgr', $row['owner_kind']);
            $this->assertFalse($row['owner_mismatch']);
            $this->assertMatchesRegularExpression('/^sha256:[0-9a-f]{64}$/', $row['schema_struct_sha256']);
            $this->assertSame($row['name'] === OwnAbilities::NAME_PAGE_CREATE ? 'write' : 'read', $row['class']);
        }
    }

    public function test_all_own_abilities_run_without_the_abilities_api(): void
    {
        $this->seedPost(3);
        foreach ([[OwnAbilities::NAME_FACTS, '{}'], [OwnAbilities::NAME_INVENTORY, '{}'], [OwnAbilities::NAME_CONTENT, '{"post_id":3}']] as [$name, $input]) {
            $this->assertTrue($this->callP($this->p('read', $name, [], $input))['ok'], $name);
        }
    }

    // -------------------------------------------------------------------------
    // precheck and ledger
    // -------------------------------------------------------------------------

    public function test_precheck_validates_without_executing(): void
    {
        Functions\expect('get_post')->never();
        $entry = $this->entry(OwnAbilities::NAME_CONTENT);
        $input = '{"post_id":41}';

        $r = $this->callP($this->p('precheck', OwnAbilities::NAME_CONTENT, ['request_id' => self::REQ_ID], $input));

        $this->assertTrue($r['ok']);
        $this->assertSame('prechecked', $r['outcome']);
        $this->assertTrue($r['valid']);
        $expected = hash('sha256', (string) json_encode([hash('sha256', $entry), hash('sha256', $input), '', '']));
        $this->assertSame($expected, $r['precheck_digest']);
        $this->assertArrayNotHasKey('output', $r);
    }

    public function test_precheck_rejects_bad_input_and_needs_a_request_id(): void
    {
        $bad = $this->callP($this->p('precheck', OwnAbilities::NAME_CONTENT, ['request_id' => self::REQ_ID], '{}'));
        $this->assertSame('bad_input', $bad['code']);

        $noId = $this->callP($this->p('precheck', OwnAbilities::NAME_FACTS));
        $this->assertSame('bad_request_id', $noId['code']);
    }

    public function test_ledger_reports_not_found(): void
    {
        $r = $this->callP((string) json_encode(['mode' => 'ledger', 'request_id' => self::REQ_ID]));

        $this->assertTrue($r['ok']);
        $this->assertFalse($r['found']);
        $this->assertFalse($r['inflight']);
        $this->assertSame(self::REQ_ID, $r['request_id']);

        $this->assertSame('bad_request_id', $this->callP((string) json_encode(['mode' => 'ledger', 'request_id' => 'nope']))['code']);
    }

    // -------------------------------------------------------------------------
    // Interception guards (WP 7.1+)
    // -------------------------------------------------------------------------

    public function test_guards_are_armed_only_on_71_and_are_removed_afterwards(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $this->assertTrue(AbilityGuards::supported());
        $seen = null;
        Functions\when('get_post')->alias(function ($id) use (&$seen) {
            $seen = count($this->filters);
            return $this->posts[(int) $id] ?? null;
        });
        $this->seedPost(9, '<p>x</p>');

        $r = $this->callP($this->p('read', OwnAbilities::NAME_CONTENT, [], '{"post_id":9}'));

        $this->assertTrue($r['ok']);
        $this->assertSame(12, $seen, 'a recorder and a guard on each of the six filters are installed while the ability runs');
        $this->assertSame([], $this->filters, 'and every one is removed afterwards');

        $GLOBALS['wp_version'] = '7.0.9';
        $this->assertFalse(AbilityGuards::supported());
    }

    public function test_a_tampered_input_or_short_circuit_or_nested_call_is_a_violation(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');

        // Recorder sees the value, a later filter rewrites it: the input guard
        // flags it and aborts the call before anything else runs.
        $this->filters[] = [AbilityGuards::FILTER_INPUT, static fn () => ['a' => 2], 10];
        $this->assertSame([true, 'ability_intercepted/input'], $this->applyOrAbort(AbilityGuards::FILTER_INPUT, ['a' => 1], 'acme/outer', null));

        $outer = new \WP_Filter_Sentinel();
        $this->assertSame($outer, $this->applyFilters(AbilityGuards::FILTER_PRE, $outer, 'acme/outer', []), 'the outer call itself passes');
        $this->assertInstanceOf(\WP_Error::class, $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/outer', []), 're-entry is refused');
        $this->assertInstanceOf(\WP_Error::class, $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/other', []), 'a nested ability is refused');

        $this->filters[] = [AbilityGuards::FILTER_PRE, static fn () => 'fake', 10];
        $this->assertInstanceOf(\WP_Error::class, $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/outer', []));

        $v = $g->violations();
        $this->assertContains('input', $v);
        $this->assertContains('short_circuit', $v);
        $this->assertContains('nested_reentry', $v);
        $this->assertContains('nested_ability_refused', $v);

        $g->disarm();
        $this->assertSame([], array_values(array_filter($this->filters, static fn ($r) => $r[2] !== 10)), 'every guard hook is removed');
    }

    public function test_a_real_sentinel_passes_through_the_short_circuit_guard(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');

        // A third-party filter that returns $pre unchanged is pass-through.
        $this->filters[] = [AbilityGuards::FILTER_PRE, static fn ($pre) => $pre, 10];
        $sentinel = new \WP_Filter_Sentinel();
        $out      = $this->applyFilters(AbilityGuards::FILTER_PRE, $sentinel, 'acme/outer', []);

        $this->assertSame($sentinel, $out, 'core compares the returned value to its own sentinel by identity');
        $this->assertSame([], $g->violations());
        $g->disarm();
    }

    /**
     * @return array<string,array{0:mixed}>
     */
    public static function shortCircuitValues(): array
    {
        return [
            'null'               => [null],
            'false'              => [false],
            'a fresh sentinel'   => ['fresh'],
            'a cached response'  => [['ok' => true]],
        ];
    }

    /**
     * @dataProvider shortCircuitValues
     *
     * @param mixed $replacement Value a third-party filter returns.
     */
    public function test_replacing_the_sentinel_with_anything_is_a_short_circuit(mixed $replacement): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $replacement     = $replacement === 'fresh' ? new \WP_Filter_Sentinel() : $replacement;
        $this->filters[] = [AbilityGuards::FILTER_PRE, static fn () => $replacement, 10];

        $out = $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/outer', []);

        $this->assertInstanceOf(\WP_Error::class, $out);
        $this->assertSame(['short_circuit'], $g->violations());
        $g->disarm();
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function validateFilters(): array
    {
        return [
            'input'  => [AbilityGuards::FILTER_VALIDATE_INPUT, 'validate_input'],
            'output' => [AbilityGuards::FILTER_VALIDATE_OUTPUT, 'validate_output'],
        ];
    }

    /**
     * @dataProvider validateFilters
     */
    public function test_a_validate_filter_flipping_wp_error_to_true_is_a_violation(string $filter, string $label): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->filters[] = [$filter, static fn () => true, 10];
        $error           = new \WP_Error('ability_invalid_' . substr($label, 9), 'bad');

        [$aborted, $out] = $this->applyOrAbort($filter, $error, ['x' => 1], 'acme/outer');

        if ($label === 'validate_input') {
            $this->assertSame([true, 'ability_intercepted/validate_input'], [$aborted, $out], 'input validation runs before the callback, so the call aborts');
        } else {
            $this->assertFalse($aborted);
            $this->assertSame($error, $out, 'the schema failure is restored');
        }
        $this->assertSame([$label], $g->violations());
        $g->disarm();
    }

    /**
     * @dataProvider validateFilters
     */
    public function test_an_untouched_validate_result_is_not_a_violation(string $filter, string $label): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->filters[] = [$filter, static fn ($v) => $v, 10];

        $this->assertTrue($this->applyFilters($filter, true, ['x' => 1], 'acme/outer'));
        $error = new \WP_Error('e', 'bad');
        $this->assertSame($error, $this->applyFilters($filter, $error, ['x' => 1], 'acme/outer'));
        $this->assertSame([], $g->violations(), $label);
        $g->disarm();
    }

    public function test_an_earliest_filter_registered_before_arm_is_a_violation(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        // Registered first, so it runs ahead of our recorder in the same bucket.
        $this->filters[] = [AbilityGuards::FILTER_PRE, static fn () => new \WP_Filter_Sentinel(), PHP_INT_MIN];
        $g = new AbilityGuards();
        $g->arm('acme/outer');

        $out = $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/outer', []);

        $this->assertInstanceOf(\WP_Error::class, $out);
        $this->assertContains('short_circuit_order', $g->violations());
        $g->disarm();
    }

    public function test_a_latest_short_circuit_added_after_arm_is_a_violation(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        // Added after arm (as from wp_ability_invoked), so it runs after our guard.
        $this->filters[] = [AbilityGuards::FILTER_PRE, static fn () => ['attacker' => 'chosen'], PHP_INT_MAX];

        $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/outer', []);

        $this->assertContains('short_circuit_order', $g->violations());
        $g->disarm();
    }

    public function test_a_latest_permission_flip_added_after_arm_is_refused_before_anything_runs(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->filters[] = [AbilityGuards::FILTER_PERMISSION, static fn () => true, PHP_INT_MAX];

        // The short-circuit guard runs before permission and execution.
        $out = $this->applyFilters(AbilityGuards::FILTER_PRE, new \WP_Filter_Sentinel(), 'acme/outer', []);
        $this->assertInstanceOf(\WP_Error::class, $out, 'core returns the short-circuit value; nothing else runs');
        $this->assertContains('permission_order', $g->violations());

        // And the permission guard itself sees it too.
        $g->arm('acme/outer');
        $this->filters[] = [AbilityGuards::FILTER_PERMISSION, static fn () => true, PHP_INT_MAX];
        $this->assertSame([true, 'ability_intercepted/permission'], $this->applyOrAbort(AbilityGuards::FILTER_PERMISSION, false, 'acme/outer', [], null), 'the permission filter aborts before the callback');
        $this->assertContains('permission_order', $g->violations());
        $g->disarm();
    }

    public function test_a_permission_flip_staged_from_an_input_filter_aborts_before_permission_runs(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->filters[] = [AbilityGuards::FILTER_INPUT, function ($v) {
            $this->filters[] = [AbilityGuards::FILTER_PERMISSION, static fn () => true, PHP_INT_MAX];
            $this->syncWpFilter();
            return $v;
        }, 10];

        $this->assertSame([true, 'ability_intercepted/input'], $this->applyOrAbort(AbilityGuards::FILTER_INPUT, ['a' => 1], 'acme/outer', null));
        $this->assertContains('permission_order', $g->violations());
        $g->disarm();
    }

    public function test_removing_our_permission_guard_from_an_input_filter_aborts(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->filters[] = [AbilityGuards::FILTER_INPUT, function ($v) {
            $this->filters   = array_values(array_filter($this->filters, static fn ($r) => !($r[0] === AbilityGuards::FILTER_PERMISSION && $r[2] === PHP_INT_MAX)));
            $this->filters[] = [AbilityGuards::FILTER_PERMISSION, static fn () => true, PHP_INT_MAX];
            $this->syncWpFilter();
            return $v;
        }, 10];

        $this->assertSame([true, 'ability_intercepted/input'], $this->applyOrAbort(AbilityGuards::FILTER_INPUT, ['a' => 1], 'acme/outer', null));
        $this->assertContains('permission_order', $g->violations());
        $g->disarm();
    }

    public function test_replacing_the_permission_hook_from_an_input_filter_aborts(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->filters[] = [AbilityGuards::FILTER_INPUT, function ($v) {
            $this->filters   = array_values(array_filter($this->filters, static fn ($r) => $r[0] !== AbilityGuards::FILTER_PERMISSION));
            $this->filters[] = [AbilityGuards::FILTER_PERMISSION, static fn () => true, 10];
            $this->syncWpFilter();
            return $v;
        }, 10];

        $this->assertSame([true, 'ability_intercepted/input'], $this->applyOrAbort(AbilityGuards::FILTER_INPUT, ['a' => 1], 'acme/outer', null));
        $this->assertContains('permission_order', $g->violations());
        $g->disarm();
    }

    public function test_a_nested_apply_of_the_permission_filter_cannot_launder_a_flip(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g     = new AbilityGuards();
        $g->arm('acme/outer');
        $depth = 0;
        $this->filters[] = [AbilityGuards::FILTER_PERMISSION, function ($v, ...$rest) use (&$depth) {
            if ($depth > 0) {
                return $v;
            }
            $depth++;
            $this->applyFilters(AbilityGuards::FILTER_PERMISSION, true, ...$rest);
            $depth--;
            return true;
        }, 10];

        $this->assertSame([true, 'ability_intercepted/permission'], $this->applyOrAbort(AbilityGuards::FILTER_PERMISSION, false, 'acme/outer', [], null));
        $this->assertContains('permission', $g->violations());
        $g->disarm();
    }

    public function test_a_nested_apply_of_the_result_filter_is_refused_at_finish(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g     = new AbilityGuards();
        $g->arm('acme/outer');
        $depth = 0;
        $this->filters[] = [AbilityGuards::FILTER_RESULT, function ($v, ...$rest) use (&$depth) {
            if ($depth > 0) {
                return $v;
            }
            $depth++;
            $this->applyFilters(AbilityGuards::FILTER_RESULT, ['attacker' => 1], ...$rest);
            $depth--;
            return ['attacker' => 1];
        }, 10];

        $this->assertSame(['real' => 1], $this->applyFilters(AbilityGuards::FILTER_RESULT, ['real' => 1], 'acme/outer', [], null), 'the recorded value is restored');
        $this->assertFalse($g->finish());
        $this->assertContains('result', $g->violations());
        $g->disarm();
    }

    public function test_a_recorder_without_its_guard_is_refused_at_finish(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        $this->syncWpFilter();
        $recorder = array_values(array_filter($this->filters, static fn ($r) => $r[0] === AbilityGuards::FILTER_RESULT && $r[2] === PHP_INT_MIN))[0][1];
        $recorder(['r' => 1], 'acme/outer', [], null);

        $this->assertFalse($g->finish());
        $this->assertContains('result_unpaired', $g->violations());
        $g->disarm();
    }

    public function test_ordinary_priority_filters_on_every_guarded_tag_are_not_violations(): void
    {
        $GLOBALS['wp_version'] = '7.1.0';
        $tags = [
            AbilityGuards::FILTER_PRE, AbilityGuards::FILTER_INPUT, AbilityGuards::FILTER_PERMISSION,
            AbilityGuards::FILTER_RESULT, AbilityGuards::FILTER_VALIDATE_INPUT, AbilityGuards::FILTER_VALIDATE_OUTPUT,
        ];
        foreach ($tags as $tag) {
            $this->filters[] = [$tag, static fn ($v) => $v, 10];
        }
        $g = new AbilityGuards();
        $g->arm('acme/outer');
        foreach ($tags as $tag) {
            // A late registration at an ordinary priority stays inside our edges.
            $this->filters[] = [$tag, static fn ($v) => $v, 1000];
        }

        $s = new \WP_Filter_Sentinel();
        $this->assertSame($s, $this->applyFilters(AbilityGuards::FILTER_PRE, $s, 'acme/outer', []));
        $this->assertSame(['a' => 1], $this->applyFilters(AbilityGuards::FILTER_INPUT, ['a' => 1], 'acme/outer', null));
        $this->assertTrue($this->applyFilters(AbilityGuards::FILTER_VALIDATE_INPUT, true, ['a' => 1], 'acme/outer'));
        $this->assertTrue($this->applyFilters(AbilityGuards::FILTER_PERMISSION, true, 'acme/outer', [], null));
        $this->assertSame(['r' => 1], $this->applyFilters(AbilityGuards::FILTER_RESULT, ['r' => 1], 'acme/outer', [], null));
        $this->assertTrue($this->applyFilters(AbilityGuards::FILTER_VALIDATE_OUTPUT, true, ['r' => 1], 'acme/outer'));
        $this->assertTrue($g->finish(), 'the end-of-call check accepts it');
        $this->assertSame([], $g->violations());
        $g->disarm();
    }

    public function test_a_sentinel_as_the_final_result_is_refused(): void
    {
        $g = new AbilityGuards();
        $this->assertFalse($g->checkResult(new \WP_Filter_Sentinel()));
        $this->assertSame(['sentinel_result'], $g->violations());
        $this->assertTrue((new AbilityGuards())->checkResult(['output' => []]));
    }

    public function test_a_vendor_ability_stays_unrunnable_with_a_resolved_owner(): void
    {
        $r = $this->callP($this->p('read', 'acmebuild/get-page-elements', [], '{}', [
            'source'        => 'vendor',
            'status'        => 'admitted',
            'owner_kind'    => 'plugin',
            'owner_dir'     => 'acmebuild',
            'owner_version' => '2.4.1',
        ]));

        $this->assertFalse($r['ok']);
        $this->assertSame('ability_not_runnable_yet', $r['code']);
    }

    /**
     * applyFilters(), reporting an AbilityInterception instead of throwing.
     *
     * @param mixed ...$args Value, then the extra arguments.
     * @return array{0:bool,1:mixed} [aborted, value or exception message]
     */
    private function applyOrAbort(string $name, mixed ...$args): array
    {
        try {
            return [false, $this->applyFilters($name, ...$args)];
        } catch (AbilityInterception $e) {
            return [true, $e->getMessage()];
        }
    }

    /**
     * Mirror the captured filters into $GLOBALS['wp_filter'] in core's shape:
     * per tag an object whose public `callbacks` maps priority (ascending) to
     * entries in registration order, each carrying its `function`.
     */
    private function syncWpFilter(): void
    {
        $registry = [];
        foreach ($this->filters as [$tag, $cb, $prio]) {
            if (!isset($registry[$tag])) {
                $registry[$tag] = new class {
                    /** @var array<int,array<int,array{function:callable,accepted_args:int}>> */
                    public array $callbacks = [];
                };
            }
            $registry[$tag]->callbacks[$prio][] = ['function' => $cb, 'accepted_args' => 4];
        }
        foreach ($registry as $hook) {
            ksort($hook->callbacks);
        }
        $GLOBALS['wp_filter'] = $registry;
    }

    /**
     * Run the captured filters for $name in priority order, the way
     * apply_filters() does, threading the first argument through.
     *
     * @param mixed ...$args Value, then the extra arguments.
     * @return mixed
     */
    private function applyFilters(string $name, mixed ...$args): mixed
    {
        $this->syncWpFilter();
        $rows = array_values(array_filter($this->filters, static fn ($r) => $r[0] === $name));
        usort($rows, static fn ($a, $b) => $a[2] <=> $b[2]);
        foreach ($rows as $row) {
            $args[0] = ($row[1])(...$args);
        }

        return $args[0];
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * @param array<string,mixed> $entryOverrides Entry field overrides.
     */
    private function entry(string $name, array $entryOverrides = []): string
    {
        return (string) json_encode($entryOverrides + [
            'name'    => $name,
            'source'  => 'wpmgr',
            'class'   => 'read',
            'status'  => 'admitted',
            'enabled' => true,
            'limits'  => new \stdClass(),
        ]);
    }

    /**
     * Build the exact `p` text.
     *
     * @param array<string,mixed> $extra          Extra top-level fields.
     * @param array<string,mixed> $entryOverrides Entry overrides.
     */
    private function p(string $mode, string $name, array $extra = [], string $input = '{}', array $entryOverrides = []): string
    {
        $entry = $this->entry($name, $entryOverrides);

        return (string) json_encode([
            'mode'         => $mode,
            'entry'        => $entry,
            'entry_sha256' => hash('sha256', $entry),
            'input'        => $input,
        ] + $extra);
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
        return $this->post((string) json_encode(['p' => $p]), $pd);
    }

    /**
     * @return array<string,mixed>
     */
    private function post(string $body, ?string $pd): array
    {
        $request = new \WP_REST_Request('POST', '/wpmgr/v1/command/ability_run');
        $request->set_url_params(['command' => 'ability_run']);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Accept', 'application/json');
        $request->set_header('Authorization', 'Bearer ' . $this->mintToken($pd));
        $request->set_body($body);

        $this->assertTrue($this->router->authorizeCommand($request, 'ability_run'), 'the signed request was not authorized');
        $response = $this->router->handleCommand($request);
        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertIsArray($response->data);

        return $response->data;
    }

    private function seedPost(int $id, string $content = 'Plain words.', string $type = 'page', string $status = 'publish', string $title = 'A title', string $password = ''): void
    {
        $p                    = new \stdClass();
        $p->ID                = $id;
        $p->post_type         = $type;
        $p->post_status       = $status;
        $p->post_title        = $title;
        $p->post_content      = $content;
        $p->post_password     = $password;
        $p->post_modified_gmt = '2026-09-01 10:00:00';
        $this->posts[$id]     = $p;
    }

    private function mintToken(?string $pd): string
    {
        $claims = [
            'aud' => $this->siteId,
            'cmd' => 'ability_run',
            'jti' => bin2hex(random_bytes(8)),
            'exp' => time() + 30,
        ];
        if ($pd !== null) {
            $claims['pd'] = $pd;
        }
        $segments = [
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
