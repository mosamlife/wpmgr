<?php
/**
 * GH #529: ServerConfigWriter::uninstall() must not report success for a block
 * it could not verify was removed.
 *
 * THE DEFECT SHAPE, AND WHY IT IS WORTH ITS OWN FILE. A true return from
 * uninstall() is a claim that the managed block is gone from disk.
 * HardeningModule::writeServerConfig() converts that claim into a
 * stampServerRev() call, and the stamp is what disarms
 * refreshServerConfigIfStale() until the next agent version bump. So a false
 * success here does not merely fail to remove the block — it records the
 * failure as completed work and suppresses every future repair, while Apache
 * goes on serving the stale block. On a site whose stale block 403s the
 * recovery routes, that is the difference between "repaired on the next boot"
 * and "stranded until someone edits .htaccess by hand".
 *
 * This is the same defect fixed on the INSTALL path in 264e0786 ("stamp the
 * server block only when the write succeeded"), reappearing on the uninstall
 * path. The install half led the stamp with the write; this half returned true
 * when the file existed but could not be read.
 *
 * An unreadable .htaccess is transient far more often than permanent — a
 * partial upgrade, a deploy mid-flight, an SELinux relabel, a lock held
 * elsewhere — which is precisely why it must be retried rather than stamped.
 *
 * @package WPMgr\Agent\Tests\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Security;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Security\ServerConfigWriter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Security\ServerConfigWriter::uninstall
 */
final class ServerConfigUninstallStampTest extends TestCase
{
    private string $dir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->dir = sys_get_temp_dir() . '/wpmgr-529-uninstall-' . bin2hex(random_bytes(6));
        mkdir($this->dir, 0777, true);

        // htaccessPath() resolves through get_home_path() first.
        Functions\when('get_home_path')->justReturn($this->dir . '/');
    }

    protected function tear_down(): void
    {
        $path = $this->dir . '/.htaccess';
        if (file_exists($path)) {
            @chmod($path, 0644);
            @unlink($path);
        }
        if ($this->dir !== '' && is_dir($this->dir)) {
            @rmdir($this->dir);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /** A .htaccess carrying the managed block plus unrelated core rules. */
    private function writeHtaccessWithBlock(): string
    {
        $content = "# BEGIN WordPress\nRewriteRule . /index.php [L]\n# END WordPress\n"
            . ServerConfigWriter::BEGIN . "\n"
            . "<IfModule mod_authz_core.c>\nRequire all denied\n</IfModule>\n"
            . ServerConfigWriter::END . "\n";

        $path = $this->dir . '/.htaccess';
        file_put_contents($path, $content);
        return $path;
    }

    // =========================================================================
    // RED -> GREEN: an unreadable file is a failure to retry, not a success.
    // =========================================================================

    /**
     * THE BUG. File exists, holds the managed block, cannot be read. Before the
     * fix this returned true, HardeningModule stamped the current revision, and
     * the block stayed on disk forever.
     */
    public function test_unreadable_htaccess_is_reported_as_failure(): void
    {
        $path = $this->writeHtaccessWithBlock();
        chmod($path, 0000);

        // Precondition, asserted rather than skipped: if the process can still
        // read a 0000 file (running as root) this test proves nothing, and it
        // must say so loudly instead of passing green.
        $this->assertFalse(
            @file_get_contents($path),
            'precondition failed: a 0000-mode file is still readable by this process '
            . '(running as root?), so this test cannot exercise the unreadable path'
        );
        $this->assertTrue(is_file($path), 'the file must still exist for this to be the unreadable case');

        $result = (new ServerConfigWriter())->uninstall();

        $this->assertFalse(
            $result,
            'uninstall() must report failure when it cannot read the file, so the caller '
            . 'retries on the next boot instead of stamping the revision as current'
        );

        // And the block is demonstrably still there once readable again.
        chmod($path, 0644);
        $this->assertStringContainsString(
            ServerConfigWriter::BEGIN,
            (string) file_get_contents($path),
            'the managed block is still on disk — which is exactly why true would have been a lie'
        );
    }

    // =========================================================================
    // OVER-FIRE: every genuine success must still return true. A uninstall()
    // that returned false more often would re-arm the repair on healthy sites
    // and rewrite .htaccess on every boot.
    // =========================================================================

    /**
     * No .htaccess at all: nothing to remove is a genuine success.
     */
    public function test_absent_htaccess_still_returns_true(): void
    {
        $this->assertFalse(file_exists($this->dir . '/.htaccess'));

        $this->assertTrue(
            (new ServerConfigWriter())->uninstall(),
            'an absent .htaccess is nothing to remove, and must stay a success'
        );
    }

    /**
     * A readable .htaccess with no managed block: also nothing to remove.
     */
    public function test_readable_htaccess_without_a_block_still_returns_true(): void
    {
        file_put_contents(
            $this->dir . '/.htaccess',
            "# BEGIN WordPress\nRewriteRule . /index.php [L]\n# END WordPress\n"
        );

        $this->assertTrue(
            (new ServerConfigWriter())->uninstall(),
            'a file with no managed block is nothing to remove, and must stay a success'
        );
    }

    /**
     * The ordinary path: block present and removable. Must return true AND
     * actually strip the block while leaving the core rules alone.
     */
    public function test_removable_block_is_stripped_and_returns_true(): void
    {
        $path = $this->writeHtaccessWithBlock();

        $this->assertTrue(
            (new ServerConfigWriter())->uninstall(),
            'a removable managed block must still report success'
        );

        $after = (string) file_get_contents($path);
        $this->assertStringNotContainsString(ServerConfigWriter::BEGIN, $after, 'the block must be gone');
        $this->assertStringContainsString(
            '# BEGIN WordPress',
            $after,
            'unrelated core rules must survive the strip'
        );
    }

    /**
     * Idempotency survives the change: running it twice on a healthy site is
     * still a success the second time, so the repair does not re-arm forever.
     */
    public function test_uninstall_is_still_idempotent(): void
    {
        $this->writeHtaccessWithBlock();

        $writer = new ServerConfigWriter();
        $this->assertTrue($writer->uninstall(), 'first removal succeeds');
        $this->assertTrue($writer->uninstall(), 'second removal is a no-op success, not a failure');
    }
}
