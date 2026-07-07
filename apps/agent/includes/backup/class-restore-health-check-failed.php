<?php
/**
 * RestoreHealthCheckFailed: GH #146 — thrown by EITHER of RestoreRunner's
 * two health-check gates (`runHealthCheck()` — Gate 1, in-process DB probe;
 * `runHealthCheckLive()` — Gate 2, loopback WSOD probe on the real
 * post-maintenance site) when a definitive fatal is found, AFTER the
 * combined files+DB rollback has already been performed.
 *
 * Caught by `RestoreRunner::run()`'s existing top-level catch, which
 * already reports `failed` + `last_error` + `failed_in` for every
 * exception type (`failed_in` distinguishes which gate: `health_check` vs
 * `health_check_live`); this subclass exists ONLY so that catch block can
 * also attach the `rolled_back:{files,db,reason}` detail without
 * inspecting message strings or re-deriving it. `reason` is one of
 * `db_unhealthy_post_restore` (Gate 1), `wsod_post_restore` (Gate 2), or
 * `db_rollback_unavailable` (either gate, when the DB was swapped but no
 * rollback source exists — neither leg is touched in that case).
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Carries which rollback legs actually completed alongside the standard
 * RuntimeException message.
 */
final class RestoreHealthCheckFailed extends \RuntimeException
{
    /** @var array{files:bool,db:bool,reason?:string} */
    private array $rolledBack;

    /**
     * @param string                              $message    Human-readable failure summary.
     * @param array{files:bool,db:bool,reason?:string} $rolledBack Which rollback legs completed
     *        (`reason` is set instead when neither leg was even attempted,
     *        e.g. `db_rollback_unavailable`).
     */
    public function __construct(string $message, array $rolledBack)
    {
        parent::__construct($message);
        $this->rolledBack = $rolledBack;
    }

    /**
     * @return array{files:bool,db:bool,reason?:string}
     */
    public function rolledBack(): array
    {
        return $this->rolledBack;
    }
}
