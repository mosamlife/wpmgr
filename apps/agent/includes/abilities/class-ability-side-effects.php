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
 * While armed it records, through core's own actions, every option added,
 * updated or deleted (by name only), every post inserted or updated, and
 * every user role set or added. It refuses every outbound HTTP request
 * through the request short-circuit filter at the latest priority, and
 * records the host; a host the entry pins in limits.http_hosts is let
 * through and not recorded. Options whose names match one of the entry's
 * pinned patterns (limits.allowed_option_patterns, "*" is the only
 * wildcard, the match is anchored at both ends) are not recorded.
 *
 * Any record means the read was not a read: the caller withholds the output
 * and refuses the call with the recorded details.
 *
 * Scope: writes made through the WordPress APIs that fire these actions.
 * A direct database write fires none of them and is not seen here.
 */
final class AbilitySideEffects
{
    public const HTTP_REFUSED = 'wpmgr_outbound_http_refused';

    /** Most option names reported, and longest name kept. */
    private const MAX_NAMES = 50;
    private const MAX_NAME  = 191;

    private bool $armed = false;

    /** @var list<string> */
    private array $optionPatterns = [];

    /** @var list<string> Lower-case hosts allowed through. */
    private array $httpHosts = [];

    /** @var list<string> */
    private array $options = [];

    private int $posts = 0;

    private int $roles = 0;

    /** @var list<string> */
    private array $hosts = [];

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
     * Install the recorders.
     *
     * @return void
     */
    public function arm(): void
    {
        $this->disarm();
        $this->options = [];
        $this->posts   = 0;
        $this->roles   = 0;
        $this->hosts   = [];
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
        $http = function ($pre = false, $args = [], $url = '') {
            return $this->http($pre, $url);
        };

        $this->hook('added_option', $option, 1);
        $this->hook('updated_option', $option, 1);
        $this->hook('deleted_option', $option, 1);
        $this->hook('wp_insert_post', $post, 0);
        $this->hook('set_user_role', $role, 0);
        $this->hook('add_user_role', $role, 0);
        $this->hook('pre_http_request', $http, 3, PHP_INT_MAX);
    }

    /**
     * Remove every recorder.
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
     * Was anything recorded?
     *
     * @return bool
     */
    public function detected(): bool
    {
        return $this->options !== [] || $this->posts > 0 || $this->roles > 0 || $this->hosts !== [];
    }

    /**
     * The records, in the shape the control plane counts.
     *
     * @return array{options:list<string>,posts:int,roles:int,http_hosts:list<string>}
     */
    public function details(): array
    {
        return [
            'options'    => $this->options,
            'posts'      => $this->posts,
            'roles'      => $this->roles,
            'http_hosts' => $this->hosts,
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
        $name = substr($name, 0, self::MAX_NAME);
        // A name beyond the cap is not listed; the call is refused all the same.
        if (!in_array($name, $this->options, true) && count($this->options) < self::MAX_NAMES) {
            $this->options[] = $name;
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
        $label = $host !== '' ? $host : '(unparsed)';
        if (!in_array($label, $this->hosts, true) && count($this->hosts) < self::MAX_NAMES) {
            $this->hosts[] = $label;
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
