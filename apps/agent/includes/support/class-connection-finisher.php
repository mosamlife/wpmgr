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
 * LiteSpeed cannot be reproduced in CI. Two more seams, the SAPI name and the
 * ini reader, are injected for detachReason() for the same reason: PHP_SAPI is
 * a compile-time constant, so a test can only reach that code by being handed
 * the value rather than by redefining it.
 *
 * Reaching the last rung is a slower path to the same outcome, never an unsafe
 * one. Nothing in this agent refuses work because of it. detachReason() exists
 * to SAY which host this is, not to gate anything.
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
     * Named here so that a caller reading the rung finish() returned and a
     * caller asking canDetach() beforehand are reading one list rather than two
     * that can drift apart. It is a vocabulary, not a gate: a caller may prefer
     * a detaching rung, log which one it got, or ignore the distinction
     * entirely, and none of those is this class's business.
     *
     * @var list<string>
     */
    public const DETACHING_RUNGS = ['fpm', 'litespeed'];

    /**
     * Which finish-request function each SAPI is expected to ship, for the SAPIs
     * that ship one at all. Everything absent from this map (apache2handler,
     * cgi, cgi-fcgi, cli, cli-server, and the rest) provides none, and saying
     * that plainly is the whole point of detachReason().
     *
     * fastcgi_finish_request is exposed by the PHP-FPM SAPI only. Plain
     * FastCGI (cgi-fcgi) does NOT carry it, which is why it is not listed here.
     *
     * @var array<string,string>
     */
    private const SAPI_FINISH_FUNCTIONS = [
        'fpm-fcgi'  => 'fastcgi_finish_request',
        'litespeed' => 'litespeed_finish_request',
    ];

    /** @var callable(string):bool */
    private $available;

    /** @var callable(string):void */
    private $invoke;

    /** @var callable():void */
    private $fallback;

    /** @var callable():string */
    private $sapi;

    /** @var callable(string):(string|false) */
    private $iniGet;

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
     * @param callable():string|null     $sapi       Returns this process's SAPI
     *        name. Defaults to PHP_SAPI, which is a compile-time constant and
     *        therefore cannot be faked in a test any other way. Read only by
     *        detachReason().
     * @param callable(string):(string|false)|null $iniGet Reads a php.ini
     *        directive. Defaults to ini_get(). Read only by detachReason(),
     *        and only for disable_functions.
     */
    public function __construct(
        ?callable $available = null,
        ?callable $invoke = null,
        ?callable $fallback = null,
        ?callable $sapi = null,
        ?callable $iniGet = null
    ) {
        $this->available = $available ?? static fn (string $fn): bool => function_exists($fn);
        $this->invoke    = $invoke ?? static function (string $fn): void {
            $fn();
        };
        $this->fallback  = $fallback ?? static function (): void {
            self::defaultFallbackFlush();
        };
        $this->sapi      = $sapi ?? static fn (): string => PHP_SAPI;
        $this->iniGet    = $iniGet ?? static fn (string $name) => ini_get($name);
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
     * Detaching first is strictly better wherever it is possible: the caller
     * gets its response back in milliseconds instead of waiting out the work,
     * and no worker holds a socket open for minutes. It is a preference and an
     * optimisation, which is exactly how WordPress core itself treats the same
     * capability in wp-cron.php ("Don't run cron until the request finishes, if
     * possible", with no else branch). It is not a precondition for doing work,
     * and nothing in this agent may use it as one.
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
     * Why this host has no detaching rung, in one sentence, or '' when it has
     * one. Diagnostic only: nothing branches on this string.
     *
     * IT READS ITS EVIDENCE INSTEAD OF ASSUMING IT. Two very different hosts
     * reach the same last rung. One runs a SAPI that never shipped a
     * finish-request function at all (apache2handler, plain CGI, the CLI
     * server), where there is nothing for an administrator to turn on. The
     * other runs a SAPI that does ship one and has had it switched off through
     * disable_functions, which is a setting somebody can change. Reporting the
     * second sentence on the first kind of host sends an operator to edit a
     * list that was never involved, which is the specific mistake this method
     * exists to stop making.
     *
     * So the disable_functions claim is made ONLY when both halves are true:
     * this SAPI is one that would have shipped the function, AND that exact
     * name is literally in the parsed list. Otherwise it says the SAPI provides
     * none, which is the truth on the overwhelming majority of these hosts.
     *
     * @return string One sentence, or '' when finish() can detach.
     */
    public function detachReason(): string
    {
        if ($this->canDetach()) {
            return '';
        }

        $sapi     = $this->sapiName();
        $expected = self::SAPI_FINISH_FUNCTIONS[$sapi] ?? '';
        $tail     = ' The response is flushed and the worker stays attached to the client until the request ends.';

        if ($expected === '') {
            return 'The ' . $sapi . ' SAPI provides no finish-request function.' . $tail;
        }

        if (in_array($expected, $this->disabledFunctions(), true)) {
            return 'The ' . $sapi . ' SAPI ships ' . $expected
                . ', but disable_functions on this host names it, so it cannot be called.' . $tail;
        }

        return 'The ' . $sapi . ' SAPI normally ships ' . $expected
            . ', but this build does not expose it and disable_functions does not name it.' . $tail;
    }

    /**
     * This process's SAPI name, normalised to the bare token shape PHP_SAPI
     * actually has (letters, digits, dot, dash, underscore), because the
     * sentence detachReason() builds around it is stored and shipped onward.
     *
     * @return string Never empty.
     */
    private function sapiName(): string
    {
        $name = (string) preg_replace('/[^A-Za-z0-9._-]/', '', (string) ($this->sapi)());

        return $name === '' ? 'unknown' : $name;
    }

    /**
     * The disable_functions directive, parsed into lowercase names.
     *
     * The directive is a comma-separated list whose spacing is whatever whoever
     * wrote the ini file used, and PHP function names are case-insensitive, so
     * both are normalised away before any comparison.
     *
     * @return list<string>
     */
    private function disabledFunctions(): array
    {
        $raw = ($this->iniGet)('disable_functions');
        if (!is_string($raw) || $raw === '') {
            return [];
        }

        $names = [];
        foreach (explode(',', $raw) as $name) {
            $name = strtolower(trim($name));
            if ($name !== '') {
                $names[] = $name;
            }
        }

        return $names;
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
