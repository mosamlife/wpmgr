<?php
/**
 * CachePreloadQueueTestRestCommand — loopback-reachability self-test.
 *
 * Wire contract (CP → agent, SIGNED dispatcher):
 *   POST /wp-json/wpmgr/v1/command/cache_preload_queue_test_rest
 *   Body: {}
 *
 * Response: { ok: bool, reachable: bool, status: int, detail: string }
 *
 * Confirms the agent can reach its OWN /wpmgr/v1/preload/run loopback runner
 * route and that the self-HMAC handshake verifies. This is the diagnostic the
 * "Test loopback REST" viewer button surfaces — when loopback POSTs are blocked
 * by the host, the watchdog cron is the only drain and the operator should know.
 *
 * The probe issues ONE blocking loopback POST with a VALID token: if the queue is
 * empty the runner returns 200 { processed: 0 } having warmed nothing, so the
 * test does not perturb queue state. A 403 means the route is reachable but the
 * HMAC failed (mis-derived salt); a transport error means loopback is blocked.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\Preload;
use WPMgr\Agent\Cache\PreloadQueue;

/**
 * Probes the loopback runner route to confirm reachability + HMAC validity.
 */
final class CachePreloadQueueTestRestCommand implements CommandInterface
{
    private CacheManager $cache;

    /**
     * @param CacheManager $cache Page-cache orchestrator (used for symmetry).
     */
    public function __construct(CacheManager $cache)
    {
        $this->cache = $cache;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'cache_preload_queue_test_rest';
    }

    /**
     * Effect: looks like a probe and is not one: it POSTs a VALID runner token to the loopback preload-run
     * route, so a reachable loopback means this call actually drained a preload batch.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: a second probe runs another batch; work is redone, nothing is lost.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Repeatable;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request params (unused).
     * @return array{ok:bool,reachable:bool,status:int,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        if (!function_exists('wp_remote_post') || !function_exists('rest_url')) {
            return ['ok' => false, 'reachable' => false, 'status' => 0, 'detail' => 'wp http unavailable'];
        }

        $queue = $this->cache->preloadQueue();
        $url   = (string) rest_url(ltrim(PreloadQueue::REST_NAMESPACE . PreloadQueue::REST_RUN_ROUTE, '/'));
        if ($url === '') {
            return ['ok' => false, 'reachable' => false, 'status' => 0, 'detail' => 'route url unresolved'];
        }

        $body = function_exists('wp_json_encode')
            ? (string) wp_json_encode([
                'group'    => PreloadQueue::GROUP_NAME,
                'callback' => PreloadQueue::CALLBACK,
                'token'    => $queue->buildRunnerToken(PreloadQueue::GROUP_NAME, PreloadQueue::CALLBACK),
            ])
            : '';

        // Blocking probe (short timeout) — same-host loopback to our own route.
        $response = wp_remote_post($url, [
            'timeout'            => 5,
            'blocking'           => true,
            'redirection'        => 0,
            'sslverify'          => false,
            'reject_unsafe_urls' => true,
            'headers'            => [
                'Content-Type'         => 'application/json',
                Preload::PRELOAD_HEADER => '1',
            ],
            'body'               => $body,
        ]);

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return [
                'ok'        => false,
                'reachable' => false,
                'status'    => 0,
                'detail'    => 'loopback blocked (transport error)',
            ];
        }

        $status = function_exists('wp_remote_retrieve_response_code')
            ? (int) wp_remote_retrieve_response_code($response)
            : 0;

        $reachable = $status > 0;
        $ok        = $status >= 200 && $status < 300;
        $detail    = $ok
            ? 'loopback reachable'
            : ($status === 403 ? 'reachable but HMAC rejected' : 'reachable, status ' . $status);

        return [
            'ok'        => $ok,
            'reachable' => $reachable,
            'status'    => $status,
            'detail'    => $detail,
        ];
    }
}
