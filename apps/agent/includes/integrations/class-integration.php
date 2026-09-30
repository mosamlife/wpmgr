<?php
/**
 * Integration — abstract base for a host/edge-cache purge bridge.
 *
 * Centralises the boilerplate every integration shares:
 *   - hook registration on the WPMgr purge actions (guarded so it is a no-op
 *     without WordPress), and
 *   - the detect()-gated dispatch: when WPMgr purges, we first check the host is
 *     actually present (its class/function/global) and silently return when it
 *     is not, so an un-managed site never makes a single outbound call.
 *
 * A concrete integration implements two things:
 *   - detect(): bool — is this host/layer present on THIS site?
 *   - purgeAll(): void — clear the host's entire cache.
 * and MAY override purgeUrls(array $urls): void to purge by URL (the default
 * falls back to purgeAll()).
 *
 * Reach. A purge can carry the option `origin_only`, which asks for a clear
 * that stays inside this one site. Each integration declares how far its own
 * purge reaches:
 *   - REACH = 'shared' (the default): the purge may reach beyond this site, or
 *     that it does not has not been confirmed. Under `origin_only` it never
 *     runs purgeAll() or purgeUrls(). For a URL clear it may run
 *     purgeUrlsExact() instead, only when a subclass overrides it and
 *     REACH_NOTE is a dated verification note.
 *   - REACH = 'install': the purge reaches only this WordPress install. It
 *     behaves under `origin_only` exactly as it does without it.
 * REACH_NOTE records the evidence. An 'install' row, and a purgeUrlsExact()
 * override, take effect only with a note of the form
 * "Verified YYYY-MM-DD: <what was checked>". Without one the integration runs
 * as 'shared', and the reach test in the suite fails.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * Base class wiring the purge actions to a host-specific cache flush.
 */
abstract class Integration
{
    /** Reach value: the purge stays inside this WordPress install. */
    public const REACH_INSTALL = 'install';

    /** Reach value: the purge may reach beyond this site, or is unconfirmed. */
    public const REACH_SHARED = 'shared';

    /** A verification note: "Verified YYYY-MM-DD: <what was checked>". */
    public const VERIFIED_NOTE_PATTERN = '/^Verified \d{4}-\d{2}-\d{2}: .+/';

    /** Report action: the integration cleared its whole cache. */
    public const ACTION_PURGED_ALL = 'purged_all';

    /** Report action: the integration cleared the given URLs. */
    public const ACTION_PURGED_URLS = 'purged_urls';

    /** Report action: the integration cleared exactly the given URLs. */
    public const ACTION_PURGED_URLS_EXACT = 'purged_urls_exact';

    /** Report action: the integration did not run, because its reach is unconfirmed. */
    public const ACTION_SKIPPED_REACH_UNCONFIRMED = 'skipped_reach_unconfirmed';

    /** Stable identifier used in the purge report. Every concrete class sets it. */
    public const SLUG = '';

    /** How far this integration's purge reaches. See the file header. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH, and for any purgeUrlsExact() override. */
    protected const REACH_NOTE = '';

    /**
     * Stack of purge reports, one frame per purge in flight (a purge hook can
     * start another purge). Empty when no report is being collected; actions
     * are recorded into the top frame.
     *
     * @var list<list<array{slug:string,action:string}>>
     */
    private static array $reportFrames = [];

    /**
     * Register on the WPMgr purge actions. Hooks fire BEFORE WPMgr deletes its
     * own files so the upstream cache is cleared first (it would otherwise
     * immediately re-cache the page WPMgr is about to regenerate).
     */
    public function __construct()
    {
        if (!function_exists('add_action')) {
            return;
        }

        \add_action('wpmgr_purge_urls:before', [$this, 'onPurgeUrls'], 10, 2);
        \add_action('wpmgr_purge_pages:before', [$this, 'onPurgeEverything'], 10, 0);
        \add_action('wpmgr_purge_everything:before', [$this, 'onPurgeEverything'], 10, 1);
    }

    /**
     * Action handler: purge the given URLs from the host cache (host-gated).
     *
     * @param mixed $urls    List of absolute URLs (from the purge action).
     * @param mixed $options Purge options; only `origin_only` is read.
     * @return void
     */
    final public function onPurgeUrls($urls = [], $options = []): void
    {
        if (!$this->detect()) {
            return;
        }
        $clean = $this->normalizeUrls($urls);

        if (self::originOnly($options) && static::effectiveReach() !== self::REACH_INSTALL) {
            if ($clean !== [] && static::exactPurgeEnabled()) {
                $this->purgeUrlsExact($clean);
                $this->record(self::ACTION_PURGED_URLS_EXACT);
                return;
            }
            $this->record(self::ACTION_SKIPPED_REACH_UNCONFIRMED);
            return;
        }

        if ($clean === []) {
            $this->purgeAll();
            $this->record(self::ACTION_PURGED_ALL);
            return;
        }
        $this->purgeUrls($clean);
        $this->record(static::overrides('purgeUrls') ? self::ACTION_PURGED_URLS : self::ACTION_PURGED_ALL);
    }

    /**
     * Action handler: purge the host's entire cache (host-gated).
     *
     * @param mixed $options Purge options; only `origin_only` is read.
     * @return void
     */
    final public function onPurgeEverything($options = []): void
    {
        if (!$this->detect()) {
            return;
        }
        if (self::originOnly($options) && static::effectiveReach() !== self::REACH_INSTALL) {
            $this->record(self::ACTION_SKIPPED_REACH_UNCONFIRMED);
            return;
        }
        $this->purgeAll();
        $this->record(self::ACTION_PURGED_ALL);
    }

    /**
     * Is this host/cache layer present on the current site?
     *
     * @return bool
     */
    abstract protected function detect(): bool;

    /**
     * Purge the host's entire cache.
     *
     * @return void
     */
    abstract protected function purgeAll(): void;

    /**
     * Purge specific URLs from the host cache. Default: fall back to purgeAll().
     *
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        $this->purgeAll();
    }

    /**
     * Purge exactly the given URLs and nothing else, on a cache whose reach is
     * otherwise shared. Runs under `origin_only` only when a subclass overrides
     * it and REACH_NOTE is a dated verification note. The base does nothing.
     *
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrlsExact(array $urls): void
    {
    }

    /**
     * Coerce a purge action payload into a clean list of absolute http(s) URLs.
     *
     * @param mixed $urls Raw payload.
     * @return list<string>
     */
    final protected function normalizeUrls($urls): array
    {
        if (!is_array($urls)) {
            $urls = [$urls];
        }
        $out = [];
        foreach ($urls as $url) {
            if (!is_string($url) || $url === '') {
                continue;
            }
            $scheme = strtolower((string) (wp_parse_url($url, PHP_URL_SCHEME) ?? ''));
            if ($scheme !== 'http' && $scheme !== 'https') {
                continue;
            }
            $out[$url] = true;
        }
        return array_keys($out);
    }

    // -------------------------------------------------------------------------
    // Reach
    // -------------------------------------------------------------------------

    /**
     * This integration's row in the reach table.
     *
     * @return array{slug:string,reach:string,note:string,urls_exact:bool}
     */
    final public static function reachRow(): array
    {
        return [
            'slug'       => (string) static::SLUG,
            'reach'      => (string) static::REACH,
            'note'       => (string) static::REACH_NOTE,
            'urls_exact' => static::overrides('purgeUrlsExact'),
        ];
    }

    /**
     * Reasons this integration's reach declaration may not merge. Empty when
     * the declaration is sound.
     *
     * @return list<string>
     */
    final public static function reachProblems(): array
    {
        $row      = static::reachRow();
        $name     = static::class;
        $verified = self::isVerifiedNote($row['note']);
        $problems = [];

        if ($row['slug'] === '') {
            $problems[] = $name . ' declares no SLUG';
        }
        if ($row['reach'] !== self::REACH_SHARED && $row['reach'] !== self::REACH_INSTALL) {
            $problems[] = $name . ' declares an unknown REACH';
        }
        if (trim($row['note']) === '') {
            $problems[] = $name . ' declares no REACH_NOTE';
        }
        if ($row['reach'] !== self::REACH_SHARED && !$verified) {
            $problems[] = $name . ' is not shared and has no dated Verified note';
        }
        if ($row['urls_exact'] && !$verified) {
            $problems[] = $name . ' overrides purgeUrlsExact() without a dated Verified note';
        }
        return $problems;
    }

    /**
     * Whether a note is a dated verification note.
     *
     * @param string $note Note text.
     * @return bool
     */
    final public static function isVerifiedNote(string $note): bool
    {
        return preg_match(self::VERIFIED_NOTE_PATTERN, $note) === 1;
    }

    /**
     * The reach this integration runs with: 'install' only when declared so
     * with a dated verification note, otherwise 'shared'.
     *
     * @return string
     */
    final protected static function effectiveReach(): string
    {
        if (static::REACH === self::REACH_INSTALL && self::isVerifiedNote((string) static::REACH_NOTE)) {
            return self::REACH_INSTALL;
        }
        return self::REACH_SHARED;
    }

    /**
     * Whether a purgeUrlsExact() override may run under `origin_only`.
     *
     * @return bool
     */
    final protected static function exactPurgeEnabled(): bool
    {
        return static::overrides('purgeUrlsExact') && self::isVerifiedNote((string) static::REACH_NOTE);
    }

    /**
     * Whether the concrete class overrides a method this base class declares.
     *
     * @param string $method Method name.
     * @return bool
     */
    final protected static function overrides(string $method): bool
    {
        try {
            $declaring = (new \ReflectionMethod(static::class, $method))->getDeclaringClass()->getName();
        } catch (\ReflectionException $e) {
            return false;
        }
        return $declaring !== self::class;
    }

    /**
     * Whether the purge options ask for an origin-only clear.
     *
     * @param mixed $options Purge options from the action.
     * @return bool
     */
    private static function originOnly($options): bool
    {
        return is_array($options) && ($options['origin_only'] ?? false) === true;
    }

    // -------------------------------------------------------------------------
    // Purge report
    // -------------------------------------------------------------------------

    /**
     * Start collecting what each detected integration does for one purge. Safe
     * to nest: each call opens its own frame, closed by the matching endReport().
     *
     * @return void
     */
    final public static function beginReport(): void
    {
        self::$reportFrames[] = [];
    }

    /**
     * Close the innermost report and return it (empty when none is open).
     *
     * @return list<array{slug:string,action:string}>
     */
    final public static function endReport(): array
    {
        $report = array_pop(self::$reportFrames);
        return $report ?? [];
    }

    /**
     * Append this integration's action to the report being collected, if any.
     *
     * @param string $action One of the ACTION_* values.
     * @return void
     */
    private function record(string $action): void
    {
        $top = array_key_last(self::$reportFrames);
        if ($top === null) {
            return;
        }
        self::$reportFrames[$top][] = ['slug' => (string) static::SLUG, 'action' => $action];
    }
}
