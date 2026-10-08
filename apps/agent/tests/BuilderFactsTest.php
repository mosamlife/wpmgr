<?php
/**
 * Tests for the `builder_facts` collector.
 *
 * The properties pinned here are the ones the control plane relies on: the
 * block is read-only, never throws, reports unknown as null or as an omitted
 * key (never as false), reads the Atomic editor from Elementor's own manager,
 * and sends exactly the documented keys and no reserved key.
 *
 * Every test stubs what it reads. The WordPress function stubs this suite
 * creates outlive the test that created them, so no test here relies on a
 * function being undefined.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\Attributes\RunInSeparateProcess;
use WPMgr\Agent\Support\BuilderFacts;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\BuilderFacts
 */
final class BuilderFactsTest extends TestCase
{
    /** A class name that is not loaded: stands for "Elementor is not on this site". */
    private const ABSENT_CLASS = 'WPMgr\\Agent\\Tests\\NoSuchElementorPlugin';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        FakeElementorPlugin::$instance = null;
    }

    protected function tear_down(): void
    {
        FakeElementorPlugin::$instance = null;
        Monkey\tearDown();
        parent::tear_down();
    }

    // ---- theme_template ----------------------------------------------------

    public function test_a_site_without_elementor_sends_the_version_and_the_template_only(): void
    {
        $this->stubTemplate('twentytwentyfour');

        $this->assertSame(
            ['v' => 1, 'theme_template' => 'twentytwentyfour'],
            BuilderFacts::collect(self::ABSENT_CLASS)
        );
    }

    public function test_a_child_theme_reports_its_parent_directory_not_its_own(): void
    {
        // The inventory's "active" flag follows the stylesheet (child) theme.
        // This fact is what closes that gap, so it must name the parent.
        Functions\when('get_stylesheet')->justReturn('bricks-child');
        $this->stubTemplate('bricks');

        $facts = BuilderFacts::collect(self::ABSENT_CLASS);

        $this->assertSame('bricks', $facts['theme_template'] ?? null);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function conformingTemplates(): array
    {
        return [
            'plain slug'              => ['bricks'],
            'default theme'           => ['twentytwentyfour'],
            'mixed case and symbols'  => ['Twenty_Twenty-Four.1'],
            'one character'           => ['a'],
            'exactly the length cap'  => [str_repeat('a', BuilderFacts::MAX_THEME_TEMPLATE_LENGTH)],
        ];
    }

    #[DataProvider('conformingTemplates')]
    public function test_a_conforming_directory_name_is_reported_as_is(string $name): void
    {
        $this->stubTemplate($name);

        $this->assertSame($name, BuilderFacts::collect(self::ABSENT_CLASS)['theme_template'] ?? null);
    }

    /**
     * @return array<string,array{0:mixed}>
     */
    public static function nonConformingTemplates(): array
    {
        return [
            'empty'                  => [''],
            'path separator'         => ['bricks/child'],
            'parent traversal'       => ['../bricks'],
            'space'                  => ['my theme'],
            'trailing newline'       => ["bricks\n"],
            'leading newline'        => ["\nbricks"],
            'nul byte'               => ["bri\0cks"],
            'non-ascii'              => ['brïcks'],
            'one over the length cap' => [str_repeat('a', BuilderFacts::MAX_THEME_TEMPLATE_LENGTH + 1)],
            'markup'                 => ['<script>'],
            'null'                   => [null],
            'false'                  => [false],
            'integer'                => [7],
            'array'                  => [['bricks']],
        ];
    }

    #[DataProvider('nonConformingTemplates')]
    public function test_a_non_conforming_directory_name_is_omitted_not_repaired(mixed $name): void
    {
        $this->stubTemplate($name);

        // Omitted, and the block is still there: one unreadable fact never
        // removes the version marker or the other facts.
        $this->assertSame(['v' => 1], BuilderFacts::collect(self::ABSENT_CLASS));
    }

    public function test_a_template_read_that_throws_is_omitted_and_does_not_escape(): void
    {
        Functions\when('get_template')->alias(static function (): string {
            throw new \RuntimeException('theme read failed');
        });

        $this->assertSame(['v' => 1], BuilderFacts::collect(self::ABSENT_CLASS));
    }

    // ---- elementor ---------------------------------------------------------

    public function test_elementor_not_loaded_sends_no_elementor_key_at_all(): void
    {
        $this->stubTemplate('twentytwentyfour');

        $facts = BuilderFacts::collect(self::ABSENT_CLASS);

        // Absence of the builder is the absence of the key. A false here would
        // read as "Elementor is installed and its Atomic editor is off".
        $this->assertArrayNotHasKey('elementor', $facts);
    }

    /**
     * @return array<string,array{0:bool}>
     */
    public static function definiteAnswers(): array
    {
        return [
            'active'   => [true],
            'inactive' => [false],
        ];
    }

    #[DataProvider('definiteAnswers')]
    public function test_the_atomic_editor_answer_is_the_managers_answer(bool $answer): void
    {
        $this->stubTemplate('twentytwentyfour');
        $class = $this->loadedElementor(new FakeElementorExperiments($answer));

        $facts = BuilderFacts::collect($class);

        $this->assertSame(['atomic_editor' => $answer], $facts['elementor'] ?? null);
    }

    public function test_the_collector_asks_the_manager_about_the_atomic_experiment_by_name(): void
    {
        $this->stubTemplate('twentytwentyfour');
        $manager = new FakeElementorExperiments(true);
        $class   = $this->loadedElementor($manager);

        BuilderFacts::collect($class);

        $this->assertSame(['e_atomic_elements'], $manager->asked);
    }

    public function test_an_experiment_that_is_active_by_default_is_reported_active_with_no_option_row(): void
    {
        // On a fresh install Elementor turns this experiment on by default and
        // stores nothing. The raw option is therefore absent while the manager
        // says "active": only asking the manager gets this right.
        $this->stubTemplate('twentytwentyfour');
        Functions\when('get_option')->alias(static fn ($name, $default = false) => $default);
        $class = $this->loadedElementor(new FakeElementorExperiments(true));

        $facts = BuilderFacts::collect($class);

        $this->assertTrue($facts['elementor']['atomic_editor'] ?? null);
    }

    public function test_a_manager_that_throws_reports_null_and_the_rest_of_the_block_survives(): void
    {
        $this->stubTemplate('bricks');
        $class = $this->loadedElementor(
            new FakeElementorExperiments(true, new \RuntimeException('experiments not ready'))
        );

        $facts = BuilderFacts::collect($class);

        $this->assertSame(
            ['v' => 1, 'theme_template' => 'bricks', 'elementor' => ['atomic_editor' => null]],
            $facts
        );
    }

    /**
     * Elementor is loaded but gives no definite answer. Each of these must be
     * "unknown" (null), never a boolean.
     *
     * @return array<string,array{0:mixed}>
     */
    public static function indefiniteInstances(): array
    {
        $noExperimentsMember = new \stdClass();

        $experimentsNull = new FakeElementorPlugin();

        $experimentsString               = new FakeElementorPlugin();
        $experimentsString->experiments  = 'not-a-manager';

        $experimentsWithoutMethod              = new FakeElementorPlugin();
        $experimentsWithoutMethod->experiments = new \stdClass();

        $answers = static function (mixed $answer): FakeElementorPlugin {
            $plugin              = new FakeElementorPlugin();
            $plugin->experiments = new FakeElementorExperiments($answer);

            return $plugin;
        };

        return [
            'no instance yet'             => [null],
            'instance is not an object'   => ['not-an-object'],
            'no experiments member'       => [$noExperimentsMember],
            'experiments is null'         => [$experimentsNull],
            'experiments is not an object' => [$experimentsString],
            'experiments lacks the method' => [$experimentsWithoutMethod],
            'answers integer one'         => [$answers(1)],
            'answers integer zero'        => [$answers(0)],
            'answers a string'            => [$answers('active')],
            'answers null'                => [$answers(null)],
            'answers an array'            => [$answers([true])],
        ];
    }

    #[DataProvider('indefiniteInstances')]
    public function test_an_indefinite_elementor_state_is_null_never_false(mixed $instance): void
    {
        $this->stubTemplate('twentytwentyfour');
        FakeElementorPlugin::$instance = $instance;

        $facts = BuilderFacts::collect(FakeElementorPlugin::class);

        $this->assertArrayHasKey('elementor', $facts, 'Elementor is loaded, so its key is present');
        $this->assertNull($facts['elementor']['atomic_editor']);
    }

    public function test_probing_for_elementor_does_not_invoke_any_autoloader(): void
    {
        $this->stubTemplate('twentytwentyfour');
        $seen = [];
        $spy  = static function (string $class) use (&$seen): void {
            $seen[] = $class;
        };
        spl_autoload_register($spy);

        try {
            BuilderFacts::collect();
        } finally {
            spl_autoload_unregister($spy);
        }

        $this->assertSame([], $seen, 'asking whether Elementor is loaded must not run autoloaders');
    }

    // ---- read-only ---------------------------------------------------------

    public function test_collecting_writes_nothing(): void
    {
        $writes = [];
        foreach ([
            'update_option', 'add_option', 'delete_option',
            'update_site_option', 'add_site_option', 'delete_site_option',
            'set_transient', 'set_site_transient', 'delete_transient', 'delete_site_transient',
            'wp_schedule_event', 'wp_schedule_single_event',
        ] as $function) {
            Functions\when($function)->alias(static function () use (&$writes, $function): bool {
                $writes[] = $function;

                return true;
            });
        }

        $this->stubTemplate('bricks');
        $class = $this->loadedElementor(new FakeElementorExperiments(true));

        BuilderFacts::collect($class);
        BuilderFacts::collect(self::ABSENT_CLASS);

        $this->assertSame([], $writes);
    }

    // ---- wire shape and reserved keys -------------------------------------

    public function test_reserved_keys_are_pinned(): void
    {
        $this->assertSame(
            [
                'elementor' => ['gate', 'service_role_excluded', 'pro_active'],
                'bricks'    => ['abilities_setting'],
            ],
            BuilderFacts::RESERVED_KEYS
        );
    }

    public function test_no_reserved_key_is_emitted_at_schema_version_one(): void
    {
        // The richest block the collector can produce: a builder theme in use,
        // Elementor loaded and answering.
        $this->stubTemplate('bricks');
        $class = $this->loadedElementor(new FakeElementorExperiments(true));

        $facts = BuilderFacts::collect($class);

        $this->assertSame(1, BuilderFacts::SCHEMA_VERSION);
        $this->assertSame(1, $facts['v']);
        foreach (BuilderFacts::RESERVED_KEYS as $parent => $keys) {
            if (!isset($facts[$parent])) {
                continue;
            }
            foreach ($keys as $key) {
                $this->assertArrayNotHasKey($key, $facts[$parent], "reserved key {$parent}.{$key} must not be emitted at v1");
            }
        }
        $this->assertArrayNotHasKey('bricks', $facts, 'no bricks block at v1, even with the bricks theme active');
    }

    public function test_the_key_whitelist_is_pinned(): void
    {
        $this->stubTemplate('bricks');
        $class = $this->loadedElementor(new FakeElementorExperiments(true));

        $facts = BuilderFacts::collect($class);

        $this->assertSame(['v', 'theme_template', 'elementor'], array_keys($facts));
        $this->assertSame(['atomic_editor'], array_keys($facts['elementor']));
    }

    public function test_the_json_the_control_plane_receives_is_pinned(): void
    {
        $this->stubTemplate('bricks');

        $active = BuilderFacts::collect($this->loadedElementor(new FakeElementorExperiments(true)));
        $unsure = BuilderFacts::collect($this->loadedElementor(new FakeElementorExperiments('maybe')));
        $none   = BuilderFacts::collect(self::ABSENT_CLASS);

        $this->assertSame('{"v":1,"theme_template":"bricks","elementor":{"atomic_editor":true}}', json_encode($active));
        // null is sent, not dropped: "Elementor is loaded and gave no answer"
        // is a different statement from "Elementor is not loaded".
        $this->assertSame('{"v":1,"theme_template":"bricks","elementor":{"atomic_editor":null}}', json_encode($unsure));
        $this->assertSame('{"v":1,"theme_template":"bricks"}', json_encode($none));
    }

    /**
     * The other tests reach Elementor through a stand-in class. This one pins
     * the real names the collector uses in production: the class, its static
     * instance, its experiments member, the method, and the experiment. It runs
     * in its own process so declaring a class under Elementor's name cannot make
     * Elementor look loaded to any other test.
     */
    #[RunInSeparateProcess]
    public function test_the_production_names_match_elementors_class_shape(): void
    {
        require_once __DIR__ . '/fixtures/elementor-plugin-stub.php';

        $this->stubTemplate('bricks');
        $manager = new FakeElementorExperiments(true);
        $plugin  = new \Elementor\Plugin();

        $plugin->experiments         = $manager;
        \Elementor\Plugin::$instance = $plugin;

        $facts = BuilderFacts::collect();

        $this->assertSame(['atomic_editor' => true], $facts['elementor'] ?? null);
        $this->assertSame(['e_atomic_elements'], $manager->asked);
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Stub the parent-theme read.
     *
     * @param mixed $name What get_template() returns.
     */
    private function stubTemplate(mixed $name): void
    {
        Functions\when('get_template')->justReturn($name);
    }

    /**
     * Make the stand-in Elementor plugin class "loaded", with the given
     * experiments manager, and return the class name to hand the collector.
     *
     * @param mixed $manager What the plugin instance exposes as `experiments`.
     */
    private function loadedElementor(mixed $manager): string
    {
        $plugin              = new FakeElementorPlugin();
        $plugin->experiments = $manager;

        FakeElementorPlugin::$instance = $plugin;

        return FakeElementorPlugin::class;
    }
}
