<?php

declare(strict_types=1);
namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\ElementorRuntime;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class ElementorThemeRuntimeTest extends TestCase
{
    private function runtime(string $module = ThemeModuleStandIn::class, string $version = 'WPMGR_TEST_PRO_VERSION'): ElementorRuntime
    {
        if (!defined('WPMGR_TEST_PRO_VERSION')) {
            define('WPMGR_TEST_PRO_VERSION', '3.35.1');
            define('WPMGR_TEST_BAD_PRO_VERSION', 'bad version');
        }
        return new ElementorRuntime(proConstant: $version, themeBuilderClass: $module);
    }

    protected function tear_down(): void
    {
        ThemeModuleStandIn::$module = null;
        parent::tear_down();
    }

    public function test_versions_inactive_missing_and_throwing_module(): void
    {
        $api = $this->runtime();
        $this->assertSame('3.35.1', $api->proVersion());
        $this->assertNull($this->runtime(version: 'WPMGR_TEST_BAD_PRO_VERSION')->proVersion());
        $this->assertFalse($this->runtime(version: 'WPMGR_TEST_NO_PRO_VERSION')->proActive());
        $this->assertFalse($api->themeBuilderReady());
        foreach (['AbsentThemeModule', \stdClass::class, ThrowingThemeModule::class] as $class) {
            $this->assertFalse($this->runtime($class)->themeBuilderReady());
        }
    }

    public function test_each_required_method_missing_and_each_read_call_throwing(): void
    {
        $api = $this->runtime();
        $methods = ['get_conditions_manager', 'get_document', 'get_document_conditions', 'get_condition', 'save_conditions'];
        $missingMethods = [
            new class {
                public function get_document(int $id): object { return new \stdClass(); }
            },
            new class {
                public function get_conditions_manager(): object { return new ThemeConditionsStandIn(); }
            },
            new ThemeBuilderStandIn(new class {
                public function get_condition(string $name): ?object { return null; }
                public function save_conditions(int $id, array $maps): void {}
            }),
            new ThemeBuilderStandIn(new class {
                public function get_document_conditions(object $document): array { return []; }
                public function save_conditions(int $id, array $maps): void {}
            }),
            new ThemeBuilderStandIn(new class {
                public function get_document_conditions(object $document): array { return []; }
                public function get_condition(string $name): ?object { return null; }
            }),
        ];
        foreach ($missingMethods as $i => $module) {
            ThemeModuleStandIn::$module = $module;
            $this->assertFalse($api->themeBuilderReady(), $methods[$i]);
        }
        foreach (array_diff($methods, ['save_conditions']) as $throws) {
            $manager = new ThemeConditionsStandIn();
            $manager->throws = $throws;
            ThemeModuleStandIn::$module = new ThemeBuilderStandIn($manager, $throws);
            $this->assertSame(!in_array($throws, ['get_conditions_manager', 'get_condition'], true), $api->themeBuilderReady(), $throws);
            if ($throws === 'get_condition') {
                $this->assertFalse($api->themeConditionRegistered('general'));
            } else {
                $this->assertNull($api->templateRules(42));
            }
        }
    }

    public function test_readiness_requires_general_and_never_probes_documents(): void
    {
        $manager = new ThemeConditionsStandIn();
        $module = new ThemeBuilderStandIn($manager);
        ThemeModuleStandIn::$module = $module;
        $api = $this->runtime();
        $manager->generalRegistered = false;
        $this->assertFalse($api->themeBuilderReady());
        $manager->generalRegistered = true;
        $this->assertTrue($api->themeBuilderReady());
        $this->assertSame([], $module->documentIds);
        $this->assertSame(0, $manager->documentReads);
    }

    public function test_rules_are_complete_validated_and_read_fresh(): void
    {
        $manager = new ThemeConditionsStandIn();
        ThemeModuleStandIn::$module = new ThemeBuilderStandIn($manager);
        $api = $this->runtime();
        $this->assertTrue($api->themeBuilderReady());
        $this->assertTrue($api->themeConditionRegistered('general'));
        $this->assertFalse($api->themeConditionRegistered('missing'));
        $manager->maps = [['type' => 'include', 'name' => 'general', 'sub_name' => '', 'sub_id' => ''], ['type' => 'exclude', 'name' => 'singular', 'sub_name' => 'page', 'sub_id' => 42]];
        $this->assertSame(['include/general', 'exclude/singular/page/42'], $api->templateRules(42));
        foreach ([false, [1 => ['type' => 'include', 'name' => 'general']], [['name' => 'general']], [['type' => 'include', 'name' => 'general/more']], [['type' => 'include', 'name' => 'singular', 'sub_name' => 'page', 'sub_id' => '0']], [['type' => 'include', 'name' => 'general', 'sub_name' => null]]] as $maps) {
            $manager->maps = $maps;
            $this->assertNull($api->templateRules(42));
        }
        $manager->maps = array_fill(0, 21, ['type' => 'include', 'name' => 'general']);
        $this->assertCount(21, $api->templateRules(42));
        $manager->maps = [];
        $this->assertSame([], $api->templateRules(42));
        $manager->throws = 'get_document_conditions';
        $this->assertNull($api->templateRules(42));
    }
}

final class ThemeModuleStandIn
{
    public static ?object $module = null;
    public static function instance(): ?object { return self::$module; }
}
final class ThrowingThemeModule
{
    public static function instance(): never { throw new \RuntimeException('unavailable'); }
}
final class ThemeBuilderStandIn
{
    public array $documentIds = [];
    public function __construct(public object $manager, public string $throws = '') {}
    public function get_conditions_manager(): object
    {
        if ($this->throws === __FUNCTION__) { throw new \RuntimeException('unavailable'); }
        return $this->manager;
    }
    public function get_document(int $id): object
    {
        $this->documentIds[] = $id;
        if ($this->throws === __FUNCTION__) { throw new \RuntimeException('unavailable'); }
        return (object) ['id' => $id];
    }
}
final class ThemeConditionsStandIn
{
    public bool $generalRegistered = true;
    public int $documentReads = 0;
    public mixed $maps = [];
    public string $throws = '';
    public function get_document_conditions(object $document): mixed
    {
        ++$this->documentReads;
        if ($this->throws === __FUNCTION__) { throw new \RuntimeException('unavailable'); }
        return $this->maps;
    }
    public function get_condition(string $name): ?object
    {
        if ($this->throws === __FUNCTION__) { throw new \RuntimeException('unavailable'); }
        return $name === 'general' && $this->generalRegistered ? new \stdClass() : null;
    }
    public function save_conditions(int $id, array $maps): never
    {
        throw new \LogicException('A read must not save conditions');
    }
}
