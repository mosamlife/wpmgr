<?php
/**
 * Agent self-update command: BEAT 1 (ARM) of the three-beat self-update.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/agent_self_update
 *   body: {}
 *   response (always HTTP 200):
 *     {
 *       "ok":           bool,
 *       "status":       "not_eligible"|"up_to_date"|"scheduled"|"already_scheduled"|"error",
 *       "from_version": string,
 *       "to_version":   string,   // "" unless status is scheduled/already_scheduled
 *       "detail":       string,
 *       "cron_mode":    "loopback"|"external",
 *       "expires_at":   int       // Unix ts; 0 unless a record was staged
 *     }
 *
 * "scheduled" is an ACKNOWLEDGEMENT, never a success. Nothing on disk has moved
 * when it is returned, and on a site whose loopback cron is broken nothing ever
 * will, which is the SAFE outcome, and must surface at the CP as unconfirmed.
 * The only trustworthy success signal is the NEW code phoning home: the
 * version-changed boot metadata push (beat 3).
 *
 * WHY THIS FILE IS A THIN SHELL
 * -----------------------------
 * `make agent-zip-wporg` physically excludes includes/support/class-update-checker.php
 * from the wp.org distribution build (Guideline 8), but this file SURVIVES that
 * rsync. So it carries two hard rules:
 *
 *   (a) its own WPMGR_WPORG_BUILD guard, and
 *   (b) NO UpdateChecker symbol at file scope. There is no `use` import, no
 *       typed property and no typed parameter naming that class anywhere in
 *       this file. A static reference would send the plugin's autoloader
 *       after a file that does not exist in that build. The collaborator
 *       arrives as a plain `?object` and the symbol is resolved lazily, by
 *       string, behind a class_exists() check inside execute().
 *
 * "not_eligible" is the correct, non-fatal answer whenever the self-updater is
 * absent (wp.org build) or the site is not enrolled.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * Arms a CP-commanded agent self-update. Verify-only: this command never
 * touches a file.
 */
final class AgentSelfUpdateCommand implements CommandInterface
{
    /**
     * wp-option holding the outcome of the last APPLY beat, so the next
     * metadata push carries it to the control plane. Declared HERE rather than
     * on the self-updater because MetadataCommand reads it and must keep
     * working in the wp.org build, where the self-updater does not exist.
     *
     * Shape: ['status'=>string,'from_version'=>string,'to_version'=>string,
     *         'detail'=>string,'at'=>int]
     */
    public const OPTION_RESULT = 'wpmgr_agent_self_update_result';

    /**
     * Fully-qualified self-updater class name, held as a STRING on purpose.
     * See the file header: a real symbol here would be resolved by the
     * autoloader in a build that does not ship the class.
     */
    private const CHECKER_CLASS = 'WPMgr\\Agent\\Support\\UpdateChecker';

    /**
     * The self-updater, or null. Typed `?object` (not the concrete class) so
     * this file names no symbol that the wp.org build omits.
     *
     * @var object|null
     */
    private ?object $updateChecker;

    /**
     * @param object|null $updateChecker Plugin passes its UpdateChecker
     *                                    instance, which is already null on the
     *                                    wp.org distribution build.
     */
    public function __construct(?object $updateChecker = null)
    {
        $this->updateChecker = $updateChecker;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'agent_self_update';
    }

    /**
     * {@inheritDoc}
     *
     * The Router's permission_callback has already enforced the signed-JWT
     * contract (Ed25519 signature, aud=siteId, cmd="agent_self_update", jti
     * anti-replay) via Connector::verifyCommand, so by the time execute() runs
     * the caller is authenticated.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused; the
     *                                    Router already enforced the aud + cmd
     *                                    binding).
     * @param array<string,mixed> $params Request parameters. This command takes
     *                                    none, and IGNORES anything that arrives.
     * @return array<string,mixed> See the wire contract in the file header.
     */
    public function execute(array $claims, array $params): array
    {
        // Body shape: this command takes no parameters, so whatever arrives is
        // ignored rather than rejected. Rejecting an unexpected body buys
        // nothing here (there is no parameter to misread) and turns a harmless
        // wire difference into a failed rollout beat, which is not a trade any
        // fleet-wide command should make. The intent still stands: future
        // options go behind explicit named params read by key, never "accept
        // whatever arrives and act on it".
        unset($params);

        // Rule (a): the wp.org distribution build never self-updates.
        if (defined('WPMGR_WPORG_BUILD') && constant('WPMGR_WPORG_BUILD')) {
            return $this->answer('not_eligible', 'Self-update is not part of this distribution build.');
        }

        // Rule (b): resolve the self-updater lazily, by string. class_exists()
        // is what makes a build that ships this file WITHOUT the self-updater
        // answer instead of fatal.
        $checker = $this->updateChecker;
        if ($checker === null || !class_exists(self::CHECKER_CLASS)) {
            return $this->answer('not_eligible', 'The self-updater is not available in this build.');
        }
        if (!method_exists($checker, 'stageSelfUpdate')) {
            return $this->answer('not_eligible', 'The self-updater does not support staged updates.');
        }

        try {
            $staged = $checker->stageSelfUpdate();
        } catch (\Throwable $e) {
            // ARM moves nothing on disk, so a throw here leaves the site
            // exactly as it was. Report it rather than letting the Router turn
            // it into an opaque 500.
            return $this->answer('error', 'Staging failed: ' . $e->getMessage());
        }

        if (!is_array($staged) || !isset($staged['status']) || !is_string($staged['status'])) {
            return $this->answer('error', 'The self-updater returned an unrecognised result.');
        }

        return $staged;
    }

    /**
     * Build a locally-generated answer in the same shape the self-updater
     * returns, so the CP parses exactly one response schema.
     *
     * @param string $status One of not_eligible|error.
     * @param string $detail Human-readable, non-secret detail.
     * @return array{status:string,ok:bool,from_version:string,to_version:string,detail:string,cron_mode:string,expires_at:int}
     */
    private function answer(string $status, string $detail): array
    {
        return [
            'status'       => $status,
            'ok'           => $status !== 'error',
            'from_version' => defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '',
            'to_version'   => '',
            'detail'       => $detail,
            'cron_mode'    => (defined('DISABLE_WP_CRON') && constant('DISABLE_WP_CRON')) ? 'external' : 'loopback',
            'expires_at'   => 0,
        ];
    }
}
