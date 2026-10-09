<?php
/**
 * Elementor round trip, run inside a booted WordPress by run.sh.
 *
 * For every outline in the agent's classic golden fixtures and in
 * cases-extra.json, in both layouts (containers and sections):
 *
 *   1. build the Elementor tree with the agent's own ElementorClassicMapper
 *      (the agent's runtime autoloader, the real WordPress sanitiser);
 *   2. for a fixture case, require the built tree to equal the fixture's tree;
 *   3. save the tree with Elementor's Document::save as the agent's service
 *      user, which has no unfiltered_html, and read _elementor_data back from
 *      the database: it must decode to exactly the tree that was saved;
 *   4. render the page with Elementor's frontend and require the page to be
 *      non-empty, to hold every text of the outline as written, to show each
 *      image with its library alt text, and to hold no script element and no
 *      on* attribute.
 *
 * Arguments are key=value tokens (the CLI drops numeric-looking tokens):
 *   elementor=<x.y.z>   the Elementor version this boot must run
 *   wp=<x.y>            the WordPress version this boot must run
 *   php=<x.y>           the PHP version this boot must run
 *   zip_sha256=<hex>    the sha256 the mounted Elementor zip must have
 *   plant=<kind>@<case> a deliberate defect on one case, used only by the
 *                       self-test (scripts/elementor-roundtrip_test.sh) to
 *                       prove each check can fail; kinds: PLANTS below
 *
 * Output is lines starting "rt: ". The verdict is the line "rt: RESULT OK"
 * or "rt: RESULT FAIL"; the exit status is 0, 1 and 2 (could not run).
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

use Elementor\Plugin;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Abilities\ServicePrincipal;

/** Deliberate defects the self-test can request, by kind. */
const RT_PLANTS = [
    // A widget type Elementor does not know is added to the tree before it is saved; Elementor drops it silently.
    'unregistered_widget',
    // The mapper's output no longer equals the golden tree.
    'mapper_drift',
    // The rendered page gains a script element.
    'render_script',
    // The rendered page gains an on* attribute.
    'render_onclick',
    // Every ampersand in the rendered page is escaped twice.
    'render_text',
];

const RT_NODE_LIMIT_DEPTH = 64;

function rt_out(string $line): void
{
    echo 'rt: ' . $line . "\n";
}

/** The run cannot give a verdict: say why and stop. */
function rt_broken(string $why): void
{
    rt_out('BROKEN ' . $why);
    rt_out('RESULT FAIL');
    exit(2);
}

/** @param mixed $v */
function rt_brief($v): string
{
    $s = is_string($v) ? $v : (string) json_encode($v, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_PARTIAL_OUTPUT_ON_ERROR);

    return strlen($s) > 90 ? substr($s, 0, 87) . '...' : $s;
}

/**
 * The first place two decoded trees differ, or null when they are identical
 * in value, type and key order.
 *
 * @param mixed $want
 * @param mixed $got
 */
function rt_diff($want, $got, string $path): ?string
{
    if (is_array($want) && is_array($got)) {
        $wk = array_keys($want);
        $gk = array_keys($got);
        foreach (array_diff($wk, $gk) as $k) {
            return $path . '.' . $k . ': missing from the stored tree';
        }
        foreach (array_diff($gk, $wk) as $k) {
            return $path . '.' . $k . ': added to the stored tree';
        }
        if (array_is_list($want) && count($want) !== count($got)) {
            return $path . ': ' . count($want) . ' item(s) saved, ' . count($got) . ' stored';
        }
        foreach ($wk as $k) {
            $d = rt_diff($want[$k], $got[$k], $path . '.' . $k);
            if ($d !== null) {
                return $d;
            }
        }
        if ($wk !== $gk) {
            return $path . ': same keys in a different order';
        }

        return null;
    }
    if ($want === $got) {
        return null;
    }

    return $path . ': saved ' . rt_brief($want) . ' but stored ' . rt_brief($got);
}

/** Comparable form of visible text: one space for any run of whitespace, typographic marks as plain ones. */
function rt_norm(string $s): string
{
    $s = strtr($s, [
        "\u{00A0}" => ' ',
        "\u{201C}" => '"',
        "\u{201D}" => '"',
        "\u{2018}" => "'",
        "\u{2019}" => "'",
        "\u{2013}" => '-',
        "\u{2014}" => '-',
        "\u{2026}" => '...',
    ]);

    return trim((string) preg_replace('/\s+/u', ' ', $s));
}

/**
 * The texts an outline holds, as the AI wrote them.
 *
 * @param mixed        $node
 * @param list<string> $out
 */
function rt_texts($node, array &$out): void
{
    if (is_array($node)) {
        foreach ($node as $child) {
            rt_texts($child, $out);
        }

        return;
    }
    if (!is_object($node)) {
        return;
    }
    $add = static function ($v) use (&$out): void {
        if (is_string($v) && $v !== '') {
            $out[] = $v;
        }
    };
    switch ($node->type ?? '') {
        case 'heading':
        case 'paragraph':
            $add($node->text ?? null);
            break;
        case 'list':
            foreach ((array) ($node->items ?? []) as $item) {
                $add($item);
            }
            break;
        case 'quote':
            foreach ((array) ($node->paragraphs ?? []) as $p) {
                $add($p);
            }
            $add($node->citation ?? null);
            break;
        case 'table':
            foreach ((array) ($node->header ?? []) as $cell) {
                $add($cell);
            }
            foreach ((array) ($node->rows ?? []) as $row) {
                foreach ((array) $row as $cell) {
                    $add($cell);
                }
            }
            break;
        case 'buttons':
            foreach ((array) ($node->buttons ?? []) as $b) {
                $add($b->text ?? null);
            }
            break;
        case 'image':
            $add($node->caption ?? null);
            break;
        case 'group':
            rt_texts($node->children ?? [], $out);
            break;
        case 'columns':
            foreach ((array) ($node->columns ?? []) as $col) {
                rt_texts($col->children ?? [], $out);
            }
            break;
    }
}

/**
 * The library alt text each image of an outline must show, by attachment id.
 *
 * @param mixed             $node
 * @param array<int,string> $out
 */
function rt_alts($node, array &$out): void
{
    if (is_array($node)) {
        foreach ($node as $child) {
            rt_alts($child, $out);
        }

        return;
    }
    if (!is_object($node)) {
        return;
    }
    if (($node->type ?? '') === 'image') {
        $out[(int) ($node->attachment_id ?? 0)] = (string) ($node->alt ?? '');
    }
    rt_alts($node->children ?? [], $out);
    foreach ((array) ($node->columns ?? []) as $col) {
        rt_alts($col->children ?? [], $out);
    }
}

/**
 * Whether a tree holds a widget that shows text the AI wrote.
 *
 * @param list<array<string,mixed>> $nodes
 */
function rt_has_text_widget(array $nodes): bool
{
    foreach ($nodes as $node) {
        if (in_array($node['widgetType'] ?? '', ['heading', 'text-editor', 'button'], true)) {
            return true;
        }
        if (($node['widgetType'] ?? '') === 'image' && (string) ($node['settings']['caption'] ?? '') !== '') {
            return true;
        }
        if (is_array($node['elements'] ?? null) && rt_has_text_widget($node['elements'])) {
            return true;
        }
    }

    return false;
}

function rt_dom(string $html): DOMDocument
{
    $dom  = new DOMDocument();
    $prev = libxml_use_internal_errors(true);
    $dom->loadHTML('<?xml encoding="UTF-8"><body>' . $html . '</body>', LIBXML_NONET);
    libxml_clear_errors();
    libxml_use_internal_errors($prev);

    return $dom;
}

/**
 * Add an unregistered widget next to the first widgets of the tree.
 *
 * @param list<array<string,mixed>> $nodes
 */
function rt_plant_widget(array &$nodes): bool
{
    foreach ($nodes as &$node) {
        $children = $node['elements'] ?? [];
        foreach ($children as $child) {
            if (($child['elType'] ?? '') === 'widget') {
                $node['elements'][] = ['id' => 'fff0001', 'elType' => 'widget', 'settings' => [], 'elements' => [], 'widgetType' => 'wpmgr-not-registered'];

                return true;
            }
        }
        if ($children !== [] && rt_plant_widget($node['elements'])) {
            return true;
        }
    }

    return false;
}

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

$rtArgs   = [];
$rtPlants = [];
foreach (array_slice($argv, 1) as $tok) {
    if (strpos((string) $tok, '=') === false) {
        rt_broken('argument is not key=value: ' . rt_brief((string) $tok));
    }
    [$k, $v] = explode('=', (string) $tok, 2);
    if ($k === 'plant') {
        $parts = explode('@', $v, 2);
        if (count($parts) !== 2 || !in_array($parts[0], RT_PLANTS, true) || $parts[1] === '') {
            rt_broken('plant must be <kind>@<case> with a kind from ' . implode(',', RT_PLANTS) . ': ' . rt_brief($v));
        }
        $rtPlants[$parts[1]][] = $parts[0];
        continue;
    }
    $rtArgs[$k] = $v;
}
foreach (['elementor', 'wp', 'php', 'zip_sha256'] as $need) {
    if (!isset($rtArgs[$need]) || $rtArgs[$need] === '') {
        rt_broken('missing argument ' . $need . '=');
    }
}

// ---------------------------------------------------------------------------
// The site: PHP, WordPress, the pinned Elementor, the agent's own code
// ---------------------------------------------------------------------------

if (PHP_MAJOR_VERSION . '.' . PHP_MINOR_VERSION !== $rtArgs['php']) {
    rt_broken('PHP is ' . PHP_VERSION . ', the pin is ' . $rtArgs['php']);
}

// A CLI request: give WordPress the globals a web request would carry.
$_SERVER['HTTP_HOST']       = $_SERVER['HTTP_HOST'] ?? '127.0.0.1';
$_SERVER['SERVER_NAME']     = $_SERVER['SERVER_NAME'] ?? '127.0.0.1';
$_SERVER['REQUEST_URI']     = $_SERVER['REQUEST_URI'] ?? '/';
$_SERVER['REQUEST_METHOD']  = $_SERVER['REQUEST_METHOD'] ?? 'GET';
$_SERVER['SERVER_PROTOCOL'] = $_SERVER['SERVER_PROTOCOL'] ?? 'HTTP/1.1';

require '/wordpress/wp-load.php';
require_once ABSPATH . 'wp-admin/includes/plugin.php';
require_once ABSPATH . 'wp-admin/includes/image.php';

/** @var list<string> $rtNotices PHP warnings and deprecations seen after boot, kept apart from the verdict. */
$rtNotices = [];
set_error_handler(static function (int $no, string $msg, string $file, int $line) use (&$rtNotices): bool {
    $key = $msg . ' @ ' . basename($file) . ':' . $line;
    if (!in_array($key, $rtNotices, true) && count($rtNotices) < 20) {
        $rtNotices[] = $key;
    }

    return true;
});

if (get_bloginfo('version') !== $rtArgs['wp']) {
    rt_broken('WordPress is ' . get_bloginfo('version') . ', the pin is ' . $rtArgs['wp']);
}
if (!is_plugin_active('elementor/elementor.php') || !defined('ELEMENTOR_VERSION') || ELEMENTOR_VERSION !== $rtArgs['elementor']) {
    rt_broken('Elementor is not active at ' . $rtArgs['elementor'] . ' (found ' . (defined('ELEMENTOR_VERSION') ? ELEMENTOR_VERSION : 'none') . ')');
}
if (!class_exists(Plugin::class) || !isset(Plugin::$instance)) {
    rt_broken('Elementor did not boot');
}
$zipHash = is_file('/rt/zips/elementor.zip') ? hash_file('sha256', '/rt/zips/elementor.zip') : false;
if ($zipHash !== $rtArgs['zip_sha256']) {
    rt_broken('the mounted Elementor zip is not the pinned file (sha256 ' . (is_string($zipHash) ? $zipHash : 'unreadable') . ')');
}
if (!class_exists(DOMDocument::class)) {
    rt_broken('DOMDocument is not available in this PHP');
}

// The agent's own resolver, registered as the plugin's main file registers it.
define('WPMGR_AGENT_DIR', '/rt/agent/');
require_once WPMGR_AGENT_DIR . 'includes/class-autoloader.php';
spl_autoload_register(static function (string $class): void {
    $file = \WPMgr\Agent\Autoloader::resolve($class);
    if ($file !== null) {
        require_once $file;
    }
});
foreach ([ElementorClassicMapper::class, IdSeed::class, PageCreateBuilder::class, ServicePrincipal::class] as $cls) {
    if (!class_exists($cls)) {
        rt_broken('the agent class ' . $cls . ' cannot be loaded by the agent autoloader');
    }
}

// The principal is the agent's own, created by its own code.
$enabled = ServicePrincipal::enable();
if (!$enabled['ok'] || (int) ($enabled['user_id'] ?? 0) < 1) {
    rt_broken('the service principal cannot be created: ' . rt_brief($enabled));
}
$principal = (int) $enabled['user_id'];
wp_set_current_user($principal);
$drift = ServicePrincipal::liveDrift();
if ($drift !== null || current_user_can('unfiltered_html')) {
    rt_broken('the service principal is not the restricted user: ' . ($drift ?? 'it has unfiltered_html'));
}

rt_out(sprintf(
    'site php=%s wp=%s elementor=%s principal=%d unfiltered_html=no containers_experiment=%s',
    PHP_VERSION,
    get_bloginfo('version'),
    ELEMENTOR_VERSION,
    $principal,
    Plugin::$instance->experiments->is_feature_active('container') ? 'active' : 'inactive'
));

// ---------------------------------------------------------------------------
// The cases
// ---------------------------------------------------------------------------

/** @var list<array{label:string,file:string,containers:bool,golden:bool}> $rtSources */
$rtSources = [
    ['label' => 'containers', 'file' => '/rt/fixtures/elementor-classic-containers.json', 'containers' => true, 'golden' => true],
    ['label' => 'sections', 'file' => '/rt/fixtures/elementor-classic-sections.json', 'containers' => false, 'golden' => true],
    ['label' => 'containers', 'file' => '/rt/harness/cases-extra.json', 'containers' => true, 'golden' => false],
    ['label' => 'sections', 'file' => '/rt/harness/cases-extra.json', 'containers' => false, 'golden' => false],
];

/**
 * Read a case file twice: as objects (what the validator takes) and as arrays.
 *
 * @return array{objects: object, arrays: array<string,mixed>}
 */
function rt_load(string $file): array
{
    $raw = is_file($file) ? file_get_contents($file) : false;
    if (!is_string($raw) || $raw === '') {
        rt_broken('case file missing or empty: ' . $file);
    }
    try {
        $objects = json_decode($raw, false, 512, JSON_THROW_ON_ERROR);
        $arrays  = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
    } catch (JsonException $e) {
        rt_broken('case file is not JSON: ' . $file);
    }
    if (!is_object($objects) || !is_array($arrays) || !isset($arrays['cases']) || !is_array($arrays['cases']) || $arrays['cases'] === []) {
        rt_broken('case file holds no cases: ' . $file);
    }

    return ['objects' => $objects, 'arrays' => $arrays];
}

// The media the fixtures name become real attachments with their library alt text.
$rtFixture = rt_load('/rt/fixtures/elementor-classic-containers.json');
$rtMedia   = $rtFixture['arrays']['media'] ?? null;
$rtRequest = $rtFixture['arrays']['request_id'] ?? null;
if (!is_array($rtMedia) || $rtMedia === [] || !is_string($rtRequest) || $rtRequest === '') {
    rt_broken('the fixture declares no media or no request id');
}
$rtMediaFacts = [];
wp_set_current_user(1);
foreach ($rtMedia as $mid => $fact) {
    $mid  = (int) $mid;
    $up   = wp_upload_dir();
    $file = $up['path'] . '/rt-image-' . $mid . '.png';
    $gd   = imagecreatetruecolor(1600, 1000);
    imagefill($gd, 0, 0, (int) imagecolorallocate($gd, 30, 120, 200));
    imagepng($gd, $file);
    imagedestroy($gd);
    $att = wp_insert_attachment(['import_id' => $mid, 'post_mime_type' => 'image/png', 'post_title' => 'rt image ' . $mid, 'post_content' => '', 'post_status' => 'inherit'], $file, 0, true);
    if (is_wp_error($att) || (int) $att !== $mid) {
        rt_broken('attachment ' . $mid . ' cannot be created with that id: ' . (is_wp_error($att) ? $att->get_error_message() : 'got id ' . (int) $att));
    }
    wp_update_attachment_metadata($mid, wp_generate_attachment_metadata($mid, $file));
    update_post_meta($mid, '_wp_attachment_image_alt', (string) ($fact['alt'] ?? ''));
    $rtMediaFacts[$mid] = ['url' => (string) ($fact['url'] ?? ''), 'alt' => (string) ($fact['alt'] ?? '')];
}
wp_set_current_user($principal);

$rtCases     = 0;
$rtChecks    = 0;
$rtFailed    = 0;
$rtPerFile   = [];
$rtTextsSeen = 0;
$rtAltsSeen  = 0;

foreach ($rtSources as $src) {
    $loaded = rt_load($src['file']);
    $label  = $src['label'];
    $listed = 0;
    foreach ($loaded['objects']->cases as $i => $caseObj) {
        $caseArr = $loaded['arrays']['cases'][$i];
        $name    = (string) ($caseArr['name'] ?? '');
        $tag     = sprintf('%s %s %s', ELEMENTOR_VERSION, $label, $name);
        if ($name === '' || !isset($caseObj->input)) {
            rt_broken('a case in ' . $src['file'] . ' has no name or no input');
        }
        ++$rtCases;
        ++$listed;
        $fails   = [];
        $checks  = 0;
        $ck      = static function (string $check, bool $ok, string $why = '') use (&$fails, &$checks): bool {
            ++$checks;
            if (!$ok) {
                $fails[] = $check . ': ' . $why;
            }

            return $ok;
        };
        $plants  = $rtPlants[$name] ?? [];
        $mappedOk = false;
        $tree    = [];

        // 1. build
        $valid = PageCreateBuilder::validate((object) [
            'post_type' => 'page',
            'editor'    => PageCreateBuilder::EDITOR_BLOCKS,
            'title'     => 'Round trip',
            'outline'   => $caseObj->input,
        ]);
        if ($ck('validate', isset($valid['spec']), (string) ($valid['code'] ?? '') . ' ' . (string) ($valid['detail'] ?? ''))) {
            $mapped = ElementorClassicMapper::map($valid['spec'], new IdSeed($rtRequest), $rtMediaFacts, $src['containers']);
            if ($ck('map', isset($mapped['tree']), (string) ($mapped['code'] ?? '') . ' ' . (string) ($mapped['detail'] ?? ''))) {
                $tree     = $mapped['tree'];
                $mappedOk = true;
            }
        }

        if ($mappedOk) {
            // 2. the golden tree
            if (in_array('mapper_drift', $plants, true)) {
                $tree[0]['id'] = 'ddddddd';
            }
            if ($src['golden']) {
                $want = $caseArr['tree'] ?? null;
                $d    = is_array($want) ? rt_diff($want, $tree, 'tree') : 'the fixture case has no tree';
                $ck('golden', $d === null, (string) $d);
            }
            if (in_array('unregistered_widget', $plants, true) && !rt_plant_widget($tree)) {
                $ck('plant', false, 'no widget to plant next to in this case');
            }

            // 3. save as the restricted principal, read back
            $ck('principal', !current_user_can('unfiltered_html') && get_current_user_id() === $principal, 'the saving user is not the restricted principal');
            $pid = wp_insert_post([
                'post_type'    => 'page',
                'post_status'  => 'draft',
                'post_title'   => 'rt ' . $label . ' ' . $name,
                'post_content' => '',
                'post_author'  => $principal,
                'meta_input'   => ['_elementor_edit_mode' => 'builder', '_elementor_template_type' => 'wp-page'],
            ], true);
            if ($ck('draft-insert', is_int($pid) && $pid > 0, is_wp_error($pid) ? $pid->get_error_message() : 'no id')) {
                $saved = false;
                $why   = 'save returned false';
                try {
                    $doc = Plugin::$instance->documents->get($pid, false);
                    if (!is_object($doc)) {
                        $why = 'Elementor has no document for the draft';
                    } else {
                        $saved = $doc->save(['elements' => $tree]);
                    }
                } catch (Throwable $e) {
                    $why = 'save threw ' . get_class($e);
                }
                setlocale(LC_NUMERIC, 'C');
                if ($ck('save', $saved === true, $why)) {
                    global $wpdb;
                    $rows = $wpdb->get_col($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s ORDER BY meta_id", $pid, '_elementor_data'));
                    if ($ck('stored', is_array($rows) && count($rows) === 1, 'expected one _elementor_data row, found ' . (is_array($rows) ? count($rows) : 0))) {
                        $decoded = json_decode((string) $rows[0], true, RT_NODE_LIMIT_DEPTH);
                        $d       = is_array($decoded) ? rt_diff($tree, $decoded, 'tree') : 'the stored value is not a JSON list';
                        $ck('stored', $d === null && $decoded === $tree, (string) $d);
                    }
                    $post = get_post($pid);
                    $ck('draft', $post instanceof WP_Post && $post->post_status === 'draft' && (int) $post->post_author === $principal, 'the page is not a draft owned by the principal');

                    // 4. render
                    $kinds = $plants;
                    $plant = static function (string $html) use ($kinds): string {
                        if (in_array('render_script', $kinds, true)) {
                            $html .= '<script>alert(1)</script>';
                        }
                        if (in_array('render_onclick', $kinds, true)) {
                            $html = (string) preg_replace('/<div /', '<div onclick="x()" ', $html, 1);
                        }
                        if (in_array('render_text', $kinds, true)) {
                            $html = str_replace('&amp;', '&amp;amp;', $html);
                        }

                        return $html;
                    };
                    add_filter('elementor/frontend/the_content', $plant, 99);
                    try {
                        $html = Plugin::$instance->frontend->get_builder_content($pid, false);
                    } catch (Throwable $e) {
                        $html = '';
                        $ck('render', false, 'render threw ' . get_class($e));
                    }
                    remove_filter('elementor/frontend/the_content', $plant, 99);
                    setlocale(LC_NUMERIC, 'C');

                    if ($ck('render-nonempty', is_string($html) && trim($html) !== '', 'the page rendered nothing')) {
                        $ck('render-script', stripos($html, '<script') === false, 'the page holds a script element');
                        $dom  = rt_dom($html);
                        $ck('render-script', $dom->getElementsByTagName('script')->length === 0, 'the parsed page holds a script element');
                        $onAttr = '';
                        foreach ($dom->getElementsByTagName('*') as $el) {
                            foreach ($el->attributes ?? [] as $attr) {
                                if (stripos($attr->name, 'on') === 0) {
                                    $onAttr = $el->nodeName . '[' . $attr->name . ']';
                                    break 2;
                                }
                            }
                        }
                        $ck('render-on', $onAttr === '' && preg_match('/\son[a-z]+\s*=/i', $html) !== 1, 'an on* attribute: ' . $onAttr);

                        $texts = [];
                        rt_texts($caseObj->input, $texts);
                        $page = rt_norm((string) $dom->textContent);
                        $miss = [];
                        foreach ($texts as $t) {
                            if (strpos($page, rt_norm($t)) === false) {
                                $miss[] = rt_brief($t);
                            }
                        }
                        // A page that builds a text widget from an outline the harness found no text in
                        // would otherwise pass this check by looking for nothing.
                        $ck('render-text', ($texts !== [] || !rt_has_text_widget($tree)) && $miss === [], $texts === [] ? 'the page has text widgets but the harness found no text in the outline' : count($miss) . ' text(s) not shown as written, first: ' . ($miss[0] ?? ''));
                        $rtTextsSeen += count($texts);

                        $alts = [];
                        rt_alts($caseObj->input, $alts);
                        $rtAltsSeen += count($alts);
                        $shown = [];
                        foreach ($dom->getElementsByTagName('img') as $img) {
                            if (preg_match('/\bwp-image-(\d+)\b/', (string) $img->getAttribute('class'), $m) === 1) {
                                $shown[(int) $m[1]] = (string) $img->getAttribute('alt');
                            }
                        }
                        foreach ($alts as $att => $alt) {
                            $ck('render-alt', ($shown[$att] ?? null) === $alt, 'image ' . $att . ' shows alt ' . rt_brief($shown[$att] ?? '(no image)') . ', the outline says ' . rt_brief($alt));
                        }
                    }
                }
            }
        }

        $rtChecks += $checks;
        if ($fails === []) {
            rt_out(sprintf('ok   [%s] checks=%d', $tag, $checks));
        } else {
            ++$rtFailed;
            foreach ($fails as $f) {
                rt_out(sprintf('FAIL [%s] %s', $tag, $f));
            }
        }
    }
    $rtPerFile[$src['file'] . ' (' . $label . ')'] = $listed;
}

foreach ($rtNotices as $n) {
    rt_out('note ' . rt_brief($n));
}

foreach ($rtPerFile as $f => $n) {
    if ($n < 1) {
        ++$rtFailed;
        rt_out('FAIL ' . $f . ' ran no cases');
    }
}
if ($rtCases < 1) {
    ++$rtFailed;
    rt_out('FAIL no case ran');
}
// The text and alt checks must have looked at something across the run.
if ($rtTextsSeen < 1 || $rtAltsSeen < 1) {
    ++$rtFailed;
    rt_out(sprintf('FAIL the render checks looked at %d text(s) and %d image alt(s)', $rtTextsSeen, $rtAltsSeen));
}
rt_out(sprintf('SUMMARY elementor=%s cases=%d checks=%d texts=%d alts=%d failed=%d', ELEMENTOR_VERSION, $rtCases, $rtChecks, $rtTextsSeen, $rtAltsSeen, $rtFailed));
rt_out($rtFailed === 0 ? 'RESULT OK' : 'RESULT FAIL');
exit($rtFailed === 0 ? 0 : 1);
