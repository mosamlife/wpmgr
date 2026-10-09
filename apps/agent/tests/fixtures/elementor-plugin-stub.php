<?php
/**
 * Declares a class under Elementor's real name, `Elementor\Plugin`, with the two
 * public members the collector reads.
 *
 * Only ever loaded by a test that runs in its own PHP process: a class cannot
 * be undefined, so loading this in the shared PHPUnit process would make every
 * later test see "Elementor is loaded".
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace Elementor;

/**
 * Elementor main plugin class stand-in under its real name.
 */
final class Plugin
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
