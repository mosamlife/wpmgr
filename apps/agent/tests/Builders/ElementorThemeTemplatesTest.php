<?php

declare(strict_types=1);
namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DraftEligibility;
use WPMgr\Agent\Abilities\Builders\ElementorThemeTemplates;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Autoloader;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class ElementorThemeTemplatesTest extends TestCase
{
    private FakeElementorApi $api;
    private array $posts = [];
    private array $menus = [];
    private array $markers = [];
    private mixed $previousWpdb;
    private const NAME = 'wpmgr/elementor-theme-templates';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->api = new FakeElementorApi();
        $this->previousWpdb = $GLOBALS['wpdb'] ?? null;
        $matching = function () {
            $posts = array_filter($this->posts, static fn ($p) => $p->post_type === 'elementor_library' && !in_array($p->post_status, ['trash', 'auto-draft', 'inherit'], true) && in_array($p->template_type, ['header', 'footer'], true));
            usort($posts, static fn ($a, $b) => strcmp($b->post_modified_gmt, $a->post_modified_gmt) ?: $b->ID <=> $a->ID);
            return array_column($posts, 'ID');
        };
        $GLOBALS['wpdb'] = new class ($matching) {
            public string $posts = 'wp_posts';
            public string $postmeta = 'wp_postmeta';
            public string $last_error = '';
            public bool $fail = false;
            public function __construct(private \Closure $matching) {}
            public function prepare(string $sql, ...$args): string
            {
                \PHPUnit\Framework\Assert::assertSame(['wp_posts', 'elementor_library', 'trash', 'auto-draft', 'inherit', 'wp_postmeta', '_elementor_template_type', 'header', 'footer'], array_slice($args, 0, 9));
                \PHPUnit\Framework\Assert::assertStringContainsString('BINARY m.meta_value IN (%s, %s)', $sql);
                return $sql;
            }
            public function get_var(string $sql): ?string
            {
                \PHPUnit\Framework\Assert::assertStringStartsWith('SELECT COUNT(*)', $sql);
                return $this->fail ? null : (string) count(($this->matching)());
            }
            public function get_col(string $sql): array
            {
                \PHPUnit\Framework\Assert::assertStringContainsString('ORDER BY p.post_modified_gmt DESC, p.ID DESC LIMIT %d', $sql);
                return array_slice(($this->matching)(), 0, 25);
            }
        };
        Functions\when('get_post')->alias(fn ($id) => $this->posts[$id] ?? null);
        Functions\when('get_post_meta')->alias(fn ($id, $key, $single = false) => $key === DraftEligibility::MARKER_KEY ? ($this->markers[$id] ?? []) : $this->posts[$id]->template_type);
        Functions\when('wp_get_nav_menus')->alias(fn () => $this->menus);
    }

    protected function tear_down(): void
    {
        $GLOBALS['wpdb'] = $this->previousWpdb;
        Monkey\tearDown();
        parent::tear_down();
    }

    private function post(int $id, string $type = 'header', string $status = 'draft'): void
    {
        $this->posts[$id] = (object) ['ID' => $id, 'post_type' => 'elementor_library', 'post_status' => $status, 'template_type' => $type, 'post_title' => 'Template ' . $id, 'post_modified_gmt' => sprintf('2026-10-%02d 12:00:00', $id % 28 + 1)];
    }

    private function read(): array
    {
        $result = (new ElementorThemeTemplates($this->api))->read((object) []);
        $this->assertArrayHasKey('output', $result, (string) wp_json_encode($result));
        return $result['output'];
    }

    public function test_inactive_and_unavailable_pro_still_list_templates(): void
    {
        $this->post(1);
        $out = $this->read();
        $this->assertSame(['active' => false, 'version' => null, 'theme_builder' => false, 'problem' => 'pro_inactive'], $out['pro']);
        $this->assertSame('pro_unavailable', $out['templates'][0]['shows_on_problem']);
        $this->assertNull($out['templates'][0]['shows_on']);
        $this->assertSame([], $out['places']);
        $this->api->pro = true;
        $this->assertSame('theme_builder_unavailable', $this->read()['pro']['problem']);
        $this->assertSame([], $this->api->callsTo('regeneratePostCss'));
    }

    public function test_ready_rules_provenance_places_and_menu_tokens(): void
    {
        $this->api->pro = true;
        $this->api->proVersion = '3.35.1';
        $this->api->themeBuilderReady = true;
        $this->api->themeConditions = ['general', 'singular', 'page'];
        $this->api->templateRules = [1 => ['include/general', 'exclude/singular/page/42']];
        $this->post(1);
        $this->markers[1] = ['12345678-1234-1234-1234-123456789abc'];
        $this->menus = [(object) ['term_id' => 7, 'slug' => 'invalid slug', 'name' => '<b>Menu</b>', 'count' => 3]];
        $out = $this->read();
        $this->assertTrue($out['pro']['theme_builder']);
        $this->assertTrue($out['templates'][0]['made_by_wpmgr']);
        $this->assertSame($this->api->templateRules[1], $out['templates'][0]['shows_on']);
        $this->assertNull($out['templates'][0]['shows_on_problem']);
        $this->assertSame(['general', 'singular/page'], array_column($out['places'], 'rule'));
        $this->assertSame([false, true], array_column($out['places'], 'takes_id'));
        $this->assertSame('2026-10-02T12:00:00Z', $out['templates'][0]['modified_gmt']);
        $this->assertSame(['id' => 7, 'slug' => null, 'name' => '<b>Menu</b>', 'items' => 3], $out['menus'][0]);
        foreach ([[], ['bad'], ['12345678-1234-1234-1234-123456789abc', '12345678-1234-1234-1234-123456789abc']] as $markers) {
            $this->markers[1] = $markers;
            $this->assertFalse($this->read()['templates'][0]['made_by_wpmgr']);
        }
        $this->assertTrue(rest_validate_value_from_schema($out, ElementorThemeTemplates::outputSchema()));
    }

    public function test_unreadable_and_too_many_rules_are_not_shortened(): void
    {
        $this->post(1);
        $this->api->pro = true;
        $this->api->themeBuilderReady = true;
        foreach ([null, ['include/general', 'bad'], [1 => 'include/general']] as $rules) {
            $this->api->templateRules[1] = $rules;
            $row = $this->read()['templates'][0];
            $this->assertNull($row['shows_on']);
            $this->assertSame('unreadable', $row['shows_on_problem']);
        }
        $this->api->templateRules[1] = array_fill(0, 21, 'include/general');
        $this->assertSame('too_many', $this->read()['templates'][0]['shows_on_problem']);
        $this->api->templateRules[1] = array_fill(0, 20, 'include/general');
        $this->assertCount(20, $this->read()['templates'][0]['shows_on']);
        $this->api->templateRules[1] = [];
        $this->assertSame([], $this->read()['templates'][0]['shows_on']);
    }

    public function test_filter_order_count_and_caps(): void
    {
        foreach (range(1, 30) as $id) {
            $this->post($id, $id % 2 ? 'header' : 'footer');
            $this->menus[] = (object) ['term_id' => $id, 'slug' => 'menu-' . $id, 'name' => 'Menu', 'count' => 0];
        }
        $this->post(31, 'section');
        foreach (['trash', 'auto-draft', 'inherit'] as $i => $status) {
            $this->post(32 + $i, 'header', $status);
        }
        $this->post(35);
        $this->posts[35]->post_type = 'page';
        $out = $this->read();
        $this->assertSame(30, $out['templates_total']);
        $this->assertSame(30, $out['menus_total']);
        $this->assertCount(25, $out['templates']);
        $this->assertCount(25, $out['menus']);
        $this->assertSame(27, $out['templates'][0]['id']);
        $this->posts[27]->post_title = str_repeat('é"\\', 12000);
        $out = $this->read();
        $this->assertSame([], $out['templates']);
        $this->assertSame(30, $out['templates_total']);
        $this->menus[0]->name = str_repeat('x', 31000);
        $out = $this->read();
        $this->assertSame([], $out['menus']);
        $this->assertLessThanOrEqual(30 * 1024, strlen(json_encode($out, JSON_THROW_ON_ERROR)));
    }

    public function test_order_uses_gmt_even_when_local_dates_differ(): void
    {
        $this->post(1);
        $this->post(2);
        $this->posts[1]->post_modified = '2026-10-10 12:00:00';
        $this->posts[2]->post_modified = '2026-10-01 12:00:00';
        $this->assertSame([2, 1], array_column($this->read()['templates'], 'id'));
    }

    public function test_database_errors_fail_closed_and_empty_site_is_valid(): void
    {
        $out = $this->read();
        $this->assertSame([], $out['templates']);
        $this->assertSame(0, $out['templates_total']);
        $GLOBALS['wpdb']->fail = true;
        $this->assertSame('read_failed', (new ElementorThemeTemplates($this->api))->read((object) [])['refusal']['code']);
    }

    public function test_both_lists_share_the_encoded_answer_budget(): void
    {
        $this->post(1);
        $this->posts[1]->post_title = str_repeat('x', 20000);
        $this->menus = [(object) ['term_id' => 7, 'slug' => 'menu', 'name' => str_repeat('y', 15000), 'count' => 1]];
        $out = $this->read();
        $this->assertCount(1, $out['templates']);
        $this->assertSame($this->posts[1]->post_title, $out['templates'][0]['title']);
        $this->assertSame([], $out['menus']);
        $this->assertSame(1, $out['menus_total']);
        $this->assertLessThanOrEqual(ElementorThemeTemplates::MAX_BYTES, strlen(json_encode($out, JSON_THROW_ON_ERROR)));
    }

    public function test_custom_status_is_other_and_does_not_hide_templates(): void
    {
        $this->post(1, 'header', 'custom-status');
        $this->post(2, 'footer', 'publish');
        $out = $this->read();
        $this->assertSame([2, 1], array_column($out['templates'], 'id'));
        $this->assertSame(['publish', 'other'], array_column($out['templates'], 'status'));
        $this->assertSame(2, $out['templates_total']);
        $this->assertTrue(rest_validate_value_from_schema($out, ElementorThemeTemplates::outputSchema()));
    }

    public function test_invalid_utf8_title_uses_wordpress_encoding_without_hiding_templates(): void
    {
        // Model WordPress's repair of invalid text for JSON serialization.
        Functions\when('wp_json_encode')->alias(static fn ($data) => json_encode($data, JSON_INVALID_UTF8_SUBSTITUTE));
        $this->post(1);
        $this->post(2, 'footer', 'publish');
        $this->posts[2]->post_title = "Title \xFF";
        $out = $this->read();
        $this->assertSame([2, 1], array_column($out['templates'], 'id'));
        $this->assertSame($this->posts[2]->post_title, $out['templates'][0]['title']);
        $encoded = wp_json_encode($out);
        $this->assertIsString($encoded);
        $this->assertCount(2, json_decode($encoded, true, 512, JSON_THROW_ON_ERROR)['templates']);
        $this->assertLessThanOrEqual(ElementorThemeTemplates::MAX_BYTES, strlen($encoded));
    }


    /**
     * @runInSeparateProcess
     * @preserveGlobalState disabled
     */
    public function test_production_dispatch_reads_the_guarded_runtime(): void
    {
        require_once __DIR__ . '/../fixtures/elementor-plugin-stub.php';
        require_once __DIR__ . '/ElementorThemeRuntimeTest.php';
        define('ELEMENTOR_VERSION', '3.35.9');
        define('ELEMENTOR_PRO_VERSION', '3.35.1');
        \Elementor\Plugin::$instance = new \stdClass();
        class_alias(ThemeModuleStandIn::class, 'ElementorPro\\Modules\\ThemeBuilder\\Module');
        $manager = new ThemeConditionsStandIn();
        $manager->maps = [['type' => 'include', 'name' => 'general']];
        ThemeModuleStandIn::$module = new ThemeBuilderStandIn($manager);
        $this->post(1);
        $result = OwnAbilities::run(self::NAME, (object) []);
        $this->assertTrue($result['output']['pro']['theme_builder']);
        $this->assertSame(['include/general'], $result['output']['templates'][0]['shows_on']);
        $this->assertSame(['general'], array_column($result['output']['places'], 'rule'));
        $registered = [];
        Functions\when('wp_register_ability')->alias(static function ($name, $definition) use (&$registered) { $registered[$name] = $definition; });
        OwnAbilities::registerAbilities();
        foreach ([null, (object) []] as $input) {
            $this->assertSame($result['output'], ($registered[self::NAME]['execute_callback'])($input));
        }

    }

    public function test_registration_validation_dispatch_inventory_and_autoload(): void
    {
        $registered = [];
        Functions\when('wp_register_ability')->alias(static function ($name, $definition) use (&$registered) { $registered[$name] = $definition; });
        OwnAbilities::registerAbilities();
        $this->assertContains(self::NAME, OwnAbilities::names());
        $this->assertContains(self::NAME, OwnAbilities::readNames());
        $this->assertSame('read', OwnAbilities::abilityClass(self::NAME));
        $this->assertSame(ElementorThemeTemplates::outputSchema(), $registered[self::NAME]['output_schema']);
        $this->assertTrue($registered[self::NAME]['meta']['annotations']['readonly']);
        foreach ([(object) ['extra' => true]] as $input) {
            $this->assertInstanceOf(\WP_Error::class, ($registered[self::NAME]['execute_callback'])($input));
        }
        $this->assertNull(OwnAbilities::validate(self::NAME, (object) []));
        $this->assertNotNull(OwnAbilities::validate(self::NAME, (object) ['extra' => true]));
        $this->assertSame('bad_input', (new ElementorThemeTemplates($this->api))->read((object) ['extra' => true])['refusal']['code']);
        $this->assertSame('builder_not_available', OwnAbilities::run(self::NAME, (object) [])['refusal']['code']);
        $this->assertNotNull(Autoloader::resolve(ElementorThemeTemplates::class));
        $inventory = OwnAbilities::run(OwnAbilities::NAME_INVENTORY, (object) [])['output'];
        $row = array_values(array_filter($inventory['abilities'], static fn ($row) => $row['name'] === self::NAME))[0];
        $this->assertSame((new \ReflectionMethod(OwnAbilities::class, 'schemaHashOfSchema'))->invoke(null, OwnAbilities::inputSchema(self::NAME)), $row['schema_struct_sha256']);
        $this->api->loaded = false;
        $this->assertSame('elementor_missing', (new ElementorThemeTemplates($this->api))->read((object) [])['refusal']['detail']);
    }

    public function test_answer_fixture_is_produced_by_the_reader(): void
    {
        $this->api->pro = true;
        $this->api->proVersion = '3.35.1';
        $this->api->themeBuilderReady = true;
        $this->api->themeConditions = ['general', 'singular', 'front_page', 'page', 'post', 'not_found404', 'archive'];
        $this->post(1);
        $this->post(2, 'footer', 'publish');
        $this->posts[1]->post_modified_gmt = '2026-10-15 12:00:00';
        $this->posts[1]->post_title = 'Draft header';
        $this->posts[2]->post_title = 'Site footer';
        $this->markers[1] = ['12345678-1234-1234-1234-123456789abc'];
        $this->api->templateRules = [1 => ['include/general', 'exclude/singular/page/42'], 2 => null];
        $this->menus = [(object) ['term_id' => 7, 'slug' => 'main-menu', 'name' => 'Main menu', 'count' => 3]];
        $want = json_encode($this->read(), JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR) . "\n";
        $path = __DIR__ . '/../fixtures/ability-run/elementor-theme-templates-answer.json';
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents($path, $want));
        }
        $this->assertFileExists($path);
        $this->assertSame($want, file_get_contents($path));
    }
}
