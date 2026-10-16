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
    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_pro_ownership_requires_active_plugin_and_anchored_resolved_source(): void
    {
        $root = dirname(__DIR__) . '/fixtures/native-pro';
        require $root . '/widget.php';
        require $root . '-other/widget.php';
        $manager = new class {
            public function get_widget_types(string $name): ?object
            {
                return match ($name) {
                    'theme-site-logo' => new ProOwnedWidgetProbe(),
                    'nav-menu' => new ProOutsideWidgetProbe(),
                    'internal' => new \stdClass(),
                    'throws' => throw new \RuntimeException('unavailable'),
                    default => null,
                };
            }
        };
        FakeElementorPlugin::$instance = (object) ['widgets_manager' => $manager];
        $runtime = new ElementorRuntime(FakeElementorPlugin::class);
        $this->assertFalse($runtime->widgetIsPro('theme-site-logo'));
        define('ELEMENTOR_PRO_VERSION', '3.35.1');
        $this->assertFalse($runtime->widgetIsPro('theme-site-logo'));
        define('ELEMENTOR_PRO_PATH', $root . '/../native-pro/');
        $this->assertTrue($runtime->widgetIsPro('theme-site-logo'));
        $this->assertFalse($runtime->widgetIsCore('theme-site-logo'));
        foreach (['nav-menu', 'internal', 'throws', 'missing'] as $name) {
            $this->assertFalse($runtime->widgetIsPro($name), $name);
        }
        $inactive = new ElementorRuntime(FakeElementorPlugin::class, proConstant: 'WPMGR_TEST_ABSENT_PRO_VERSION');
        $this->assertFalse($inactive->widgetIsPro('theme-site-logo'));
        FakeElementorPlugin::$instance = null;
        $this->assertFalse($runtime->widgetIsPro('theme-site-logo'));
    }

    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_pro_ownership_refuses_unresolvable_plugin_path(): void
    {
        define('ELEMENTOR_PRO_VERSION', '3.35.1');
        define('ELEMENTOR_PRO_PATH', __DIR__ . '/missing-pro-directory');
        FakeElementorPlugin::$instance = (object) ['widgets_manager' => new class {
            public function get_widget_types(string $name): object { return $this; }
        }];
        $this->assertFalse((new ElementorRuntime(FakeElementorPlugin::class))->widgetIsPro('theme-site-logo'));
    }

}
