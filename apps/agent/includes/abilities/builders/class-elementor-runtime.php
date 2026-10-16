<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * ElementorApi over the Elementor loaded on this request.
 *
 * Elementor is reached only through its main plugin instance and the public
 * managers on it, and only when its classes are already in memory:
 * class_exists() is asked without autoloading, so on a site without Elementor
 * nothing is loaded and every method answers "cannot tell". The classes
 * autoloaded are Elementor's fonts class and its base document class, which
 * Elementor loads on first use: each is asked for only once the main plugin
 * instance is found. Every call is guarded, and anything Elementor throws
 * becomes that answer (null, false or 0); no method throws. The writes are
 * deletePostCss(), which asks Elementor to delete one post's generated CSS,
 * and regeneratePostCss(), which asks it to make that CSS again.
 */
final class ElementorRuntime implements ElementorApi
{
    /** Elementor Pro's conditions meta key; to be confirmed on a Pro site. */
    public const KEY_CONDITIONS = '_elementor_conditions';

    public const OPTION_CONDITIONS = 'elementor_pro_theme_builder_conditions';

    /** Elementor's main plugin class. */
    public const PLUGIN_CLASS = 'Elementor\\Plugin';

    /** Elementor's utility class, which holds its document sanitiser. */
    public const UTILS_CLASS = 'Elementor\\Utils';

    /** The constant Elementor defines its version in. */
    public const VERSION_CONSTANT = 'ELEMENTOR_VERSION';

    /** Elementor's post CSS file class, which its files manager builds. */
    public const POST_CSS_CLASS = 'Elementor\\Core\\Files\\CSS\\Post';

    /** Elementor's fonts class, which holds its font registry. */
    public const FONTS_CLASS = 'Elementor\\Fonts';

    /** Elementor's base document class, which names the meta key of a post's cached element output. */
    public const DOCUMENT_CLASS = 'Elementor\\Core\\Base\\Document';

    /** The constant on that class holding the cache's meta key. */
    public const CACHE_KEY_CONSTANT = 'CACHE_META_KEY';

    /** Most families a font registry may hold to be read at all. */
    public const MAX_FONT_FAMILIES = 20000;

    /** A plain version token: a digit first, at most 32 bytes. */
    private const RE_VERSION = '/^[0-9][0-9A-Za-z._+-]{0,31}$/D';

    /** A positive decimal id that fits an int. */
    private const RE_ID = '/^[1-9][0-9]{0,17}$/D';

    /** A cache's meta key as Elementor spells one: an _elementor_ key with "cache" in its name. */
    private const RE_CACHE_KEY = '/^_elementor_[a-z0-9_]{0,40}cache[a-z0-9_]{0,40}$/D';

    /** A breakpoint name as Elementor keys one: lowercase letters, digits and _. */
    private const RE_BREAKPOINT = '/^[a-z][a-z0-9_]{0,31}$/D';

    /** Most breakpoint names reported. */
    private const MAX_BREAKPOINTS = 16;

    /**
     * @param string $pluginClass     Tests only: stands in for Elementor's main plugin class. Production passes none.
     * @param string $utilsClass      Tests only: stands in for Elementor's utility class. Production passes none.
     * @param string $versionConstant Tests only: stands in for Elementor's version constant. Production passes none.
     * @param string $fontsClass      Tests only: stands in for Elementor's fonts class. Production passes none.
     * @param string $postCssClass    Tests only: stands in for Elementor's post CSS class. Production passes none.
     * @param string $proConstant     Tests only: stands in for Elementor Pro's version constant. Production passes none.
     * @param string $documentClass   Tests only: stands in for Elementor's base document class. Production passes none.
     * @param string $themeBuilderClass Tests only: stands in for the Theme Builder module.
     */
    public function __construct(
        private readonly string $pluginClass = self::PLUGIN_CLASS,
        private readonly string $utilsClass = self::UTILS_CLASS,
        private readonly string $versionConstant = self::VERSION_CONSTANT,
        private readonly string $fontsClass = self::FONTS_CLASS,
        private readonly string $postCssClass = self::POST_CSS_CLASS,
        private readonly string $proConstant = ElementorFacts::PRO_VERSION_CONSTANT,
        private readonly string $documentClass = self::DOCUMENT_CLASS,
        private readonly string $themeBuilderClass = 'ElementorPro\\Modules\\ThemeBuilder\\Module'
    ) {
    }

    /**
     * {@inheritDoc}
     */
    public function loaded(): bool
    {
        try {
            return $this->plugin() !== null;
        } catch (\Throwable $e) {
            return false;
        }
    }

    /**
     * {@inheritDoc}
     */
    public function version(): ?string
    {
        try {
            if (!defined($this->versionConstant)) {
                return null;
            }
            $version = constant($this->versionConstant);
        } catch (\Throwable $e) {
            return null;
        }

        return is_string($version) && preg_match(self::RE_VERSION, $version) === 1 ? $version : null;
    }

    /**
     * {@inheritDoc}
     */
    public function experimentActive(string $name): ?bool
    {
        try {
            $active = $this->call('experiments', 'is_feature_active', [$name]);
        } catch (\Throwable $e) {
            return null;
        }

        return is_bool($active) ? $active : null;
    }

    /**
     * {@inheritDoc}
     */
    public function activeKitId(): int
    {
        try {
            $id = $this->call('kits_manager', 'get_active_id', []);
        } catch (\Throwable $e) {
            return 0;
        }

        return self::positiveId($id);
    }

    /**
     * {@inheritDoc}
     *
     * Elementor's breakpoints manager keys its active breakpoints by name; a
     * key that is not a plain breakpoint name is not reported.
     */
    public function activeBreakpoints(): ?array
    {
        try {
            $active = $this->call('breakpoints', 'get_active_breakpoints', []);
        } catch (\Throwable $e) {
            return null;
        }
        if (!is_array($active)) {
            return null;
        }
        $names = [];
        foreach (array_keys($active) as $name) {
            if (!is_string($name) || preg_match(self::RE_BREAKPOINT, $name) !== 1) {
                continue;
            }
            $names[] = $name;
            if (count($names) >= self::MAX_BREAKPOINTS) {
                break;
            }
        }

        return $names;
    }

    /**
     * {@inheritDoc}
     *
     * The registry is the map of family to font type that Elementor's fonts
     * class answers; a family key that is empty is not reported.
     */
    public function fontFamilies(): ?array
    {
        try {
            if ($this->plugin() === null) {
                return null;
            }
            $class = $this->fontsClass;
            $read  = class_exists($class) ? [$class, 'get_fonts'] : null;
            if ($read === null || !is_callable($read)) {
                return null;
            }
            $fonts = $read();
        } catch (\Throwable $e) {
            return null;
        }
        if (!is_array($fonts) || count($fonts) > self::MAX_FONT_FAMILIES) {
            return null;
        }
        $families = [];
        foreach (array_keys($fonts) as $family) {
            $family = (string) $family;
            if ($family !== '') {
                $families[] = $family;
            }
        }

        return $families;
    }

    /**
     * {@inheritDoc}
     */
    public function document(int $postId): ?object
    {
        if ($postId <= 0) {
            return null;
        }
        try {
            // false: a fresh document, never Elementor's per-request cached one.
            $document = $this->call('documents', 'get', [$postId, false]);
        } catch (\Throwable $e) {
            return null;
        }

        return is_object($document) ? $document : null;
    }

    /**
     * {@inheritDoc}
     */
    public function createElementInstance(array $node): ?object
    {
        // A node without a type, or a widget without a widget type, is never
        // handed to Elementor.
        $type = $node['elType'] ?? null;
        if (!is_string($type) || $type === '') {
            return null;
        }
        if ($type === 'widget') {
            $widget = $node['widgetType'] ?? null;
            if (!is_string($widget) || $widget === '') {
                return null;
            }
        }
        try {
            $element = $this->call('elements_manager', 'create_element_instance', [$node]);
        } catch (\Throwable $e) {
            return null;
        }

        return is_object($element) ? $element : null;
    }

    /**
     * {@inheritDoc}
     */
    public function elementTypeExists(string $type): bool
    {
        if ($type === '') {
            return false;
        }
        try {
            return is_object($this->call('elements_manager', 'get_element_types', [$type]));
        } catch (\Throwable $e) {
            return false;
        }
    }

    /**
     * {@inheritDoc}
     */
    public function widgetTypeExists(string $type): bool
    {
        if ($type === '') {
            return false;
        }
        try {
            return is_object($this->call('widgets_manager', 'get_widget_types', [$type]));
        } catch (\Throwable $e) {
            return false;
        }
    }

    /**
     * {@inheritDoc}
     */
    public function widgetCatalogue(): ?array
    {
        try {
            $widgets = $this->call('widgets_manager', 'get_widget_types', []);
            if (!is_array($widgets)) {
                return null;
            }
            $rows = [];
            foreach ($widgets as $widget) {
                if (!is_object($widget)) {
                    return null;
                }
                $name       = $widget->get_name();
                $title      = $widget->get_title();
                $categories = $widget->get_categories();
                if (!is_string($name) || $name === '' || !is_string($title) || !is_array($categories)) {
                    return null;
                }
                foreach ($categories as $category) {
                    if (!is_string($category)) {
                        return null;
                    }
                }
                $rows[] = [
                    'name'       => $name,
                    'title'      => $title,
                    'categories' => array_values($categories),
                    'pro'        => str_starts_with(get_class($widget), 'ElementorPro\\'),
                    'atomic'     => method_exists($widget, 'get_props_schema'),
                ];
            }

            return $rows;
        } catch (\Throwable $e) {
            return null;
        }
    }

    /** {@inheritDoc} */
    public function widgetIsCore(string $type): bool
    {
        try {
            $widget = $this->call('widgets_manager', 'get_widget_types', [$type]);
            if (!is_object($widget) || !defined('ELEMENTOR_PATH')) {
                return false;
            }
            $class = new \ReflectionClass($widget);
            $file = $class->getFileName();
            $root = realpath(ELEMENTOR_PATH . 'includes/widgets');
            $source = is_string($file) ? realpath($file) : false;
            return is_string($root) && is_string($source) && str_starts_with($source, $root . DIRECTORY_SEPARATOR);
        } catch (\Throwable $e) {
            return false;
        }
    }

    /** {@inheritDoc} */
    public function widgetIsPro(string $type): bool
    {
        try {
            if (!$this->proActive() || !defined('ELEMENTOR_PRO_PATH') || !is_string(ELEMENTOR_PRO_PATH) || ELEMENTOR_PRO_PATH === '') {
                return false;
            }
            $widget = $this->call('widgets_manager', 'get_widget_types', [$type]);
            if (!is_object($widget)) {
                return false;
            }
            $file = (new \ReflectionClass($widget))->getFileName();
            $root = @realpath(ELEMENTOR_PRO_PATH);
            $source = is_string($file) ? @realpath($file) : false;
            return is_string($root) && is_string($source) && str_starts_with($source, $root . DIRECTORY_SEPARATOR);
        } catch (\Throwable $e) {
            return false;
        }
    }

    /**
     * {@inheritDoc}
     */
    public function widgetControls(string $name): ?array
    {
        if ($name === '') {
            return null;
        }
        try {
            $widget = $this->call('widgets_manager', 'get_widget_types', [$name]);
            if (!is_object($widget)) {
                return null;
            }
            $controls = $widget->get_controls();
            if (!is_array($controls)) {
                return null;
            }
            foreach ($controls as $id => $control) {
                if (!is_string($id) || !is_array($control)) {
                    return null;
                }
            }

            return $controls;
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * {@inheritDoc}
     *
     * Elementor's own Utils::kses_post_deep() is used where the running
     * Elementor has it. Where it does not, every string goes through
     * wp_kses_post() here, which is the sanitiser that Elementor's save
     * applies to a document's strings. A sanitiser Elementor does have is
     * never replaced by the fallback: if it fails or does not return an
     * array, the answer is null.
     */
    public function ksesPostDeep(array $data): ?array
    {
        try {
            if ($this->plugin() === null) {
                return null;
            }
            $class    = $this->utilsClass;
            $sanitise = class_exists($class, false) ? [$class, 'kses_post_deep'] : null;
            if ($sanitise !== null && is_callable($sanitise)) {
                $clean = $sanitise($data);

                return is_array($clean) ? $clean : null;
            }
            if (!function_exists('wp_kses_post')) {
                return null;
            }

            return self::ksesStrings($data, 1);
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * {@inheritDoc}
     *
     * The object is the one Elementor's own post CSS create() returns: its
     * files manager builds it from the class name and the post id. Its
     * delete() removes the file under Elementor's uploads folder and the
     * post's CSS meta.
     */
    public function deletePostCss(int $postId): bool
    {
        if ($postId <= 0) {
            return false;
        }
        try {
            $file = $this->call('files_manager', 'get', [self::POST_CSS_CLASS, [$postId]]);
            if (!is_object($file)) {
                return false;
            }
            $delete = [$file, 'delete'];
            if (!is_callable($delete)) {
                return false;
            }
            $delete();
        } catch (\Throwable $e) {
            return false;
        }

        return true;
    }

    /**
     * {@inheritDoc}
     *
     * A new post CSS object, from the class's own create(), so what an earlier
     * render of the post in this request left in Elementor's per-request
     * file objects is never written again. Its update() writes the
     * stylesheet file and the CSS meta; its meta then says whether the
     * stylesheet is a file, and its get_path() says where.
     */
    public function regeneratePostCss(int $postId): ?array
    {
        if ($postId <= 0) {
            return null;
        }
        try {
            if ($this->plugin() === null) {
                return null;
            }
            $create = [$this->postCssClass, 'create'];
            if (!is_callable($create)) {
                return null;
            }
            $file = $create($postId);
            if (!is_object($file)) {
                return null;
            }
            $update  = [$file, 'update'];
            $getMeta = [$file, 'get_meta'];
            $getPath = [$file, 'get_path'];
            if (!is_callable($update) || !is_callable($getMeta)) {
                return null;
            }
            $update();
            $meta = $getMeta();
            $path = is_callable($getPath) ? $getPath() : null;
        } catch (\Throwable $e) {
            return null;
        }
        $status = is_array($meta) ? ($meta['status'] ?? null) : null;
        if (!is_string($status) || $status === '') {
            return null;
        }

        return ['status' => $status, 'path' => is_string($path) ? $path : ''];
    }

    /**
     * {@inheritDoc}
     */
    public function proActive(): bool
    {
        return defined($this->proConstant);
    }

    /** {@inheritDoc} */
    public function proVersion(): ?string
    {
        try {
            $version = $this->proActive() ? constant($this->proConstant) : null;
            return is_string($version) && preg_match(self::RE_VERSION, $version) === 1 ? $version : null;
        } catch (\Throwable $e) {
            return null;
        }
    }

    /** {@inheritDoc} */
    public function themeBuilderReady(): bool
    {
        try {
            $parts = $this->themeBuilder();
            if ($parts === null) {
                return false;
            }
            return is_object($parts[1]->get_condition('general'));
        } catch (\Throwable $e) {
            return false;
        }
    }

    /** {@inheritDoc} */
    public function templateRules(int $postId): ?array
    {
        try {
            $parts = $postId > 0 ? $this->themeBuilder() : null;
            if ($parts === null) {
                return null;
            }
            [$module, $manager] = $parts;
            $document = $module->get_document($postId);
            if (!is_object($document)) {
                return null;
            }
            $maps = $manager->get_document_conditions($document);
            if (!is_array($maps) || !ArrayShape::isList($maps)) {
                return null;
            }
            $rules = [];
            foreach ($maps as $map) {
                if (!is_array($map) || !isset($map['type'], $map['name'])) {
                    return null;
                }
                $parts = [];
                foreach (['type', 'name', 'sub_name', 'sub_id'] as $key) {
                    $value = array_key_exists($key, $map) ? $map[$key] : '';
                    if (!is_string($value) && !($key === 'sub_id' && is_int($value))) {
                        return null;
                    }
                    $value = (string) $value;
                    // Each map member is one path part, never a path itself.
                    if (str_contains($value, '/')) {
                        return null;
                    }
                    $parts[] = $value;
                }
                while (end($parts) === '') {
                    array_pop($parts);
                }
                $rule = implode('/', $parts);
                if (preg_match(ElementorThemeTemplates::RE_RULE, $rule) !== 1) {
                    return null;
                }
                $rules[] = $rule;
            }
            return $rules;
        } catch (\Throwable $e) {
            return null;
        }
    }

    /** {@inheritDoc} */
    public function saveTemplateRules(int $postId, array $rules): bool
    {
        if (ThemeTemplateCreate::inputProblem($rules) !== null) {
            return false;
        }
        try {
            $parts = $postId > 0 ? $this->themeBuilder() : null;
            if ($parts === null) {
                return false;
            }
            $maps = [];
            foreach ($rules as $rule) {
                $segments = explode('/', $rule);
                $maps[] = ['type' => $segments[0], 'name' => $segments[1], 'sub_name' => $segments[2] ?? '', 'sub_id' => $segments[3] ?? ''];
            }
            return $parts[1]->save_conditions($postId, $maps) !== false;
        } catch (\Throwable $e) {
            return false;
        }
    }

    /** {@inheritDoc} */
    public function themeConditionRegistered(string $name): bool
    {
        try {
            $parts = $this->themeBuilder();
            return $parts !== null && is_object($parts[1]->get_condition($name));
        } catch (\Throwable $e) {
            return false;
        }
    }

    /**
     * The guarded Pro module and manager, without writing conditions.
     *
     * @return array{object, object}|null
     * @throws \Throwable Caught by every public caller.
     */
    private function themeBuilder(): ?array
    {
        $class = $this->themeBuilderClass;
        if (!$this->proActive() || !class_exists($class) || !method_exists($class, 'instance') || !is_callable([$class, 'instance'])) {
            return null;
        }
        $module = $class::instance();
        if (!is_object($module)) {
            return null;
        }
        foreach (['get_conditions_manager', 'get_document'] as $method) {
            if (!method_exists($module, $method) || !is_callable([$module, $method])) {
                return null;
            }
        }
        $manager = $module->get_conditions_manager();
        if (!is_object($manager)) {
            return null;
        }
        foreach (['get_document_conditions', 'get_condition', 'save_conditions'] as $method) {
            if (!method_exists($manager, $method) || !is_callable([$manager, $method])) {
                return null;
            }
        }
        return [$module, $manager];
    }

    /**
     * {@inheritDoc}
     *
     * Elementor's constant is read only once its main plugin instance is
     * found, and its value is taken only when it is an _elementor_ key that
     * names a cache, so a key that is a page row, or anything else, is never
     * the key a caller deletes.
     */
    public function elementCacheKey(): string
    {
        try {
            if ($this->plugin() !== null && class_exists($this->documentClass)) {
                $name = $this->documentClass . '::' . self::CACHE_KEY_CONSTANT;
                $key  = defined($name) ? constant($name) : null;
                if (is_string($key) && preg_match(self::RE_CACHE_KEY, $key) === 1) {
                    return $key;
                }
            }
        } catch (\Throwable $e) {
            unset($e);
        }

        return ElementorDocument::KEY_ELEMENT_CACHE;
    }

    /**
     * Every string of $data through wp_kses_post(); other values as they are.
     *
     * @param array<mixed> $data  Data.
     * @param int          $depth Nesting depth of $data.
     * @return array<mixed>|null Null when nested deeper than ElementorDocument::MAX_DEPTH.
     */
    private static function ksesStrings(array $data, int $depth): ?array
    {
        if ($depth > ElementorDocument::MAX_DEPTH) {
            return null;
        }
        $out = [];
        foreach ($data as $key => $value) {
            if (is_array($value)) {
                $value = self::ksesStrings($value, $depth + 1);
                if ($value === null) {
                    return null;
                }
            } elseif (is_string($value)) {
                $value = wp_kses_post($value);
            }
            $out[$key] = $value;
        }

        return $out;
    }

    /**
     * Elementor's main plugin instance, or null when its class is not in
     * memory or the instance is not set.
     *
     * @return object|null
     * @throws \ReflectionException Never in practice: the class is known to be loaded.
     */
    private function plugin(): ?object
    {
        $class = $this->pluginClass;
        if (!class_exists($class, false)) {
            return null;
        }
        $instance = (new \ReflectionClass($class))->getStaticPropertyValue('instance', null);

        return is_object($instance) ? $instance : null;
    }

    /**
     * Call a public method of one of the plugin instance's public managers.
     * Null when the instance, the manager or the method is not there.
     *
     * @param string       $manager Public property of the plugin instance.
     * @param string       $method  Public method of that manager.
     * @param array<mixed> $args    Arguments.
     * @return mixed
     * @throws \Throwable Whatever Elementor throws; each public method catches it.
     */
    private function call(string $manager, string $method, array $args): mixed
    {
        $plugin = $this->plugin();
        if ($plugin === null) {
            return null;
        }
        $object = get_object_vars($plugin)[$manager] ?? null;
        if (!is_object($object)) {
            return null;
        }
        $callback = [$object, $method];
        if (!is_callable($callback)) {
            return null;
        }

        return $callback(...$args);
    }

    /**
     * A positive int id from an int or a decimal string, else 0.
     *
     * @param mixed $value Stored value.
     * @return int
     */
    private static function positiveId(mixed $value): int
    {
        if (is_int($value)) {
            return $value > 0 ? $value : 0;
        }
        if (is_string($value) && preg_match(self::RE_ID, $value) === 1) {
            return (int) $value;
        }

        return 0;
    }
}
