<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

/** Reconstitute live control metadata from the measured widget fixture. */
final class RealNativeControls
{
    public static function install(FakeElementorApi $api): void
    {
        $fixture = json_decode(file_get_contents(__DIR__ . '/../fixtures/native-elementor/real-controls-3.35.9.json'), true, 512, JSON_THROW_ON_ERROR);
        foreach ($fixture['widgets'] as $widget) {
            $name = $widget['name'];
            $api->widgetTypes[] = $name;
            $api->coreWidgets[] = $name;
            $api->widgetCatalogue = array_values(array_filter($api->widgetCatalogue, static fn ($row) => $row['name'] !== $name));
            $api->widgetCatalogue[] = ['name' => $name, 'title' => $widget['title'], 'categories' => ['basic'], 'pro' => false, 'atomic' => false];
            $api->widgetControls[$name] = self::controls($widget['controls']);
        }
    }

    private static function controls(array $controls): array
    {
        $raw = [];
        foreach ($controls as $control) {
            $id = $control['id'];
            $raw[$id] = ['name' => $id, 'type' => $control['type'], 'label' => str_repeat('L', 30),
                'options' => array_fill_keys($control['options'], 'Option')];
            if ($control['responsive'] !== null) {
                $raw[$id]['is_responsive'] = true;
                $raw[$id]['responsive'] = ['devices' => $control['responsive']['devices']];
            }
            if ($control['group'] !== null) {
                $group = $control['group'];
                $raw[$id]['groupType'] = $group['family'];
                $raw[$id]['groupPrefix'] = $group['prefix'];
                if ($id !== $group['activator'] && $group['activator'] !== null) {
                    $raw[$id]['condition'] = [$group['activator'] => 'custom'];
                }
            }
            if ($control['fields'] !== []) {
                $raw[$id]['fields'] = self::controls($control['fields']);
            }
        }
        return $raw;
    }
}
