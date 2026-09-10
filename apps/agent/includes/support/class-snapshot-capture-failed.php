<?php
/**
 * SnapshotCaptureFailed: thrown by SnapshotManager::captureRequired() when no
 * restorable before-state could be recorded.
 *
 * This exists so a caller whose only undo is the snapshot can DEMAND one
 * instead of receiving an empty snapshot id it has to remember to check.
 * SnapshotManager::capture() stays best-effort and keeps returning a receipt
 * — UpdateCommand deliberately degrades to an unprotected apply rather than
 * refusing an update outright, and that behaviour is unchanged.
 *
 * `failureCode()` is one of SnapshotManager's CAPTURE_FAILURE_* constants, so
 * a handler can distinguish an environment problem (no writable snapshot
 * store) from a real copy error. Neither is the same as a legitimately absent
 * source, which is a SUCCESSFUL capture with before_state = absent and never
 * reaches this exception.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * No restorable before-state could be captured.
 */
class SnapshotCaptureFailed extends \RuntimeException
{
    /**
     * One of SnapshotManager's CAPTURE_FAILURE_* constants.
     *
     * @var string
     */
    private string $failureCode;

    /**
     * The full capture receipt, for a handler that wants to log or report it.
     *
     * @var array<string,mixed>
     */
    private array $receipt;

    /**
     * @param string              $message     Human-readable log line from the receipt.
     * @param string              $failureCode One of SnapshotManager's CAPTURE_FAILURE_* constants.
     * @param array<string,mixed> $receipt     The receipt capture() returned.
     */
    public function __construct(string $message, string $failureCode, array $receipt = [])
    {
        parent::__construct($message);
        $this->failureCode = $failureCode;
        $this->receipt     = $receipt;
    }

    /**
     * Machine-readable reason the capture recorded nothing.
     *
     * @return string
     */
    public function failureCode(): string
    {
        return $this->failureCode;
    }

    /**
     * The capture receipt behind this failure.
     *
     * @return array<string,mixed>
     */
    public function receipt(): array
    {
        return $this->receipt;
    }
}
