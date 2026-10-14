<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilityWriteScope;
use WPMgr\Agent\Support\ArrayShape;

if (!defined('ABSPATH')) {
    exit;
}

/** The closed draft-template contract shared by parsing, precheck and verification. */
final class ThemeTemplateCreate
{
    public const TYPES = ['header', 'footer'];

    public function __construct(private readonly ElementorApi $api)
    {
    }

    /** @param mixed $rules Input display rules. */
    public static function inputProblem(mixed $rules): ?string
    {
        if (!is_array($rules) || !ArrayShape::isList($rules) || count($rules) > ElementorThemeTemplates::MAX_RULES) {
            return 'shows_on must be a list of at most 20 unique rules';
        }
        $places = array_column(ElementorThemeTemplates::PLACES, 'takes_id', 'rule');
        $seen = [];
        foreach ($rules as $i => $rule) {
            if (!is_string($rule)) {
                return 'shows_on[' . $i . '] must be a rule string';
            }
            if (preg_match(ElementorThemeTemplates::RE_RULE, $rule) !== 1 || isset($seen[$rule])) {
                return 'shows_on rule is invalid or repeated: ' . $rule;
            }
            [$place, $id] = self::parts($rule);
            if (!array_key_exists($place, $places) || ($id !== null && !$places[$place])) {
                return 'shows_on rule names an unknown place or disallowed id: ' . $rule;
            }
            $seen[$rule] = true;
        }
        return null;
    }

    /** @return array{string, string|null} Place and optional id, after grammar validation. */
    public static function parts(string $rule): array
    {
        $parts = explode('/', $rule);
        array_shift($parts);
        $id = preg_match('/^[1-9][0-9]*$/D', (string) end($parts)) === 1 ? array_pop($parts) : null;
        return [implode('/', $parts), $id];
    }

    /** @return array{code:string,detail:string}|null */
    public function availabilityRefusal(): ?array
    {
        if (!$this->api->proActive() || !$this->api->themeBuilderReady()) {
            return [
                'code' => 'theme_builder_unavailable',
                'detail' => 'Headers and footers need Elementor Pro with its Theme Builder, and it is not ready on this site.',
            ];
        }
        return null;
    }

    /** @param list<string> $rules Validated input. @return array{code:string,detail:string}|null */
    public function readinessRefusal(array $rules): ?array
    {
        $unavailable = $this->availabilityRefusal();
        if ($unavailable !== null) {
            return $unavailable;
        }
        foreach ($rules as $rule) {
            [$place, $id] = self::parts($rule);
            foreach (explode('/', $place) as $name) {
                if (!$this->api->themeConditionRegistered($name)) {
                    return [
                        'code' => 'shows_on_place_unavailable',
                        'detail' => 'shows_on: ' . $rule . " is not a place this site's Elementor Pro can show a template",
                    ];
                }
            }
            if ($id !== null) {
                // The grammar permits decimal ids wider than this PHP host can address.
                $post = (string) (int) $id === $id ? get_post((int) $id) : null;
                $type = $place === 'singular/page' ? 'page' : 'post';
                if (!is_object($post) || ($post->post_type ?? null) !== $type || ($post->post_status ?? null) === 'trash') {
                    return ['code' => 'shows_on_target_missing', 'detail' => 'shows_on: ' . $rule . ' needs an existing non-trashed ' . $type];
                }
            }
        }
        return null;
    }

    /** @return list<array<string,mixed>>|null Published templates, or null when the optional read fails. */
    public function alsoLive(string $type): ?array
    {
        return (new ElementorThemeTemplates($this->api))->alsoLive($type);
    }

    /** @param list<string> $rules Requested rules. @param bool $replace Save even an empty replacement list. @return array<string, mixed> */
    public function saveRules(int $postId, array $rules, bool $replace = false): array
    {
        if ($replace || $rules !== []) {
            $scope = new AbilityWriteScope($postId, []);
            try {
                $saved = $scope->run(fn (): bool => $this->api->saveTemplateRules($postId, $rules));
            } catch (\Throwable $e) {
                $saved = false;
            }
            $outcome = $scope->outcome();
            if (!$outcome['clean']) {
                $detail = in_array(AbilityWriteScope::V_OPTION, $outcome['violations'], true)
                    ? 'Elementor Pro changed its live display list for a draft'
                    : 'the rules save wrote outside the draft: ' . implode(', ', $outcome['violations']);
                return ['code' => ElementorDocument::CODE_SIDE_EFFECT, 'detail' => $detail];
            }
            if (!$saved) {
                return ['code' => ElementorDocument::CODE_REFUSED, 'detail' => 'Elementor Pro did not save shows_on'];
            }
        }
        $stored = $this->api->templateRules($postId);
        if (!self::sameRules($rules, $stored)) {
            return ['code' => 'verify_mismatch', 'detail' => 'shows_on differs; Elementor Pro stored: ' . wp_json_encode($stored, JSON_UNESCAPED_SLASHES)];
        }
        return ['rules' => $stored];
    }

    /** Read the current rules through Pro; null means unreadable. */
    public function readRules(int $postId): ?array
    {
        return $this->api->templateRules($postId);
    }

    /** @param list<string> $wanted @param array<mixed>|null $stored */
    public static function sameRules(array $wanted, ?array $stored): bool
    {
        if (ElementorThemeTemplates::rulesProblem($stored) !== null) {
            return false;
        }
        sort($wanted, SORT_STRING);
        sort($stored, SORT_STRING);
        return $wanted === $stored;
    }

    /** @param list<string> $rules @return list<array{id:int,title:string}> */
    public static function ruleTitles(array $rules): array
    {
        $titles = [];
        foreach ($rules as $rule) {
            [$place, $id] = self::parts($rule);
            if (in_array($place, ['singular/page', 'singular/post'], true) && $id !== null && !isset($titles[$id])) {
                $post = (string) (int) $id === $id ? get_post((int) $id) : null;
                if (!is_object($post)) {
                    continue;
                }
                if (!is_string($post->post_title ?? null)) {
                    throw new \UnexpectedValueException('shows_on title could not be read');
                }
                $titles[$id] = ['id' => (int) $id, 'title' => $post->post_title];
            }
        }
        return array_values($titles);
    }

    /** Exactly the named library type, read by taxonomy rather than cached document state. */
    public static function termMatches(int $postId, string $type): bool
    {
        try {
            return wp_get_object_terms($postId, 'elementor_library_type', ['fields' => 'slugs']) === [$type];
        } catch (\Throwable $e) {
            return false;
        }
    }
}
