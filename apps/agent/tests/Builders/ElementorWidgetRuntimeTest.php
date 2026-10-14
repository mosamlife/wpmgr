<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\ElementorRuntime;
use WPMgr\Agent\Abilities\Builders\ElementorFacts;
use WPMgr\Agent\Abilities\OwnAbilities;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/** @covers \WPMgr\Agent\Abilities\Builders\ElementorRuntime */
final class ElementorWidgetRuntimeTest extends TestCase
{
    protected function tear_down(): void
    {
        WidgetPluginStandIn::$instance = null;
        parent::tear_down();
    }

    public function test_runtime_reads_widget_manager_and_classifies_pro_and_atomic_fresh(): void
    {
        $manager = new WidgetManagerStandIn();
        $manager->widgets = [
            'heading' => new DiscoveryWidgetStandIn(),
            'pro-heading' => new \ElementorPro\WpmgrTests\DiscoveryWidgetStandIn(),
            'e-heading' => new AtomicDiscoveryWidgetStandIn(),
        ];
        WidgetPluginStandIn::$instance = (object) ['widgets_manager' => $manager];
        $api = new ElementorRuntime(WidgetPluginStandIn::class);
        $rows = $api->widgetCatalogue();
        $this->assertSame([false, true, false], array_column($rows, 'pro'));
        $this->assertSame([false, false, true], array_column($rows, 'atomic'));
        $this->assertSame('heading', $rows[0]['name']);
        $this->assertSame('Heading', $rows[0]['title']);
        $this->assertSame(['basic'], $rows[0]['categories']);
        $this->assertSame(['title' => ['type' => 'text']], $api->widgetControls('heading'));
        $manager->widgets['heading']->controls = ['other' => ['type' => 'select']];
        $this->assertSame(['other' => ['type' => 'select']], $api->widgetControls('heading'));
        $manager->widgets = [];
        $this->assertSame([], $api->widgetCatalogue());
        $this->assertSame([[], ['heading'], ['heading'], []], $manager->calls);
    }

    public function test_absent_throwing_or_malformed_manager_and_controls_answer_null(): void
    {
        $api = new ElementorRuntime(WidgetPluginStandIn::class);
        $this->assertNull($api->widgetCatalogue());
        $this->assertNull($api->widgetControls('heading'));
        WidgetPluginStandIn::$instance = (object) ['widgets_manager' => new class {
            public function get_widget_types(): never
            {
                throw new \RuntimeException('unavailable');
            }
        }];
        $this->assertNull($api->widgetCatalogue());
        $this->assertNull($api->widgetControls('heading'));
        $manager = new WidgetManagerStandIn();
        WidgetPluginStandIn::$instance = (object) ['widgets_manager' => $manager];
        $manager->widgets = ['heading' => new DiscoveryWidgetStandIn()];
        $manager->widgets['heading']->controls = false;
        $this->assertNull($api->widgetControls('heading'));
        $this->assertNull($api->widgetControls('missing'));
        $manager->widgets = ['broken' => new \stdClass()];
        $this->assertNull($api->widgetCatalogue());
    }

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_own_read_dispatch_uses_the_loaded_elementor_runtime(): void
    {
        require_once __DIR__ . '/../fixtures/elementor-plugin-stub.php';
        define('ELEMENTOR_VERSION', ElementorFacts::CLASSIC_MIN);
        $manager = new WidgetManagerStandIn();
        $manager->widgets = ['heading' => new DiscoveryWidgetStandIn()];
        \Elementor\Plugin::$instance = (object) ['widgets_manager' => $manager];
        $catalogue = OwnAbilities::run('wpmgr/elementor-widgets', (object) []);
        $this->assertSame(['heading'], array_column($catalogue['output']['widgets'], 'name'));
        $schema = OwnAbilities::run('wpmgr/elementor-widget-schema', (object) ['widget' => 'heading', 'controls' => ['title']]);
        $this->assertFalse($catalogue['output']['widgets'][0]['usable']);
        $this->assertSame('widget_not_usable', $schema['refusal']['code'] ?? null);
        $this->assertSame([[], ['heading'], ['heading'], [], ['heading'], ['heading']], $manager->calls);
    }
}

final class WidgetPluginStandIn
{
    public static ?object $instance = null;
}

final class WidgetManagerStandIn
{
    public array $widgets = [];
    public array $calls = [];

    public function get_widget_types(?string $name = null): mixed
    {
        $this->calls[] = $name === null ? [] : [$name];
        return $name === null ? $this->widgets : ($this->widgets[$name] ?? null);
    }
}

class DiscoveryWidgetStandIn
{
    public mixed $controls = ['title' => ['type' => 'text']];

    public function get_name(): string
    {
        return 'heading';
    }

    public function get_title(): string
    {
        return 'Heading';
    }

    public function get_categories(): array
    {
        return ['basic'];
    }

    public function get_controls(): mixed
    {
        return $this->controls;
    }
}

final class AtomicDiscoveryWidgetStandIn extends DiscoveryWidgetStandIn
{
    public static function get_props_schema(): array
    {
        return [];
    }
}

namespace ElementorPro\WpmgrTests;

final class DiscoveryWidgetStandIn extends \WPMgr\Agent\Tests\Builders\DiscoveryWidgetStandIn
{
}
