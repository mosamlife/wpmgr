<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

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
 * nothing is loaded and every method answers "cannot tell". Every call is
 * guarded, and anything Elementor throws becomes that answer (null, false or
 * 0); no method throws. Nothing here writes.
 */
final class ElementorRuntime implements ElementorApi
{
    /** Elementor's main plugin class. */
    public const PLUGIN_CLASS = 'Elementor\\Plugin';

    /** Elementor's utility class, which holds its document sanitiser. */
    public const UTILS_CLASS = 'Elementor\\Utils';

    /** The constant Elementor defines its version in. */
    public const VERSION_CONSTANT = 'ELEMENTOR_VERSION';

    /** A plain version token: a digit first, at most 32 bytes. */
    private const RE_VERSION = '/^[0-9][0-9A-Za-z._+-]{0,31}$/D';

    /** A positive decimal id that fits an int. */
    private const RE_ID = '/^[1-9][0-9]{0,17}$/D';

    /**
     * @param string $pluginClass     Tests only: stands in for Elementor's main plugin class. Production passes none.
     * @param string $utilsClass      Tests only: stands in for Elementor's utility class. Production passes none.
     * @param string $versionConstant Tests only: stands in for Elementor's version constant. Production passes none.
     */
    public function __construct(
        private readonly string $pluginClass = self::PLUGIN_CLASS,
        private readonly string $utilsClass = self::UTILS_CLASS,
        private readonly string $versionConstant = self::VERSION_CONSTANT
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
