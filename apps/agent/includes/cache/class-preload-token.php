<?php
/**
 * PreloadToken — the site's own token that marks a request as a preload.
 *
 * The agent warms pages by fetching them from itself (the preloader, the RUCSS
 * compute pass). Such a request carries the preload header, and the serving
 * drop-in renders it fresh and stores the result instead of answering it from
 * the cache. The drop-in recognises a preload by this token: random, generated
 * by the agent on first use, kept in its own option (never in the cache config
 * the control plane pushes, which would overwrite it), inlined into the
 * drop-in's config when the drop-in is rendered, and compared there with
 * hash_equals(). A request that carries the header without the token is an
 * ordinary visitor request.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

if (!defined('ABSPATH')) {
    exit;
}

/**
 * Creates and reads the per-site preload token.
 */
final class PreloadToken
{
    /** Option holding the token (not autoloaded: read only to render or fetch). */
    public const OPTION = 'wpmgr_cache_preload_token';

    /** Random bytes in a token; it is stored and sent as lower-case hex. */
    private const BYTES = 32;

    /**
     * The site's token, created and stored on first use. '' when it can be
     * neither read nor stored, in which case no request is treated as a
     * preload: the drop-in only recognises a well-formed token.
     *
     * @return string 64 lower-case hex characters, or ''.
     */
    public static function get(): string
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return '';
        }

        $stored = get_option(self::OPTION, '');
        if (self::isWellFormed($stored)) {
            return $stored;
        }

        try {
            $fresh = bin2hex(random_bytes(self::BYTES));
        } catch (\Throwable $e) {
            return ''; // no CSPRNG available: better no token than a weak one
        }

        return update_option(self::OPTION, $fresh, false) ? $fresh : '';
    }

    /**
     * Whether a value has the shape of a token this class creates.
     *
     * @param mixed $value Candidate.
     * @return bool
     */
    public static function isWellFormed($value): bool
    {
        return is_string($value) && preg_match('/^[a-f0-9]{64}$/', $value) === 1;
    }
}
