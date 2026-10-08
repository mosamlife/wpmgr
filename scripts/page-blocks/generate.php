<?php
// Renders outlines.json through the agent's real PageCreateBuilder.
//
// Usage: php generate.php <blocks-out.json> [<classic-out.json>]
//
// Writes {"cases":[{"name","content","title"}]} to each path. Block editor
// markup goes to the first path; markup for the classic editor goes to the
// second, because the block validator reads classic markup as freeform. Exits
// non-zero on any refusal, any empty render, a render the builder's own
// retokenise pass would refuse, or zero cases. The builder is standalone.
//
// outlines.json is a list of cases:
//   name      unique, kebab-case
//   outline   the page-create outline, exactly as the AI would send it
//   classic   true: also render this case for the classic editor
//   title     the page title (default: "T <name>")
// Four conveniences exist only here, never in the builder's input grammar:
//   "LONGTEXT"      any string, replaced by 4900 characters of Unicode text
//   "LONGTEXT:<n>"  replaced by exactly n characters
//   {"@repeat":n,"node":X}  inside any list, replaced by n copies of X, with
//                   "{n}" in any string replaced by the copy's number (1-based)
// so the limit-sized cases stay readable.
//
// Image nodes name an attachment id; media.json maps each id to the address
// WordPress would give it. An id the map does not have is an error.
//
// Test seams (used by check-page-blocks_test.sh and check-page-kses_test.sh):
//   PAGE_BLOCKS_OUTLINES  use this outlines file
//   PAGE_BLOCKS_MEDIA     use this media map
declare(strict_types=1);

define('ABSPATH', '/');
require dirname(__DIR__, 2) . '/apps/agent/includes/abilities/class-page-create-builder.php';

use WPMgr\Agent\Abilities\PageCreateBuilder;

function fail(string $m): never
{
    fwrite(STDERR, "generate.php: $m\n");
    exit(1);
}

/** n characters of text with &, non-Latin script and an emoji in it. */
function longText(int $n): string
{
    $unit = 'Long & winding text, Ünï 東京 🚀. ';

    return mb_substr(str_repeat($unit, intdiv($n, mb_strlen($unit)) + 1), 0, $n);
}

/** Replace the helper markers (see the header) anywhere inside a decoded value. */
function expand(mixed $v, int $n = 0): mixed
{
    if (is_string($v)) {
        if ($v === 'LONGTEXT') {
            return longText(4900);
        }
        if (preg_match('/^LONGTEXT:([0-9]+)$/', $v, $m) === 1) {
            return longText((int) $m[1]);
        }

        return $n > 0 ? str_replace('{n}', (string) $n, $v) : $v;
    }
    if (is_array($v)) {
        $out = [];
        foreach ($v as $item) {
            if (is_object($item) && property_exists($item, '@repeat')) {
                $count = $item->{'@repeat'};
                if (!is_int($count) || $count < 1 || !property_exists($item, 'node')) {
                    fail('a @repeat needs a positive integer count and a node');
                }
                $template = json_encode($item->node, JSON_THROW_ON_ERROR);
                for ($i = 1; $i <= $count; $i++) {
                    $out[] = expand(json_decode($template, false, 512, JSON_THROW_ON_ERROR), $i);
                }
                continue;
            }
            $out[] = expand($item, $n);
        }

        return $out;
    }
    if (is_object($v)) {
        foreach (get_object_vars($v) as $k => $x) {
            $v->$k = expand($x, $n);
        }

        return $v;
    }

    return $v;
}

/** True when a decoded outline has a node of the given type anywhere inside it. */
function hasType(mixed $v, string $type): bool
{
    if (is_array($v)) {
        foreach ($v as $x) {
            if (hasType($x, $type)) {
                return true;
            }
        }

        return false;
    }
    if (is_object($v)) {
        if (($v->type ?? null) === $type) {
            return true;
        }
        foreach (get_object_vars($v) as $x) {
            if (hasType($x, $type)) {
                return true;
            }
        }
    }

    return false;
}

$outBlocks  = $argv[1] ?? '';
$outClassic = $argv[2] ?? '';
if ($outBlocks === '') {
    fail('usage: generate.php <blocks-out.json> [<classic-out.json>]');
}

$outlinesFile = getenv('PAGE_BLOCKS_OUTLINES');
$outlinesFile = is_string($outlinesFile) && $outlinesFile !== '' ? $outlinesFile : __DIR__ . '/outlines.json';
$mediaFile    = getenv('PAGE_BLOCKS_MEDIA');
$mediaFile    = is_string($mediaFile) && $mediaFile !== '' ? $mediaFile : __DIR__ . '/media.json';

$raw = file_get_contents($outlinesFile);
if ($raw === false) {
    fail("cannot read $outlinesFile");
}
$defs = json_decode($raw, false);
if (!is_array($defs) || count($defs) === 0) {
    fail("$outlinesFile has no cases");
}
$rawMedia = file_get_contents($mediaFile);
if ($rawMedia === false) {
    fail("cannot read $mediaFile");
}
$fixture = json_decode($rawMedia, true);
if (!is_array($fixture)) {
    fail("$mediaFile is not a JSON object");
}

$cases = [PageCreateBuilder::EDITOR_BLOCKS => [], PageCreateBuilder::EDITOR_CLASSIC => []];
$names = [];
foreach ($defs as $d) {
    $name = is_object($d) ? ($d->name ?? null) : null;
    if (!is_string($name) || preg_match('/^[a-z0-9][a-z0-9-]*$/', $name) !== 1) {
        fail('every case needs a kebab-case name');
    }
    if (isset($names[$name])) {
        fail("case name used twice: $name");
    }
    $names[$name] = true;
    if (!isset($d->outline) || !is_array($d->outline) || $d->outline === []) {
        fail("$name has no outline");
    }
    $editors = [PageCreateBuilder::EDITOR_BLOCKS];
    if (($d->classic ?? false) === true) {
        $editors[] = PageCreateBuilder::EDITOR_CLASSIC;
    }
    $needsMedia = hasType($d->outline, 'image');
    $frozen     = json_encode($d, JSON_THROW_ON_ERROR);

    foreach ($editors as $editor) {
        $label = $name . ' (' . $editor . ')';
        // A fresh copy per editor: expansion edits what it is given.
        $copy    = json_decode($frozen, false, 512, JSON_THROW_ON_ERROR);
        $outline = expand($copy->outline);
        $title   = expand(isset($copy->title) ? $copy->title : 'T ' . $name);
        $input   = (object) ['post_type' => 'page', 'editor' => $editor, 'title' => $title, 'outline' => $outline];

        $r = PageCreateBuilder::validate($input);
        if (!isset($r['spec'])) {
            fail($label . ' refused: ' . ($r['code'] ?? '?') . ' ' . ($r['detail'] ?? ''));
        }
        $spec  = $r['spec'];
        $media = [];
        if ($needsMedia) {
            if (!method_exists(PageCreateBuilder::class, 'mediaIds')) {
                fail($label . ' has an image, but the builder has no mediaIds()');
            }
            foreach (PageCreateBuilder::mediaIds($spec) as $id) {
                if (!isset($fixture[(string) $id]['url']) || !is_string($fixture[(string) $id]['url'])) {
                    fail($label . ' uses attachment ' . $id . ', which ' . basename($mediaFile) . ' does not have');
                }
                $media[$id] = ['url' => $fixture[(string) $id]['url']];
            }
            if ($media === []) {
                fail($label . ' has an image node but the builder asks for no media');
            }
        }
        $content = PageCreateBuilder::render($spec, $media);
        if (trim($content) === '') {
            fail($label . ' rendered empty');
        }
        // The agent runs this on its own render before it writes anything.
        $problem = PageCreateBuilder::retokenise($content);
        if ($problem !== null) {
            fail($label . ' is refused by the builder\'s own retokenise: ' . $problem);
        }
        $cases[$editor][] = ['name' => $name, 'content' => $content, 'title' => PageCreateBuilder::storedTitle($title)];
    }
}

$write = static function (string $path, array $list): void {
    if (count($list) === 0) {
        fail("no cases to write to $path");
    }
    file_put_contents($path, json_encode(['cases' => $list], JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR));
};
$write($outBlocks, $cases[PageCreateBuilder::EDITOR_BLOCKS]);
if ($outClassic !== '') {
    $write($outClassic, $cases[PageCreateBuilder::EDITOR_CLASSIC]);
}
fwrite(
    STDERR,
    'generate.php: wrote ' . count($cases[PageCreateBuilder::EDITOR_BLOCKS]) . ' block cases'
    . ($outClassic !== '' ? ' and ' . count($cases[PageCreateBuilder::EDITOR_CLASSIC]) . ' classic cases' : '')
    . "\n"
);
