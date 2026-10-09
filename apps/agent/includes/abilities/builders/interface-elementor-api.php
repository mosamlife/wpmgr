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
}
