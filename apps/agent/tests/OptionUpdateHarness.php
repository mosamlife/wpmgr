<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey\Functions;

/** WordPress's update filter ordering and missing-option branch over captured hooks. */
trait OptionUpdateHarness
{
    private function installOptionUpdates(): void
    {
        Functions\when('maybe_serialize')->alias(static fn ($value) => is_array($value) || is_object($value) ? serialize($value) : $value);
        Functions\when('update_option')->alias(function ($name, $value): bool {
            $before = get_option($name);
            $value = $this->optionFilter('pre_update_option_' . $name, $value, $before, $name);
            $value = $this->optionFilter('pre_update_option', $value, $name, $before);
            if ($value === $before || serialize($value) === serialize($before)) {
                return false;
            }
            if (!array_key_exists($name, $this->options)) {
                $added = add_option($name, $value);
                if ($added) {
                    $this->fire('added_option', $name, $value);
                }
                return $added;
            }
            $this->options[$name] = $value;
            $this->fire('updated_option', $name, $before, $value);
            return true;
        });
    }

    private function optionFilter(string $tag, mixed ...$args): mixed
    {
        $rows = array_values(array_filter($this->hooks, static fn ($r) => $r[0] === $tag));
        usort($rows, static fn ($a, $b) => $a[2] <=> $b[2]);
        foreach ($rows as [, $callback, , $accepted]) {
            $args[0] = $callback(...array_slice($args, 0, $accepted));
        }
        return $args[0];
    }
}
