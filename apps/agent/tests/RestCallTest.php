<?php
/**
 * E3-A3: wpmgr/rest-read and wpmgr/rest-write, RC1, the REST guards and the
 * post_fields write, driven through AbilityRunCommand::execute() with the pd
 * claim, against a REST server double that runs core's dispatch order.
 *
 * Route handlers that stand for core's live in a file under the test
 * install's wp-includes, so the ownership check classifies them as core,
 * exactly as it does a real core controller. A handler defined anywhere else
 * classifies as not core.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\RestCall;
use WPMgr\Agent\Abilities\ServicePrincipal;
use WPMgr\Agent\Commands\AbilityRunCommand;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Router;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\AuthHeaderShield;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\AbilityRunCommand
 * @covers \WPMgr\Agent\Abilities\RestCall
 * @covers \WPMgr\Agent\Abilities\RestGuards
 */
final class RestCallTest extends TestCase
{
    private const REQ = '22222222-3333-4444-8555-666666666666';

    private const PRINCIPAL = 7;

    /** @var array<string,mixed> */
    public array $options = [];

    /** @var array<int,object> */
    public array $posts = [];

    /** @var array<string,string|null> Raw option rows the tripwires read. */
    public array $rawOptions = [];

    public string $othersModified = '2026-01-01 00:00:00';

    private int $currentUser = 0;

    public \WP_REST_Server $server;

    /** @var list<string> What the core-standing handlers did, in order. */
    public array $calls = [];

    /** @var array<string,mixed>|null The ledger row the update handler saw. */
    public ?array $ledgerAtEffect = null;

    /** A transform the update handler applies to the title it stores. */
    public ?\Closure $mangle = null;

    /** A side effect the update handler also performs. */
    public ?\Closure $alsoDo = null;

    /** @var array<int,int> post id => newest revision id */
    public array $revisions = [];

    /** @var array<int,object|false> */
    public array $autosaves = [];

    /** @var array<int,bool> */
    public array $locks = [];

    private bool $canEdit = true;

    /** A hostile filter that grants every capability. */
    public bool $grantAll = false;

    /** Runs whenever a save filter is simulated. */
    public \Closure $duringSanitise;

    /** @var array<int,array<string,list<string>>> */
    public array $meta = [];

    private int $clock = 0;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $GLOBALS['wp_filter']        = [];
        $GLOBALS['wp_version']       = '7.1';
        $GLOBALS['wpmgr_rest_test']  = $this;
        $this->options               = [ServicePrincipal::OPTION_USER_ID => self::PRINCIPAL];
        $this->posts                 = [];
        $this->calls                 = [];
        $this->rawOptions            = ['admin_email' => 'owner@example.test', 'siteurl' => 'https://example.test'];
        $this->ledgerAtEffect        = null;
        $this->mangle                = null;
        $this->alsoDo                = null;
        $this->revisions             = [];
        $this->autosaves             = [];
        $this->locks                 = [];
        $this->canEdit               = true;
        $this->grantAll              = false;
        $this->duringSanitise        = static function (): void {
        };
        $this->meta                  = [];
        $this->currentUser           = 0;

        $this->installCoreHandlers();
        $this->server = new \WP_REST_Server();
        $this->registerPageRoutes();

        $role               = new \stdClass();
        $role->capabilities = array_fill_keys(ServicePrincipal::CAPS, true);
        $user               = new \stdClass();
        $user->ID           = self::PRINCIPAL;
        $user->user_login   = ServicePrincipal::USER_LOGIN;
        $user->roles        = [ServicePrincipal::ROLE];
        $user->caps         = [ServicePrincipal::ROLE => true];

        Functions\when('get_option')->alias(fn ($n, $d = false) => array_key_exists($n, $this->options) ? $this->options[$n] : $d);
        Functions\when('add_option')->alias(function ($n, $v = '') {
            if (array_key_exists($n, $this->options)) {
                return false;
            }
            $this->options[$n] = $v;
            return true;
        });
        Functions\when('update_option')->alias(function ($n, $v) {
            $this->options[$n] = $v;
            return true;
        });
        Functions\when('get_role')->alias(fn ($r) => $r === ServicePrincipal::ROLE ? $role : null);
        Functions\when('get_userdata')->alias(fn ($id) => (int) $id === self::PRINCIPAL ? $user : false);
        Functions\when('wp_set_current_user')->alias(function ($id) {
            $this->currentUser = (int) $id;
            return null;
        });
        Functions\when('get_current_user_id')->alias(fn () => $this->currentUser);
        Functions\when('current_user_can')->alias(function ($cap, ...$args) {
            if ($this->currentUser !== self::PRINCIPAL) {
                return false;
            }
            if ($this->grantAll) {
                return true;
            }
            if ($cap === 'edit_post') {
                return $this->canEdit;
            }
            return in_array($cap, ServicePrincipal::CAPS, true);
        });
        Functions\when('add_filter')->alias(function ($tag, $cb, $prio = 10, $args = 1) {
            $hook = $GLOBALS['wp_filter'][$tag] ?? null;
            if (!is_object($hook)) {
                $hook = new \stdClass();
                $hook->callbacks = [];
                $GLOBALS['wp_filter'][$tag] = $hook;
            }
            $hook->callbacks[(int) $prio][] = ['function' => $cb, 'accepted_args' => (int) $args];
            ksort($hook->callbacks);
            return true;
        });
        Functions\when('remove_filter')->alias(function ($tag, $cb, $prio = 10) {
            $hook = $GLOBALS['wp_filter'][$tag] ?? null;
            if (!is_object($hook) || !isset($hook->callbacks[(int) $prio])) {
                return false;
            }
            foreach ($hook->callbacks[(int) $prio] as $i => $e) {
                if ($e['function'] === $cb) {
                    unset($hook->callbacks[(int) $prio][$i]);
                }
            }
            $hook->callbacks[(int) $prio] = array_values($hook->callbacks[(int) $prio]);
            if ($hook->callbacks[(int) $prio] === []) {
                unset($hook->callbacks[(int) $prio]);
            }
            return true;
        });
        Functions\when('rest_get_server')->alias(fn () => $this->server);
        Functions\when('rest_do_request')->alias(fn ($r) => $this->server->dispatch($r));
        Functions\when('clean_post_cache')->justReturn(null);
        Functions\when('get_post')->alias(fn ($id) => isset($this->posts[(int) $id]) ? clone $this->posts[(int) $id] : null);
        Functions\when('get_post_status')->alias(fn ($id) => isset($this->posts[(int) $id]) ? $this->posts[(int) $id]->post_status : false);
        Functions\when('wp_slash')->alias(fn ($v) => is_array($v) ? array_map(fn ($x) => is_string($x) ? addslashes($x) : $x, $v) : (is_string($v) ? addslashes($v) : $v));
        Functions\when('wp_unslash')->alias(fn ($v) => is_string($v) ? stripslashes($v) : $v);
        // The principal has no unfiltered_html, so the save filters run kses.
        // Entity normalisation as kses does it: a bare '&' becomes '&amp;'.
        Functions\when('sanitize_post_field')->alias(function ($field, $value) {
            ($this->duringSanitise)();
            return self::kses((string) $value);
        });
        Functions\when('wp_kses_post')->alias(fn ($v) => self::kses((string) $v));
        // As core: the existing row is merged with the change and the whole
        // merged row is re-sanitised as the current user (no unfiltered_html),
        // then wp_insert_post_data may rewrite it.
        Functions\when('wp_update_post')->alias(function (array $data) {
            ($this->duringSanitise)();
            $id     = (int) $data['ID'];
            $old    = $this->posts[$id];
            // As core: a never-dated draft is dated now on every update
            // unless edit_date is set.
            $clear  = in_array($old->post_status, ['draft', 'pending', 'auto-draft'], true)
                && empty($data['edit_date']) && ($old->post_date_gmt ?? '') === '0000-00-00 00:00:00';
            $merged = array_merge(get_object_vars($old), array_map(fn ($v) => is_string($v) ? stripslashes($v) : $v, $data));
            if ($clear) {
                $merged['post_date'] = '2026-10-02 12:' . sprintf('%02d', ++$this->clock) . ':00';
            }
            unset($merged['ID']);
            foreach ($merged as $k => $v) {
                if (is_string($v)) {
                    $merged[$k] = self::kses($v);
                }
            }
            $merged = \WP_REST_Server::applyHooks('wp_insert_post_data', $merged, $data);
            foreach ($merged as $k => $v) {
                $this->posts[$id]->{$k} = $v;
            }
            $this->posts[$id]->post_modified_gmt = '2026-10-02 12:00:' . sprintf('%02d', count($this->calls) + 10);
            return $id;
        });
        Functions\when('get_post_meta')->alias(fn ($id) => $this->meta[(int) $id] ?? []);
        Functions\when('get_post_type')->alias(fn ($id) => isset($this->posts[(int) $id]) ? $this->posts[(int) $id]->post_type : false);
        Functions\when('get_object_taxonomies')->justReturn([]);
        Functions\when('wp_get_object_terms')->justReturn([]);
        Functions\when('wp_check_post_lock')->alias(fn ($id) => !empty($this->locks[(int) $id]) ? 99 : false);
        Functions\when('wp_get_post_revisions')->alias(fn ($id) => isset($this->revisions[(int) $id]) ? [$this->revisions[(int) $id] => new \stdClass()] : []);
        Functions\when('wp_get_post_autosave')->alias(fn ($id) => $this->autosaves[(int) $id] ?? false);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('wp_parse_url')->alias(fn ($u, $c = -1) => parse_url($u, $c));

        $test = $this;
        $GLOBALS['wpmgr_rest_test'] = $test;
        $GLOBALS['wpdb'] = new class ($test) {
            public string $prefix   = 'wp_';
            public string $options  = 'wp_options';
            public string $posts    = 'wp_posts';
            public string $usermeta = 'wp_usermeta';

            public function __construct(private RestCallTest $t)
            {
            }

            public function prepare(string $q, ...$a): string
            {
                return (string) json_encode(['q' => $q, 'a' => $a]);
            }

            public string $last_error = '';

            /** @param array<string,mixed> $row */
            public function insert(string $t, array $row, $f = null): int
            {
                return 1;
            }

            public function esc_like(string $s): string
            {
                return $s;
            }

            public function query(string $p): int
            {
                $d = json_decode($p, true);
                $q = $d['q'];
                $a = $d['a'];
                if (str_starts_with($q, 'INSERT IGNORE')) {
                    if (array_key_exists($a[0], $this->t->options)) {
                        return 0;
                    }
                    $this->t->options[$a[0]] = $a[1];
                    return 1;
                }
                if (str_starts_with($q, 'DELETE FROM wp_options WHERE option_name = %s AND option_value = %s')) {
                    if (($this->t->options[$a[0]] ?? null) === $a[1]) {
                        unset($this->t->options[$a[0]]);
                        return 1;
                    }
                    return 0;
                }
                return 0;
            }

            /** @return mixed */
            public function get_var(string $p)
            {
                $d = json_decode($p, true);
                $q = $d['q'];
                $a = $d['a'];
                if (str_contains($q, 'MAX(post_modified_gmt)')) {
                    return $this->t->othersModified;
                }
                if (str_contains($q, 'COUNT(*)')) {
                    return '1';
                }
                if (str_contains($q, 'FROM wp_options')) {
                    $v = $this->t->rawOptions[$a[0]] ?? ($this->t->options[$a[0]] ?? null);
                    return is_scalar($v) ? (string) $v : null;
                }
                return null;
            }
        };
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb'], $GLOBALS['wp_filter'], $GLOBALS['wpmgr_rest_test']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // ------------------------------------------------------------------
    // Harness
    // ------------------------------------------------------------------

    /** kses entity normalisation, reduced: a '&' that starts no entity becomes '&amp;'. */
    /**
     * kses as a user without unfiltered_html, reduced to what these tests
     * need: script, iframe, form and input are removed; inline tags such as
     * b survive; a bare '&' becomes '&amp;'. Brackets are left alone.
     */
    public static function kses(string $v): string
    {
        $v = (string) preg_replace('#<script\b[^>]*>.*?</script>#is', '', $v);
        $v = (string) preg_replace('#</?(iframe|form|input)\b[^>]*>#i', '', $v);

        return self::ksesEntities($v);
    }

    public static function ksesEntities(string $v): string
    {
        return (string) preg_replace('/&(?![A-Za-z][A-Za-z0-9]{0,31};|#[0-9]{1,7};|#x[0-9A-Fa-f]{1,6};)/', '&amp;', $v);
    }

    private function installCoreHandlers(): void
    {
        $dir = rtrim((string) ABSPATH, '/') . '/wp-includes';
        if (!is_dir($dir)) {
            mkdir($dir, 0777, true);
        }
        $file = $dir . '/wpmgr-test-rest-core.php';
        if (!function_exists('wpmgr_test_core_rest_get')) {
            file_put_contents($file, implode("\n", [
                '<?php',
                'function wpmgr_test_core_rest_permission($r) { return $GLOBALS["wpmgr_rest_test"]->corePermission($r); }',
                'function wpmgr_test_core_rest_get($r) { return $GLOBALS["wpmgr_rest_test"]->coreGet($r); }',
                'function wpmgr_test_core_rest_list($r) { return $GLOBALS["wpmgr_rest_test"]->coreList($r); }',
                'function wpmgr_test_core_rest_update($r) { return $GLOBALS["wpmgr_rest_test"]->coreUpdate($r); }',
                '',
            ]));
            require_once $file;
        }
    }

    private function registerPageRoutes(): void
    {
        $this->server->register_route('/wp/v2/pages', ['methods' => ['GET' => true], 'callback' => 'wpmgr_test_core_rest_list', 'permission_callback' => 'wpmgr_test_core_rest_permission']);
        $this->server->register_route('/wp/v2/media', ['methods' => ['GET' => true], 'callback' => 'wpmgr_test_core_rest_list', 'permission_callback' => 'wpmgr_test_core_rest_permission']);
        $this->server->register_route('/wp/v2/pages/(?P<id>[\d]+)', ['methods' => ['GET' => true], 'callback' => 'wpmgr_test_core_rest_get', 'permission_callback' => 'wpmgr_test_core_rest_permission']);
        $this->server->register_route('/wp/v2/pages/(?P<id>[\d]+)', ['methods' => ['POST' => true, 'PUT' => true, 'PATCH' => true], 'callback' => 'wpmgr_test_core_rest_update', 'permission_callback' => 'wpmgr_test_core_rest_permission']);
    }

    /** @return bool */
    public function corePermission($r): bool
    {
        $this->calls[] = 'permission';
        return true;
    }

    /** @return array<string,mixed>|\WP_Error */
    public function coreGet($r)
    {
        $this->calls[] = 'get';
        $id = (int) ($r->get_url_params()['id'] ?? 0);
        $p  = $this->posts[$id] ?? null;
        if ($p === null) {
            return new \WP_Error('rest_post_invalid_id', 'Invalid post ID.', ['status' => 404]);
        }
        return $this->restShape($p);
    }

    /** @return list<array<string,mixed>> */
    public function coreList($r): array
    {
        $this->calls[] = 'list';
        $out = [];
        foreach ($this->posts as $p) {
            $out[] = $this->restShape($p);
        }
        return $out;
    }

    /** @return array<string,mixed>|\WP_Error */
    public function coreUpdate($r)
    {
        $this->calls[] = 'update';
        $this->ledgerAtEffect = AbilityLedger::get(self::REQ);
        $id   = (int) ($r->get_url_params()['id'] ?? 0);
        $body = $r->get_body_params();
        $data = ['ID' => $id];
        if (isset($body['title'])) {
            $data['post_title'] = addslashes($this->mangle !== null ? ($this->mangle)($body['title']) : $body['title']);
        }
        if (isset($body['excerpt'])) {
            $data['post_excerpt'] = addslashes($body['excerpt']);
        }
        wp_update_post($data);
        if ($this->alsoDo !== null) {
            ($this->alsoDo)();
        }
        return $this->restShape($this->posts[$id]);
    }

    /** @return array<string,mixed> */
    private function restShape(object $p): array
    {
        return [
            'id'           => (int) $p->ID,
            'status'       => (string) $p->post_status,
            'type'         => (string) $p->post_type,
            'modified_gmt' => (string) $p->post_modified_gmt,
            'link'         => 'https://example.test/?p=' . $p->ID,
            'password'     => 'leak',
            'guid'         => ['rendered' => 'x', 'raw' => 'secret'],
            'post'         => $p->post_parent ?? null,
            'title'        => ['rendered' => (string) $p->post_title, 'raw' => (string) $p->post_title],
            'excerpt'      => ['rendered' => (string) $p->post_excerpt, 'raw' => (string) $p->post_excerpt],
        ];
    }

    private function addPost(int $id, string $status = 'publish', string $type = 'page', string $title = 'Old title', ?int $parent = null): void
    {
        $p                    = new \stdClass();
        $p->ID                = $id;
        $p->post_type         = $type;
        $p->post_status       = $status;
        $p->post_title        = $title;
        $p->post_excerpt      = 'Old excerpt';
        $p->post_modified_gmt = '2026-09-01 10:00:00';
        $p->post_date         = '2026-09-01 10:00:00';
        $p->post_date_gmt     = '2026-09-01 10:00:00';
        $p->post_parent       = $parent;
        $p->post_content      = '<!-- wp:paragraph --><p>Intro &amp; more</p><!-- /wp:paragraph -->';
        $p->post_author       = 3;
        $p->post_name         = 'old-title';
        $p->menu_order        = 0;
        $p->comment_status    = 'closed';
        $p->post_password     = '';
        $this->posts[$id]     = $p;
    }

    private static function fixture(string $name): string
    {
        // The bytes as Go wrote them: never trimmed, never re-encoded.
        $text = file_get_contents(__DIR__ . '/fixtures/rest-call/' . $name . '.json');
        self::assertIsString($text);

        return $text;
    }

    private static function entry(string $name): string
    {
        $write = $name === RestCall::NAME_WRITE;

        return (string) json_encode([
            'name'          => $name,
            'source'        => 'wpmgr',
            'class'         => $write ? 'write' : 'read',
            'status'        => 'admitted',
            'enabled'       => true,
            'approval_mode' => $write ? 'per_call' : 'none',
            'snapshot'      => $write ? 'post_fields' : 'none',
        ]);
    }

    /**
     * @param array<string,mixed> $p Decoded p.
     * @return array<string,mixed>
     */
    private function runP(array $p): array
    {
        $text = (string) json_encode($p);

        return (new AbilityRunCommand())->execute(['pd' => hash('sha256', $text)], ['p' => $text]);
    }

    /**
     * @param array<string,mixed> $input  Input.
     * @param string|null         $route  Route row text.
     * @param array<string,mixed> $extra  Extra p fields.
     * @return array<string,mixed>
     */
    private function readCall(array $input, ?string $route = null, array $extra = []): array
    {
        $route = $route ?? self::fixture('read-wp-v2-pages-get');
        $entry = self::entry(RestCall::NAME_READ);

        return $this->runP($extra + [
            'mode'         => 'read',
            'entry'        => $entry,
            'entry_sha256' => hash('sha256', $entry),
            'route'        => $route,
            'route_sha256' => hash('sha256', $route),
            'input'        => (string) json_encode($input),
        ]);
    }

    /**
     * @param string              $mode     Mode.
     * @param array<string,mixed> $input    Input.
     * @param array<string,mixed> $expected Expected digests.
     * @return array<string,mixed>
     */
    private function writeCall(string $mode, array $input, array $expected = []): array
    {
        $route = self::fixture('write-wp-v2-pages-update-fields');
        $entry = self::entry(RestCall::NAME_WRITE);
        $p     = [
            'mode'         => $mode,
            'request_id'   => self::REQ,
            'entry'        => $entry,
            'entry_sha256' => hash('sha256', $entry),
            'route'        => $route,
            'route_sha256' => hash('sha256', $route),
            'input'        => $input === [] ? '{}' : (string) json_encode($input),
        ];
        if ($expected !== []) {
            $p['expected'] = $expected;
        }

        return $this->runP($p);
    }

    /** @return array<string,mixed> */
    private static function retitle(string $title, int $id = 412): array
    {
        return ['route_id' => 'wp-v2-pages-update-fields', 'path' => ['id' => $id], 'query' => new \stdClass(), 'body' => ['title' => $title]];
    }

    /** @return array<string,mixed> Precheck then write. */
    private function precheckAndWrite(string $title): array
    {
        $pre = $this->writeCall('precheck', self::retitle($title));
        $this->assertTrue($pre['ok'], (string) json_encode($pre));

        return $this->writeCall('write', self::retitle($title), [
            'precheck_digest'  => $pre['precheck_digest'],
            'base_fingerprint' => $pre['base_fingerprint'],
        ]);
    }

    /**
     * A filter added the way a plugin adds one.
     *
     * @param string   $tag  Filter.
     * @param callable $cb   Callback.
     * @param int      $prio Priority.
     * @param int      $args Accepted args.
     */
    private function plugin(string $tag, callable $cb, int $prio, int $args = 3): void
    {
        add_filter($tag, $cb, $prio, $args);
    }

    // ------------------------------------------------------------------
    // Fixtures
    // ------------------------------------------------------------------

    public function test_the_fixtures_are_one_read_row_and_one_write_row_of_m161_shape(): void
    {
        $read  = json_decode(self::fixture('read-wp-v2-pages-get'), true);
        $write = json_decode(self::fixture('write-wp-v2-pages-update-fields'), true);
        $cols  = ['route_id', 'method', 'namespace', 'template', 'core_pattern', 'path_params', 'query_keys', 'pinned_query', 'body_keys', 'class', 'output_fields', 'snapshot', 'target', 'arg_render', 'operator_permission', 'effect_copy', 'enabled', 'min_wp_version', 'title', 'description'];
        $this->assertSame($cols, array_keys($read));
        $this->assertSame($cols, array_keys($write));
        $this->assertSame('read', $read['class']);
        $this->assertSame('write', $write['class']);
        $this->assertArrayHasKey('route', RestCall::parseRoute(self::fixture('read-wp-v2-pages-get'), hash('sha256', self::fixture('read-wp-v2-pages-get')), RestCall::NAME_READ, '7.1'));
        $this->assertArrayHasKey('route', RestCall::parseRoute(self::fixture('write-wp-v2-pages-update-fields'), hash('sha256', self::fixture('write-wp-v2-pages-update-fields')), RestCall::NAME_WRITE, '7.1'));
    }

    // ------------------------------------------------------------------
    // Reads
    // ------------------------------------------------------------------

    public function test_a_published_page_is_read_and_projected_onto_the_row_fields(): void
    {
        $this->addPost(412);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame('wp-v2-pages-get', $out['route_id']);
        $this->assertSame(200, $out['status']);
        $o = json_decode((string) json_encode($out['output']), true);
        $this->assertSame(412, $o['id']);
        $this->assertSame(['rendered' => 'Old title'], $o['title']);
        $this->assertArrayNotHasKey('password', $o);
        $this->assertArrayNotHasKey('guid', $o);
        $this->assertSame(['permission', 'get'], $this->calls);
        $this->assertSame(0, $this->currentUser, 'always switched back to user 0');
    }

    public function test_a_single_item_read_of_a_draft_is_refused_not_published(): void
    {
        $this->addPost(412, 'draft');
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_not_published', $out['code'] ?? null);
        $this->assertArrayNotHasKey('output', $out);
    }

    public function test_a_list_holding_any_unpublished_item_is_refused_whole(): void
    {
        $route = $this->listRoute('/wp/v2/pages', ['status' => 'publish', 'context' => 'view']);
        $this->addPost(1);
        $this->addPost(2, 'private');
        $out = $this->readCall(['route_id' => 'test-pages-list'], $route);
        $this->assertSame('rest_not_published', $out['code'] ?? null);
    }

    public function test_a_media_list_drops_files_attached_to_unpublished_parents(): void
    {
        $route = $this->listRoute('/wp/v2/media', ['status' => 'inherit', 'context' => 'view']);
        $this->addPost(10, 'publish', 'page');
        $this->addPost(11, 'draft', 'page');
        $this->posts = [];
        $this->addPost(21, 'inherit', 'attachment', 'on published', 10);
        $this->addPost(22, 'inherit', 'attachment', 'on draft', 11);
        $this->addPost(23, 'inherit', 'attachment', 'unattached', null);
        // Parents are looked up separately from the list.
        Functions\when('get_post_status')->alias(fn ($id) => [10 => 'publish', 11 => 'draft'][(int) $id] ?? false);
        $out = $this->readCall(['route_id' => 'test-pages-list'], $route);
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $ids = array_map(fn ($i) => $i->id, $out['output']);
        $this->assertSame([21, 23], $ids);
    }

    public function test_route_bytes_that_do_not_match_their_hash_are_refused(): void
    {
        $route = self::fixture('read-wp-v2-pages-get');
        $entry = self::entry(RestCall::NAME_READ);
        $out   = $this->runP([
            'mode' => 'read', 'entry' => $entry, 'entry_sha256' => hash('sha256', $entry),
            'route' => $route, 'route_sha256' => hash('sha256', $route . ' '),
            'input' => '{"route_id":"wp-v2-pages-get","path":{"id":412}}',
        ]);
        $this->assertSame('route_entry_changed', $out['code']);
        $this->assertSame([], $this->calls);
    }

    public function test_input_naming_another_route_is_refused(): void
    {
        $out = $this->readCall(['route_id' => 'wp-v2-posts-get', 'path' => ['id' => 412]]);
        $this->assertSame('route_entry_changed', $out['code']);
    }

    public function test_a_raw_route_string_in_input_is_refused(): void
    {
        $this->addPost(412);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412], 'route' => '/wp/v2/settings']);
        $this->assertSame('route_key_not_allowed', $out['code']);
        $this->assertSame([], $this->calls);
    }

    public function test_path_values_are_typed_ints_only(): void
    {
        foreach ([['id' => '412'], ['id' => '412/../../settings'], ['id' => 0], ['id' => -1], ['id' => 1.5], []] as $path) {
            $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => $path]);
            $this->assertSame('route_param_invalid', $out['code'], (string) json_encode($path));
        }
        $this->assertSame([], $this->calls);
    }

    public function test_a_row_for_a_refused_namespace_is_refused_in_code(): void
    {
        $row = json_decode(self::fixture('read-wp-v2-pages-get'), true);
        $row['route_id']     = 'bad-abilities';
        $row['namespace']    = 'wp-abilities/v1';
        $row['template']     = '/wp-abilities/v1/abilities';
        $row['core_pattern'] = '/wp-abilities/v1/abilities';
        $row['path_params']  = new \stdClass();
        $out = $this->readCall(['route_id' => 'bad-abilities'], (string) json_encode($row));
        $this->assertSame('route_namespace_refused', $out['code']);
        foreach (['/batch/v1', '/wp/v2/settings', '/wp/v2/menu-items', '/wp/v2/menus/3', '/wp/v2/plugins', '/wp/v2/templates/x', '/wp/v2/global-styles/1', '/oembed/1.0/proxy', '/wp-block-editor/v1/url-details', '/wp-site-health/v1/tests', '/mcp/mcp-adapter-default-server', '/wpmgr/v1/command', '/WP/V2/Settings'] as $p) {
            $this->assertTrue(RestCall::denied($p, 'GET'), $p);
        }
        $this->assertTrue(RestCall::denied('/wp/v2/users/1', 'POST'));
        $this->assertFalse(RestCall::denied('/wp/v2/users/1', 'GET'));
        foreach (['/wp/v2/pages/1', '/wp/v2/posts', '/wp/v2/media', '/wp/v2/types', '/wp/v2/taxonomies', '/wp/v2/categories', '/wp/v2/tags'] as $p) {
            $this->assertFalse(RestCall::denied($p, 'GET'), $p);
        }
    }

    public function test_status_and_method_override_keys_are_refused_from_input_and_rows(): void
    {
        $route = $this->listRoute('/wp/v2/pages', ['status' => 'publish', 'context' => 'view']);
        foreach (['status' => 'draft', '_method' => 'POST', '_embed' => '1', '_fields' => 'id', 'context' => 'edit', 'author' => '1', 'password' => 'x'] as $k => $v) {
            $out = $this->readCall(['route_id' => 'test-pages-list', 'query' => [$k => $v]], $route);
            $this->assertSame('route_key_forbidden', $out['code'], $k);
        }
        // A row that lists status as a free key is refused too.
        $row = json_decode($route, true);
        $row['query_keys']['status'] = ['type' => 'enum', 'values' => ['draft']];
        unset($row['pinned_query']['status']);
        $out = $this->readCall(['route_id' => 'test-pages-list', 'query' => ['status' => 'draft']], (string) json_encode($row));
        $this->assertSame('route_key_forbidden', $out['code']);
        $this->assertSame([], $this->calls);
    }

    public function test_a_query_key_the_row_does_not_list_is_refused(): void
    {
        $route = $this->listRoute('/wp/v2/pages', ['status' => 'publish', 'context' => 'view']);
        $out   = $this->readCall(['route_id' => 'test-pages-list', 'query' => ['parent' => 3]], $route);
        $this->assertSame('route_key_not_allowed', $out['code']);
    }

    public function test_a_handler_that_is_not_core_is_refused_before_its_permission_or_callback_run(): void
    {
        $this->addPost(412);
        $ran = [];
        // A plugin registers its own GET handler for the core pages route, ahead of core's.
        $this->server->routes['/wp/v2/pages/(?P<id>[\d]+)'] = array_merge([[
            'methods'             => ['GET' => true],
            'callback'            => function () use (&$ran) {
                $ran[] = 'callback';
                return ['id' => 412, 'status' => 'publish'];
            },
            'permission_callback' => function () use (&$ran) {
                $ran[] = 'permission';
                return true;
            },
            'args'                => [],
        ]], $this->server->routes['/wp/v2/pages/(?P<id>[\d]+)']);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_handler_not_core', $out['code']);
        $this->assertSame([], $ran, 'neither the permission callback nor the callback ran');
    }

    public function test_a_pre_dispatch_short_circuit_added_after_ours_is_refused(): void
    {
        $this->addPost(412, 'draft');
        $forged = function ($result, $server, $request) {
            $r = new \WP_REST_Response(['id' => 412, 'status' => 'publish', 'title' => ['rendered' => 'forged']]);
            $r->set_matched_route('/wp/v2/pages/(?P<id>[\d]+)');
            return $r;
        };
        // Added once our guard is armed, at the same latest priority, so it runs after it.
        Functions\when('rest_do_request')->alias(function ($r) use ($forged) {
            add_filter('rest_pre_dispatch', $forged, PHP_INT_MAX, 3);
            return $this->server->dispatch($r);
        });
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_intercepted', $out['code'], (string) json_encode($out));
        $this->assertContains('pre_dispatch', $out['violations']);
    }

    public function test_a_pre_dispatch_short_circuit_that_unhooks_our_guard_is_refused_after_the_fact(): void
    {
        $this->addPost(412, 'draft');
        // Runs just before our latest-priority guard, removes it, and answers
        // with a forged published page. Our guard never runs, so only the
        // check made after the dispatch returns can see it.
        $forger = function ($result, $server, $request) {
            foreach ($GLOBALS['wp_filter']['rest_pre_dispatch']->callbacks[PHP_INT_MAX] ?? [] as $e) {
                remove_filter('rest_pre_dispatch', $e['function'], PHP_INT_MAX);
            }
            $r = new \WP_REST_Response(['id' => 412, 'status' => 'publish', 'title' => ['rendered' => 'forged']]);
            $r->set_matched_route('/wp/v2/pages/(?P<id>[\d]+)');
            return $r;
        };
        $this->plugin('rest_pre_dispatch', $forger, PHP_INT_MAX - 1);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_intercepted', $out['code'], (string) json_encode($out));
        $this->assertContains('pre_dispatch', $out['violations']);
        $this->assertSame([], $this->calls);
    }

    public function test_another_route_pattern_answering_the_path_is_refused_even_with_core_handlers(): void
    {
        $this->addPost(412);
        // Registered ahead of the reviewed pattern, with core's own handlers.
        $this->server->routes = ['/wp/v2/pages/(?P<id>\d+)' => [[
            'methods' => ['GET' => true], 'callback' => 'wpmgr_test_core_rest_get',
            'permission_callback' => 'wpmgr_test_core_rest_permission', 'args' => [],
        ]]] + $this->server->routes;
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_intercepted', $out['code'], (string) json_encode($out));
    }

    public function test_a_before_callbacks_filter_that_drops_a_validation_error_is_refused(): void
    {
        $this->addPost(412);
        $this->server->routes['/wp/v2/pages/(?P<id>[\d]+)'][0]['validate'] = fn () => new \WP_Error('rest_invalid_param', 'bad', ['status' => 400]);
        $this->plugin('rest_request_before_callbacks', fn ($response) => null, 10);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_intercepted', $out['code']);
        $this->assertSame(['before_callbacks'], $out['violations']);
        $this->assertSame([], $this->calls, 'the callback never ran');
    }

    public function test_a_dispatch_filter_that_answers_instead_of_the_callback_is_refused(): void
    {
        $this->addPost(412);
        $this->plugin('rest_dispatch_request', fn ($r) => ['id' => 412, 'status' => 'publish'], 10, 4);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_intercepted', $out['code']);
        $this->assertContains('dispatch', $out['violations']);
    }

    public function test_an_after_callbacks_rewrite_on_a_read_is_refused(): void
    {
        $this->addPost(412);
        $this->plugin('rest_request_after_callbacks', function ($response) {
            $response['title']['rendered'] = 'injected';
            return $response;
        }, 10);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('rest_intercepted', $out['code']);
        $this->assertSame(['after_callbacks'], $out['violations']);
    }

    public function test_an_unrelated_rest_request_passes_through_the_guards_untouched(): void
    {
        $this->addPost(412);
        $seen = null;
        // A nested request a plugin makes during ours is not ours to judge.
        $this->plugin('rest_request_after_callbacks', function ($response, $handler, $request) use (&$seen) {
            return $response;
        }, 10);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertTrue($out['ok'], 'a pass-through filter is not a violation');
    }

    public function test_a_read_that_writes_an_option_is_refused(): void
    {
        $this->addPost(412);
        $this->plugin('rest_request_after_callbacks', function ($response) {
            \WP_REST_Server::applyHooks('updated_option', 'some_plugin_cache');
            return $response;
        }, 10);
        $out = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]]);
        $this->assertSame('read_side_effect_detected', $out['code']);
    }

    public function test_a_write_row_sent_to_rest_read_is_a_class_mismatch(): void
    {
        $out = $this->readCall(['route_id' => 'wp-v2-pages-update-fields', 'path' => ['id' => 412], 'body' => ['title' => 'x']], self::fixture('write-wp-v2-pages-update-fields'));
        $this->assertSame('mode_class_mismatch', $out['code']);
    }

    public function test_a_disabled_route_is_refused(): void
    {
        $row            = json_decode(self::fixture('read-wp-v2-pages-get'), true);
        $row['enabled'] = false;
        $out            = $this->readCall(['route_id' => 'wp-v2-pages-get', 'path' => ['id' => 412]], (string) json_encode($row));
        $this->assertSame('route_disabled', $out['code']);
    }

    // ------------------------------------------------------------------
    // Writes
    // ------------------------------------------------------------------

    public function test_precheck_reports_the_live_status_and_binds_the_digest(): void
    {
        $this->addPost(412);
        $out = $this->writeCall('precheck', self::retitle('Spring sale 2026'));
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame(['id' => 412, 'post_type' => 'page', 'status' => 'publish', 'live' => true, 'title_before' => 'Old title', 'excerpt_before' => 'Old excerpt'], $out['target_facts']);
        $this->assertSame([['key' => 'title', 'before' => 'Old title', 'after' => 'Spring sale 2026', 'stored' => 'Spring sale 2026']], $out['changes']);
        $route = self::fixture('write-wp-v2-pages-update-fields');
        $this->assertSame(
            hash('sha256', (string) json_encode([hash('sha256', self::entry(RestCall::NAME_WRITE)), hash('sha256', $route), hash('sha256', (string) json_encode(self::retitle('Spring sale 2026'))), $out['base_fingerprint']])),
            $out['precheck_digest']
        );
        $this->assertSame([], $this->calls, 'a precheck dispatches nothing');
    }

    public function test_an_ampersand_title_is_not_a_sanitiser_change(): void
    {
        $this->addPost(412);
        // Sent raw, kses would turn "&" into "&amp;": that would be a change.
        $this->assertNotSame('Tom & Jerry', self::ksesEntities('Tom & Jerry'));
        $out = $this->writeCall('precheck', self::retitle('Tom & Jerry'));
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame('Tom &amp; Jerry', $out['changes'][0]['stored']);
        $w = $this->precheckAndWrite('Tom & Jerry');
        $this->assertTrue($w['ok'], (string) json_encode($w));
        $this->assertSame('Tom &amp; Jerry', $this->posts[412]->post_title);
    }

    public function test_a_sanitiser_that_changes_the_value_is_refused(): void
    {
        $this->addPost(412);
        Functions\when('sanitize_post_field')->alias(fn ($f, $v) => strtoupper((string) $v));
        $out = $this->writeCall('precheck', self::retitle('Spring sale'));
        $this->assertSame('sanitiser_changed_value', $out['code']);
    }

    public function test_a_post_of_another_type_or_not_editable_is_refused(): void
    {
        $this->addPost(412, 'publish', 'post');
        $this->assertSame('post_not_editable', $this->writeCall('precheck', self::retitle('x'))['code']);
        $this->addPost(412);
        $this->canEdit = false;
        $this->assertSame('post_not_editable', $this->writeCall('precheck', self::retitle('x'))['code']);
    }

    public function test_the_write_snapshots_the_prior_bytes_before_the_effect_and_verifies(): void
    {
        $this->addPost(412);
        $out = $this->precheckAndWrite('Spring sale 2026');
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame('updated', $out['outcome']);
        $this->assertTrue($out['live']);
        $this->assertNotNull($this->ledgerAtEffect, 'the ledger row existed when the route ran');
        $this->assertSame(['post_title' => 'Old title', 'post_excerpt' => 'Old excerpt'], $this->ledgerAtEffect['prior']);
        $row = AbilityLedger::get(self::REQ);
        $this->assertSame('completed', $row['phase']);
        $this->assertSame('available', $row['undo_state']);
        $this->assertSame(412, $row['target_post_id']);
        $this->assertSame('Spring sale 2026', $this->posts[412]->post_title);
        // Idempotent replay.
        $again = $this->writeCall('write', self::retitle('Spring sale 2026'), ['precheck_digest' => str_repeat('a', 64), 'base_fingerprint' => str_repeat('b', 64)]);
        $this->assertSame('already_applied', $again['outcome']);
        $this->assertSame(412, $again['post_id']);
    }

    public function test_a_post_changed_after_precheck_is_a_conflict(): void
    {
        $this->addPost(412);
        $pre = $this->writeCall('precheck', self::retitle('New'));
        $this->posts[412]->post_title = 'Edited by a person';
        $out = $this->writeCall('write', self::retitle('New'), ['precheck_digest' => $pre['precheck_digest'], 'base_fingerprint' => $pre['base_fingerprint']]);
        $this->assertSame('conflict', $out['code']);
        $this->assertSame([], $this->calls);
    }

    public function test_a_stored_value_that_differs_is_undone(): void
    {
        $this->addPost(412);
        $this->mangle = fn ($t) => $t . ' (edited by a filter)';
        $out = $this->precheckAndWrite('New');
        $this->assertSame('verify_mismatch', $out['code']);
        $this->assertTrue($out['restored']);
        $this->assertSame('Old title', $this->posts[412]->post_title);
    }

    public function test_a_tripwire_hit_is_undone_and_reported(): void
    {
        $this->addPost(412);
        $this->alsoDo = function () {
            $this->rawOptions['admin_email'] = 'attacker@example.test';
        };
        $out = $this->precheckAndWrite('New');
        $this->assertSame('side_effect_detected', $out['code']);
        $this->assertTrue($out['restored']);
        $this->assertSame('Old title', $this->posts[412]->post_title);
    }

    public function test_a_handler_that_is_not_core_never_writes(): void
    {
        $this->addPost(412);
        $wrote = false;
        $this->server->routes['/wp/v2/pages/(?P<id>[\d]+)'] = array_merge([[
            'methods'             => ['POST' => true],
            'callback'            => function () use (&$wrote) {
                $wrote = true;
                return [];
            },
            'permission_callback' => '__return_true',
            'args'                => [],
        ]], $this->server->routes['/wp/v2/pages/(?P<id>[\d]+)']);
        $out = $this->precheckAndWrite('New');
        $this->assertSame('rest_handler_not_core', $out['code']);
        $this->assertFalse($wrote);
        $this->assertSame('Old title', $this->posts[412]->post_title);
    }

    public function test_an_after_callbacks_rewrite_on_a_write_is_verified_from_the_stored_post(): void
    {
        $this->addPost(412);
        $this->plugin('rest_request_after_callbacks', function ($response) {
            if (is_array($response)) {
                $response['title']['rendered'] = 'lie';
            }
            return $response;
        }, 10);
        $out = $this->precheckAndWrite('New');
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame(['id' => 412, 'modified_gmt' => $this->posts[412]->post_modified_gmt, 'status' => 'publish'], $out['output']);
    }

    public function test_revert_restores_the_prior_bytes_from_the_ledger(): void
    {
        $this->addPost(412);
        $this->assertTrue($this->precheckAndWrite('New')['ok']);
        $out = $this->writeCall('revert', []);
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame('reverted', $out['outcome']);
        $this->assertTrue($out['exact']);
        $this->assertSame('Old title', $this->posts[412]->post_title);
        $this->assertSame('already_reverted', $this->writeCall('revert', [])['outcome']);
    }

    public function test_revert_takes_no_input_and_never_an_id_from_it(): void
    {
        $this->addPost(412);
        $this->addPost(999, 'publish', 'page', 'Someone else');
        $this->assertTrue($this->precheckAndWrite('New')['ok']);
        $out = $this->writeCall('revert', ['route_id' => 'wp-v2-pages-update-fields', 'path' => ['id' => 999]]);
        $this->assertSame('bad_input', $out['code']);
        $this->assertSame('Someone else', $this->posts[999]->post_title);
        $this->assertSame('New', $this->posts[412]->post_title);
    }

    public function test_revert_refuses_a_post_changed_since_our_write(): void
    {
        $this->addPost(412);
        $this->assertTrue($this->precheckAndWrite('New')['ok']);
        $this->posts[412]->post_title        = 'Later edit';
        $this->posts[412]->post_modified_gmt = '2026-10-03 09:00:00';
        $this->assertSame('conflict', $this->writeCall('revert', [])['code']);
        $this->assertSame('Later edit', $this->posts[412]->post_title);
    }

    public function test_revert_refuses_a_post_someone_touched(): void
    {
        $this->addPost(412);
        $this->assertTrue($this->precheckAndWrite('New')['ok']);
        $this->revisions[412] = 5000;
        $this->assertSame('post_touched', $this->writeCall('revert', [])['code']);
        unset($this->revisions[412]);
        $a                    = new \stdClass();
        $a->ID                = 6000;
        $a->post_modified_gmt = '2026-10-03 09:00:00';
        $this->autosaves[412] = $a;
        $this->assertSame('post_touched', $this->writeCall('revert', [])['code']);
        $this->autosaves = [];
        $this->locks[412] = true;
        $this->assertSame('post_touched', $this->writeCall('revert', [])['code']);
        $this->locks = [];
        $this->assertTrue($this->writeCall('revert', [])['ok']);
    }

    // ------------------------------------------------------------------
    // Review: whole-row safety, approved bytes, fail-closed reads
    // ------------------------------------------------------------------

    public function test_a_page_with_content_the_principal_may_not_save_is_refused_at_precheck_and_write(): void
    {
        $this->addPost(412);
        $this->posts[412]->post_content = '<!-- wp:paragraph --><p>Intro</p><!-- /wp:paragraph --><!-- wp:html --><iframe src="https://video.example.test/x"></iframe><script>window.dataLayer=[]</script><form action="/subscribe"><input name="email"></form><!-- /wp:html -->';
        $before = $this->posts[412]->post_content;
        $pre    = $this->writeCall('precheck', self::retitle('New'));
        $this->assertSame('post_content_would_change', $pre['code']);
        $this->assertSame('this page has content the WPMgr user may not save; edit the title in WordPress', $pre['detail']);
        // A write sent anyway (a precheck from before the content changed) re-checks.
        $out = $this->writeCall('write', self::retitle('New'), ['precheck_digest' => str_repeat('a', 64), 'base_fingerprint' => \WPMgr\Agent\Abilities\RestCall::postFingerprint($this->posts[412])]);
        $this->assertSame('post_content_would_change', $out['code']);
        $this->assertSame($before, $this->posts[412]->post_content);
        $this->assertSame([], $this->calls);
    }

    public function test_a_plugin_changing_other_columns_is_reported_honestly_and_stays_undoable(): void
    {
        $this->addPost(412);
        $this->plugin('wp_insert_post_data', function ($data) {
            $data['post_content'] = 'replaced';
            $data['post_author']  = 1;
            $data['post_name']    = 'hijacked';
            return $data;
        }, 10, 2);
        $out = $this->precheckAndWrite('New');
        $this->assertSame('side_effect_detected', $out['code'], (string) json_encode($out));
        $this->assertSame(['post_author', 'post_content', 'post_name'], $out['columns']);
        // Title and excerpt are back, but the row is not: never claimed restored.
        $this->assertSame('Old title', $this->posts[412]->post_title);
        $this->assertSame('replaced', $this->posts[412]->post_content);
        $this->assertFalse($out['restored']);
        $this->assertFalse($out['exact']);
        $this->assertSame(['post_author', 'post_content', 'post_name'], $out['columns_still_changed']);
        $row = AbilityLedger::get(self::REQ);
        $this->assertSame('failed_needs_attention', $row['phase']);
        $this->assertSame('available', $row['undo_state']);

        // A person's undo is not blocked, and is just as honest.
        $r = $this->writeCall('revert', []);
        $this->assertTrue($r['ok'], (string) json_encode($r));
        $this->assertSame('reverted', $r['outcome']);
        $this->assertFalse($r['restored']);
        $this->assertSame(['post_author', 'post_content', 'post_name'], $r['columns_still_changed']);
        $this->assertSame('Old title', $this->posts[412]->post_title);
        $again = $this->writeCall('revert', []);
        $this->assertSame('already_reverted', $again['outcome']);
        $this->assertFalse($again['restored']);
    }

    public function test_a_clean_failed_write_is_restored_and_says_so(): void
    {
        $this->addPost(412);
        $this->mangle = fn ($t) => $t . ' (edited by a filter)';
        $out = $this->precheckAndWrite('New');
        $this->assertSame('verify_mismatch', $out['code']);
        $this->assertTrue($out['restored']);
        $this->assertArrayNotHasKey('columns_still_changed', $out);
        $this->assertSame('restored', AbilityLedger::get(self::REQ)['undo_state']);
    }

    public function test_a_never_dated_draft_can_be_retitled_and_undone(): void
    {
        $this->addPost(412, 'draft');
        $this->posts[412]->post_date_gmt = '0000-00-00 00:00:00';
        $out = $this->precheckAndWrite('New');
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertNotSame('2026-09-01 10:00:00', $this->posts[412]->post_date, 'core re-dated the draft');
        $r = $this->writeCall('revert', []);
        $this->assertTrue($r['ok'], (string) json_encode($r));
        $this->assertTrue($r['restored']);
        $this->assertSame('Old title', $this->posts[412]->post_title);
    }

    public function test_a_failed_write_to_a_never_dated_draft_is_restored_without_a_date_side_effect(): void
    {
        $this->addPost(412, 'draft');
        $this->posts[412]->post_date_gmt = '0000-00-00 00:00:00';
        $this->mangle = fn ($t) => $t . ' (edited by a filter)';
        $out = $this->precheckAndWrite('New');
        $this->assertSame('verify_mismatch', $out['code'], (string) json_encode($out));
        $this->assertTrue($out['restored']);
        $this->assertArrayNotHasKey('columns_still_changed', $out);
    }

    public function test_a_dated_draft_whose_date_moves_is_still_a_side_effect(): void
    {
        $this->addPost(412, 'draft');
        $this->plugin('wp_insert_post_data', function ($data) {
            $data['post_date'] = '2030-01-01 00:00:00';
            return $data;
        }, 10, 2);
        $out = $this->precheckAndWrite('New');
        $this->assertSame('side_effect_detected', $out['code']);
        $this->assertSame(['post_date'], $out['columns']);
    }

    public function test_a_scheduled_post_is_refused(): void
    {
        $this->addPost(412, 'future');
        $out = $this->writeCall('precheck', self::retitle('New'));
        $this->assertSame('post_scheduled', $out['code']);
        $this->assertSame([], $this->calls);
    }

    public function test_the_bytes_sent_are_the_approved_stored_bytes(): void
    {
        $this->addPost(412);
        $plain = 'Top [10] tips <b>now</b> & more';
        $pre   = $this->writeCall('precheck', self::retitle($plain));
        $this->assertTrue($pre['ok'], (string) json_encode($pre));
        $stored = 'Top &#091;10&#093; tips &lt;b&gt;now&lt;/b&gt; &amp; more';
        $this->assertSame($stored, $pre['changes'][0]['stored']);
        $sent = null;
        $this->plugin('rest_request_before_callbacks', function ($response, $handler, $request) use (&$sent) {
            $sent = $request->get_body_params();
            return $response;
        }, 10);
        $out = $this->writeCall('write', self::retitle($plain), ['precheck_digest' => $pre['precheck_digest'], 'base_fingerprint' => $pre['base_fingerprint']]);
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertSame(['title' => $stored], $sent);
        $this->assertSame($stored, $this->posts[412]->post_title);
    }

    public function test_the_fingerprint_covers_content_and_author(): void
    {
        $this->addPost(412);
        $pre = $this->writeCall('precheck', self::retitle('New'));
        $this->posts[412]->post_content = 'edited by a person';
        $this->assertSame('conflict', $this->writeCall('write', self::retitle('New'), ['precheck_digest' => $pre['precheck_digest'], 'base_fingerprint' => $pre['base_fingerprint']])['code']);
        $this->posts[412]->post_content = '<!-- wp:paragraph --><p>Intro &amp; more</p><!-- /wp:paragraph -->';
        $this->posts[412]->post_author  = 9;
        $this->assertSame('conflict', $this->writeCall('write', self::retitle('New'), ['precheck_digest' => $pre['precheck_digest'], 'base_fingerprint' => $pre['base_fingerprint']])['code']);
    }

    public function test_target_meta_changes_are_recorded(): void
    {
        $this->addPost(412);
        $this->alsoDo = function () {
            $this->meta[412]['_some_seo_title'] = ['x'];
        };
        $out = $this->precheckAndWrite('New');
        $this->assertTrue($out['ok'], (string) json_encode($out));
        $this->assertTrue($out['verify']['target_meta_changed']);
        $this->assertFalse($out['verify']['target_terms_changed']);
    }

    public function test_a_post_like_read_without_a_pinned_status_fails_closed(): void
    {
        $this->addPost(1);
        $route = $this->listRoute('/wp/v2/pages', ['context' => 'view']);
        $this->assertSame('rest_not_published', $this->readCall(['route_id' => 'test-pages-list'], $route)['code']);
        $this->assertFalse(RestCall::publishedOnly([['status' => 'publish']], ['template' => '/wp/v2/media', 'pinned_query' => ['status' => 'publish']]));
        $this->assertTrue(RestCall::publishedOnly(['post' => []], ['template' => '/wp/v2/types', 'pinned_query' => []]));
    }

    public function test_a_single_media_item_on_a_draft_parent_is_refused(): void
    {
        $this->addPost(22, 'inherit', 'attachment', 'on draft', 11);
        Functions\when('get_post_status')->alias(fn ($id) => [11 => 'draft', 10 => 'publish'][(int) $id] ?? false);
        $this->server->register_route('/wp/v2/media/(?P<id>[\d]+)', ['methods' => ['GET' => true], 'callback' => 'wpmgr_test_core_rest_get', 'permission_callback' => 'wpmgr_test_core_rest_permission']);
        $row = json_decode(self::fixture('read-wp-v2-pages-get'), true);
        $row['route_id']     = 'test-media-get';
        $row['template']     = '/wp/v2/media/{id}';
        $row['core_pattern'] = '/wp/v2/media/(?P<id>[\d]+)';
        $row['pinned_query'] = ['status' => 'inherit', 'context' => 'view'];
        $text = (string) json_encode($row);
        $out  = $this->readCall(['route_id' => 'test-media-get', 'path' => ['id' => 22]], $text);
        $this->assertSame('rest_not_published', $out['code'], (string) json_encode($out));
        $this->posts[22]->post_parent = 10;
        $this->assertTrue($this->readCall(['route_id' => 'test-media-get', 'path' => ['id' => 22]], $text)['ok']);
    }

    public function test_a_capability_granted_during_precheck_refuses_it(): void
    {
        $this->addPost(412);
        $this->duringSanitise = function (): void {
            $this->grantAll = true;
        };
        $this->assertSame('principal_capabilities_drifted', $this->writeCall('precheck', self::retitle('New'))['code']);
    }

    public function test_a_capability_granted_during_revert_is_reported(): void
    {
        $this->addPost(412);
        $this->assertTrue($this->precheckAndWrite('New')['ok']);
        $this->duringSanitise = function (): void {
            $this->grantAll = true;
        };
        $out = $this->writeCall('revert', []);
        $this->assertSame('principal_capabilities_drifted', $out['code']);
    }

    // ------------------------------------------------------------------
    // Cross-language contract
    // ------------------------------------------------------------------

    public function test_go_rest_write_fixture_replays_precheck_write_and_revert_byte_for_byte(): void
    {
        $raw = file_get_contents(__DIR__ . '/fixtures/ability-run/rest-write.json');
        $this->assertIsString($raw);
        $f = json_decode($raw, true);
        $this->assertIsArray($f);

        // The real signed path: an Ed25519 command token carrying pd, then
        // Router::authorizeCommand() and Router::handleCommand().
        $keyFile = sys_get_temp_dir() . '/wpmgr-agent-a3-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $keyFile);
        }
        Functions\when('is_user_logged_in')->justReturn(false);
        Functions\when('register_rest_route')->justReturn(true);
        $pair   = sodium_crypto_sign_keypair();
        $secret = sodium_crypto_sign_secretkey($pair);
        $keys   = new Keystore();
        $keys->storeControlPlanePublicKey(sodium_crypto_sign_publickey($pair));
        $siteId = (string) $f['site_id'];
        $this->options[Settings::OPTION_SITE_ID] = $siteId;
        $this->resetShieldStash();
        $router = new Router(new Connector($keys, new Settings()), [new AbilityRunCommand()]);
        $call   = function (string $p) use ($router, $secret, $siteId): array {
            $b64    = static fn (string $d): string => rtrim(strtr(base64_encode($d), '+/', '-_'), '=');
            $claims = ['aud' => $siteId, 'cmd' => 'ability_run', 'jti' => bin2hex(random_bytes(8)), 'exp' => time() + 30, 'pd' => hash('sha256', $p)];
            $seg    = [$b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT'])), $b64((string) json_encode($claims))];
            $seg[]  = $b64(sodium_crypto_sign_detached(implode('.', $seg), $secret));
            $req    = new \WP_REST_Request('POST', '/wpmgr/v1/command/ability_run');
            $req->set_url_params(['command' => 'ability_run']);
            $req->set_header('Content-Type', 'application/json');
            $req->set_header('Accept', 'application/json');
            $req->set_header('Authorization', 'Bearer ' . implode('.', $seg));
            $req->set_body((string) json_encode(['p' => $p]));
            $this->assertTrue($router->authorizeCommand($req, 'ability_run'), 'the signed request was not authorized');
            $res = $router->handleCommand($req);
            $this->assertInstanceOf(\WP_REST_Response::class, $res);
            $this->assertIsArray($res->data);

            return $res->data;
        };

        $this->addPost(412);
        $prior = ['Old title', 'Old excerpt'];

        // The fixture's own hashes hold over Go's exact bytes.
        $this->assertSame(hash('sha256', (string) $f['entry']), $f['entry_sha256']);
        $this->assertSame(hash('sha256', (string) $f['route']), $f['route_sha256']);
        $this->assertStringContainsString('"route_sha256":"' . $f['route_sha256'] . '"', (string) $f['precheck']['p']);

        // Precheck: Go's exact p.
        $pre = $call((string) $f['precheck']['p']);
        $this->assertTrue($pre['ok'] ?? false, (string) json_encode($pre));
        $this->assertSame('prechecked', $pre['outcome']);
        $input   = json_decode((string) $f['input'], true);
        $title   = RestCall::storedValue((string) $input['body']['title']);
        $excerpt = RestCall::storedValue((string) $input['body']['excerpt']);
        $this->assertSame('Café &amp; croissants 🥐', $title);
        $this->assertSame(['key' => 'title', 'before' => 'Old title', 'after' => $input['body']['title'], 'stored' => $title], $pre['changes'][0]);

        // The stand-ins are refused and nothing changes.
        $standIn = (string) $f['write']['p'];
        $stale   = $call($standIn);
        $this->assertFalse($stale['ok'] ?? true);
        $this->assertSame('conflict', $stale['code'] ?? null, (string) json_encode($stale));
        $this->assertSame($prior, [$this->posts[412]->post_title, $this->posts[412]->post_excerpt]);

        // Write: the real digests substituted as plain text, Go's encoding kept.
        $oldPre  = (string) $f['write']['expected']['precheck_digest'];
        $oldBase = (string) $f['write']['expected']['base_fingerprint'];
        $this->assertSame(1, substr_count($standIn, $oldPre));
        $this->assertSame(1, substr_count($standIn, $oldBase));
        $writeP = str_replace([$oldPre, $oldBase], [(string) $pre['precheck_digest'], (string) $pre['base_fingerprint']], $standIn);
        $w      = $call($writeP);
        $this->assertTrue($w['ok'] ?? false, (string) json_encode($w));
        $this->assertSame('updated', $w['outcome']);
        $this->assertSame($title, $this->posts[412]->post_title, 'stored == the approved escaped bytes');
        $this->assertSame($excerpt, $this->posts[412]->post_excerpt);

        // Revert: Go's exact p; the post and prior bytes come from the ledger.
        $r = $call((string) $f['revert']['p']);
        $this->assertTrue($r['ok'] ?? false, (string) json_encode($r));
        $this->assertSame('reverted', $r['outcome']);
        $this->assertSame($prior, [$this->posts[412]->post_title, $this->posts[412]->post_excerpt]);
        $this->resetShieldStash();
    }

    public function test_go_closed_post_column_list_equals_the_agent_list(): void
    {
        $go = file_get_contents(dirname(__DIR__, 2) . '/api/internal/agentcmd/ability_run_vendor.go');
        $this->assertIsString($go);
        $start = strpos($go, 'var postColumnLabels');
        $this->assertNotFalse($start, 'Go postColumnLabels not found');
        $body = substr($go, $start, (int) strpos($go, 'return m', $start) - $start);
        preg_match_all('/"([A-Za-z_]+)"/', (string) substr($body, (int) strpos($body, '[]string{')), $m);
        $this->assertNotSame([], $m[1]);
        $this->assertSame(RestCall::POST_COLUMNS, $m[1]);
    }

    private function resetShieldStash(): void
    {
        $prop = new \ReflectionProperty(AuthHeaderShield::class, 'stashedBearer');
        $prop->setValue(null, null);
    }

    /**
     * A list route row for tests.
     *
     * @param string               $template Template.
     * @param array<string,string> $pinned   Pinned query.
     * @return string
     */
    private function listRoute(string $template, array $pinned): string
    {
        return (string) json_encode([
            'route_id'      => 'test-pages-list',
            'method'        => 'GET',
            'namespace'     => 'wp/v2',
            'template'      => $template,
            'core_pattern'  => $template,
            'path_params'   => new \stdClass(),
            'query_keys'    => ['per_page' => ['type' => 'int', 'min' => 1, 'max' => 50], 'search' => ['type' => 'string', 'max_len' => 64]],
            'pinned_query'  => $pinned,
            'body_keys'     => new \stdClass(),
            'class'         => 'read',
            'output_fields' => ['items' => ['fields' => ['id' => 'int', 'status' => 'string', 'title' => ['fields' => ['rendered' => 'string']]]]],
            'snapshot'      => 'none',
            'target'        => null,
            'enabled'       => true,
        ]);
    }
}
