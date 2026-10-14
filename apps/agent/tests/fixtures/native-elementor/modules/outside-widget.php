<?php

declare(strict_types=1);

namespace Elementor; // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedNamespaceFound -- isolated fixture models a widget subclass in Elementor's namespace

/** A subclass outside the core widget source directory retains its own source identity. */
final class Widget_Wpmgr_Outside_Probe extends Widget_Wpmgr_Core_Probe
{
}
