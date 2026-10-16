<?php
/**
 * A configurable ElementorApi for unit tests.
 *
 * Every answer is a public property a test sets. Unloaded, it answers the
 * way ElementorRuntime does without Elementor: null, false or 0 for
 * everything but version(), which reads the constant and so may still be set.
 * Every call is recorded in $calls as [method, arguments].
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\ElementorApi;

/**
 * Test double for ElementorApi.
 */
final class FakeElementorApi implements ElementorApi
{
    /** Elementor's main instance exists. */
    public bool $loaded = true;

    /** What version() answers. */
    public ?string $version = '3.35.9';

    /**
     * Experiment name => answer. A name not listed answers false, as
     * Elementor does for an experiment it does not know.
     *
     * @var array<string, bool|null>
     */
    public array $experiments = ['container' => true, 'e_atomic_elements' => false];

    /** What activeKitId() answers. */
    public int $activeKitId = 0;

    /**
     * What activeBreakpoints() answers: the breakpoints manager's names, as
     * Elementor's defaults give them.
     *
     * @var list<string>|null
     */
    public ?array $activeBreakpoints = ['mobile', 'tablet'];

    /**
     * What fontFamilies() answers: the registry the shared case table's font
     * row gives the site.
     *
     * @var list<string>|null
     */
    public ?array $fontFamilies = ['Manrope', 'Inter', 'Lora', 'Roboto', 'Source Sans 3'];

    /**
     * Post id => Elementor document stand-in.
     *
     * @var array<int, object>
     */
    public array $documents = [];

    /**
     * Registered element types.
     *
     * @var list<string>
     */
    public array $elementTypes = ['section', 'column', 'container'];

    /**
     * Registered widget types.
     *
     * @var list<string>
     */
    public array $widgetTypes = ['heading', 'text-editor', 'button', 'image', 'divider', 'spacer'];

    /** @var list<array{name: string, title: string, categories: list<string>, pro: bool, atomic: bool}>|null */
    public ?array $widgetCatalogue = [];

    /** @var list<string> Registered classes whose source is in Elementor core. */
    public array $coreWidgets = [];

    /** @var list<string> Registered classes whose source is in the Pro plugin. */
    public array $proWidgets = [];

    /** @var array<string, array<string, array<string, mixed>>|null> */
    public array $widgetControls = [];

    /**
     * Builds the element instance for a node whose type is registered. Null
     * gives an instance whose get_data_for_save() returns the node unchanged.
     *
     * @var (\Closure(array<string, mixed>): ?object)|null
     */
    public ?\Closure $elementFactory = null;

    /**
     * Stands in for ElementorApi::ksesPostDeep(), the sanitiser answer that
     * is the same on every Elementor version. Null returns the data
     * unchanged; a closure may return null for "cannot be had".
     *
     * @var (\Closure(array<mixed>): ?array<mixed>)|null
     */
    public ?\Closure $kses = null;

    /**
     * What regeneratePostCss() answers once it has run: the CSS meta status
     * ("file", "inline" or "empty") and the file path Elementor writes the
     * stylesheet to. The default is a page with no stylesheet to write. Null
     * is "cannot be had".
     *
     * @var array{status: string, path: string}|null
     */
    public ?array $regenerated = ['status' => 'empty', 'path' => ''];

    /**
     * Runs inside regeneratePostCss(), before it answers: writes the
     * stylesheet, fires the hooks a regeneration's writes fire, or throws.
     *
     * @var (\Closure(int): void)|null
     */
    public ?\Closure $onRegenerate = null;

    /** What proActive() answers: Elementor Pro's version constant is defined. */
    public bool $pro = false;

    public ?string $proVersion = null;
    public bool $themeBuilderReady = false;
    /** @var array<int, array<mixed>|null> */
    public array $templateRules = [];
    public ?\Closure $onSaveTemplateRules = null;

    public ?\Closure $onTemplateRules = null;
    /** @var list<string> */
    public array $themeConditions = [];

    /** What elementCacheKey() answers. */
    public string $elementCacheKey = '_elementor_element_cache';

    /**
     * Every call: [method, arguments].
     *
     * @var list<array{0: string, 1: list<mixed>}>
     */
    public array $calls = [];

    /**
     * {@inheritDoc}
     */
    public function loaded(): bool
    {
        $this->calls[] = ['loaded', []];

        return $this->loaded;
    }

    /**
     * {@inheritDoc}
     */
    public function version(): ?string
    {
        $this->calls[] = ['version', []];

        return $this->version;
    }

    /**
     * {@inheritDoc}
     */
    public function experimentActive(string $name): ?bool
    {
        $this->calls[] = ['experimentActive', [$name]];
        if (!$this->loaded) {
            return null;
        }

        return array_key_exists($name, $this->experiments) ? $this->experiments[$name] : false;
    }

    /**
     * {@inheritDoc}
     */
    public function activeKitId(): int
    {
        $this->calls[] = ['activeKitId', []];

        return $this->loaded ? $this->activeKitId : 0;
    }

    /**
     * {@inheritDoc}
     */
    public function activeBreakpoints(): ?array
    {
        $this->calls[] = ['activeBreakpoints', []];

        return $this->loaded ? $this->activeBreakpoints : null;
    }

    /**
     * {@inheritDoc}
     */
    public function fontFamilies(): ?array
    {
        $this->calls[] = ['fontFamilies', []];

        return $this->loaded ? $this->fontFamilies : null;
    }

    /**
     * {@inheritDoc}
     */
    public function document(int $postId): ?object
    {
        $this->calls[] = ['document', [$postId]];

        return $this->loaded ? ($this->documents[$postId] ?? null) : null;
    }

    /**
     * {@inheritDoc}
     */
    public function createElementInstance(array $node): ?object
    {
        $this->calls[] = ['createElementInstance', [$node]];
        if (!$this->loaded) {
            return null;
        }
        $type   = $node['elType'] ?? null;
        $widget = $node['widgetType'] ?? null;
        $known  = $type === 'widget'
            ? is_string($widget) && in_array($widget, $this->widgetTypes, true)
            : is_string($type) && in_array($type, $this->elementTypes, true);
        if (!$known) {
            return null;
        }
        if ($this->elementFactory !== null) {
            return ($this->elementFactory)($node);
        }

        return new class ($node) {
            /**
             * @param array<string, mixed> $node The node.
             */
            public function __construct(private array $node)
            {
            }

            /**
             * @return array<string, mixed>
             */
            public function get_data_for_save(): array
            {
                return $this->node;
            }
        };
    }

    /**
     * {@inheritDoc}
     */
    public function elementTypeExists(string $type): bool
    {
        $this->calls[] = ['elementTypeExists', [$type]];

        return $this->loaded && in_array($type, $this->elementTypes, true);
    }

    /**
     * {@inheritDoc}
     */
    public function widgetTypeExists(string $type): bool
    {
        $this->calls[] = ['widgetTypeExists', [$type]];

        return $this->loaded && in_array($type, $this->widgetTypes, true);
    }

    /**
     * {@inheritDoc}
     */
    public function widgetCatalogue(): ?array
    {
        $this->calls[] = ['widgetCatalogue', []];

        return $this->loaded ? $this->widgetCatalogue : null;
    }

    public function widgetIsCore(string $type): bool
    {
        $this->calls[] = ['widgetIsCore', [$type]];
        return $this->loaded && in_array($type, $this->coreWidgets, true);
    }

    public function widgetIsPro(string $type): bool
    {
        $this->calls[] = ['widgetIsPro', [$type]];
        return $this->loaded && $this->pro && in_array($type, $this->proWidgets, true);
    }

    /**
     * {@inheritDoc}
     */
    public function widgetControls(string $name): ?array
    {
        $this->calls[] = ['widgetControls', [$name]];

        return $this->loaded ? ($this->widgetControls[$name] ?? null) : null;
    }

    /**
     * {@inheritDoc}
     */
    public function ksesPostDeep(array $data): ?array
    {
        $this->calls[] = ['ksesPostDeep', [$data]];
        if (!$this->loaded) {
            return null;
        }

        return $this->kses === null ? $data : ($this->kses)($data);
    }

    /**
     * {@inheritDoc}
     *
     * Answers true when loaded and the post id is positive; the call is
     * recorded either way.
     */
    public function deletePostCss(int $postId): bool
    {
        $this->calls[] = ['deletePostCss', [$postId]];

        return $this->loaded && $postId > 0;
    }

    /**
     * {@inheritDoc}
     *
     * Answers $regenerated when loaded and the post id is positive, after
     * running $onRegenerate; the call is recorded either way.
     */
    public function regeneratePostCss(int $postId): ?array
    {
        $this->calls[] = ['regeneratePostCss', [$postId]];
        if (!$this->loaded || $postId < 1) {
            return null;
        }
        if ($this->onRegenerate !== null) {
            ($this->onRegenerate)($postId);
        }

        return $this->regenerated;
    }

    /**
     * {@inheritDoc}
     */
    public function proActive(): bool
    {
        $this->calls[] = ['proActive', []];

        return $this->pro;
    }

    public function proVersion(): ?string
    {
        $this->calls[] = ['proVersion', []];
        return $this->pro ? $this->proVersion : null;
    }

    public function themeBuilderReady(): bool
    {
        $this->calls[] = ['themeBuilderReady', []];
        return $this->loaded && $this->pro && $this->themeBuilderReady;
    }

    public function templateRules(int $postId): ?array
    {
        $this->calls[] = ['templateRules', [$postId]];
        if ($this->onTemplateRules !== null) {
            return ($this->onTemplateRules)($postId);
        }
        return $this->loaded && $this->pro && $this->themeBuilderReady ? ($this->templateRules[$postId] ?? null) : null;
    }

    public function saveTemplateRules(int $postId, array $rules): bool
    {
        $this->calls[] = ['saveTemplateRules', [$postId, $rules]];
        if ($this->onSaveTemplateRules !== null) {
            return ($this->onSaveTemplateRules)($postId, $rules);
        }
        $this->templateRules[$postId] = $rules;
        return true;
    }

    public function themeConditionRegistered(string $name): bool
    {
        $this->calls[] = ['themeConditionRegistered', [$name]];
        return $this->loaded && $this->pro && $this->themeBuilderReady && in_array($name, $this->themeConditions, true);
    }

    /**
     * {@inheritDoc}
     */
    public function elementCacheKey(): string
    {
        $this->calls[] = ['elementCacheKey', []];

        return $this->elementCacheKey;
    }

    /**
     * The arguments of every call to $method, in order.
     *
     * @param string $method Method name.
     * @return list<list<mixed>>
     */
    public function callsTo(string $method): array
    {
        $args = [];
        foreach ($this->calls as [$name, $given]) {
            if ($name === $method) {
                $args[] = $given;
            }
        }

        return $args;
    }

    /**
     * Register a stand-in Elementor document for $postId and return it.
     *
     * Its save() records the data it is given, sets the numeric locale to
     * "C" as Elementor's does first, then answers what $onSave answers (true
     * when unset; $onSave may also throw, fire hooks or change rows), and
     * puts the locale back only when that answer is true, as Elementor does.
     * Its get_newer_autosave() answers $newerAutosave (false when unset, as
     * Elementor does without a newer autosave), or what that closure
     * answers, which may throw.
     *
     * @param int    $postId   Post id.
     * @param string $name     What get_name() answers.
     * @param bool   $editable What is_editable_by_current_user() answers.
     * @return object
     */
    public function addDocument(int $postId, string $name = 'wp-page', bool $editable = true): object
    {
        $document = new class ($name, $editable) {
            /** @var list<mixed> Data given to each save(), in order. */
            public array $saves = [];

            /** @var (\Closure(mixed): mixed)|null */
            public ?\Closure $onSave = null;

            /** @var mixed What get_newer_autosave() answers; a closure is called. */
            public mixed $newerAutosave = false;

            public function __construct(public string $name, public bool $editable)
            {
            }

            public function get_name(): string
            {
                return $this->name;
            }

            public function is_editable_by_current_user(): bool
            {
                return $this->editable;
            }

            /**
             * @return mixed
             */
            public function get_newer_autosave()
            {
                return $this->newerAutosave instanceof \Closure ? ($this->newerAutosave)() : $this->newerAutosave;
            }

            /**
             * @param mixed $data Save data.
             * @return mixed
             */
            public function save($data)
            {
                $this->saves[] = $data;
                $original      = setlocale(LC_NUMERIC, '0');
                setlocale(LC_NUMERIC, 'C');
                $answer = $this->onSave === null ? true : ($this->onSave)($data);
                if ($answer === true && is_string($original)) {
                    setlocale(LC_NUMERIC, $original);
                }

                return $answer;
            }
        };
        $this->documents[$postId] = $document;

        return $document;
    }

    /**
     * Register a stand-in document for $postId whose save() stores the
     * elements it is given as Elementor's does: every _elementor_data row of
     * the post replaced by one row holding json_encode() of the elements, then
     * the version row. $filter, when given, changes the elements first, as a
     * plugin's filter on the saved data would.
     *
     * @param int                                      $postId Post id.
     * @param FakeBuilderWpdb                          $wpdb   The rows.
     * @param (\Closure(list<mixed>): list<mixed>)|null $filter Changes the elements before they are stored.
     * @return object
     */
    public function storingDocument(int $postId, FakeBuilderWpdb $wpdb, ?\Closure $filter = null): object
    {
        $document         = $this->addDocument($postId);
        $document->onSave = static function ($data) use ($postId, $wpdb, $filter): bool {
            $elements = is_array($data) && is_array($data['elements'] ?? null) ? $data['elements'] : [];
            if ($filter !== null) {
                $elements = $filter($elements);
            }
            $wpdb->delete($wpdb->postmeta, ['post_id' => $postId, 'meta_key' => '_elementor_data']);
            $wpdb->insert($wpdb->postmeta, ['post_id' => $postId, 'meta_key' => '_elementor_data', 'meta_value' => (string) json_encode($elements)]);
            $wpdb->delete($wpdb->postmeta, ['post_id' => $postId, 'meta_key' => '_elementor_version']);
            $wpdb->insert($wpdb->postmeta, ['post_id' => $postId, 'meta_key' => '_elementor_version', 'meta_value' => '3.35.9']);

            return true;
        };

        return $document;
    }

    /**
     * The names of the experiments asked about, in order.
     *
     * @return list<string>
     */
    public function experimentsAsked(): array
    {
        $names = [];
        foreach ($this->calls as [$method, $args]) {
            if ($method === 'experimentActive') {
                $names[] = (string) $args[0];
            }
        }

        return $names;
    }
}
