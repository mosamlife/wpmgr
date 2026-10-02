<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/rest-read and wpmgr/rest-write: one reviewed WordPress REST route,
 * called in-process as the service principal.
 *
 * The control plane sends the reviewed route row (rest_route_catalogue,
 * serialised) as exact JSON text with its sha256, and an input naming that
 * row plus typed path, query and body values. There is no raw route string
 * anywhere in the input: the path is built here from the row's template and
 * the typed path values, the method comes from the row, and every query and
 * body key is checked against the row and against a code list that data can
 * never shorten. Route families that dispatch other routes, fetch remote
 * URLs, expose abilities or administer the site are refused in code.
 *
 * Reads are published content only: a response item whose status is not the
 * pinned status refuses the whole call.
 */
final class RestCall
{
    public const NAME_READ  = 'wpmgr/rest-read';
    public const NAME_WRITE = 'wpmgr/rest-write';

    /** Largest accepted route row text, in bytes. */
    public const MAX_ROUTE_BYTES = 65536;

    /** Keys no row and no input may carry, in query, body or path. */
    public const FORBIDDEN_KEYS = [
        '_method', '_embed', '_envelope', '_jsonp', '_fields', 'password',
        'context', 'status', 'author', 'meta', 'slug', 'template',
    ];

    /** Keys a row may pin, and the only values each may take. */
    private const PINNABLE = [
        'context' => ['view'],
        'status'  => ['publish', 'inherit'],
    ];

    /** The only body keys a post_fields write may carry. */
    public const POST_FIELDS = ['title' => 'post_title', 'excerpt' => 'post_excerpt'];

    /**
     * First path segments refused whatever follows (anchored on the segment:
     * the namespace's first segment equals, or starts with, the entry).
     */
    private const DENIED_FIRST_SEGMENT_PREFIXES = ['wp-abilities', 'mcp', 'wpmgr', 'batch', 'wp-site-health'];

    /** Path prefixes refused, matched on whole segments. */
    private const DENIED_PATHS = [
        'oembed/1.0/proxy',
        'wp-block-editor/v1/url-details',
        'wp/v2/pattern-directory',
        'wp/v2/block-directory',
        'wp/v2/settings',
        'wp/v2/plugins',
        'wp/v2/themes',
        'wp/v2/global-styles',
        'wp/v2/templates',
        'wp/v2/template-parts',
        'wp/v2/navigation',
    ];

    /** Path segment prefixes refused under wp/v2 (menus, menu-items, menu-locations). */
    private const DENIED_WP_V2_SEGMENT_PREFIXES = ['menu'];

    /** Refused for every method but GET. */
    private const DENIED_WRITE_PATHS = ['wp/v2/users'];

    private const RE_ROUTE_ID = '/^[a-z0-9-]{1,64}$/';
    private const RE_KEY      = '/^[a-z][a-z0-9_]{0,31}$/';
    private const RE_PATH     = '/^\/[a-z0-9_.\/-]{1,255}$/';
    private const RE_INT      = '/^[1-9][0-9]{0,9}$/';
    private const RE_HEX64    = '/^[0-9a-f]{64}$/';

    /** Control characters (newline allowed separately), bidi controls, BOM, noncharacters. */
    private const RE_BAD_CHARS = '/[\x{0000}-\x{0009}\x{000B}-\x{001F}\x{007F}-\x{009F}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{FFFE}\x{FFFF}]/u';

    /**
     * RC1 steps 1 and 3, and the row's own shape: the route text matches its
     * hash, its class matches the entry, and every part of it is one this
     * agent can run.
     *
     * @param mixed  $routeText Route row JSON text.
     * @param mixed  $routeSha  Its sha256, lowercase hex.
     * @param string $name      Entry name (rest-read or rest-write).
     * @param string $wpVersion Running WordPress version.
     * @return array{route?:array<string,mixed>,refusal?:array{code:string,detail:string}}
     */
    public static function parseRoute($routeText, $routeSha, string $name, string $wpVersion): array
    {
        if (!is_string($routeText) || $routeText === '' || strlen($routeText) > self::MAX_ROUTE_BYTES) {
            return self::refusal('route_not_reviewed', 'route must be the reviewed route row JSON text');
        }
        if (!is_string($routeSha) || preg_match(self::RE_HEX64, $routeSha) !== 1
            || !hash_equals($routeSha, hash('sha256', $routeText))) {
            return self::refusal('route_entry_changed', 'the route row does not match its hash');
        }
        $row = json_decode($routeText, true, 16);
        if (!is_array($row) || array_is_list($row)) {
            return self::refusal('route_not_reviewed', 'the route row is not a JSON object');
        }

        $routeId = $row['route_id'] ?? null;
        if (!is_string($routeId) || preg_match(self::RE_ROUTE_ID, $routeId) !== 1) {
            return self::refusal('route_not_reviewed', 'the route row has no valid route_id');
        }
        $class = $row['class'] ?? null;
        $want  = $name === self::NAME_WRITE ? 'write' : ($name === self::NAME_READ ? 'read' : '');
        if ($class !== $want) {
            return self::refusal('mode_class_mismatch', 'the route class does not match the entry');
        }
        if (($row['enabled'] ?? null) !== true) {
            return self::refusal('route_disabled', 'the route is not enabled');
        }

        $method = $row['method'] ?? null;
        if (($class === 'read' && $method !== 'GET') || ($class === 'write' && $method !== 'POST')) {
            return self::refusal('route_not_reviewed', 'this agent runs reads as GET and writes as POST only');
        }

        $min = $row['min_wp_version'] ?? null;
        if ($min !== null) {
            if (!is_string($min) || preg_match('/^[0-9]+(\.[0-9]+){0,2}$/', $min) !== 1) {
                return self::refusal('route_not_reviewed', 'the route has a malformed min_wp_version');
            }
            if ($wpVersion === '' || VersionCompare::compare($wpVersion, $min) < 0) {
                return self::refusal('route_wp_version_unsupported', 'this route needs a newer WordPress');
            }
        }

        foreach (['namespace', 'template', 'core_pattern'] as $field) {
            if (!is_string($row[$field] ?? null) || $row[$field] === '') {
                return self::refusal('route_not_reviewed', 'the route row has no ' . $field);
            }
        }
        $namespace = (string) $row['namespace'];
        $template  = (string) $row['template'];
        $pattern   = (string) $row['core_pattern'];
        if (preg_match('/^\/[a-z0-9_.\/{}-]{1,200}$/', $template) !== 1
            || ($template !== '/' . $namespace && strncmp($template, '/' . $namespace . '/', strlen($namespace) + 2) !== 0)
            || strpos($template, '//') !== false || strpos($template, '..') !== false) {
            return self::refusal('route_not_reviewed', 'the route template is malformed');
        }
        if (preg_match('/^\/[!-~]{1,255}$/', $pattern) !== 1 || strncmp($pattern, '/' . $namespace, strlen($namespace) + 1) !== 0) {
            return self::refusal('route_not_reviewed', 'the route core_pattern is malformed');
        }
        // A row naming a refused family is refused even though the
        // database also forbids it: this check does not depend on data.
        $templateBare = (string) preg_replace('/\{[a-z][a-z0-9_]{0,31}\}/', '1', $template);
        if (self::denied($templateBare, $method) || self::denied('/' . $namespace, $method)) {
            return self::refusal('route_namespace_refused', 'this route family is never called by the agent');
        }

        $specs = [];
        foreach (['path_params', 'query_keys', 'body_keys'] as $field) {
            $spec = $row[$field] ?? [];
            if (!is_array($spec) || ($spec !== [] && array_is_list($spec))) {
                return self::refusal('route_not_reviewed', 'the route ' . $field . ' is malformed');
            }
            foreach ($spec as $key => $def) {
                $key = (string) $key;
                if (in_array($key, self::FORBIDDEN_KEYS, true)) {
                    return self::refusal('route_key_forbidden', 'the route names a key the agent never sends: ' . $key);
                }
                if (preg_match(self::RE_KEY, $key) !== 1 || !is_array($def) || self::specProblem($def) !== null) {
                    return self::refusal('route_not_reviewed', 'the route ' . $field . ' has a malformed key');
                }
            }
            $specs[$field] = $spec;
        }
        $pinned = $row['pinned_query'] ?? [];
        if (!is_array($pinned) || ($pinned !== [] && array_is_list($pinned))) {
            return self::refusal('route_not_reviewed', 'the route pinned_query is malformed');
        }
        foreach ($pinned as $key => $value) {
            $key = (string) $key;
            if (!isset(self::PINNABLE[$key]) || !is_string($value) || !in_array($value, self::PINNABLE[$key], true)) {
                return self::refusal('route_not_reviewed', 'the route pins a key or value this agent does not allow');
            }
        }

        // Every placeholder in the template is a path param, and every path
        // param is a required int placeholder.
        preg_match_all('/\{([a-z][a-z0-9_]{0,31})\}/', $template, $m);
        $placeholders = $m[1];
        sort($placeholders);
        $declared = array_keys($specs['path_params']);
        sort($declared);
        if ($placeholders !== $declared || count($placeholders) !== count(array_unique($placeholders))) {
            return self::refusal('route_not_reviewed', 'the route template and path_params disagree');
        }
        foreach ($specs['path_params'] as $def) {
            if (($def['type'] ?? null) !== 'int' || ($def['required'] ?? false) !== true) {
                return self::refusal('route_not_reviewed', 'every path param must be a required int');
            }
        }
        if (strpos((string) preg_replace('/\{[a-z][a-z0-9_]{0,31}\}/', '', $template), '{') !== false
            || strpos((string) preg_replace('/\{[a-z][a-z0-9_]{0,31}\}/', '', $template), '}') !== false) {
            return self::refusal('route_not_reviewed', 'the route template is malformed');
        }

        $output = $row['output_fields'] ?? null;
        if (!is_array($output) || $output === []) {
            return self::refusal('route_not_reviewed', 'the route has no output_fields');
        }

        if ($class === 'read') {
            if ($specs['body_keys'] !== [] || ($row['snapshot'] ?? 'none') !== 'none') {
                return self::refusal('route_not_reviewed', 'a read route has no body and no snapshot');
            }
        } else {
            $problem = self::writeRowProblem($row, $specs, $pinned);
            if ($problem !== null) {
                return self::refusal('route_not_reviewed', $problem);
            }
        }

        return ['route' => [
            'route_id'      => $routeId,
            'method'        => (string) $method,
            'namespace'     => $namespace,
            'template'      => $template,
            'core_pattern'  => $pattern,
            'path_params'   => $specs['path_params'],
            'query_keys'    => $specs['query_keys'],
            'body_keys'     => $specs['body_keys'],
            'pinned_query'  => $pinned,
            'class'         => $class,
            'output_fields' => $output,
            'target'        => is_array($row['target'] ?? null) ? $row['target'] : null,
        ]];
    }

    /**
     * Why a write row is not a post_fields write this agent can run, or null.
     *
     * @param array<string,mixed>                $row    Row.
     * @param array<string,array<string,mixed>>  $specs  Parsed key specs.
     * @param array<string,mixed>                $pinned Pinned query.
     * @return string|null
     */
    private static function writeRowProblem(array $row, array $specs, array $pinned): ?string
    {
        if (($row['snapshot'] ?? null) !== 'post_fields') {
            return 'a write route needs the post_fields snapshot';
        }
        // A pinned or free query value on a POST would be read as a body
        // field by the route, so a write takes neither.
        if ($pinned !== [] || $specs['query_keys'] !== []) {
            return 'a write route takes no query values';
        }
        if ($specs['body_keys'] === []) {
            return 'a write route needs body keys';
        }
        foreach ($specs['body_keys'] as $key => $def) {
            if (!isset(self::POST_FIELDS[$key]) || ($def['type'] ?? null) !== 'string') {
                return 'a post_fields write changes only the title and excerpt';
            }
        }
        $target = $row['target'] ?? null;
        if (!is_array($target) || ($target['kind'] ?? null) !== 'post'
            || !is_string($target['param'] ?? null) || !isset($specs['path_params'][$target['param']])
            || !in_array($target['post_type'] ?? null, ['page', 'post'], true)) {
            return 'a write route needs a post target with a post type';
        }
        $base = $target['post_type'] === 'page' ? '/wp/v2/pages/{' : '/wp/v2/posts/{';
        if (strncmp((string) $row['template'], $base, strlen($base)) !== 0) {
            return 'the write route does not address its target post type';
        }

        return null;
    }

    /**
     * Why a key spec is malformed, or null.
     *
     * @param array<string,mixed> $def Spec.
     * @return string|null
     */
    private static function specProblem(array $def): ?string
    {
        $type = $def['type'] ?? null;
        if (array_key_exists('required', $def) && !is_bool($def['required'])) {
            return 'required';
        }
        switch ($type) {
            case 'int':
                return is_int($def['min'] ?? null) && is_int($def['max'] ?? null) && $def['min'] <= $def['max'] ? null : 'int';
            case 'string':
                return is_int($def['max_len'] ?? null) && $def['max_len'] >= 1 && $def['max_len'] <= 10000 ? null : 'string';
            case 'enum':
                $values = $def['values'] ?? null;
                if (!is_array($values) || $values === [] || !array_is_list($values)) {
                    return 'enum';
                }
                foreach ($values as $v) {
                    if (!is_string($v) || preg_match('/^[a-z0-9_-]{1,32}$/', $v) !== 1) {
                        return 'enum';
                    }
                }

                return null;
            case 'int_list':
                return is_int($def['max_items'] ?? null) && $def['max_items'] >= 1 && $def['max_items'] <= 100 ? null : 'int_list';
            default:
                return 'type';
        }
    }

    /**
     * RC1 steps 2, 4, 5 and 6: the input names this row, and the path, query
     * and body are built from typed values only.
     *
     * @param mixed               $inputText Input JSON text.
     * @param array<string,mixed> $route     Parsed route.
     * @return array{call?:array{path:string,query:array<string,mixed>,body:array<string,mixed>,target_id:int},refusal?:array{code:string,detail:string}}
     */
    public static function parseInput($inputText, array $route): array
    {
        if (!is_string($inputText) || $inputText === '' || strlen($inputText) > 65536) {
            return self::refusal('bad_input', 'input must be JSON text of an object');
        }
        $input = json_decode($inputText, true, 8);
        if (!is_array($input) || ($input !== [] && array_is_list($input)) || $input === []) {
            return self::refusal('bad_input', 'input must be a JSON object');
        }
        foreach (array_keys($input) as $key) {
            if (!in_array((string) $key, ['route_id', 'path', 'query', 'body'], true)) {
                return self::refusal('route_key_not_allowed', 'input takes route_id, path, query and body only');
            }
        }
        if (($input['route_id'] ?? null) !== $route['route_id']) {
            return self::refusal('route_entry_changed', 'input.route_id does not name the route row sent');
        }

        $parts = [];
        foreach (['path' => 'path_params', 'query' => 'query_keys', 'body' => 'body_keys'] as $part => $specField) {
            $given = $input[$part] ?? [];
            if (!is_array($given) || ($given !== [] && array_is_list($given))) {
                return self::refusal('bad_input', 'input.' . $part . ' must be an object');
            }
            foreach (array_keys($given) as $key) {
                if (in_array((string) $key, self::FORBIDDEN_KEYS, true)) {
                    return self::refusal('route_key_forbidden', 'input.' . $part . ' carries a key the agent never sends');
                }
            }
            $checked = self::typed($given, $route[$specField], $part);
            if (isset($checked['refusal'])) {
                return $checked;
            }
            $parts[$part] = $checked['values'];
        }

        // The path: the row's template, each placeholder filled from a typed int.
        $path = (string) $route['template'];
        foreach ($parts['path'] as $key => $value) {
            $path = str_replace('{' . $key . '}', (string) $value, $path);
        }
        if (preg_match(self::RE_PATH, $path) !== 1 || strpos($path, '//') !== false || strpos($path, '..') !== false
            || strpos($path, '{') !== false) {
            return self::refusal('route_param_invalid', 'the built path is malformed');
        }
        if (self::denied($path, (string) $route['method'])) {
            return self::refusal('route_namespace_refused', 'this route family is never called by the agent');
        }

        $targetId = 0;
        if ($route['class'] === 'write') {
            $param    = (string) ($route['target']['param'] ?? '');
            $targetId = (int) ($parts['path'][$param] ?? 0);
            if ($targetId < 1) {
                return self::refusal('route_param_invalid', 'the write has no target id');
            }
            if ($parts['body'] === []) {
                return self::refusal('bad_input', 'a write needs at least one body value');
            }
            if (isset($parts['body']['title']) && trim((string) $parts['body']['title']) === '') {
                return self::refusal('route_param_invalid', 'a title cannot be empty');
            }
        }

        // Pinned values last, so nothing supplied can stand in for them.
        $query = $parts['query'];
        foreach ($route['pinned_query'] as $key => $value) {
            $query[(string) $key] = $value;
        }

        return ['call' => [
            'path'      => $path,
            'query'     => $query,
            'body'      => $parts['body'],
            'target_id' => $targetId,
        ]];
    }

    /**
     * Check values against a typed key spec.
     *
     * @param array<string,mixed>               $given Supplied values.
     * @param array<string,array<string,mixed>> $spec  Key spec.
     * @param string                            $part  path, query or body.
     * @return array{values?:array<string,mixed>,refusal?:array{code:string,detail:string}}
     */
    private static function typed(array $given, array $spec, string $part): array
    {
        $out = [];
        foreach ($given as $key => $value) {
            $key = (string) $key;
            if (!isset($spec[$key])) {
                return self::refusal('route_key_not_allowed', 'input.' . $part . '.' . $key . ' is not a key this route takes');
            }
            $def = $spec[$key];
            $bad = self::refusal('route_param_invalid', 'input.' . $part . '.' . $key . ' is not a valid value');
            switch ($def['type']) {
                case 'int':
                    if (!is_int($value) || $value < (int) $def['min'] || $value > (int) $def['max']) {
                        return $bad;
                    }
                    if ($part === 'path' && preg_match(self::RE_INT, (string) $value) !== 1) {
                        return $bad;
                    }
                    $out[$key] = $value;
                    break;
                case 'string':
                    if (!is_string($value) || preg_match('//u', $value) !== 1
                        || preg_match(self::RE_BAD_CHARS, $value) === 1
                        || ($part !== 'body' && strpos($value, "\n") !== false)
                        || self::chars($value) > (int) $def['max_len']) {
                        return $bad;
                    }
                    $out[$key] = $value;
                    break;
                case 'enum':
                    if (!is_string($value) || !in_array($value, (array) $def['values'], true)) {
                        return $bad;
                    }
                    $out[$key] = $value;
                    break;
                case 'int_list':
                    if (!is_array($value) || !array_is_list($value) || $value === [] || count($value) > (int) $def['max_items']) {
                        return $bad;
                    }
                    foreach ($value as $item) {
                        if (!is_int($item) || preg_match(self::RE_INT, (string) $item) !== 1) {
                            return $bad;
                        }
                    }
                    $out[$key] = $value;
                    break;
                default:
                    return $bad;
            }
        }
        foreach ($spec as $key => $def) {
            if (($def['required'] ?? false) === true && !array_key_exists((string) $key, $out)) {
                return self::refusal('route_param_invalid', 'input.' . $part . '.' . $key . ' is required');
            }
        }

        return ['values' => $out];
    }

    /**
     * Is this path in a family the agent never calls? Matched on whole
     * segments, case-folded, against the route without its leading slash.
     *
     * @param string $path   Built path or template.
     * @param string $method Method.
     * @return bool
     */
    public static function denied(string $path, string $method): bool
    {
        $p = strtolower(ltrim($path, '/'));
        if ($p === '') {
            return true;
        }
        $segments = explode('/', $p);
        foreach (self::DENIED_FIRST_SEGMENT_PREFIXES as $prefix) {
            if (strncmp($segments[0], $prefix, strlen($prefix)) === 0) {
                return true;
            }
        }
        foreach (self::DENIED_PATHS as $denied) {
            if ($p === $denied || strncmp($p, $denied . '/', strlen($denied) + 1) === 0) {
                return true;
            }
        }
        if ($segments[0] === 'wp' && ($segments[1] ?? '') === 'v2' && isset($segments[2])) {
            foreach (self::DENIED_WP_V2_SEGMENT_PREFIXES as $prefix) {
                if (strncmp($segments[2], $prefix, strlen($prefix)) === 0) {
                    return true;
                }
            }
        }
        if (strtoupper($method) !== 'GET') {
            foreach (self::DENIED_WRITE_PATHS as $denied) {
                if ($p === $denied || strncmp($p, $denied . '/', strlen($denied) + 1) === 0) {
                    return true;
                }
            }
        }

        return false;
    }

    /**
     * The request object for one call. The method comes from the row.
     *
     * @param array<string,mixed>                                                             $route Parsed route.
     * @param array{path:string,query:array<string,mixed>,body:array<string,mixed>,target_id:int} $call  Parsed call.
     * @return \WP_REST_Request
     */
    public static function buildRequest(array $route, array $call): \WP_REST_Request
    {
        $request = new \WP_REST_Request((string) $route['method'], $call['path']);
        $request->set_query_params($call['query']);
        if ($call['body'] !== []) {
            $request->set_body_params($call['body']);
        }

        return $request;
    }

    /**
     * Ruling 5: reads return published content only. With a pinned status,
     * every returned object (the one item, or every list item) must carry
     * exactly that status; the pinned list query does not filter a
     * single-item read, so this is checked on the response itself.
     *
     * @param mixed               $data  Response data, decoded to arrays.
     * @param array<string,mixed> $route Parsed route.
     * @return bool True when the response may be returned.
     */
    public static function publishedOnly($data, array $route): bool
    {
        $status = $route['pinned_query']['status'] ?? null;
        if (!is_string($status)) {
            return true;
        }
        if (!is_array($data)) {
            return false;
        }
        $items = array_is_list($data) ? $data : [$data];
        foreach ($items as $item) {
            if (!is_array($item) || ($item['status'] ?? null) !== $status) {
                return false;
            }
        }

        return true;
    }

    /**
     * Ruling 5 for media: an attachment inherits its parent's read
     * permission, so a media list can include files attached to a draft.
     * For a route pinned to status inherit, drop every list item whose
     * parent is set and is not a published post. An item without a readable
     * parent id is dropped too.
     *
     * @param mixed               $data  Response data, decoded to arrays.
     * @param array<string,mixed> $route Parsed route.
     * @return mixed
     */
    public static function dropUnpublishedParents($data, array $route)
    {
        if (($route['pinned_query']['status'] ?? null) !== 'inherit' || !is_array($data) || !array_is_list($data)) {
            return $data;
        }
        $out = [];
        foreach ($data as $item) {
            $parent = is_array($item) ? ($item['post'] ?? null) : null;
            if ($parent === null && is_array($item) && array_key_exists('post', $item)) {
                $parent = 0;
            }
            if (!is_int($parent) || $parent < 0) {
                continue;
            }
            if ($parent > 0 && (!function_exists('get_post_status') || get_post_status($parent) !== 'publish')) {
                continue;
            }
            $out[] = $item;
        }

        return $out;
    }

    /**
     * Response data as plain arrays (objects become associative arrays).
     *
     * @param mixed $data Data.
     * @return mixed Null when it cannot be encoded.
     */
    public static function plain($data)
    {
        $json = json_encode($data);
        if (!is_string($json)) {
            return null;
        }

        return json_decode($json, true);
    }

    // ---------------------------------------------------------------------
    // post_fields (rest-write)
    // ---------------------------------------------------------------------

    /**
     * base_fingerprint of a post: what the write was approved against.
     *
     * @param object $post Post.
     * @return string
     */
    public static function postFingerprint(object $post): string
    {
        return hash('sha256', (string) json_encode([
            (int) ($post->ID ?? 0),
            (string) ($post->post_type ?? ''),
            (string) ($post->post_status ?? ''),
            (string) ($post->post_modified_gmt ?? ''),
            (string) ($post->post_title ?? ''),
            (string) ($post->post_excerpt ?? ''),
        ]));
    }

    /**
     * The exact bytes stored for a plain-text value: HTML-escaped, the
     * escaping the page-create title uses, which every save filter leaves
     * byte-identical. "Tom & Jerry" is stored as "Tom &amp; Jerry" and
     * displays as typed, so an ampersand is never a sanitiser change.
     *
     * @param string $plain Plain text.
     * @return string
     */
    public static function storedValue(string $plain): string
    {
        return PageCreateBuilder::storedTitle($plain);
    }

    /**
     * The precheck digest.
     *
     * @param string $entrySha Entry hash.
     * @param string $routeSha Route hash.
     * @param string $inputSha Input hash.
     * @param string $baseFp   Base fingerprint.
     * @return string
     */
    public static function precheckDigest(string $entrySha, string $routeSha, string $inputSha, string $baseFp): string
    {
        return hash('sha256', (string) json_encode([$entrySha, $routeSha, $inputSha, $baseFp]));
    }

    /**
     * @param string $value Text.
     * @return int
     */
    private static function chars(string $value): int
    {
        return function_exists('mb_strlen') ? mb_strlen($value, 'UTF-8') : strlen($value);
    }

    /**
     * @param string $code   Code.
     * @param string $detail Detail.
     * @return array{refusal:array{code:string,detail:string}}
     */
    private static function refusal(string $code, string $detail): array
    {
        return ['refusal' => ['code' => $code, 'detail' => $detail]];
    }
}
