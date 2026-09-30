<?php
/**
 * ContentProbeCommand tests, driven through the real request path: a signed
 * Ed25519 command token, Router::authorizeCommand(), Router::handleCommand().
 *
 * WordPress itself is an in-memory fake (posts, options, post meta, abilities).
 * The command must reach its verdict from that fake and from the descriptors
 * in the request, and from nothing else.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Commands\CommandEffect;
use WPMgr\Agent\Commands\CommandRepeatability;
use WPMgr\Agent\Commands\ContentProbeCommand;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\ContentProbeCommand
 */
final class ContentProbeCommandTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<int,object> */
    private array $posts = [];

    /** @var array<int,array<string,mixed>> */
    private array $meta = [];

    /** @var list<object> Registered abilities. */
    private array $abilities = [];

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-probe-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options   = [];
        $this->posts     = [];
        $this->meta      = [];
        $this->abilities = [];

        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('current_user_can')->justReturn(true);
        Functions\when('register_rest_route')->justReturn(true);

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;
        $GLOBALS['wpdb']                         = new FakeWpdb();
        $this->resetShieldStash();

        $this->router = new Router(new Connector($keystore, new Settings()), [new ContentProbeCommand()]);

        $this->installWordPressFake();
    }

    protected function tear_down(): void
    {
        $this->resetShieldStash();
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    private function installWordPressFake(): void
    {
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('get_post')->alias(fn ($id) => $this->posts[(int) $id] ?? null);
        Functions\when('get_bloginfo')->justReturn('6.9.1');
        Functions\when('post_type_supports')->alias(
            static fn ($type, $feature) => in_array($type, ['post', 'page', 'product'], true)
        );
        Functions\when('has_blocks')->alias(
            static fn ($c) => is_string($c) && str_contains($c, '<!-- wp:')
        );
        Functions\when('metadata_exists')->alias(
            fn ($t, $id, $key) => array_key_exists($key, $this->meta[(int) $id] ?? [])
        );
        Functions\when('get_post_meta')->alias(function ($id, $key = '', $single = false) {
            $all = $this->meta[(int) $id] ?? [];
            if ($key === '') {
                return array_map(static fn ($v) => [$v], $all);
            }
            return $all[$key] ?? '';
        });
        Functions\when('wp_get_post_revisions')->justReturn([]);
        Functions\when('wp_revisions_to_keep')->justReturn(-1);
        Functions\when('get_userdata')->justReturn(false);
        Functions\when('get_posts')->alias(function ($args) {
            $ids = [];
            foreach ($this->posts as $p) {
                if (in_array($p->post_type, $args['post_type'], true) && in_array($p->post_status, $args['post_status'], true)) {
                    $ids[] = $p->ID;
                }
            }
            sort($ids);
            return array_slice($ids, $args['offset'], $args['posts_per_page']);
        });
        // The abilities API is absent on older WordPress: one test runs without it.
        if ($this->name() !== 'test_abilities_api_absent_degrades_cleanly') {
            Functions\when('wp_get_abilities')->alias(fn () => $this->abilities);
        }
    }

    private function seedPost(int $id, string $content = 'Plain words.', string $type = 'page', string $status = 'publish', string $title = 'A title'): void
    {
        $p                    = new \stdClass();
        $p->ID                = $id;
        $p->post_type         = $type;
        $p->post_status       = $status;
        $p->post_title        = $title;
        $p->post_content      = $content;
        $p->post_password     = '';
        $p->post_modified_gmt = '2026-09-01 10:00:00';
        $this->posts[$id]     = $p;
    }

    /**
     * @param array<string,mixed> $overrides Descriptor overrides.
     * @return array<string,mixed>
     */
    private function descriptor(array $overrides = []): array
    {
        return $overrides + [
            'integration_id' => 'acme-builder',
            'status'         => 'detect_only',
            'namespace'      => 'acme-builder',
            'mode_flag'      => ['meta_key' => '_acme_enabled', 'on_values' => ['1']],
            'payload_keys'   => ['_acme_data'],
            'draft_keys'     => ['_acme_draft'],
            'plugin_dir'     => 'acme-builder',
        ];
    }

    /**
     * @param string              $name   Ability name.
     * @param array<string,mixed> $schema Input schema.
     */
    private function registerAbility(string $name, array $schema = []): void
    {
        $this->abilities[] = new class ($name, $schema) {
            /** @param array<string,mixed> $s */
            public function __construct(private string $n, private array $s)
            {
            }

            public function get_name(): string
            {
                return $this->n;
            }

            /** @return array<string,mixed> */
            public function get_input_schema(): array
            {
                return $this->s;
            }
        };
    }

    /**
     * @param array<string,mixed> $params Command body.
     * @return array<string,mixed>
     */
    private function probe(array $params): array
    {
        $request    = $this->signedRequest((string) json_encode($params));
        $authorized = $this->router->authorizeCommand($request, 'content_probe');
        $this->assertTrue($authorized, 'the signed request was not authorized');
        $response = $this->router->handleCommand($request);
        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertIsArray($response->data);

        return $response->data;
    }

    // -------------------------------------------------------------------------
    // Identity
    // -------------------------------------------------------------------------

    public function test_declares_read_and_idempotent_under_its_own_name(): void
    {
        $command = new ContentProbeCommand();
        $this->assertSame('content_probe', $command->name());
        $this->assertSame(CommandEffect::Read, $command->effect());
        $this->assertSame(CommandRepeatability::Idempotent, $command->repeatability());
    }

    // -------------------------------------------------------------------------
    // Verdicts
    // -------------------------------------------------------------------------

    public function test_classic_page_is_route_1_and_returns_no_bodies(): void
    {
        $this->seedPost(10, 'Secret body text.', 'page', 'publish', 'Secret title');

        $r = $this->probe(['post_id' => 10]);

        $this->assertTrue($r['ok']);
        $this->assertSame('classic', $r['verdict']);
        $this->assertSame(1, $r['route']['number']);
        $this->assertSame('content_column', $r['route']['reason']);
        $this->assertSame(strlen('Secret body text.'), $r['post']['content_bytes']);
        $expected = 'sha256:' . hash('sha256', "wpmgr.content_update.v1\n12\nSecret title\n17\nSecret body text.");
        $this->assertSame($expected, $r['fingerprints']['content_v1']);
        $encoded = (string) json_encode($r);
        $this->assertStringNotContainsString('Secret body', $encoded);
        $this->assertStringNotContainsString('Secret title', $encoded);
    }

    public function test_block_page_is_route_3_block_editor(): void
    {
        $this->seedPost(11, '<!-- wp:paragraph --><p>x</p><!-- /wp:paragraph -->');

        $r = $this->probe(['post_id' => 11]);

        $this->assertSame('block_document', $r['verdict']);
        $this->assertSame(3, $r['route']['number']);
        $this->assertSame('block_editor_unsupported', $r['route']['reason']);
    }

    public function test_page_for_posts_is_a_special_page(): void
    {
        $this->seedPost(12);
        $this->options['page_for_posts'] = 12;

        $r = $this->probe(['post_id' => 12]);

        $this->assertSame('special_page', $r['verdict']);
        $this->assertSame(3, $r['route']['number']);
        $this->assertSame('special_page', $r['route']['reason']);
    }

    public function test_descriptor_named_option_marks_a_special_page(): void
    {
        $this->seedPost(13);
        $this->options['acme_shop_page_id'] = '13';

        $r = $this->probe(['post_id' => 13, 'descriptors' => [$this->descriptor(['special_page_options' => ['acme_shop_page_id']])]]);

        $this->assertSame('special_page', $r['verdict']);
    }

    public function test_page_on_front_is_special_only_when_the_front_shows_posts(): void
    {
        $this->seedPost(14);
        $this->options['page_on_front'] = 14;
        $this->options['show_on_front'] = 'page';
        $this->assertSame('classic', $this->probe(['post_id' => 14])['verdict']);

        $this->options['show_on_front'] = 'posts';
        $this->assertSame('special_page', $this->probe(['post_id' => 14])['verdict']);
    }

    public function test_descriptor_matched_builder_page_is_detect_only_route_3_and_names_the_builder(): void
    {
        $this->seedPost(15, '');
        $this->registerAbility('acme-builder/get-page');
        $this->meta[15] = ['_acme_enabled' => '1', '_acme_data' => '{"a":1}'];

        $r = $this->probe([
            'post_id'     => 15,
            'descriptors' => [$this->descriptor(['ability_names' => ['acme-builder/get-page']])],
        ]);

        $this->assertSame('builder', $r['verdict']);
        $this->assertSame('acme-builder', $r['owner']['integration_id']);
        $this->assertSame(3, $r['route']['number']);
        $this->assertSame('builder_not_supported', $r['route']['reason']);
        $this->assertSame('match', $r['matches'][0]['evidence']);
        $this->assertSame(7, $r['matches'][0]['payload_bytes']);
        $this->assertStringNotContainsString('"a":1', (string) json_encode($r));
    }

    public function test_version_constant_named_for_a_version_is_reported_and_others_are_null(): void
    {
        if (!defined('WPMGR_PROBE_TEST_VERSION')) {
            define('WPMGR_PROBE_TEST_VERSION', '3.4.5');
        }
        if (!defined('WPMGR_PROBE_ODD_VERSION')) {
            define('WPMGR_PROBE_ODD_VERSION', 'not a version at all');
        }
        $this->seedPost(60, '');
        $this->meta[60] = ['_acme_enabled' => '1', '_acme_data' => '{"a":1}'];

        $ok  = $this->probe(['post_id' => 60, 'descriptors' => [$this->descriptor(['version_constant' => 'WPMGR_PROBE_TEST_VERSION'])]]);
        $odd = $this->probe(['post_id' => 60, 'descriptors' => [$this->descriptor(['version_constant' => 'WPMGR_PROBE_ODD_VERSION'])]]);

        $this->assertSame('3.4.5', $ok['owner']['version']);
        $this->assertSame('builder', $odd['verdict']);
        $this->assertNull($odd['owner']['version']);
    }

    public function test_mode_flag_is_reported_only_with_a_payload(): void
    {
        $this->seedPost(61, '');
        $this->registerAbility('acme-builder/get-page');
        $this->meta[61] = ['_acme_enabled' => '1'];

        $r = $this->probe(['post_id' => 61, 'descriptors' => [$this->descriptor(['ability_names' => ['acme-builder/get-page']])]]);

        $this->assertSame('flag_without_payload', $r['matches'][0]['evidence']);
        $this->assertFalse($r['matches'][0]['mode_flag']);
    }

    public function test_empty_content_with_a_matching_descriptor_is_a_builder_page_not_empty(): void
    {
        $this->seedPost(70, '');
        $this->meta[70] = ['_acme_enabled' => '1', '_acme_data' => '{"layout":true}'];
        if (!defined('WPMGR_PROBE_B2_VERSION')) {
            define('WPMGR_PROBE_B2_VERSION', '1.2.3');
        }

        $r = $this->probe(['post_id' => 70, 'descriptors' => [$this->descriptor(['version_constant' => 'WPMGR_PROBE_B2_VERSION'])]]);

        $this->assertSame('builder', $r['verdict']);
        $this->assertSame('acme-builder', $r['owner']['integration_id']);
        $this->assertSame(3, $r['route']['number']);
    }

    public function test_empty_content_with_no_match_stays_empty(): void
    {
        $this->seedPost(71, '');
        if (!defined('WPMGR_PROBE_B2_VERSION')) {
            define('WPMGR_PROBE_B2_VERSION', '1.2.3');
        }

        $r = $this->probe(['post_id' => 71, 'descriptors' => [$this->descriptor(['version_constant' => 'WPMGR_PROBE_B2_VERSION'])]]);

        $this->assertSame('empty', $r['verdict']);
        $this->assertSame('empty_page', $r['route']['reason']);
        $this->assertSame([], $r['matches']);
    }

    public function test_stale_payload_is_never_classic(): void
    {
        $this->seedPost(16, 'Looks classic.');
        $this->registerAbility('acme-builder/get-page');
        $this->meta[16] = ['_acme_data' => '{"a":1}'];

        $r = $this->probe(['post_id' => 16, 'descriptors' => [$this->descriptor(['ability_names' => ['acme-builder/get-page']])]]);

        $this->assertSame(3, $r['route']['number']);
        $this->assertSame('stale_payload', $r['matches'][0]['evidence']);
    }

    public function test_site_hint_without_a_live_descriptor_is_unrecognised_builder(): void
    {
        $this->seedPost(17);
        $this->options['active_plugins'] = ['some-builder/some-builder.php'];

        $r = $this->probe(['post_id' => 17, 'indicators' => ['plugin_slugs' => ['some-builder']]]);

        $this->assertSame('unrecognised_builder', $r['verdict']);
        $this->assertSame(3, $r['route']['number']);
        $this->assertSame(['plugin:some-builder'], $r['hints']);
    }

    public function test_hint_with_a_verified_clean_descriptor_stays_route_1(): void
    {
        $this->seedPost(18);
        $this->options['active_plugins'] = ['acme-builder/acme.php'];
        $this->registerAbility('acme-builder/get-page');

        $r = $this->probe([
            'post_id'     => 18,
            'indicators'  => ['plugin_slugs' => ['acme-builder']],
            'descriptors' => [$this->descriptor(['verified' => true, 'singular_override' => 'none', 'ability_names' => ['acme-builder/get-page']])],
        ]);

        $this->assertSame('classic', $r['verdict']);
        $this->assertSame(1, $r['route']['number']);
    }

    public function test_hint_whose_descriptor_may_override_the_template_is_route_3(): void
    {
        $this->seedPost(19);
        $this->options['active_plugins'] = ['acme-builder/acme.php'];
        $this->registerAbility('acme-builder/get-page');

        $r = $this->probe([
            'post_id'     => 19,
            'indicators'  => ['plugin_slugs' => ['acme-builder']],
            'descriptors' => [$this->descriptor(['verified' => true, 'singular_override' => 'unknown', 'ability_names' => ['acme-builder/get-page']])],
        ]);

        $this->assertSame('template_may_override', $r['verdict']);
        $this->assertSame(3, $r['route']['number']);
    }

    public function test_post_refusals(): void
    {
        $this->seedPost(20, 'x', 'page', 'trash');
        $this->seedPost(21, 'x', 'product');
        $this->seedPost(22, 'x', 'nav_menu_item');

        $this->assertSame('post_not_found', $this->probe(['post_id' => 999])['code']);
        $this->assertSame('post_in_trash', $this->probe(['post_id' => 20])['code']);
        $this->assertSame('post_type_not_allowed', $this->probe(['post_id' => 21])['code']);
        $this->assertSame('post_type_not_content', $this->probe(['post_id' => 22, 'allowed_post_types' => ['nav_menu_item']])['code']);
    }

    // -------------------------------------------------------------------------
    // List mode
    // -------------------------------------------------------------------------

    public function test_list_mode_pages_and_returns_titles_for_published_rows_only(): void
    {
        $this->seedPost(30, 'a', 'page', 'publish', 'Public one');
        $this->seedPost(31, 'b', 'page', 'draft', 'Unreleased draft title');
        $this->seedPost(32, 'c', 'page', 'publish', str_repeat('é', 100));
        $this->seedPost(33, 'd', 'page', 'publish', 'Public three');

        $first = $this->probe(['list' => ['types' => ['page'], 'status' => ['publish', 'draft'], 'limit' => 2, 'offset' => 0]]);

        $this->assertTrue($first['ok']);
        $this->assertCount(2, $first['rows']);
        $this->assertSame(2, $first['next_offset']);
        $this->assertSame('Public one', $first['rows'][0]['title']);
        $this->assertNull($first['rows'][1]['title'], 'a draft title must never leave the site');
        $this->assertStringNotContainsString('Unreleased', (string) json_encode($first));

        $second = $this->probe(['list' => ['types' => ['page'], 'status' => ['publish', 'draft'], 'limit' => 2, 'offset' => 2]]);
        $this->assertCount(2, $second['rows']);
        $this->assertLessThanOrEqual(120, strlen($second['rows'][0]['title']));
        $this->assertSame(1, preg_match('//u', $second['rows'][0]['title']));
        $this->assertSame('classic', $second['rows'][0]['verdict']);
        $this->assertSame(1, $second['rows'][0]['route']['number']);
    }

    // -------------------------------------------------------------------------
    // Validation
    // -------------------------------------------------------------------------

    /**
     * @dataProvider provideInvalidDescriptors
     * @param array<string,mixed> $override Descriptor overrides.
     */
    public function test_invalid_descriptors_are_refused(array $override): void
    {
        $this->seedPost(40);
        $r = $this->probe(['post_id' => 40, 'descriptors' => [$this->descriptor(), $this->descriptor(['integration_id' => 'other'] + $override)]]);

        $this->assertFalse($r['ok']);
        $this->assertSame('invalid_descriptor', $r['code']);
        $this->assertSame(1, $r['index']);
    }

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function provideInvalidDescriptors(): array
    {
        return [
            'lowercase constant' => [['version_constant' => 'acme_version']],
            'credential constant' => [['version_constant' => 'DB_PASSWORD']],
            'salt constant'      => [['version_constant' => 'AUTH_SALT']],
            'not version named'  => [['version_constant' => 'ABSPATH']],
            'secret version name' => [['version_constant' => 'STRIPE_SECRET_VERSION']],
            'key version name'   => [['version_constant' => 'API_KEY_VERSION']],
            'bad meta key'       => [['payload_keys' => ['bad key!']]],
            'bad ability name'   => [['ability_names' => ['Acme/Get']]],
            'callable field'     => [['callback' => 'system']],
            'override check'     => [['override_check' => 'is_admin']],
            'bad status'         => [['status' => 'trusted']],
            'flag on_values'     => [['mode_flag' => ['meta_key' => '_x', 'on_values' => []]]],
        ];
    }

    public function test_too_many_descriptors_and_bad_params(): void
    {
        $this->seedPost(41);
        $many = [];
        for ($i = 0; $i < 33; $i++) {
            $many[] = $this->descriptor(['integration_id' => 'b' . $i]);
        }
        $this->assertSame('too_many_descriptors', $this->probe(['post_id' => 41, 'descriptors' => $many])['code']);
        $this->assertSame('invalid_params', $this->probe(['post_id' => 0])['code']);
        $this->assertSame('invalid_params', $this->probe(['post_id' => 41, 'list' => []])['code']);
        $this->assertSame('invalid_params', $this->probe(['list' => ['limit' => 201]])['code']);
        $this->assertSame('invalid_params', $this->probe(['post_id' => 41, 'extra' => 1])['code']);
    }

    public function test_invalid_descriptor_echoes_at_most_64_bytes(): void
    {
        $r = $this->probe(['post_id' => 1, 'descriptors' => [$this->descriptor([str_repeat('k', 300) => 1])]]);

        $this->assertSame('invalid_descriptor', $r['code']);
        $this->assertLessThan(160, strlen($r['detail']));
    }

    // -------------------------------------------------------------------------
    // Abilities API
    // -------------------------------------------------------------------------

    public function test_ability_schema_hash_is_structural(): void
    {
        $this->seedPost(50);
        $d = $this->descriptor(['ability_names' => ['acme-builder/get-page']]);
        $this->registerAbility('acme-builder/get-page', ['type' => 'object', 'description' => 'One', 'properties' => ['id' => ['type' => 'integer', 'title' => 'A']]]);
        $a = $this->probe(['post_id' => 50, 'descriptors' => [$d]])['abilities']['allowlisted'][0];

        $this->abilities = [];
        $this->registerAbility('acme-builder/get-page', ['type' => 'object', 'description' => 'Uno', 'properties' => ['id' => ['type' => 'integer', 'title' => 'B']]]);
        $b = $this->probe(['post_id' => 50, 'descriptors' => [$d]])['abilities']['allowlisted'][0];

        $this->abilities = [];
        $this->registerAbility('acme-builder/get-page', ['type' => 'object', 'properties' => ['id' => ['type' => 'string']]]);
        $c = $this->probe(['post_id' => 50, 'descriptors' => [$d]])['abilities']['allowlisted'][0];

        $this->assertTrue($a['registered']);
        $this->assertSame($a['schema_struct_sha256'], $b['schema_struct_sha256']);
        $this->assertNotSame($a['schema_struct_sha256'], $c['schema_struct_sha256']);
    }

    /**
     * Runs in its own process: once any test has defined wp_get_abilities the
     * function cannot be removed again, and this test is about it not existing.
     *
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_abilities_api_absent_degrades_cleanly(): void
    {
        $this->assertFalse(function_exists('wp_get_abilities'), 'this process must not have the abilities API');
        $this->seedPost(51);

        $r = $this->probe(['post_id' => 51, 'descriptors' => [$this->descriptor(['ability_names' => ['acme-builder/get-page']])]]);

        $this->assertTrue($r['ok']);
        $this->assertFalse($r['abilities']['api_present']);
        $this->assertFalse($r['abilities']['filters_71']);
        $this->assertFalse($r['abilities']['allowlisted'][0]['registered']);
        $this->assertSame('classic', $r['verdict']);
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function signedRequest(string $body): \WP_REST_Request
    {
        $request = new \WP_REST_Request('POST', '/wpmgr/v1/command/content_probe');
        $request->set_url_params(['command' => 'content_probe']);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Accept', 'application/json');
        $request->set_header('Authorization', 'Bearer ' . $this->mintToken());
        $request->set_body($body);

        return $request;
    }

    private function mintToken(): string
    {
        $segments = [
            $this->b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])),
            $this->b64((string) json_encode([
                'aud' => $this->siteId,
                'cmd' => 'content_probe',
                'jti' => bin2hex(random_bytes(8)),
                'exp' => time() + 30,
            ])),
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
