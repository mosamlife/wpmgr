<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

/** Scripted close failures discard pending edits and preserve the durable part. */
class ScriptedCloseZipArchive extends \ZipArchive
{
    /** @param list<int> $failedCalls Close calls that return ER_NOENT. */
    public function __construct(private array $failedCalls, private int &$closeCalls) {}

    public function close(): bool
    {
        if (in_array(++$this->closeCalls, $this->failedCalls, true)) {
            \PHPUnit\Framework\Assert::assertTrue($this->unchangeAll());
            \PHPUnit\Framework\Assert::assertFalse($this->locateName('__scripted_missing_entry__'));
            \PHPUnit\Framework\Assert::assertSame(\ZipArchive::ER_NOENT, $this->status);
            return false;
        }
        return parent::close();
    }
}
