<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

if (!defined('ABSPATH')) {
    exit;
}

/**
 * Compact classic widget schema shared by discovery and native validation.
 *
 * document: {widget: string, atomic: false, controls: map<id, control>}.
 * A control has type, optional label/default, options (a list of option keys),
 * units (a list of slider/dimensions units), multiple (select2 boolean),
 * responsive (true, false, or {min?: breakpoint, max?: breakpoint}),
 * group (typography, background, border or box_shadow), and activator
 * ({key: control id, values: nonempty list<string>}). Repeater fields are a
 * map of row control ids to the same control shape. Group members carry both
 * group and activator; the activator control may carry them too.
 *
 * Discovery constructs this from the live widget registry, never AI input.
 * libraryImages is a separate trusted inventory of {id: numeric string,
 * url: string}: the caller must resolve each attachment on this site, verify
 * it is a library image, and supply its canonical attachment URL. An absent
 * inventory authorises no media. It is not part of the public control schema.
 * No database lookup, coercion or mutation occurs in this value object.
 * AI input uses NativeSettingsValidator::validateRaw or validateWritten.
 * Stored-value shape checks retain stored booleans; AI-written settings use
 * string scalars, including dimensions isLinked "yes" or "", and top-level
 * null only as a deletion instruction. The raw parser refuses duplicate keys.
 */
final class NativeControlSchema
{
    /** Maximum encoded compact schema size. */
    public const MAX_BYTES = 32768;

    /** @var array<string,mixed> */
    public readonly array $document;

    /** @var list<array{id:string,url:string}> */
    public readonly array $libraryImages;

    /**
     * @param array<string,mixed>                  $document      Live compact schema.
     * @param list<array{id:string,url:string}>    $libraryImages Verified library images.
     */
    public function __construct(array $document, array $libraryImages = [])
    {
        $this->document      = $document;
        $this->libraryImages = $libraryImages;
    }
}
