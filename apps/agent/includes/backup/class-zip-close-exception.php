<?php
/**
 * ZIP finalization diagnostics for backup failure reports.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

final class ZipCloseException extends \RuntimeException
{
    private string $part;
    private int $libzipStatus;

    public function __construct(string $part, int $libzipStatus, string $reason)
    {
        $this->part = basename($part);
        $this->libzipStatus = $libzipStatus;
        parent::__construct(sprintf(
            'FilesArchiver: zip close failed (%s; libzip status %d) for %s',
            esc_html($reason),
            $libzipStatus,
            esc_html($this->part)
        ));
    }

    public function part(): string
    {
        return $this->part;
    }

    public function libzipStatus(): int
    {
        return $this->libzipStatus;
    }
}
