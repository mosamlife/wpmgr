<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

if (!defined('ABSPATH')) {
    exit;
}

/** Display-rule evidence and verification for edits of header and footer drafts. */
final class ThemeTemplateEdit
{
    /** @param list<array<string,mixed>> $ops @return array<string,mixed> */
    public static function plan(int $postId, ?string $type, array $ops, ThemeTemplateCreate $templates): array
    {
        $after = null;
        foreach ($ops as $op) {
            if ($op['op'] === 'set_shows_on') {
                if ($type === null) {
                    return ['code' => 'bad_input', 'detail' => 'set_shows_on requires a header or footer template draft'];
                }
                $after = $op['rules'];
            }
        }
        if ($type === null) {
            return [];
        }
        if (!ThemeTemplateCreate::termMatches($postId, $type)) {
            return ['code' => 'target_not_eligible', 'detail' => 'template_type_term'];
        }
        $unavailable = $templates->availabilityRefusal();
        if ($unavailable !== null) {
            return $unavailable;
        }
        try {
            $before = $templates->readRules($postId);
            if (ElementorThemeTemplates::rulesProblem($before) !== null) {
                return ['code' => 'data_unreadable', 'detail' => 'shows_on could not be read through Elementor Pro'];
            }
            if ($after !== null) {
                $refusal = $templates->readinessRefusal($after);
                if ($refusal !== null) {
                    return $refusal;
                }
            }
            return ['replace' => $after !== null, 'preview' => [
                'template' => $type,
                'shows_on' => ['before' => $before, 'after' => $after ?? $before],
                'rule_titles' => ThemeTemplateCreate::ruleTitles(array_merge($before, $after ?? $before)),
            ]];
        } catch (\Throwable $e) {
            return ['code' => 'data_unreadable', 'detail' => 'shows_on or its rule titles could not be read through Elementor Pro'];
        }
    }

    /** Same rule strings and count, regardless of the order Pro returns them in. */
    public static function rulesProblem(array $wanted, ?array $stored): ?string
    {
        return ThemeTemplateCreate::sameRules($wanted, $stored) ? null
            : 'shows_on differs; Elementor Pro reports: ' . wp_json_encode($stored, JSON_UNESCAPED_SLASHES);
    }

    /** Verify Pro against the conditions rows in the authenticated snapshot after restoration. */
    public static function verifyRestored(int $postId, array $snapshot, ThemeTemplateCreate $templates, ?array &$reported = null): ?string
    {
        if (($snapshot['post']['post_type'] ?? null) !== 'elementor_library') {
            return null;
        }
        $rows = [];
        foreach ($snapshot['meta'] as $pair) {
            if ($pair[0] === ElementorRuntime::KEY_CONDITIONS) {
                $rows[] = $pair[1];
            }
        }
        $wanted = [];
        if ($rows !== []) {
            if (count($rows) !== 1 || !is_string($rows[0])) {
                return 'shows_on snapshot is unreadable';
            }
            $wanted = @unserialize($rows[0], ['allowed_classes' => false]);
            if (!is_array($wanted) || ElementorThemeTemplates::rulesProblem($wanted) !== null) {
                return 'shows_on snapshot is unreadable';
            }
        }
        try {
            $reported = $templates->readRules($postId);
            return self::rulesProblem($wanted, $reported);
        } catch (\Throwable $e) {
            return 'shows_on could not be read back through Elementor Pro';
        }
    }
}
