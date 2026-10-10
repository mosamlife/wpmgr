<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Write scope of one post, armed only around a builder save.
 *
 * While armed, the save may write the target post and the revisions of it
 * inserted while armed, and nothing else. Through core's own hooks:
 *
 * - posts: an insert or update of the target row is allowed, and so is the
 *   insert of a revision whose parent is the target, whose id is recorded.
 *   Any other insert or update, and the delete or trash of anything but a
 *   recorded revision (the target included), is a violation;
 * - post meta: a row added, updated or deleted on the target (its key is
 *   recorded) or on a recorded revision is allowed; on any other object it
 *   is a violation;
 * - terms: a change to any object's term relationships is a violation.
 *   Setting an object's terms to the set it already holds is not a change:
 *   core's post update sets the categories of every post type that has
 *   them again;
 * - options: a name outside the given patterns is a violation. "{target}" in
 *   a pattern stands for the target id in decimal, so a pattern written for
 *   one post never admits another post's option. A name that starts with
 *   the prefix of a given value check is admitted only write by write: each
 *   add, update and delete of it is a violation unless the check, given the
 *   value before and the value after (ABSENT for an option that did not
 *   exist, or no longer does) and the target id, answers true. A check that
 *   throws answers false. The value before a delete is read just before the
 *   delete; one that cannot be read is a violation;
 * - users, roles, super admins, the privilege options and the active blog:
 *   blocked and put back as AbilitySideEffects does, and a violation;
 * - outbound HTTP: refused and its host reported, never a violation.
 *
 * Capabilities are narrowed while armed: editing, deleting or publishing any
 * post other than the target and its recorded revisions maps to
 * do_not_allow, and unfiltered_html and every publish_* and delete_*
 * capability read as not granted.
 *
 * Violations are fixed labels; no site text is reported in them. The
 * outcome is complete once the scope is disarmed. Writes are recorded, not
 * undone, except the blocked privilege writes.
 *
 * Scope: effects made through the WordPress APIs that fire these hooks.
 */
final class AbilityWriteScope
{
    /** Placeholder in an option pattern for the target id. */
    public const TARGET = '{target}';

    /** What a value check is given for an option that did not exist, or no longer does. */
    public const ABSENT = "\0wpmgr_absent_option";

    /** A post other than the target, or a revision of another post, was written. */
    public const V_OTHER_POST = 'other_post_written';

    /** A post other than a recorded revision was deleted. */
    public const V_POST_DELETED = 'post_deleted';

    /** A post other than a recorded revision was trashed. */
    public const V_POST_TRASHED = 'post_trashed';

    /** Post meta of an object other than the target and its recorded revisions was written. */
    public const V_OTHER_META = 'other_post_meta_written';

    /** An object's term relationships changed. */
    public const V_TERMS = 'term_changed';

    /** An option outside the patterns was written. */
    public const V_OPTION = 'option_written';

    /** A user's roles or a super-admin grant changed. */
    public const V_ROLE = 'role_changed';

    /** A user was registered or updated. */
    public const V_USER = 'user_changed';

    /** A blocked write whose label is not a plain word. */
    public const V_BLOCKED = 'blocked_write';

    /** More revisions than the scope records. */
    public const V_REVISIONS = 'too_many_revisions';

    /** More distinct target meta keys than the scope records. */
    public const V_KEYS = 'too_many_target_meta_keys';

    /** Meta capabilities narrowed to the target and its recorded revisions. */
    public const NARROWED_CAPS = ['edit_post', 'delete_post', 'publish_post', 'edit_page', 'delete_page', 'publish_page'];

    /** Most revisions recorded; one more is a violation. */
    private const MAX_REVISIONS = 50;

    /** Most distinct target meta keys recorded; one more is a violation. */
    private const MAX_KEYS = 200;

    private int $targetId;

    /** @var list<string> Option patterns with the target substituted. */
    private array $patterns = [];

    /** @var array<string, callable(mixed, mixed, int): mixed> Option-name prefix => value check. */
    private array $checks = [];

    private bool $armed = false;

    private ?AbilitySideEffects $inner = null;

    /** @var array<int,true> Recorded revision ids, in insert order. */
    private array $revisions = [];

    /** @var array<string,true> Meta keys written on the target, in write order. */
    private array $targetKeys = [];

    /** @var array<string,true> Violation labels. */
    private array $violations = [];

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    /**
     * @param int                 $targetId       Post the save may write.
     * @param list<mixed>         $optionPatterns Option-name patterns the save may write; "*" is the only wildcard.
     * @param array<mixed, mixed> $optionChecks   Option-name prefix => value check: callable(mixed $before, mixed $after, int $targetId), true when that one write is the save's own.
     * @throws \InvalidArgumentException When the post id is not positive, or a check has no plain prefix or is not callable.
     */
    public function __construct(int $targetId, array $optionPatterns, array $optionChecks = [])
    {
        if ($targetId < 1) {
            throw new \InvalidArgumentException('A write scope needs a post id.');
        }
        $this->targetId = $targetId;
        foreach ($optionPatterns as $pattern) {
            if (is_string($pattern) && $pattern !== '') {
                $this->patterns[] = str_replace(self::TARGET, (string) $targetId, $pattern);
            }
        }
        foreach ($optionChecks as $prefix => $check) {
            if (!is_string($prefix) || preg_match('/^[A-Za-z0-9_-]{1,150}$/D', $prefix) !== 1 || !is_callable($check)) {
                throw new \InvalidArgumentException('An option check needs a plain name prefix and a callable.');
            }
            $this->checks[$prefix] = $check;
        }
    }

    /**
     * Arm, run the work, and disarm whatever happens.
     *
     * @param callable $fn Work to run inside the scope.
     * @return mixed What the work returns.
     */
    public function run(callable $fn): mixed
    {
        try {
            $this->arm();

            return $fn();
        } finally {
            $this->disarm();
        }
    }

    /**
     * Start a fresh record and install the hooks.
     *
     * @return void
     */
    public function arm(): void
    {
        $this->disarm();
        $this->revisions  = [];
        $this->targetKeys = [];
        $this->violations = [];
        $this->armed      = true;
        // A checked name is left to its check, write by write, never to the
        // inner recorder's patterns alone.
        $checked = [];
        foreach (array_keys($this->checks) as $prefix) {
            $checked[] = $prefix . '*';
        }
        $this->inner = new AbilitySideEffects(array_merge($this->patterns, $checked), []);
        $this->inner->arm();

        $insert = function ($postId = 0, $post = null, $update = false): void {
            $this->inserted($postId, $post, $update);
        };
        $delete = function ($postId = 0): void {
            if ($this->armed && !isset($this->revisions[self::id($postId)])) {
                $this->violations[self::V_POST_DELETED] = true;
            }
        };
        $trash = function ($postId = 0): void {
            if ($this->armed && !isset($this->revisions[self::id($postId)])) {
                $this->violations[self::V_POST_TRASHED] = true;
            }
        };
        $meta = function ($metaId = 0, $objectId = 0, $key = ''): void {
            $this->metaWritten($objectId, $key);
        };
        $setTerms = function ($objectId = 0, $terms = null, $ttIds = null, $taxonomy = '', $append = false, $oldTtIds = null): void {
            if ($this->armed && self::termsChanged($ttIds, $oldTtIds, (bool) $append)) {
                $this->violations[self::V_TERMS] = true;
            }
        };
        $deletedTerms = function (): void {
            if ($this->armed) {
                $this->violations[self::V_TERMS] = true;
            }
        };
        $metaCap = function ($caps = [], $cap = '', $userId = 0, $args = []) {
            return $this->narrowMetaCap($caps, $cap, $args);
        };
        $userCaps = function ($allcaps = [], $caps = []) {
            return $this->withoutForbiddenCaps($allcaps, $caps);
        };

        $this->hook('wp_insert_post', $insert, 3, PHP_INT_MIN);
        $this->hook('delete_post', $delete, 1, PHP_INT_MIN);
        $this->hook('wp_trash_post', $trash, 1, PHP_INT_MIN);
        foreach (['added_post_meta', 'updated_post_meta', 'deleted_post_meta'] as $tag) {
            $this->hook($tag, $meta, 3, PHP_INT_MIN);
        }
        $this->hook('set_object_terms', $setTerms, 6, PHP_INT_MIN);
        $this->hook('deleted_term_relationships', $deletedTerms, 1, PHP_INT_MIN);
        $this->hook('map_meta_cap', $metaCap, 4, PHP_INT_MAX);
        $this->hook('user_has_cap', $userCaps, 2, PHP_INT_MAX);
        if ($this->checks === []) {
            return;
        }
        $updated = function ($name = '', $before = null, $after = null): void {
            $this->checkedWrite($name, $before, $after);
        };
        $added = function ($name = '', $value = null): void {
            $this->checkedWrite($name, self::ABSENT, $value);
        };
        // Core fires delete_option before the delete, and only for an option
        // that exists, so the value read here is the one being deleted.
        $deleting = function ($name = ''): void {
            if (!$this->armed || $this->checkFor($name) === null) {
                return;
            }
            $before = get_option(self::nameOf($name), self::ABSENT);
            if ($before === self::ABSENT) {
                $this->violations[self::V_OPTION] = true;

                return;
            }
            $this->checkedWrite($name, $before, self::ABSENT);
        };
        $this->hook('updated_option', $updated, 3, PHP_INT_MIN);
        $this->hook('added_option', $added, 2, PHP_INT_MIN);
        $this->hook('delete_option', $deleting, 1, PHP_INT_MIN);
    }

    /**
     * Remove every hook, then let the inner recorder remove its own and put
     * back any privilege option that moved.
     *
     * @return void
     */
    public function disarm(): void
    {
        $this->armed = false;
        foreach ($this->hooks as [$tag, $callback, , $priority]) {
            remove_filter($tag, $callback, $priority);
        }
        $this->hooks = [];
        if ($this->inner !== null) {
            $this->inner->disarm();
        }
    }

    /**
     * Is the scope armed?
     *
     * @return bool
     */
    public function isArmed(): bool
    {
        return $this->armed;
    }

    /**
     * What the scope saw. Complete once the scope is disarmed.
     *
     * @return array{clean:bool,violations:list<string>,http_hosts:list<string>,revision_ids:list<int>,target_meta_keys:list<string>}
     */
    public function outcome(): array
    {
        $violations = $this->violations;
        $hosts      = [];
        if ($this->inner !== null) {
            // The inner post, post meta and term counts are not read: the
            // object-aware records above stand in for them.
            $details = $this->inner->details();
            if ($details['options'] !== []) {
                $violations[self::V_OPTION] = true;
            }
            if ($details['roles'] > 0) {
                $violations[self::V_ROLE] = true;
            }
            if ($details['users'] > 0) {
                $violations[self::V_USER] = true;
            }
            foreach ($details['blocked'] as $label) {
                $label = (string) $label;
                $violations[preg_match('/^[a-z_]{1,64}$/D', $label) === 1 ? $label : self::V_BLOCKED] = true;
            }
            $hosts = $details['http_hosts'];
        }
        $labels = array_map('strval', array_keys($violations));
        sort($labels, SORT_STRING);

        return [
            'clean'            => $labels === [],
            'violations'       => $labels,
            'http_hosts'       => $hosts,
            'revision_ids'     => array_keys($this->revisions),
            'target_meta_keys' => array_map('strval', array_keys($this->targetKeys)),
        ];
    }

    /**
     * One write of an option: a violation when a value check covers its name
     * and does not answer true. A name no check covers is the inner
     * recorder's.
     *
     * @param mixed $name   Option name.
     * @param mixed $before Value before the write, or ABSENT.
     * @param mixed $after  Value after the write, or ABSENT.
     * @return void
     */
    private function checkedWrite($name, $before, $after): void
    {
        if (!$this->armed) {
            return;
        }
        $check = $this->checkFor($name);
        if ($check === null) {
            return;
        }
        try {
            $own = $check($before, $after, $this->targetId) === true;
        } catch (\Throwable $e) {
            $own = false;
        }
        if (!$own) {
            $this->violations[self::V_OPTION] = true;
        }
    }

    /**
     * The value check whose prefix the option name starts with, or null.
     *
     * @param mixed $name Option name.
     * @return callable|null
     */
    private function checkFor($name): ?callable
    {
        $name = self::nameOf($name);
        foreach ($this->checks as $prefix => $check) {
            if (str_starts_with($name, (string) $prefix)) {
                return $check;
            }
        }

        return null;
    }

    /**
     * An option name as the inner recorder reads it: a scalar as text, else ''.
     *
     * @param mixed $name Option name.
     * @return string
     */
    private static function nameOf($name): string
    {
        return is_scalar($name) ? (string) $name : '';
    }

    /**
     * A post row was inserted or updated.
     *
     * @param mixed $postId Post id.
     * @param mixed $post   The post as stored.
     * @param mixed $update Whether the row already existed.
     * @return void
     */
    private function inserted($postId, $post, $update): void
    {
        if (!$this->armed) {
            return;
        }
        $id = self::id($postId);
        if ($id === $this->targetId) {
            return;
        }
        if (isset($this->revisions[$id])) {
            return;
        }
        // A new revision of the target is the save's own; a revision that
        // existed before the scope was armed is not.
        $isRevision = is_object($post) && isset($post->post_type) && $post->post_type === 'revision';
        $parent     = is_object($post) && isset($post->post_parent) ? self::id($post->post_parent) : 0;
        if ($id > 0 && $isRevision && $parent === $this->targetId && !$update) {
            if (count($this->revisions) >= self::MAX_REVISIONS) {
                $this->violations[self::V_REVISIONS] = true;

                return;
            }
            $this->revisions[$id] = true;

            return;
        }
        $this->violations[self::V_OTHER_POST] = true;
    }

    /**
     * A post meta row was added, updated or deleted.
     *
     * @param mixed $objectId Post id the row belongs to.
     * @param mixed $key      Meta key.
     * @return void
     */
    private function metaWritten($objectId, $key): void
    {
        if (!$this->armed) {
            return;
        }
        $id = self::id($objectId);
        if ($id !== $this->targetId) {
            if (!isset($this->revisions[$id])) {
                $this->violations[self::V_OTHER_META] = true;
            }

            return;
        }
        if (!is_string($key) || $key === '' || isset($this->targetKeys[$key])) {
            return;
        }
        if (count($this->targetKeys) >= self::MAX_KEYS) {
            $this->violations[self::V_KEYS] = true;

            return;
        }
        $this->targetKeys[$key] = true;
    }

    /**
     * Narrow the post meta capabilities to the target and its recorded
     * revisions.
     *
     * @param mixed $caps Primitive capabilities mapped so far.
     * @param mixed $cap  Capability asked for.
     * @param mixed $args Arguments of the check; the first is the object.
     * @return mixed
     */
    private function narrowMetaCap($caps, $cap, $args)
    {
        if (!$this->armed || !is_string($cap) || !in_array($cap, self::NARROWED_CAPS, true)) {
            return $caps;
        }
        $id = is_array($args) && array_key_exists(0, $args) ? self::id($args[0]) : 0;
        if ($id > 0 && ($id === $this->targetId || isset($this->revisions[$id]))) {
            return $caps;
        }

        return ['do_not_allow'];
    }

    /**
     * Read unfiltered_html and every publish_* and delete_* capability as not
     * granted.
     *
     * @param mixed $allcaps The user's capabilities.
     * @param mixed $caps    Primitive capabilities asked for.
     * @return mixed
     */
    private function withoutForbiddenCaps($allcaps, $caps)
    {
        if (!$this->armed || !is_array($allcaps)) {
            return $allcaps;
        }
        $names = array_keys($allcaps);
        if (is_array($caps)) {
            $names = array_merge($names, array_values($caps));
        }
        foreach ($names as $name) {
            if (is_string($name) && self::forbidden($name)) {
                $allcaps[$name] = false;
            }
        }

        return $allcaps;
    }

    /**
     * @param string $cap Primitive capability.
     * @return bool
     */
    private static function forbidden(string $cap): bool
    {
        return $cap === 'unfiltered_html' || str_starts_with($cap, 'publish_') || str_starts_with($cap, 'delete_');
    }

    /**
     * Did a term set change the object's relationships? Without the set the
     * object held before, an appended set that is not empty counts as a
     * change; anything unreadable counts as a change.
     *
     * @param mixed $ttIds    Term taxonomy ids set.
     * @param mixed $oldTtIds Term taxonomy ids held before.
     * @param bool  $append   Whether the set was appended.
     * @return bool
     */
    private static function termsChanged($ttIds, $oldTtIds, bool $append): bool
    {
        $now = self::idSet($ttIds);
        $old = self::idSet($oldTtIds);
        if ($now === null || $old === null) {
            return true;
        }
        if ($append) {
            return array_diff_key($now, $old) !== [];
        }

        return $now !== $old;
    }

    /**
     * @param mixed $ids Ids.
     * @return array<int,true>|null Sorted set, or null when an id is unreadable.
     */
    private static function idSet($ids): ?array
    {
        if (!is_array($ids)) {
            return null;
        }
        $set = [];
        foreach ($ids as $id) {
            $n = self::id($id);
            if ($n < 1) {
                return null;
            }
            $set[$n] = true;
        }
        ksort($set);

        return $set;
    }

    /**
     * A positive object id, or 0.
     *
     * @param mixed $value Id, numeric string or object with an ID.
     * @return int
     */
    private static function id($value): int
    {
        if (is_object($value) && isset($value->ID)) {
            $value = $value->ID;
        }
        if (is_int($value)) {
            return max(0, $value);
        }
        if (is_string($value) && preg_match('/^[1-9][0-9]{0,18}$/D', $value) === 1) {
            return (int) $value;
        }

        return 0;
    }

    /**
     * @param string   $tag      Hook.
     * @param callable $callback Callback.
     * @param int      $args     Accepted args.
     * @param int      $priority Priority.
     * @return void
     */
    private function hook(string $tag, callable $callback, int $args, int $priority): void
    {
        add_filter($tag, $callback, $priority, $args);
        $this->hooks[] = [$tag, $callback, $args, $priority];
    }
}
