<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Deterministic node ids for one request.
 *
 * A precheck and its write must build the same bytes, so a new node's id is
 * derived, never random:
 *
 *   id = first $length hex characters of
 *        sha256(json_encode(["wpmgr.builder.id.v1", request_id, node_path, counter]))
 *
 * json_encode() with default flags. The counter starts at 0 for each call and
 * is bumped while the id equals one already issued by this seed or reserved
 * (an id already on the page), so the same request, node paths, call order
 * and reservations always give the same ids.
 *
 * Ids are lowercase hex; Elementor's are 7 characters long.
 */
final class IdSeed
{
    /** Domain label hashed into every id. */
    public const DOMAIN = 'wpmgr.builder.id.v1';

    /** Counter values tried before giving up on a free id. */
    private const MAX_ATTEMPTS = 1000;

    private string $requestId;

    private int $length;

    /**
     * Ids issued or reserved (PHP turns an all-digit id into an int key).
     *
     * @var array<int|string, true>
     */
    private array $taken = [];

    /**
     * @param string $requestId The request the ids belong to.
     * @param int    $length    Hex characters per id, 1 to 64.
     * @throws \InvalidArgumentException When the request id is empty or the length is out of range.
     */
    public function __construct(string $requestId, int $length = 7)
    {
        if ($requestId === '') {
            throw new \InvalidArgumentException('an id seed needs a request id');
        }
        if ($length < 1 || $length > 64) {
            throw new \InvalidArgumentException('an id length must be 1 to 64');
        }
        $this->requestId = $requestId;
        $this->length    = $length;
    }

    /**
     * The id for the node at $nodePath.
     *
     * @param string $nodePath Where the node sits in the input, e.g. "outline[2].children[0]".
     * @return string
     * @throws \RuntimeException When no free id is found within the attempt limit.
     * @throws \JsonException When the request id or node path is not valid UTF-8.
     */
    public function next(string $nodePath): string
    {
        for ($counter = 0; $counter < self::MAX_ATTEMPTS; $counter++) {
            $encoded = json_encode([self::DOMAIN, $this->requestId, $nodePath, $counter], JSON_THROW_ON_ERROR);
            $id      = substr(hash('sha256', $encoded), 0, $this->length);
            if (!isset($this->taken[$id])) {
                $this->taken[$id] = true;

                return $id;
            }
        }

        throw new \RuntimeException('no free node id within the attempt limit');
    }

    /**
     * Mark ids already on the page as taken. Strings and integers are taken
     * as their string form; anything else is ignored.
     *
     * @param array<mixed> $existing Ids on the page.
     * @return void
     */
    public function reserve(array $existing): void
    {
        foreach ($existing as $id) {
            if (is_string($id) || is_int($id)) {
                $this->taken[(string) $id] = true;
            }
        }
    }
}
