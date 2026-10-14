<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\ElementorRuntime;
use WPMgr\Agent\Autoloader;
use WPMgr\Agent\Tests\FakeElementorPlugin;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/** @covers \WPMgr\Agent\Abilities\Builders\ElementorRuntime */
final class NativeCoreWidgetRuntimeTest extends TestCase
{
    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_core_uses_registered_class_source_not_widget_name_or_parent_class(): void
    {
        $root = dirname(__DIR__) . '/fixtures/native-elementor/';
        define('ELEMENTOR_PATH', $root); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedConstantFound -- isolated process supplies Elementor's own path constant
        require $root . 'includes/widgets/core-widget.php';
        require $root . 'modules/outside-widget.php';
        $core = new \Elementor\Widget_Wpmgr_Core_Probe();
        $outside = new \Elementor\Widget_Wpmgr_Outside_Probe();
        $manager = new class ($core, $outside) {
            public function __construct(private object $core, private object $outside)
            {
            }
            public function get_widget_types(string $name): ?object
            {
                return match ($name) {
                    'unusual-widget-name' => $this->core,
                    'heading' => $this->outside,
                    default => null,
                };
            }
        };
        FakeElementorPlugin::$instance = (object) ['widgets_manager' => $manager];
        $runtime = new ElementorRuntime(FakeElementorPlugin::class);
        $this->assertTrue($runtime->widgetIsCore('unusual-widget-name'));
        $this->assertFalse($runtime->widgetIsCore('heading'));
        $this->assertFalse($runtime->widgetIsCore('missing'));
        $this->assertFalse((new ElementorRuntime())->widgetIsCore('heading'));
        $this->assertSame(dirname(__DIR__, 2) . '/includes/abilities/builders/class-native-widget-policy.php', Autoloader::resolve('WPMgr\\Agent\\Abilities\\Builders\\NativeWidgetPolicy'));
    }
}
