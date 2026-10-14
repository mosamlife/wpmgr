<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Support\ArrayShape;

if (!defined('ABSPATH')) {
    exit;
}

/** Read-only Theme Builder discovery, with whole rows bounded by encoded size. */
final class ElementorThemeTemplates
{
    public const MAX_BYTES = 30 * 1024;
    public const MAX_ROWS = 25;
    public const MAX_RULES = 20;
    public const STATUSES = ['publish', 'draft', 'pending', 'private', 'future', 'other'];
    public const RE_RULE = '/^(include|exclude)(\/[a-z0-9_-]{1,64}){1,2}(\/[1-9][0-9]{0,19})?$/D';
    public const PLACES = [
        ['rule' => 'general', 'takes_id' => false],
        ['rule' => 'singular/front_page', 'takes_id' => false],
        ['rule' => 'singular/page', 'takes_id' => true],
        ['rule' => 'singular/post', 'takes_id' => true],
        ['rule' => 'singular/not_found404', 'takes_id' => false],
        ['rule' => 'archive', 'takes_id' => false],
    ];

    public function __construct(private readonly ElementorApi $api = new ElementorRuntime())
    {
    }

    /** @return array<string, mixed> */
    public static function inputSchema(): array
    {
        return ['type' => 'object', 'properties' => new \stdClass(), 'additionalProperties' => false];
    }

    /** @return array<string, mixed> */
    public static function outputSchema(): array
    {
        $string = ['type' => 'string'];
        $bool = ['type' => 'boolean'];
        $id = ['type' => 'integer', 'minimum' => 1];
        $total = ['type' => 'integer', 'minimum' => 0];
        $nullableEnum = static fn (array $values) => ['type' => ['string', 'null'], 'enum' => array_merge($values, [null])];
        $object = static fn (array $properties) => ['type' => 'object', 'properties' => $properties, 'required' => array_keys($properties), 'additionalProperties' => false];
        $rows = static fn (array $schema) => ['type' => 'array', 'maxItems' => self::MAX_ROWS, 'items' => $schema];
        return $object([
            'pro' => $object([
                'active' => $bool,
                'version' => ['type' => ['string', 'null'], 'pattern' => '^[0-9][0-9A-Za-z._+-]{0,31}$', 'maxLength' => 32],
                'theme_builder' => $bool,
                'problem' => $nullableEnum(['pro_inactive', 'theme_builder_unavailable']),
            ]),
            'templates' => $rows($object([
                'id' => $id, 'type' => ['type' => 'string', 'enum' => ['header', 'footer']],
                'status' => ['type' => 'string', 'enum' => self::STATUSES],
                'made_by_wpmgr' => $bool, 'title' => $string, 'modified_gmt' => $string,
                'shows_on' => ['type' => ['array', 'null'], 'maxItems' => self::MAX_RULES, 'items' => ['type' => 'string', 'pattern' => '^(include|exclude)(/[a-z0-9_-]{1,64}){1,2}(/[1-9][0-9]{0,19})?$']],
                'shows_on_problem' => $nullableEnum(['pro_unavailable', 'unreadable', 'too_many']),
            ])),
            'templates_total' => $total,
            'places' => ['type' => 'array', 'maxItems' => count(self::PLACES), 'items' => $object([
                'rule' => ['type' => 'string', 'enum' => array_column(self::PLACES, 'rule')], 'takes_id' => $bool,
            ])],
            'menus' => $rows($object([
                'id' => $id, 'slug' => ['type' => ['string', 'null'], 'pattern' => '^[a-z0-9_-]{1,200}$', 'maxLength' => 200],
                'name' => $string, 'items' => $total,
            ])),
            'menus_total' => $total,
        ]);
    }

    /**
     * @param object $input Empty input object.
     * @return array{output?: array<string, mixed>, refusal?: array{code: string, detail: string}}
     */
    public function read(object $input): array
    {
        $bad = OwnAbilities::validate(OwnAbilities::NAME_ELEMENTOR_THEME_TEMPLATES, $input);
        if ($bad !== null) {
            return self::refuse('bad_input', $bad);
        }
        $active = $this->api->loaded();
        $status = new AdapterStatus($active, $this->api->version(), ElementorFacts::CLASSIC_MIN, ElementorFacts::CLASSIC_MAX, $active ? [] : ['elementor_missing']);
        $refusal = $status->refusal();
        if ($refusal !== null) {
            return ['refusal' => $refusal];
        }
        $pro = $this->api->proActive();
        $ready = $pro && $this->api->themeBuilderReady();
        try {
            $templates = self::templateIds();
            if ($templates === null) {
                return self::refuse('read_failed', 'the theme templates could not be read');
            }
            $ids = $templates['ids'];
            $menus = wp_get_nav_menus();
            if (!is_array($ids) || !is_array($menus)) {
                return self::refuse('read_failed', 'the theme templates or menus could not be read');
            }
            $out = [
                'pro' => ['active' => $pro, 'version' => $this->api->proVersion(), 'theme_builder' => $ready, 'problem' => !$pro ? 'pro_inactive' : (!$ready ? 'theme_builder_unavailable' : null)],
                'templates' => [], 'templates_total' => $templates['total'], 'places' => [],
                'menus' => [], 'menus_total' => count($menus),
            ];
            if ($ready) {
                foreach (self::PLACES as $place) {
                    foreach (explode('/', $place['rule']) as $name) {
                        if (!$this->api->themeConditionRegistered($name)) {
                            continue 2;
                        }
                    }
                    $out['places'][] = $place;
                }
            }
            foreach (array_slice($ids, 0, self::MAX_ROWS) as $id) {
                $post = get_post((int) $id);
                if (!is_object($post)) {
                    return self::refuse('read_failed', 'a theme template could not be read');
                }
                $type = get_post_meta((int) $id, '_elementor_template_type', true);
                if (!in_array($type, ['header', 'footer'], true)) {
                    return self::refuse('read_failed', 'a theme template type could not be read');
                }
                $rules = $ready ? $this->api->templateRules((int) $id) : null;
                $problem = !$ready ? 'pro_unavailable' : self::rulesProblem($rules);
                $markers = get_post_meta((int) $id, DraftEligibility::MARKER_KEY, false);
                $date = \DateTimeImmutable::createFromFormat('!Y-m-d H:i:s', $post->post_modified_gmt, new \DateTimeZone('UTC'));
                if ($date === false || $date->format('Y-m-d H:i:s') !== $post->post_modified_gmt) {
                    return self::refuse('read_failed', 'a theme template modification time could not be read');
                }
                $row = [
                    'id' => (int) $id, 'type' => $type, 'status' => in_array($post->post_status, self::STATUSES, true) ? $post->post_status : 'other',
                    'made_by_wpmgr' => is_array($markers) && DraftEligibility::validCreationMarker($markers),
                    'title' => $post->post_title, 'modified_gmt' => $date->format('Y-m-d\TH:i:s\Z'),
                    'shows_on' => $problem === null ? $rules : null, 'shows_on_problem' => $problem,
                ];
                if (!self::append($out, 'templates', $row)) {
                    break;
                }
            }
            foreach (array_slice($menus, 0, self::MAX_ROWS) as $menu) {
                $row = ['id' => (int) $menu->term_id, 'slug' => is_string($menu->slug) && preg_match('/^[a-z0-9_-]{1,200}$/D', $menu->slug) === 1 ? $menu->slug : null, 'name' => $menu->name, 'items' => (int) $menu->count];
                if (!self::append($out, 'menus', $row)) {
                    break;
                }
            }
            return ['output' => $out];
        } catch (\Throwable $e) {
            return self::refuse('read_failed', 'the theme templates or menus could not be read');
        }
    }

    /**
     * Count every matching post and read only the newest bounded set of ids.
     * EXISTS keeps duplicate meta rows from duplicating a template.
     *
     * @return array{ids: list<int>, total: int}|null
     */
    private static function templateIds(): ?array
    {
        global $wpdb;
        $where = "FROM %i p WHERE p.post_type = %s AND p.post_status NOT IN (%s, %s, %s)
            AND EXISTS (SELECT 1 FROM %i m WHERE m.post_id = p.ID
                AND BINARY m.meta_key = BINARY %s AND BINARY m.meta_value IN (%s, %s))";
        $args = [$wpdb->posts, 'elementor_library', 'trash', 'auto-draft', 'inherit', $wpdb->postmeta, '_elementor_template_type', 'header', 'footer'];
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.PreparedSQLPlaceholders.UnfinishedPrepare -- Prepared identifiers and values; fresh totals must include all matching statuses.
        $total = $wpdb->get_var($wpdb->prepare('SELECT COUNT(*) ' . $where, ...$args));
        if ((!is_string($total) && !is_int($total)) || preg_match('/^[0-9]+$/D', (string) $total) !== 1 || $wpdb->last_error !== '') {
            return null;
        }
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.NotPrepared -- Same prepared predicate; GMT ordering and bounded ids are part of this read's contract.
        $ids = $wpdb->get_col($wpdb->prepare('SELECT p.ID ' . $where . ' ORDER BY p.post_modified_gmt DESC, p.ID DESC LIMIT %d', ...array_merge($args, [self::MAX_ROWS])));
        if (!is_array($ids) || $wpdb->last_error !== '') {
            return null;
        }
        return ['ids' => array_map('intval', $ids), 'total' => (int) $total];
    }

    /**
     * @param array<mixed>|null $rules Complete runtime answer.
     */
    private static function rulesProblem(?array $rules): ?string
    {
        if ($rules === null || !ArrayShape::isList($rules)) {
            return 'unreadable';
        }
        if (count($rules) > self::MAX_RULES) {
            return 'too_many';
        }
        foreach ($rules as $rule) {
            if (!is_string($rule) || preg_match(self::RE_RULE, $rule) !== 1) {
                return 'unreadable';
            }
        }
        return null;
    }

    /**
     * @param array<string, mixed> $out Answer being built.
     * @param string $list List name.
     * @param array<string, mixed> $row Whole row to add.
     */
    private static function append(array &$out, string $list, array $row): bool
    {
        $candidate = $out;
        $candidate[$list][] = $row;
        $encoded = wp_json_encode($candidate);
        if (!is_string($encoded)) {
            throw new \UnexpectedValueException('the theme templates answer could not be encoded');
        }
        if (strlen($encoded) > self::MAX_BYTES) {
            return false;
        }
        $out = $candidate;
        return true;
    }

    /** @return array{refusal: array{code: string, detail: string}} */
    private static function refuse(string $code, string $detail): array
    {
        return ['refusal' => ['code' => $code, 'detail' => $detail]];
    }
}
