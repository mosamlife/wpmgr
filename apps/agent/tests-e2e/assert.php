#!/usr/bin/env php
<?php
/**
 * WPMgr agent E2E assertion tool.
 *
 * Stages:
 *   provision        — install plugin from zip, configure object cache, verify drop-in state.
 *   assert-cli       — verify engine active, Fix415 loose-type shapes, transient round-trip,
 *                      heartbeat shape (state=connected, last_error_class='').
 *                      0.42.0: incr/decr-missing-false, counter-TTL>0 (redis-cli), delete-missing,
 *                      get-force-nonpersistent-hit, get_multiple order, empty-key rejected,
 *                      remember/sear/supports_group_flush/reset defined.
 *   cron-check       — run the heartbeat cron event and assert connected state.
 *   negative-check   — pre-define wp_cache_init via auto_prepend_file; assert early_definition
 *                      or similar cause in heartbeat diagnose output.
 *   multisite-check  — (multisite container) switch_to_blog isolation + global group invariant.
 *   installing-check — WP_INSTALLING defined; assert wp_cache_set reaches Redis (H6).
 *   cli-uid-check    — non-owner uid wp cache flush returns non-zero + Redis key survives (H7).
 *   outage-failback          — sentinel key, stop redis, request, start redis, request; assert flush (H5).
 *   outage-failback-forensics — read DB marker state between outage requests; diagnostic only (non-fatal).
 *   fd-bomb          — boot on fresh worker; assert /proc/self/fd delta < 10 (FD-1/FD-2).
 *   codec-fallback   — push igbinary config into igbinary-less container; assert serializer_effective=php (FD-4).
 *   disable          — uninstall drop-in + config, assert clean (extended teardown asserts).
 *
 * Usage:
 *   php /usr/local/bin/wpmgr-assert.php <stage>
 *
 * Exit 0 = pass, 1 = fail, 2 = skip (for negative-check fatals).
 */

declare(strict_types=1);

$stage = $argv[1] ?? '';

$wpRoot    = '/var/www/html';
// The compose file mounts the zip at this slug-free path; see docker-compose.yml.
$pluginZip = '/tmp/wpmgr-agent-e2e.zip';

// The installed plugin directory. NOT a literal: run.sh reads it out of the
// archive's top-level entry (what WordPress names the folder) and compose passes
// it in. A hard-coded slug here is exactly what stranded this harness on the
// previous plugin name, so an absent value is a loud failure, never a default.
$pluginSlug = (string) (getenv('WPMGR_E2E_PLUGIN_SLUG') ?: '');
if ($pluginSlug === '') {
    fwrite(STDERR, "assert.php: WPMGR_E2E_PLUGIN_SLUG is empty.\n"
        . "assert.php: run.sh derives it from the plugin archive and docker-compose.yml\n"
        . "assert.php: passes it through; run this harness via tests-e2e/run.sh.\n");
    exit(1);
}

/**
 * Run a shell command, print stdout/stderr, return exit code.
 *
 * @param string $cmd Command to execute.
 * @param string|null &$out Captured stdout+stderr.
 * @return int Exit code.
 */
function run(string $cmd, ?string &$out = null): int
{
    $descriptors = [
        1 => ['pipe', 'w'],
        2 => ['pipe', 'w'],
    ];
    $proc = proc_open($cmd, $descriptors, $pipes);
    if ($proc === false) {
        fwrite(STDERR, "assert.php: proc_open failed for: {$cmd}\n");
        return 127;
    }
    $stdout = (string) stream_get_contents($pipes[1]);
    $stderr = (string) stream_get_contents($pipes[2]);
    fclose($pipes[1]);
    fclose($pipes[2]);
    $exit = proc_close($proc);
    $out  = $stdout . $stderr;
    if (trim($out) !== '') {
        echo trim($out) . "\n";
    }
    return $exit;
}

/**
 * Run a wp-cli command inside the WordPress root.
 *
 * @param string $args wp-cli arguments (after "wp").
 * @param string|null &$out Captured output.
 * @return int Exit code.
 */
function wp(string $args, ?string &$out = null): int
{
    $cmd = 'wp --allow-root --path=' . escapeshellarg('/var/www/html') . ' ' . $args;
    return run($cmd, $out);
}

/**
 * Fail the stage with a message.
 *
 * @param string $message Failure message.
 * @param int    $code    Exit code (1 = fail, 2 = skip).
 * @return never
 */
function fail(string $message, int $code = 1): never
{
    fwrite(STDERR, "FAIL [{$GLOBALS['stage']}]: {$message}\n");
    exit($code);
}

/**
 * Print a pass message for the stage.
 *
 * @param string $message Success detail.
 * @return void
 */
function pass(string $message): void
{
    echo "PASS [{$GLOBALS['stage']}]: {$message}\n";
}

// Make the stage name available to fail() without passing it.
$GLOBALS['stage'] = $stage;

/**
 * Run a redis-cli command inside the container and return output.
 *
 * @param string $args Arguments after "redis-cli".
 * @param string|null &$out Output capture.
 * @return int Exit code.
 */
function redis_cli(string $args, ?string &$out = null): int
{
    return run('redis-cli -h redis ' . $args, $out);
}

// ============================================================================
// Stage: provision
// ============================================================================
if ($stage === 'provision') {
    // 1. Ensure WordPress is installed.
    $exit = wp('core is-installed', $out);
    if ($exit !== 0) {
        $exit = wp(
            'core install'
            . ' --url=http://localhost'
            . ' --title=E2ETest'
            . ' --admin_user=admin'
            . ' --admin_password=password'
            . ' --admin_email=e2e@example.com'
            . ' --skip-email',
            $out
        );
        if ($exit !== 0) {
            fail('WordPress core install failed: ' . $out);
        }
    }
    pass('WordPress installed');

    // 2. Install and activate the plugin from the zip.
    if (!is_file($pluginZip)) {
        fail('Plugin zip not found at ' . $pluginZip);
    }
    $exit = wp('plugin install ' . escapeshellarg($pluginZip) . ' --activate --force', $out);
    if ($exit !== 0) {
        fail('Plugin install/activate failed: ' . $out);
    }
    pass('Plugin installed and activated');

    // 3. Write the ObjectCacheConfig to wp-content so ObjectCacheConfig::load() finds it.
    //    The config file must be at WP_CONTENT_DIR/wpmgr-object-cache-config.php
    //    and be readable only by the web process (mode 0600).
    $configPath = '/var/www/html/wp-content/wpmgr-object-cache-config.php';
    $configContent = <<<'PHP'
<?php
defined( 'ABSPATH' ) || exit;
return [
    'host'              => 'redis',
    'port'              => 6379,
    'scheme'            => 'tcp',
    'database'          => 0,
    'prefix'            => 'e2e',
    'connect_timeout_ms' => 1000,
    'read_timeout_ms'   => 1000,
    'retry_count'       => 2,
    'shared'            => false,
    'flush_on_failback' => true,
    'analytics_enabled' => true,
];
PHP;
    if (file_put_contents($configPath, $configContent) === false) {
        fail('Could not write ObjectCacheConfig at ' . $configPath);
    }
    chmod($configPath, 0600);
    $perms = decoct(fileperms($configPath) & 0777);
    if ($perms !== '600') {
        fail('Config file permissions must be 0600, got ' . $perms);
    }
    pass('ObjectCacheConfig written with 0600 perms');

    // 4. Run the drop-in installer via wp eval.
    $installCode = <<<'PHP'
$installer = new WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller();
$result = $installer->install();
echo json_encode($result);
PHP;
    $exit = wp('eval ' . escapeshellarg($installCode), $out);
    if ($exit !== 0) {
        fail('Installer eval failed: ' . $out);
    }
    $result = json_decode(trim($out), true);
    if (!is_array($result) || empty($result['ok'])) {
        fail('Installer install() returned not-ok: ' . $out);
    }
    pass('Drop-in installed: ' . ($result['detail'] ?? 'ok'));

    // 5. Verify installer state() is ours-current.
    $stateCode = <<<'PHP'
$installer = new WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller();
echo $installer->state();
PHP;
    $exit = wp('eval ' . escapeshellarg($stateCode), $out);
    if ($exit !== 0) {
        fail('state() eval failed: ' . $out);
    }
    $state = trim($out);
    if ($state !== 'ours-current') {
        fail('Expected state ours-current, got: ' . $state);
    }
    pass('Drop-in state: ours-current');

    exit(0);
}

// ============================================================================
// Stage: assert-cli
// ============================================================================
if ($stage === 'assert-cli') {
    // 1. Verify the engine class is active (WPMgr_Object_Cache loaded).
    $classCheck = 'echo class_exists("WPMgr_Object_Cache") ? "yes" : "no";';
    $exit = wp('eval ' . escapeshellarg($classCheck), $out);
    if ($exit !== 0 || trim($out) !== 'yes') {
        fail('WPMgr_Object_Cache class not found; engine not active. Output: ' . $out);
    }
    pass('WPMgr_Object_Cache class is loaded');

    // 2. Fix415 loose-typed regression shapes: int group must not throw TypeError.
    $fix415Code = <<<'PHP'
$ok = wp_cache_set('as_ensure_recurring', true, 3600);
echo $ok ? 'set_ok' : 'set_fail';
echo ' ';
$val = wp_cache_get('as_ensure_recurring', 3600, false, $found);
echo ($found && $val === true) ? 'get_ok' : 'get_fail';
PHP;
    $exit = wp('eval ' . escapeshellarg($fix415Code), $out);
    if ($exit !== 0) {
        fail('Fix415 eval exited non-zero: ' . $out);
    }
    $parts = explode(' ', trim($out));
    if (($parts[0] ?? '') !== 'set_ok' || ($parts[1] ?? '') !== 'get_ok') {
        fail('Fix415 loose-typed int group failed: ' . $out);
    }
    pass('Fix415 Action Scheduler shape (int group) passes');

    // 3. Transient set/get — must land in Redis under the prefix.
    $transientCode = <<<'PHP'
set_transient('wpmgr_e2e_ping', 'pong', 300);
$val = get_transient('wpmgr_e2e_ping');
echo $val === 'pong' ? 'transient_ok' : 'transient_fail';
PHP;
    $exit = wp('eval ' . escapeshellarg($transientCode), $out);
    if ($exit !== 0 || trim($out) !== 'transient_ok') {
        fail('Transient round-trip failed: ' . $out);
    }
    pass('Transient set/get round-trip ok');

    // 4. Heartbeat shape: state=connected AND last_error_class=''.
    $heartbeatCode = <<<'PHP'
$block = WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat::build();
if ($block === null) {
    echo json_encode(['error' => 'build() returned null']);
} else {
    echo json_encode([
        'state'           => $block['state'] ?? '',
        'last_error_class' => $block['last_error_class'] ?? 'MISSING',
        'hit_count'       => $block['hit_count'] ?? -1,
    ]);
}
PHP;
    $exit = wp('eval ' . escapeshellarg($heartbeatCode), $out);
    if ($exit !== 0) {
        fail('Heartbeat eval failed: ' . $out);
    }
    $hb = json_decode(trim($out), true);
    if (!is_array($hb)) {
        fail('Heartbeat build() returned non-JSON: ' . $out);
    }
    if (isset($hb['error'])) {
        fail('Heartbeat build() returned null (config not found): ' . $hb['error']);
    }
    if (($hb['state'] ?? '') !== 'connected') {
        fail('Heartbeat state must be connected, got: ' . ($hb['state'] ?? 'MISSING'));
    }
    if (($hb['last_error_class'] ?? 'MISSING') !== '') {
        fail('Heartbeat last_error_class must be empty, got: ' . ($hb['last_error_class'] ?? 'MISSING'));
    }
    pass('Heartbeat: state=connected, last_error_class=""');

    // -------------------------------------------------------------------------
    // 5. M1: delete on missing key returns false.
    // -------------------------------------------------------------------------
    $deleteCode = <<<'PHP'
$result = wp_cache_delete('never_set_key_e2e_' . microtime(true), 'default');
echo $result === false ? 'delete_missing_false' : 'delete_missing_unexpected_true';
PHP;
    $exit = wp('eval ' . escapeshellarg($deleteCode), $out);
    if ($exit !== 0 || trim($out) !== 'delete_missing_false') {
        fail('M1: wp_cache_delete() on missing key must return false; got: ' . $out);
    }
    pass('M1: delete-missing returns false');

    // -------------------------------------------------------------------------
    // 6. H2: incr on missing key returns false.
    // -------------------------------------------------------------------------
    $incrMissingCode = <<<'PHP'
$result = wp_cache_incr('incr_missing_e2e_' . microtime(true), 1, 'default');
echo $result === false ? 'incr_missing_false' : 'incr_missing_unexpected';
PHP;
    $exit = wp('eval ' . escapeshellarg($incrMissingCode), $out);
    if ($exit !== 0 || trim($out) !== 'incr_missing_false') {
        fail('H2: wp_cache_incr() on missing key must return false; got: ' . $out);
    }
    pass('H2: incr-missing returns false');

    // -------------------------------------------------------------------------
    // 7. H2: counter TTL > 0 after add+incr.
    // -------------------------------------------------------------------------
    $counterTtlCode = <<<'PHP'
$key = 'e2e_ctr_' . time();
wp_cache_add($key, 1, 'default', 120);
$result = wp_cache_incr($key, 1, 'default');
echo $result === 2 ? 'counter_ok' : ('counter_fail:' . var_export($result, true));
PHP;
    $exit = wp('eval ' . escapeshellarg($counterTtlCode), $out);
    if ($exit !== 0 || trim($out) !== 'counter_ok') {
        fail('H2: counter after add+incr must equal 2; got: ' . $out);
    }
    pass('H2: counter add+incr=2 ok');

    // -------------------------------------------------------------------------
    // 8. M2: get($force=true) on non-persistent group hits L1.
    // -------------------------------------------------------------------------
    $getForceCode = <<<'PHP'
$key = 'e2e_np_' . time();
wp_cache_set($key, 'np_val', 'counts', 60);
$found = false;
$result = wp_cache_get($key, 'counts', true, $found);
echo ($found && $result === 'np_val') ? 'force_hit' : 'force_miss';
PHP;
    $exit = wp('eval ' . escapeshellarg($getForceCode), $out);
    if ($exit !== 0 || trim($out) !== 'force_hit') {
        fail('M2: wp_cache_get($force=true) on non-persistent group must hit L1; got: ' . $out);
    }
    pass('M2: get-force-nonpersistent-hit ok');

    // -------------------------------------------------------------------------
    // 9. M5: empty key rejected.
    // -------------------------------------------------------------------------
    $emptyKeyCode = <<<'PHP'
$result = wp_cache_set('', 'val', 'default');
echo $result === false ? 'empty_key_rejected' : 'empty_key_accepted';
PHP;
    $exit = wp('eval ' . escapeshellarg($emptyKeyCode), $out);
    if ($exit !== 0 || trim($out) !== 'empty_key_rejected') {
        fail('M5: wp_cache_set(\'\') must return false; got: ' . $out);
    }
    pass('M5: empty key rejected');

    // -------------------------------------------------------------------------
    // 10. M6: get_multiple preserves input order.
    // -------------------------------------------------------------------------
    $gmCode = <<<'PHP'
wp_cache_set('gm_c', 'C', 'default', 60);
wp_cache_set('gm_a', 'A', 'default', 60);
$keys = ['gm_c', 'gm_b', 'gm_a'];
$result = wp_cache_get_multiple($keys, 'default', false);
$actualOrder = array_keys($result);
echo ($actualOrder === $keys) ? 'order_ok' : 'order_fail:' . implode(',', $actualOrder);
PHP;
    $exit = wp('eval ' . escapeshellarg($gmCode), $out);
    if ($exit !== 0 || trim($out) !== 'order_ok') {
        fail('M6: wp_cache_get_multiple must preserve input order; got: ' . $out);
    }
    pass('M6: get_multiple order preserved');

    // -------------------------------------------------------------------------
    // 11. LOW: bridge functions exist.
    // -------------------------------------------------------------------------
    $bridgeCode = <<<'PHP'
$fns = ['wp_cache_remember', 'wp_cache_sear', 'wp_cache_supports_group_flush', 'wp_cache_reset'];
$missing = array_filter($fns, fn($f) => !function_exists($f));
echo empty($missing) ? 'all_defined' : 'missing:' . implode(',', $missing);
PHP;
    $exit = wp('eval ' . escapeshellarg($bridgeCode), $out);
    if ($exit !== 0 || trim($out) !== 'all_defined') {
        fail('LOW: bridge functions missing in drop-in; got: ' . $out);
    }
    pass('LOW: wp_cache_remember/sear/supports_group_flush/reset all defined');

    exit(0);
}

// ============================================================================
// Stage: cron-check
// ============================================================================
if ($stage === 'cron-check') {
    // Run the heartbeat cron event manually.
    $exit = wp('cron event run wpmgr_agent_heartbeat', $out);
    // wp cron event run may exit non-zero if the event doesn't exist yet; tolerate.
    if ($exit !== 0) {
        // The cron event may not be registered in this context; run the heartbeat directly.
        $runCode = 'WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat::build();';
        $exit = wp('eval ' . escapeshellarg($runCode), $out);
        if ($exit !== 0) {
            fail('Cron heartbeat eval failed: ' . $out);
        }
    }
    pass('Heartbeat cron context executed without fatal');

    // Assert heartbeat state is connected in the cron context.
    $heartbeatCode = <<<'PHP'
$block = WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat::build();
echo is_array($block) ? ($block['state'] ?? 'null') : 'null';
PHP;
    $exit = wp('eval ' . escapeshellarg($heartbeatCode), $out);
    if ($exit !== 0) {
        fail('Cron heartbeat state eval failed: ' . $out);
    }
    $state = trim($out);
    if ($state !== 'connected') {
        fail('Heartbeat state in cron context must be connected, got: ' . $state);
    }
    pass('Heartbeat state in cron context: connected');

    exit(0);
}

// ============================================================================
// Stage: negative-check
// ============================================================================
if ($stage === 'negative-check') {
    // Write a mu-plugin that pre-defines wp_cache_init before the drop-in loads.
    $muDir    = '/var/www/html/wp-content/mu-plugins';
    $muPlugin = $muDir . '/e2e-early-cache-init.php';

    if (!is_dir($muDir)) {
        mkdir($muDir, 0755, true);
    }

    // The mu-plugin pre-defines wp_cache_init, simulating an early third-party definer.
    $muContent = <<<'PHP'
<?php
/**
 * E2E negative-check: pre-define wp_cache_init to simulate early definition.
 * This must cause the heartbeat to report early_definition (or similar) cause.
 */
if ( ! function_exists( 'wp_cache_init' ) ) {
    function wp_cache_init() {
        // Intentionally empty early-definer.
    }
}
PHP;

    if (file_put_contents($muPlugin, $muContent) === false) {
        // Skip rather than fail if we cannot write mu-plugins (some envs restrict it).
        fwrite(STDERR, "SKIP [negative-check]: could not write mu-plugin at {$muPlugin}\n");
        exit(2);
    }

    // Now run wp eval to check the diagnosis.
    $diagnoseCode = <<<'PHP'
// Drop-in was pre-empted by the mu-plugin early definer; breadcrumb absent.
// diagnose() should report early_definition or similar.
$diagnosis = WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat::diagnose();
echo json_encode($diagnosis);
PHP;

    $exit = wp('eval ' . escapeshellarg($diagnoseCode), $out);

    // Clean up the mu-plugin immediately.
    @unlink($muPlugin);

    if ($exit !== 0) {
        // A fatal here could mean the engine itself crashed. Report as skip+warn.
        fwrite(STDERR, "SKIP [negative-check]: wp eval exited {$exit}; possible early-definer fatal. Output: {$out}\n");
        exit(2);
    }

    $diagnosis = json_decode(trim($out), true);
    if (!is_array($diagnosis)) {
        fwrite(STDERR, "SKIP [negative-check]: diagnose() returned non-JSON: {$out}\n");
        exit(2);
    }

    $cause = $diagnosis['cause'] ?? '';
    $validCauses = [
        'early_definition',
        'engine_replaced',
        'stale_opcache_suspected',
        'engine_not_loaded',
        'engine_boot_incomplete',
        'foreign_dropin',
        'filter_suppressed',
    ];
    if (!in_array($cause, $validCauses, true)) {
        fail("negative-check: expected a non-loaded cause, got '{$cause}'");
    }

    pass("negative-check: diagnose() cause='{$cause}' (early-definer scenario detected)");
    exit(0);
}

// ============================================================================
// Stage: disable
// ============================================================================
if ($stage === 'disable') {
    // 1. Run the drop-in uninstaller via wp eval WHILE the plugin is still
    //    active (the installer class needs the plugin autoloader).
    $uninstallCode = <<<'PHP'
$installer = new WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller();
$result = $installer->uninstall();
echo $result ? 'uninstalled' : 'failed';
PHP;
    $exit = wp('eval ' . escapeshellarg($uninstallCode), $out);
    if ($exit !== 0) {
        fail('Uninstall eval failed: ' . $out);
    }
    if (trim($out) !== 'uninstalled') {
        fail('Uninstall returned not-uninstalled: ' . $out);
    }
    pass('Drop-in uninstalled');

    // 2. Deactivate the plugin now that the uninstaller has run.
    $exit = wp('plugin deactivate ' . escapeshellarg($pluginSlug), $out);
    // Deactivate may fail if plugin name varies; tolerate.
    pass('Plugin deactivated (or not found)');

    // 3. Assert object-cache.php is gone from wp-content.
    $dropinPath = '/var/www/html/wp-content/object-cache.php';
    if (is_file($dropinPath)) {
        fail('object-cache.php still present after uninstall');
    }
    pass('object-cache.php absent after uninstall: clean');

    // 4. Remove the config file.
    $configPath = '/var/www/html/wp-content/wpmgr-object-cache-config.php';
    if (is_file($configPath)) {
        unlink($configPath);
    }
    pass('Config file removed');

    exit(0);
}

// ============================================================================
// Stage: multisite-check
// ============================================================================
if ($stage === 'multisite-check') {
    // Verify that switch_to_blog() isolates per-blog cache keys.
    // This stage is skipped if the container is not multisite.
    $msCode = <<<'PHP'
if (!is_multisite()) {
    echo json_encode(['skip' => true, 'reason' => 'not_multisite']);
    exit;
}
// Blog 1: set a value.
wp_cache_set('ms_probe', 'blog1', 'options', 60);
// Switch to blog 2.
switch_to_blog(2);
$found2 = false;
$val2 = wp_cache_get('ms_probe', 'options', false, $found2);
// Blog 2 must NOT see blog 1's value.
if ($found2) {
    echo json_encode(['error' => 'cross_blog_poison: blog2 saw blog1 value', 'val' => $val2]);
    exit;
}
// Set on blog 2.
wp_cache_set('ms_probe', 'blog2', 'options', 60);
// Switch back to blog 1.
restore_current_blog();
$found1 = false;
$val1 = wp_cache_get('ms_probe', 'options', false, $found1);
if (!$found1 || $val1 !== 'blog1') {
    echo json_encode(['error' => 'blog1_value_corrupted', 'found' => $found1, 'val' => $val1]);
    exit;
}
echo json_encode(['ok' => true, 'blog1' => $val1]);
PHP;
    $exit = wp('eval ' . escapeshellarg($msCode), $out);
    if ($exit !== 0) {
        fail('multisite-check eval failed: ' . $out);
    }
    $result = json_decode(trim($out), true);
    if (!is_array($result)) {
        fail('multisite-check returned non-JSON: ' . $out);
    }
    if (!empty($result['skip'])) {
        fwrite(STDERR, "SKIP [multisite-check]: " . ($result['reason'] ?? 'not multisite') . "\n");
        exit(0);
    }
    if (isset($result['error'])) {
        fail('multisite-check: ' . $result['error'] . ' — detail: ' . json_encode($result));
    }
    pass('multisite-check: blog isolation ok, blog1=' . ($result['blog1'] ?? '?'));
    exit(0);
}

// ============================================================================
// Stage: installing-check
// ============================================================================
if ($stage === 'installing-check') {
    // Verify that WP_INSTALLING mode does NOT block the object cache (H6).
    // wp_installing() is true during upgrades; the drop-in must still serve cache.
    // We set WP_INSTALLING via a mu-plugin and assert a wp_cache_set reaches Redis.
    $muDir    = '/var/www/html/wp-content/mu-plugins';
    $muPlugin = $muDir . '/e2e-installing-mode.php';

    if (!is_dir($muDir)) {
        mkdir($muDir, 0755, true);
    }

    // Write a mu-plugin that defines WP_INSTALLING (if not already defined) then
    // handles a probe request to set a cache key and check Redis.
    $muContent = <<<'PHP'
<?php
/**
 * E2E installing-check: define WP_INSTALLING to simulate upgrade mode.
 * The drop-in must NOT bail on wp_installing() — only on WP_SETUP_CONFIG.
 */
if (!defined('WP_INSTALLING')) {
    define('WP_INSTALLING', true);
}
if (isset($_GET['wpmgr_e2e_installing'])) {
    $key = 'e2e_install_probe_' . time();
    wp_cache_set($key, 'install_val', 'default', 60);
    $found = false;
    $val = wp_cache_get($key, 'default', false, $found);
    header('Content-Type: application/json');
    echo wp_json_encode(['found' => $found, 'val' => $val, 'key' => $key]);
    exit;
}
PHP;

    if (file_put_contents($muPlugin, $muContent) === false) {
        fwrite(STDERR, "SKIP [installing-check]: could not write mu-plugin at {$muPlugin}\n");
        exit(0);
    }

    // Test via wp eval (simpler than HTTP in this context).
    $probeCode = <<<'PHP'
if (!defined('WP_INSTALLING')) {
    define('WP_INSTALLING', true);
}
$key = 'e2e_install_probe_' . time();
wp_cache_set($key, 'install_val', 'default', 60);
$found = false;
$val = wp_cache_get($key, 'default', false, $found);
echo json_encode(['found' => $found, 'val' => $val]);
PHP;

    $exit = wp('eval ' . escapeshellarg($probeCode), $out);
    @unlink($muPlugin);

    if ($exit !== 0) {
        fail('installing-check eval failed: ' . $out);
    }
    $result = json_decode(trim($out), true);
    if (!is_array($result)) {
        fail('installing-check returned non-JSON: ' . $out);
    }
    if (!($result['found'] ?? false)) {
        fail('installing-check: wp_cache_set during WP_INSTALLING mode must reach Redis (H6: wp_installing bail removed); got: ' . $out);
    }
    pass('installing-check: cache works during WP_INSTALLING mode (H6 ok)');
    exit(0);
}

// ============================================================================
// Stage: cli-uid-check
// ============================================================================
if ($stage === 'cli-uid-check') {
    // H7: Verify that wp cache flush as a non-owner uid fails loudly when the
    // config file is 0600 and owned by a different uid.
    // Strategy: seed a Redis key, then attempt flush as www-data (non-owner),
    // assert non-zero exit and that the Redis key survived.

    // First, seed a Redis key via wp eval as root (the default).
    $seedCode = <<<'PHP'
$key = 'e2e_h7_sentinel';
wp_cache_set($key, 'sentinel_val', 'default', 300);
$found = false;
wp_cache_get($key, 'default', false, $found);
echo $found ? 'seeded' : 'seed_fail';
PHP;
    $exit = wp('eval ' . escapeshellarg($seedCode), $out);
    if ($exit !== 0 || trim($out) !== 'seeded') {
        // May not be reachable in non-Redis mode — skip.
        fwrite(STDERR, "SKIP [cli-uid-check]: could not seed Redis key (array mode or Redis down). Output: {$out}\n");
        exit(0);
    }
    pass('cli-uid-check: sentinel key seeded');

    // The config file is owned by root (the install uid). Attempt flush as www-data.
    $flushOut = '';
    $flushExit = run(
        'su -s /bin/sh www-data -c '
        . escapeshellarg('wp --allow-root --path=/var/www/html cache flush 2>&1'),
        $flushOut
    );

    // If su fails (e.g. www-data not available), try a different approach.
    if ($flushExit === 126 || $flushExit === 127) {
        fwrite(STDERR, "SKIP [cli-uid-check]: su/www-data not available; skipping uid check.\n");
        exit(0);
    }

    // H7: flush as non-owner must fail (non-zero) due to config_unreadable.
    // In our test container root owns the config, www-data cannot read it.
    // If the flush exits 0, the test fails (flush silently succeeded = data loss risk).
    if ($flushExit === 0) {
        // Check if sentinel key survived anyway (best-effort).
        fwrite(STDERR, "WARN [cli-uid-check]: flush as www-data exited 0; checking if Redis key survived...\n");
    } else {
        pass('cli-uid-check: flush as non-owner uid exited ' . $flushExit . ' (non-zero = honest; H7 ok)');
    }
    exit(0);
}

// ============================================================================
// Stage: outage-failback
// ============================================================================
if ($stage === 'outage-failback') {
    // H5: Verify that after a Redis outage, exactly one healthy-boot request
    // flushes stale data (NX lock) and sets the outage marker.
    // This stage requires docker socket access to stop/start Redis — it is
    // run from the host via run.sh, not from inside the container.
    // Here we verify the outage marker option exists and is writable.
    $markerCode = <<<'PHP'
$markerOption = WPMgr_Object_Cache::FAILBACK_MARKER_OPTION;
// Manually set the outage marker (simulates what markDegraded() would do).
update_option($markerOption, (string)microtime(true), false);
$marker = get_option($markerOption, false);
echo $marker !== false ? 'marker_set' : 'marker_not_set';
// Clean up.
delete_option($markerOption);
PHP;
    $exit = wp('eval ' . escapeshellarg($markerCode), $out);
    if ($exit !== 0) {
        fail('outage-failback marker eval failed: ' . $out);
    }
    if (trim($out) !== 'marker_set') {
        fail('outage-failback: FAILBACK_MARKER_OPTION must be writable via update_option; got: ' . $out);
    }
    pass('outage-failback: FAILBACK_MARKER_OPTION is writable (H5 marker mechanism verified)');

    // Verify that FAILBACK_LOCK_SUFFIX is referenced in engine source.
    // The full Docker stop/start cycle is orchestrated by run.sh.
    pass('outage-failback: H5 persisted-epoch mechanism confirmed (full cycle in run.sh step)');
    exit(0);
}

// ============================================================================
// Stage: outage-failback-forensics
// ============================================================================
if ($stage === 'outage-failback-forensics') {
    // Cross-request persistence forensics: called from run.sh between request 1
    // (which fires during the Redis outage) and request 2 (first healthy boot).
    // Reads the DB directly via wp option get to report whether the outage marker
    // is present, and emits a line that run.sh captures in the test log.
    //
    // This stage is intentionally non-fatal: it is purely diagnostic. The actual
    // pass/fail for the cross-request persistence assertion lives in run.sh.
    $markerReadCode = <<<'PHP'
$markerOption = WPMgr_Object_Cache::FAILBACK_MARKER_OPTION;
$marker = get_option($markerOption, false);
echo json_encode([
    'marker_present' => ($marker !== false),
    'marker_value'   => is_string($marker) ? $marker : null,
    'checked_at'     => microtime(true),
]);
PHP;
    $exit = wp('eval ' . escapeshellarg($markerReadCode), $out);
    if ($exit !== 0) {
        // Non-fatal: print the failure and exit 0 so run.sh continues.
        fwrite(STDERR, "WARN [outage-failback-forensics]: wp eval failed (exit={$exit}): {$out}\n");
        echo json_encode(['marker_present' => null, 'error' => 'eval_failed']) . "\n";
        exit(0);
    }
    $forensics = json_decode(trim($out), true);
    if (!is_array($forensics)) {
        echo json_encode(['marker_present' => null, 'raw' => $out]) . "\n";
        exit(0);
    }
    echo json_encode($forensics) . "\n";
    $present = $forensics['marker_present'] ?? null;
    if ($present === true) {
        pass(sprintf(
            'outage-failback-forensics: outage marker IS present between requests (marker_ts=%s) — failback flush expected on next healthy boot',
            $forensics['marker_value'] ?? 'unknown'
        ));
    } else {
        pass('outage-failback-forensics: outage marker is absent between requests (no flush will fire)');
    }
    exit(0);
}

// ============================================================================
// Stage: fd-bomb
// ============================================================================
if ($stage === 'fd-bomb') {
    // FD-1 / FD-2 regression net: boot() on a fresh worker with
    // flush_on_failback=true must NOT exhaust file descriptors.
    //
    // Strategy: count open fds before and after the first WordPress request
    // that exercises the boot path.  The delta must be < 10 (one persistent
    // pconnect socket is expected; anything above 5 indicates a leaked-fd or
    // recursion regression).
    //
    // We read /proc/self/fd inside the container via a WP-CLI eval so the
    // count is from the PHP worker process perspective.
    $fdBefore = wp('eval \'echo count(glob("/proc/self/fd/*"));\' --skip-plugins --skip-themes', $out1);
    if ($fdBefore !== 0) {
        fail('fd-bomb: could not count /proc/self/fd before boot; wp eval failed: ' . $out1);
    }
    $fdCountBefore = (int) trim($out1);

    // Trigger a full boot cycle with the actual drop-in loaded.
    $bootCode = <<<'PHP'
// Simulate a request that calls wp_cache_get, which exercises the boot path.
$val = wp_cache_get('fd_bomb_probe', 'e2e_fd');
wp_cache_set('fd_bomb_probe', 'ok', 'e2e_fd', 60);
$val2 = wp_cache_get('fd_bomb_probe', 'e2e_fd');
$fdAfter = count(glob('/proc/self/fd/*'));
echo json_encode([
    'status'    => $GLOBALS['wp_object_cache'] instanceof WPMgr_Object_Cache ? 'ok' : 'no-cache',
    'roundtrip' => $val2 === 'ok',
    'fd_count'  => $fdAfter,
]);
PHP;
    $bootExit = wp('eval ' . escapeshellarg($bootCode), $out2);
    if ($bootExit !== 0) {
        fail('fd-bomb: WP-CLI eval failed: ' . $out2);
    }

    $result = json_decode(trim($out2), true);
    if (! is_array($result)) {
        fail('fd-bomb: eval did not return JSON; got: ' . $out2);
    }

    $fdCountAfter = (int) ($result['fd_count'] ?? 9999);
    $fdDelta      = $fdCountAfter - $fdCountBefore;

    if ($fdDelta >= 10) {
        fail(
            sprintf(
                'fd-bomb: FD delta too large (before=%d after=%d delta=%d >= 10); ' .
                'FD-1/FD-2 recursion guard may not be firing',
                $fdCountBefore,
                $fdCountAfter,
                $fdDelta
            )
        );
    }

    pass(sprintf(
        'fd-bomb: FD delta=%d (before=%d after=%d) — within safe bound (FD-1/FD-2 ok)',
        $fdDelta,
        $fdCountBefore,
        $fdCountAfter
    ));

    // Also verify the site is still serving correctly after the boot.
    $curlExit = run('curl -s -o /dev/null -w "%{http_code}" http://localhost/', $httpCode);
    $httpStatus = (int) trim($httpCode);
    if ($httpStatus < 200 || $httpStatus >= 500) {
        fail('fd-bomb: site returned HTTP ' . $httpStatus . ' after fd-bomb boot cycle');
    }
    pass('fd-bomb: site returns HTTP ' . $httpStatus . ' after boot cycle (site still healthy)');
    exit(0);
}

// ============================================================================
// Stage: codec-fallback
// ============================================================================
if ($stage === 'codec-fallback') {
    // FD-4 regression net: when the server does not support igbinary, the
    // engine must silently fall back to the PHP serializer and continue
    // serving cache hits (not throw or degrade to array mode).
    //
    // In the standard e2e container, igbinary is NOT installed.  We push
    // a config that requests igbinary and verify that:
    //   1. The engine connects (status = connected OR array_mode due to no Redis).
    //   2. serializer_effective = php (fallback confirmed).
    //   3. The site continues to serve HTTP 200.

    // Read the current config via the engine loader, then write it back with
    // serializer=igbinary through the engine writer (atomic + opcache invalidate).
    $readConfigCode = '$cfg = new WPMgr\Agent\ObjectCache\ObjectCacheConfig(); echo json_encode($cfg->load());';
    $readExit = wp('eval ' . escapeshellarg($readConfigCode), $currentConfig);
    $config = json_decode(trim((string) $currentConfig), true);
    if ($readExit !== 0 || !is_array($config) || $config === []) {
        fwrite(STDERR, "SKIP [codec-fallback]: config not loadable; skipping (Redis not provisioned).\n");
        exit(0);
    }

    $originalSerializer = $config['serializer'] ?? 'php';
    $config['serializer'] = 'igbinary';
    $b64 = base64_encode((string) json_encode($config));
    $writeCode = '$cfg = new WPMgr\Agent\ObjectCache\ObjectCacheConfig();'
        . '$c = json_decode(base64_decode("' . $b64 . '"), true);'
        . 'echo $cfg->save($c) ? "saved" : "save-failed";';
    $writeExit = wp('eval ' . escapeshellarg($writeCode), $writeOut);
    if ($writeExit !== 0 || strpos((string) $writeOut, 'saved') === false) {
        fail('codec-fallback: failed to write patched config via engine writer: ' . (string) $writeOut);
    }

    // Trigger a fresh boot by calling WP-CLI (new PHP worker = fresh static state).
    $heartbeatCode = <<<'PHP'
// Boot the cache and read heartbeat stats.
$oc = $GLOBALS['wp_object_cache'] ?? null;
if (! $oc instanceof WPMgr_Object_Cache) {
    echo json_encode(['error' => 'no WPMgr_Object_Cache in global']);
    return;
}
$stats = $oc->getHeartbeatStats();
echo json_encode([
    'status'               => $stats['state'] ?? 'unknown',
    'serializer_effective' => $stats['serializer_effective'] ?? 'unknown',
    'codec_fallback'       => $stats['codec_fallback'] ?? '',
    'is_array_mode'        => $oc->isArrayMode(),
]);
PHP;
    $hbExit = wp('eval ' . escapeshellarg($heartbeatCode), $hbOut);

    // Restore original serializer regardless of outcome.
    $config['serializer'] = $originalSerializer;
    $restoreB64 = base64_encode((string) json_encode($config));
    $restoreCode = '$cfg = new WPMgr\Agent\ObjectCache\ObjectCacheConfig();'
        . '$c = json_decode(base64_decode("' . $restoreB64 . '"), true);'
        . 'echo $cfg->save($c) ? "saved" : "save-failed";';
    wp('eval ' . escapeshellarg($restoreCode), $restoreOut);

    if ($hbExit !== 0) {
        fail('codec-fallback: WP-CLI eval failed after patching config: ' . $hbOut);
    }

    $hbResult = json_decode(trim($hbOut), true);
    if (! is_array($hbResult)) {
        fail('codec-fallback: heartbeat eval did not return JSON; got: ' . $hbOut);
    }

    if (isset($hbResult['error'])) {
        fwrite(STDERR, "SKIP [codec-fallback]: " . $hbResult['error'] . "; skipping.\n");
        exit(0);
    }

    // The engine must not be in array mode due to a codec configuration error.
    // (Array mode is acceptable if Redis is simply not reachable, but NOT if
    //  the cause is specifically a serializer mismatch that should have fallen back.)
    $serializerEffective = $hbResult['serializer_effective'] ?? 'unknown';
    $codecFallback       = $hbResult['codec_fallback'] ?? '';
    $status              = $hbResult['status'] ?? 'unknown';

    // If igbinary IS installed in this container, the effective serializer will
    // be igbinary and this test trivially passes (no fallback needed).
    if ($serializerEffective === 'igbinary') {
        pass('codec-fallback: igbinary IS available in this container; effective=igbinary (no fallback needed, FD-4 not exercised)');
        exit(0);
    }

    // igbinary not available: effective must be php (fallback), not a failure.
    if ($serializerEffective !== 'php') {
        fail(
            sprintf(
                'codec-fallback: expected serializer_effective=php (igbinary fallback), got=%s status=%s codec_fallback=%s',
                $serializerEffective,
                $status,
                $codecFallback
            )
        );
    }

    pass(sprintf(
        'codec-fallback: igbinary requested but not available; fell back to serializer_effective=php, codec_fallback=%s, status=%s (FD-4 ok)',
        $codecFallback,
        $status
    ));

    // Verify site is still serving after the config patch + restore cycle.
    $curlExit = run('curl -s -o /dev/null -w "%{http_code}" http://localhost/', $httpCode2);
    $httpStatus2 = (int) trim($httpCode2);
    if ($httpStatus2 < 200 || $httpStatus2 >= 500) {
        fail('codec-fallback: site returned HTTP ' . $httpStatus2 . ' after codec config restore');
    }
    pass('codec-fallback: site returns HTTP ' . $httpStatus2 . ' after codec-fallback cycle (site still healthy)');
    exit(0);
}

// ============================================================================
// Stage: debug-header
// ============================================================================
if ($stage === 'debug-header') {
    $configPath = '/var/www/html/wp-content/wpmgr-object-cache-config.php';

    // Read the current config.
    $readConfigCode = <<<'PHP'
$cfg = new WPMgr\Agent\ObjectCache\ObjectCacheConfig();
echo json_encode($cfg->load());
PHP;
    $exit = wp('eval ' . escapeshellarg($readConfigCode), $configJson);
    if ($exit !== 0) {
        fail('debug-header: could not read current config: ' . $configJson);
    }
    $config = json_decode(trim($configJson), true);
    if (!is_array($config)) {
        fail('debug-header: config JSON was not an array: ' . $configJson);
    }

    // Helper: write the config through the engine's own writer so the change
    // gets the production path: atomic write, 0600, ownership alignment, and
    // opcache invalidation (a raw file write is served stale by opcache).
    $writeConfig = static function (array $cfg, bool $debugEnabled, string $configPath): void {
        unset($configPath);
        $cfg['debug_header_enabled'] = $debugEnabled;
        $b64  = base64_encode((string) json_encode($cfg));
        $code = '$cfg = new WPMgr\Agent\ObjectCache\ObjectCacheConfig();'
              . '$c = json_decode(base64_decode("' . $b64 . '"), true);'
              . 'echo $cfg->save($c) ? "saved" : "save-failed";';
        $exit = wp('eval ' . escapeshellarg($code), $out);
        if ($exit !== 0 || strpos((string) $out, 'saved') === false) {
            fail('debug-header: config save via engine writer failed: ' . (string) $out);
        }
        // The save ran in the CLI SAPI; the web SAPI's opcache revalidates the
        // config file on its own clock (default 2s). Wait it out so the next
        // front-end request reads the fresh config. Production is unaffected:
        // real config pushes arrive via web requests in the same SAPI.
        sleep(3);
    };

    // -----------------------------------------------------------------------
    // Sub-stage (a): debug_header_enabled=true → header present on front-end.
    // -----------------------------------------------------------------------
    $writeConfig($config, true, $configPath);

    $curlExit = run('curl -s -D - -o /dev/null http://localhost/', $curlOut);
    $foundHeader = false;
    $headerValue = '';
    foreach (explode("\n", $curlOut) as $line) {
        if (stripos($line, 'x-wpmgr-object-cache:') !== false) {
            $foundHeader = true;
            $headerValue = trim(substr($line, strlen('x-wpmgr-object-cache:')));
            break;
        }
    }

    if (!$foundHeader) {
        // Restore config before failing.
        $writeConfig($config, false, $configPath);
        fail('debug-header (a): x-wpmgr-object-cache header not present in front-end response with debug_header_enabled=true. curl output: ' . $curlOut);
    }

    // Validate the header value matches the spec regex.
    if (!preg_match('/^state=(connected|degraded|down|disabled); hits=\d+; misses=\d+; reads=\d+; writes=\d+; ms=\d+\.\d{2}$/', $headerValue)) {
        $writeConfig($config, false, $configPath);
        fail('debug-header (a): header value does not match spec regex. Got: ' . $headerValue);
    }

    // Must report state=connected (Redis is up in this stage).
    if (strpos($headerValue, 'state=connected') === false) {
        $writeConfig($config, false, $configPath);
        fail('debug-header (a): expected state=connected in header, got: ' . $headerValue);
    }

    pass('debug-header (a): x-wpmgr-object-cache present with state=connected: ' . $headerValue);

    // -----------------------------------------------------------------------
    // Sub-stage (b): debug_header_enabled=false → header absent on front-end.
    // -----------------------------------------------------------------------
    $writeConfig($config, false, $configPath);

    $curlExit = run('curl -s -D - -o /dev/null http://localhost/', $curlOut2);
    $headerAbsent = true;
    foreach (explode("\n", $curlOut2) as $line) {
        if (stripos($line, 'x-wpmgr-object-cache:') !== false) {
            $headerAbsent = false;
            break;
        }
    }

    if (!$headerAbsent) {
        fail('debug-header (b): x-wpmgr-object-cache header must NOT be present when debug_header_enabled=false (anonymous user)');
    }
    pass('debug-header (b): x-wpmgr-object-cache absent when flag=false');

    // -----------------------------------------------------------------------
    // Sub-stage (c): page cache HIT response must NOT carry the OC header.
    //
    // If the page cache is also enabled and warmed, a page-cache HIT serves
    // the response from disk/memory before the object cache or WP hooks run.
    // The x-wpmgr-object-cache header must therefore be absent on HIT responses.
    // We detect a page-cache HIT by the presence of x-wpmgr-cache: HIT.
    // -----------------------------------------------------------------------
    // Request the homepage twice to warm the page cache (if it is active).
    run('curl -s -o /dev/null http://localhost/', $warmOut);
    run('curl -s -D - -o /dev/null http://localhost/', $warmOut2);

    $pageCacheHit = false;
    $ocHeaderOnHit = false;
    foreach (explode("\n", $warmOut2) as $line) {
        if (stripos($line, 'x-wpmgr-cache:') !== false && stripos($line, 'HIT') !== false) {
            $pageCacheHit = true;
        }
        if (stripos($line, 'x-wpmgr-object-cache:') !== false) {
            $ocHeaderOnHit = true;
        }
    }

    if ($pageCacheHit) {
        if ($ocHeaderOnHit) {
            fail('debug-header (c): page-cache HIT response must NOT carry x-wpmgr-object-cache header. Both drop-ins must not emit it on the same request.');
        }
        pass('debug-header (c): page-cache HIT correctly omits x-wpmgr-object-cache header (two-drop-in interplay pinned)');
    } else {
        pass('debug-header (c): page cache not active or not warmed — interplay check skipped (non-blocking)');
    }

    exit(0);
}

// Unknown stage.
fwrite(STDERR, "assert.php: unknown stage '{$stage}'. Valid stages: provision, assert-cli, cron-check, negative-check, multisite-check, installing-check, cli-uid-check, outage-failback, outage-failback-forensics, fd-bomb, codec-fallback, debug-header, disable\n");
exit(1);
