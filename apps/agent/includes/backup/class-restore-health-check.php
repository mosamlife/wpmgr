<?php
/**
 * RestoreHealthCheck: GH #146 — post-restore boot verification.
 *
 * `RestoreRunner`'s state machine reported `completed` the instant the file
 * and/or DB swap renames succeeded, with nothing checking whether the
 * swapped-in site actually BOOTS. This class is the check RestoreRunner runs
 * (as the `health_check` phase, between `post_hooks` and `maintenance_off`)
 * to close that gap, mirroring the fail-open discipline
 * `UpdateRunner::isComplete()` / GitHub issue #131 already established: a
 * constrained or ambiguous detection environment must NEVER cause a false
 * rollback of a genuinely good restore. Two probes — see `checkDatabase()`
 * (Probe A, DB, always runs, hard-fail-on-error) and `checkLoopback()`
 * (Probe B, the loopback boot/WSOD gate, with a maintenance-mode-aware,
 * fail-open classification — see that method's doc for the full rationale).
 *
 * Verdict: `ok = ProbeA.ok && (ProbeB.ok || ProbeB.inconclusive)`. The
 * caller (`RestoreRunner::runHealthCheck()`) rolls back ONLY on a
 * definitive fatal from either probe — never on an inconclusive/blocked
 * result.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Stateless — every call to run() re-probes fresh. Holds no DB/HTTP
 * collaborators of its own; talks to the live `global $wpdb` and the WP
 * HTTP API directly, exactly like `RestoreCommand::preflightChecks()` does
 * for its own DB ping. The one piece of state it accepts is the WP root
 * path, so it can check for the runner's own `.maintenance` drop-file.
 */
final class RestoreHealthCheck
{
    /** Loopback HTTP probe timeout, seconds. */
    private const LOOPBACK_TIMEOUT_SECONDS = 10;

    /** Capped redirect-follow count for the loopback probes. */
    private const LOOPBACK_MAX_REDIRECTS = 2;

    /** User-Agent identifying these probes in server access logs. */
    private const USER_AGENT = 'WPMgr-Agent-HealthCheck';

    /**
     * Case-insensitive substrings that, found anywhere in a probe response
     * body, are treated as definitive proof of a fatal PHP error rendering
     * to the browser (a "White Screen Of Death" the HTTP status code alone
     * doesn't always signal — a fatal under `display_errors` still often
     * returns 200).
     *
     * @var list<string>
     */
    private const FATAL_BODY_SIGNATURES = [
        'Fatal error',
        'There has been a critical error on this website',
    ];

    /**
     * Case-insensitive markers that, found anywhere in a 2xx response body,
     * count as "recognizable HTML" — a real rendered page, not a blank/
     * truncated response a swallowed fatal (`display_errors=Off`) can leave
     * behind under a 200 status.
     *
     * @var list<string>
     */
    private const HTML_MARKERS = ['<html', '<!doctype', '<body'];

    /** Absolute WP root path — used only to check for `.maintenance`. */
    private string $wpRoot;

    /**
     * @param string $wpRoot Absolute WP root (ABSPATH). Defaults to the real
     *                       `ABSPATH` constant when not supplied.
     */
    public function __construct(string $wpRoot = '')
    {
        $this->wpRoot = $wpRoot !== '' ? $wpRoot : (defined('ABSPATH') ? (string) constant('ABSPATH') : '');
    }

    /**
     * Run both probes and compute the combined verdict.
     *
     * @return array{ok:bool,failures:list<string>,warnings:list<string>}
     */
    public function run(): array
    {
        $failures = [];
        $warnings = [];

        $probeA = $this->checkDatabase();
        if (!$probeA['ok']) {
            $failures = array_merge($failures, $probeA['failures']);
        }

        $probeB = $this->checkLoopback();
        if ($probeB['inconclusive']) {
            $warnings[] = $probeB['detail'];
        } elseif (!$probeB['ok']) {
            $failures[] = $probeB['detail'];
        }

        $ok = $probeA['ok'] && ($probeB['ok'] || $probeB['inconclusive']);

        return [
            'ok'       => $ok,
            'failures' => $failures,
            'warnings' => $warnings,
        ];
    }

    /**
     * Probe B ONLY — same `{ok,failures,warnings}` shape as `run()`, for
     * `RestoreRunner`'s Gate 2 (`health_check_live`), which runs the
     * loopback probe AFTER Probe A already passed in Gate 1
     * (`health_check`). `ok` follows the same fail-open rule as `run()`'s
     * combined verdict: true on either a confident pass OR an inconclusive
     * result — a destructive rollback fires only on a definitive fatal.
     *
     * @return array{ok:bool,failures:list<string>,warnings:list<string>}
     */
    public function checkLoopbackOnly(): array
    {
        $failures = [];
        $warnings = [];

        $probeB = $this->checkLoopback();
        if ($probeB['inconclusive']) {
            $warnings[] = $probeB['detail'];
        } elseif (!$probeB['ok']) {
            $failures[] = $probeB['detail'];
        }

        return [
            'ok'       => $probeB['ok'] || $probeB['inconclusive'],
            'failures' => $failures,
            'warnings' => $warnings,
        ];
    }

    // ==================================================================
    // Probe A — in-process DB check (public: also used as the "quick
    // re-check" RestoreRunner runs immediately after a rollback).
    // ==================================================================

    /**
     * Assert the (post-swap) live tables exist and answer queries. Reads
     * are made directly against `global $wpdb` — freshly, never through
     * `get_option()`/the object cache, which could still hold a pre-swap
     * cached value and mask a genuinely broken options table.
     *
     * @return array{ok:bool,failures:list<string>}
     */
    public function checkDatabase(): array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return ['ok' => false, 'failures' => ['DB probe: $wpdb is not available']];
        }

        $failures = [];

        // $wpdb's declared type here is bare `object` (a runtime interface,
        // not a concrete class) — every property access below is guarded by
        // isset()+is_string() first; PHPStan only needs an explicit ignore
        // for the METHOD calls (get_var()) and the `last_error` property,
        // which are never isset()-narrowed.
        $prefix = isset($wpdb->prefix) && is_string($wpdb->prefix) ? $wpdb->prefix : 'wp_';

        // siteurl — a fresh, uncached read straight off the live options
        // table. A missing/empty value after a restore swap means the
        // options table is either gone or the swap landed on the wrong
        // tables entirely.
        try {
            $optionsTable = $prefix . 'options';
            if (isset($wpdb->options) && is_string($wpdb->options) && $wpdb->options !== '') {
                $optionsTable = $wpdb->options;
            }
            /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
            $siteurl = $wpdb->get_var("SELECT option_value FROM {$optionsTable} WHERE option_name = 'siteurl' LIMIT 1"); // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,PluginCheck.Security.DirectDB.UnescapedDBParameter -- $optionsTable is either the real $wpdb->options property or prefix+the literal 'options', never user input; a fresh uncached read of the just-swapped live table is the entire point of this probe
            /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
            $lastError = (string) $wpdb->last_error;
            if ($lastError !== '') {
                $failures[] = 'DB probe: siteurl query error: ' . substr($lastError, 0, 200);
            } elseif (!is_string($siteurl) || trim($siteurl) === '') {
                $failures[] = 'DB probe: siteurl is empty after the restore swap';
            }
        } catch (\Throwable $e) {
            $failures[] = 'DB probe: siteurl query threw: ' . substr($e->getMessage(), 0, 200);
        }

        // users / posts — presence + answerability, not content
        // correctness. A NULL/empty result is fine (an empty table is a
        // legitimate site state); only a DB-level error (missing table,
        // connection drop) is a failure.
        foreach (['users', 'posts'] as $core) {
            try {
                $table = $prefix . $core;
                if (isset($wpdb->{$core}) && is_string($wpdb->{$core}) && $wpdb->{$core} !== '') {
                    $table = $wpdb->{$core};
                }
                /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
                $wpdb->get_var("SELECT 1 FROM {$table} LIMIT 1"); // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,PluginCheck.Security.DirectDB.UnescapedDBParameter -- $table is either the real $wpdb->users/$wpdb->posts property or prefix+a literal from the hardcoded ['users','posts'] loop, never user input; a fresh uncached read of the just-swapped live table is the entire point of this probe
                /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
                $lastError = (string) $wpdb->last_error;
                if ($lastError !== '') {
                    $failures[] = 'DB probe: ' . $core . ' table query error: ' . substr($lastError, 0, 200);
                }
            } catch (\Throwable $e) {
                $failures[] = 'DB probe: ' . $core . ' table query threw: ' . substr($e->getMessage(), 0, 200);
            }
        }

        return ['ok' => $failures === [], 'failures' => $failures];
    }

    // ==================================================================
    // Probe B — loopback boot probe.
    // ==================================================================

    /**
     * GH #146 two-gate note: `RestoreRunner` runs THIS probe only from Gate
     * 2 (`health_check_live`), which is scheduled AFTER `maintenance_off` —
     * so the `.maintenance`-aware downgrade below normally never triggers
     * in production (the file is already gone by the time this runs). It's
     * kept as harmless belt-and-suspenders: a caller that runs Probe B
     * (directly, or via `run()`) while maintenance IS still active — e.g. a
     * future call site, or a test — still gets the correct fail-open
     * behavior rather than a false rollback trigger.
     *
     * @return array{ok:bool,inconclusive:bool,detail:string}
     */
    private function checkLoopback(): array
    {
        if (!function_exists('wp_remote_get')) {
            return $this->inconclusive('loopback probe unavailable: wp_remote_get missing');
        }

        $targets = [];
        if (function_exists('home_url')) {
            $home = (string) home_url('/');
            if ($home !== '') {
                $targets['home'] = $home;
            }
        }
        if (function_exists('admin_url')) {
            $admin = (string) admin_url();
            if ($admin !== '') {
                $targets['admin'] = $admin;
            }
        }
        if ($targets === []) {
            return $this->inconclusive('loopback probe unavailable: home_url/admin_url missing');
        }

        // GH #146 review (CRITICAL 1): health_check runs BEFORE
        // maintenance_off, deliberately — but that means `.maintenance` is
        // present for the ENTIRE health-check window on every ordinary
        // restore that reaches this phase, and WordPress answers every
        // loopback request with its OWN 503 maintenance page for as long as
        // that file exists. That 503 is core successfully rendering ITS OWN
        // page — not evidence the restore broke anything — so a fatal
        // classification is downgraded to inconclusive whenever the file is
        // present; Probe A remains the destructive gate for this window.
        $maintenanceActive = $this->isMaintenanceModeActive();

        $connectFailures = 0;
        $ambiguous       = [];
        foreach ($targets as $label => $url) {
            $result = $this->fetchOnce($url);
            if ($result['connect_error']) {
                $connectFailures++;
                continue;
            }

            $verdict = $this->classify($result);

            if ($verdict === 'fatal') {
                if ($maintenanceActive) {
                    return $this->inconclusive(
                        $label . '_url probe: ' . $result['detail'] . ' — but .maintenance is active '
                        . '(the runner\'s own maintenance-mode file); a maintenance-mode 5xx proves core booted '
                        . 'far enough to render its own page and is not evidence of a broken restore. '
                        . 'Probe A (DB) remains the destructive gate for this window.'
                    );
                }
                return ['ok' => false, 'inconclusive' => false, 'detail' => $label . '_url probe: ' . $result['detail']];
            }

            if ($verdict === 'pass') {
                // At least one target answered with a confident, recognizable
                // response — the site is booting.
                return ['ok' => true, 'inconclusive' => false, 'detail' => 'loopback probes passed'];
            }

            // 'ambiguous': a 2xx with no recognizable HTML marker. Not a
            // confident pass — record it and keep checking the other target.
            $ambiguous[] = $label . '_url probe: ' . $result['detail'] . ' (2xx but body has no recognizable HTML marker)';
        }

        if ($connectFailures === count($targets)) {
            // GH #146 review (CRITICAL 2): a destructive rollback must fire
            // ONLY on a POSITIVE fatal signal — never on mere unreachability.
            // The wp-cron.php sentinel probe below is kept ONLY to enrich the
            // diagnostic detail (whether the host structurally gates loopback
            // requests, e.g. a membership plugin, vs. a plain connect
            // failure) — its result no longer changes the verdict. The
            // previous "sentinel succeeded -> definitive fatal" branch is
            // deleted entirely: DISABLE_WP_CRON (extremely common on managed
            // hosts) made `sentinelLoopbackGated()` unconditionally report
            // "not gated" without even probing, which turned an ordinary
            // transient connect blip into a destructive rollback on
            // essentially any such host.
            $detail = $this->sentinelLoopbackGated()
                ? 'loopback unreachable on every probe target; reproduces against the wp-cron.php sentinel — '
                    . 'the host structurally gates loopback requests (membership/firewall)'
                : 'loopback unreachable on every probe target — treating as inconclusive; a destructive rollback '
                    . 'requires a positive fatal signal (an actual 5xx or a fatal body signature), never mere '
                    . 'unreachability';
            return $this->inconclusive($detail);
        }

        if ($ambiguous !== []) {
            return $this->inconclusive(
                implode('; ', $ambiguous)
                . ' — treating as inconclusive rather than risking a false rollback on a blank/marker-less 200'
            );
        }

        // Every target either connect-failed or was ambiguous, with no
        // clean pass and no fatal — fail open defensively.
        return $this->inconclusive('loopback probe produced no confident signal');
    }

    /**
     * @return array{ok:true,inconclusive:true,detail:string}
     */
    private function inconclusive(string $detail): array
    {
        return ['ok' => true, 'inconclusive' => true, 'detail' => $detail];
    }

    /**
     * Whether the runner's own `.maintenance` drop-file (written by
     * `RestoreRunner::maintenanceOn()`, cleared by `maintenanceOff()`) is
     * currently present. Same path convention as those two methods.
     */
    private function isMaintenanceModeActive(): bool
    {
        if ($this->wpRoot === '') {
            return false;
        }
        $path = rtrim($this->wpRoot, '/\\') . DIRECTORY_SEPARATOR . '.maintenance';
        return is_file($path);
    }

    /**
     * Single HTTP GET against a probe target with a capped timeout, capped
     * redirects, and TLS verification off (mirrors the agent's other
     * loopback-style probes — the target is the site's OWN URL, dodging a
     * self-signed/staging cert should never itself fail health).
     *
     * @return array{connect_error:bool,code:int,body:string,detail:string}
     */
    private function fetchOnce(string $url): array
    {
        $response = wp_remote_get($url, [
            'timeout'     => self::LOOPBACK_TIMEOUT_SECONDS,
            'redirection' => self::LOOPBACK_MAX_REDIRECTS,
            'sslverify'   => false,
            'blocking'    => true,
            'user-agent'  => self::USER_AGENT,
        ]);

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            // is_wp_error() already confirms $response is a WP_Error, which
            // always exposes get_error_message() — no method_exists() guard needed.
            $msg = (string) $response->get_error_message();
            return [
                'connect_error' => true,
                'code'          => 0,
                'body'          => '',
                'detail'        => 'connect error: ' . substr($msg, 0, 160),
            ];
        }

        $code = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        $body = '';
        if (function_exists('wp_remote_retrieve_body')) {
            $body = (string) wp_remote_retrieve_body($response);
        } elseif (is_array($response) && isset($response['body']) && is_string($response['body'])) {
            $body = $response['body'];
        }

        return [
            'connect_error' => false,
            'code'          => $code,
            'body'          => $body,
            'detail'        => 'HTTP ' . $code,
        ];
    }

    /**
     * Classify a single (non-connect-error) probe result.
     *
     *   'fatal'     — HTTP 5xx, or a body carrying a fatal-error signature.
     *   'ambiguous' — HTTP 2xx but the body is empty or has no recognizable
     *                 HTML marker (a swallowed fatal under
     *                 `display_errors=Off` can leave a blank 200 behind).
     *   'pass'      — everything else: a 2xx with recognizable markup, or
     *                 any 3xx/401/403/other non-5xx (login-walled or
     *                 redirected still proves the web server is alive and
     *                 running PHP for this site).
     *
     * @param array{connect_error:bool,code:int,body:string,detail:string} $result
     */
    private function classify(array $result): string
    {
        if ($result['code'] >= 500 && $result['code'] < 600) {
            return 'fatal';
        }
        if ($result['body'] !== '') {
            foreach (self::FATAL_BODY_SIGNATURES as $needle) {
                if (stripos($result['body'], $needle) !== false) {
                    return 'fatal';
                }
            }
        }
        if ($result['code'] >= 200 && $result['code'] < 300 && !$this->hasRecognizableMarkup($result['body'])) {
            return 'ambiguous';
        }
        return 'pass';
    }

    /**
     * Whether a response body contains at least one recognizable HTML
     * marker. Empty/whitespace-only bodies never do.
     */
    private function hasRecognizableMarkup(string $body): bool
    {
        if (trim($body) === '') {
            return false;
        }
        foreach (self::HTML_MARKERS as $marker) {
            if (stripos($body, $marker) !== false) {
                return true;
            }
        }
        return false;
    }

    /**
     * Diagnostic-only mirror of `BackupCommand::isLoopbackGated()`: probes
     * `/wp-cron.php` with zero redirects. A 3xx or a connect-level WP_Error
     * means the host structurally gates loopback HTTP requests (a
     * membership/privacy plugin, a firewall rule blocking self-requests).
     *
     * GH #146 review (CRITICAL 2): this result is used ONLY to enrich the
     * inconclusive detail message in `checkLoopback()` — it must NEVER be
     * used to promote an all-connect-refused result to a definitive fatal.
     * `DISABLE_WP_CRON` (extremely common on managed hosts) makes this
     * short-circuit to `false` WITHOUT actually probing anything, so
     * treating that `false` as "the host does not gate loopback, therefore
     * refusal is real" previously turned an ordinary transient connect blip
     * into a destructive rollback on any such host.
     */
    private function sentinelLoopbackGated(): bool
    {
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            return false;
        }
        if (!function_exists('wp_remote_get') || !function_exists('site_url')) {
            return false;
        }

        $cronUrl = (string) site_url('/wp-cron.php');
        if ($cronUrl === '' || $cronUrl === '/wp-cron.php') {
            return false;
        }

        $response = wp_remote_get($cronUrl, [
            'timeout'     => 5,
            'redirection' => 0,
            'sslverify'   => false,
            'blocking'    => true,
            'user-agent'  => self::USER_AGENT,
        ]);

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return true;
        }

        $code = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        return $code >= 300 && $code < 400;
    }
}
