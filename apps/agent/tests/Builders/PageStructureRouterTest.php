<?php
/**
 * wpmgr/page-structure driven through the real signed Router dispatch: the
 * catalogue entry and its builders_enabled, p.allowed_draft_ids, the input
 * schema, and the read of an eligible draft and of a published page.
 *
 * Elementor is FakeElementorApi behind the real ElementorAdapter, handed to
 * the command through its test-only adapter seam. Post and meta rows live in
 * FakeBuilderWpdb and are read back through the SQL reads production uses;
 * the ledger's rows are this test's options.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionProperty;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Commands\AbilityRunCommand;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AbilityRunCommand
 * @covers \WPMgr\Agent\Abilities\OwnAbilities
 * @covers \WPMgr\Agent\Abilities\Builders\BuilderPageStructure
 * @covers \WPMgr\Agent\Abilities\Builders\DraftEligibility
 */
final class PageStructureRouterTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/elementor-classic-containers.json';

    /** The draft WPMgr created. */
    private const DRAFT = 120;

    /** A published Elementor page. */
    private const PUBLISHED = 130;

    /** The page-create request that created DRAFT. */
    private const REQ = '11111111-2222-4333-8444-777777777777';

    /** The read's own request id, as the control plane sends one. */
    private const READ_REQ = '33333333-2222-4333-8444-666666666666';

    private string $keyFile;

    private string $cpSecret;

    private string $siteId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

    private Router $router;

    private FakeElementorApi $api;

    private FakeBuilderWpdb $rows;

    /** @var array<string,mixed> */
    private array $options = [];

    private int $metaId = 10;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-bfe-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        Functions\when('add_filter')->justReturn(true);
        Functions\when('add_action')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        Functions\when('remove_action')->justReturn(true);
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
        Functions\when('get_userdata')->justReturn(false);
        $GLOBALS['wp_version'] = '6.4.2';

        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $keystore       = new Keystore();
        $keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($keypair));
        $this->options[Settings::OPTION_SITE_ID] = $this->siteId;

        $this->rows      = new FakeBuilderWpdb();
        $GLOBALS['wpdb'] = $this->wpdb($this->rows);

        $this->page(self::DRAFT, 'draft');
        $this->rows->addMeta(++$this->metaId, self::DRAFT, DraftEligibility::MARKER_KEY, self::REQ);
        $this->options['wpmgr_ability_ledger_' . self::REQ] = [
            'request_id'      => self::REQ,
            'ability'         => OwnAbilities::NAME_PAGE_CREATE,
            'phase'           => 'completed',
            'created_post_id' => self::DRAFT,
            'undo_state'      => 'available',
            'builder'         => 'elementor',
        ];
        $this->page(self::PUBLISHED, 'publish');

        $this->api = new FakeElementorApi();
        $this->resetShieldStash();
        $api          = $this->api;
        $this->router = new Router(
            new Connector($keystore, new Settings()),
            [new AbilityRunCommand(static fn (): array => ['elementor' => new ElementorAdapter($api)])]
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

    public function test_reads_the_draft_only_when_the_signed_list_names_it(): void
    {
        $r = $this->read('{"post_id":' . self::DRAFT . '}', [self::DRAFT]);
        $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
        $this->assertSame(['completed', 'read', OwnAbilities::NAME_PAGE_STRUCTURE, hash('sha256', $this->entry())], [$r['outcome'], $r['mode'], $r['ability'], $r['entry_sha256']]);
        $out = $r['output'];
        $this->assertSame([self::DRAFT, 'elementor', 'classic', 'draft', true, 5], [$out['post_id'], $out['builder'], $out['format'], $out['status'], $out['editable'], $out['node_count']]);
        $this->assertSame(BuilderDocumentFingerprint::ofPost(self::DRAFT, ElementorDocument::DESCRIPTOR_KEYS), $out['base_fingerprint']);
        $this->assertSame(['text'], $out['nodes'][2]['editable']);

        // Without the control plane naming it, the agent's own records are
        // not enough.
        foreach (['no allowed_draft_ids' => null, 'an empty list' => [], 'another post' => [self::DRAFT + 1]] as $why => $allowed) {
            $refused = $this->read('{"post_id":' . self::DRAFT . '}', $allowed);
            $this->assertSame(['post_not_readable', 'not_in_signed_list', false], [$refused['code'] ?? null, $refused['detail'] ?? null, $refused['ok'] ?? null], $why . ': ' . json_encode($refused));
            $this->assertArrayNotHasKey('output', $refused, $why);
        }

        // A subtree, under the same rule.
        $sub = $this->read('{"post_id":' . self::DRAFT . ',"node":"c87900b","max_nodes":1}', [self::DRAFT]);
        $this->assertSame([['c87900b'], 2, true], [array_column($sub['output']['nodes'] ?? [], 'ref'), $sub['output']['node_count'] ?? null, $sub['output']['truncated'] ?? null], (string) json_encode($sub));
        $this->assertSame('node_not_found', $this->read('{"post_id":' . self::DRAFT . ',"node":"fffffff"}', [self::DRAFT])['code'] ?? null);
    }

    public function test_published_page_reads_without_the_list(): void
    {
        foreach ([null, [], [self::PUBLISHED]] as $allowed) {
            $r = $this->read('{"post_id":' . self::PUBLISHED . '}', $allowed);
            $this->assertTrue($r['ok'] ?? null, (string) json_encode($r));
            $this->assertSame([false, 'publish'], [$r['output']['editable'], $r['output']['status']]);
            $this->assertSame([[], [], [], [], []], array_column($r['output']['nodes'], 'editable'));
        }
        $missing = $this->read('{"post_id":999}', [999]);
        $this->assertSame(['post_not_readable', 'missing'], [$missing['code'] ?? null, $missing['detail'] ?? null]);
    }

    public function test_malformed_allowed_draft_ids_is_bad_params(): void
    {
        foreach (['[' . self::DRAFT . ',' . self::PUBLISHED . ']', '["' . self::DRAFT . '"]', '[' . self::DRAFT . '.0]', '[0]', '{}', 'null', (string) self::DRAFT] as $ids) {
            $p = $this->p('{"post_id":' . self::DRAFT . '}', null, $ids);
            $r = $this->callP($p);
            $this->assertSame(['bad_params', false], [$r['code'] ?? null, $r['ok'] ?? null], $ids . ': ' . json_encode($r));
            $this->assertArrayNotHasKey('output', $r, $ids);
        }
    }

    public function test_entry_and_input_are_checked_first(): void
    {
        $noBuilders = $this->read('{"post_id":' . self::PUBLISHED . '}', [], ['builders_enabled' => []]);
        $this->assertSame('builder_not_enabled', $noBuilders['code'] ?? null, (string) json_encode($noBuilders));
        $notCompiled = $this->read('{"post_id":' . self::PUBLISHED . '}', [], ['builders_enabled' => ['beaver']]);
        $this->assertSame(['builder_not_available', 'not_compiled'], [$notCompiled['code'] ?? null, $notCompiled['detail'] ?? null]);
        $malformed = $this->read('{"post_id":' . self::PUBLISHED . '}', [], ['builders_enabled' => 'elementor']);
        $this->assertSame('bad_input', $malformed['code'] ?? null);

        foreach (['{}', '{"post_id":0}', '{"post_id":"130"}', '{"post_id":130,"node":"a b"}', '{"post_id":130,"max_nodes":501}', '{"post_id":130,"editor":"builder:elementor"}', '[]'] as $input) {
            $this->assertSame('bad_input', $this->read($input, [])['code'] ?? null, $input);
        }

        $this->assertSame('mode_class_mismatch', $this->callP($this->p('{"post_id":' . self::DRAFT . '}', null, '[' . self::DRAFT . ']', 'write'))['code'] ?? null, 'a read ability is never written');
    }

    // ---- helpers -----------------------------------------------------------

    /** An Elementor page holding the golden two-column tree. */
    private function page(int $id, string $status): void
    {
        $this->rows->addPost($id, [
            'post_type'         => 'page',
            'post_status'       => $status,
            'post_title'        => 'Our services',
            'post_content'      => '',
            'post_modified_gmt' => '2026-10-09 07:00:05',
        ]);
        $fixture = json_decode((string) file_get_contents(self::FIXTURE), true, 512, JSON_THROW_ON_ERROR);
        $tree    = array_values(array_filter($fixture['cases'], static fn (array $c): bool => $c['name'] === 'columns'))[0]['tree'];
        $this->rows->addMeta(++$this->metaId, $id, ElementorDocument::KEY_EDIT_MODE, 'builder');
        $this->rows->addMeta(++$this->metaId, $id, ElementorDocument::KEY_TEMPLATE_TYPE, 'wp-page');
        $this->rows->addMeta(++$this->metaId, $id, ElementorDocument::KEY_DATA, (string) json_encode($tree));
    }

    /**
     * The page-structure entry as the control plane sends it.
     *
     * @param array<string,mixed>|null $limits The entry's limits; null for builders_enabled ["elementor"].
     */
    private function entry(?array $limits = null): string
    {
        return (string) json_encode([
            'name'          => OwnAbilities::NAME_PAGE_STRUCTURE,
            'source'        => 'wpmgr',
            'class'         => 'read',
            'status'        => 'admitted',
            'enabled'       => true,
            'approval_mode' => 'none',
            'snapshot'      => 'none',
            'limits'        => $limits ?? ['builders_enabled' => ['elementor']],
        ]);
    }

    /**
     * @param list<int>|null           $allowed The signed list; null leaves allowed_draft_ids out.
     * @param array<string,mixed>|null $limits  The entry's limits.
     * @return array<string,mixed>
     */
    private function read(string $input, ?array $allowed, ?array $limits = null): array
    {
        return $this->callP($this->p($input, $limits, $allowed === null ? null : (string) json_encode($allowed)));
    }

    /**
     * p as the control plane builds it, with allowed_draft_ids as the given
     * JSON text (left out when null).
     *
     * @param array<string,mixed>|null $limits The entry's limits.
     */
    private function p(string $input, ?array $limits, ?string $idsJson, string $mode = 'read'): string
    {
        $entry = $this->entry($limits);
        $p     = (string) json_encode(['mode' => $mode, 'request_id' => self::READ_REQ, 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry), 'input' => $input]);

        return $idsJson === null ? $p : substr($p, 0, -1) . ',"allowed_draft_ids":' . $idsJson . '}';
    }

    /**
     * @return array<string,mixed>
     */
    private function callP(string $p): array
    {
        $request = new \WP_REST_Request('POST', '/wpmgr/v1/command/ability_run');
        $request->set_url_params(['command' => 'ability_run']);
        $request->set_header('Content-Type', 'application/json');
        $request->set_header('Accept', 'application/json');
        $request->set_header('Authorization', 'Bearer ' . $this->mintToken('ability_run', hash('sha256', $p)));
        $request->set_body((string) json_encode(['p' => $p]));

        $this->assertTrue($this->router->authorizeCommand($request, 'ability_run'), 'the signed request was not authorized');
        $response = $this->router->handleCommand($request);
        $this->assertInstanceOf(\WP_REST_Response::class, $response);
        $this->assertIsArray($response->data);

        return $response->data;
    }

    private function mintToken(string $cmd, string $pd): string
    {
        $claims     = ['aud' => $this->siteId, 'cmd' => $cmd, 'jti' => bin2hex(random_bytes(8)), 'exp' => time() + 30, 'pd' => $pd];
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
     * The post and meta reads go to FakeBuilderWpdb; the token replay
     * record and its prune are accepted, as PageCreateWriteTest has them.
     */
    private function wpdb(FakeBuilderWpdb $rows): object
    {
        return new class ($rows) {
            public string $prefix     = 'wp_';
            public string $options    = 'wp_options';
            public string $posts      = 'wp_posts';
            public string $postmeta   = 'wp_postmeta';
            public string $last_error = '';

            public function __construct(private FakeBuilderWpdb $rows)
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
                return null;
            }

            public function query(string $q): int
            {
                return 0;
            }

            /**
             * @param array<string,mixed> $row Row.
             */
            public function insert(string $table, array $row, $format = null): int
            {
                return 1;
            }
        };
    }
}
