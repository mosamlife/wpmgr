<?php
/**
 * CommandInterface: contract for all WPMgr agent commands.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * A command executed in response to a verified, signed control-plane request.
 */
interface CommandInterface
{
    /**
     * Stable command identifier (used as the REST route slug).
     *
     * @return string
     */
    public function name(): string;

    /**
     * What this command does to the managed site.
     *
     * Declared, never inferred. There is deliberately no default and no base
     * class or trait supplying one: a default is exactly how a new command
     * arrives silently unlabelled, which is the failure this contract exists to
     * prevent. Adding a command without answering this question does not
     * compile, and PHPStan says so before PHP does.
     *
     * A command whose effect depends on its parameters declares its WORST case
     * and explains that in the comment on its implementation, so a caller can
     * trust the label without also parsing the request body.
     *
     * This is metadata. Nothing in the agent reads it to allow or refuse
     * anything.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect;

    /**
     * Whether running this command again is safe when the caller never learned
     * the outcome of the first run.
     *
     * A SEPARATE axis from effect(): a backup is a Write and safe to repeat, a
     * restore is Destructive and is not, a file delete is Destructive and is.
     * One combined score cannot say all three, which is why there are two.
     *
     * Same rules as effect(): no default, and the worst case across a
     * command's accepted arguments.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability;

    /**
     * Execute the command.
     *
     * Implementations receive the validated JWT claim set and the request
     * parameters. They MUST NOT trust unsigned input and MUST NOT emit secrets.
     *
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed> Serializable result payload.
     */
    public function execute(array $claims, array $params): array;
}
