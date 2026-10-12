<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Everything the agent asks of Elementor, behind one seam.
 *
 * ElementorRuntime is the implementation that talks to the Elementor loaded
 * on this request; tests use a double. No method throws: an answer that
 * cannot be had is null, false or 0, as each method states, and a caller
 * refuses on it.
 */
interface ElementorApi
{
    /**
     * Elementor's main plugin instance exists on this request.
     *
     * @return bool
     */
    public function loaded(): bool;

    /**
     * Elementor's own version string, or null when it is not defined or is
     * not a plain version token.
     *
     * @return string|null
     */
    public function version(): ?string;

    /**
     * Whether an Elementor experiment is active, asked of Elementor's
     * experiments manager. Null when that cannot be asked or does not
     * answer with a boolean.
     *
     * @param string $name Experiment name.
     * @return bool|null
     */
    public function experimentActive(string $name): ?bool;

    /**
     * The post id of the site's active Elementor kit, or 0 when there is
     * none or it cannot be read.
     *
     * @return int
     */
    public function activeKitId(): int;

    /**
     * The names Elementor's breakpoints manager keys its active breakpoints
     * by (mobile, tablet, laptop, ...), in the order it reports them. Null
     * when the manager cannot be asked or does not answer with a list.
     *
     * The manager answers from the active kit's saved list when there is
     * one and from Elementor's defaults when there is none, so this is the
     * answer to use when the kit has never saved its list.
     *
     * @return list<string>|null
     */
    public function activeBreakpoints(): ?array;

    /**
     * The family names in Elementor's font registry, as Elementor keys them:
     * its own fonts and those other plugins register through Elementor. Null
     * when Elementor is not loaded, the registry cannot be read or is not a
     * map, or it holds more families than the runtime reads.
     *
     * @return list<string>|null
     */
    public function fontFamilies(): ?array;

    /**
     * Elementor's document for a post, read fresh (not from Elementor's
     * per-request cache). Null when there is none.
     *
     * @param int $postId Post id.
     * @return object|null
     */
    public function document(int $postId): ?object;

    /**
     * Elementor's element instance for one node of an element tree. Null when
     * the node names no registered element type or widget, or Elementor
     * could not build it.
     *
     * @param array<string, mixed> $node One node: elType, widgetType for a widget, settings, elements.
     * @return object|null
     */
    public function createElementInstance(array $node): ?object;

    /**
     * An element type (section, column, container, ...) is registered.
     *
     * @param string $type Element type name.
     * @return bool
     */
    public function elementTypeExists(string $type): bool;

    /**
     * A widget type is registered.
     *
     * @param string $type Widget type name.
     * @return bool
     */
    public function widgetTypeExists(string $type): bool;

    /**
     * $data with every string passed through wp_kses_post(), which is what
     * Elementor's document save does to the strings of a document saved by a
     * user without unfiltered_html; every other value is returned as given.
     * Elementor's own deep sanitiser answers where the running Elementor has
     * one, and wp_kses_post() is applied here where it has none, so the
     * answer is the same on every supported Elementor version.
     *
     * Null only when no answer can be had: Elementor is not loaded, its own
     * sanitiser fails or does not return an array, wp_kses_post() is not
     * available, or $data nests deeper than ElementorDocument::MAX_DEPTH. A
     * caller refuses on null.
     *
     * @param array<mixed> $data Element tree or settings.
     * @return array<mixed>|null
     */
    public function ksesPostDeep(array $data): ?array;

    /**
     * Delete a post's generated CSS file and its CSS meta through Elementor's
     * own post CSS object, as Elementor's document save does.
     *
     * False when that cannot be asked: the post id is not positive, Elementor
     * is not loaded, its files manager or the object cannot be had, or
     * Elementor throws.
     *
     * @param int $postId Post id.
     * @return bool
     */
    public function deletePostCss(int $postId): bool;

    /**
     * Make a post's CSS again through a new post CSS object of Elementor's
     * own, which writes the stylesheet file and the post's CSS meta, then ask
     * that object what it left.
     *
     * "status" is the status of the post's CSS meta: "file" when the
     * stylesheet is served as a file, "inline" when it is printed in the page,
     * "empty" when the page has nothing to style. "path" is the file Elementor
     * writes a "file" stylesheet to, or "" when it cannot be had.
     *
     * Null when the answer cannot be had: the post id is not positive,
     * Elementor is not loaded or has no post CSS class, Elementor throws, or
     * its object does not report a status.
     *
     * @param int $postId Post id.
     * @return array{status: string, path: string}|null
     */
    public function regeneratePostCss(int $postId): ?array;

    /**
     * Whether Elementor Pro is active on this request: its version constant
     * is defined. Asked of the constant alone, so it is the same answer the
     * site's builder facts give as pro_active.
     *
     * @return bool
     */
    public function proActive(): bool;

    /**
     * The post meta key under which Elementor keeps a post's cached element
     * output: Elementor's own document constant when this request has one
     * that names a cache, else ElementorDocument::KEY_ELEMENT_CACHE. Always a
     * key, never empty.
     *
     * @return string
     */
    public function elementCacheKey(): string;
}
