<?php
/**
 * The frozen Elementor round-trip probe trees, read from the committed
 * harness file so a compile test and the harness judge one set of bytes.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

/**
 * A probe is a raw Elementor tree the harness saves, renders and holds to the
 * post CSS. Key order is the order the compiler writes, and PHP keeps it when
 * the file is decoded, so a compile test compares encoded bytes.
 */
final class RoundTripProbes
{
    /** Where the harness keeps the probes, from the repository root. */
    private const FILE = 'scripts/elementor-roundtrip/render-probes.json';

    /**
     * The containers tree of a probe.
     *
     * @param string $name Probe name, e.g. layout-section-container.
     * @return list<array<string,mixed>>
     */
    public static function tree(string $name): array
    {
        $probe = self::probe($name);
        $trees = $probe['trees'] ?? null;
        $tree  = is_array($trees) ? ($trees['containers'] ?? null) : null;
        if (!is_array($tree) || $tree === []) {
            throw new \RuntimeException('probe ' . $name . ' has no containers tree');
        }

        return $tree;
    }

    /**
     * The settings of the first container of a probe's containers tree.
     *
     * @param string $name Probe name.
     * @return array<string,mixed>
     */
    public static function settings(string $name): array
    {
        $settings = self::tree($name)[0]['settings'] ?? null;
        if (!is_array($settings) || $settings === []) {
            throw new \RuntimeException('probe ' . $name . ' has no container settings');
        }

        return $settings;
    }

    /**
     * One probe.
     *
     * @param string $name Probe name.
     * @return array<string,mixed>
     */
    private static function probe(string $name): array
    {
        $path = dirname(__DIR__, 4) . '/' . self::FILE;
        $json = is_readable($path) ? file_get_contents($path) : false;
        if (!is_string($json) || $json === '') {
            throw new \RuntimeException('the round-trip probe file is missing: ' . self::FILE);
        }
        $doc    = json_decode($json, true, 512, JSON_THROW_ON_ERROR);
        $probes = is_array($doc) ? ($doc['probes'] ?? null) : null;
        if (!is_array($probes)) {
            throw new \RuntimeException('the round-trip probe file has no probes');
        }
        foreach ($probes as $probe) {
            if (is_array($probe) && ($probe['name'] ?? null) === $name) {
                return $probe;
            }
        }

        throw new \RuntimeException('no probe named ' . $name);
    }
}
