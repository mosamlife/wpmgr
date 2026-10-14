<?php
declare(strict_types=1);

namespace Elementor;

/** Core heading registered from the stand-in install's widget directory. */
class Widget_Roundtrip_Heading
{
    public function __construct(private array $controls)
    {
    }

    public function get_name(): string
    {
        return 'heading';
    }

    public function get_title(): string
    {
        return 'Heading';
    }

    public function get_categories(): array
    {
        return ['basic'];
    }

    public function get_controls(): array
    {
        return $this->controls;
    }
}
