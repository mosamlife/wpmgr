<?php
/**
 * RouterTokenErrorKeyFileTest: a run of RouterTokenErrorTest leaves no file
 * behind in the temporary directory.
 *
 * WPMGR_AGENT_KEY_FILE is defined once per process, so its file outlives the
 * test that named it and every later test recreates it. The run happens in a
 * child process given a temporary directory of its own: the class is then the
 * one that names the constant, and whatever the run leaves is in that
 * directory and nowhere else.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @coversNothing
 */
final class RouterTokenErrorKeyFileTest extends TestCase
{
    private const DIR_PREFIX = 'wpmgr-tokenerr-run-';

    /** The child's temporary directory, made by set_up(). */
    private string $dir = '';

    protected function set_up(): void
    {
        parent::set_up();

        $this->dir = sys_get_temp_dir() . '/' . self::DIR_PREFIX . bin2hex(random_bytes(8));
        if (!mkdir($this->dir, 0700)) {
            $this->fail('could not create ' . $this->dir);
        }
    }

    protected function tear_down(): void
    {
        // Remove the directory set_up() made and what the run left in it.
        // Anchored to that one path; a link is removed, never followed.
        $own = strpos($this->dir, sys_get_temp_dir() . '/' . self::DIR_PREFIX) === 0;
        if ($own && is_dir($this->dir) && !is_link($this->dir)) {
            foreach ($this->paths($this->dir) as $path) {
                if (is_dir($path) && !is_link($path)) {
                    rmdir($path);
                } else {
                    unlink($path);
                }
            }
            rmdir($this->dir);
        }

        parent::tear_down();
    }

    public function test_a_run_of_the_class_leaves_no_file_behind(): void
    {
        $agentDir = dirname(__DIR__);
        $process  = proc_open(
            [
                PHP_BINARY,
                '-d',
                'memory_limit=1G',
                '-d',
                'sys_temp_dir=' . $this->dir,
                $agentDir . '/vendor/bin/phpunit',
                '--configuration',
                $agentDir . '/phpunit.xml.dist',
                '--do-not-cache-result',
                '--colors=never',
                $agentDir . '/tests/RouterTokenErrorTest.php',
            ],
            [1 => ['pipe', 'w'], 2 => ['pipe', 'w']],
            $pipes,
            $agentDir
        );
        $this->assertIsResource($process, 'could not start the child run');

        $output = (string) stream_get_contents($pipes[1]) . (string) stream_get_contents($pipes[2]);
        fclose($pipes[1]);
        fclose($pipes[2]);
        $status = proc_close($process);

        // Positive controls: the class ran and passed, and the child's
        // temporary directory was this one (the bootstrap makes its ABSPATH
        // parent there), so an empty result below is not a vacuous pass.
        $this->assertSame(0, $status, $output);
        $this->assertMatchesRegularExpression('/^OK\b/m', $output);
        $this->assertDirectoryExists($this->dir . '/wpmgr_wp_abspath', 'the child used another temporary directory');

        $left = [];
        foreach ($this->paths($this->dir) as $path) {
            if (!is_dir($path) || is_link($path)) {
                $left[] = substr($path, strlen($this->dir) + 1);
            }
        }
        sort($left);

        $this->assertSame([], $left, 'files the run left behind');
    }

    /**
     * Every path under $dir, each directory after its contents, links not
     * followed.
     *
     * @param string $dir Directory to walk.
     * @return list<string>
     */
    private function paths(string $dir): array
    {
        $names = scandir($dir);
        if ($names === false) {
            return [];
        }

        $paths = [];
        foreach (array_diff($names, ['.', '..']) as $name) {
            $path = $dir . '/' . $name;
            if (is_dir($path) && !is_link($path)) {
                array_push($paths, ...$this->paths($path));
            }
            $paths[] = $path;
        }

        return $paths;
    }
}
