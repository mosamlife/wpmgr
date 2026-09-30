<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * WPMgr's own abilities: a closed map, in code.
 *
 * Each is callable in-process, so the engine works on WordPress 6.2+, and is
 * also registered with the WordPress Abilities API when that exists (6.9+).
 * The engine never calls a site-registered ability that merely carries a
 * wpmgr/ name: resolution goes through this map only.
 *
 * All three are reads. They write nothing, and text that came from the site
 * (titles, page text, ability labels) is returned only under a
 * `from_the_site` key, so the control plane can fence it as data.
 */
final class OwnAbilities
{
    public const NAME_INVENTORY = 'wpmgr/abilities-inventory';
    public const NAME_FACTS     = 'wpmgr/site-facts';
    public const NAME_CONTENT   = 'wpmgr/content-read';

    private const CATEGORY = 'wpmgr';

    /** Most abilities the inventory lists. */
    private const INVENTORY_MAX = 500;

    private const LABEL_CAP_BYTES = 120;

    private const DESCRIPTION_CAP_BYTES = 300;

    private const TITLE_CAP_BYTES = 200;

    private const TEXT_DEFAULT_BYTES = 20000;

    private const TEXT_MAX_BYTES = 65536;

    private const PLUGINS_MAX = 200;

    private const RE_SLUG = '/^[A-Za-z0-9._-]{1,100}$/';

    /**
     * Names of the abilities this agent implements.
     *
     * @return list<string>
     */
    public static function names(): array
    {
        return [self::NAME_INVENTORY, self::NAME_FACTS, self::NAME_CONTENT];
    }

    /**
     * Is this one of ours?
     *
     * @param string $name Ability name.
     * @return bool
     */
    public static function has(string $name): bool
    {
        return in_array($name, self::names(), true);
    }

    /**
     * Effect class of an own ability. Every one is a read.
     *
     * @param string $name Ability name.
     * @return string
     */
    public static function abilityClass(string $name): string
    {
        return self::has($name) ? 'read' : 'unknown';
    }

    /**
     * JSON schema of an ability's input, as registered with the API.
     *
     * @param string $name Ability name.
     * @return array<string,mixed>
     */
    public static function inputSchema(string $name): array
    {
        $slugs = ['type' => 'array', 'maxItems' => 32, 'items' => ['type' => 'string']];
        switch ($name) {
            case self::NAME_FACTS:
                return [
                    'type'                 => 'object',
                    'properties'           => ['plugin_slugs' => $slugs, 'theme_slugs' => $slugs],
                    'additionalProperties' => false,
                ];
            case self::NAME_CONTENT:
                return [
                    'type'                 => 'object',
                    'properties'           => [
                        'post_id'   => ['type' => 'integer', 'minimum' => 1],
                        'max_bytes' => ['type' => 'integer', 'minimum' => 256, 'maximum' => self::TEXT_MAX_BYTES],
                    ],
                    'required'             => ['post_id'],
                    'additionalProperties' => false,
                ];
            default:
                return ['type' => 'object', 'properties' => new \stdClass(), 'additionalProperties' => false];
        }
    }

    /**
     * Validate an input object. Null means valid.
     *
     * @param string $name  Ability name.
     * @param object $input Decoded input object.
     * @return string|null Reason, or null.
     */
    public static function validate(string $name, object $input): ?string
    {
        $props = get_object_vars($input);
        $allowed = array_keys((array) (self::inputSchema($name)['properties'] ?? []));
        foreach (array_keys($props) as $key) {
            if (!in_array((string) $key, $allowed, true)) {
                return 'unknown input field';
            }
        }

        if ($name === self::NAME_FACTS) {
            foreach (['plugin_slugs', 'theme_slugs'] as $field) {
                if (!array_key_exists($field, $props)) {
                    continue;
                }
                $list = $props[$field];
                if (!is_array($list) || count($list) > 32) {
                    return $field . ' must be a list of at most 32 slugs';
                }
                foreach ($list as $slug) {
                    if (!is_string($slug) || preg_match(self::RE_SLUG, $slug) !== 1) {
                        return $field . ' holds a malformed slug';
                    }
                }
            }
        }

        if ($name === self::NAME_CONTENT) {
            if (!isset($props['post_id']) || !is_int($props['post_id']) || $props['post_id'] < 1) {
                return 'post_id must be an integer of at least 1';
            }
            if (array_key_exists('max_bytes', $props)) {
                $max = $props['max_bytes'];
                if (!is_int($max) || $max < 256 || $max > self::TEXT_MAX_BYTES) {
                    return 'max_bytes out of range';
                }
            }
        }

        return null;
    }

    /**
     * Run an own ability. The input has already passed validate().
     *
     * @param string $name  Ability name.
     * @param object $input Validated input.
     * @return array{output?:array<string,mixed>,refusal?:array{code:string,detail:string}}
     */
    public static function run(string $name, object $input): array
    {
        switch ($name) {
            case self::NAME_INVENTORY:
                return ['output' => self::inventory()];
            case self::NAME_FACTS:
                return ['output' => self::siteFacts($input)];
            case self::NAME_CONTENT:
                return self::contentRead($input);
            default:
                return ['refusal' => ['code' => 'ability_unknown', 'detail' => 'not an ability this agent implements']];
        }
    }

    /**
     * Register the abilities with the WordPress Abilities API. Called from the
     * API's own init hooks, so it is a no-op on older WordPress.
     *
     * @return void
     */
    public static function registerCategory(): void
    {
        if (!function_exists('wp_register_ability_category')) {
            return;
        }
        wp_register_ability_category(self::CATEGORY, [
            'label'       => 'Fleet Agent',
            'description' => 'Read-only abilities provided by the Fleet Agent plugin.',
        ]);
    }

    /**
     * @return void
     */
    public static function registerAbilities(): void
    {
        if (!function_exists('wp_register_ability')) {
            return;
        }
        $copy = [
            self::NAME_INVENTORY => ['Ability inventory', 'Lists the abilities registered on this site.'],
            self::NAME_FACTS     => ['Site facts', 'WordPress and PHP versions, the active theme and builder hints.'],
            self::NAME_CONTENT   => ['Read a page', 'Reads the text of one published page or post.'],
        ];
        foreach (self::names() as $name) {
            $registered = strtolower($name);
            if ($registered === '' || $registered === '0') {
                continue;
            }
            wp_register_ability($registered, [
                'label'               => $copy[$name][0],
                'description'         => $copy[$name][1],
                'category'            => self::CATEGORY,
                'input_schema'        => self::inputSchema($name),
                'execute_callback'    => static function ($input = null) use ($name) {
                    $decoded = json_decode((string) json_encode($input === null ? new \stdClass() : $input), false);
                    $obj     = is_object($decoded) ? $decoded : new \stdClass();
                    $bad     = self::validate($name, $obj);
                    if ($bad !== null) {
                        return new \WP_Error('wpmgr_bad_input', $bad);
                    }
                    $out = self::run($name, $obj);
                    if (isset($out['refusal'])) {
                        return new \WP_Error('wpmgr_' . $out['refusal']['code'], $out['refusal']['detail']);
                    }

                    return $out['output'] ?? [];
                },
                'permission_callback' => static fn () => function_exists('current_user_can') && current_user_can('manage_options'),
                'meta'                => ['show_in_rest' => false, 'annotations' => ['readonly' => true]],
            ]);
        }
    }

    // ---------------------------------------------------------------------
    // wpmgr/abilities-inventory
    // ---------------------------------------------------------------------

    /**
     * @return array<string,mixed>
     */
    private static function inventory(): array
    {
        $rows      = [];
        $seen      = [];
        $apiPresent = function_exists('wp_get_abilities');
        $version   = defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : null;

        foreach (self::names() as $name) {
            $seen[$name] = true;
            $rows[]      = [
                'name'                 => $name,
                'owner_kind'           => 'wpmgr',
                'owner_mismatch'       => false,
                'version'              => $version,
                'schema_struct_sha256' => self::schemaHashOfSchema(self::inputSchema($name)),
                'class'                => 'read',
            ];
        }

        $truncated = false;
        if ($apiPresent) {
            try {
                $all = wp_get_abilities();
            } catch (\Throwable $e) {
                $all = [];
            }
            foreach ($all as $ability) {
                if (!is_object($ability) || !method_exists($ability, 'get_name')) {
                    continue;
                }
                $name = $ability->get_name();
                if (!is_string($name) || $name === '' || isset($seen[$name])) {
                    continue;
                }
                if (count($rows) >= self::INVENTORY_MAX) {
                    $truncated = true;
                    break;
                }
                $seen[$name] = true;
                $ns          = strstr($name, '/', true);
                $ns          = $ns === false ? $name : $ns;
                $squat       = $ns === 'wpmgr';
                $rows[]      = [
                    'name'                 => $name,
                    'owner_kind'           => $squat ? 'wpmgr_squat' : ($ns === 'core' ? 'core' : 'site'),
                    'owner_mismatch'       => $squat,
                    'version'              => null,
                    'schema_struct_sha256' => AbilitySchema::hashOf($ability),
                    'from_the_site'        => [
                        'label'       => self::cap(self::text($ability, 'get_label'), self::LABEL_CAP_BYTES),
                        'description' => self::cap(self::text($ability, 'get_description'), self::DESCRIPTION_CAP_BYTES),
                    ],
                ];
            }
        }

        return [
            'api_present' => $apiPresent,
            'count'       => count($rows),
            'truncated'   => $truncated,
            'abilities'   => $rows,
        ];
    }

    /**
     * @param array<string,mixed> $schema Schema.
     * @return string|null
     */
    private static function schemaHashOfSchema(array $schema): ?string
    {
        $json = json_encode(AbilitySchema::structural($schema, '', []));

        return is_string($json) ? 'sha256:' . hash('sha256', $json) : null;
    }

    /**
     * @param object $ability Ability.
     * @param string $method  Getter.
     * @return string
     */
    private static function text(object $ability, string $method): string
    {
        if (!method_exists($ability, $method)) {
            return '';
        }
        try {
            $v = $ability->{$method}();
        } catch (\Throwable $e) {
            return '';
        }

        return is_string($v) ? $v : '';
    }

    // ---------------------------------------------------------------------
    // wpmgr/site-facts
    // ---------------------------------------------------------------------

    /**
     * @param object $input Validated input.
     * @return array<string,mixed>
     */
    private static function siteFacts(object $input): array
    {
        $wp        = AbilityGuards::wpVersion();
        $plugins   = self::activePluginSlugs();
        $template  = function_exists('get_option') ? (string) get_option('template', '') : '';
        $style     = function_exists('get_option') ? (string) get_option('stylesheet', '') : '';
        $themes    = array_values(array_filter([$template, $style], static fn ($s) => $s !== ''));
        $hints     = [];
        $vars      = get_object_vars($input);
        foreach ((array) ($vars['plugin_slugs'] ?? []) as $slug) {
            if (is_string($slug) && in_array($slug, $plugins, true)) {
                $hints[] = 'plugin:' . $slug;
            }
        }
        foreach ((array) ($vars['theme_slugs'] ?? []) as $slug) {
            if (is_string($slug) && in_array($slug, $themes, true)) {
                $hints[] = 'theme:' . $slug;
            }
        }

        return [
            'wp_version'         => $wp,
            'php_version'        => PHP_VERSION,
            'multisite'          => function_exists('is_multisite') && is_multisite(),
            'agent_version'      => defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : null,
            'abilities_api'      => [
                'present'    => function_exists('wp_get_abilities'),
                'filters_71' => version_compare($wp, '7.1', '>='),
            ],
            'active_theme'       => ['template' => $template, 'stylesheet' => $style],
            'active_plugins'     => array_slice($plugins, 0, self::PLUGINS_MAX),
            'builder_hints'      => $hints,
        ];
    }

    /**
     * Directory slugs of active and network-active plugins.
     *
     * @return list<string>
     */
    private static function activePluginSlugs(): array
    {
        $files = [];
        $act   = function_exists('get_option') ? get_option('active_plugins', []) : [];
        if (is_array($act)) {
            $files = array_merge($files, array_filter($act, 'is_string'));
        }
        if (function_exists('is_multisite') && is_multisite() && function_exists('get_site_option')) {
            $net = get_site_option('active_sitewide_plugins', []);
            if (is_array($net)) {
                $files = array_merge($files, array_filter(array_keys($net), 'is_string'));
            }
        }
        $slugs = [];
        foreach ($files as $file) {
            $dir = strstr($file, '/', true);
            $slug = $dir === false ? basename($file, '.php') : $dir;
            if ($slug !== '' && preg_match(self::RE_SLUG, $slug) === 1) {
                $slugs[$slug] = true;
            }
        }
        $out = array_keys($slugs);
        sort($out);

        return array_map('strval', $out);
    }

    // ---------------------------------------------------------------------
    // wpmgr/content-read
    // ---------------------------------------------------------------------

    /**
     * Published, password-free posts and pages only. Every other case answers
     * with the same refusal, so it cannot be used to probe for drafts.
     *
     * @param object $input Validated input.
     * @return array{output?:array<string,mixed>,refusal?:array{code:string,detail:string}}
     */
    private static function contentRead(object $input): array
    {
        $vars = get_object_vars($input);
        $id   = (int) ($vars['post_id'] ?? 0);
        $max  = isset($vars['max_bytes']) ? (int) $vars['max_bytes'] : self::TEXT_DEFAULT_BYTES;

        $post = function_exists('get_post') ? get_post($id) : null;
        $ok   = is_object($post)
            && in_array((string) $post->post_type, ['post', 'page'], true)
            && (string) $post->post_status === 'publish'
            && (string) $post->post_password === '';
        if (!$ok || !is_object($post)) {
            return ['refusal' => ['code' => 'post_not_readable', 'detail' => 'no published, unprotected post or page with that id']];
        }

        $raw  = (string) $post->post_content;
        $text = function_exists('wp_strip_all_tags') ? wp_strip_all_tags($raw) : strip_tags($raw); // phpcs:ignore WordPress.WP.AlternativeFunctions.strip_tags_strip_tags -- fallback only when core's helper is absent; text is returned as data, never printed
        $text = trim((string) preg_replace('/[ \t]*\R{3,}/u', "\n\n", $text));
        $bytes = strlen($text);

        return ['output' => [
            'post_id'       => $id,
            'post_type'     => (string) $post->post_type,
            'status'        => 'publish',
            'modified_gmt'  => (string) $post->post_modified_gmt,
            'text_bytes'    => $bytes,
            'truncated'     => $bytes > $max,
            'from_the_site' => [
                'title' => self::cap((string) $post->post_title, self::TITLE_CAP_BYTES),
                'text'  => self::cap($text, $max),
            ],
        ]];
    }

    /**
     * Cap a string at a byte budget without cutting a UTF-8 sequence.
     *
     * @param string $value Text.
     * @param int    $max   Byte budget.
     * @return string
     */
    private static function cap(string $value, int $max): string
    {
        if (strlen($value) <= $max) {
            return self::validUtf8($value);
        }
        $cut = function_exists('mb_strcut') ? mb_strcut($value, 0, $max, 'UTF-8') : substr($value, 0, $max);

        return self::validUtf8((string) $cut);
    }

    /**
     * @param string $value Text.
     * @return string
     */
    private static function validUtf8(string $value): string
    {
        if (preg_match('//u', $value) === 1) {
            return $value;
        }
        $clean = @iconv('UTF-8', 'UTF-8//IGNORE', $value);

        return is_string($clean) ? $clean : '';
    }
}
