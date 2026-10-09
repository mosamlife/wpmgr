<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\VersionCompare;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Whether a builder can be used on this site now.
 *
 * The tested range is agent code. Catalogue data may only narrow it
 * (narrowed()), never widen it, so a builder version WPMgr has not been
 * tested against is refused whatever an entry says. Versions are ordered by
 * VersionCompare, the same ordering the control plane uses.
 *
 * Immutable. Reasons are detail tokens from WPMgr's vocabulary, never text
 * read from the site.
 */
final class AdapterStatus
{
    /** The refusal code for every status problem. */
    public const CODE = 'builder_not_available';

    /** A reason token. */
    private const RE_REASON = '/^[a-z0-9_]{1,64}$/D';

    /**
     * Detail tokens that each refuse, in the order recorded.
     *
     * @var list<string>
     */
    public readonly array $reasons;

    /**
     * @param bool          $active    The builder is installed and active.
     * @param string|null   $version   The builder's own version; null when unknown.
     * @param string        $testedMin Lowest version this code admits.
     * @param string        $testedMax Highest version this code admits.
     * @param array<string> $reasons   Detail tokens that each refuse.
     * @throws \InvalidArgumentException When a reason is not a lowercase token.
     */
    public function __construct(
        public readonly bool $active,
        public readonly ?string $version,
        public readonly string $testedMin,
        public readonly string $testedMax,
        array $reasons = []
    ) {
        $list = [];
        foreach ($reasons as $reason) {
            if (!is_string($reason) || preg_match(self::RE_REASON, $reason) !== 1) {
                throw new \InvalidArgumentException('a status reason must be a lowercase token');
            }
            $list[] = $reason;
        }
        $this->reasons = $list;
    }

    /**
     * The same status with the admitted range intersected with [$min, $max].
     * A bound that would widen the range is ignored.
     *
     * @param string|null $min Lowest version the catalogue admits; null for no bound.
     * @param string|null $max Highest version the catalogue admits; null for no bound.
     * @return self
     */
    public function narrowed(?string $min, ?string $max): self
    {
        $low  = $this->testedMin;
        $high = $this->testedMax;
        if ($min !== null && VersionCompare::compare($min, $low) > 0) {
            $low = $min;
        }
        if ($max !== null && VersionCompare::compare($max, $high) < 0) {
            $high = $max;
        }

        return new self($this->active, $this->version, $low, $high, $this->reasons);
    }

    /**
     * The refusal this status answers with, or null when the builder may be
     * used. The detail is the first that applies of: the first reason (or
     * builder_missing) for an inactive builder; version_unverified for an
     * unknown version or one outside the admitted range; the first reason.
     *
     * @return array{code: string, detail: string}|null
     */
    public function refusal(): ?array
    {
        if (!$this->active) {
            return ['code' => self::CODE, 'detail' => $this->reasons[0] ?? 'builder_missing'];
        }
        if ($this->version === null || !VersionCompare::inRange($this->version, $this->testedMin, $this->testedMax)) {
            return ['code' => self::CODE, 'detail' => 'version_unverified'];
        }
        if ($this->reasons !== []) {
            return ['code' => self::CODE, 'detail' => $this->reasons[0]];
        }

        return null;
    }
}
