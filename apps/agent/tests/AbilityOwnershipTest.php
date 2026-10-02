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
        $exec = $this->closureIn('content/plugins/bricks/includes/abilities.php');
        $perm = $this->functionIn('content/plugins/bricks/bricks.php');

        $c = AbilityOwnership::classify($this->ability($exec, $perm), $this->roots);

        $this->assertSame(['owner_kind' => 'plugin', 'owner_dir' => 'bricks', 'owner_split' => false, 'ability_class_ok' => true], $c);
        $this->assertNull(AbilityOwnership::refusal($this->ability($exec, $perm), 'plugin', 'bricks', $this->roots));
    }

    public function test_plugin_dir_with_the_owner_name_as_prefix_is_not_the_owner(): void
    {
        $exec = $this->closureIn('content/plugins/bricks-evil/x.php');
        $perm = $this->closureIn('content/plugins/bricks-evil/y.php');
        $a    = $this->ability($exec, $perm);

        $this->assertSame('bricks-evil', AbilityOwnership::classify($a, $this->roots)['owner_dir']);
        $this->assertSame(AbilityOwnership::REFUSE_MISMATCH, AbilityOwnership::refusal($a, 'plugin', 'bricks', $this->roots));
    }

    public function test_a_symlinked_plugin_dir_resolves_to_its_link_name_and_not_to_a_prefix_sibling(): void
    {
        $this->closureIn('shared/bricks/x.php');
        symlink($this->base . '/shared/bricks', $this->base . '/content/plugins/bricks');
        $linked = $this->closureIn('content/plugins/bricks/y.php');
        $this->assertSame(
            ['kind' => 'plugin', 'dir' => 'bricks'],
            AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($linked), $this->roots)
        );

        // A sibling of the link target whose name starts with the target's.
        $sibling = $this->closureIn('shared/bricks-evil/z.php');
        $this->assertSame(
            ['kind' => 'unknown', 'dir' => ''],
            AbilityOwnership::classifyPath(AbilityOwnership::sourceFile($sibling), $this->roots)
        );
    }

    public function test_permission_callback_from_another_plugin_is_owner_split(): void
    {
        $exec = $this->closureIn('content/plugins/bricks/a.php');
        $perm = $this->closureIn('content/plugins/evil/b.php');
        $a    = $this->ability($exec, $perm);

        $c = AbilityOwnership::classify($a, $this->roots);
        $this->assertTrue($c['owner_split']);
        $this->assertSame('unknown', $c['owner_kind']);
        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($a, 'plugin', 'bricks', $this->roots));
    }

    public function test_execute_callback_from_another_plugin_is_owner_split(): void
    {
        $a = $this->ability($this->closureIn('content/plugins/evil/a.php'), $this->closureIn('content/plugins/bricks/b.php'));

        $this->assertSame(AbilityOwnership::REFUSE_SPLIT, AbilityOwnership::refusal($a, 'plugin', 'bricks', $this->roots));
    }

    public function test_callbacks_swapped_at_registration_by_a_second_plugin_are_refused(): void
    {
        $args = [
            'execute_callback'    => $this->closureIn('content/plugins/bricks/a.php'),
            'permission_callback' => $this->closureIn('content/plugins/bricks/b.php'),
        ];
        $this->assertNull(AbilityOwnership::refusal(new \WP_Ability('bricks/get', $args), 'plugin', 'bricks', $this->roots));

        // The second plugin's registration-args filter, as core applies it
        // before constructing the ability.
        $evilExec = $this->closureIn('content/plugins/evil/swap.php');
        $evilPerm = $this->closureIn('content/plugins/evil/perm.php');
        $swapOne  = static fn (array $a): array => ['execute_callback' => $evilExec] + $a;
        $swapBoth = static fn (array $a): array => ['execute_callback' => $evilExec, 'permission_callback' => $evilPerm] + $a;

        $this->assertSame(
            AbilityOwnership::REFUSE_SPLIT,
            AbilityOwnership::refusal(new \WP_Ability('bricks/get', $swapOne($args)), 'plugin', 'bricks', $this->roots)
        );
        $this->assertSame(
            AbilityOwnership::REFUSE_MISMATCH,
            AbilityOwnership::refusal(new \WP_Ability('bricks/get', $swapBoth($args)), 'plugin', 'bricks', $this->roots)
        );
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
        $exec = $this->closureIn('content/plugins/bricks/a.php');
        $perm = $this->closureIn('content/plugins/bricks/b.php');
        $a    = new OwnershipTestOverridingAbility('bricks/get', ['execute_callback' => $exec, 'permission_callback' => $perm]);

        $this->assertFalse(AbilityOwnership::abilityClassOk($a));
        $this->assertSame(AbilityOwnership::REFUSE_CLASS, AbilityOwnership::refusal($a, 'plugin', 'bricks', $this->roots));
    }

    public function test_a_subclass_overriding_a_protected_execution_method_is_refused(): void
    {
        $a = new OwnershipTestOverridingInvokeAbility('bricks/get', []);

        $this->assertFalse(AbilityOwnership::abilityClassOk($a));
    }

    public function test_a_subclass_that_overrides_nothing_on_the_execution_path_is_accepted(): void
    {
        $exec = $this->closureIn('content/plugins/bricks/a.php');
        $perm = $this->closureIn('content/plugins/bricks/b.php');
        $a    = new OwnershipTestHarmlessAbility('bricks/get', ['execute_callback' => $exec, 'permission_callback' => $perm]);

        $this->assertTrue(AbilityOwnership::abilityClassOk($a));
        $this->assertNull(AbilityOwnership::refusal($a, 'plugin', 'bricks', $this->roots));
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
            'bricks-evil/bricks-evil.php' => ['Version' => '9.9.9'],
            'bricks/bricks.php'           => ['Version' => '2.4.1'],
        ];
        $a = $this->ability($this->closureIn('content/plugins/bricks/a.php'), $this->closureIn('content/plugins/bricks/b.php'));

        $this->assertSame([
            'owner_kind'       => 'plugin',
            'owner_dir'        => 'bricks',
            'owner_version'    => '2.4.1',
            'owner_split'      => false,
            'ability_class_ok' => true,
        ], AbilityOwnership::inventoryFields($a, $this->roots, $plugins));
        $this->assertSame('hello', AbilityOwnership::ownerVersion('plugin', 'hello.php', ['hello.php' => ['Version' => 'hello']]), 'a single-file plugin');
        $this->assertNull(AbilityOwnership::ownerVersion('plugin', 'bricks', ['bricks-pro/x.php' => ['Version' => '1']]), 'a prefix sibling is not the plugin');
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
        $file = $this->write($rel, '<?php return static function ($input = null) { return true; };');

        return require $file;
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
