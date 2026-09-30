<?php
/**
 * Integrations — host & edge cache purge bridge loader.
 *
 * When WPMgr purges its own on-disk page cache it ALSO needs to clear whatever
 * server-side / edge cache the managed host runs in front of PHP (Varnish, a
 * host control-panel cache, an operator Cloudflare zone, …). Otherwise the host
 * keeps serving the stale HTML it captured and WPMgr looks broken even though
 * its files are gone.
 *
 * Each integration:
 *   - registers in its own constructor on `wpmgr_purge_urls:before` and
 *     `wpmgr_purge_everything:before` (and, where useful, `wpmgr_purge_pages`),
 *   - detects its host/layer through that host's OWN public class / function /
 *     global (never a fragile header sniff), and
 *   - NO-OPS silently when the host is not present — no errors, no outbound
 *     calls.
 *
 * This loader simply instantiates every integration on boot. Detection is the
 * integration's job, so a site only ever talks to the one cache it actually
 * runs behind. Booting is idempotent and safe to call without WordPress loaded
 * (add_action is guarded inside each integration).
 *
 * The host-detection signals (which class/function/global identifies each host)
 * are public, host-published integration points.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * Boots all host/edge-cache purge integrations.
 */
final class Integrations
{
    /**
     * Fully-qualified integration class names to instantiate on boot. Order is
     * irrelevant — each guards on its own host and no-ops otherwise.
     *
     * @var list<class-string<Integration>>
     */
    private const INTEGRATIONS = [
        CloudPanel::class,
        Varnish::class,
        Cloudflare::class,
        Kinsta::class,
        SiteGround::class,
        WPEngine::class,
        Cloudways::class,
        RunCloud::class,
        GridPane::class,
        SpinupWP::class,
        RocketNet::class,
        WPCloud::class,
    ];

    /** Guards against double-booting within a single request. */
    private bool $booted = false;

    /**
     * Every integration class this loader boots.
     *
     * @return list<class-string<Integration>>
     */
    public static function classes(): array
    {
        return self::INTEGRATIONS;
    }

    /**
     * The reach table: one row per integration, in boot order.
     *
     * @return list<array{slug:string,reach:string,note:string,urls_exact:bool}>
     */
    public static function reachTable(): array
    {
        $rows = [];
        foreach (self::INTEGRATIONS as $class) {
            if (class_exists($class)) {
                $rows[] = $class::reachRow();
            }
        }
        return $rows;
    }

    /**
     * Instantiate every integration. Each one wires its own purge hooks in its
     * constructor and detects its host lazily when a purge actually fires.
     *
     * @return void
     */
    public function boot(): void
    {
        if ($this->booted) {
            return;
        }
        $this->booted = true;

        foreach (self::INTEGRATIONS as $class) {
            // Defensive: a missing class never aborts the rest of the chain.
            if (class_exists($class)) {
                new $class();
            }
        }
    }
}
