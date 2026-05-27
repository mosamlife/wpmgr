<?php
/**
 * Backup command (skeleton stub).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * Placeholder for the backup workflow; returns a not-implemented marker.
 */
final class BackupCommand implements CommandInterface
{
    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'backup';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        return ['status' => 'not_implemented', 'command' => $this->name()];
    }
}
