<?php
/**
 * Child process for SiteUpdateLockConcurrencyTest.
 *
 * Runs the REAL WPMgr\Agent\Support\SiteUpdateLock against a file-backed
 * option store whose lock row is created under an exclusive flock(), which is a
 * genuine OS-level compare-and-set. That is deliberately the same SHAPE as the
 * primitive SiteUpdateLock is built on in production (WordPress core's single
 * `INSERT IGNORE` at wp-admin/includes/class-wp-upgrader.php:1065), which this
 * project does not reimplement and must not claim to have tested. What IS under
 * test here is the logic layered on top of it: the three-way acquire verdict,
 * the owner token, and release, across two real OS processes.
 *
 * Usage: php site-update-lock-child.php <store-file> <log-file> <hold-ms>
 *
 * Appends one of:
 *   ENTER <pid> <microtime>   acquired
 *   EXIT  <pid> <microtime>   released
 *   BUSY  <pid>               refused because another process held it
 *   UNAVAILABLE <pid>         could not evaluate the lock at all
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

// The agent's classes guard on ABSPATH; nothing here reads a real WordPress.
define('ABSPATH', __DIR__ . '/');

$storeFile = (string) ($argv[1] ?? '');
$logFile   = (string) ($argv[2] ?? '');
$holdMs    = (int) ($argv[3] ?? 0);

if ($storeFile === '' || $logFile === '') {
    fwrite(STDERR, "usage: site-update-lock-child.php <store> <log> <hold-ms>\n");
    exit(2);
}

$GLOBALS['wpmgr_lock_store_file'] = $storeFile;

/**
 * Read the whole option store.
 *
 * @return array<string,mixed>
 */
function wpmgr_test_store_read(): array
{
    $raw = @file_get_contents((string) $GLOBALS['wpmgr_lock_store_file']);
    if (!is_string($raw) || $raw === '') {
        return [];
    }
    $decoded = json_decode($raw, true);

    return is_array($decoded) ? $decoded : [];
}

/**
 * Write the whole option store ATOMICALLY.
 *
 * Write-then-rename, not file_put_contents in place. A plain in-place write
 * truncates the file before the new bytes land, and the lock row is read
 * OUTSIDE the guard lock (exactly as WordPress reads get_option outside its own
 * INSERT), so a concurrent reader could see an empty file and conclude the lock
 * was unreadable rather than held. That would make this fixture, not the code
 * under test, decide the result. Real WordPress reads this row out of MySQL,
 * where the same guarantee is free.
 *
 * @param array<string,mixed> $store Options.
 * @return void
 */
function wpmgr_test_store_write(array $store): void
{
    $target = (string) $GLOBALS['wpmgr_lock_store_file'];
    $tmp    = $target . '.' . getmypid() . '.tmp';
    file_put_contents($tmp, (string) json_encode($store));
    rename($tmp, $target);
}

/**
 * @param string $option  Option name.
 * @param mixed  $default Default value.
 * @return mixed
 */
function get_option(string $option, $default = false)
{
    $store = wpmgr_test_store_read();

    return $store[$option] ?? $default;
}

/**
 * @param string $option   Option name.
 * @param mixed  $value    Value.
 * @param mixed  $autoload Ignored.
 * @return bool
 */
function update_option(string $option, $value, $autoload = null): bool
{
    $store           = wpmgr_test_store_read();
    $store[$option]  = $value;
    wpmgr_test_store_write($store);

    return true;
}

/**
 * @param string $option Option name.
 * @return bool
 */
function delete_option(string $option): bool
{
    $store = wpmgr_test_store_read();
    unset($store[$option]);
    wpmgr_test_store_write($store);

    return true;
}

/**
 * WP_Upgrader double whose create_lock() performs its insert-if-absent step
 * under an exclusive flock, giving the same atomicity guarantee core's
 * `INSERT IGNORE` provides.
 */
class WP_Upgrader
{
    /**
     * @param string   $lock_name       Lock name.
     * @param int|null $release_timeout Seconds an existing lock is respected.
     * @return bool
     */
    public static function create_lock($lock_name, $release_timeout = null): bool
    {
        $releaseTimeout = (int) ($release_timeout ?: 3600);
        $option         = $lock_name . '.lock';
        $guard          = (string) $GLOBALS['wpmgr_lock_store_file'] . '.guard';

        $handle = fopen($guard, 'c');
        if ($handle === false) {
            return false;
        }
        if (!flock($handle, LOCK_EX)) {
            fclose($handle);

            return false;
        }

        try {
            $store    = wpmgr_test_store_read();
            $existing = isset($store[$option]) ? (int) $store[$option] : 0;

            if ($existing > 0 && $existing > (time() - $releaseTimeout)) {
                return false;
            }

            $store[$option] = time();
            wpmgr_test_store_write($store);

            return true;
        } finally {
            flock($handle, LOCK_UN);
            fclose($handle);
        }
    }

    /**
     * @param string $lock_name Lock name.
     * @return bool
     */
    public static function release_lock($lock_name): bool
    {
        return delete_option($lock_name . '.lock');
    }
}

require_once dirname(__DIR__, 2) . '/includes/support/class-debug-log.php';
require_once dirname(__DIR__, 2) . '/includes/support/class-site-update-lock.php';

/**
 * Append one line to the shared observation log.
 *
 * @param string $line Line to append.
 * @return void
 */
function wpmgr_test_log(string $line): void
{
    file_put_contents((string) ($GLOBALS['wpmgr_lock_log_file'] ?? ''), $line . "\n", FILE_APPEND | LOCK_EX);
}

$GLOBALS['wpmgr_lock_log_file'] = $logFile;

$state = \WPMgr\Agent\Support\SiteUpdateLock::acquire();
$pid   = (string) getmypid();

if ($state === \WPMgr\Agent\Support\SiteUpdateLock::ACQUIRED) {
    wpmgr_test_log('ENTER ' . $pid . ' ' . microtime(true));
    usleep($holdMs * 1000);
    wpmgr_test_log('EXIT ' . $pid . ' ' . microtime(true));
    \WPMgr\Agent\Support\SiteUpdateLock::release();
    exit(0);
}

if ($state === \WPMgr\Agent\Support\SiteUpdateLock::HELD_BY_OTHER) {
    wpmgr_test_log('BUSY ' . $pid);
    exit(0);
}

wpmgr_test_log('UNAVAILABLE ' . $pid);
exit(0);
