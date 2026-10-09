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
