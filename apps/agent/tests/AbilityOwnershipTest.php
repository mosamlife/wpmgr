<?php
/**
 * AbilityOwnership tests: callbacks resolved by Reflection to real files laid
 * out like a WordPress install, classified against canonical roots.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Abilities\AbilityOwnership;
use WPMgr\Agent\Abilities\OwnAbilities;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * A subclass that overrides one execution-path method.
 */
class OwnershipTestOverridingAbility extends \WP_Ability
{
    /** @param mixed $input Input. @return mixed */
    public function execute($input = null)
    {
        return 'ran without the filters';
    }
}

/**
 * A subclass that overrides a protected execution-path method.
 */
class OwnershipTestOverridingInvokeAbility extends \WP_Ability
{
    /** @param mixed $input Input. @return mixed */
    protected function invoke_callback(callable $callback, $input = null)
    {
        return 'ran other code';
    }
}

/**
 * A subclass that adds behaviour but overrides nothing on the execution path.
 */
class OwnershipTestHarmlessAbility extends \WP_Ability
{
    public function describe(): string
    {
        return 'extra';
    }
}

/**
 * @covers \WPMgr\Agent\Abilities\AbilityOwnership
 * @covers \WPMgr\Agent\Abilities\OwnAbilities::siteRow
 */
final class AbilityOwnershipTest extends TestCase
{
    private string $base;

    /** @var array<string,string> */
    private array $roots;

    /** Unique suffix per test, so included functions never collide. */
    private string $sfx;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->sfx  = bin2hex(random_bytes(6));
        $this->base = sys_get_temp_dir() . '/wpmgr-own-' . $this->sfx;
        foreach (['wp/wp-includes', 'wp/wp-admin', 'content/plugins', 'content/mu-plugins', 'content/themes', 'shared'] as $d) {
            mkdir($this->base . '/' . $d, 0755, true);
        }
        $this->roots = AbilityOwnership::canonicalRoots([
            'core_inc'   => $this->base . '/wp/wp-includes',
            'core_admin' => $this->base . '/wp/wp-admin',
            'plugin'     => $this->base . '/content/plugins',
            'mu-plugin'  => $this->base . '/content/mu-plugins',
            'theme'      => $this->base . '/content/themes',
        ]);
    }

    protected function tear_down(): void
    {
        $this->rmTree($this->base);
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Kinds
    // -------------------------------------------------------------------------

    public function test_both_callbacks_in_one_plugin_classify_as_that_plugin(): void
    {
        $exec = $this->closureIn('content/plugins/acmebuild/includes/abilities.php');
        $perm = $this->functionIn('content/plugins/acmebuild/acmebuild.php');

        $c = AbilityOwnership::classify($this->ability($exec, $perm), $this->roots);

        $this->assertSame(['owner_kind' => 'plugin', 'owner_dir' => 'acmebuild', 'owner_split' => false, 'ability_class_ok' => true], $c);
        $this->assertNull(AbilityOwnership::refusal($this->ability($exec, $perm), 'plugin', 'acmebuild', $this->roots));
    }

    public function test_plugin_dir_with_the_owner_name_as_prefix_is_not_the_owner(): void
    {
        $exec = $this->closureIn('content/plugins/acmebuild-evil/x.php');
        $perm = $this->closureIn('content/plugins/acmebuild-evil/y.php');
        $a    = $this->ability($exec, $perm);

        $this->assertSame('acmebuild-evil', AbilityOwnership::classify($a, $this->roots)['owner_dir']);
        $this->assertSame(AbilityOwnership::REFUSE_MISMATCH, AbilityOwnership::refusal($a, 'plugin', 'acmebuild', $this->roots));
    }

    public function test_a_symlinked_plugin_dir_resolves_to_its_link_name_and_not_to_a_prefix_sibling(): void
    {
        $this->closureIn('shared/acmebuild/x.php');
        symlink($this->base . '/shared/acmebuild', $this->base . '/content/plugins/acmebuild');
        $linked = $this->closureIn('content/plugins/acmebuild/y.php');
        $this->assertSame(
            ['kind' => 'plugin', 'dir' => 'acmebuild'],
            AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($linked), $this->roots)
        );

        // A sibling of the link target whose name starts with the target's.
        $sibling = $this->closureIn('shared/acmebuild-evil/z.php');
        $this->assertSame(
            ['kind' => 'unknown', 'dir' => ''],
            AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($sibling), $this->roots)
        );
    }

    public function test_permission_callback_from_another_plugin_is_owner_split(): void
    {
        $exec = $this->closureIn('content/plugins/acmebuild/a.php');
        $perm = $this->closureIn('content/plugins/evil/b.php');
        $a    = $this->ability($exec, $perm);

        $c = AbilityOwnership::classify($a, $this->roots);
        $this->assertTrue($c['owner_split']);
        $this->assertSame('unknown', $c['owner_kind']);
        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($a, 'plugin', 'acmebuild', $this->roots));
    }

    public function test_execute_callback_from_another_plugin_is_owner_split(): void
    {
        $a = $this->ability($this->closureIn('content/plugins/evil/a.php'), $this->closureIn('content/plugins/acmebuild/b.php'));

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($a, 'plugin', 'acmebuild', $this->roots));
    }

    public function test_callbacks_swapped_at_registration_by_a_second_plugin_are_refused(): void
    {
        $args = [
            'execute_callback'    => $this->closureIn('content/plugins/acmebuild/a.php'),
            'permission_callback' => $this->closureIn('content/plugins/acmebuild/b.php'),
        ];
        $this->assertNull(AbilityOwnership::refusal(new \WP_Ability('acmebuild/get', $args), 'plugin', 'acmebuild', $this->roots));

        // The second plugin's registration-args filter, as core applies it
        // before constructing the ability.
        $evilExec = $this->closureIn('content/plugins/evil/swap.php');
        $evilPerm = $this->closureIn('content/plugins/evil/perm.php');
        $swapOne  = static fn (array $a): array => ['execute_callback' => $evilExec] + $a;
        $swapBoth = static fn (array $a): array => ['execute_callback' => $evilExec, 'permission_callback' => $evilPerm] + $a;

        $this->assertSame(
            AbilityOwnership::REFUSE_SPLIT,
            AbilityOwnership::refusal(new \WP_Ability('acmebuild/get', $swapOne($args)), 'plugin', 'acmebuild', $this->roots)
        );
        $this->assertSame(
            AbilityOwnership::REFUSE_MISMATCH,
            AbilityOwnership::refusal(new \WP_Ability('acmebuild/get', $swapBoth($args)), 'plugin', 'acmebuild', $this->roots)
        );
    }

    /**
     * The owner plugin ships a trait, a base class and a tool whose closures
     * call helpers on $this; a second plugin reuses each so its own code runs.
     *
     * @return array<string,string> Class names by role.
     */
    private function builderAndOther(): array
    {
        $x = $this->sfx;
        $n = [
            'trait' => 'OwnTrait' . $x, 'base' => 'OwnBase' . $x, 'tool' => 'OwnTool' . $x,
            'viaTrait' => 'OtherViaTrait' . $x, 'viaInherit' => 'OtherViaInherit' . $x, 'otherTool' => 'OtherTool' . $x,
        ];
        $lib = $this->write('content/plugins/acmebuild/lib.php', '<?php
trait ' . $n['trait'] . ' { public function run($i = null) { return $this->payload(); } public function perm($i = null) { return $this->allowed(); } }
class ' . $n['base'] . ' { public function run($i = null) { return $this->payload(); } public function perm($i = null) { return $this->allowed(); }
  protected function payload() { return "owner"; } protected function allowed() { return false; } }
class ' . $n['tool'] . ' { public function closures() { return [function ($i = null) { return $this->payload(); }, function ($i = null) { return $this->allowed(); }]; }
  protected function payload() { return "owner"; } protected function allowed() { return false; } }
');
        $other = $this->write('content/plugins/other/main.php', '<?php
class ' . $n['viaTrait'] . ' { use ' . $n['trait'] . '; protected function payload() { return "other"; } protected function allowed() { return true; } }
class ' . $n['viaInherit'] . ' extends ' . $n['base'] . ' { protected function payload() { return "other"; } protected function allowed() { return true; } }
class ' . $n['otherTool'] . ' extends ' . $n['tool'] . ' { protected function payload() { return "other"; } protected function allowed() { return true; } }
');
        require_once $lib;
        require_once $other;

        return $n;
    }

    public function test_a_trait_method_used_by_another_plugin_is_split(): void
    {
        $n = $this->builderAndOther();
        $o = new $n['viaTrait']();

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability([$o, 'run'], [$o, 'perm']), 'plugin', 'acmebuild', $this->roots));
    }

    public function test_an_inherited_method_with_helpers_overridden_by_another_plugin_is_split(): void
    {
        $n = $this->builderAndOther();
        $o = new $n['viaInherit']();

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability([$o, 'run'], [$o, 'perm']), 'plugin', 'acmebuild', $this->roots));
        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($n['viaInherit'] . '::run', $n['viaInherit'] . '::perm'), 'plugin', 'acmebuild', $this->roots), 'the Class::method form');
    }

    public function test_an_owner_closure_rebound_to_another_plugins_object_is_split(): void
    {
        $n          = $this->builderAndOther();
        [$c1, $c2]  = (new $n['tool']())->closures();
        $o          = new $n['otherTool']();
        $exec       = \Closure::bind($c1, $o, $n['otherTool']);
        $perm       = \Closure::bind($c2, $o, $n['otherTool']);

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($exec, $perm), 'plugin', 'acmebuild', $this->roots));

        // Rebound to the other plugin's object while keeping the owner's
        // scope: $this still dispatches to the other plugin's overrides.
        $exec = \Closure::bind($c1, $o, $n['tool']);
        $perm = \Closure::bind($c2, $o, $n['tool']);
        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($exec, $perm), 'plugin', 'acmebuild', $this->roots), 'owner scope, foreign object');
    }

    public function test_an_owner_class_using_a_trait_from_another_plugin_is_split(): void
    {
        $x     = $this->sfx;
        $trait = 'ForeignTrait' . $x;
        $cls   = 'OwnUsesForeign' . $x;
        $t     = $this->write('content/plugins/other/trait.php', '<?php trait ' . $trait . ' { protected function helper() { return "other"; } }');
        $c     = $this->write('content/plugins/acmebuild/uses.php', '<?php class ' . $cls . ' { use ' . $trait . '; public function run($i = null) { return $this->helper(); } public function perm($i = null) { return true; } }');
        require_once $t;
        require_once $c;
        $o = new $cls();

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability([$o, 'run'], [$o, 'perm']), 'plugin', 'acmebuild', $this->roots));
    }

    public function test_the_owners_own_objects_and_closures_still_classify_as_the_owner(): void
    {
        $n         = $this->builderAndOther();
        [$c1, $c2] = (new $n['tool']())->closures();
        $base      = new $n['base']();

        $this->assertNull(AbilityOwnership::refusal($this->ability($c1, $c2), 'plugin', 'acmebuild', $this->roots));
        $this->assertNull(AbilityOwnership::refusal($this->ability([$base, 'run'], [$base, 'perm']), 'plugin', 'acmebuild', $this->roots));
    }

    public function test_a_static_closure_late_bound_to_another_plugins_subclass_is_split(): void
    {
        $x    = $this->sfx;
        $base = 'LateBase' . $x;
        $sub  = 'LateSub' . $x;
        $lib  = $this->write('content/plugins/acmebuild/late.php', '<?php class ' . $base . ' {
  public static function makeRun() { return static function ($i = null) { return static::payload(); }; }
  public static function makePerm() { return static function ($i = null) { return static::allowed(); }; }
  protected static function payload() { return "owner"; } protected static function allowed() { return false; } }');
        $oth  = $this->write('content/plugins/other/late.php', '<?php class ' . $sub . ' extends ' . $base . ' {
  protected static function payload() { return "other"; } protected static function allowed() { return true; } }');
        require_once $lib;
        require_once $oth;

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($sub::makeRun(), $sub::makePerm()), 'plugin', 'acmebuild', $this->roots));
        $this->assertNull(AbilityOwnership::refusal($this->ability($base::makeRun(), $base::makePerm()), 'plugin', 'acmebuild', $this->roots), 'the owner\'s own class still classifies as the owner');
    }

    public function test_an_owner_closure_capturing_another_plugins_callable_or_object_is_split(): void
    {
        $x     = $this->sfx;
        $wrap  = 'own_wrap_' . $x;
        $oRun  = 'other_run_' . $x;
        $oCls  = 'OtherCaptured' . $x;
        $lib   = $this->write('content/plugins/acmebuild/wrap.php', '<?php function ' . $wrap . '($c) { return function (...$a) use ($c) { return is_callable($c) ? $c(...$a) : $c; }; }');
        $oth   = $this->write('content/plugins/other/run.php', '<?php function ' . $oRun . '($i = null) { return "other"; } class ' . $oCls . ' { public $v = 1; }');
        require_once $lib;
        require_once $oth;

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($wrap($oRun), $wrap($oRun)), 'plugin', 'acmebuild', $this->roots), 'a captured function');
        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($wrap(new $oCls()), $wrap(new $oCls())), 'plugin', 'acmebuild', $this->roots), 'a captured object');
        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($this->ability($wrap([[$oRun]]), $wrap([[$oRun]])), 'plugin', 'acmebuild', $this->roots), 'a callable inside a captured array');

        // Built-in functions and plain data captured by the owner stay the owner's.
        $this->assertNull(AbilityOwnership::refusal($this->ability($wrap('strlen'), $wrap(['k' => 'v', 3])), 'plugin', 'acmebuild', $this->roots));
    }

    public function test_captures_nested_past_the_depth_bound_are_unknown(): void
    {
        $x    = $this->sfx;
        $wrap = 'own_nest_' . $x;
        $lib  = $this->write('content/plugins/acmebuild/nest.php', '<?php function ' . $wrap . '($c) { return function () use ($c) { return $c; }; }');
        require_once $lib;
        $deep = 'strlen';
        for ($i = 0; $i < 8; $i++) {
            $deep = $wrap($deep);
        }

        $this->assertNull(AbilityOwnership::sourceFiles($deep));
    }

    public function test_core_mu_plugin_and_theme_kinds(): void
    {
        $core = $this->functionIn('wp/wp-includes/functions.php');
        $this->assertSame(['kind' => 'core', 'dir' => ''], AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($core), $this->roots));
        $admin = $this->functionIn('wp/wp-admin/includes/x.php');
        $this->assertSame(['kind' => 'core', 'dir' => ''], AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($admin), $this->roots));

        $mu = $this->functionIn('content/mu-plugins/loader.php');
        $this->assertSame(['kind' => 'mu-plugin', 'dir' => 'loader.php'], AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($mu), $this->roots));

        $theme = $this->closureIn('content/themes/twenty/functions.php');
        $this->assertSame(['kind' => 'theme', 'dir' => 'twenty'], AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($theme), $this->roots));

        $outside = $this->closureIn('shared/elsewhere.php');
        $this->assertSame(['kind' => 'unknown', 'dir' => ''], AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($outside), $this->roots));
    }

    public function test_every_callable_shape_resolves_to_its_file(): void
    {
        $cls  = 'OwnershipShape' . $this->sfx;
        $file = $this->write('content/plugins/shapes/c.php', '<?php class ' . $cls . ' { public static function s() {} public function m() {} public function __invoke() {} }');
        require $file;
        $real = (string) realpath($file);

        $this->assertSame($real, AbilityOwnership::sourceFile($cls . '::s'));
        $this->assertSame($real, AbilityOwnership::sourceFile([$cls, 's']));
        $this->assertSame($real, AbilityOwnership::sourceFile([new $cls(), 'm']));
        $this->assertSame($real, AbilityOwnership::sourceFile(new $cls()));
    }

    public function test_internal_missing_and_evald_callbacks_are_unknown(): void
    {
        $this->assertNull(AbilityOwnership::sourceFile('strlen'));
        $this->assertNull(AbilityOwnership::sourceFile('no_such_function_' . $this->sfx));
        $this->assertNull(AbilityOwnership::sourceFile(['NoSuchClass' . $this->sfx, 'x']));
        $this->assertNull(AbilityOwnership::sourceFile(null));
        $this->assertNull(AbilityOwnership::sourceFile(42));

        $a = $this->ability('strlen', 'strlen');
        $this->assertSame(AbilityOwnership::REFUSE_MISMATCH, AbilityOwnership::refusal($a, 'unknown', '', $this->roots), 'unknown is never an owner');
    }

    // -------------------------------------------------------------------------
    // ability_class
    // -------------------------------------------------------------------------

    public function test_a_subclass_overriding_execute_is_refused(): void
    {
        $exec = $this->closureIn('content/plugins/acmebuild/a.php');
        $perm = $this->closureIn('content/plugins/acmebuild/b.php');
        $a    = new OwnershipTestOverridingAbility('acmebuild/get', ['execute_callback' => $exec, 'permission_callback' => $perm]);

        $this->assertFalse(AbilityOwnership::abilityClassOk($a));
        $this->assertSame(AbilityOwnership::REFUSE_CLASS, AbilityOwnership::refusal($a, 'plugin', 'acmebuild', $this->roots));
    }

    public function test_a_subclass_overriding_a_protected_execution_method_is_refused(): void
    {
        $a = new OwnershipTestOverridingInvokeAbility('acmebuild/get', []);

        $this->assertFalse(AbilityOwnership::abilityClassOk($a));
    }

    public function test_a_subclass_that_overrides_nothing_on_the_execution_path_is_accepted(): void
    {
        $exec = $this->closureIn('content/plugins/acmebuild/a.php');
        $perm = $this->closureIn('content/plugins/acmebuild/b.php');
        $a    = new OwnershipTestHarmlessAbility('acmebuild/get', ['execute_callback' => $exec, 'permission_callback' => $perm]);

        $this->assertTrue(AbilityOwnership::abilityClassOk($a));
        $this->assertNull(AbilityOwnership::refusal($a, 'plugin', 'acmebuild', $this->roots));
    }

    public function test_an_object_that_is_not_a_wp_ability_is_not_ok(): void
    {
        $this->assertFalse(AbilityOwnership::abilityClassOk(new \stdClass()));
    }

    // -------------------------------------------------------------------------
    // Inventory row
    // -------------------------------------------------------------------------

    public function test_inventory_fields_carry_the_plugin_version(): void
    {
        // Injected, not mocked: a mocked get_plugins() would exist for the
        // rest of the process and change what other suites see.
        $plugins = [
            'acmebuild-evil/acmebuild-evil.php' => ['Version' => '9.9.9'],
            'acmebuild/acmebuild.php'           => ['Version' => '2.4.1'],
        ];
        $a = $this->ability($this->closureIn('content/plugins/acmebuild/a.php'), $this->closureIn('content/plugins/acmebuild/b.php'));

        $this->assertSame([
            'owner_kind'       => 'plugin',
            'owner_dir'        => 'acmebuild',
            'owner_version'    => '2.4.1',
            'owner_split'      => false,
            'ability_class_ok' => true,
        ], AbilityOwnership::inventoryFields($a, $this->roots, $plugins));
        $this->assertSame('hello', AbilityOwnership::ownerVersion('plugin', 'hello.php', ['hello.php' => ['Version' => 'hello']]), 'a single-file plugin');
        $this->assertNull(AbilityOwnership::ownerVersion('plugin', 'acmebuild', ['acmebuild-pro/x.php' => ['Version' => '1']]), 'a prefix sibling is not the plugin');
    }

    public function test_core_version_comes_from_wp_version(): void
    {
        $GLOBALS['wp_version'] = '7.1';
        try {
            $this->assertSame('7.1', AbilityOwnership::ownerVersion('core', ''));
        } finally {
            unset($GLOBALS['wp_version']);
        }
        $this->assertNull(AbilityOwnership::ownerVersion('unknown', ''));
        $this->assertNull(AbilityOwnership::ownerVersion('plugin', ''));
    }

    public function test_site_row_marks_a_squat_split_or_overridden_ability_as_mismatched(): void
    {
        $ok = $this->ability('strlen', 'strlen');

        $squat = OwnAbilities::siteRow('wpmgr/fake', $ok);
        $this->assertSame('wpmgr_squat', $squat['owner_kind']);
        $this->assertTrue($squat['owner_mismatch']);

        $plain = OwnAbilities::siteRow('acme/thing', $ok);
        $this->assertSame('unknown', $plain['owner_kind']);
        $this->assertFalse($plain['owner_mismatch']);
        foreach (['owner_dir', 'owner_version', 'owner_split', 'ability_class_ok', 'version'] as $k) {
            $this->assertArrayHasKey($k, $plain);
        }

        $over = OwnAbilities::siteRow('acme/thing', new OwnershipTestOverridingAbility('acme/thing', []));
        $this->assertFalse($over['ability_class_ok']);
        $this->assertTrue($over['owner_mismatch']);
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * @param mixed $exec Execute callback.
     * @param mixed $perm Permission callback.
     */
    private function ability($exec, $perm): \WP_Ability
    {
        return new \WP_Ability('acme/test', ['execute_callback' => $exec, 'permission_callback' => $perm]);
    }

    /**
     * Write a file that returns a closure, include it, return the closure.
     */
    private function closureIn(string $rel): \Closure
    {
        // Made inside a plain function, so the closure has no bound object and
        // no class scope (one written directly in an included file would
        // inherit this test class as its scope).
        $fn   = 'wpmgr_ownership_mk_' . $this->sfx . '_' . substr(hash('sha256', $rel), 0, 8);
        $file = $this->write($rel, '<?php function ' . $fn . '() { return static function ($input = null) { return true; }; }');
        require_once $file;

        return $fn();
    }

    /**
     * Write a file that defines a uniquely named function; return its name.
     */
    private function functionIn(string $rel): string
    {
        $name = 'wpmgr_ownership_fn_' . $this->sfx . '_' . substr(hash('sha256', $rel), 0, 8);
        $file = $this->write($rel, '<?php function ' . $name . '() { return true; }');
        require $file;

        return $name;
    }

    private function write(string $rel, string $code): string
    {
        $path = $this->base . '/' . $rel;
        if (!is_dir(dirname($path))) {
            mkdir(dirname($path), 0755, true);
        }
        file_put_contents($path, $code);

        return $path;
    }

    private function rmTree(string $dir): void
    {
        if (is_link($dir)) {
            unlink($dir);
            return;
        }
        if (!is_dir($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $e) {
            if ($e === '.' || $e === '..') {
                continue;
            }
            $p = $dir . '/' . $e;
            if (is_link($p) || !is_dir($p)) {
                unlink($p);
            } else {
                $this->rmTree($p);
            }
        }
        rmdir($dir);
    }
}
