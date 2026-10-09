<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Exactly which post meta rows make up a page in one builder's format.
 *
 * The post fields (type, status, title, content, modified time) always belong
 * to the document; this names the meta. Snapshot, fingerprint, restore and the
 * write recorder all read the same descriptor, so they agree on what the page
 * is.
 *
 * - exactKeys    meta keys of the document; the fingerprint covers these.
 * - prefixes     meta key prefixes whose every key belongs to the document.
 * - derivedKeys  caches the builder derives from the document; deleted on
 *                restore, never fingerprinted.
 * - flagKeys     meta keys that mark the post as built with the builder.
 *
 * Immutable. Each list holds distinct non-empty strings.
 */
final class DocumentDescriptor
{
    /** @var list<string> */
    public readonly array $exactKeys;

    /** @var list<string> */
    public readonly array $prefixes;

    /** @var list<string> */
    public readonly array $derivedKeys;

    /** @var list<string> */
    public readonly array $flagKeys;

    /**
     * @param array<string> $exactKeys   Exact meta keys of the document.
     * @param array<string> $prefixes    Meta key prefixes of the document.
     * @param array<string> $derivedKeys Meta keys of derived caches.
     * @param array<string> $flagKeys    Meta keys that mark the builder's posts.
     * @throws \InvalidArgumentException When a list holds an empty, repeated or non-string key.
     */
    public function __construct(array $exactKeys, array $prefixes = [], array $derivedKeys = [], array $flagKeys = [])
    {
        $this->exactKeys   = self::keys($exactKeys);
        $this->prefixes    = self::keys($prefixes);
        $this->derivedKeys = self::keys($derivedKeys);
        $this->flagKeys    = self::keys($flagKeys);
    }

    /**
     * @param array<mixed> $keys Keys.
     * @return list<string>
     * @throws \InvalidArgumentException When a key is empty, repeated or not a string.
     */
    private static function keys(array $keys): array
    {
        $list = [];
        foreach ($keys as $key) {
            if (!is_string($key) || $key === '' || in_array($key, $list, true)) {
                throw new \InvalidArgumentException('a descriptor key must be a distinct non-empty string');
            }
            $list[] = $key;
        }

        return $list;
    }
}
