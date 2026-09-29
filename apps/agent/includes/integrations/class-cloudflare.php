<?php
/**
 * Cloudflare — purge the site-owner's own Cloudflare zone.
 *
 * This is the HOST / site-owner Cloudflare (configured locally via the official
 * "Cloudflare" plugin), which is distinct from the WPMgr control-plane operator
 * CDN purge — both can run, and clearing both is correct: the operator CDN sits
 * in front of WPMgr's infra, this zone sits in front of the customer's origin.
 *
 * We only act when a LOCAL Cloudflare config is present and self-sufficient:
 *   - the official Cloudflare plugin's hook object (\CF\WordPress\Hooks +
 *     the `$cloudflareHooks` global / `purgeCacheEverything`), OR
 *   - a locally-defined API config (CLOUDFLARE_API_TOKEN/EMAIL + CLOUDFLARE_ZONE
 *     constants) we can call directly.
 * If neither exists we NO-OP — WPMgr never invents Cloudflare credentials.
 *
 * Uses Cloudflare's public v4 purge_cache endpoint and the official plugin's
 * published hook surface.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * Cloudflare zone purger (site-owner config only).
 */
final class Cloudflare extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'cloudflare';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Shared: a zone purge covers every site in the zone. An exact-URL purge is a candidate and is not yet confirmed.';

    /** Max files-by-URL Cloudflare accepts in one purge call. */
    private const MAX_URLS = 30;

    /**
     * Present only when a local Cloudflare config can drive a purge.
     *
     * @return bool
     */
    protected function detect(): bool
    {
        return $this->officialPluginHooks() !== null || $this->apiConfig() !== null;
    }

    /**
     * Purge the entire zone.
     *
     * @return void
     */
    protected function purgeAll(): void
    {
        $hooks = $this->officialPluginHooks();
        if ($hooks !== null && method_exists($hooks, 'purgeCacheEverything')) {
            $hooks->purgeCacheEverything();
            return;
        }

        $cfg = $this->apiConfig();
        if ($cfg !== null) {
            $this->api($cfg, ['purge_everything' => true]);
        }
    }

    /**
     * Purge specific URLs from the zone (files-by-URL).
     *
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        $hooks = $this->officialPluginHooks();
        if ($hooks !== null && method_exists($hooks, 'purgeCacheByRelevantURLs')) {
            $hooks->purgeCacheByRelevantURLs($urls);
            return;
        }

        $cfg = $this->apiConfig();
        if ($cfg !== null) {
            $this->api($cfg, ['files' => array_slice(array_values($urls), 0, self::MAX_URLS)]);
        }
    }

    /**
     * The official Cloudflare plugin's hook object, if loaded.
     *
     * @return object|null
     */
    private function officialPluginHooks(): ?object
    {
        if (!class_exists('\CF\WordPress\Hooks')) {
            return null;
        }
        if (isset($GLOBALS['cloudflareHooks']) && is_object($GLOBALS['cloudflareHooks'])) {
            return $GLOBALS['cloudflareHooks'];
        }
        return null;
    }

    /**
     * A locally-defined direct API config (token/email + zone), or null.
     *
     * @return array{token:string,email:string,zone:string}|null
     */
    private function apiConfig(): ?array
    {
        $zone = defined('CLOUDFLARE_ZONE') ? (string) \CLOUDFLARE_ZONE : '';
        if ($zone === '') {
            return null;
        }
        $token = defined('CLOUDFLARE_API_TOKEN') ? (string) \CLOUDFLARE_API_TOKEN : '';
        $email = defined('CLOUDFLARE_EMAIL') ? (string) \CLOUDFLARE_EMAIL : '';
        $key   = defined('CLOUDFLARE_API_KEY') ? (string) \CLOUDFLARE_API_KEY : '';

        if ($token === '' && ($email === '' || $key === '')) {
            return null;
        }
        return ['token' => $token, 'email' => $email, 'zone' => $zone, 'key' => $key];
    }

    /**
     * POST a purge body to the Cloudflare v4 API.
     *
     * @param array{token:string,email:string,zone:string,key?:string} $cfg  Resolved config.
     * @param array<string,mixed>                                       $body Purge body.
     * @return void
     */
    private function api(array $cfg, array $body): void
    {
        if (!function_exists('wp_remote_post')) {
            return;
        }
        $headers = ['Content-Type' => 'application/json'];
        if ($cfg['token'] !== '') {
            $headers['Authorization'] = 'Bearer ' . $cfg['token'];
        } else {
            $headers['X-Auth-Email'] = $cfg['email'];
            $headers['X-Auth-Key']   = (string) ($cfg['key'] ?? '');
        }

        \wp_remote_post(
            'https://api.cloudflare.com/client/v4/zones/' . rawurlencode($cfg['zone']) . '/purge_cache',
            [
                'method'   => 'POST',
                'timeout'  => 5,
                'blocking' => false,
                'headers'  => $headers,
                'body'     => (string) wp_json_encode($body),
            ]
        );
    }
}
