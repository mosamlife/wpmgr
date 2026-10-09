<?php
/**
 * Stand-in for Elementor's main plugin class, for BuilderFacts tests.
 *
 * It has the same two public members the collector reads: a static `$instance`
 * and an instance `$experiments`. It lives in the test namespace so that
 * loading it never defines a class in Elementor's own namespace for the rest
 * of the PHPUnit process; the one test that needs the real name runs in its own
 * process.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Elementor `Plugin` stand-in.
 */
final class FakeElementorPlugin
{
    /**
     * @var mixed
     */
    public static $instance = null;

    /**
     * @var mixed
     */
    public $experiments = null;
}
