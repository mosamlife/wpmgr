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
 * Then the agent's own create path, on the same site, for every outline of the
 * fixture that names the layout this boot runs and of cases-extra.json. The
 * agent's ability_run command is called as the router calls it once a request
 * is verified (AbilityRunCommand::execute with the digest claim), so the site
 * facts, the sanitiser check, the write scope, the verify and the undo all run
 * on the real Elementor:
 *
 *   5. precheck: the agent builds the page and answers its digests, and the
 *      layout it chose from the site's facts must be the layout this boot runs;
 *   6. write, with the digests the precheck gave: the answer must be "created"
 *      with verify tree_equal, and the database, read here with SQL and not
 *      taken from the answer, must hold one draft of the principal whose
 *      _elementor_data is the previewed tree;
 *   7. revert: the answer must be "reverted", the post must be in the trash,
 *      and a second revert must answer "already_reverted".
 *
 * And the refusals the path must make: a site that rewrites the saved tree
 * (verify must refuse and the draft must be trashed), a write under digests
 * that are not the precheck's (nothing may be created), and a draft a person
 * has edited (undo must refuse and leave it). At the end no post the agent
 * created may be outside the trash.
 *
 * Then the agent's edit path, on the same site, for every edit case of the
 * shared edit fixture and of edit-cases-extra.json that names the layout this
 * boot runs. Each case starts from a draft the agent's page-create path made
 * (so the agent admits it as WPMgr's), the page's tree is the case's, and the
 * agent's ability_run command edits it as the router calls it, with the signed
 * list naming the draft:
 *
 *   8. golden: the shared fixture's operations, applied by the agent's own
 *      LayoutOps under the fixture's request id, give the fixture's tree;
 *   9. precheck, then write under the precheck's digests: the answer must be
 *      "applied", and Elementor's own save (Document::save, as the service
 *      user, which has no unfiltered_html) must have stored exactly the
 *      previewed tree: the whole tree, read here with SQL, equal in value,
 *      type and key order to the tree the precheck planned, so no node the
 *      edit did not touch was dropped or rewritten;
 *  10. render: the edited page must hold every text the operations wrote as
 *      written (an ampersand, an entity-shaped text and a bracket are shown
 *      literally), none of the texts they removed, the links they set, no
 *      script element and no on* attribute.
 *
 * Together the cases must have applied all five operations: set_text, insert,
 * replace, remove and move.
 *
 * And the full restore: a write that fails after the agent has snapshotted the
 * page (a site that rewrites the saved tree) must put the page back. The
 * postmeta rows of the draft, [meta_key, meta_value] in meta_id order, read
 * with SQL before the write and after the failed one, must be the same bytes,
 * and so must the posts columns a snapshot keeps. The rows include values with
 * backslashes, a serialized-looking value, a key with several rows, a NULL, an
 * empty string and a four-byte character, so a restore that goes through the
 * meta API (which unslashes, serializes and renumbers) cannot pass. Rows the
 * agent leaves alone by contract, the edit lock and the derived caches, are
 * left out of both sides. At the end no post the edit path made may be outside
 * the trash.
 *
 * Arguments are key=value tokens (the CLI drops numeric-looking tokens):
 *   elementor=<x.y.z>   the Elementor version this boot must run
 *   wp=<x.y>            the WordPress version this boot must run
 *   php=<x.y>           the PHP version this boot must run
 *   zip_sha256=<hex>    the sha256 the mounted Elementor zip must have
 *   stored=<form>       how this Elementor version stores scalars (pins.txt):
 *                       as_given, or strings (true "1", false and null "")
 *   layout=<layout>     containers or sections: the layout this site runs.
 *                       run.sh sets Elementor's container experiment to match
 *                       before the boot; the harness refuses a site that
 *                       disagrees. A containers site runs every golden source,
 *                       a sections site only the sections ones, because a
 *                       container element is not registered there.
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
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentRestore;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\BuilderPageEdit;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LayoutOps;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Abilities\ServicePrincipal;
use WPMgr\Agent\Commands\AbilityRunCommand;

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
    // The agent path is handed no outline at all (use the case name "all").
    'agent_no_cases',
    // The edit path is handed no case at all (use the case name "all").
    'edit_no_cases',
    // The page an edit case rendered gains a script element.
    'edit_render_script',
    // Every ampersand in the page an edit case rendered is escaped twice.
    'edit_render_text',
    // After an edit case is written, Elementor's stored copy loses one setting of an element the edit did not touch.
    'edit_drops_setting',
    // After the full restore, one preserved postmeta row differs from its bytes before the write (use the case name "restore").
    'restore_row_drift',
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
            return $path . '.' . $k . ': missing';
        }
        foreach (array_diff($gk, $wk) as $k) {
            return $path . '.' . $k . ': unexpected';
        }
        if (array_is_list($want) && count($want) !== count($got)) {
            return $path . ': want ' . count($want) . ' item(s), got ' . count($got);
        }
        foreach ($wk as $k) {
            $d = rt_diff($want[$k], $got[$k], $path . '.' . $k);
            if ($d !== null) {
                return $d;
            }
        }
        if ($wk !== $gk) {
            return $path . ': same keys, different order';
        }

        return null;
    }
    if ($want === $got) {
        return null;
    }

    return $path . ': want ' . rt_brief($want) . ', got ' . rt_brief($got);
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
 * A tree with every scalar as the string a version of Elementor that sanitises
 * every scalar stores: true as "1", false and null as "", numbers as digits.
 *
 * @param array<mixed> $data
 * @return array<mixed>
 */
function rt_scalars_as_strings(array $data): array
{
    $out = [];
    foreach ($data as $key => $value) {
        if (is_array($value)) {
            $value = rt_scalars_as_strings($value);
        } elseif (is_bool($value)) {
            $value = $value ? '1' : '';
        } elseif ($value === null) {
            $value = '';
        } elseif (is_int($value) || is_float($value)) {
            $value = (string) $value;
        }
        $out[$key] = $value;
    }

    return $out;
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
// The agent's own create path
// ---------------------------------------------------------------------------

/** The refusal scenarios the agent phase must have run, by name. A run that ran fewer is red. */
const RT_AGENT_SCENARIOS = ['tamper', 'digest', 'person-edit'];

/** The post meta the agent marks a post it created with; its value is the request id. */
const RT_MARKER = '_wpmgr_created_by_request';

/** The checks of one scenario: what failed, and how many were made. */
final class RtChecks
{
    /** @var list<string> */
    public array $fails = [];

    public int $count = 0;

    public function ck(string $check, bool $ok, string $why = ''): bool
    {
        ++$this->count;
        if (!$ok) {
            $this->fails[] = $check . ': ' . $why;
        }

        return $ok;
    }
}

/**
 * The agent's ability_run command, called as the router calls it once a request
 * has been verified: the claims carry the digest of the exact text of p.
 *
 * @param array<string,mixed> $p The members of p.
 * @return array<string,mixed>
 */
function rt_ability(AbilityRunCommand $cmd, array $p): array
{
    try {
        $text = json_encode($p, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    } catch (JsonException $e) {
        rt_broken('a request to the agent cannot be encoded: ' . $e->getMessage());
    }
    $answer = $cmd->execute(['pd' => hash('sha256', $text)], ['p' => $text]);
    wp_set_current_user(0);

    return $answer;
}

/**
 * The posts a request created, read with SQL by the marker the agent puts on them.
 *
 * @return list<int>
 */
function rt_posts_of(string $requestId): array
{
    global $wpdb;
    $ids = $wpdb->get_col($wpdb->prepare("SELECT post_id FROM {$wpdb->postmeta} WHERE meta_key = %s AND meta_value = %s ORDER BY post_id", RT_MARKER, $requestId));

    return array_map('intval', is_array($ids) ? $ids : []);
}

/** A post's status read with SQL; "(gone)" when there is no such post. */
function rt_status_of(int $postId): string
{
    global $wpdb;
    $status = $wpdb->get_var($wpdb->prepare("SELECT post_status FROM {$wpdb->posts} WHERE ID = %d", $postId));

    return is_string($status) ? $status : '(gone)';
}

/**
 * The _elementor_data a post holds, read with SQL: how many rows there are, and
 * the decoded tree when there is exactly one and it is a list.
 *
 * @return array{0:int,1:?array<mixed>}
 */
function rt_stored_tree(int $postId): array
{
    global $wpdb;
    $rows = $wpdb->get_col($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s ORDER BY meta_id", $postId, '_elementor_data'));
    $rows = is_array($rows) ? $rows : [];
    $tree = count($rows) === 1 ? json_decode((string) $rows[0], true, RT_NODE_LIMIT_DEPTH) : null;

    return [count($rows), is_array($tree) ? $tree : null];
}

/**
 * Run the agent's create path on this site for every outline in the case files,
 * then the refusals it must make. See the steps 5 to 7 in the file header.
 *
 * @param list<string> $files   Case files; each holds outlines in "cases".
 * @param bool         $noCases Plant: hand the agent no outline at all.
 * @return array{cases:int,scenarios:list<string>,checks:int,failed:int}
 */
function rt_agent_phase(string $layout, string $stored, int $principal, array $files, bool $noCases): array
{
    global $wpdb;
    $out      = ['cases' => 0, 'scenarios' => [], 'checks' => 0, 'failed' => 0];
    $tagBase  = ELEMENTOR_VERSION . ' agent-' . $layout;
    $created  = 0;
    $hex      = static fn ($v): bool => is_string($v) && preg_match('/^[0-9a-f]{64}$/D', $v) === 1;
    $report   = static function (string $what, RtChecks $c) use (&$out, $tagBase): void {
        $out['checks'] += $c->count;
        if ($c->fails === []) {
            rt_out(sprintf('ok   [%s %s] checks=%d', $tagBase, $what, $c->count));

            return;
        }
        ++$out['failed'];
        foreach ($c->fails as $f) {
            rt_out(sprintf('FAIL [%s %s] %s', $tagBase, $what, $f));
        }
    };

    // The catalogue entry the control plane sends for wpmgr/page-create.
    $entryText = (string) json_encode([
        'name'          => 'wpmgr/page-create',
        'source'        => 'wpmgr',
        'class'         => 'write',
        'status'        => 'admitted',
        'enabled'       => true,
        'approval_mode' => 'per_call',
        'snapshot'      => 'created_post_trash',
        'limits'        => ['builders_enabled' => ['elementor']],
    ], JSON_UNESCAPED_SLASHES);
    $entrySha = hash('sha256', $entryText);
    $cmd      = new AbilityRunCommand();
    wp_set_current_user(0);
    $call = static fn (string $mode, string $rid, array $more = []): array => rt_ability($cmd, ['mode' => $mode, 'request_id' => $rid, 'entry' => $entryText, 'entry_sha256' => $entrySha] + $more);
    $inputOf = static function (string $name, $outline): string {
        try {
            return json_encode(['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => 'Agent path ' . $name, 'outline' => $outline], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
        } catch (JsonException $e) {
            rt_broken('an outline cannot be encoded: ' . $name);
        }

        return '';
    };
    $digestsOf = static fn (array $pre): array => ['precheck_digest' => (string) ($pre['precheck_digest'] ?? ''), 'preview_digest' => (string) ($pre['preview_digest'] ?? '')];

    // What the agent reads of this site.
    $c       = new RtChecks();
    $facts   = (new ElementorAdapter())->facts();
    $kitId   = Plugin::$instance->kits_manager->get_active_id();
    $c->ck('active', ($facts['active'] ?? null) === true && ($facts['version'] ?? null) === ELEMENTOR_VERSION, 'the agent reads ' . rt_brief($facts));
    $c->ck('in-range', ($facts['classic_in_range'] ?? null) === true, 'the agent finds this version outside the tested range');
    $c->ck('layout', ($facts['containers'] ?? null) === ($layout === 'containers'), 'the agent reads the container layout as ' . rt_brief($facts['containers'] ?? null) . ' on a ' . $layout . ' site');
    $c->ck('principal', ($facts['role_excluded'] ?? null) === false, 'the agent finds its service user excluded from Elementor');
    $c->ck('kit', is_int($facts['active_kit_id'] ?? null) && $facts['active_kit_id'] > 0 && $facts['active_kit_id'] === (int) $kitId, 'the agent reads kit ' . rt_brief($facts['active_kit_id'] ?? null) . ', Elementor says ' . rt_brief($kitId));
    $c->ck('page-type', in_array('page', is_array($facts['post_types'] ?? null) ? $facts['post_types'] : [], true), 'the agent does not find pages enabled in Elementor');
    $report('facts', $c);

    // Every outline: precheck, write under the precheck's digests, read the database, undo.
    $probe = null; // the first outline, kept for the refusals below
    foreach ($noCases ? [] : $files as $file) {
        $loaded = rt_load($file);
        foreach ($loaded['objects']->cases as $i => $caseObj) {
            $name = (string) ($loaded['arrays']['cases'][$i]['name'] ?? '');
            if ($name === '' || !isset($caseObj->input)) {
                rt_broken('a case in ' . $file . ' has no name or no input');
            }
            $probe ??= [$name, $caseObj->input];
            $rid    = wp_generate_uuid4();
            $input  = $inputOf($name, $caseObj->input);
            $c      = new RtChecks();
            $pre    = $call('precheck', $rid, ['input' => $input]);
            if ($c->ck('precheck', ($pre['ok'] ?? false) === true && ($pre['outcome'] ?? null) === 'prechecked', rt_brief($pre))) {
                $preview = is_array($pre['preview'] ?? null) ? $pre['preview'] : [];
                $tree    = $preview['tree'] ?? null;
                $c->ck('layout', ($preview['layout'] ?? null) === $layout, 'the agent built the ' . rt_brief($preview['layout'] ?? '(none)') . ' layout on a ' . $layout . ' site');
                $c->ck('digests', $hex($pre['preview_digest'] ?? null) && $hex($pre['precheck_digest'] ?? null), 'the precheck gave no digests');
                $c->ck('preview-tree', is_array($tree) && $tree !== [], 'the precheck previewed no tree');
                $w = $call('write', $rid, ['input' => $input, 'expected' => $digestsOf($pre)]);
                if ($c->ck('write', ($w['ok'] ?? false) === true && ($w['outcome'] ?? null) === 'created', rt_brief($w))) {
                    ++$created;
                    $pid = (int) ($w['post_id'] ?? 0);
                    $c->ck('write-digest', ($w['preview_digest'] ?? null) === ($pre['preview_digest'] ?? ''), 'the write answers a preview digest that is not the precheck\'s');
                    $c->ck('verify', ($w['verify'] ?? null) === ['tree_equal' => true, 'status' => 'draft'], 'the write answers verify ' . rt_brief($w['verify'] ?? null));

                    // What the database holds, read here and not taken from the answer.
                    $posts = rt_posts_of($rid);
                    $c->ck('one-post', $pid > 0 && $posts === [$pid], 'the posts marked with this request are [' . implode(',', $posts) . '], the answer names ' . $pid);
                    $row = $wpdb->get_row($wpdb->prepare("SELECT post_status, post_author, post_type, post_parent FROM {$wpdb->posts} WHERE ID = %d", $pid), ARRAY_A);
                    $c->ck('draft', is_array($row) && $row['post_status'] === 'draft' && (int) $row['post_author'] === $principal && $row['post_type'] === 'page', 'the post is not a draft page of the principal: ' . rt_brief($row));
                    $mode = $wpdb->get_col($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s", $pid, '_elementor_edit_mode'));
                    $c->ck('edit-mode', $mode === ['builder'], 'the edit mode rows are ' . rt_brief($mode));
                    [$rows, $decoded] = rt_stored_tree($pid);
                    $expect = $stored === 'strings' && is_array($tree) ? rt_scalars_as_strings($tree) : $tree;
                    $d      = $decoded !== null && is_array($expect) ? rt_diff($expect, $decoded, 'tree') : 'expected one _elementor_data row holding a tree, found ' . $rows;
                    $c->ck('stored-tree', $d === null, (string) $d);

                    // Undo.
                    $r = $call('revert', $rid, ['input' => '{}']);
                    $c->ck('revert', ($r['ok'] ?? false) === true && ($r['outcome'] ?? null) === 'reverted' && (int) ($r['post_id'] ?? 0) === $pid && ($r['trashed'] ?? null) === true, rt_brief($r));
                    $c->ck('trashed', rt_status_of($pid) === 'trash', 'after the revert the post is ' . rt_status_of($pid));
                    $again = $call('revert', $rid, ['input' => '{}']);
                    $c->ck('revert-twice', ($again['ok'] ?? false) === true && ($again['outcome'] ?? null) === 'already_reverted', rt_brief($again));
                }
            }
            ++$out['cases'];
            $report($name, $c);
        }
    }

    // The refusals. Each needs an outline to work on: the first one.
    if ($probe !== null) {
        [$name, $outline] = $probe;
        $input = $inputOf($name, $outline);

        // 1. A site that rewrites what is stored: the verify must refuse and the draft must be trashed.
        $c   = new RtChecks();
        $rid = wp_generate_uuid4();
        $pre = $call('precheck', $rid, ['input' => $input]);
        if ($c->ck('precheck', ($pre['ok'] ?? false) === true, rt_brief($pre))) {
            $fired  = 0;
            $rewrite = static function ($value) use (&$fired) {
                if (!is_string($value)) {
                    return $value;
                }
                $changed = preg_replace('/"id":"/', '"id":"z', $value, 1, $n);
                if (!is_string($changed) || $n !== 1) {
                    return $value;
                }
                ++$fired;

                return $changed;
            };
            add_filter('sanitize_post_meta__elementor_data', $rewrite, 10, 1);
            try {
                $w = $call('write', $rid, ['input' => $input, 'expected' => $digestsOf($pre)]);
            } finally {
                remove_filter('sanitize_post_meta__elementor_data', $rewrite, 10);
            }
            $c->ck('armed', $fired >= 1, 'the rewrite never ran, so this scenario proved nothing');
            $c->ck('refused', ($w['ok'] ?? true) === false && ($w['code'] ?? null) === 'verify_mismatch', 'a write whose stored tree differs from the built one was answered ' . rt_brief($w));
            $posts = rt_posts_of($rid);
            $created += count($posts);
            $c->ck('trashed', count($posts) === 1 && rt_status_of($posts[0]) === 'trash', 'the draft of the refused write is not in the trash: ' . implode(',', array_map(static fn (int $p): string => $p . '=' . rt_status_of($p), $posts)));
            $led = $call('ledger', $rid);
            $c->ck('ledger', ($led['found'] ?? false) === true && ($led['phase'] ?? null) === 'failed', 'the ledger says ' . rt_brief($led));
        }
        $out['scenarios'][] = 'tamper';
        $report('tamper', $c);

        // 2. Digests that are not the precheck's: nothing may be created.
        $c    = new RtChecks();
        $rid  = wp_generate_uuid4();
        $pre  = $call('precheck', $rid, ['input' => $input]);
        if ($c->ck('precheck', ($pre['ok'] ?? false) === true, rt_brief($pre))) {
            $wrong = str_repeat('0', 64);
            foreach (['preview_digest', 'precheck_digest'] as $which) {
                $w = $call('write', $rid, ['input' => $input, 'expected' => [$which => $wrong] + $digestsOf($pre)]);
                $c->ck('refused-' . $which, ($w['ok'] ?? true) === false && ($w['code'] ?? null) === 'preview_changed', 'a write under a wrong ' . $which . ' was answered ' . rt_brief($w));
            }
            $made = rt_posts_of($rid);
            $created += count($made);
            $c->ck('nothing-created', $made === [], 'a write under wrong digests created post(s) [' . implode(',', $made) . ']');
            $led = $call('ledger', $rid);
            $c->ck('no-ledger', ($led['found'] ?? true) === false, 'the ledger holds a row for a write that was refused: ' . rt_brief($led));
        }
        $out['scenarios'][] = 'digest';
        $report('digest', $c);

        // 3. A draft a person has edited: the undo must refuse and leave it.
        $c    = new RtChecks();
        $rid  = wp_generate_uuid4();
        $pre  = $call('precheck', $rid, ['input' => $input]);
        $w    = ($pre['ok'] ?? false) === true ? $call('write', $rid, ['input' => $input, 'expected' => $digestsOf($pre)]) : [];
        if ($c->ck('write', ($w['ok'] ?? false) === true && ($w['outcome'] ?? null) === 'created', rt_brief($w !== [] ? $w : $pre))) {
            ++$created;
            $pid    = (int) ($w['post_id'] ?? 0);
            $edited = is_array($pre['preview']['tree'] ?? null) ? $pre['preview']['tree'] : [];
            if ($edited !== []) {
                $edited[0]['id'] = 'e' . substr((string) ($edited[0]['id'] ?? 'xxxxxxx'), 1);
            }
            wp_set_current_user(1);
            $doc   = Plugin::$instance->documents->get($pid, false);
            $saved = is_object($doc) && $edited !== [] ? $doc->save(['elements' => $edited]) : false;
            setlocale(LC_NUMERIC, 'C');
            wp_set_current_user(0);
            $c->ck('edit-saved', $saved === true, 'Elementor did not save the person\'s edit, so this scenario proved nothing');
            $r = $call('revert', $rid, ['input' => '{}']);
            $c->ck('refused', ($r['ok'] ?? true) === false && ($r['code'] ?? null) === 'created_post_touched', 'an undo of a draft a person edited was answered ' . rt_brief($r));
            $c->ck('kept', rt_status_of($pid) === 'draft', 'after the refused undo the post is ' . rt_status_of($pid));
            wp_set_current_user(1);
            wp_trash_post($pid);
            wp_set_current_user(0);
        }
        $out['scenarios'][] = 'person-edit';
        $report('person-edit', $c);
    }

    // Nothing the agent made may be left outside the trash, and the agent made exactly what this run asked for.
    $c    = new RtChecks();
    $left = $wpdb->get_col($wpdb->prepare("SELECT p.ID FROM {$wpdb->posts} p INNER JOIN {$wpdb->postmeta} m ON m.post_id = p.ID AND m.meta_key = %s WHERE p.post_status <> 'trash' ORDER BY p.ID", RT_MARKER));
    $all  = (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM {$wpdb->postmeta} WHERE meta_key = %s", RT_MARKER));
    $c->ck('left-behind', is_array($left) && $left === [], 'post(s) the agent created are not in the trash: [' . implode(',', is_array($left) ? $left : []) . ']');
    $c->ck('created', $all === $created, 'the database holds ' . $all . ' post(s) the agent created, the run created ' . $created);
    $report('leftover', $c);

    return $out;
}

// ---------------------------------------------------------------------------
// The agent's edit path
// ---------------------------------------------------------------------------

/** The scenarios the edit phase must have run besides its cases, by name. A run that ran fewer is red. */
const RT_EDIT_SCENARIOS = ['restore'];

/** The title of every draft the edit phase makes. */
const RT_EDIT_TITLE = 'Edit path draft';

/**
 * The catalogue entry the control plane sends for one of WPMgr's own abilities.
 *
 * @return array{0:string,1:string} The entry's exact text and its sha256.
 */
function rt_entry(string $name, string $snapshot): array
{
    $text = (string) json_encode([
        'name'          => $name,
        'source'        => 'wpmgr',
        'class'         => 'write',
        'status'        => 'admitted',
        'enabled'       => true,
        'approval_mode' => 'per_call',
        'snapshot'      => $snapshot,
        'limits'        => ['builders_enabled' => ['elementor']],
    ], JSON_UNESCAPED_SLASHES);

    return [$text, hash('sha256', $text)];
}

/**
 * One call of the agent's command under an entry.
 *
 * @param array{0:string,1:string} $entry See rt_entry().
 * @param array<string,mixed>      $more  The other members of p.
 * @return array<string,mixed>
 */
function rt_call(AbilityRunCommand $cmd, array $entry, string $mode, string $rid, array $more = []): array
{
    return rt_ability($cmd, ['mode' => $mode, 'request_id' => $rid, 'entry' => $entry[0], 'entry_sha256' => $entry[1]] + $more);
}

/**
 * The two digests a write must carry, from a precheck's answer.
 *
 * @param array<string,mixed> $pre A precheck answer.
 * @return array{precheck_digest:string,preview_digest:string}
 */
function rt_digests(array $pre): array
{
    return ['precheck_digest' => (string) ($pre['precheck_digest'] ?? ''), 'preview_digest' => (string) ($pre['preview_digest'] ?? '')];
}

/**
 * A draft made by the agent's own page-create path, so the agent admits it as
 * WPMgr's: precheck, then write under the precheck's digests.
 *
 * @param array{0:string,1:string} $entry   The page-create entry.
 * @param array<mixed>             $outline The outline, as the AI sends it.
 * @return array{0:int,1:string} The post id (0 when it could not be made) and, then, why not.
 */
function rt_make_draft(AbilityRunCommand $cmd, array $entry, array $outline): array
{
    $rid = wp_generate_uuid4();
    try {
        $input = json_encode(['post_type' => 'page', 'editor' => 'builder:elementor', 'title' => RT_EDIT_TITLE, 'outline' => $outline], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    } catch (JsonException $e) {
        return [0, 'the seed outline cannot be encoded'];
    }
    $pre = rt_call($cmd, $entry, 'precheck', $rid, ['input' => $input]);
    if (($pre['ok'] ?? false) !== true) {
        return [0, 'the seed precheck was answered ' . rt_brief($pre)];
    }
    $w = rt_call($cmd, $entry, 'write', $rid, ['input' => $input, 'expected' => rt_digests($pre)]);
    $id = (int) ($w['post_id'] ?? 0);
    if (($w['ok'] ?? false) !== true || ($w['outcome'] ?? null) !== 'created' || $id < 1) {
        return [0, 'the seed write was answered ' . rt_brief($w)];
    }

    return [$id, ''];
}

/**
 * Save an element tree on a post with Elementor's own document save, as the
 * service user, which has no unfiltered_html.
 *
 * @param array<mixed> $tree Elements.
 */
function rt_save_tree(int $postId, array $tree, int $principal): bool
{
    wp_set_current_user($principal);
    $saved = false;
    try {
        $doc   = Plugin::$instance->documents->get($postId, false);
        $saved = is_object($doc) && $doc->save(['elements' => $tree]) === true;
    } catch (Throwable $e) {
        $saved = false;
    }
    setlocale(LC_NUMERIC, 'C');
    wp_set_current_user(0);

    return $saved;
}

/** Put a draft in the trash, as a person would. */
function rt_trash(int $postId): void
{
    wp_set_current_user(1);
    wp_trash_post($postId);
    wp_set_current_user(0);
}

/**
 * The operations with every reference written as @kind#n (the n-th node of
 * that kind on the page, in page order) replaced by the node's real reference.
 *
 * @param array<mixed> $ops  Operations, as the AI sends them.
 * @param array<mixed> $tree The page's stored tree.
 * @return array{0:?array<mixed>,1:string} The operations, or null and why not.
 */
function rt_resolve_refs(array $ops, array $tree): array
{
    $asks = false;
    foreach ($ops as $op) {
        foreach (['ref', 'after', 'before', 'into'] as $member) {
            $asks = $asks || (is_string($op[$member] ?? null) && strpos($op[$member], '@') === 0);
        }
    }
    if (!$asks) {
        return [$ops, ''];
    }
    try {
        $nodes = ElementorClassicMapper::project($tree)->nodes();
    } catch (Throwable $e) {
        return [null, 'the page cannot be projected: ' . get_class($e)];
    }
    $byKind = [];
    foreach ($nodes as $node) {
        $byKind[(string) $node['kind']][] = (string) $node['ref'];
    }
    foreach ($ops as $i => $op) {
        foreach (['ref', 'after', 'before', 'into'] as $member) {
            $v = $op[$member] ?? null;
            if (!is_string($v) || strpos($v, '@') !== 0) {
                continue;
            }
            $parts = explode('#', substr($v, 1), 2);
            $found = count($parts) === 2 ? ($byKind[$parts[0]][(int) $parts[1]] ?? null) : null;
            if ($found === null) {
                return [null, 'operations[' . $i . '].' . $member . ' names ' . $v . ', the page has: ' . implode(', ', array_map(static fn (string $k): string => $k . 'x' . count($byKind[$k]), array_keys($byKind)))];
            }
            $ops[$i][$member] = $found;
        }
    }

    return [$ops, ''];
}

/**
 * The strings under the keys text and caption of what an operation replaced,
 * from a change in a precheck's preview.
 *
 * @param array<mixed> $change One entry of the preview's changes.
 * @return list<string>
 */
function rt_replaced_texts(array $change): array
{
    $op = $change['op'] ?? '';
    if (!in_array($op, ['set_text', 'replace', 'remove'], true) || !is_array($change['before'] ?? null)) {
        return [];
    }
    $out = [];
    foreach (['text', 'caption'] as $member) {
        $v = $change['before'][$member] ?? null;
        if (is_string($v) && $v !== '') {
            $out[] = $v;
        }
    }

    return $out;
}

/**
 * The links an operation sets: the url of a set_text on a url field, and the
 * links of the buttons an outline makes. Only absolute https links are held
 * to the page, since a relative one is shown as the site resolves it.
 *
 * @param mixed        $node An operation or an outline node, as objects.
 * @param list<string> $out  Gets the links.
 */
function rt_links($node, array &$out): void
{
    if (is_array($node)) {
        foreach ($node as $child) {
            rt_links($child, $out);
        }

        return;
    }
    if (!is_object($node)) {
        return;
    }
    if (($node->op ?? '') === 'set_text' && ($node->field ?? '') === 'url' && is_string($node->text ?? null)) {
        $out[] = $node->text;
    }
    if (($node->type ?? '') === 'buttons') {
        foreach ((array) ($node->buttons ?? []) as $b) {
            if (is_object($b) && is_string($b->url ?? null)) {
                $out[] = $b->url;
            }
        }
    }
    rt_links($node->outline ?? [], $out);
    foreach ((array) ($node->columns ?? []) as $col) {
        rt_links($col->children ?? [], $out);
    }
    rt_links($node->children ?? [], $out);
}

/**
 * Render a page with Elementor's frontend and hold it to what an edit wrote.
 *
 * @param list<string> $texts Texts the page must show as written.
 * @param list<string> $gone  Texts the page must no longer show.
 * @param list<string> $links Absolute links an anchor of the page must carry.
 * @param list<string> $kinds The plants of the case.
 */
function rt_render_checks(RtChecks $c, int $postId, array $texts, array $gone, array $links, array $kinds): void
{
    $plant = static function (string $html) use ($kinds): string {
        if (in_array('edit_render_script', $kinds, true)) {
            $html .= '<script>alert(1)</script>';
        }
        if (in_array('edit_render_text', $kinds, true)) {
            $html = str_replace('&amp;', '&amp;amp;', $html);
        }

        return $html;
    };
    add_filter('elementor/frontend/the_content', $plant, 99);
    try {
        $html = Plugin::$instance->frontend->get_builder_content($postId, false);
    } catch (Throwable $e) {
        $html = '';
        $c->ck('render', false, 'render threw ' . get_class($e));
    }
    remove_filter('elementor/frontend/the_content', $plant, 99);
    setlocale(LC_NUMERIC, 'C');

    if (!$c->ck('render-nonempty', is_string($html) && trim($html) !== '', 'the edited page rendered nothing')) {
        return;
    }
    $dom = rt_dom($html);
    $c->ck('render-script', stripos($html, '<script') === false && $dom->getElementsByTagName('script')->length === 0, 'the edited page holds a script element');
    $onAttr = '';
    foreach ($dom->getElementsByTagName('*') as $el) {
        foreach ($el->attributes ?? [] as $attr) {
            if (stripos($attr->name, 'on') === 0) {
                $onAttr = $el->nodeName . '[' . $attr->name . ']';
                break 2;
            }
        }
    }
    $c->ck('render-on', $onAttr === '' && preg_match('/\son[a-z]+\s*=/i', $html) !== 1, 'an on* attribute: ' . $onAttr);

    $page = rt_norm((string) $dom->textContent);
    $miss = [];
    foreach ($texts as $t) {
        if (strpos($page, rt_norm($t)) === false) {
            $miss[] = rt_brief($t);
        }
    }
    // An edit that wrote text but handed the page nothing to look for would otherwise pass by looking for nothing.
    $c->ck('render-text', $texts !== [] && $miss === [], $texts === [] ? 'the harness found no text the operations wrote' : count($miss) . ' text(s) not shown as written, first: ' . ($miss[0] ?? ''));
    $still = [];
    foreach ($gone as $t) {
        if (strpos($page, rt_norm($t)) !== false) {
            $still[] = rt_brief($t);
        }
    }
    $c->ck('render-gone', $still === [], count($still) . ' replaced or removed text(s) still shown, first: ' . ($still[0] ?? ''));
    $hrefs = [];
    foreach ($dom->getElementsByTagName('a') as $a) {
        $hrefs[] = (string) $a->getAttribute('href');
    }
    foreach ($links as $link) {
        if (strpos($link, 'https://') === 0) {
            $c->ck('render-link', in_array($link, $hrefs, true), 'no anchor carries the link ' . rt_brief($link));
        }
    }
}

/**
 * The post meta rows of a post read with SQL, [meta_key, meta_value] in meta_id
 * order, without the rows the agent leaves alone by contract: the edit lock and
 * the derived caches.
 *
 * @param list<string> $skip Meta keys left out.
 * @return list<array{0:string,1:?string}>
 */
function rt_raw_rows(int $postId, array $skip): array
{
    global $wpdb;
    $found = $wpdb->get_results($wpdb->prepare("SELECT meta_key, meta_value FROM {$wpdb->postmeta} WHERE post_id = %d ORDER BY meta_id ASC", $postId), ARRAY_A);
    if (!is_array($found) || (string) $wpdb->last_error !== '') {
        rt_broken('the postmeta rows of post ' . $postId . ' cannot be read');
    }
    $rows = [];
    foreach ($found as $row) {
        $key   = (string) $row['meta_key'];
        $value = $row['meta_value'] ?? null;
        if (in_array($key, $skip, true)) {
            continue;
        }
        $rows[] = [$key, $value === null ? null : (string) $value];
    }

    return $rows;
}

/**
 * The posts columns a snapshot keeps, read with SQL.
 *
 * @return array<string,string>
 */
function rt_raw_post(int $postId): array
{
    global $wpdb;
    $row = $wpdb->get_row($wpdb->prepare("SELECT " . implode(', ', BuilderDocumentSnapshot::RESTORE_POST_COLUMNS) . " FROM {$wpdb->posts} WHERE ID = %d", $postId), ARRAY_A);
    if (!is_array($row)) {
        rt_broken('the posts row of post ' . $postId . ' cannot be read');
    }

    return array_map('strval', $row);
}

/**
 * The first place two row lists differ, or null when they are the same bytes
 * in the same order.
 *
 * @param list<array{0:string,1:?string}> $want
 * @param list<array{0:string,1:?string}> $got
 */
function rt_rows_diff(array $want, array $got): ?string
{
    $n = max(count($want), count($got));
    for ($i = 0; $i < $n; ++$i) {
        if (!isset($want[$i])) {
            return 'row ' . $i . ' is extra: ' . $got[$i][0] . ' (' . ($got[$i][1] === null ? 'NULL' : strlen($got[$i][1]) . ' bytes') . ')';
        }
        if (!isset($got[$i])) {
            return 'row ' . $i . ' is missing: ' . $want[$i][0] . ' (' . ($want[$i][1] === null ? 'NULL' : strlen($want[$i][1]) . ' bytes') . ')';
        }
        if ($want[$i] !== $got[$i]) {
            $w = $want[$i][1] === null ? 'NULL' : strlen($want[$i][1]) . ' bytes ' . substr(hash('sha256', $want[$i][1]), 0, 12);
            $g = $got[$i][1] === null ? 'NULL' : strlen($got[$i][1]) . ' bytes ' . substr(hash('sha256', $got[$i][1]), 0, 12);

            return 'row ' . $i . ' differs: was ' . $want[$i][0] . ' (' . $w . '), is ' . $got[$i][0] . ' (' . $g . ')';
        }
    }

    return null;
}

/**
 * Drop one setting of the first element that has any, from the page's stored
 * tree, with SQL: what Elementor losing a setting of an untouched element looks
 * like. Plant only.
 */
function rt_plant_drop_setting(int $postId): bool
{
    global $wpdb;
    $raw  = $wpdb->get_var($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s ORDER BY meta_id LIMIT 1", $postId, '_elementor_data'));
    $tree = is_string($raw) ? json_decode($raw, true, RT_NODE_LIMIT_DEPTH) : null;
    if (!is_array($tree)) {
        return false;
    }
    $drop = static function (array &$nodes) use (&$drop): bool {
        foreach ($nodes as &$node) {
            if (is_array($node['settings'] ?? null) && $node['settings'] !== []) {
                array_pop($node['settings']);

                return true;
            }
            if (is_array($node['elements'] ?? null) && $drop($node['elements'])) {
                return true;
            }
        }

        return false;
    };
    if (!$drop($tree)) {
        return false;
    }

    return $wpdb->update($wpdb->postmeta, ['meta_value' => wp_json_encode($tree)], ['post_id' => $postId, 'meta_key' => '_elementor_data'], ['%s'], ['%d', '%s']) !== false;
}

/**
 * Run the agent's edit path on this site: every edit case that names the layout
 * of this boot, then the full restore. See the steps 8 to 10 and the restore in
 * the file header.
 *
 * @param array<string,list<string>> $plants  Plant kinds by case name.
 * @param bool                       $noCases Plant: hand the edit path no case at all.
 * @return array{cases:int,scenarios:list<string>,ops:list<string>,checks:int,failed:int}
 */
function rt_edit_phase(string $layout, string $stored, int $principal, array $plants, bool $noCases): array
{
    global $wpdb;
    $out     = ['cases' => 0, 'scenarios' => [], 'ops' => [], 'checks' => 0, 'failed' => 0];
    $tagBase = ELEMENTOR_VERSION . ' edit-' . $layout;
    $made    = [];
    $report  = static function (string $what, RtChecks $c) use (&$out, $tagBase): void {
        $out['checks'] += $c->count;
        if ($c->fails === []) {
            rt_out(sprintf('ok   [%s %s] checks=%d', $tagBase, $what, $c->count));

            return;
        }
        ++$out['failed'];
        foreach ($c->fails as $f) {
            rt_out(sprintf('FAIL [%s %s] %s', $tagBase, $what, $f));
        }
    };

    $createEntry = rt_entry(OwnAbilities::NAME_PAGE_CREATE, 'created_post_trash');
    $editEntry   = rt_entry(OwnAbilities::NAME_PAGE_EDIT, BuilderPageEdit::SNAPSHOT);
    $cmd         = new AbilityRunCommand();
    wp_set_current_user(0);
    $containers = $layout === 'containers';
    $dirOps     = static function (array $ops): array {
        try {
            $parsed = PageEditValidator::parse((string) json_encode(['post_id' => 1, 'base_fingerprint' => str_repeat('0', 64), 'operations' => $ops], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
        } catch (Throwable $e) {
            return [];
        }

        return is_array($parsed['input']['operations'] ?? null) ? $parsed['input']['operations'] : [];
    };

    // The cases of this layout: the shared fixture's, and the extra file's.
    $specs   = [];
    $restore = null;
    $media   = [];
    foreach (['/rt/fixtures/elementor-edit-cases.json', '/rt/harness/edit-cases-extra.json'] as $file) {
        $loaded = rt_load($file);
        $shared = strpos($file, '/rt/fixtures/') === 0;
        if ($shared) {
            $media = is_array($loaded['arrays']['media'] ?? null) ? $loaded['arrays']['media'] : [];
        } else {
            $restore = is_array($loaded['arrays']['restore'] ?? null) ? $loaded['arrays']['restore'] : null;
        }
        foreach ($loaded['arrays']['cases'] as $case) {
            $name = (string) ($case['name'] ?? '');
            if ($name === '' || !is_array($case['ops'] ?? null) || ($shared && (!is_array($case['before_tree'] ?? null) || !is_array($case['after_tree'] ?? null))) || (!$shared && !is_array($case['seed'] ?? null))) {
                rt_broken('an edit case in ' . $file . ' has no name, no operations or no page');
            }
            if ($shared && ($case['layout'] ?? null) !== $layout) {
                continue;
            }
            $specs[] = [
                'name'   => (string) preg_replace('/-(containers|sections)$/D', '', $name),
                'seed'   => $shared ? null : $case['seed'],
                'before' => $shared ? $case['before_tree'] : null,
                'ops'    => $case['ops'],
                'golden' => $shared ? ['request_id' => (string) ($case['request_id'] ?? ''), 'after' => $case['after_tree']] : null,
            ];
        }
    }

    foreach ($noCases ? [] : $specs as $spec) {
        $name = $spec['name'];
        $c    = new RtChecks();
        $kind = $plants[$name] ?? [];
        ++$out['cases'];

        // The draft: made by the agent's page-create path, so the agent admits it as WPMgr's.
        [$pid, $why] = rt_make_draft($cmd, $createEntry, $spec['seed'] ?? [['type' => 'paragraph', 'text' => 'Seed']]);
        if (!$c->ck('seed', $pid > 0, $why)) {
            $report($name, $c);
            continue;
        }
        $made[] = $pid;
        if ($spec['before'] !== null) {
            $c->ck('before-saved', rt_save_tree($pid, $spec['before'], $principal), 'Elementor did not save the case\'s page');
        }
        [$rows, $decoded] = rt_stored_tree($pid);
        $before = $decoded ?? [];
        if ($spec['before'] !== null) {
            $want = $stored === 'strings' ? rt_scalars_as_strings($spec['before']) : $spec['before'];
            $d    = $decoded !== null ? rt_diff($want, $decoded, 'tree') : 'expected one _elementor_data row holding a tree, found ' . $rows;
            $c->ck('before-stored', $d === null, (string) $d);
        }
        [$ops, $refWhy] = rt_resolve_refs($spec['ops'], $before);
        if (!$c->ck('refs', $ops !== null, $refWhy)) {
            $report($name, $c);
            rt_trash($pid);
            continue;
        }

        // 8. The operations as the agent's own LayoutOps applies them, under the fixture's request id.
        if (is_array($spec['golden'])) {
            $planned = LayoutOps::apply($spec['before'], $dirOps($ops), new IdSeed($spec['golden']['request_id']), $media, $containers);
            $d       = isset($planned['tree']) ? rt_diff($spec['golden']['after'], $planned['tree'], 'tree') : 'LayoutOps refused: ' . rt_brief($planned);
            $c->ck('golden', $d === null, (string) $d);
        }

        // 9. Precheck, then write under the precheck's digests, as the router calls them.
        $fp = BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS);
        try {
            $input = json_encode(['post_id' => $pid, 'base_fingerprint' => (string) $fp, 'operations' => $ops], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
        } catch (JsonException $e) {
            rt_broken('the operations of ' . $name . ' cannot be encoded');
        }
        $rid  = wp_generate_uuid4();
        $more = ['input' => $input, 'allowed_draft_ids' => [$pid]];
        $pre  = rt_call($cmd, $editEntry, 'precheck', $rid, $more);
        if ($c->ck('precheck', ($pre['ok'] ?? false) === true && ($pre['outcome'] ?? null) === 'prechecked', rt_brief($pre))) {
            $preview = is_array($pre['preview'] ?? null) ? $pre['preview'] : [];
            $tree    = $preview['tree'] ?? null;
            $changes = is_array($preview['changes'] ?? null) ? $preview['changes'] : [];
            $c->ck('base-fingerprint', ($pre['base_fingerprint'] ?? null) === $fp, 'the precheck read another page than the one the input was made from');
            $c->ck('changes', count($changes) === count($ops), 'the preview lists ' . count($changes) . ' change(s) for ' . count($ops) . ' operation(s)');
            $c->ck('digests', preg_match('/^[0-9a-f]{64}$/D', (string) ($pre['preview_digest'] ?? '')) === 1 && preg_match('/^[0-9a-f]{64}$/D', (string) ($pre['precheck_digest'] ?? '')) === 1, 'the precheck gave no digests');
            $c->ck('preview-tree', is_array($tree) && $tree !== [], 'the precheck previewed no tree');
            $w = rt_call($cmd, $editEntry, 'write', $rid, $more + ['expected' => rt_digests($pre)]);
            if ($c->ck('write', ($w['ok'] ?? false) === true && ($w['outcome'] ?? null) === 'applied', rt_brief($w))) {
                foreach ($ops as $op) {
                    $out['ops'][] = (string) ($op['op'] ?? '');
                }
                if (in_array('edit_drops_setting', $kind, true) && !rt_plant_drop_setting($pid)) {
                    $c->ck('plant', false, 'no setting to drop in this case');
                }
                $c->ck('write-digest', ($w['preview_digest'] ?? null) === ($pre['preview_digest'] ?? ''), 'the write answers a preview digest that is not the precheck\'s');
                $c->ck('changes-applied', ($w['changes_applied'] ?? null) === count($ops), 'the write applied ' . rt_brief($w['changes_applied'] ?? null) . ' change(s) of ' . count($ops));
                $c->ck('before-fp', ($w['before_fp'] ?? null) === $fp, 'the write answers a fingerprint before the edit that is not the page\'s');
                $now = BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS);
                $c->ck('after-fp', ($w['after_fp'] ?? null) === $now && $now !== $fp, 'the write answers a fingerprint after the edit that is not the page\'s now');

                // What the database holds, read here and not taken from the answer: the whole tree.
                [$rows, $decoded] = rt_stored_tree($pid);
                $expect           = $stored === 'strings' && is_array($tree) ? rt_scalars_as_strings($tree) : $tree;
                $d                = $decoded !== null && is_array($expect) ? rt_diff($expect, $decoded, 'tree') : 'expected one _elementor_data row holding a tree, found ' . $rows;
                $c->ck('stored-tree', $d === null, (string) $d);
                $row = $wpdb->get_row($wpdb->prepare("SELECT post_status, post_author, post_type FROM {$wpdb->posts} WHERE ID = %d", $pid), ARRAY_A);
                $c->ck('draft', is_array($row) && $row['post_status'] === 'draft' && (int) $row['post_author'] === $principal && $row['post_type'] === 'page', 'the page is not a draft page of the principal: ' . rt_brief($row));
                $mode = $wpdb->get_col($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s", $pid, '_elementor_edit_mode'));
                $c->ck('edit-mode', $mode === ['builder'], 'the edit mode rows are ' . rt_brief($mode));

                // 10. Render the edited page.
                $texts = [];
                $links = [];
                $objs  = json_decode((string) json_encode($ops, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES), false);
                foreach (is_array($objs) ? $objs : [] as $op) {
                    if (($op->op ?? '') === 'set_text' && in_array($op->field ?? '', ['text', 'caption'], true) && is_string($op->text ?? null)) {
                        $texts[] = $op->text;
                    }
                    rt_texts($op->outline ?? [], $texts);
                }
                rt_links($objs, $links);
                $gone = [];
                foreach ($changes as $change) {
                    if (is_array($change)) {
                        array_push($gone, ...rt_replaced_texts($change));
                    }
                }
                rt_render_checks($c, $pid, $texts, array_values(array_diff($gone, $texts)), $links, $kind);
            }
        }
        $report($name, $c);
        rt_trash($pid);
    }

    // The full restore.
    if (!$noCases && is_array($restore)) {
        $name = (string) ($restore['name'] ?? 'restore');
        $c    = new RtChecks();
        $kind = $plants[$name] ?? [];
        [$pid, $why] = is_array($restore['seed'] ?? null) && is_array($restore['ops'] ?? null) ? rt_make_draft($cmd, $createEntry, $restore['seed']) : [0, 'the restore case has no seed or no operations'];
        if ($c->ck('seed', $pid > 0, $why)) {
            $made[] = $pid;
            // Rows another plugin might keep on the page, as bytes the meta API would change.
            $extra = [
                ['_rt_backslashes', 'C:\\path\\"q"\\u00e9 \\/ \\\\ end'],
                ['_rt_serialized', 'a:2:{s:1:"k";s:3:"a\\b";s:1:"q";s:5:"\\"x\\"";}'],
                ['_rt_multi', 'one'],
                ['_rt_multi', 'two\\three'],
                ['_rt_null', null],
                ['_rt_empty', ''],
                ['_rt_wide', "caf\u{00E9} \u{1F600} \u{65E5}\u{672C}"],
            ];
            $seeded = 0;
            foreach ($extra as [$k, $v]) {
                $seeded += $wpdb->insert($wpdb->postmeta, ['post_id' => $pid, 'meta_key' => $k, 'meta_value' => $v], ['%d', '%s', '%s']) === 1 ? 1 : 0;
            }
            $c->ck('rows-seeded', $seeded === count($extra), 'only ' . $seeded . ' of ' . count($extra) . ' extra rows could be stored');

            [$ops, $refWhy] = rt_resolve_refs($restore['ops'], rt_stored_tree($pid)[1] ?? []);
            if ($c->ck('refs', $ops !== null, $refWhy)) {
                $skip      = array_merge([BuilderDocumentSnapshot::EDIT_LOCK_KEY], (new ElementorAdapter())->descriptor()->derivedKeys);
                $rowsWas   = rt_raw_rows($pid, $skip);
                $postWas   = rt_raw_post($pid);
                $fp        = BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS);
                $c->ck('rows-before', count($rowsWas) > count($extra) && array_filter($rowsWas, static fn (array $r): bool => $r[0] === '_elementor_data') !== [], 'the page holds ' . count($rowsWas) . ' postmeta row(s) before the write');
                $input     = (string) json_encode(['post_id' => $pid, 'base_fingerprint' => (string) $fp, 'operations' => $ops], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
                $rid       = wp_generate_uuid4();
                $more      = ['input' => $input, 'allowed_draft_ids' => [$pid]];
                $pre       = rt_call($cmd, $editEntry, 'precheck', $rid, $more);
                if ($c->ck('precheck', ($pre['ok'] ?? false) === true && ($pre['outcome'] ?? null) === 'prechecked', rt_brief($pre))) {
                    // A site that rewrites what Elementor stores: the write fails after the snapshot and must put the page back.
                    $fired   = 0;
                    $rewrite = static function ($value) use (&$fired) {
                        if (!is_string($value)) {
                            return $value;
                        }
                        $changed = preg_replace('/"id":"/', '"id":"z', $value, 1, $n);
                        if (!is_string($changed) || $n !== 1) {
                            return $value;
                        }
                        ++$fired;

                        return $changed;
                    };
                    add_filter('sanitize_post_meta__elementor_data', $rewrite, 10, 1);
                    try {
                        $w = rt_call($cmd, $editEntry, 'write', $rid, $more + ['expected' => rt_digests($pre)]);
                    } finally {
                        remove_filter('sanitize_post_meta__elementor_data', $rewrite, 10);
                    }
                    $c->ck('armed', $fired >= 1, 'the rewrite never ran, so this scenario proved nothing');
                    $c->ck('refused', ($w['ok'] ?? true) === false && ($w['code'] ?? null) === 'verify_mismatch', 'a write whose stored tree differs from the planned one was answered ' . rt_brief($w));
                    $c->ck('restored', ($w['restored'] ?? null) === true, 'the answer says the page was put back: ' . rt_brief($w['restored'] ?? null));
                    if (in_array('restore_row_drift', $kind, true)) {
                        $victim = $wpdb->get_row($wpdb->prepare("SELECT meta_id, meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s", $pid, '_rt_backslashes'), ARRAY_A);
                        if (!is_array($victim) || $wpdb->update($wpdb->postmeta, ['meta_value' => $victim['meta_value'] . 'x'], ['meta_id' => (int) $victim['meta_id']], ['%s'], ['%d']) === false) {
                            $c->ck('plant', false, 'no row to change in this case');
                        }
                    }
                    $diff = rt_rows_diff($rowsWas, rt_raw_rows($pid, $skip));
                    $c->ck('rows-back', $diff === null, (string) $diff);
                    $postNow = rt_raw_post($pid);
                    $moved   = array_keys(array_filter($postWas, static fn (string $v, string $col): bool => $postNow[$col] !== $v, ARRAY_FILTER_USE_BOTH));
                    $c->ck('post-back', $moved === [], 'the posts column(s) ' . implode(',', $moved) . ' are not the bytes they were before the write');
                    $c->ck('fingerprint', BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS) === $fp, 'the page\'s fingerprint is not the one it had before the write');
                    $led = rt_call($cmd, $editEntry, 'ledger', $rid);
                    $c->ck('ledger', ($led['found'] ?? false) === true && ($led['phase'] ?? null) === 'failed', 'the ledger says ' . rt_brief($led));
                }
            }
            rt_trash($pid);
        }
        $out['scenarios'][] = 'restore';
        $report($name, $c);
    }

    // Nothing the edit path made may be left outside the trash.
    $c    = new RtChecks();
    $left = [];
    foreach ($made as $id) {
        if (rt_status_of($id) !== 'trash') {
            $left[] = $id . '=' . rt_status_of($id);
        }
    }
    $c->ck('left-behind', $left === [], 'post(s) the edit path made are not in the trash: ' . implode(',', $left));
    $report('leftover', $c);

    return $out;
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
foreach (['elementor', 'wp', 'php', 'zip_sha256', 'stored', 'layout'] as $need) {
    if (!isset($rtArgs[$need]) || $rtArgs[$need] === '') {
        rt_broken('missing argument ' . $need . '=');
    }
}
if (!in_array($rtArgs['stored'], ['as_given', 'strings'], true)) {
    rt_broken('stored= must be as_given or strings: ' . rt_brief($rtArgs['stored']));
}
if (!in_array($rtArgs['layout'], ['containers', 'sections'], true)) {
    rt_broken('layout= must be containers or sections: ' . rt_brief($rtArgs['layout']));
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
foreach ([ElementorClassicMapper::class, ElementorAdapter::class, IdSeed::class, PageCreateBuilder::class, ServicePrincipal::class, AbilityRunCommand::class, BuilderContract::class, BuilderDocumentFingerprint::class, BuilderDocumentRestore::class, BuilderDocumentSnapshot::class, BuilderPageEdit::class, ElementorDocument::class, LayoutOps::class, PageEditValidator::class, OwnAbilities::class] as $cls) {
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

// The boot was set up for one layout (run.sh sets the experiment before it starts).
// A site that runs the other one would give every agent answer below the wrong meaning.
$rtContainersOn = Plugin::$instance->experiments->is_feature_active('container');
if ($rtContainersOn !== ($rtArgs['layout'] === 'containers')) {
    rt_broken('the site runs the container experiment ' . ($rtContainersOn ? 'on' : 'off') . ' but this run is for the ' . $rtArgs['layout'] . ' layout');
}

rt_out(sprintf(
    'site php=%s wp=%s elementor=%s stored=%s layout=%s principal=%d unfiltered_html=no containers_experiment=%s',
    PHP_VERSION,
    get_bloginfo('version'),
    ELEMENTOR_VERSION,
    $rtArgs['stored'],
    $rtArgs['layout'],
    $principal,
    $rtContainersOn ? 'active' : 'inactive'
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
    // A container element is not registered on a site that runs the sections layout,
    // so Elementor would drop it on save. Sections are stored on every site.
    if ($src['containers'] && $rtArgs['layout'] !== 'containers') {
        continue;
    }
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
                        $expect  = $rtArgs['stored'] === 'strings' ? rt_scalars_as_strings($tree) : $tree;
                        $d       = is_array($decoded) ? rt_diff($expect, $decoded, 'tree') : 'the stored value is not a JSON list';
                        $ck('stored', $d === null && $decoded === $expect, (string) $d);
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

// The agent's own create path, on the same site.
$rtAgent = rt_agent_phase(
    $rtArgs['layout'],
    $rtArgs['stored'],
    $principal,
    [$rtArgs['layout'] === 'containers' ? '/rt/fixtures/elementor-classic-containers.json' : '/rt/fixtures/elementor-classic-sections.json', '/rt/harness/cases-extra.json'],
    in_array('agent_no_cases', $rtPlants['all'] ?? [], true)
);
$rtChecks += $rtAgent['checks'];
$rtFailed += $rtAgent['failed'];

// The agent's edit path and the full restore, on the same site.
$rtEdit    = rt_edit_phase($rtArgs['layout'], $rtArgs['stored'], $principal, $rtPlants, in_array('edit_no_cases', $rtPlants['all'] ?? [], true));
$rtChecks += $rtEdit['checks'];
$rtFailed += $rtEdit['failed'];

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
// So must the agent path: an outline through it, and every refusal scenario.
if ($rtAgent['cases'] < 1) {
    ++$rtFailed;
    rt_out('FAIL the agent path ran no case');
}
foreach (array_diff(RT_AGENT_SCENARIOS, $rtAgent['scenarios']) as $missing) {
    ++$rtFailed;
    rt_out('FAIL the agent path did not run the refusal scenario ' . $missing);
}
// So must the edit path: a case, every operation, and the full restore.
if ($rtEdit['cases'] < 1) {
    ++$rtFailed;
    rt_out('FAIL the edit path ran no case');
}
foreach (array_diff(BuilderContract::OPS, $rtEdit['ops']) as $missing) {
    ++$rtFailed;
    rt_out('FAIL the edit path applied no ' . $missing . ' operation');
}
foreach (array_diff(RT_EDIT_SCENARIOS, $rtEdit['scenarios']) as $missing) {
    ++$rtFailed;
    rt_out('FAIL the edit path did not run the ' . $missing . ' scenario');
}
rt_out(sprintf(
    'SUMMARY elementor=%s layout=%s cases=%d checks=%d texts=%d alts=%d agent=%d refused=%d edit=%d restore=%d failed=%d',
    ELEMENTOR_VERSION,
    $rtArgs['layout'],
    $rtCases,
    $rtChecks,
    $rtTextsSeen,
    $rtAltsSeen,
    $rtAgent['cases'],
    count($rtAgent['scenarios']),
    $rtEdit['cases'],
    count($rtEdit['scenarios']),
    $rtFailed
));
rt_out($rtFailed === 0 ? 'RESULT OK' : 'RESULT FAIL');
exit($rtFailed === 0 ? 0 : 1);
