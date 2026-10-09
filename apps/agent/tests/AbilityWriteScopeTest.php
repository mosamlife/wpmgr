<?php
/**
 * AbilityWriteScope: what a builder save may write while the scope is armed.
 *
 * Every hook the scope and its inner recorder install is captured through
 * add_filter() and fired here by hand, with the arguments core passes, so
 * each rule is driven through the callback core would call.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\AbilitySideEffects;
use WPMgr\Agent\Abilities\AbilityWriteScope;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\AbilityWriteScope
 */
final class AbilityWriteScopeTest extends TestCase
{
    private const TARGET = 41;

    private const OTHER = 77;

    /** The two option patterns Elementor's save is known to write, bound to the target. */
    private const PATTERNS = [
        '_transient__elementor_editor_unsaved_{target}',
        '_transient_timeout__elementor_editor_unsaved_{target}',
    ];

    /** Hooks every arming installs: the scope's own and the inner recorder's. */
    private const OWN_TAGS = [
        'wp_insert_post',
        'delete_post',
        'wp_trash_post',
        'added_post_meta',
        'updated_post_meta',
        'deleted_post_meta',
        'set_object_terms',
        'deleted_term_relationships',
        'map_meta_cap',
        'user_has_cap',
    ];

    /** @var list<array{0:string,1:callable,2:int,3:int}> Captured add_filter calls: tag, callback, priority, accepted args. */
    private array $hooks = [];

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var array<string,mixed> */
    private array $siteOptions = [];

    /** @var mixed */
    private $savedWpdb;

    private bool $hadWpdb = false;

    /** @var mixed */
    private $savedStack;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->hooks       = [];
        $this->options     = [];
        $this->siteOptions = [];

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

        Functions\when('get_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default);
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);

            return true;
        });
        Functions\when('get_site_option')->alias(fn ($name, $default = false) => array_key_exists($name, $this->siteOptions) ? $this->siteOptions[$name] : $default);
        Functions\when('update_site_option')->alias(function ($name, $value) {
            $this->siteOptions[$name] = $value;

            return true;
        });
        Functions\when('delete_site_option')->alias(function ($name) {
            unset($this->siteOptions[$name]);

            return true;
        });
        Functions\when('get_current_blog_id')->justReturn(1);

        $this->hadWpdb    = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb  = $GLOBALS['wpdb'] ?? null;
        $this->savedStack = $GLOBALS['_wp_switched_stack'] ?? null;
        unset($GLOBALS['_wp_switched_stack']);
        $GLOBALS['wpdb'] = new class {
            public string $prefix = 'wp_';
        };
    }

    protected function tear_down(): void
    {
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->savedWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
        if ($this->savedStack !== null) {
            $GLOBALS['_wp_switched_stack'] = $this->savedStack;
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_target_update_and_its_revisions_allowed(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, self::PATTERNS);
        $scope->run(function (): void {
            $this->fire('wp_insert_post', self::TARGET, $this->post(self::TARGET, 'page', 0), true);
            $this->fire('wp_insert_post', 50, $this->post(50, 'revision', self::TARGET), false);
            $this->fire('wp_insert_post', 51, $this->post(51, 'revision', (string) self::TARGET), false);
            // A revision inserted in this scope may be updated and deleted.
            $this->fire('wp_insert_post', 50, $this->post(50, 'revision', self::TARGET), true);
            $this->fire('delete_post', 51, $this->post(51, 'revision', self::TARGET));
        });

        $this->assertSame([
            'clean'            => true,
            'violations'       => [],
            'http_hosts'       => [],
            'revision_ids'     => [50, 51],
            'target_meta_keys' => [],
        ], $scope->outcome());
    }

    public function test_insert_or_update_of_another_post_is_a_violation(): void
    {
        $cases = [
            'update of another post'           => [self::OTHER, $this->post(self::OTHER, 'page', 0), true],
            'insert of a new post'             => [90, $this->post(90, 'post', 0), false],
            'insert of a child of the target'  => [91, $this->post(91, 'page', self::TARGET), false],
            'revision of another post'         => [92, $this->post(92, 'revision', self::OTHER), false],
            'update of an earlier revision'    => [60, $this->post(60, 'revision', self::TARGET), true],
            'insert without a post object'     => [93, null, false],
        ];
        foreach ($cases as $name => $args) {
            $outcome = $this->outcomeOf(function () use ($args): void {
                $this->fire('wp_insert_post', ...$args);
            });
            $this->assertSame([AbilityWriteScope::V_OTHER_POST], $outcome['violations'], $name);
            $this->assertFalse($outcome['clean'], $name);
            $this->assertSame([], $outcome['revision_ids'], $name . ': nothing recorded as the save\'s own');
        }
    }

    public function test_delete_or_trash_outside_recorded_revisions_is_a_violation(): void
    {
        $cases = [
            'delete of another post'      => ['delete_post', self::OTHER, AbilityWriteScope::V_POST_DELETED],
            'delete of the target'        => ['delete_post', self::TARGET, AbilityWriteScope::V_POST_DELETED],
            'delete of an earlier revision' => ['delete_post', 60, AbilityWriteScope::V_POST_DELETED],
            'trash of the target'         => ['wp_trash_post', self::TARGET, AbilityWriteScope::V_POST_TRASHED],
            'trash of another post'       => ['wp_trash_post', self::OTHER, AbilityWriteScope::V_POST_TRASHED],
        ];
        foreach ($cases as $name => [$tag, $id, $label]) {
            $outcome = $this->outcomeOf(function () use ($tag, $id): void {
                $this->fire($tag, $id, 'publish');
            });
            $this->assertSame([$label], $outcome['violations'], $name);
        }
    }

    public function test_meta_write_on_another_post_is_a_violation(): void
    {
        $cases = [
            'added on another post'          => ['added_post_meta', 7, self::OTHER],
            'updated on another post'        => ['updated_post_meta', 7, self::OTHER],
            'deleted on another post'        => ['deleted_post_meta', [7, 8], self::OTHER],
            'added on an earlier revision'   => ['added_post_meta', 7, 60],
            'deleted for every object'       => ['deleted_post_meta', [7], 0],
            'added on an object given as text' => ['added_post_meta', 7, (string) self::OTHER],
        ];
        foreach ($cases as $name => [$tag, $metaId, $objectId]) {
            $outcome = $this->outcomeOf(function () use ($tag, $metaId, $objectId): void {
                $this->fire($tag, $metaId, $objectId, '_elementor_data', '[]');
            });
            $this->assertSame([AbilityWriteScope::V_OTHER_META], $outcome['violations'], $name);
            $this->assertSame([], $outcome['target_meta_keys'], $name . ': no key is recorded for the target');
        }
    }

    public function test_meta_write_on_target_and_revision_allowed(): void
    {
        $outcome = $this->outcomeOf(function (): void {
            $this->fire('added_post_meta', 1, self::TARGET, '_elementor_data', '[]');
            $this->fire('updated_post_meta', 2, self::TARGET, '_elementor_version', '4.3.4');
            $this->fire('updated_post_meta', 1, self::TARGET, '_elementor_data', '[{}]');
            $this->fire('deleted_post_meta', [3], (string) self::TARGET, '_elementor_css', '');
            $this->fire('added_post_meta', 4, self::TARGET, '123', 'a numeric key stays text');
            $this->fire('wp_insert_post', 50, $this->post(50, 'revision', self::TARGET), false);
            $this->fire('added_post_meta', 5, 50, '_elementor_data', '[{}]');
            $this->fire('deleted_post_meta', [6], 50, '_elementor_css', '');
        });

        $this->assertTrue($outcome['clean']);
        $this->assertSame([], $outcome['violations']);
        $this->assertSame(['_elementor_data', '_elementor_version', '_elementor_css', '123'], $outcome['target_meta_keys']);
        $this->assertSame([50], $outcome['revision_ids']);
    }

    public function test_option_pattern_is_bound_to_the_target(): void
    {
        $own = $this->outcomeOf(function (): void {
            $this->fire('added_option', '_transient__elementor_editor_unsaved_41', 'x');
            $this->fire('updated_option', '_transient_timeout__elementor_editor_unsaved_41', 1, 2);
            $this->fire('deleted_option', '_transient__elementor_editor_unsaved_41');
        }, self::PATTERNS);
        $this->assertTrue($own['clean'], 'the target\'s own options are allowed');

        foreach (['_transient__elementor_editor_unsaved_999', '_transient__elementor_editor_unsaved_410', '_transient__elementor_editor_unsaved_4', 'siteurl'] as $name) {
            $outcome = $this->outcomeOf(function () use ($name): void {
                $this->fire('deleted_option', $name);
            }, self::PATTERNS);
            $this->assertSame([AbilityWriteScope::V_OPTION], $outcome['violations'], $name);
        }

        $other = new AbilityWriteScope(999, self::PATTERNS);
        $other->run(function (): void {
            $this->fire('deleted_option', '_transient__elementor_editor_unsaved_999');
            $this->fire('deleted_option', '_transient__elementor_editor_unsaved_41');
        });
        $this->assertSame([AbilityWriteScope::V_OPTION], $other->outcome()['violations'], 'a scope on 999 admits 999 and not 41');
    }

    public function test_term_change_is_a_violation(): void
    {
        $cases = [
            'term added to the target'     => ['set_object_terms', self::TARGET, ['news'], [3, 5], 'category', false, [3]],
            'term removed from the target' => ['set_object_terms', self::TARGET, [], [], 'category', false, [3]],
            'term appended'                => ['set_object_terms', self::TARGET, [5], [5], 'post_tag', true, []],
            'term set on another post'     => ['set_object_terms', self::OTHER, [5], [5], 'category', false, [3]],
            'unreadable term ids'          => ['set_object_terms', self::TARGET, [5], 'x', 'category', false, [3]],
            'relationships deleted'        => ['deleted_term_relationships', self::TARGET, [3], 'category'],
        ];
        foreach ($cases as $name => $event) {
            $outcome = $this->outcomeOf(function () use ($event): void {
                $this->fire(...$event);
            });
            $this->assertSame([AbilityWriteScope::V_TERMS], $outcome['violations'], $name);
        }
    }

    public function test_setting_the_terms_an_object_already_holds_is_not_a_change(): void
    {
        // Core's post update sets a post's categories again with the ids it holds.
        $outcome = $this->outcomeOf(function (): void {
            $this->fire('set_object_terms', self::TARGET, [1], ['1'], 'category', false, [1]);
            $this->fire('set_object_terms', self::TARGET, [7, 3], [7, 3, 3], 'category', false, [3, 7]);
            $this->fire('set_object_terms', self::TARGET, [], [], 'post_tag', false, []);
        });

        $this->assertTrue($outcome['clean']);
    }

    public function test_map_meta_cap_denies_other_posts(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, []);
        $scope->run(function (): void {
            $this->fire('wp_insert_post', 50, $this->post(50, 'revision', self::TARGET), false);
            foreach (AbilityWriteScope::NARROWED_CAPS as $cap) {
                $this->assertSame(['do_not_allow'], $this->apply('map_meta_cap', ['edit_others_pages'], $cap, 7, [self::OTHER]), $cap . ' on another post');
                $this->assertSame(['do_not_allow'], $this->apply('map_meta_cap', ['edit_others_pages'], $cap, 7, [60]), $cap . ' on an earlier revision');
                $this->assertSame(['edit_others_pages'], $this->apply('map_meta_cap', ['edit_others_pages'], $cap, 7, [self::TARGET]), $cap . ' on the target');
                $this->assertSame(['edit_others_pages'], $this->apply('map_meta_cap', ['edit_others_pages'], $cap, 7, [50]), $cap . ' on its revision');
            }
            $this->assertSame(['do_not_allow'], $this->apply('map_meta_cap', ['edit_pages'], 'edit_post', 7, [(object) ['ID' => self::OTHER]]), 'a post object');
            $this->assertSame(['edit_pages'], $this->apply('map_meta_cap', ['edit_pages'], 'edit_post', 7, [(object) ['ID' => self::TARGET]]), 'the target as an object');
            $this->assertSame(['edit_pages'], $this->apply('map_meta_cap', ['edit_pages'], 'edit_post', 7, ['41']), 'the target as text');
            $this->assertSame(['do_not_allow'], $this->apply('map_meta_cap', ['edit_pages'], 'edit_post', 7, []), 'no object');
            $this->assertSame(['read'], $this->apply('map_meta_cap', ['read'], 'read_post', 7, [self::OTHER]), 'reads are not narrowed');
        });

        $this->assertTrue($scope->outcome()['clean'], 'a denied capability is not itself a violation');
    }

    public function test_forbidden_caps_forced_false(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, []);
        $scope->run(function (): void {
            $all = [
                'read'                => true,
                'edit_posts'          => true,
                'edit_pages'          => true,
                'edit_others_pages'   => true,
                'upload_files'        => true,
                'unfiltered_html'     => true,
                'publish_pages'       => true,
                'publish_posts'       => true,
                'delete_posts'        => true,
                'delete_others_pages' => true,
            ];
            $this->assertSame([
                'read'                => true,
                'edit_posts'          => true,
                'edit_pages'          => true,
                'edit_others_pages'   => true,
                'upload_files'        => true,
                'unfiltered_html'     => false,
                'publish_pages'       => false,
                'publish_posts'       => false,
                'delete_posts'        => false,
                'delete_others_pages' => false,
            ], $this->apply('user_has_cap', $all, ['publish_pages'], ['publish_pages', 7], null));
            $this->assertSame(
                ['read' => true, 'delete_users' => false],
                $this->apply('user_has_cap', ['read' => true], ['delete_users'], ['delete_users'], null),
                'a capability asked for and not held reads as false too'
            );
        });
    }

    public function test_http_refused_and_recorded_not_failed(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, []);
        $refused = $scope->run(fn () => $this->apply('pre_http_request', false, [], 'https://API.Example.com/v1/ping'));

        $this->assertInstanceOf(\WP_Error::class, $refused);
        $this->assertSame(AbilitySideEffects::HTTP_REFUSED, $refused->get_error_code());
        $outcome = $scope->outcome();
        $this->assertTrue($outcome['clean'], 'a refused request does not fail the save');
        $this->assertSame([], $outcome['violations']);
        $this->assertSame(['api.example.com'], $outcome['http_hosts']);
    }

    public function test_role_change_blocked_restored_and_a_violation(): void
    {
        $roles = ['editor' => ['name' => 'Editor', 'capabilities' => ['edit_posts' => true]]];
        $this->options['wp_user_roles'] = $roles;
        // Another test may have left a wp_roles() double behind; reloading
        // the in-memory roles is not what this test is about.
        if (function_exists('wp_roles')) {
            Functions\when('wp_roles')->justReturn(null);
        }
        $scope                          = new AbilityWriteScope(self::TARGET, []);

        $scope->run(function () use ($roles): void {
            $grown = ['editor' => ['name' => 'Editor', 'capabilities' => ['edit_posts' => true, 'manage_options' => true]]];
            $this->assertSame($roles, $this->apply('pre_update_option_wp_user_roles', $grown, $roles, 'wp_user_roles'), 'the update keeps the old roles');
            // A change that does not pass the update filter, e.g. delete then add.
            $this->options['wp_user_roles'] = $grown;
            $this->assertFalse($this->apply('update_user_metadata', null, 7, 'wp_capabilities', ['administrator' => true], ''), 'the capability meta write is blocked');
            $this->fire('set_user_role', 7, 'administrator', ['editor']);
        });

        $this->assertSame($roles, $this->options['wp_user_roles'], 'the roles are put back');
        $outcome = $scope->outcome();
        $this->assertFalse($outcome['clean']);
        $this->assertSame(['role_changed', 'user_capabilities_meta', 'user_roles_option'], $outcome['violations']);
    }

    public function test_disarm_removes_every_hook_even_when_the_work_throws(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, self::PATTERNS);
        $tags  = [];
        try {
            $scope->run(function () use (&$tags): void {
                $tags = array_values(array_unique(array_column($this->hooks, 0)));
                throw new \RuntimeException('the save failed');
            });
            $this->fail('the exception reaches the caller');
        } catch (\RuntimeException $e) {
            $this->assertSame('the save failed', $e->getMessage());
        }

        foreach (array_merge(self::OWN_TAGS, ['pre_http_request', 'updated_option', 'set_user_role', 'update_user_metadata']) as $tag) {
            $this->assertContains($tag, $tags, $tag . ' was installed while armed');
        }
        $this->assertSame([], $this->hooks, 'every hook is removed');
        $this->assertFalse($scope->isArmed());
    }

    public function test_run_returns_the_work_result_and_disarms(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, []);
        $armed = null;

        $this->assertSame('saved', $scope->run(function () use ($scope, &$armed): string {
            $armed = $scope->isArmed();

            return 'saved';
        }));
        $this->assertTrue($armed);
        $this->assertFalse($scope->isArmed());
        $this->assertSame([], $this->hooks);
    }

    public function test_nothing_recorded_when_not_armed(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, self::PATTERNS);
        $this->assertSame([
            'clean'            => true,
            'violations'       => [],
            'http_hosts'       => [],
            'revision_ids'     => [],
            'target_meta_keys' => [],
        ], $scope->outcome(), 'never armed');

        $scope->arm();
        $captured = $this->hooks;
        $scope->disarm();
        $this->assertSame([], $this->hooks);
        $this->assertNotSame([], $captured);

        $events = [
            'wp_insert_post'             => [self::OTHER, $this->post(self::OTHER, 'page', 0), true],
            'delete_post'                => [self::OTHER, null],
            'wp_trash_post'              => [self::OTHER, 'publish'],
            'added_post_meta'            => [7, self::OTHER, 'k', 'v'],
            'updated_post_meta'          => [7, self::TARGET, 'k', 'v'],
            'deleted_post_meta'          => [[7], self::OTHER, 'k', 'v'],
            'set_object_terms'           => [self::OTHER, [5], [5], 'category', false, [3]],
            'deleted_term_relationships' => [self::OTHER, [3], 'category'],
            'map_meta_cap'               => [['edit_pages'], 'edit_post', 7, [self::OTHER]],
            'user_has_cap'               => [['unfiltered_html' => true], ['unfiltered_html'], [], null],
            'pre_http_request'           => [false, [], 'https://api.example.com/'],
            'updated_option'             => ['siteurl', 'a', 'b'],
            'set_user_role'              => [7, 'administrator', []],
        ];
        $filters = ['map_meta_cap', 'user_has_cap', 'pre_http_request'];
        foreach ($captured as [$tag, $callback, , $accepted]) {
            $args = $events[$tag] ?? [null, 7, 'wp_capabilities', 'x'];
            $ret  = $callback(...array_slice($args, 0, $accepted));
            if (in_array($tag, $filters, true) || str_starts_with($tag, 'pre_') || str_ends_with($tag, '_metadata') || str_ends_with($tag, '_by_mid')) {
                $this->assertSame($args[0], $ret, $tag . ' passes its value through');
            }
        }

        $this->assertSame([
            'clean'            => true,
            'violations'       => [],
            'http_hosts'       => [],
            'revision_ids'     => [],
            'target_meta_keys' => [],
        ], $scope->outcome(), 'callbacks fired after disarm record nothing');
    }

    public function test_arming_again_starts_a_fresh_record(): void
    {
        $scope = new AbilityWriteScope(self::TARGET, []);
        $scope->run(function (): void {
            $this->fire('wp_insert_post', 50, $this->post(50, 'revision', self::TARGET), false);
            $this->fire('added_post_meta', 1, self::TARGET, '_elementor_data', '[]');
            $this->fire('delete_post', self::OTHER, null);
        });
        $this->assertFalse($scope->outcome()['clean']);

        $scope->run(static function (): void {
        });
        $this->assertSame([
            'clean'            => true,
            'violations'       => [],
            'http_hosts'       => [],
            'revision_ids'     => [],
            'target_meta_keys' => [],
        ], $scope->outcome());
    }

    public function test_counts_are_capped_and_the_cap_is_a_violation(): void
    {
        $revisions = $this->outcomeOf(function (): void {
            for ($id = 1000; $id < 1051; $id++) {
                $this->fire('wp_insert_post', $id, $this->post($id, 'revision', self::TARGET), false);
            }
        });
        $this->assertSame([AbilityWriteScope::V_REVISIONS], $revisions['violations']);
        $this->assertCount(50, $revisions['revision_ids']);

        $keys = $this->outcomeOf(function (): void {
            for ($i = 0; $i < 201; $i++) {
                $this->fire('added_post_meta', $i + 1, self::TARGET, 'key_' . $i, 'v');
            }
        });
        $this->assertSame([AbilityWriteScope::V_KEYS], $keys['violations']);
        $this->assertCount(200, $keys['target_meta_keys']);
    }

    public function test_violation_labels_carry_no_site_text(): void
    {
        $outcome = $this->outcomeOf(function (): void {
            $this->fire('updated_option', '<b>Site Name</b>', 'a', 'b');
            $this->fire('wp_insert_post', self::OTHER, $this->post(self::OTHER, 'page', 0), true);
            $this->fire('user_register', 8);
        });

        $this->assertSame(['option_written', 'other_post_written', 'user_changed'], $outcome['violations']);
        foreach ($outcome['violations'] as $label) {
            $this->assertMatchesRegularExpression('/^[a-z_]+$/', $label);
        }
    }

    public function test_target_must_be_a_post_id(): void
    {
        foreach ([0, -41] as $id) {
            try {
                new AbilityWriteScope($id, []);
                $this->fail('refused: ' . $id);
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Run $events inside a scope on the target and return its outcome.
     *
     * @param list<mixed> $patterns Option patterns.
     * @return array{clean:bool,violations:list<string>,http_hosts:list<string>,revision_ids:list<int>,target_meta_keys:list<string>}
     */
    private function outcomeOf(callable $events, array $patterns = []): array
    {
        $scope = new AbilityWriteScope(self::TARGET, $patterns);
        $scope->run($events);

        return $scope->outcome();
    }

    /**
     * Run the captured callbacks for $tag in priority order, as
     * apply_filters() does, threading the first argument through.
     *
     * @param mixed ...$args Value, then the extra arguments.
     * @return mixed
     */
    private function apply(string $tag, mixed ...$args): mixed
    {
        $rows = array_values(array_filter($this->hooks, static fn ($r) => $r[0] === $tag));
        usort($rows, static fn ($a, $b) => $a[2] <=> $b[2]);
        foreach ($rows as [, $callback, , $accepted]) {
            $args[0] = $callback(...array_slice($args, 0, $accepted));
        }

        return $args[0] ?? null;
    }

    /**
     * Run the captured callbacks for an action.
     *
     * @param mixed ...$args Arguments core passes.
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
     * A post as the insert hook passes it.
     *
     * @param int|string $parent Parent id.
     */
    private function post(int $id, string $type, $parent): object
    {
        return (object) ['ID' => $id, 'post_type' => $type, 'post_parent' => $parent];
    }
}
