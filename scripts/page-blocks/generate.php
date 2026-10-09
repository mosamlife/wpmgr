<?php
// Renders outlines.json through the agent's real PageCreateBuilder and writes
// {"cases":[{"name","content"}]} to the path in argv[1]. Exits non-zero on any
// refusal, any empty render, or zero cases. The builder is standalone.
declare(strict_types=1);

define('ABSPATH', '/');
require dirname(__DIR__, 2) . '/apps/agent/includes/abilities/class-page-create-builder.php';

use WPMgr\Agent\Abilities\PageCreateBuilder;

function fail(string $m): never
{
    fwrite(STDERR, "generate.php: $m\n");
    exit(1);
}

$out = $argv[1] ?? '';
if ($out === '') {
    fail('usage: generate.php <out.json>');
}
$defs = json_decode((string) file_get_contents(__DIR__ . '/outlines.json'), false);
if (!is_array($defs) || count($defs) === 0) {
    fail('outlines.json has no cases');
}

$cases = [];
foreach ($defs as $d) {
    $outline = $d->outline;
    foreach ($outline as $n) {
        // Long text: Unicode-heavy, & throughout, just under the per-text limit.
        if (($n->text ?? null) === 'LONGTEXT') {
            $n->text = mb_substr(str_repeat('Long & winding text, Ünï 東京 🚀. ', 400), 0, 4900);
        }
        if (isset($n->items)) {
            foreach ($n->items as $i => $it) {
                if ($it === 'LONGTEXT') {
                    $n->items[$i] = mb_substr(str_repeat('Item & long 東京. ', 300), 0, 4000);
                }
            }
        }
    }
    $input = (object) ['post_type' => 'page', 'editor' => PageCreateBuilder::EDITOR_BLOCKS, 'title' => 'T ' . $d->name, 'outline' => $outline];
    $r = PageCreateBuilder::validate($input);
    if (!isset($r['spec'])) {
        fail($d->name . ' refused: ' . ($r['code'] ?? '?') . ' ' . ($r['detail'] ?? ''));
    }
    $content = PageCreateBuilder::render($r['spec']);
    if (trim($content) === '') {
        fail($d->name . ' rendered empty');
    }
    $cases[] = ['name' => $d->name, 'content' => $content];
}
if (count($cases) === 0) {
    fail('no cases generated');
}
file_put_contents($out, json_encode(['cases' => $cases], JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR));
fwrite(STDERR, 'generate.php: wrote ' . count($cases) . " cases\n");
