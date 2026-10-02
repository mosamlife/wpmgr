<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Side-effect recorder for a call that is declared to only read.
 *
 * While armed it records, through core's own hooks:
 * - every option added, updated or deleted (by name only), except names that
 *   match one of the entry's pinned patterns (limits.allowed_option_patterns,
 *   "*" is the only wildcard, the match is anchored at both ends);
 * - every post inserted, updated or deleted;
 * - every user role set, added or removed, every user registered or updated,
 *   and every super-admin grant;
 * - every outbound HTTP request made through the WordPress HTTP API, which it
 *   also refuses at the latest priority of the request short-circuit filter;
 *   a host the entry pins in limits.http_hosts is let through unrecorded.
 *
 * Privilege writes are blocked as well as recorded: user capability and user
 * level meta, the stored role definitions and the network super-admin list
 * keep their values for the duration of the call.
 *
 * Any record means the read was not a read: the caller withholds the output
 * and refuses the call. Writes other than the blocked ones are not undone.
 *
 * Reported names are site text, so only names in a safe character set are
 * reported as is; any other name is reported as a fixed placeholder, one per
 * name, so the counts stay true.
 *
 * Scope: effects made through the WordPress APIs that fire these hooks.
 */
final class AbilitySideEffects
{
    public const HTTP_REFUSED = 'wpmgr_outbound_http_refused';

    /** Stand-in for a reported name outside the safe character set. */
    public const UNPRINTABLE = '(unprintable)';

    /** Most names reported per list. */
    private const MAX_NAMES = 50;

    private const RE_OPTION = '/^[A-Za-z0-9._:-]{1,191}$/';
    private const RE_HOST   = '/^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/';

    /** User meta keys (suffix of the prefixed key) whose writes are blocked. */
    private const BLOCKED_META = '/(?:^|_)(capabilities|user_level)$/';

    private bool $armed = false;

    /** @var list<string> */
    private array $optionPatterns = [];

    /** @var list<string> Lower-case hosts allowed through. */
    private array $httpHosts = [];

    /** @var array<string,true> Raw option names. */
    private array $options = [];

    private int $posts = 0;

    private int $roles = 0;

    private int $users = 0;

    /** @var array<string,true> Raw hosts. */
    private array $hosts = [];

    /** @var array<string,true> Fixed labels of blocked writes. */
    private array $blocked = [];

    /** @var list<array{0:string,1:callable,2:int,3:int}> */
    private array $hooks = [];

    /**
     * @param list<mixed> $optionPatterns Pinned option-name patterns.
     * @param list<mixed> $httpHosts      Pinned outbound hosts.
     */
    public function __construct(array $optionPatterns = [], array $httpHosts = [])
    {
        foreach ($optionPatterns as $p) {
            if (is_string($p) && $p !== '' && $p !== '*') {
                $this->optionPatterns[] = $p;
            }
        }
        foreach ($httpHosts as $h) {
            if (is_string($h) && $h !== '') {
                $this->httpHosts[] = strtolower($h);
            }
        }
    }

    /**
     * Install the recorders and the blocks.
     *
     * @return void
     */
    public function arm(): void
    {
        $this->disarm();
        $this->options = [];
        $this->posts   = 0;
        $this->roles   = 0;
        $this->users   = 0;
        $this->hosts   = [];
        $this->blocked = [];
        $this->armed   = true;

        $option = function ($name = null): void {
            $this->option($name);
        };
        $post = function (): void {
            if ($this->armed) {
                $this->posts++;
            }
        };
        $role = function (): void {
            if ($this->armed) {
                $this->roles++;
            }
        };
        $user = function (): void {
            if ($this->armed) {
                $this->users++;
            }
        };
        $http = function ($pre = false, $args = [], $url = '') {
            return $this->http($pre, $url);
        };
        $meta = function ($check = null, $objectId = 0, $key = '') {
            if ($this->armed && is_string($key) && preg_match(self::BLOCKED_META, $key, $m) === 1) {
                $this->blocked['user_' . $m[1] . '_meta'] = true;

                return false;
            }

            return $check;
        };
        $keepOld = function (string $label): callable {
            return function ($value = null, $old = null) use ($label) {
                if (!$this->armed) {
                    return $value;
                }
                $this->blocked[$label] = true;

                return $old;
            };
        };

        $this->hook('added_option', $option, 1);
        $this->hook('updated_option', $option, 1);
        $this->hook('deleted_option', $option, 1);
        $this->hook('wp_insert_post', $post, 0);
        $this->hook('delete_post', $post, 0);
        $this->hook('deleted_post', $post, 0);
        $this->hook('set_user_role', $role, 0);
        $this->hook('add_user_role', $role, 0);
        $this->hook('remove_user_role', $role, 0);
        $this->hook('granted_super_admin', $role, 0);
        $this->hook('user_register', $user, 0);
        $this->hook('profile_update', $user, 0);
        $this->hook('pre_http_request', $http, 3, PHP_INT_MAX);
        foreach (['update_user_metadata', 'add_user_metadata', 'delete_user_metadata'] as $tag) {
            $this->hook($tag, $meta, 3, PHP_INT_MAX);
        }
        $prefix = self::dbPrefix();
        if ($prefix !== '') {
            $this->hook('pre_update_option_' . $prefix . 'user_roles', $keepOld('user_roles_option'), 2, PHP_INT_MAX);
        }
        $this->hook('pre_update_site_option_site_admins', $keepOld('site_admins'), 2, PHP_INT_MAX);
    }

    /**
     * Remove every recorder and block.
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
    }

    /**
     * Was anything recorded or blocked?
     *
     * @return bool
     */
    public function detected(): bool
    {
        return $this->options !== [] || $this->posts > 0 || $this->roles > 0 || $this->users > 0
            || $this->hosts !== [] || $this->blocked !== [];
    }

    /**
     * The records, in the shape the control plane counts. Names outside the
     * safe character set are replaced by UNPRINTABLE.
     *
     * @return array{options:list<string>,posts:int,roles:int,users:int,http_hosts:list<string>,blocked:list<string>}
     */
    public function details(): array
    {
        $options = [];
        foreach (array_keys($this->options) as $name) {
            $name      = (string) $name;
            $options[] = preg_match(self::RE_OPTION, $name) === 1 ? $name : self::UNPRINTABLE;
        }
        $hosts = [];
        foreach (array_keys($this->hosts) as $host) {
            $host    = (string) $host;
            $hosts[] = preg_match(self::RE_HOST, $host) === 1 ? $host : self::UNPRINTABLE;
        }

        return [
            'options'    => $options,
            'posts'      => $this->posts,
            'roles'      => $this->roles,
            'users'      => $this->users,
            'http_hosts' => $hosts,
            'blocked'    => array_keys($this->blocked),
        ];
    }

    /**
     * @param mixed $name Option name.
     * @return void
     */
    private function option($name): void
    {
        if (!$this->armed) {
            return;
        }
        $name = is_scalar($name) ? (string) $name : '';
        foreach ($this->optionPatterns as $pattern) {
            if (self::matches($pattern, $name)) {
                return;
            }
        }
        // A name beyond the cap is not listed; the call is refused all the same.
        if (count($this->options) < self::MAX_NAMES) {
            $this->options[$name] = true;
        }
    }

    /**
     * Refuse and record an outbound request, unless its host is pinned.
     *
     * @param mixed $pre Short-circuit value so far.
     * @param mixed $url Request URL.
     * @return mixed
     */
    private function http($pre, $url)
    {
        if (!$this->armed) {
            return $pre;
        }
        $host = '';
        if (is_string($url)) {
            $parsed = function_exists('wp_parse_url') ? wp_parse_url($url, PHP_URL_HOST) : null;
            $host   = is_string($parsed) ? strtolower($parsed) : '';
        }
        if ($host !== '' && in_array($host, $this->httpHosts, true)) {
            return $pre;
        }
        if (count($this->hosts) < self::MAX_NAMES) {
            $this->hosts[$host === '' ? self::UNPRINTABLE : $host] = true;
        }

        return new \WP_Error(self::HTTP_REFUSED, 'outbound requests are refused during a read');
    }

    /**
     * Anchored match where "*" is the only wildcard.
     *
     * @param string $pattern Pattern.
     * @param string $name    Option name.
     * @return bool
     */
    public static function matches(string $pattern, string $name): bool
    {
        $re = '/^' . implode('.*', array_map(static fn ($s) => preg_quote($s, '/'), explode('*', $pattern))) . '$/s';

        return preg_match($re, $name) === 1;
    }

    /**
     * The site's table prefix, or '' when unknown.
     *
     * @return string
     */
    private static function dbPrefix(): string
    {
        $db     = $GLOBALS['wpdb'] ?? null;
        $prefix = is_object($db) && isset($db->prefix) && is_string($db->prefix) ? $db->prefix : '';

        return preg_match('/^[A-Za-z0-9_]{1,64}$/', $prefix) === 1 ? $prefix : '';
    }

    /**
     * @param string   $tag      Hook.
     * @param callable $callback Callback.
     * @param int      $args     Accepted args.
     * @param int      $priority Priority.
     * @return void
     */
    private function hook(string $tag, callable $callback, int $args, int $priority = PHP_INT_MIN): void
    {
        add_filter($tag, $callback, $priority, $args);
        $this->hooks[] = [$tag, $callback, $args, $priority];
    }
}
