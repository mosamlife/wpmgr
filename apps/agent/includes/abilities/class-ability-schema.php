<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Structural projection and hash of an ability input schema.
 *
 * Free text never survives the projection: title, description, default,
 * examples and translated strings are dropped, so a site or vendor cannot
 * smuggle prose to the model through a schema. Only shape stays: type,
 * property names, required, items, additionalProperties and (unless the path
 * is declared dynamic) enum values.
 */
final class AbilitySchema
{
    /**
     * Structural hash of an ability's input schema, or null when there is none.
     *
     * @param object       $ability Registered ability.
     * @param list<string> $dynamic Dotted property paths whose enum is dropped.
     * @return string|null
     */
    public static function hashOf(object $ability, array $dynamic = []): ?string
    {
        if (!method_exists($ability, 'get_input_schema')) {
            return null;
        }
        try {
            $schema = $ability->get_input_schema();
        } catch (\Throwable $e) {
            return null;
        }
        if (!is_array($schema)) {
            return null;
        }
        $json = json_encode(self::structural($schema, '', $dynamic));

        return is_string($json) ? 'sha256:' . hash('sha256', $json) : null;
    }

    /**
     * The structural projection itself.
     *
     * @param array<mixed> $node    Schema node.
     * @param string       $path    Dotted property path of this node.
     * @param list<string> $dynamic Dynamic enum paths.
     * @return array<mixed>
     */
    public static function structural(array $node, string $path, array $dynamic): array
    {
        $out = [];
        if (isset($node['type']) && (is_string($node['type']) || is_array($node['type']))) {
            $type = $node['type'];
            if (is_array($type)) {
                $type = array_values(array_filter($type, 'is_string'));
                sort($type);
            }
            $out['type'] = $type;
        }
        if (isset($node['properties']) && is_array($node['properties'])) {
            $props = [];
            foreach ($node['properties'] as $key => $child) {
                $childPath = $path === '' ? (string) $key : $path . '.' . $key;
                $props[(string) $key] = is_array($child) ? self::structural($child, $childPath, $dynamic) : [];
            }
            ksort($props);
            $out['properties'] = $props === [] ? new \stdClass() : $props;
        }
        if (isset($node['required']) && is_array($node['required'])) {
            $req = array_values(array_filter($node['required'], 'is_string'));
            sort($req);
            $out['required'] = $req;
        }
        if (isset($node['items']) && is_array($node['items'])) {
            $out['items'] = self::structural($node['items'], $path, $dynamic);
        }
        if (array_key_exists('additionalProperties', $node)) {
            $out['additionalProperties'] = is_array($node['additionalProperties'])
                ? self::structural($node['additionalProperties'], $path, $dynamic)
                : (bool) $node['additionalProperties'];
        }
        if (isset($node['enum']) && is_array($node['enum']) && !in_array($path, $dynamic, true)) {
            $enum = array_map(static fn ($v) => is_scalar($v) ? $v : null, array_values($node['enum']));
            usort($enum, static fn ($a, $b) => strcmp((string) json_encode($a), (string) json_encode($b)));
            $out['enum'] = $enum;
        }
        ksort($out);

        return $out;
    }
}
