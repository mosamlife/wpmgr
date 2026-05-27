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
