<?php
/**
 * ElementorFacts and ElementorRuntime: what the agent reads about Elementor on
 * a site, when it refuses to build an Elementor page there, and that reading
 * Elementor never throws.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\ElementorFacts;
use WPMgr\Agent\Abilities\Builders\ElementorRuntime;
use WPMgr\Agent\Abilities\ServicePrincipal;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorFacts
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorRuntime
 */
final class ElementorFactsTest extends TestCase
{
    /** The service user's id in these tests. */
    private const PRINCIPAL = 7;

    /** The keys the readiness facts block reserves for Elementor. */
    private const RESERVED_ELEMENTOR_KEYS = ['gate', 'service_role_excluded', 'pro_active'];

    /** A version constant only these tests define. */
    private const VERSION_STANDIN = 'WPMGR_TEST_ELEMENTOR_VERSION_STANDIN';

    /** A Pro version constant only these tests define. */
    private const PRO_STANDIN = 'WPMGR_TEST_ELEMENTOR_PRO_VERSION_STANDIN';

    /** @var array<string, mixed> */
    private array $options = [];

    /** @var array<int, object> */
    private array $users = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->options = [];
        $this->users   = [self::PRINCIPAL => (object) ['roles' => [ServicePrincipal::ROLE]]];
        Functions\when('get_option')->alias(
            fn ($name, $default = false) => array_key_exists($name, $this->options) ? $this->options[$name] : $default
        );
        Functions\when('get_userdata')->alias(fn ($id) => $this->users[(int) $id] ?? false);
        ElementorPluginStandIn::$instance = null;
        ElementorUtilsStandIn::$sanitise  = null;
    }

    protected function tear_down(): void
    {
        ElementorPluginStandIn::$instance = null;
        ElementorUtilsStandIn::$sanitise  = null;
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_absent_elementor_is_elementor_missing(): void
    {
        $api          = new FakeElementorApi();
        $api->loaded  = false;
        $api->version = null;
        $facts        = $this->collect($api);

        $this->assertFalse($facts['active']);
        $this->assertNull($facts['version']);
        $this->assertFalse($facts['classic_in_range']);
        $this->assertNull($facts['containers']);
        $this->assertNull($facts['atomic']);
        $this->assertSame(0, $facts['active_kit_id']);
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::createRefusal($facts, 'page', 'atomic'));
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::status($facts)->refusal());

        // The version constant is defined before Elementor finishes loading;
        // on its own it is not Elementor.
        $api->version = '3.35.9';
        $facts        = $this->collect($api);
        $this->assertTrue($facts['classic_in_range']);
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::status($facts)->refusal());

        // The real runtime, in this process, where Elementor is not loaded.
        $this->assertFalse(class_exists('Elementor\\Plugin', false), 'precondition: no Elementor in this process');
        $facts = ElementorFacts::collect(new ElementorRuntime(), self::PRINCIPAL);
        $this->assertFalse($facts['active']);
        $this->assertNull($facts['version']);
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::status($facts)->refusal());

        // A facts array without a true `active` refuses the same way.
        $good = $this->collect(new FakeElementorApi());
        $this->assertNull(ElementorFacts::createRefusal($good, 'page', 'classic'));
        foreach ([[], ['active' => 1] + $good, ['active' => 'yes'] + $good] as $facts) {
            $this->assertSame(self::refusal('elementor_missing'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        }
    }

    public function test_version_range_edges(): void
    {
        $api = new FakeElementorApi();
        foreach (['3.20.0', '3.35.9', '4.3.4', '4.3.99'] as $version) {
            $api->version = $version;
            $facts        = $this->collect($api);
            $this->assertTrue($facts['classic_in_range'], $version);
            $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'), $version);
            $this->assertNull(ElementorFacts::status($facts)->refusal(), $version);
        }

        foreach (['3.19.9', '4.4.0', '3.20.0-beta1', '4.4.0-beta1', '2.9.14', '10.0.0'] as $version) {
            $api->version = $version;
            $facts        = $this->collect($api);
            $this->assertFalse($facts['classic_in_range'], $version);
            $this->assertSame(self::refusal('version_unverified'), ElementorFacts::createRefusal($facts, 'page', 'classic'), $version);
            $this->assertSame(self::refusal('version_unverified'), ElementorFacts::status($facts)->refusal(), $version);
            // Catalogue data may only narrow the range, never widen it.
            $this->assertSame(
                self::refusal('version_unverified'),
                ElementorFacts::status($facts)->narrowed('1.0.0', '99.0.0')->refusal(),
                $version
            );
        }

        // An unknown version is never in range.
        $api->version = null;
        $facts        = $this->collect($api);
        $this->assertFalse($facts['classic_in_range']);
        $this->assertSame(self::refusal('version_unverified'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertSame(self::refusal('version_unverified'), ElementorFacts::status($facts)->refusal());

        // The range itself, as the edges above exercise it.
        $this->assertSame('3.20.0', ElementorFacts::CLASSIC_MIN);
        $this->assertSame('4.3.99', ElementorFacts::CLASSIC_MAX);
    }

    public function test_excluded_role_refuses(): void
    {
        $api = new FakeElementorApi();

        // Not excluded: no option, other roles only, entries that are not
        // role names, a stored value that is not a list.
        $notExcluding = [
            'absent'      => null,
            'empty'       => [],
            'other roles' => ['editor', 'author'],
            'not strings' => [1, null, [ServicePrincipal::ROLE]],
            'not a list'  => ServicePrincipal::ROLE,
        ];
        foreach ($notExcluding as $label => $stored) {
            unset($this->options[ElementorFacts::OPTION_EXCLUDED_ROLES]);
            if ($stored !== null) {
                $this->options[ElementorFacts::OPTION_EXCLUDED_ROLES] = $stored;
            }
            $facts = $this->collect($api);
            $this->assertFalse($facts['role_excluded'], $label);
            $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'), $label);
            $this->assertNull(ElementorFacts::status($facts)->refusal(), $label);
        }

        // The service user's role is excluded.
        $this->options[ElementorFacts::OPTION_EXCLUDED_ROLES] = ['editor', ServicePrincipal::ROLE];
        $facts = $this->collect($api);
        $this->assertTrue($facts['role_excluded']);
        $this->assertSame(self::refusal('role_excluded'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertSame(self::refusal('role_excluded'), ElementorFacts::createRefusal($facts, 'post', 'classic'));
        $this->assertSame(self::refusal('role_excluded'), ElementorFacts::status($facts)->refusal());

        // A service user that cannot be read: the role it is created with stands in.
        $this->users = [];
        $this->assertTrue($this->collect($api)['role_excluded']);
        $this->assertTrue(ElementorFacts::collect($api, 0)['role_excluded']);

        // The roles the service user holds are what count.
        $this->users[self::PRINCIPAL] = (object) ['roles' => ['editor']];
        $this->options[ElementorFacts::OPTION_EXCLUDED_ROLES] = [ServicePrincipal::ROLE, 'author'];
        $this->assertFalse($this->collect($api)['role_excluded']);
        $this->options[ElementorFacts::OPTION_EXCLUDED_ROLES] = ['editor'];
        $this->assertTrue($this->collect($api)['role_excluded']);

        // A facts array that does not say "not excluded" refuses.
        $this->options = [];
        $this->users   = [self::PRINCIPAL => (object) ['roles' => [ServicePrincipal::ROLE]]];
        $facts         = $this->collect($api);
        $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'));
        foreach ([null, 0, 'false'] as $unclear) {
            $facts['role_excluded'] = $unclear;
            $this->assertSame(self::refusal('role_excluded'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
            $this->assertSame(self::refusal('role_excluded'), ElementorFacts::status($facts)->refusal());
        }
    }

    public function test_unsupported_post_type_refuses(): void
    {
        $api = new FakeElementorApi();

        // No option: Elementor's default, pages and posts.
        $facts = $this->collect($api);
        $this->assertSame(['page', 'post'], $facts['post_types']);
        $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertNull(ElementorFacts::createRefusal($facts, 'post', 'classic'));
        foreach (['product', 'Page', '', 'page '] as $postType) {
            $this->assertSame(self::refusal('post_type_not_supported'), ElementorFacts::createRefusal($facts, $postType, 'classic'), $postType);
        }

        // Switched on for pages only.
        $this->options[ElementorFacts::OPTION_POST_TYPES] = ['page'];
        $facts = $this->collect($api);
        $this->assertSame(['page'], $facts['post_types']);
        $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'));
        $this->assertSame(self::refusal('post_type_not_supported'), ElementorFacts::createRefusal($facts, 'post', 'classic'));
        // Post type support is per request, not part of the site status.
        $this->assertNull(ElementorFacts::status($facts)->refusal());

        // Only post type keys count, each once, in order.
        $this->options[ElementorFacts::OPTION_POST_TYPES] = ['post', 7, null, 'Bad Type', 'post', ['page'], 'product', str_repeat('a', 21)];
        $facts = $this->collect($api);
        $this->assertSame(['post', 'product'], $facts['post_types']);
        $this->assertSame(self::refusal('post_type_not_supported'), ElementorFacts::createRefusal($facts, 'page', 'classic'));

        // A stored value that is not a list, or an empty list, switches on nothing.
        foreach (['page', [], false] as $stored) {
            $this->options[ElementorFacts::OPTION_POST_TYPES] = $stored;
            $facts = $this->collect($api);
            $this->assertSame([], $facts['post_types']);
            $this->assertSame(self::refusal('post_type_not_supported'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
        }

        // A facts array without a list of post types refuses.
        unset($this->options[ElementorFacts::OPTION_POST_TYPES]);
        $facts = $this->collect($api);
        unset($facts['post_types']);
        $this->assertSame(self::refusal('post_type_not_supported'), ElementorFacts::createRefusal($facts, 'page', 'classic'));
    }

    public function test_containers_and_atomic_from_experiments(): void
    {
        $api = new FakeElementorApi();
        foreach ([[true, false], [false, true], [true, true], [false, false], [null, null]] as [$containers, $atomic]) {
            $api->experiments = ['container' => $containers, 'e_atomic_elements' => $atomic];
            $facts            = $this->collect($api);
            $this->assertSame($containers, $facts['containers']);
            $this->assertSame($atomic, $facts['atomic']);
            // Neither decides whether a classic page may be built.
            $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'));
        }

        // Exactly those two experiments are asked, by Elementor's names.
        $api->calls = [];
        $this->collect($api);
        $this->assertSame(['container', 'e_atomic_elements'], $api->experimentsAsked());
        $this->assertSame('container', ElementorFacts::EXPERIMENT_CONTAINER);
        $this->assertSame('e_atomic_elements', ElementorFacts::EXPERIMENT_ATOMIC);

        // An experiment Elementor does not know answers false, and that is reported.
        $api->experiments = [];
        $facts            = $this->collect($api);
        $this->assertFalse($facts['containers']);
        $this->assertFalse($facts['atomic']);

        // The runtime hands the name to Elementor's experiments manager and
        // reports only a boolean answer.
        $plugin              = new ElementorPluginStandIn();
        $plugin->experiments = new ElementorManagerStandIn(
            static fn (string $method, array $args) => ['container' => true, 'e_atomic_elements' => false][$args[0]] ?? 'yes'
        );
        ElementorPluginStandIn::$instance = $plugin;
        $runtime = $this->standInRuntime();
        $this->assertTrue($runtime->experimentActive('container'));
        $this->assertFalse($runtime->experimentActive('e_atomic_elements'));
        $this->assertNull($runtime->experimentActive('something_else'));
        $this->assertSame(
            [['is_feature_active', ['container']], ['is_feature_active', ['e_atomic_elements']], ['is_feature_active', ['something_else']]],
            $plugin->experiments->calls
        );
    }

    public function test_active_kit_id(): void
    {
        $api              = new FakeElementorApi();
        $api->activeKitId = 42;
        $this->assertSame(42, $this->collect($api)['active_kit_id']);
        $api->activeKitId = 0;
        $this->assertSame(0, $this->collect($api)['active_kit_id']);
        $api->activeKitId = 42;
        $api->loaded      = false;
        $this->assertSame(0, $this->collect($api)['active_kit_id']);

        // The runtime asks Elementor's kits manager and keeps only a positive id.
        $cases = [
            [42, 42],
            ['42', 42],
            ['999999999999999999', 999999999999999999],
            [0, 0],
            ['0', 0],
            [-5, 0],
            ['-5', 0],
            ['042', 0],
            [' 42', 0],
            ['42 ', 0],
            ['4x', 0],
            ['', 0],
            ['9999999999999999999', 0],
            [4.0, 0],
            [false, 0],
            [null, 0],
            [[42], 0],
        ];
        $plugin = new ElementorPluginStandIn();
        ElementorPluginStandIn::$instance = $plugin;
        $runtime = $this->standInRuntime();
        foreach ($cases as [$stored, $expected]) {
            $plugin->kits_manager = new ElementorManagerStandIn(static fn () => $stored);
            $this->assertSame($expected, $runtime->activeKitId(), var_export($stored, true));
            $this->assertSame([['get_active_id', []]], $plugin->kits_manager->calls);
        }
    }

    public function test_atomic_format_refused_in_a1(): void
    {
        $api              = new FakeElementorApi();
        $api->version     = '4.3.4';
        $api->experiments = ['container' => true, 'e_atomic_elements' => true];
        $facts            = $this->collect($api);
        $this->assertTrue($facts['atomic']);
        $this->assertSame(self::refusal('atomic_unavailable'), ElementorFacts::createRefusal($facts, 'page', 'atomic'));
        $this->assertNull(ElementorFacts::createRefusal($facts, 'page', 'classic'));

        // Refused ahead of every site fact but Elementor itself.
        $this->options[ElementorFacts::OPTION_EXCLUDED_ROLES] = [ServicePrincipal::ROLE];
        $this->options[ElementorFacts::OPTION_POST_TYPES]     = [];
        $api->version = '4.4.0';
        $facts        = $this->collect($api);
        $this->assertSame(self::refusal('atomic_unavailable'), ElementorFacts::createRefusal($facts, 'page', 'atomic'));
        $this->assertSame(self::refusal('version_unverified'), ElementorFacts::createRefusal($facts, 'page', 'classic'));

        // Any other format is bad input; site_default is resolved by the caller.
        foreach (['site_default', 'Classic', 'ATOMIC', '', 'classic ', 'v4'] as $format) {
            $this->assertSame(
                ['code' => 'bad_input', 'detail' => 'elementor_format must be classic or atomic'],
                ElementorFacts::createRefusal($facts, 'page', $format),
                $format
            );
        }

        // The Atomic range is declared for later and admits nothing here.
        $this->assertSame('4.3.0', ElementorFacts::ATOMIC_MIN);
        $this->assertSame('4.3.99', ElementorFacts::ATOMIC_MAX);
    }

    public function test_runtime_never_throws_without_elementor(): void
    {
        $this->assertFalse(class_exists('Elementor\\Plugin', false), 'precondition: no Elementor in this process');
        $this->assertFalse(class_exists('Elementor\\Utils', false), 'precondition: no Elementor in this process');
        $this->assertFalse(defined('ELEMENTOR_VERSION'), 'precondition: no Elementor in this process');

        $this->assertRuntimeAnswersNothing(new ElementorRuntime(), false);

        // An Elementor whose every manager method throws, an Error as much
        // as an Exception, and whose sanitiser throws.
        $throwers = [
            static fn () => throw new \RuntimeException('manager failed'),
            static fn () => throw new \TypeError('manager failed'),
        ];
        foreach ($throwers as $throw) {
            $plugin = new ElementorPluginStandIn();
            foreach (['documents', 'elements_manager', 'widgets_manager', 'experiments', 'kits_manager'] as $name) {
                $plugin->{$name} = new ElementorManagerStandIn($throw);
            }
            ElementorPluginStandIn::$instance = $plugin;
            ElementorUtilsStandIn::$sanitise  = $throw;
            $this->assertRuntimeAnswersNothing($this->standInRuntime(), true);
        }

        // An instance whose managers are not objects.
        $plugin = new ElementorPluginStandIn();
        ElementorPluginStandIn::$instance = $plugin;
        ElementorUtilsStandIn::$sanitise  = static fn () => 'not an array';
        $this->assertRuntimeAnswersNothing($this->standInRuntime(), true);

        // A plugin class whose instance is not set.
        ElementorPluginStandIn::$instance = null;
        $this->assertRuntimeAnswersNothing($this->standInRuntime(), false);

        // A version constant that does not hold a plain version is not reported.
        foreach (['WPMGR_TEST_ELEMENTOR_VERSION_MARKUP' => '3.35.9<b>', 'WPMGR_TEST_ELEMENTOR_VERSION_INT' => 335] as $name => $value) {
            if (!defined($name)) {
                define($name, $value);
            }
            $this->assertNull((new ElementorRuntime(ElementorPluginStandIn::class, ElementorUtilsStandIn::class, $name))->version(), $name);
        }
    }

    public function test_runtime_reads_elementor_through_its_managers(): void
    {
        $object = new \stdClass();
        $plugin = new ElementorPluginStandIn();
        $plugin->documents        = new ElementorManagerStandIn(static fn (string $m, array $args) => $args[0] === 41 ? $object : false);
        $plugin->elements_manager = new ElementorManagerStandIn(
            static fn (string $m, array $args) => match ($m) {
                'get_element_types'       => $args[0] === 'container' ? $object : null,
                'create_element_instance' => ($args[0]['elType'] ?? null) !== 'unknown' ? $object : null,
                default                   => null,
            }
        );
        $plugin->widgets_manager = new ElementorManagerStandIn(static fn (string $m, array $args) => $args[0] === 'heading' ? $object : null);
        ElementorPluginStandIn::$instance = $plugin;
        ElementorUtilsStandIn::$sanitise  = static fn ($data) => ['sanitised' => $data];
        $runtime = $this->standInRuntime();

        $this->assertTrue($runtime->loaded());
        $this->assertSame('3.35.9', $runtime->version());

        // A document is always read fresh, never from Elementor's cache.
        $this->assertSame($object, $runtime->document(41));
        $this->assertNull($runtime->document(42));
        $this->assertNull($runtime->document(0));
        $this->assertNull($runtime->document(-1));
        $this->assertSame([['get', [41, false]], ['get', [42, false]]], $plugin->documents->calls);

        $this->assertTrue($runtime->elementTypeExists('container'));
        $this->assertFalse($runtime->elementTypeExists('section'));
        $this->assertFalse($runtime->elementTypeExists(''));
        $this->assertTrue($runtime->widgetTypeExists('heading'));
        $this->assertFalse($runtime->widgetTypeExists('html'));
        $this->assertFalse($runtime->widgetTypeExists(''));
        $this->assertSame([['get_widget_types', ['heading']], ['get_widget_types', ['html']]], $plugin->widgets_manager->calls);

        // A node without a type, or a widget without a widget type, never reaches Elementor.
        $plugin->elements_manager->calls = [];
        $this->assertSame($object, $runtime->createElementInstance(['elType' => 'container', 'id' => 'a1b2c3d']));
        $this->assertSame($object, $runtime->createElementInstance(['elType' => 'widget', 'widgetType' => 'heading']));
        $this->assertNull($runtime->createElementInstance(['elType' => 'unknown']));
        foreach ([[], ['elType' => ''], ['elType' => 5], ['elType' => 'widget'], ['elType' => 'widget', 'widgetType' => ''], ['elType' => 'widget', 'widgetType' => ['heading']]] as $node) {
            $this->assertNull($runtime->createElementInstance($node), (string) json_encode($node));
        }
        $this->assertSame(
            [
                ['create_element_instance', [['elType' => 'container', 'id' => 'a1b2c3d']]],
                ['create_element_instance', [['elType' => 'widget', 'widgetType' => 'heading']]],
                ['create_element_instance', [['elType' => 'unknown']]],
            ],
            $plugin->elements_manager->calls
        );

        $this->assertSame(['sanitised' => ['a' => '<b>x</b>']], $runtime->ksesPostDeep(['a' => '<b>x</b>']));
    }

    public function test_builder_facts_block_emits_only_reserved_keys(): void
    {
        $this->assertFalse(defined('ELEMENTOR_PRO_VERSION'), 'precondition: no Elementor Pro in this process');
        if (!defined(self::PRO_STANDIN)) {
            define(self::PRO_STANDIN, '3.35.0');
        }

        $api    = new FakeElementorApi();
        $blocks = [];

        $blocks['default'] = [ElementorFacts::builderFactsBlock($this->collect($api)), false, false];

        $this->options[ElementorFacts::OPTION_EXCLUDED_ROLES] = [ServicePrincipal::ROLE];
        $blocks['excluded'] = [ElementorFacts::builderFactsBlock($this->collect($api)), true, false];

        $blocks['excluded, pro'] = [ElementorFacts::builderFactsBlock(ElementorFacts::collect($api, self::PRINCIPAL, self::PRO_STANDIN)), true, true];

        $this->options = [];
        $blocks['pro'] = [ElementorFacts::builderFactsBlock(ElementorFacts::collect($api, self::PRINCIPAL, self::PRO_STANDIN)), false, true];

        $api->loaded = false;
        $blocks['no elementor'] = [ElementorFacts::builderFactsBlock($this->collect($api)), false, false];

        // A role exclusion not known to be false is reported as excluded.
        $blocks['unknown role'] = [ElementorFacts::builderFactsBlock(['pro_active' => 'yes']), true, false];

        foreach ($blocks as $label => [$block, $excluded, $pro]) {
            $this->assertSame(['service_role_excluded', 'pro_active'], array_keys($block), $label);
            $this->assertSame([], array_values(array_diff(array_keys($block), self::RESERVED_ELEMENTOR_KEYS)), $label);
            $this->assertArrayNotHasKey('gate', $block, $label);
            $this->assertSame(['service_role_excluded' => $excluded, 'pro_active' => $pro], $block, $label);
        }
        $this->assertSame(['service_role_excluded', 'pro_active'], ElementorFacts::BLOCK_KEYS);
    }

    /**
     * @param FakeElementorApi $api Elementor double.
     * @return array<string, mixed>
     */
    private function collect(FakeElementorApi $api): array
    {
        return ElementorFacts::collect($api, self::PRINCIPAL);
    }

    /**
     * A runtime over the stand-in plugin and utility classes, with a version
     * constant only these tests define.
     *
     * @return ElementorRuntime
     */
    private function standInRuntime(): ElementorRuntime
    {
        if (!defined(self::VERSION_STANDIN)) {
            define(self::VERSION_STANDIN, '3.35.9');
        }

        return new ElementorRuntime(ElementorPluginStandIn::class, ElementorUtilsStandIn::class, self::VERSION_STANDIN);
    }

    /**
     * Every method answers "cannot tell" and nothing throws.
     *
     * @param ElementorRuntime $runtime Runtime under test.
     * @param bool             $loaded  What loaded() answers.
     * @return void
     */
    private function assertRuntimeAnswersNothing(ElementorRuntime $runtime, bool $loaded): void
    {
        $this->assertSame($loaded, $runtime->loaded());
        $this->assertNull($runtime->experimentActive('container'));
        $this->assertNull($runtime->experimentActive('e_atomic_elements'));
        $this->assertSame(0, $runtime->activeKitId());
        $this->assertNull($runtime->document(41));
        $this->assertNull($runtime->createElementInstance(['elType' => 'widget', 'widgetType' => 'heading']));
        $this->assertNull($runtime->createElementInstance(['elType' => 'container']));
        $this->assertFalse($runtime->elementTypeExists('section'));
        $this->assertFalse($runtime->widgetTypeExists('heading'));
        $this->assertNull($runtime->ksesPostDeep(['a' => '<b>x</b>']));

        $facts = ElementorFacts::collect($runtime, self::PRINCIPAL);
        $this->assertSame($loaded, $facts['active']);
        $this->assertNull($facts['containers']);
        $this->assertNull($facts['atomic']);
        $this->assertSame(0, $facts['active_kit_id']);
    }

    /**
     * @param string $detail Detail token.
     * @return array{code: string, detail: string}
     */
    private static function refusal(string $detail): array
    {
        return ['code' => 'builder_not_available', 'detail' => $detail];
    }
}

/**
 * Stands in for Elementor's main plugin class: a static instance carrying
 * public managers.
 */
final class ElementorPluginStandIn
{
    /** @var object|null */
    public static ?object $instance = null;

    /** @var object|null */
    public $documents = null;

    /** @var object|null */
    public $elements_manager = null;

    /** @var object|null */
    public $widgets_manager = null;

    /** @var object|null */
    public $experiments = null;

    /** @var object|null */
    public $kits_manager = null;
}

/**
 * Stands in for any of Elementor's managers: every method answers through
 * one closure and is recorded.
 */
final class ElementorManagerStandIn
{
    /**
     * Every call: [method, arguments].
     *
     * @var list<array{0: string, 1: array<mixed>}>
     */
    public array $calls = [];

    /** @var \Closure */
    private \Closure $answer;

    /**
     * @param \Closure $answer Called with (method, arguments).
     */
    public function __construct(\Closure $answer)
    {
        $this->answer = $answer;
    }

    /**
     * @param mixed ...$args Arguments.
     * @return mixed
     */
    public function get(...$args)
    {
        return $this->answer('get', $args);
    }

    /**
     * @param mixed ...$args Arguments.
     * @return mixed
     */
    public function create_element_instance(...$args)
    {
        return $this->answer('create_element_instance', $args);
    }

    /**
     * @param mixed ...$args Arguments.
     * @return mixed
     */
    public function get_element_types(...$args)
    {
        return $this->answer('get_element_types', $args);
    }

    /**
     * @param mixed ...$args Arguments.
     * @return mixed
     */
    public function get_widget_types(...$args)
    {
        return $this->answer('get_widget_types', $args);
    }

    /**
     * @param mixed ...$args Arguments.
     * @return mixed
     */
    public function is_feature_active(...$args)
    {
        return $this->answer('is_feature_active', $args);
    }

    /**
     * @param mixed ...$args Arguments.
     * @return mixed
     */
    public function get_active_id(...$args)
    {
        return $this->answer('get_active_id', $args);
    }

    /**
     * @param string       $method Method called.
     * @param array<mixed> $args   Arguments.
     * @return mixed
     */
    private function answer(string $method, array $args)
    {
        $this->calls[] = [$method, $args];

        return ($this->answer)($method, $args);
    }
}

/**
 * Stands in for Elementor's utility class.
 */
final class ElementorUtilsStandIn
{
    /** @var \Closure|null */
    public static ?\Closure $sanitise = null;

    /**
     * @param mixed $data Data.
     * @return mixed
     */
    public static function kses_post_deep($data)
    {
        return self::$sanitise === null ? $data : (self::$sanitise)($data);
    }
}
