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
 * The post fields always belong to the document: the fingerprint covers the
 * type, status, title, content, excerpt, slug, password, modified time (GMT),
 * parent and menu order (BuilderDocumentFingerprint), and a snapshot keeps
 * those and the other columns a restore puts back
 * (BuilderDocumentSnapshot::RESTORE_POST_COLUMNS). This names the meta. A
 * snapshot keeps every meta row of the post, not only the descriptor's, so a
 * row any plugin writes during a save can be put back too.
 *
 * - exactKeys    meta keys of the document; the fingerprint covers these.
 * - prefixes     meta key prefixes whose every key belongs to the document.
 * - derivedKeys  caches the builder derives from the document; deleted on
 *                restore, never fingerprinted.
 * - flagKeys     meta keys that mark the post as built with the builder.
 *
 * Immutable. Each list holds distinct non-empty strings. A restore puts every
 * key but a derived one back byte for byte, so a derived key is never one of
 * the document's keys (an exact key, a flag key or a key under a prefix) and
 * never core's page template key (PAGE_TEMPLATE_KEY).
 */
final class DocumentDescriptor
{
    /** Core's page template meta key; a restore always puts it back. */
    public const PAGE_TEMPLATE_KEY = '_wp_page_template';

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
     * @throws \InvalidArgumentException When a list holds an empty, repeated or non-string key, or a derived key is one a restore puts back.
     */
    public function __construct(array $exactKeys, array $prefixes = [], array $derivedKeys = [], array $flagKeys = [])
    {
        $this->exactKeys   = self::keys($exactKeys);
        $this->prefixes    = self::keys($prefixes);
        $this->derivedKeys = self::keys($derivedKeys);
        $this->flagKeys    = self::keys($flagKeys);
        foreach ($this->derivedKeys as $key) {
            if ($key === self::PAGE_TEMPLATE_KEY || in_array($key, $this->exactKeys, true) || in_array($key, $this->flagKeys, true) || $this->underPrefix($key)) {
                throw new \InvalidArgumentException('a derived key must not be a key a restore puts back');
            }
        }
    }

    /**
     * Whether $key starts with one of the document's prefixes, byte for byte.
     *
     * @param string $key Meta key.
     * @return bool
     */
    private function underPrefix(string $key): bool
    {
        foreach ($this->prefixes as $prefix) {
            if (str_starts_with($key, $prefix)) {
                return true;
            }
        }

        return false;
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
