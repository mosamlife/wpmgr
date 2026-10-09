<?php
/**
 * Stand-in for Elementor's experiments manager, for BuilderFacts tests.
 *
 * Answers `is_feature_active()` with whatever it was built to answer (or throws
 * what it was built to throw) and records every feature name it was asked
 * about, so a test can pin the experiment name the collector asks for.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Elementor `Experiments_Manager` stand-in.
 */
final class FakeElementorExperiments
{
    /**
     * Feature names the collector asked about, in order.
     *
     * @var list<string>
     */
    public array $asked = [];

    /**
     * @param mixed           $answer What is_feature_active() returns.
     * @param \Throwable|null $throws What is_feature_active() throws instead, when set.
     */
    public function __construct(private mixed $answer = true, private ?\Throwable $throws = null)
    {
    }

    /**
     * Same signature as Elementor's method.
     *
     * @param string $featureName       Experiment name.
     * @param bool   $checkDependencies Unused.
     * @return mixed
     */
    public function is_feature_active($featureName, $checkDependencies = false)
    {
        $this->asked[] = (string) $featureName;

        if ($this->throws !== null) {
            throw $this->throws;
        }

        return $this->answer;
    }
}
