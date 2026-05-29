<?php
/**
 * Unit tests for the SqlInspector. Writes a synthetic WordPress-like SQL dump
 * to a temp file, runs the inspector, and asserts the structured report
 * carries the expected siteurl/prefix/row-count signals.
 *
 * Kept synthetic on purpose — DbDumperTest covers the live-DB integration
 * path; this test focuses on the parser regex + tuple counter, which are
 * the only moving parts unique to SqlInspector.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\SqlInspector;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\SqlInspector
 */
final class SqlInspectorTest extends TestCase
{
    /** Absolute paths to clean up between tests. */
    private array $tempFiles = [];

    protected function tear_down(): void
    {
        foreach ($this->tempFiles as $path) {
            if (is_file($path)) {
                @unlink($path);
            }
        }
        $this->tempFiles = [];
        parent::tear_down();
    }

    /**
     * The class must autoload via the includes/ classmap.
     */
    public function test_class_exists_in_expected_namespace(): void
    {
        $this->assertTrue(class_exists(SqlInspector::class));
    }

    /**
     * Synthetic WP-shaped dump: SET NAMES + CREATE TABLE wp_options +
     * CREATE TABLE wp_users + a 3-row multi-row INSERT carrying siteurl /
     * home / db_version. Asserts that every restore-safety preflight
     * signal the CP cares about is present in the report.
     */
    public function test_inspect_synthetic_wordpress_dump(): void
    {
        $sql = <<<'SQL'
SET NAMES utf8mb4;
CREATE TABLE `wp_options` (
  `option_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `option_name` varchar(191) NOT NULL DEFAULT '',
  `option_value` longtext NOT NULL,
  `autoload` varchar(20) NOT NULL DEFAULT 'yes',
  PRIMARY KEY (`option_id`)
) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
INSERT INTO `wp_options` (`option_id`, `option_name`, `option_value`, `autoload`) VALUES (1,'siteurl','https://example.com','yes'),(2,'home','https://example.com','yes'),(3,'db_version','58975','yes');
CREATE TABLE `wp_users` (
  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_login` varchar(60) NOT NULL,
  PRIMARY KEY (`ID`)
) ENGINE=InnoDB AUTO_INCREMENT=1 DEFAULT CHARSET=utf8mb4;
INSERT INTO `wp_users` VALUES (1,'admin');
SQL;

        $path = $this->writeTempDump($sql, false);
        $report = (new SqlInspector())->inspect($path);

        $this->assertSame(1, $report['schema_version']);
        $this->assertSame('agent', $report['source']);
        $this->assertSame('utf8mb4', $report['charset']);
        $this->assertSame('wp_', $report['table_prefix']);
        $this->assertTrue($report['is_wordpress']);
        $this->assertSame('https://example.com', $report['siteurl']);
        $this->assertSame('https://example.com', $report['home']);
        $this->assertSame('58975', $report['wp_version']);

        // Two tables, in order. wp_options first.
        $this->assertCount(2, $report['tables']);
        $this->assertSame('wp_options', $report['tables'][0]['name']);
        $this->assertSame(3, $report['tables'][0]['rows_estimate']);
        $this->assertSame(42, $report['tables'][0]['auto_increment']);
        $this->assertSame('utf8mb4', $report['tables'][0]['charset']);

        $this->assertSame('wp_users', $report['tables'][1]['name']);
        $this->assertSame(1, $report['tables'][1]['rows_estimate']);
    }

    /**
     * Same synthetic dump, but written as a gzip file. inspect() should
     * decode transparently via gzopen — this is how the production path
     * works (DbDumper writes `.sql.gz`).
     */
    public function test_inspect_handles_gzipped_dump(): void
    {
        $sql = <<<'SQL'
SET NAMES utf8mb4;
CREATE TABLE `wp_options` (`option_id` bigint NOT NULL AUTO_INCREMENT, PRIMARY KEY (`option_id`)) ENGINE=InnoDB AUTO_INCREMENT=10 DEFAULT CHARSET=utf8mb4;
INSERT INTO `wp_options` VALUES (1,'siteurl','https://gz.example','yes');
CREATE TABLE `wp_users` (`ID` bigint NOT NULL AUTO_INCREMENT, PRIMARY KEY (`ID`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
SQL;
        $path = $this->writeTempDump($sql, true);
        $report = (new SqlInspector())->inspect($path);

        $this->assertSame('https://gz.example', $report['siteurl']);
        $this->assertTrue($report['is_wordpress']);
        $this->assertSame('wp_', $report['table_prefix']);
    }

    /**
     * A dump with no `_options` + `_users` co-located prefix is not
     * WordPress. The inspector should still return a well-formed report —
     * it just flips `is_wordpress` to false.
     */
    public function test_inspect_non_wordpress_dump(): void
    {
        $sql = <<<'SQL'
SET NAMES utf8mb4;
CREATE TABLE `customers` (`id` int NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO `customers` VALUES (1,'a'),(2,'b'),(3,'c'),(4,'d');
SQL;
        $path = $this->writeTempDump($sql, false);
        $report = (new SqlInspector())->inspect($path);

        $this->assertFalse($report['is_wordpress']);
        $this->assertSame('', $report['siteurl']);
        $this->assertCount(1, $report['tables']);
        $this->assertSame('customers', $report['tables'][0]['name']);
        $this->assertSame(4, $report['tables'][0]['rows_estimate']);
    }

    /**
     * countTuples must NOT be fooled by `),(` inside a quoted string value.
     * This is the canonical false-positive risk of a naive substring count.
     */
    public function test_inspect_tuple_count_ignores_quoted_parens(): void
    {
        // Three real tuples; the middle row's option_value contains the
        // literal text "),(" which a naive counter would treat as a tuple
        // separator.
        $sql = <<<'SQL'
SET NAMES utf8mb4;
CREATE TABLE `wp_options` (`option_id` int) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE `wp_users` (`ID` int) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO `wp_options` VALUES (1,'a','simple','yes'),(2,'tricky','before),(after','yes'),(3,'c','simple','yes');
SQL;
        $path = $this->writeTempDump($sql, false);
        $report = (new SqlInspector())->inspect($path);

        $this->assertSame(3, $report['tables'][0]['rows_estimate']);
    }

    /**
     * Write SQL to a temp file (optionally gzipped) and queue it for
     * cleanup. Returns the absolute path.
     */
    private function writeTempDump(string $sql, bool $gzip): string
    {
        $suffix = $gzip ? '.sql.gz' : '.sql';
        $path   = tempnam(sys_get_temp_dir(), 'wpmgr-sqlinsp-') . $suffix;
        if ($gzip) {
            $fh = gzopen($path, 'wb');
            $this->assertIsResource($fh);
            gzwrite($fh, $sql);
            gzclose($fh);
        } else {
            file_put_contents($path, $sql);
        }
        $this->tempFiles[] = $path;
        // tempnam() created an empty file at the suffix-less path; clean it.
        $stub = substr($path, 0, -strlen($suffix));
        if (is_file($stub)) {
            $this->tempFiles[] = $stub;
        }
        return $path;
    }
}
