<?php
/**
 * BackupSource: the seam between backup/restore logic and the WordPress install
 * (filesystem walk + DB export/import). Isolated so the commands are unit-
 * testable without a real WP or shell.
 *
 * Scope (matches what the CP/restore expects): FILES back up the wp-content
 * directory tree (site-relative paths are stored relative to wp-content, which
 * is the unit a restore reassembles into). DB backs up a single logical entry
 * "database.sql" produced by `wp db export --single-transaction` (WP-CLI) with a
 * mysqldump fallback.
 *
 * Path containment: every file enumerated and every restore target is bounded
 * to the wp-content root via realpath/prefix checks, so a crafted manifest path
 * can never read or write outside wp-content.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Enumerates backup sources and runs DB export/import.
 */
class BackupSource
{
    /** Logical manifest path for the database dump entry. */
    public const DB_ENTRY_PATH = 'database.sql';

    /**
     * Directories under wp-content excluded from file backups (agent's own
     * snapshot/scratch space and caches that are not worth shipping).
     *
     * @var list<string>
     */
    private const EXCLUDE_DIRS = ['wpmgr-snapshots', 'wpmgr-agent', 'cache', 'upgrade'];

    /**
     * The wp-content root that bounds all file backup/restore paths.
     *
     * @return string Absolute path with no trailing slash, or '' if unknown.
     */
    public function contentRoot(): string
    {
        if (defined('WP_CONTENT_DIR') && is_string(WP_CONTENT_DIR) && WP_CONTENT_DIR !== '') {
            $real = realpath((string) WP_CONTENT_DIR);
            if ($real !== false) {
                return rtrim($real, '/\\');
            }

            return rtrim((string) WP_CONTENT_DIR, '/\\');
        }

        return '';
    }

    /**
     * Enumerate regular files under wp-content, yielding [absolutePath,
     * relativePath, mode, size] tuples. Excludes the agent's scratch dirs.
     *
     * @return \Generator<int,array{abs:string,rel:string,mode:int,size:int}>
     */
    public function files(): \Generator
    {
        $root = $this->contentRoot();
        if ($root === '' || !is_dir($root)) {
            return;
        }

        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveCallbackFilterIterator(
                new \RecursiveDirectoryIterator($root, \FilesystemIterator::SKIP_DOTS),
                static function (\SplFileInfo $current) use ($root): bool {
                    $rel = self::relativeTo($root, $current->getPathname());
                    if ($rel === null) {
                        return false;
                    }
                    $top = explode('/', $rel)[0];

                    return !in_array($top, self::EXCLUDE_DIRS, true);
                }
            ),
            \RecursiveIteratorIterator::LEAVES_ONLY
        );

        foreach ($iterator as $info) {
            if (!$info instanceof \SplFileInfo || !$info->isFile() || $info->isLink()) {
                continue;
            }
            $abs = $info->getPathname();
            $rel = self::relativeTo($root, $abs);
            if ($rel === null) {
                continue;
            }
            yield [
                'abs'  => $abs,
                'rel'  => $rel,
                'mode' => $info->getPerms() & 0777,
                'size' => (int) $info->getSize(),
            ];
        }
    }

    /**
     * Read a slice of a file (bounded memory) for chunking.
     *
     * @param string $absPath Absolute file path.
     * @param int    $offset  Byte offset.
     * @param int    $length  Bytes to read.
     * @return string Bytes read (possibly shorter at EOF).
     */
    public function readSlice(string $absPath, int $offset, int $length): string
    {
        if ($length < 1) {
            return '';
        }
        $handle = @fopen($absPath, 'rb');
        if ($handle === false) {
            return '';
        }
        try {
            if ($offset > 0 && fseek($handle, $offset) !== 0) {
                return '';
            }
            $data = fread($handle, $length);

            return $data === false ? '' : $data;
        } finally {
            fclose($handle);
        }
    }

    /**
     * Produce a database dump as a string ("wp db export" / mysqldump fallback).
     *
     * @return string SQL dump (empty string on failure).
     */
    public function exportDatabase(): string
    {
        $tmp = $this->tempFile();
        if ($tmp === '') {
            return '';
        }

        try {
            if ($this->wpCliAvailable()) {
                $res = $this->runWpCli(['db', 'export', $tmp, '--single-transaction', '--quiet']);
                if ($res['ok'] && is_file($tmp)) {
                    $sql = (string) file_get_contents($tmp);
                    if ($sql !== '') {
                        return $sql;
                    }
                }
            }

            return $this->exportDatabaseFallback();
        } finally {
            if (is_file($tmp)) {
                @unlink($tmp);
            }
        }
    }

    /**
     * Import a database dump ("wp db import" / mysqli fallback).
     *
     * @param string $sql SQL dump.
     * @return bool Success.
     */
    public function importDatabase(string $sql): bool
    {
        $tmp = $this->tempFile();
        if ($tmp === '' || file_put_contents($tmp, $sql) === false) {
            return false;
        }

        try {
            if ($this->wpCliAvailable()) {
                $res = $this->runWpCli(['db', 'import', $tmp, '--quiet']);

                return $res['ok'];
            }

            return $this->importDatabaseFallback($sql);
        } finally {
            if (is_file($tmp)) {
                @unlink($tmp);
            }
        }
    }

    // ------------------------------------------------------------------
    // Path safety
    // ------------------------------------------------------------------

    /**
     * Resolve a site-relative file path to an absolute path bounded inside
     * wp-content. Rejects traversal/absolute paths; returns '' when unsafe.
     *
     * @param string $rel Site-relative path (e.g. "plugins/foo/bar.php").
     * @return string Absolute path inside wp-content, or '' if unsafe.
     */
    public function resolveWritePath(string $rel): string
    {
        $root = $this->contentRoot();
        if ($root === '') {
            return '';
        }
        $rel = str_replace('\\', '/', trim($rel));
        if ($rel === '' || $rel[0] === '/' || str_contains($rel, "\0")) {
            return '';
        }
        // Reject any traversal component outright.
        foreach (explode('/', $rel) as $segment) {
            if ($segment === '..' || $segment === '.') {
                return '';
            }
        }
        if (preg_match('#^[A-Za-z]:#', $rel) === 1) {
            return '';
        }

        $target   = $root . '/' . $rel;
        $resolved = $this->normalizePath($target);

        // Final containment guard: the normalized path must stay under root.
        $prefix = $root . '/';
        if (strncmp($resolved, $prefix, strlen($prefix)) !== 0) {
            return '';
        }

        return $resolved;
    }

    /**
     * Compute a path relative to a root directory, or null if outside it.
     *
     * @param string $root Absolute root directory (no trailing slash).
     * @param string $path Absolute path.
     * @return string|null Relative path with forward slashes, or null.
     */
    public static function relativeTo(string $root, string $path): ?string
    {
        $root = rtrim(str_replace('\\', '/', $root), '/');
        $path = str_replace('\\', '/', $path);
        $prefix = $root . '/';
        if (strncmp($path, $prefix, strlen($prefix)) !== 0) {
            return null;
        }

        return substr($path, strlen($prefix));
    }

    /**
     * Lexically normalize a path (resolve "." / ".." without touching disk).
     *
     * @param string $path Path to normalize.
     * @return string Normalized path.
     */
    private function normalizePath(string $path): string
    {
        $path  = str_replace('\\', '/', $path);
        $isAbs = $path !== '' && $path[0] === '/';
        $parts = explode('/', $path);
        $stack = [];
        foreach ($parts as $part) {
            if ($part === '' || $part === '.') {
                continue;
            }
            if ($part === '..') {
                array_pop($stack);
                continue;
            }
            $stack[] = $part;
        }

        return ($isAbs ? '/' : '') . implode('/', $stack);
    }

    // ------------------------------------------------------------------
    // Runtime seams (overridable in tests)
    // ------------------------------------------------------------------

    /**
     * Is a WP-CLI execution context available?
     *
     * @return bool
     */
    public function wpCliAvailable(): bool
    {
        return defined('WP_CLI') && WP_CLI;
    }

    /**
     * Run a WP-CLI command, capturing its outcome.
     *
     * @param array<int,string> $args Argument vector.
     * @return array{ok:bool,log:string}
     */
    protected function runWpCli(array $args): array
    {
        if (!class_exists('\WP_CLI')) {
            return ['ok' => false, 'log' => 'WP-CLI not loadable.'];
        }
        $command = implode(' ', array_map('escapeshellarg', $args));
        try {
            /** @var array{return_code?:int,stderr?:string} $res */
            $res  = \WP_CLI::runcommand($command, ['return' => 'all', 'exit_error' => false, 'launch' => false]);
            $code = isset($res['return_code']) ? (int) $res['return_code'] : 0;

            return ['ok' => $code === 0, 'log' => $code === 0 ? 'ok' : 'wp-cli error'];
        } catch (\Throwable $e) {
            return ['ok' => false, 'log' => 'WP-CLI execution error.'];
        }
    }

    /**
     * Allocate a temp file path under the system temp dir.
     *
     * @return string Absolute temp path, or '' on failure.
     */
    protected function tempFile(): string
    {
        $dir = function_exists('get_temp_dir') ? get_temp_dir() : sys_get_temp_dir();
        $tmp = tempnam($dir, 'wpmgr-db');

        return $tmp === false ? '' : $tmp;
    }

    /**
     * mysqldump-equivalent DB export using wpdb (fallback when WP-CLI absent).
     *
     * Emits a portable INSERT-based dump of every table. Intended as a
     * last-resort fallback; production sites should have WP-CLI available.
     *
     * @return string SQL dump (empty string on failure).
     */
    protected function exportDatabaseFallback(): string
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return '';
        }

        $tables = $wpdb->get_col('SHOW TABLES');
        if (!is_array($tables)) {
            return '';
        }

        $out = "-- WPMgr Agent DB export (fallback)\nSET FOREIGN_KEY_CHECKS=0;\n";
        foreach ($tables as $table) {
            if (!is_string($table) || $table === '') {
                continue;
            }
            $create = $wpdb->get_row('SHOW CREATE TABLE `' . str_replace('`', '', $table) . '`', ARRAY_N);
            if (is_array($create) && isset($create[1]) && is_string($create[1])) {
                $out .= "\nDROP TABLE IF EXISTS `{$table}`;\n" . $create[1] . ";\n";
            }
            $rows = $wpdb->get_results('SELECT * FROM `' . str_replace('`', '', $table) . '`', ARRAY_A);
            if (!is_array($rows)) {
                continue;
            }
            foreach ($rows as $row) {
                if (!is_array($row)) {
                    continue;
                }
                $cols = array_map(static fn ($c) => '`' . str_replace('`', '', (string) $c) . '`', array_keys($row));
                $vals = array_map(
                    static function ($v) use ($wpdb) {
                        if ($v === null) {
                            return 'NULL';
                        }
                        $escaped = $wpdb->_escape((string) $v);
                        if (is_array($escaped)) {
                            $escaped = '';
                        }

                        return "'" . $escaped . "'";
                    },
                    array_values($row)
                );
                $out .= 'INSERT INTO `' . $table . '` (' . implode(',', $cols) . ') VALUES (' . implode(',', $vals) . ");\n";
            }
        }
        $out .= "SET FOREIGN_KEY_CHECKS=1;\n";

        return $out;
    }

    /**
     * Execute a multi-statement SQL dump via wpdb (fallback import).
     *
     * @param string $sql SQL dump.
     * @return bool Success.
     */
    protected function importDatabaseFallback(string $sql): bool
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return false;
        }
        // Split on statement terminators at line ends (sufficient for our own
        // fallback export; arbitrary dumps should be imported via WP-CLI).
        $statements = preg_split('/;\s*\n/', $sql);
        if (!is_array($statements)) {
            return false;
        }
        $ok = true;
        foreach ($statements as $statement) {
            $statement = trim($statement);
            if ($statement === '' || str_starts_with($statement, '--')) {
                continue;
            }
            if ($wpdb->query($statement) === false) {
                $ok = false;
            }
        }

        return $ok;
    }

    /**
     * Resolve the WordPress DB handle, or null outside a WP runtime.
     *
     * @return \wpdb|null
     */
    protected function wpdb(): ?object
    {
        if (!isset($GLOBALS['wpdb']) || !is_object($GLOBALS['wpdb'])) {
            return null;
        }

        /** @var \wpdb $wpdb */
        $wpdb = $GLOBALS['wpdb'];

        return $wpdb;
    }
}
