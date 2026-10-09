<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * A page in one builder's native format, as built and before it is stored.
 *
 * - tree            the builder's native element tree.
 * - meta            post meta the write stores, key => value, in write order.
 * - postContent     post_content the write stores; null leaves it unchanged.
 * - canonicalBytes  json_encode() of the tree with default flags. The preview
 *                   digest and the read-back compare use these bytes, and they
 *                   are computed here, so they always describe the tree.
 *
 * Immutable.
 */
final class NativeDocument
{
    /** @var array<mixed> */
    public readonly array $tree;

    /** @var array<string, mixed> */
    public readonly array $meta;

    public readonly ?string $postContent;

    public readonly string $canonicalBytes;

    /**
     * @param array<mixed> $tree        Native element tree.
     * @param array<mixed> $meta        Post meta, key => value.
     * @param string|null  $postContent post_content, or null.
     * @throws \InvalidArgumentException When a meta key is not a non-empty string.
     * @throws \JsonException When the tree cannot be encoded.
     */
    public function __construct(array $tree, array $meta = [], ?string $postContent = null)
    {
        $checked = [];
        foreach ($meta as $key => $value) {
            if (!is_string($key) || $key === '') {
                throw new \InvalidArgumentException('a meta key must be a non-empty string');
            }
            $checked[$key] = $value;
        }
        $this->tree           = $tree;
        $this->meta           = $checked;
        $this->postContent    = $postContent;
        $this->canonicalBytes = json_encode($tree, JSON_THROW_ON_ERROR);
    }
}
