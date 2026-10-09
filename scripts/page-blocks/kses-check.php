<?php
// Runs the agent's generated page markup through the save pipeline of a real
// WordPress core, as a user WITHOUT unfiltered_html, and reports any byte that
// the sanitiser would change. This is the same test the agent applies before it
// writes a page (it refuses the write when the site would alter the content),
// run in CI against pinned cores instead of on a customer's site.
//
// Usage: php kses-check.php <core-root> <expected-wp-version> <known-file|-> <markup.json>...
//   <core-root>    holds wordpress/wp-includes/ (the php files are enough)
//   <known-file>   cases a given core is KNOWN to change, one per line:
//                  "<version> <markup file name> <case name>"; "-" for none
//   <markup.json>  {"cases":[{"name","content","title"?}]}
//
// A known case must still change (otherwise the entry is stale and the run is
// red), and every other change is red. Exits non-zero on: an unlisted changed
// byte, a stale known entry, a core that does not boot or is not the version it
// claims, a save chain that is missing the sanitiser, an empty markup file, a
// case with empty content, or zero cases checked.
declare(strict_types=1);

// Old cores on a new PHP raise deprecations that say nothing about our bytes.
error_reporting(E_ALL & ~E_DEPRECATED);
// One copy of any fatal error, on stderr, without argument dumps.
ini_set('display_errors', 'stderr');
ini_set('log_errors', '0');
ini_set('html_errors', '0');
ini_set('zend.exception_ignore_args', '1');

function fail(string $m): never
{
    fwrite(STDERR, "kses-check: $m\n");
    exit(1);
}

if ($argc < 5) {
    fail('usage: kses-check.php <core-root> <expected-wp-version> <known-file|-> <markup.json>...');
}
$root      = rtrim((string) $argv[1], '/');
$expected  = (string) $argv[2];
$knownFile = (string) $argv[3];
$files     = array_slice($argv, 4);

// Known changes for THIS core: "<markup file name>\t<case name>" => seen yet?
$known = [];
if ($knownFile !== '-') {
    $lines = is_file($knownFile) ? file($knownFile, FILE_IGNORE_NEW_LINES) : false;
    if ($lines === false) {
        fail("cannot read the known-changes file: $knownFile");
    }
    foreach ($lines as $n => $line) {
        $line = trim($line);
        if ($line === '' || $line[0] === '#') {
            continue;
        }
        $f = preg_split('/\s+/', $line);
        if ($f === false || count($f) !== 3) {
            fail('known-changes line ' . ($n + 1) . ' needs "<version> <markup file> <case>": ' . $line);
        }
        if ($f[0] === $expected) {
            $known[$f[1] . "\t" . $f[2]] = false;
        }
    }
}

define('ABSPATH', $root . '/wordpress/');
define('WPINC', 'wp-includes');

$load = [
    'version.php', 'compat.php', 'plugin.php', 'load.php', 'formatting.php', 'functions.php',
    'class-wp-token-map.php',
    'html-api/html5-named-character-references.php', 'html-api/class-wp-html-attribute-token.php',
    'html-api/class-wp-html-span.php', 'html-api/class-wp-html-doctype-info.php',
    'html-api/class-wp-html-text-replacement.php', 'html-api/class-wp-html-decoder.php',
    'html-api/class-wp-html-tag-processor.php',
    'kses.php',
    // core's kses reads block comment attributes through the block parser.
    'class-wp-block-parser-block.php', 'class-wp-block-parser-frame.php', 'class-wp-block-parser.php', 'blocks.php',
];
// A core older than a file in this list simply does not have it. The files
// below are not optional in any core: without them there is nothing to test.
$required = ['version.php', 'plugin.php', 'formatting.php', 'functions.php', 'kses.php', 'default-filters.php'];
foreach ($required as $f) {
    if (!is_file(ABSPATH . WPINC . '/' . $f)) {
        fail("core is missing wp-includes/$f under $root");
    }
}
foreach ($load as $f) {
    $p = ABSPATH . WPINC . '/' . $f;
    if (is_file($p)) {
        require_once $p;
    }
}
if (!isset($wp_version) || $wp_version !== $expected) {
    fail('core reports version ' . var_export($wp_version ?? null, true) . ' but the pin says ' . var_export($expected, true));
}

// The one function the save chain needs that core defines outside the files
// above: the user being simulated has no unfiltered_html.
if (!function_exists('current_user_can')) {
    function current_user_can($capability)
    {
        return false;
    }
}
// core's balanceTags reads this option on every save. A fresh install stores 0
// (no tag balancing), and there is no database here, so answer it the way core
// lets any option be answered before it is read.
add_filter('pre_option_use_balanceTags', static fn() => 0);

// Register core's own default filters, then the sanitiser a user without
// unfiltered_html gets (kses_init does exactly this on a real site).
require ABSPATH . WPINC . '/default-filters.php';
kses_init_filters();

// The chain under test must really be the save chain. If any of these is
// missing the comparison below would pass over nothing.
$chain = [
    ['content_save_pre', 'convert_invalid_entities'],
    ['content_save_pre', 'wp_filter_post_kses'],
    ['title_save_pre', 'wp_filter_kses'],
    ['pre_kses', 'wp_pre_kses_less_than'],
    ['pre_kses', 'wp_pre_kses_block_attributes'],
];
foreach ($chain as [$hook, $fn]) {
    if (!function_exists($fn) || has_filter($hook, $fn) === false) {
        fail("the save chain of WP $wp_version has no $fn on $hook");
    }
}
// What the chain calls into; a core that boots without these cannot sanitise.
foreach (['filter_block_content', 'parse_blocks', 'wp_kses_post', 'wp_slash', 'wp_unslash'] as $fn) {
    if (!function_exists($fn)) {
        fail("WP $wp_version booted without $fn()");
    }
}

/** A short, safe rendering of a byte string around the first difference. */
function around(string $s, int $at): string
{
    $part = substr($s, max(0, $at - 70), 170);

    return (string) json_encode($part, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE);
}

function firstDiff(string $a, string $b): int
{
    $i = 0;
    $n = min(strlen($a), strlen($b));
    while ($i < $n && $a[$i] === $b[$i]) {
        $i++;
    }

    return $i;
}

$cases     = 0;
$changed   = 0;
$knownSeen = 0;
foreach ($files as $file) {
    if (!is_file($file) || filesize($file) === 0) {
        fail("markup file missing or empty: $file");
    }
    $doc = json_decode((string) file_get_contents($file), true);
    if (!is_array($doc) || !isset($doc['cases']) || !is_array($doc['cases']) || $doc['cases'] === []) {
        fail("no cases in $file");
    }
    $label = basename($file);
    foreach ($doc['cases'] as $i => $c) {
        $name    = is_array($c) && isset($c['name']) && is_string($c['name']) && $c['name'] !== '' ? $c['name'] : null;
        $content = is_array($c) && isset($c['content']) && is_string($c['content']) ? $c['content'] : null;
        if ($name === null || $content === null || $content === '') {
            fail("case #$i in $file has no name or no content; an empty page would pass for the wrong reason");
        }
        $cases++;
        $report = '';

        // Exactly what the agent runs before it writes (see the agent's page
        // create builder step): the save filters, then a direct kses pass.
        $viaFilters = wp_unslash(apply_filters('content_save_pre', wp_slash($content)));
        $direct     = wp_kses_post($content);
        foreach (['content_save_pre' => $viaFilters, 'wp_kses_post' => $direct] as $via => $out) {
            if ($out !== $content) {
                $at      = firstDiff($content, $out);
                $report .= "  via $via\n    in : " . around($content, $at) . "\n    out: " . around($out, $at) . "\n";
            }
        }

        if (isset($c['title'])) {
            $title = $c['title'];
            if (!is_string($title) || $title === '') {
                fail("case $name in $file has an empty or non-string title");
            }
            $simTitle = wp_unslash(apply_filters('title_save_pre', wp_slash($title)));
            if ($simTitle !== $title) {
                $at      = firstDiff($title, $simTitle);
                $report .= "  via title_save_pre\n    in : " . around($title, $at) . "\n    out: " . around($simTitle, $at) . "\n";
            }
        }
        if ($report === '') {
            continue;
        }
        $key = $label . "\t" . $name;
        if (array_key_exists($key, $known)) {
            $known[$key] = true;
            $knownSeen++;
            // The first difference is evidence enough; the full diff is for a real change.
            echo "KSES-KNOWN $label:$name\n" . implode("\n", array_slice(explode("\n", $report), 0, 3)) . "\n";
        } else {
            $changed++;
            echo "KSES-CHANGED $label:$name\n$report";
        }
    }
}
if ($cases === 0) {
    fail('zero cases checked');
}
// A known change that did not happen: the core or the builder moved, and the
// list must move with it, or it would keep excusing nothing.
$stale = 0;
foreach ($known as $key => $seen) {
    if (!$seen) {
        $stale++;
        [$k1, $k2] = explode("\t", (string) $key, 2);
        echo "KSES-STALE $k1:$k2 is listed as a known change on WP $wp_version, but it was not changed (or is not in the markup)\n";
    }
}
echo 'kses-check WP ' . $wp_version . ': ' . count($files) . ' file(s), ' . $cases . ' case(s), '
    . $changed . ' changed, ' . $knownSeen . ' known, ' . $stale . " stale\n";
exit($changed === 0 && $stale === 0 ? 0 : 1);
