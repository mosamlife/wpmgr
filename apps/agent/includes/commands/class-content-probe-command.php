<?php

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

// Direct-file-access guard: keep above the docblock (see the note in
// class-content-update-command.php).
if (!defined('ABSPATH')) {
    exit;
}

/**
 * ContentProbeCommand: a READ-ONLY check of who owns the content of a page, and
 * therefore which write route, if any, may touch it.
 *
 * It answers one question for the control plane: is `post_content` the thing a
 * visitor sees for this page? For a classic page it is. For a block document,
 * a page built by a page builder, a page whose template may override the
 * column, or a special page (the blog index, a shop page) it is not, and
 * writing the column would "succeed" invisibly.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/content_probe
 *
 * Single mode:  { "post_id": <int >= 1>, ... }
 * List mode:    { "list": { "types": [..], "status": [..], "limit": 1..200,
 *                            "offset": >= 0 }, ... }
 * Common:       "allowed_post_types" (default post + page), "descriptors"
 *               (at most 32, may be empty), "indicators" ({plugin_slugs,
 *               theme_slugs, meta_key_prefixes}).
 *
 * The route decision is computed from the descriptors the control plane sends
 * and from nothing else: no builder is named in this file's logic. A
 * descriptor is DATA (identifiers matching fixed regexes); no callable, class
 * name or method name is ever accepted, evaluated or invoked, and no ability
 * is ever executed.
 *
 * Output carries presence, sizes and fingerprints only. It never returns
 * content bodies, meta values or layout; the one exception is list mode, which
 * returns the title of PUBLISHED rows, capped at 120 bytes.
 *
 * Route 2 (a vendor ability that stages a draft) is not decided here yet: no
 * descriptor can admit a page in this slice, so every page resolves to route 1
 * or route 3.
 *
 * @package WPMgr\Agent\Commands
 */

/**
 * Read-only page ownership probe.
 */
final class ContentProbeCommand implements CommandInterface
{
    private const PROBE_VERSION = 2;

    /** @var list<string> */
    private const DEFAULT_ALLOWED_POST_TYPES = ['post', 'page'];

    private const MAX_DESCRIPTORS = 32;

    private const MAX_LIST_LIMIT = 200;

    private const TITLE_CAP_BYTES = 120;

    private const DISPLAY_CAP_BYTES = 64;

    /** Most bytes the probe will fingerprint for one page. */
    private const PROBE_LIMIT_BYTES = 8388608;

    /** Statuses list mode may ask for. */
    private const LIST_STATUSES = ['publish', 'draft', 'pending', 'private', 'future'];

    /** Same domain as ContentUpdateCommand, so content_v1 is interchangeable. */
    private const CONTENT_FINGERPRINT_DOMAIN = 'wpmgr.content_update.v1';

    private const DOCUMENT_DOMAIN = 'wpmgr.content_probe.document.v1';

    private const LIVE_DOMAIN = 'wpmgr.content_probe.live.v1';

    /** A version constant is named for a version; nothing else is read. */
    private const RE_CONSTANT   = '/^[A-Z][A-Z0-9_]{1,55}_VERSION$/';
    /** Credential-shaped names are refused even when they end in _VERSION. */
    private const RE_CONSTANT_DENY = '/(^DB_|KEY|SALT|PASSWORD|SECRET|TOKEN)/';
    private const RE_META_KEY   = '/^[A-Za-z0-9_\-]{1,128}$/';
    private const RE_ABILITY    = '/^[a-z0-9-]+\/[a-z0-9-]+$/';
    private const RE_NAMESPACE  = '/^[a-z0-9-]{1,64}$/';
    private const RE_ID         = '/^[a-z0-9_-]{1,64}$/';
    private const RE_SLUG       = '/^[A-Za-z0-9._-]{1,100}$/';
    private const RE_POST_TYPE  = '/^[a-z0-9_-]{1,20}$/';
    private const RE_VERSION    = '/^\d{1,4}(\.\d{1,4}){0,3}([-+][A-Za-z0-9.]{1,32})?$/';
    private const RE_ENUM_PATH  = '/^[A-Za-z0-9_.\-]{1,128}$/';

    /** @var list<string> */
    private const DESCRIPTOR_KEYS = [
        'integration_id', 'status', 'verified', 'enabled', 'namespace', 'version_constant',
        'version_ability', 'mode_flag', 'payload_keys', 'draft_keys', 'shortcode_prefixes',
        'special_page_options', 'singular_override', 'override_check', 'plugin_dir',
        'ability_names', 'dynamic_enum_paths',
    ];

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'content_probe';
    }

    /**
     * Effect: reads posts, options and post meta; writes nothing and executes
     * no ability. The one side effect is core's own cache invalidation for the
     * post being read, which is what makes the read authoritative.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Read;
    }

    /**
     * Repeatability: idempotent. A repeat returns the same facts for the same
     * site state and changes nothing.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Idempotent;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        try {
            return $this->run($params);
        } catch (\Throwable $e) {
            return $this->fail('internal', 'the probe failed unexpectedly; it is safe to retry', true);
        }
    }

    /**
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed>
     */
    private function run(array $params): array
    {
        $cfg = $this->parse($params);
        if (isset($cfg['refusal'])) {
            return $cfg['refusal'];
        }

        $site = $this->siteContext($cfg['descriptors'], $cfg['indicators']);

        if ($cfg['mode'] === 'list') {
            return $this->runList($cfg, $site);
        }

        return $this->runSingle($cfg, $site);
    }

    // ---------------------------------------------------------------------
    // Input
    // ---------------------------------------------------------------------

    /**
     * Validates and normalises the request. Returns ['refusal' => ...] on any
     * violation.
     *
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed>
     */
    private function parse(array $params): array
    {
        $known = ['post_id', 'list', 'allowed_post_types', 'descriptors', 'indicators'];
        foreach (array_keys($params) as $key) {
            if (!in_array($key, $known, true)) {
                return $this->refuse('invalid_params', 'unknown parameter');
            }
        }

        $hasPost = array_key_exists('post_id', $params);
        $hasList = array_key_exists('list', $params);
        if ($hasPost === $hasList) {
            return $this->refuse('invalid_params', 'provide exactly one of post_id or list');
        }

        $allowed = self::DEFAULT_ALLOWED_POST_TYPES;
        if (array_key_exists('allowed_post_types', $params)) {
            $allowed = $this->stringList($params['allowed_post_types'], self::RE_POST_TYPE, 20, 1);
            if ($allowed === null) {
                return $this->refuse('invalid_params', 'allowed_post_types must be a non-empty list of post type names');
            }
        }

        $rawDescriptors = $params['descriptors'] ?? [];
        if (!is_array($rawDescriptors) || !array_is_list($rawDescriptors)) {
            return $this->refuse('invalid_params', 'descriptors must be a list');
        }
        if (count($rawDescriptors) > self::MAX_DESCRIPTORS) {
            return $this->refuse('too_many_descriptors', 'at most ' . self::MAX_DESCRIPTORS . ' descriptors are accepted');
        }
        $descriptors = [];
        $seenIds     = [];
        foreach ($rawDescriptors as $i => $raw) {
            $norm = $this->normaliseDescriptor($raw);
            if (is_string($norm)) {
                return $this->refuse(
                    'invalid_descriptor',
                    'descriptor[' . $i . ']: ' . $norm,
                    ['index' => $i]
                );
            }
            if (isset($seenIds[$norm['integration_id']])) {
                return $this->refuse('invalid_descriptor', 'descriptor[' . $i . ']: duplicate integration_id', ['index' => $i]);
            }
            $seenIds[$norm['integration_id']] = true;
            $descriptors[]                    = $norm;
        }

        $indicators = ['plugin_slugs' => [], 'theme_slugs' => [], 'meta_key_prefixes' => []];
        if (array_key_exists('indicators', $params)) {
            $raw = $params['indicators'];
            if (!is_array($raw)) {
                return $this->refuse('invalid_params', 'indicators must be an object');
            }
            $regexes = [
                'plugin_slugs'      => self::RE_SLUG,
                'theme_slugs'       => self::RE_SLUG,
                'meta_key_prefixes' => self::RE_META_KEY,
            ];
            foreach ($raw as $key => $value) {
                if (!isset($regexes[$key])) {
                    return $this->refuse('invalid_params', 'unknown indicators key');
                }
                $list = $this->stringList($value, $regexes[$key], 32, 0);
                if ($list === null) {
                    return $this->refuse('invalid_params', 'indicators.' . $key . ' is invalid');
                }
                $indicators[$key] = $list;
            }
        }

        $cfg = [
            'allowed'     => $allowed,
            'descriptors' => $descriptors,
            'indicators'  => $indicators,
            'mode'        => $hasList ? 'list' : 'single',
        ];

        if ($hasPost) {
            $id = $params['post_id'];
            if (!is_int($id) || $id < 1) {
                return $this->refuse('invalid_params', 'post_id must be an integer of at least 1');
            }
            $cfg['post_id'] = $id;

            return $cfg;
        }

        $list = $params['list'];
        if (!is_array($list) || array_is_list($list) && $list !== []) {
            return $this->refuse('invalid_params', 'list must be an object');
        }
        foreach (array_keys($list) as $key) {
            if (!in_array($key, ['types', 'status', 'limit', 'offset'], true)) {
                return $this->refuse('invalid_params', 'unknown list key');
            }
        }
        $types = $this->stringList($list['types'] ?? $allowed, self::RE_POST_TYPE, 20, 1);
        if ($types === null || array_diff($types, $allowed) !== []) {
            return $this->refuse('invalid_params', 'list.types must be a non-empty subset of allowed_post_types');
        }
        $statuses = $this->stringList($list['status'] ?? ['publish'], '/^[a-z]{1,10}$/', 5, 1);
        if ($statuses === null || array_diff($statuses, self::LIST_STATUSES) !== []) {
            return $this->refuse('invalid_params', 'list.status contains an unsupported status');
        }
        $limit  = $list['limit'] ?? 50;
        $offset = $list['offset'] ?? 0;
        if (!is_int($limit) || $limit < 1 || $limit > self::MAX_LIST_LIMIT) {
            return $this->refuse('invalid_params', 'list.limit must be an integer from 1 to ' . self::MAX_LIST_LIMIT);
        }
        if (!is_int($offset) || $offset < 0) {
            return $this->refuse('invalid_params', 'list.offset must be an integer of at least 0');
        }
        $cfg['list'] = ['types' => $types, 'status' => $statuses, 'limit' => $limit, 'offset' => $offset];

        return $cfg;
    }

    /**
     * @param mixed  $value Candidate list.
     * @param string $regex Each element must match.
     * @param int    $max   Most elements.
     * @param int    $min   Fewest elements.
     * @return list<string>|null
     */
    private function stringList(mixed $value, string $regex, int $max, int $min): ?array
    {
        if (!is_array($value) || !array_is_list($value) || count($value) > $max || count($value) < $min) {
            return null;
        }
        foreach ($value as $item) {
            if (!is_string($item) || preg_match($regex, $item) !== 1) {
                return null;
            }
        }

        return array_values(array_unique($value));
    }

    /**
     * Validates one descriptor. Returns the normalised descriptor, or a short
     * reason string (which never echoes more than 64 bytes of the input).
     *
     * @param mixed $raw Candidate descriptor.
     * @return array<string,mixed>|string
     */
    private function normaliseDescriptor(mixed $raw): array|string
    {
        if (!is_array($raw) || array_is_list($raw) && $raw !== []) {
            return 'must be an object';
        }
        foreach (array_keys($raw) as $key) {
            if (!in_array($key, self::DESCRIPTOR_KEYS, true)) {
                return 'unknown field ' . $this->echoCap((string) $key);
            }
        }

        $id = $raw['integration_id'] ?? null;
        if (!is_string($id) || preg_match(self::RE_ID, $id) !== 1) {
            return 'invalid integration_id';
        }

        $status = $raw['status'] ?? 'detect_only';
        if ($status !== 'detect_only' && $status !== 'admitted') {
            return 'invalid status';
        }
        foreach (['verified', 'enabled'] as $flag) {
            if (array_key_exists($flag, $raw) && !is_bool($raw[$flag])) {
                return 'invalid ' . $flag;
            }
        }

        $namespace = $raw['namespace'] ?? null;
        if ($namespace !== null && (!is_string($namespace) || preg_match(self::RE_NAMESPACE, $namespace) !== 1)) {
            return 'invalid namespace';
        }
        $constant = $raw['version_constant'] ?? null;
        if ($constant !== null && (!is_string($constant) || preg_match(self::RE_CONSTANT, $constant) !== 1 || preg_match(self::RE_CONSTANT_DENY, $constant) === 1)) {
            return 'invalid version_constant';
        }
        $versionAbility = $raw['version_ability'] ?? null;
        if ($versionAbility !== null && (!is_string($versionAbility) || preg_match(self::RE_ABILITY, $versionAbility) !== 1)) {
            return 'invalid version_ability';
        }

        $flagSpec = $raw['mode_flag'] ?? null;
        if (!is_array($flagSpec) || array_is_list($flagSpec) || array_diff(array_keys($flagSpec), ['meta_key', 'on_values']) !== []) {
            return 'invalid mode_flag';
        }
        $flagKey = $flagSpec['meta_key'] ?? null;
        if (!is_string($flagKey) || preg_match(self::RE_META_KEY, $flagKey) !== 1) {
            return 'invalid mode_flag.meta_key';
        }
        $on = $flagSpec['on_values'] ?? null;
        if (!is_array($on) || !array_is_list($on) || $on === [] || count($on) > 8) {
            return 'invalid mode_flag.on_values';
        }
        foreach ($on as $value) {
            if (!is_string($value) || $value === '' || strlen($value) > 64) {
                return 'invalid mode_flag.on_values';
            }
        }

        $lists = [
            'payload_keys'         => [self::RE_META_KEY, 16, 1],
            'draft_keys'           => [self::RE_META_KEY, 16, 0],
            'shortcode_prefixes'   => [self::RE_ID, 8, 0],
            'special_page_options' => [self::RE_META_KEY, 8, 0],
            'ability_names'        => [self::RE_ABILITY, 16, 0],
            'dynamic_enum_paths'   => [self::RE_ENUM_PATH, 16, 0],
        ];
        /** @var array<string,list<string>> $out */
        $out = [];
        foreach ($lists as $field => [$regex, $max, $min]) {
            $list = $this->stringList($raw[$field] ?? [], $regex, $max, $min);
            if ($list === null) {
                return 'invalid ' . $field;
            }
            $out[$field] = $list;
        }

        $override = $raw['singular_override'] ?? 'unknown';
        if (!in_array($override, ['none', 'detectable', 'unknown'], true)) {
            return 'invalid singular_override';
        }
        // No override check can be expressed as data in this slice, and a
        // callable is never accepted: the field must be null.
        if (($raw['override_check'] ?? null) !== null) {
            return 'override_check must be null';
        }

        $dir = $raw['plugin_dir'] ?? null;
        if ($dir !== null && (!is_string($dir) || preg_match(self::RE_SLUG, $dir) !== 1)) {
            return 'invalid plugin_dir';
        }

        return [
            'integration_id'       => $id,
            'status'               => $status,
            'verified'             => ($raw['verified'] ?? false) === true,
            'enabled'              => ($raw['enabled'] ?? true) === true,
            'namespace'            => $namespace,
            'version_constant'     => $constant,
            'version_ability'      => $versionAbility,
            'flag_key'             => $flagKey,
            'flag_on'              => $on,
            'payload_keys'         => $out['payload_keys'],
            'draft_keys'           => $out['draft_keys'],
            'shortcode_prefixes'   => $out['shortcode_prefixes'],
            'special_page_options' => $out['special_page_options'],
            'ability_names'        => $out['ability_names'],
            'dynamic_enum_paths'   => $out['dynamic_enum_paths'],
            'singular_override'    => $override,
            'plugin_dir'           => $dir,
        ];
    }

    // ---------------------------------------------------------------------
    // Site-level facts (once per request)
    // ---------------------------------------------------------------------

    /**
     * @param list<array<string,mixed>> $descriptors Normalised descriptors.
     * @param array<string,list<string>> $indicators Indicator lists.
     * @return array<string,mixed>
     */
    private function siteContext(array $descriptors, array $indicators): array
    {
        $registry   = $this->abilityRegistry();
        $namespaces = [];
        foreach (array_keys($registry['abilities']) as $name) {
            $ns              = strstr($name, '/', true);
            $ns              = $ns === false ? $name : $ns;
            $namespaces[$ns] = ($namespaces[$ns] ?? 0) + 1;
        }

        $names = [];
        foreach ($descriptors as $d) {
            foreach ($d['ability_names'] as $n) {
                $names[$n] = true;
            }
            if ($d['version_ability'] !== null) {
                $names[$d['version_ability']] = true;
            }
        }
        $allowlisted = [];
        foreach (array_keys($names) as $n) {
            $ability       = $registry['abilities'][$n] ?? null;
            $allowlisted[] = [
                'name'                => $n,
                'registered'          => $ability !== null,
                'schema_struct_sha256' => null,
                // Callback ownership is evaluated when a descriptor can be
                // admitted; until then it is never reported as satisfied.
                'owner_ok'            => false,
                '_ability'            => $ability,
            ];
        }
        /** @var array<string,list<string>> $dynamicPaths */
        $dynamicPaths = [];
        foreach ($descriptors as $d) {
            foreach ($d['ability_names'] as $n) {
                $dynamicPaths[$n] = array_values(array_merge($dynamicPaths[$n] ?? [], $d['dynamic_enum_paths']));
            }
        }
        foreach ($allowlisted as $i => $row) {
            if ($row['_ability'] !== null) {
                $allowlisted[$i]['schema_struct_sha256'] = $this->schemaHash($row['_ability'], $dynamicPaths[$row['name']] ?? []);
            }
            unset($allowlisted[$i]['_ability']);
        }

        $live = [];
        foreach ($descriptors as $d) {
            if (!$d['enabled']) {
                $live[$d['integration_id']] = ['live' => false, 'version' => null, 'version_source' => null];
                continue;
            }
            $abilitySignal = ($d['namespace'] !== null && isset($namespaces[$d['namespace']]));
            foreach ($d['ability_names'] as $n) {
                $abilitySignal = $abilitySignal || isset($registry['abilities'][$n]);
            }
            if ($d['version_ability'] !== null && isset($registry['abilities'][$d['version_ability']])) {
                $abilitySignal = true;
            }

            $version       = null;
            $versionSource = null;
            $constantSet   = false;
            if ($d['version_constant'] !== null && defined($d['version_constant'])) {
                $constantSet = true;
                $raw         = constant($d['version_constant']);
                if (is_string($raw) && preg_match(self::RE_VERSION, $raw) === 1) {
                    $version       = $raw;
                    $versionSource = 'constant';
                }
            }
            $live[$d['integration_id']] = [
                'live'           => $abilitySignal || $constantSet,
                'version'        => $version,
                'version_source' => $versionSource,
            ];
        }

        $activePlugins = $this->activePluginSlugs();
        $activeThemes  = array_values(array_filter([
            (string) get_option('template', ''),
            (string) get_option('stylesheet', ''),
        ], static fn ($s) => $s !== ''));

        $hints = [];
        foreach ($indicators['plugin_slugs'] as $slug) {
            if (in_array($slug, $activePlugins, true)) {
                $hints[] = 'plugin:' . $slug;
            }
        }
        foreach ($indicators['theme_slugs'] as $slug) {
            if (in_array($slug, $activeThemes, true)) {
                $hints[] = 'theme:' . $slug;
            }
        }

        $wpVersion = (string) get_bloginfo('version');
        ksort($namespaces);
        $nsRows = [];
        foreach (array_slice($namespaces, 0, 64, true) as $name => $count) {
            $nsRows[] = ['name' => (string) $name, 'count' => $count];
        }

        return [
            'live'          => $live,
            'site_hints'    => $hints,
            'active_plugins' => $activePlugins,
            'abilities'     => [
                'api_present' => $registry['present'],
                'namespaces'  => $nsRows,
                'allowlisted' => $allowlisted,
                'filters_71'  => $registry['present'] && version_compare($wpVersion, '7.1', '>='),
            ],
            'meta_prefixes' => $indicators['meta_key_prefixes'],
            'wp_version'    => $wpVersion,
        ];
    }

    /**
     * Registered abilities, when the API exists. Only names and input schemas
     * are read; an ability is never executed.
     *
     * @return array{present:bool,abilities:array<string,object>}
     */
    private function abilityRegistry(): array
    {
        if (!function_exists('wp_get_abilities')) {
            return ['present' => false, 'abilities' => []];
        }

        $found = [];
        try {
            $all = wp_get_abilities();
            if ($all !== []) {
                foreach ($all as $ability) {
                    if (is_object($ability) && method_exists($ability, 'get_name')) {
                        $name = $ability->get_name();
                        if (is_string($name) && $name !== '') {
                            $found[$name] = $ability;
                        }
                    }
                }
            }
        } catch (\Throwable $e) {
            return ['present' => true, 'abilities' => []];
        }

        return ['present' => true, 'abilities' => $found];
    }

    /**
     * Structural hash of an ability's input schema: type, property names
     * (recursively), required, items, additionalProperties and enum values,
     * with enum dropped at dynamic paths. Titles, descriptions, examples,
     * defaults and translated text never contribute, and keys are sorted, so a
     * locale change or a newly registered element type does not change the
     * hash but a real change of shape does.
     *
     * @param object       $ability Registered ability.
     * @param list<string> $dynamic Dotted property paths whose enum is dropped.
     * @return string|null
     */
    private function schemaHash(object $ability, array $dynamic): ?string
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
        $json = json_encode($this->structural($schema, '', $dynamic));

        return is_string($json) ? 'sha256:' . hash('sha256', $json) : null;
    }

    /**
     * @param array<mixed> $node    Schema node.
     * @param string       $path    Dotted property path of this node.
     * @param list<string> $dynamic Dynamic enum paths.
     * @return array<mixed>
     */
    private function structural(array $node, string $path, array $dynamic): array
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
                $props[(string) $key] = is_array($child) ? $this->structural($child, $childPath, $dynamic) : [];
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
            $out['items'] = $this->structural($node['items'], $path, $dynamic);
        }
        if (array_key_exists('additionalProperties', $node)) {
            $out['additionalProperties'] = is_array($node['additionalProperties'])
                ? $this->structural($node['additionalProperties'], $path, $dynamic)
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

    /**
     * Directory slugs of active and network-active plugins.
     *
     * @return list<string>
     */
    private function activePluginSlugs(): array
    {
        $files = [];
        $act   = get_option('active_plugins', []);
        if (is_array($act)) {
            $files = array_merge($files, array_filter($act, 'is_string'));
        }
        if (function_exists('is_multisite') && is_multisite()) {
            $net = get_site_option('active_sitewide_plugins', []);
            if (is_array($net)) {
                $files = array_merge($files, array_map('strval', array_keys($net)));
            }
        }
        $slugs = [];
        foreach ($files as $file) {
            $slash   = strpos($file, '/');
            $slugs[] = $slash === false ? (string) preg_replace('/\.php$/', '', $file) : substr($file, 0, $slash);
        }

        return array_values(array_unique($slugs));
    }

    // ---------------------------------------------------------------------
    // Modes
    // ---------------------------------------------------------------------

    /**
     * @param array<string,mixed> $cfg  Parsed request.
     * @param array<string,mixed> $site Site context.
     * @return array<string,mixed>
     */
    private function runSingle(array $cfg, array $site): array
    {
        $post = $this->readStored($cfg['post_id']);
        if ($post === null) {
            return $this->fail('post_not_found', 'post not found: ' . $cfg['post_id']);
        }
        $refusal = $this->postRefusal($post, $cfg['allowed']);
        if ($refusal !== null) {
            return $refusal;
        }

        $eval = $this->evaluate($post, $cfg['descriptors'], $site);
        if ($eval['bytes'] > self::PROBE_LIMIT_BYTES) {
            return $this->fail('probe_limit', 'the page is too large to fingerprint');
        }

        $id       = (int) $post->ID;
        $title    = $this->title($post);
        $content  = $this->content($post);
        $revision = $this->newestRevisionId($post);
        $contentV1 = $this->contentFingerprint($title, $content);

        $modified = (string) $post->post_modified_gmt;
        $matchRaw = $eval['match_raw'];
        $liveParts = [$contentV1];
        $docParts  = [$contentV1];
        foreach ($matchRaw as $m) {
            foreach ($m['payload'] as $k => $v) {
                $liveParts[] = $this->frame($k, $v);
                $docParts[]  = $this->frame($k, $v);
            }
            $docParts[] = $this->frame($m['flag_key'], $m['flag_raw']);
            foreach ($m['draft'] as $k => $v) {
                $docParts[] = $this->frame($k, $v);
            }
        }
        $docParts[] = 'modified_gmt' . "\n" . $modified . "\n";
        $docParts[] = 'newest_revision_id' . "\n" . ($revision === null ? '-1' : (string) $revision) . "\n";

        $keep = (int) wp_revisions_to_keep($post);

        $owner = null;
        if ($eval['verdict'] === 'builder' && count($eval['matches']) === 1) {
            $mid   = $eval['matches'][0]['integration_id'];
            $owner = [
                'integration_id' => $mid,
                'version'        => $site['live'][$mid]['version'] ?? null,
                'version_source' => $site['live'][$mid]['version_source'] ?? null,
            ];
        }

        return [
            'ok'            => true,
            'probe_version' => self::PROBE_VERSION,
            'post'          => [
                'id'               => $id,
                'type'             => (string) $post->post_type,
                'status'           => (string) $post->post_status,
                'modified_gmt'     => $modified,
                'title_bytes'      => strlen($title),
                'content_bytes'    => strlen($content),
                'has_password'     => (string) $post->post_password !== '',
                'newest_revision_id' => $revision,
                'last_editor'      => $this->lastEditor($id),
            ],
            'verdict'       => $eval['verdict'],
            'owner'         => $owner,
            'matches'       => $eval['matches'],
            'route'         => [
                'number'      => $eval['route'],
                'reason'      => $eval['reason'],
                'ability_set' => null,
            ],
            'fingerprints'  => [
                'content_v1'  => $contentV1,
                'document_v1' => 'sha256:' . hash('sha256', self::DOCUMENT_DOMAIN . "\n" . implode('', $docParts)),
                'live_v1'     => 'sha256:' . hash('sha256', self::LIVE_DOMAIN . "\n" . implode('', $liveParts)),
            ],
            'abilities'     => $site['abilities'],
            'revisions'     => [
                'enabled'  => $keep !== 0,
                'keep'     => $keep,
                'supports' => (bool) post_type_supports((string) $post->post_type, 'revisions'),
            ],
            'lock'          => $this->lockInfo($id),
            'inflight'      => null,
            'hints'         => $eval['hints'],
            'principal'     => ['present' => false, 'caps_exact' => null, 'vendor_access' => null],
            'wp_version'    => $site['wp_version'],
            'php_version'   => PHP_VERSION,
        ];
    }

    /**
     * @param array<string,mixed> $cfg  Parsed request.
     * @param array<string,mixed> $site Site context.
     * @return array<string,mixed>
     */
    private function runList(array $cfg, array $site): array
    {
        $list = $cfg['list'];
        $ids  = get_posts([
            'post_type'        => $list['types'],
            'post_status'      => $list['status'],
            'posts_per_page'   => $list['limit'],
            'offset'           => $list['offset'],
            'orderby'          => 'ID',
            'order'            => 'ASC',
            'fields'           => 'ids',
            'no_found_rows'    => true,
            'suppress_filters' => true,
        ]);

        $rows = [];
        foreach ($ids as $rawId) {
            $post = $this->readStored((int) $rawId);
            if ($post === null || $this->postRefusal($post, $cfg['allowed']) !== null) {
                continue;
            }
            $eval = $this->evaluate($post, $cfg['descriptors'], $site);
            $row  = [
                'post'    => (int) $post->ID,
                'type'    => (string) $post->post_type,
                'status'  => (string) $post->post_status,
                'verdict' => $eval['verdict'],
                'route'   => ['number' => $eval['route'], 'reason' => $eval['reason']],
                'owner'   => null,
                'title'   => null,
            ];
            if ($eval['bytes'] > self::PROBE_LIMIT_BYTES) {
                $row['verdict'] = 'unrecognised_builder';
                $row['route']   = ['number' => 3, 'reason' => 'unrecognised_builder'];
            }
            if ($eval['verdict'] === 'builder' && count($eval['matches']) === 1) {
                $mid         = $eval['matches'][0]['integration_id'];
                $row['owner'] = [
                    'integration_id' => $mid,
                    'version'        => $site['live'][$mid]['version'] ?? null,
                ];
            }
            // Titles leave the site for published rows only.
            if ((string) $post->post_status === 'publish') {
                $row['title'] = $this->capBytes($this->title($post), self::TITLE_CAP_BYTES);
            }
            $rows[] = $row;
        }

        return [
            'ok'            => true,
            'probe_version' => self::PROBE_VERSION,
            'mode'          => 'list',
            'rows'          => $rows,
            'next_offset'   => count($ids) >= $list['limit'] ? $list['offset'] + $list['limit'] : null,
            'abilities'     => ['api_present' => $site['abilities']['api_present']],
            'wp_version'    => $site['wp_version'],
            'php_version'   => PHP_VERSION,
        ];
    }

    // ---------------------------------------------------------------------
    // The decision
    // ---------------------------------------------------------------------

    /**
     * Refusals for a post that cannot be probed, or null.
     *
     * @param \WP_Post       $post    Post.
     * @param list<string> $allowed Allowed post types.
     * @return array<string,mixed>|null
     */
    private function postRefusal(object $post, array $allowed): ?array
    {
        $id = (int) $post->ID;
        if ((string) $post->post_status === 'trash') {
            return $this->fail('post_in_trash', 'post ' . $id . ' is in the trash');
        }
        $type = (string) $post->post_type;
        if (!in_array($type, $allowed, true)) {
            return $this->fail('post_type_not_allowed', 'post ' . $id . ' is of a type this request did not allow');
        }
        if (!post_type_supports($type, 'editor')) {
            return $this->fail('post_type_not_content', 'post ' . $id . ' is not an editable content type');
        }

        return null;
    }

    /**
     * Verdict, matches and route for one post, from the descriptors alone.
     *
     * @param \WP_Post                    $post        Post.
     * @param list<array<string,mixed>> $descriptors Normalised descriptors.
     * @param array<string,mixed>       $site        Site context.
     * @return array<string,mixed>
     */
    private function evaluate(object $post, array $descriptors, array $site): array
    {
        $id      = (int) $post->ID;
        $content = $this->content($post);
        $bytes   = strlen($this->title($post)) + strlen($content);
        $budget  = self::PROBE_LIMIT_BYTES - $bytes;

        $matches  = [];
        $matchRaw = [];
        $claimed  = [];
        foreach ($descriptors as $d) {
            if (!$d['enabled']) {
                continue;
            }
            $claimed[$d['flag_key']] = true;
            foreach (array_merge($d['payload_keys'], $d['draft_keys']) as $k) {
                $claimed[$k] = true;
            }
        }

        try {
        foreach ($descriptors as $d) {
            if (!$d['enabled'] || !($site['live'][$d['integration_id']]['live'] ?? false)) {
                continue;
            }
            $flagRaw  = $this->metaBytes($id, $d['flag_key'], $budget);
            $flagOn   = $flagRaw !== null && in_array($flagRaw, $d['flag_on'], true);
            $payload  = [];
            $payloadBytes = 0;
            $payloadPresent = false;
            foreach ($d['payload_keys'] as $k) {
                $payload[$k]   = $this->metaBytes($id, $k, $budget);
                $bytes        += strlen((string) $payload[$k]);
                $payloadBytes += strlen((string) $payload[$k]);
                $payloadPresent = $payloadPresent || $this->nonEmptyPayload($payload[$k]);
            }
            $draft = [];
            $draftPresent = false;
            foreach ($d['draft_keys'] as $k) {
                $draft[$k]     = $this->metaBytes($id, $k, $budget);
                $bytes        += strlen((string) $draft[$k]);
                $draftPresent = $draftPresent || $this->nonEmptyPayload($draft[$k]);
            }

            if (!$flagOn && !$payloadPresent) {
                continue;
            }
            $evidence = $flagOn && $payloadPresent ? 'match' : ($flagOn ? 'flag_without_payload' : 'stale_payload');

            $draftEqualsLive = null;
            if ($draftPresent && count($d['draft_keys']) === count($d['payload_keys'])) {
                $draftEqualsLive = array_values($draft) === array_values($payload);
            }

            $matches[]  = [
                'integration_id'   => $d['integration_id'],
                'mode_flag'        => $flagOn && $payloadPresent,
                'payload_present'  => $payloadPresent,
                'payload_bytes'    => $payloadBytes,
                'draft_present'    => $draftPresent,
                'draft_equals_live' => $draftEqualsLive,
                'evidence'         => $evidence,
            ];
            $matchRaw[] = ['flag_key' => $d['flag_key'], 'flag_raw' => $flagRaw, 'payload' => $payload, 'draft' => $draft];
        }
        } catch (\LengthException $e) {
            // Over the per-probe read budget: nothing more is loaded.
            return [
                'matches' => [], 'match_raw' => [], 'hints' => [], 'bytes' => self::PROBE_LIMIT_BYTES + 1,
            ] + $this->verdict('unrecognised_builder', 3, 'unrecognised_builder');
        }

        // Unclaimed meta-prefix hints need the post's own meta keys.
        $hints = $site['site_hints'];
        $metaKeys = $this->metaKeys($id);
        foreach ($site['meta_prefixes'] as $prefix) {
            foreach ($metaKeys as $key) {
                if (str_starts_with($key, $prefix) && !isset($claimed[$key])) {
                    $hints[] = 'meta_prefix:' . $prefix;
                    break;
                }
            }
        }

        $base = [
            'matches'   => $matches,
            'match_raw' => $matchRaw,
            'hints'     => array_slice($hints, 0, 64),
            'bytes'     => $bytes,
        ];

        // 1. Special pages: they render something other than post_content.
        if ($this->isSpecialPage($id, $descriptors)) {
            return $base + $this->verdict('special_page', 3, 'special_page');
        }

        // 2. Ownership by a descriptor.
        $real = array_values(array_filter($matches, static fn ($m) => $m['evidence'] === 'match'));
        if (count($real) === 1 && count($matches) === 1) {
            return $base + $this->verdict('builder', 3, 'builder_not_supported');
        }
        if ($matches !== []) {
            return $base + $this->verdict('ambiguous', 3, 'ambiguous_owner');
        }

        // 3. The content column.
        if (has_blocks($content)) {
            return $base + $this->verdict('block_document', 3, 'block_editor_unsupported');
        }
        foreach ($descriptors as $d) {
            if (!$d['enabled']) {
                continue;
            }
            foreach ($d['shortcode_prefixes'] as $prefix) {
                if (preg_match('/\[' . preg_quote($prefix, '/') . '/', $content) === 1) {
                    return $base + $this->verdict('ambiguous', 3, 'ambiguous_owner');
                }
            }
        }
        if ($content === '') {
            return $base + $this->verdict('empty', 3, 'empty_page');
        }

        // 4. A site-level hint blocks route 1 unless every hinted builder has a
        //    live, verified descriptor with negative page signals (a) and
        //    declares no singular override (b).
        if ($hints !== []) {
            $blocked = $this->hintVerdict($hints, $descriptors, $site);
            if ($blocked !== null) {
                return $base + $this->verdict($blocked, 3, $blocked);
            }
        }

        return $base + $this->verdict('classic', 1, 'content_column');
    }

    /**
     * @param string $verdict Verdict.
     * @param int    $route   Route number.
     * @param string $reason  Closed reason code.
     * @return array{verdict:string,route:int,reason:string}
     */
    private function verdict(string $verdict, int $route, string $reason): array
    {
        return ['verdict' => $verdict, 'route' => $route, 'reason' => $reason];
    }

    /**
     * @param list<string>              $hints       Hints found for the post.
     * @param list<array<string,mixed>> $descriptors Normalised descriptors.
     * @param array<string,mixed>       $site        Site context.
     * @return string|null 'unrecognised_builder', 'template_may_override' or null when route 1 stands.
     */
    private function hintVerdict(array $hints, array $descriptors, array $site): ?string
    {
        $needsOverrideCheck = false;
        foreach ($hints as $hint) {
            $covering = null;
            if (str_starts_with($hint, 'plugin:')) {
                $slug = substr($hint, 7);
                foreach ($descriptors as $d) {
                    if ($d['enabled'] && $d['plugin_dir'] === $slug) {
                        $covering = $d;
                        break;
                    }
                }
            }
            if (
                $covering === null
                || !$covering['verified']
                || !($site['live'][$covering['integration_id']]['live'] ?? false)
            ) {
                return 'unrecognised_builder';
            }
            if ($covering['singular_override'] !== 'none') {
                // "detectable" needs an override check, and none can be
                // expressed as data yet, so it is treated like "unknown".
                $needsOverrideCheck = true;
            }
        }

        return $needsOverrideCheck ? 'template_may_override' : null;
    }

    /**
     * @param int                       $id          Post ID.
     * @param list<array<string,mixed>> $descriptors Normalised descriptors.
     * @return bool
     */
    private function isSpecialPage(int $id, array $descriptors): bool
    {
        if ((int) get_option('page_for_posts', 0) === $id) {
            return true;
        }
        if ((string) get_option('show_on_front', 'posts') === 'posts' && (int) get_option('page_on_front', 0) === $id) {
            return true;
        }
        foreach ($descriptors as $d) {
            if (!$d['enabled']) {
                continue;
            }
            foreach ($d['special_page_options'] as $option) {
                $value = get_option($option, 0);
                if ((is_int($value) || (is_string($value) && ctype_digit($value))) && (int) $value === $id) {
                    return true;
                }
            }
        }

        return false;
    }

    // ---------------------------------------------------------------------
    // Reads
    // ---------------------------------------------------------------------

    /**
     * Cache-cleaned post read.
     *
     * @param int $postId Post ID.
     * @return \WP_Post|null
     */
    private function readStored(int $postId): ?object
    {
        clean_post_cache($postId);
        $post = get_post($postId);
        if (!is_object($post) || (int) $post->ID !== $postId) {
            return null;
        }

        return $post;
    }

    /**
     * Byte length of a stored meta value via a length-only query, or null when
     * the database handle is unavailable.
     *
     * @param int    $id  Post ID.
     * @param string $key Meta key.
     * @return int|null
     */
    private function storedLength(int $id, string $key): ?int
    {
        global $wpdb;
        if (!is_object($wpdb) || !method_exists($wpdb, 'get_var') || !method_exists($wpdb, 'prepare') || !isset($wpdb->postmeta)) {
            return null;
        }
        $len = $wpdb->get_var($wpdb->prepare("SELECT MAX(LENGTH(meta_value)) FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s", $id, $key)); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- length-only read so a large value is never loaded; table name from core
        return is_numeric($len) ? (int) $len : null;
    }

    /**
     * Stored bytes of one meta key: null when the key is absent, the string as
     * stored otherwise. Non-string values are serialised deterministically.
     *
     * @param int    $id  Post ID.
     * @param string $key Meta key.
     * @return string|null
     */
    private function metaBytes(int $id, string $key, int &$budget): ?string
    {
        if (!metadata_exists('post', $id, $key)) {
            return null;
        }
        // Size is checked before the value is loaded.
        $length = $this->storedLength($id, $key);
        if ($length !== null) {
            $budget -= $length;
            if ($budget < 0) {
                throw new \LengthException('probe read budget exceeded');
            }
        }
        $value = get_post_meta($id, $key, true);
        if (is_string($value)) {
            return $value;
        }
        if (is_scalar($value)) {
            return (string) $value;
        }

        return serialize($value);
    }

    /**
     * @param string|null $bytes Stored bytes.
     * @return bool
     */
    private function nonEmptyPayload(?string $bytes): bool
    {
        return $bytes !== null && !in_array(trim($bytes), ['', '[]', '{}', 'null', 'a:0:{}'], true);
    }

    /**
     * @param int $id Post ID.
     * @return list<string>
     */
    private function metaKeys(int $id): array
    {
        $all = get_post_meta($id);
        if (!is_array($all)) {
            return [];
        }

        return array_map('strval', array_keys($all));
    }

    /**
     * Newest genuine revision ID, autosaves excluded.
     *
     * @param \WP_Post $post Post.
     * @return int|null
     */
    private function newestRevisionId(object $post): ?int
    {
        $revisions = wp_get_post_revisions((int) $post->ID);
        if (!is_array($revisions)) {
            return null;
        }
        foreach ($revisions as $revision) {
            if (!is_object($revision)) {
                continue;
            }
            $parent = (int) $revision->post_parent;
            if (str_contains((string) $revision->post_name, $parent . '-revision')) {
                return (int) $revision->ID;
            }
        }

        return null;
    }

    /**
     * @param int $id Post ID.
     * @return array{user_id:int|null,display:string|null}
     */
    private function lastEditor(int $id): array
    {
        $raw = get_post_meta($id, '_edit_last', true);
        $uid = (is_int($raw) || (is_string($raw) && ctype_digit($raw))) ? (int) $raw : 0;
        if ($uid < 1) {
            return ['user_id' => null, 'display' => null];
        }
        $user = get_userdata($uid);
        $name = is_object($user) && isset($user->display_name) ? (string) $user->display_name : null;

        return ['user_id' => $uid, 'display' => $name === null ? null : $this->capBytes($name, self::DISPLAY_CAP_BYTES)];
    }

    /**
     * Core's editor lock, read from post meta (the admin helper is not loaded
     * in a REST request).
     *
     * @param int $id Post ID.
     * @return array<string,mixed>
     */
    private function lockInfo(int $id): array
    {
        $none = ['locked' => false, 'by_user_id' => null, 'by_display' => null, 'since_gmt' => null, 'source' => 'core'];
        $raw  = get_post_meta($id, '_edit_lock', true);
        if (!is_string($raw) || preg_match('/^(\d{1,12}):(\d{1,12})$/', $raw, $m) !== 1) {
            return $none;
        }
        $window = (int) apply_filters('wp_check_post_lock_window', 150); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- core's own lock-window filter, read so the reported lock matches core
        if ((int) $m[1] <= time() - $window) {
            return $none;
        }
        $user = get_userdata((int) $m[2]);
        $name = is_object($user) && isset($user->display_name) ? (string) $user->display_name : null;

        return [
            'locked'     => true,
            'by_user_id' => (int) $m[2],
            'by_display' => $name === null ? null : $this->capBytes($name, self::DISPLAY_CAP_BYTES),
            'since_gmt'  => gmdate('Y-m-d H:i:s', (int) $m[1]),
            'source'     => 'core',
        ];
    }

    // ---------------------------------------------------------------------
    // Fingerprints and small helpers
    // ---------------------------------------------------------------------

    /**
     * @param \WP_Post $post Post.
     * @return string
     */
    private function title(object $post): string
    {
        return (string) $post->post_title;
    }

    /**
     * @param \WP_Post $post Post.
     * @return string
     */
    private function content(object $post): string
    {
        return (string) $post->post_content;
    }

    /**
     * Same formula as ContentUpdateCommand::fingerprint(), so a fingerprint
     * from either command is valid for the other.
     *
     * @param string $title   Stored title.
     * @param string $content Stored content.
     * @return string
     */
    private function contentFingerprint(string $title, string $content): string
    {
        return 'sha256:' . hash(
            'sha256',
            self::CONTENT_FINGERPRINT_DOMAIN . "\n"
            . strlen($title) . "\n" . $title . "\n"
            . strlen($content) . "\n" . $content
        );
    }

    /**
     * "key\nlen\nbytes\n" framing; an absent key is "key\n-1\n".
     *
     * @param string      $key   Meta key.
     * @param string|null $value Raw stored bytes, or null when absent.
     * @return string
     */
    private function frame(string $key, ?string $value): string
    {
        return $value === null ? $key . "\n-1\n" : $key . "\n" . strlen($value) . "\n" . $value . "\n";
    }

    /**
     * Truncates to a byte budget without splitting a UTF-8 character.
     *
     * @param string $value Text.
     * @param int    $max   Byte budget.
     * @return string
     */
    private function capBytes(string $value, int $max): string
    {
        if (strlen($value) <= $max) {
            return $value;
        }
        $cut = substr($value, 0, $max);
        while ($cut !== '' && preg_match('//u', $cut) !== 1) {
            $cut = substr($cut, 0, -1);
        }

        return $cut;
    }

    /**
     * @param string $value Untrusted text to echo in a refusal.
     * @return string
     */
    private function echoCap(string $value): string
    {
        return (string) preg_replace('/[^\x20-\x7E]/', '?', $this->capBytes($value, 64));
    }

    /**
     * @param string              $code   Refusal code.
     * @param string              $detail Human-readable reason.
     * @param array<string,mixed> $extra  Extra machine fields.
     * @return array{refusal:array<string,mixed>}
     */
    private function refuse(string $code, string $detail, array $extra = []): array
    {
        return ['refusal' => $this->fail($code, $detail, false, $extra)];
    }

    /**
     * @param string              $code      Refusal code.
     * @param string              $detail    Human-readable reason.
     * @param bool                $retryable Whether a retry can succeed.
     * @param array<string,mixed> $extra     Extra machine fields.
     * @return array<string,mixed>
     */
    private function fail(string $code, string $detail, bool $retryable = false, array $extra = []): array
    {
        return [
            'ok'        => false,
            'outcome'   => 'failed',
            'code'      => $code,
            'detail'    => $detail,
            'retryable' => $retryable,
        ] + $extra;
    }
}
