<?php

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

/** Test seam preserving native flock's by-reference contention output. */
function flock($handle, int $operation, &$wouldBlock = null): bool
{
    return \flock($handle, $operation, $wouldBlock);
}
