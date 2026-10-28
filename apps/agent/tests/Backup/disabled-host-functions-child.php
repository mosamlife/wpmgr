<?php
/**
 * Child process for DisabledHostFunctionsTest. The test runs it as
 * `php -d disable_functions=set_time_limit,ignore_user_abort,disk_free_space,disk_total_space,link`
 * with an empty temp directory as the only argument.
 *
 * PHPUnit's bootstrap cannot start under those flags (Patchwork builds stubs
 * for set_time_limit and the disk_* functions at load), so this loads the
 * agent classes and the WordPress stubs itself, drives each backup and
 * restore entry point once, and prints one JSON line: which functions are
 * really disabled, what each step returned, and any Throwable a step raised.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

use WPMgr\Agent\Backup\FilesArchiver;
use WPMgr\Agent\Backup\FilesRestorer;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Support\LongRunningJob;

$agentDir = dirname(__DIR__, 2);
$root     = isset($argv[1]) ? realpath((string) $argv[1]) : false;
if ($root === false || !is_dir($root)) {
    fwrite(STDERR, "usage: php disabled-host-functions-child.php <empty temp dir>\n");
    exit(2);
}

define('ABSPATH', $root . '/site/');
define('WPMGR_AGENT_DIR', $agentDir);
require $agentDir . '/vendor/autoload.php';
require $agentDir . '/tests/wp-stubs.php';

$report = ['disabled' => [], 'steps' => [], 'errors' => []];
foreach (['set_time_limit', 'ignore_user_abort', 'disk_free_space', 'disk_total_space', 'link'] as $fn) {
    $report['disabled'][$fn] = !function_exists($fn);
}

$step = static function (string $name, callable $body) use (&$report): void {
    try {
        $report['steps'][$name] = $body();
    } catch (\Throwable $e) {
        $report['errors'][$name] = get_class($e) . ': ' . $e->getMessage() . ' at ' . basename($e->getFile()) . ':' . $e->getLine();
    }
};

$write = static function (string $path, string $bytes): void {
    if (!is_dir(dirname($path))) {
        mkdir(dirname($path), 0700, true);
    }
    file_put_contents($path, $bytes);
};

$step('extend_limits', static function (): bool {
    LongRunningJob::extendLimits();
    return true;
});

$step('disk_space', static fn (): array => [
    'free'  => LongRunningJob::diskFreeSpace($root),
    'total' => LongRunningJob::diskTotalSpace($root),
]);

// The backup worker: TaskRunner::run() lifts the limits before anything else.
$step('task_runner', static function () use ($root): string {
    return (new TaskRunner([
        'snapshot_id'     => 'bbbbbbbb-cccc-4ddd-8eee-ffffffffffff',
        'kind'            => 'files',
        'scratch_dir'     => $root . '/runs/bbbbbbbb-cccc-4ddd-8eee-ffffffffffff',
        'wp_content_path' => $root . '/wp-content',
    ]))->run();
});

// The files archive pass.
$step('archive', static function () use ($root, $write): int {
    $write($root . '/wp-content/plugins/acme/acme.php', '<?php');
    $write($root . '/wp-content/uploads/photo.jpg', 'photo');
    $result = (new FilesArchiver($root . '/wp-content'))->archive($root . '/archive-out', [], static function (): void {});
    return (int) $result['files_total'];
});

// The files restore: stage, then swap over a live Local store, with link()
// disabled too, and the disk fallback the staging estimate uses.
$step('restore', static function () use ($root, $write): array {
    $live = $root . '/restore/wp-content';
    $write($live . '/uploads/wpmgr-backups/live-snap/chunks/a.bin', 'LIVE_CHUNK');
    $write($live . '/uploads/photo.jpg', 'LIVE_PHOTO');
    $zipPath = $root . '/restore-part.zip';
    $zip = new \ZipArchive();
    $zip->open($zipPath, \ZipArchive::CREATE | \ZipArchive::OVERWRITE);
    $zip->addFromString('uploads/photo.jpg', 'RESTORED_PHOTO');
    $zip->close();

    $restorer  = new FilesRestorer();
    $restoreId = 'cccccccc-dddd-4eee-8fff-000000000000';
    $staging   = $restorer->stage([$zipPath], $live, $restoreId, static function (): void {});
    $restorer->swap($staging, $live, $restoreId, static function (): void {});
    $fallback = new \ReflectionMethod(FilesRestorer::class, 'diskUsageFallback');

    return [
        'photo'          => (string) file_get_contents($live . '/uploads/photo.jpg'),
        'store'          => (string) @file_get_contents($live . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'),
        'disk_fallback'  => $fallback->invoke(null, $live),
    ];
});

echo json_encode($report), "\n";
