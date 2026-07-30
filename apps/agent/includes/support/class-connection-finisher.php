<?php
/**
 * ConnectionFinisher: the SAPI-aware "flush the response, keep the worker
 * running" seam shared by every in-process early-ACK command.
 *
 * WordPress agent commands (backup, db_clean, db_orphan_delete, the daily
 * diagnostics push) ACK a control-plane request in well under a second and
 * then continue heavy work in the SAME PHP worker via
 * register_shutdown_function(). Releasing the HTTP response to the client
 * BEFORE that heavy work starts is what keeps the control plane — and any
 * reverse proxy in front of it — from holding the connection open (and
 * eventually timing out) for the full duration of the job.
 *
 * GitHub issue #274: this used to be ONLY fastcgi_finish_request(), gated by
 * function_exists(). That function is PHP-FPM-specific. OpenLiteSpeed's PHP
 * SAPI is `litespeed`, which does NOT expose fastcgi_finish_request() (the
 * LSAPI alias was disabled in php-src) — but DOES expose the equivalent
 * litespeed_finish_request() (LSAPI >= 7.3, present on every current
 * OpenLiteSpeed build). Without that second rung, a LiteSpeed host had no
 * early flush at all: the worker held the HTTP response open for the entire
 * backup, and a reverse proxy in front of the control plane (e.g. Nginx
 * Proxy Manager) 504'd waiting on the header read.
 *
 * Ladder (first available rung wins):
 *   1. fastcgi_finish_request() — PHP-FPM.
 *   2. litespeed_finish_request() — OpenLiteSpeed / LiteSpeed (GH #274).
 *   3. A portable best-effort flush — every other SAPI (mod_php, the CLI
 *      dev server, etc). This does NOT truly detach the connection — there
 *      is no portable equivalent — which is exactly why it is only ever
 *      reached as the last rung.
 *
 * The three collaborators (availability probe, invocation, fallback) are
 * constructor-injected so every rung is unit testable without a real SAPI —
 * LiteSpeed cannot be reproduced in CI.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * SAPI-aware early-flush ladder: fastcgi_finish_request -> litespeed_finish_request -> fallback.
 */
final class ConnectionFinisher
{
    /**
     * The rung names finish() returns when it TRULY detached the connection,
     * as opposed to the portable last rung, which only flushes what is already
     * buffered and leaves the worker attached to the client.
     *
     * Named here so that a caller deciding AFTER the flush (from the rung
     * finish() returned) and a caller deciding BEFORE it (from canDetach())
     * are reading one list rather than two that can drift apart.
     *
     * @var list<string>
     */
    public const DETACHING_RUNGS = ['fpm', 'litespeed'];

    /** @var callable(string):bool */
    private $available;

    /** @var callable(string):void */
    private $invoke;

    /** @var callable():void */
    private $fallback;

    /**
     * @param callable(string):bool|null $available Returns whether the named
     *        global function exists. Defaults to function_exists().
     * @param callable(string):void|null $invoke     Invokes the named global
     *        function with no arguments. Reached ONLY after $available has
     *        already returned true for that same name, so it can never be
     *        asked to dispatch a function that isn't there. Defaults to
     *        calling it directly.
     * @param callable():void|null       $fallback   The last-rung flush, used
     *        when neither finish-request function is available. Defaults to
     *        defaultFallbackFlush().
     */
    public function __construct(?callable $available = null, ?callable $invoke = null, ?callable $fallback = null)
    {
        $this->available = $available ?? static fn (string $fn): bool => function_exists($fn);
        $this->invoke    = $invoke ?? static function (string $fn): void {
            $fn();
        };
        $this->fallback  = $fallback ?? static function (): void {
            self::defaultFallbackFlush();
        };
    }

    /**
     * Release the HTTP response to the client using the best mechanism this
     * SAPI offers.
     *
     * @return string One of 'fpm' | 'litespeed' | 'fallback' — which rung fired.
     */
    public function finish(): string
    {
        if (($this->available)('fastcgi_finish_request')) {
            ($this->invoke)('fastcgi_finish_request'); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- intentional early-flush of the FCGI connection so the caller receives the ACK before the heavy work runs
            return 'fpm';
        }

        if (($this->available)('litespeed_finish_request')) {
            ($this->invoke)('litespeed_finish_request'); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- OpenLiteSpeed/LiteSpeed equivalent of fastcgi_finish_request (GH #274); LSAPI >= 7.3
            return 'litespeed';
        }

        ($this->fallback)();
        return 'fallback';
    }

    /**
     * Whether this SAPI offers a rung that truly detaches the connection.
     * Answers the question WITHOUT flushing anything, so it is safe to ask
     * before the response is ready to be released.
     *
     * finish() reports which rung fired, which is the right shape for a caller
     * that has already committed to the work. It is the wrong shape for a
     * caller whose decision to start the work at all depends on the answer:
     * the CP-commanded self-update has told the control plane what it is about
     * to do by the time it releases the response, so it asks this first and
     * declines the whole operation up front instead of abandoning it after the
     * acknowledgement has gone out.
     *
     * Asks the SAME two questions finish() asks, through the same injected
     * probe, so the two can only ever agree.
     *
     * @return bool True when finish() would return one of DETACHING_RUNGS.
     */
    public function canDetach(): bool
    {
        return ($this->available)('fastcgi_finish_request')
            || ($this->available)('litespeed_finish_request');
    }

    /**
     * Best-effort flush for SAPIs offering neither finish-request function
     * (mod_php, the CLI dev server, etc). This does NOT truly detach the
     * connection — there is no portable equivalent for that — which is why
     * it is only ever reached as the last rung of finish().
     *
     * The ACK body was already echoed by WP REST before this runs (finish()
     * is invoked from a register_shutdown_function callback, which fires
     * AFTER the response body has been sent), so this must NOT — and does
     * NOT — echo a second body. It only flushes what is already buffered and
     * signals the peer that the response is complete.
     *
     * @return void
     */
    private static function defaultFallbackFlush(): void
    {
        ignore_user_abort(true);

        if (!headers_sent()) {
            header('Connection: close');
            header('Content-Encoding: none'); // defeat gzip so the peer sees a complete response
        }

        while (ob_get_level() > 0) {
            @ob_end_flush(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort flush; not all SAPI/OB setups support this
        }
        @flush(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort flush; not all SAPI/OB setups support this
    }
}
