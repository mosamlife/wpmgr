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
 *  10. render: the edited page must hold every text the operations wrote that
 *      is still on the page after the call, as written (an ampersand, an
 *      entity-shaped text and a bracket are shown literally), none of the
 *      texts they removed, the links they set that are still on the page, no
 *      script element and no on* attribute. A batch can take its own earlier
 *      changes off the page (a later operation removes or replaces the section
 *      that holds them). For a case with a golden after tree, an operation is
 *      held to the page only if an element it wrote is in that tree: the
 *      element the fixture's change names for a set_text, or an element it
 *      lists as made by an insert or a replace. The fixture's changes are
 *      used and not the precheck's, because the precheck runs under a request
 *      id of its own and so makes its new elements under other ids. A case
 *      without a golden after tree is held to everything its operations wrote,
 *      and a case whose operations leave none of their texts on the page is
 *      red, since it would look at nothing.
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
 * And an edit after a view: a draft the agent's page-create path made is
 * previewed the way a person's browser previews it (the WordPress preview of the
 * draft as an administrator, then Elementor's page styles for it), and the
 * agent's ability_run command then prechecks and writes an edit of it. On an
 * Elementor that keeps style-cache entries per page, the look must have left one
 * for the page, since a look that left none would make the case look at nothing.
 * The write must answer "applied" with the stored tree equal to the previewed
 * one, and the style-cache entries of a second draft, which the edit does not
 * touch, must be the same after the edit as before it.
 *
 * And the agent's create path for designed pages (outline version 3), on the same site. The
 * inputs are the page-create inputs of the designed golden (elementor-designed-containers.json,
 * each distinct page once), sent as the control plane sends them: with the page frame the site
 * admits as the last member. On a containers site every page goes through precheck, write and
 * undo exactly as in the agent path above, and is then held to what Elementor made of it:
 *
 *  14. the precheck answers the facts it compiled with (a block theme, the canvas frame, the
 *      breakpoints Elementor reports) and the write stores the frame's page template;
 *  15. every settings key of the stored tree is a control Elementor registers for its element, so
 *      a misspelt key, which Elementor stores and ignores, is red for every page and not only for
 *      the ones a probe was written for;
 *  16. the rendered page carries each section's anchor as the id of exactly one element, shows
 *      every text of the outline as written, holds no script element and no on* attribute, and
 *      its post CSS file carries a rule for each section and the colour of each heading;
 *  17. the undo trashes the page, and a second undo answers "already_reverted".
 *
 * On a sections site, where a container is not registered, one designed page is refused
 * containers_required and nothing is created. The summary counts these pages as designed=.
 *
 * And the render probes, from render-probes.json beside this file. A probe is a raw
 * Elementor tree (by layout), the declarations Elementor's own post CSS file must
 * carry for it and the ids the rendered page must carry. It is the check that a key
 * Elementor ignores fails the build: the write looks successful and the page is
 * unstyled. For every probe that has a tree for the layout this boot runs:
 *
 *  11. save the tree with Elementor's Document::save as the agent's service user,
 *      read _elementor_data back with SQL: it must be the tree that was saved;
 *  12. render the page, then read the post CSS file Elementor wrote for it and the
 *      rendered HTML;
 *  13. hold them to the probe: every "css" expectation is a declaration a rule of the
 *      named element must carry (in the named media condition, null for none; "decl"
 *      is all of a set of declarations, "any" is a list of such sets of which one must
 *      hold, since a release may write the same fact in another property); every
 *      "css_absent" declaration must be declared by no rule of the element in any
 *      media condition (a fact recorded as false); every "html_ids" id is on exactly
 *      one element, and every "html_tags" id is on an element of that tag.
 *
 * A probe may also hold the page itself to "html_text" (texts shown), "html_has" (the elements
 * a small selector finds: exactly "count", default one, or at least "min", and optionally one
 * of them showing "text"), "widgets" (registered or not on this site), "fonts" (families queued
 * for Google's font service once the post's stylesheet is enqueued), "theme" (a block theme that
 * must be active), "render": "page" with "page": {template, settings} (the published page as a
 * visitor gets it, with its template and page settings written as plain post meta), "since" (the
 * Elementor release a probe applies from; on older pins it is skipped with a note and not counted)
 * and "plants" (the element and setting key the plants misspell_key and drop_activator act on).
 * dump=<probe> prints the stored tree and the rendered page of a probe, to write its expectations.
 * dumpcss=<probe name, or its first words> prints the rules of the post CSS that are about an element of the probe's tree (media condition, selectors, declarations), to write them.
 *
 * A probe must expect at least one declaration to be present (its positive control),
 * and the phase must run at least one probe on every layout, or it is red. A probe
 * that fails prints a line "FAIL [<version> <layout> probe-<name>] <check>: <why>"
 * and the SUMMARY counts probes= and probes_failed=.
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

use Elementor\Core\Files\CSS\Post as RtPostCss;
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
    // A text the operations wrote, and that is still on the page after the call, is missing from the page an edit case rendered.
    'edit_render_drop_text',
    // A link the operations wrote, and that is still on the page after the call, is missing from the page an edit case rendered.
    'edit_render_drop_link',
    // After an edit case is written, Elementor's stored copy loses one setting of an element the edit did not touch.
    'edit_drops_setting',
    // After the full restore, one preserved postmeta row differs from its bytes before the write (use the case name "restore").
    'restore_row_drift',
    // While the edit of a previewed page is saved, the style-cache entry of another page is cleared (use the case name "after-view").
    'edit_view_other_cache',
    // The preview before the edit does not enqueue the page's styles, so it leaves no style-cache entry for the page (use the case name "after-view").
    'edit_view_no_styles',
    // The designed pages are handed no page at all (use the case name "all").
    'designed_no_cases',
    // One settings key of a designed page's stored tree is misspelt before the keys are held to Elementor's controls (use the page's case name).
    'designed_unknown_key',
    // The rendered designed page loses the id of its first section (use the page's case name).
    'designed_drop_anchor',
    // The page template of a created designed page is another than the frame's, as stored (use the page's case name).
    'designed_template_drift',
    // The rendered designed page loses its first text (use the page's case name).
    'designed_drop_text',
    // The render probes are handed no probe at all (use the case name "all").
    'probes_no_cases',
    // One setting key of a probe's tree is misspelt before the tree is saved (use the probe's name): Elementor keeps the
    // unknown key and ignores it, so the declaration the key stood for is missing from the post CSS.
    'misspell_key',
    // The activator of a group control is removed from a probe's tree before it is saved (use the probe's name): the
    // group's other keys stay, and Elementor writes none of them.
    'drop_activator',
];

/** The plants that act on a probe's tree; each probe names its own targets in "plants". */
const RT_PROBE_PLANTS = ['misspell_key', 'drop_activator'];

/** The plants that act on one designed page, named by its case in the designed golden. */
const RT_DESIGNED_PLANTS = ['designed_unknown_key', 'designed_drop_anchor', 'designed_template_drift', 'designed_drop_text'];

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

/** As rt_brief, for an answer whose detail the reader needs: up to $max characters. */
function rt_long($v, int $max = 700): string
{
    $s = is_string($v) ? $v : (string) json_encode($v, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_PARTIAL_OUTPUT_ON_ERROR);

    return strlen($s) > $max ? substr($s, 0, $max - 3) . '...' : $s;
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
 * A caller of the agent's ability_run command for wpmgr/page-create, with the catalogue entry the
 * control plane sends for it: call(mode, request id, more members of p).
 *
 * @return Closure(string,string,array<string,mixed>=):array<string,mixed>
 */
function rt_create_caller(): Closure
{
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

    return static fn (string $mode, string $rid, array $more = []): array => rt_ability($cmd, ['mode' => $mode, 'request_id' => $rid, 'entry' => $entryText, 'entry_sha256' => $entrySha] + $more);
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

    $call    = rt_create_caller();
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
// The agent's create path for designed pages
// ---------------------------------------------------------------------------

/** The golden of designed pages. Each case's "input" is the exact page-create input the control plane sends. */
const RT_DESIGNED_FILE = '/rt/fixtures/elementor-designed-containers.json';

/** The frame a block theme shows a designed page in. */
const RT_DESIGNED_FRAME = 'canvas';

/** The page template Elementor stores for that frame. */
const RT_DESIGNED_TEMPLATE = 'elementor_canvas';

/**
 * The keys of an outline whose strings a visitor does not read: they choose a shape, a place or an
 * address, or they are an attribute. Every other string of an outline is text the page must show.
 */
const RT_DESIGNED_NOT_TEXT = ['type', 'pattern', 'variant', 'anchor', 'url', 'style', 'tone', 'icon', 'align', 'width', 'min_height', 'spacing', 'overlay', 'focal', 'alt', 'size', 'suffix'];

/**
 * The designed inputs of this run: each distinct page of the golden once, as the control plane sends
 * it to a site that admits only the canvas frame. Two cases that differ only in the facts of their
 * site or in their frame are one page here, because this site decides those.
 *
 * @return list<array{name:string,input:string,outline:list<mixed>}>
 */
function rt_designed_inputs(): array
{
    $loaded = rt_load(RT_DESIGNED_FILE);
    $flags  = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR;
    $seen   = [];
    $out    = [];
    try {
        foreach ($loaded['arrays']['cases'] as $case) {
            $name  = is_array($case) ? (string) ($case['name'] ?? '') : '';
            $input = is_array($case) ? ($case['input'] ?? null) : null;
            $page  = is_string($input) ? json_decode($input, false, 64) : null;
            if ($name === '' || !is_object($page) || !is_array($page->outline ?? null)) {
                rt_broken('a case in ' . RT_DESIGNED_FILE . ' has no name or no page-create input: ' . rt_brief($name));
            }
            unset($page->page_frame);
            $key = hash('sha256', json_encode($page, $flags));
            if (isset($seen[$key])) {
                continue;
            }
            $seen[$key]       = true;
            $outline          = json_decode(json_encode($page->outline, $flags), true, 64);
            $page->page_frame = RT_DESIGNED_FRAME;
            $out[]            = ['name' => $name, 'input' => json_encode($page, $flags), 'outline' => is_array($outline) ? $outline : []];
        }
    } catch (JsonException $e) {
        rt_broken('a designed input cannot be re-encoded: ' . $e->getMessage());
    }

    return $out;
}

/**
 * Every string of a designed outline that a visitor reads, in document order.
 *
 * @param mixed        $node
 * @param list<string> $out
 */
function rt_designed_texts($node, array &$out, string $key = ''): void
{
    if (is_array($node)) {
        foreach ($node as $k => $child) {
            rt_designed_texts($child, $out, is_string($k) ? $k : $key);
        }

        return;
    }
    if (is_string($node) && $node !== '' && !in_array($key, RT_DESIGNED_NOT_TEXT, true)) {
        $out[] = $node;
    }
}

/**
 * The anchors of an outline's sections, in order.
 *
 * @param list<mixed> $outline
 * @return list<string>
 */
function rt_designed_anchors(array $outline): array
{
    $out = [];
    foreach ($outline as $section) {
        if (is_array($section) && is_string($section['anchor'] ?? null) && $section['anchor'] !== '') {
            $out[] = $section['anchor'];
        }
    }

    return $out;
}

/** Settings Elementor stores and reads without a control, by element type: the navigator label of a container. */
const RT_UNOWNED_KEYS = ['container' => ['_title']];

/**
 * Hold every settings key of a tree to the controls Elementor registers for its element. A key no
 * control answers to is stored and ignored. Returns how many keys it looked at.
 *
 * @param array<mixed> $nodes
 * @param list<string> $bad   What no control answers to.
 */
function rt_unregistered_keys(array $nodes, array &$bad, string $path = 'tree'): int
{
    $looked = 0;
    foreach ($nodes as $i => $node) {
        if (!is_array($node)) {
            continue;
        }
        $at     = $path . '[' . $i . ']';
        $widget = ($node['elType'] ?? '') === 'widget';
        $type   = $widget ? (string) ($node['widgetType'] ?? '') : (string) ($node['elType'] ?? '');
        // The element as Elementor builds it from the stored data, which is what its CSS is generated from.
        $data = ['id' => (string) ($node['id'] ?? ''), 'elType' => (string) ($node['elType'] ?? ''), 'settings' => is_array($node['settings'] ?? null) ? $node['settings'] : [], 'elements' => [], 'isInner' => (bool) ($node['isInner'] ?? false)];
        if ($widget) {
            $data['widgetType'] = $type;
        }
        $el = Plugin::$instance->elements_manager->create_element_instance($data);
        if (!is_object($el)) {
            $bad[] = $at . ' ' . $type . ': Elementor has no such element';
        } else {
            foreach (array_keys(is_array($node['settings'] ?? null) ? $node['settings'] : []) as $key) {
                ++$looked;
                // By name: a release may keep the controls that carry CSS apart from the others when it lists them all.
                if ($el->get_controls((string) $key) === null && !in_array((string) $key, RT_UNOWNED_KEYS[$type] ?? [], true)) {
                    $bad[] = $at . ' ' . $type . ' ' . (string) ($node['id'] ?? '?') . ': no control named ' . $key;
                }
            }
        }
        if (is_array($node['elements'] ?? null)) {
            $looked += rt_unregistered_keys($node['elements'], $bad, $at . '.elements');
        }
    }

    return $looked;
}

/**
 * Plant: misspell the first settings key named $key anywhere in a tree.
 *
 * @param array<mixed> $nodes
 */
function rt_misspell_key(array &$nodes, string $key): bool
{
    foreach ($nodes as &$node) {
        if (!is_array($node)) {
            continue;
        }
        if (is_array($node['settings'] ?? null) && array_key_exists($key, $node['settings'])) {
            $settings = [];
            foreach ($node['settings'] as $k => $v) {
                $settings[$k === $key ? $key . 'x' : $k] = $v;
            }
            $node['settings'] = $settings;

            return true;
        }
        if (is_array($node['elements'] ?? null) && rt_misspell_key($node['elements'], $key)) {
            return true;
        }
    }

    return false;
}

/**
 * The colour each heading of a tree is given, by element id.
 *
 * @param array<mixed> $nodes
 * @return array<string,string>
 */
function rt_heading_colours(array $nodes): array
{
    $out = [];
    foreach ($nodes as $node) {
        if (!is_array($node)) {
            continue;
        }
        if (($node['widgetType'] ?? '') === 'heading' && is_string($node['settings']['title_color'] ?? null) && $node['settings']['title_color'] !== '' && is_string($node['id'] ?? null)) {
            $out[$node['id']] = $node['settings']['title_color'];
        }
        if (is_array($node['elements'] ?? null)) {
            $out += rt_heading_colours($node['elements']);
        }
    }

    return $out;
}

/**
 * Run the agent's create path for the designed pages on this site. See the steps 14 to 17 in the
 * file header.
 *
 * @param bool $noCases Plant: hand the agent no page at all.
 * @return array{cases:int,checks:int,failed:int,keys:int,css:int}
 */
function rt_designed_phase(string $layout, string $stored, int $principal, bool $noCases): array
{
    global $wpdb;
    $out       = ['cases' => 0, 'checks' => 0, 'failed' => 0, 'keys' => 0, 'css' => 0];
    $tagBase   = ELEMENTOR_VERSION . ' designed-' . $layout;
    $created   = 0;
    $hex       = static fn ($v): bool => is_string($v) && preg_match('/^[0-9a-f]{64}$/D', $v) === 1;
    $report    = static function (string $what, RtChecks $c) use (&$out, $tagBase): void {
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
    $call      = rt_create_caller();
    $digestsOf = static fn (array $pre): array => ['precheck_digest' => (string) ($pre['precheck_digest'] ?? ''), 'preview_digest' => (string) ($pre['preview_digest'] ?? '')];
    $marked    = static fn (): int => (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM {$wpdb->postmeta} WHERE meta_key = %s", RT_MARKER));
    $before    = $marked();
    $inputs    = $noCases ? [] : rt_designed_inputs();

    if ($layout !== 'containers') {
        // A container is not registered on a sections site, so a designed page is refused before anything is built.
        if ($inputs !== []) {
            $c   = new RtChecks();
            $rid = wp_generate_uuid4();
            $pre = $call('precheck', $rid, ['input' => $inputs[0]['input']]);
            $c->ck('refused', ($pre['ok'] ?? true) === false && ($pre['code'] ?? null) === 'containers_required', 'a designed page on a sections site was answered ' . rt_brief($pre));
            $c->ck('nothing-created', rt_posts_of($rid) === [], 'a refused designed page created post(s)');
            ++$out['cases'];
            $report($inputs[0]['name'], $c);
        }

        return $out;
    }

    // What this site reports, which the pages are held to.
    $c        = new RtChecks();
    $c->ck('block-theme', wp_is_block_theme(), 'the harness site must run a block theme, which shows a designed page only as ' . RT_DESIGNED_FRAME);
    $ctl      = Plugin::$instance->elements_manager->create_element_instance(['id' => 'a000001', 'elType' => 'container', 'settings' => [], 'elements' => [], 'isInner' => false]);
    // The positive control of the key check: controls it must find, and one name it must not.
    $found    = is_object($ctl) ? [$ctl->get_controls('padding') !== null, $ctl->get_controls('flex_direction') !== null, $ctl->get_controls('background_color') !== null, $ctl->get_controls('wpmgr_no_such_control') === null] : [];
    $c->ck('controls', $found === [true, true, true, true], 'Elementor answers ' . rt_brief($found) . ' for a container\'s padding, flex_direction, background_color and a name no control has, so the key check would look at nothing');
    $report('facts', $c);
    $breakpoints = array_keys(Plugin::$instance->breakpoints->get_active_breakpoints());

    foreach ($inputs as $case) {
        $name   = $case['name'];
        $plants = $GLOBALS['rtPlants'][$name] ?? [];
        $rid    = wp_generate_uuid4();
        $c      = new RtChecks();
        $pre    = $call('precheck', $rid, ['input' => $case['input']]);
        if ($c->ck('precheck', ($pre['ok'] ?? false) === true && ($pre['outcome'] ?? null) === 'prechecked', rt_long($pre))) {
            $preview = is_array($pre['preview'] ?? null) ? $pre['preview'] : [];
            $tree    = $preview['tree'] ?? null;
            $bf      = is_array($pre['builder_facts'] ?? null) ? $pre['builder_facts'] : [];
            $c->ck('layout', ($preview['layout'] ?? null) === 'containers', 'the agent built the ' . rt_brief($preview['layout'] ?? '(none)') . ' layout on a containers site');
            $c->ck('frame', ($bf['block_theme'] ?? null) === true && ($bf['page_frame'] ?? null) === RT_DESIGNED_FRAME, 'the precheck resolved ' . rt_brief($bf) . ' on a block theme');
            $c->ck('breakpoints', ($bf['active_breakpoints'] ?? null) === $breakpoints, 'the precheck compiled for the breakpoints ' . rt_brief($bf['active_breakpoints'] ?? null) . ', Elementor reports ' . rt_brief($breakpoints));
            $c->ck('digests', $hex($pre['preview_digest'] ?? null) && $hex($pre['precheck_digest'] ?? null), 'the precheck gave no digests');
            $c->ck('preview-tree', is_array($tree) && $tree !== [], 'the precheck previewed no tree');
            // The options written during the write are named in a refusal, so a refused page says what the save wrote.
            $optionsWritten = [];
            $noteOption     = static function ($name = '') use (&$optionsWritten): void {
                $optionsWritten[(string) $name] = true;
            };
            add_action('added_option', $noteOption, 1, 1);
            add_action('updated_option', $noteOption, 1, 1);
            $w = $call('write', $rid, ['input' => $case['input'], 'expected' => $digestsOf($pre)]);
            remove_action('added_option', $noteOption, 1);
            remove_action('updated_option', $noteOption, 1);
            if (($w['code'] ?? null) === 'side_effect_detected') {
                $w['harness_options_written'] = array_keys($optionsWritten);
            }
            if ($c->ck('write', ($w['ok'] ?? false) === true && ($w['outcome'] ?? null) === 'created', rt_long($w))) {
                $pid = (int) ($w['post_id'] ?? 0);
                $c->ck('write-digest', ($w['preview_digest'] ?? null) === ($pre['preview_digest'] ?? ''), 'the write answers a preview digest that is not the precheck\'s');
                $c->ck('verify', ($w['verify'] ?? null) === ['tree_equal' => true, 'status' => 'draft'], 'the write answers verify ' . rt_brief($w['verify'] ?? null));

                // What the database holds, read here and not taken from the answer.
                $posts = rt_posts_of($rid);
                $c->ck('one-post', $pid > 0 && $posts === [$pid], 'the posts marked with this request are [' . implode(',', $posts) . '], the answer names ' . $pid);
                $row = $wpdb->get_row($wpdb->prepare("SELECT post_status, post_author, post_type FROM {$wpdb->posts} WHERE ID = %d", $pid), ARRAY_A);
                $c->ck('draft', is_array($row) && $row['post_status'] === 'draft' && (int) $row['post_author'] === $principal && $row['post_type'] === 'page', 'the post is not a draft page of the principal: ' . rt_brief($row));
                if (in_array('designed_template_drift', $plants, true)) {
                    $wpdb->update($wpdb->postmeta, ['meta_value' => 'default'], ['post_id' => $pid, 'meta_key' => '_wp_page_template']);
                }
                $template = $wpdb->get_col($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s ORDER BY meta_id", $pid, '_wp_page_template'));
                $c->ck('template', $template === [RT_DESIGNED_TEMPLATE], 'the page template rows are ' . rt_brief($template) . ', the ' . RT_DESIGNED_FRAME . ' frame stores ' . RT_DESIGNED_TEMPLATE);
                [$rows, $decoded] = rt_stored_tree($pid);
                $expect           = $stored === 'strings' && is_array($tree) ? rt_scalars_as_strings($tree) : $tree;
                $d                = $decoded !== null && is_array($expect) ? rt_diff($expect, $decoded, 'tree') : 'expected one _elementor_data row holding a tree, found ' . $rows;
                $c->ck('stored-tree', $d === null, (string) $d);

                // Every settings key is a control Elementor registers for its element.
                $held = is_array($decoded) ? $decoded : [];
                if (in_array('designed_unknown_key', $plants, true)) {
                    $c->ck('plant', rt_misspell_key($held, 'padding'), 'no padding key to misspell in this page');
                }
                $bad = [];
                $n   = rt_unregistered_keys($held, $bad);
                $out['keys'] += $n;
                $c->ck('keys', $n > 0 && $bad === [], $n === 0 ? 'the tree holds no settings key to look at' : count($bad) . ' settings key(s) no Elementor control answers to, first: ' . implode(' | ', array_slice($bad, 0, 3)));

                // The rendered page, as a visitor gets it, and the post CSS file Elementor wrote for it.
                $anchors = rt_designed_anchors($case['outline']);
                $texts   = [];
                rt_designed_texts($case['outline'], $texts);
                wp_set_current_user($principal);
                try {
                    $html = (string) Plugin::$instance->frontend->get_builder_content($pid, false);
                } catch (Throwable $e) {
                    $html = '';
                    $c->ck('render', false, 'render threw ' . get_class($e) . ': ' . $e->getMessage());
                }
                setlocale(LC_NUMERIC, 'C');
                if (in_array('designed_drop_anchor', $plants, true) && $anchors !== []) {
                    $html = (string) preg_replace('/\sid="' . preg_quote($anchors[0], '/') . '"/', ' data-planted="1"', $html, 1);
                }
                if ($c->ck('render-nonempty', trim($html) !== '', 'the page rendered nothing')) {
                    $dom = rt_dom($html);
                    // A page may carry structured data (a FAQ's schema); no other script element may be on it.
                    $runnable = 0;
                    foreach ($dom->getElementsByTagName('script') as $script) {
                        if (strtolower(trim($script->getAttribute('type'))) !== 'application/ld+json' || json_decode((string) $script->textContent, true) === null) {
                            ++$runnable;
                        }
                    }
                    $c->ck('render-script', $runnable === 0, 'the page holds ' . $runnable . ' script element(s) that are not structured data');
                    $onAttr = '';
                    $ids    = [];
                    foreach ($dom->getElementsByTagName('*') as $el) {
                        foreach ($el->attributes ?? [] as $attr) {
                            if (stripos($attr->name, 'on') === 0 && $onAttr === '') {
                                $onAttr = $el->nodeName . '[' . $attr->name . ']';
                            }
                        }
                        $id = $el->getAttribute('id');
                        if ($id !== '') {
                            $ids[$id] = ($ids[$id] ?? 0) + 1;
                        }
                    }
                    $c->ck('render-on', $onAttr === '', 'an on* attribute: ' . $onAttr);
                    $c->ck('anchors', $anchors !== [], 'the page has no section anchor, so the anchor check would look at nothing');
                    foreach ($anchors as $anchor) {
                        $c->ck('anchor', ($ids[$anchor] ?? 0) === 1, 'the rendered page carries id="' . $anchor . '" on ' . ($ids[$anchor] ?? 0) . ' element(s), not one');
                    }
                    $page = rt_norm((string) $dom->textContent);
                    if (in_array('designed_drop_text', $plants, true) && $texts !== []) {
                        $page = str_replace(rt_norm($texts[0]), '', $page);
                    }
                    $miss = [];
                    foreach ($texts as $t) {
                        if (strpos($page, rt_norm($t)) === false) {
                            $miss[] = rt_brief($t);
                        }
                    }
                    $c->ck('render-text', ($texts !== [] || !rt_has_text_widget($held)) && $miss === [], $texts === [] ? 'the page has text widgets but the harness found no text in the outline' : count($miss) . ' text(s) not shown as written, first: ' . ($miss[0] ?? ''));
                }
                $file = RtPostCss::create($pid);
                $path = (string) $file->get_path();
                if (!is_file($path)) {
                    $file->update();
                    clearstatcache();
                }
                $css = is_file($path) ? (string) file_get_contents($path) : '';
                if ($c->ck('css-file', $css !== '', 'no post CSS file at ' . $path . ', or it is empty')) {
                    $rules = rt_css_rules($css);
                    foreach ($held as $section) {
                        $c->ck('css-section', is_array($section) && strpos($css, '.elementor-element-' . (string) ($section['id'] ?? '?')) !== false, 'the post CSS has no rule for the section ' . rt_brief(is_array($section) ? ($section['id'] ?? null) : null));
                    }
                    foreach (rt_heading_colours($held) as $el => $colour) {
                        [$ok, $why] = rt_css_expect($rules, ['el' => $el, 'sel' => '.elementor-heading-title', 'media' => null, 'decl' => ['color' => $colour]]);
                        ++$out['css'];
                        $c->ck('css-heading', $ok, 'heading ' . $el . ': want color:' . $colour . ' ' . $why);
                    }
                }
                wp_set_current_user(0);

                // Undo.
                $r = $call('revert', $rid, ['input' => '{}']);
                $c->ck('revert', ($r['ok'] ?? false) === true && ($r['outcome'] ?? null) === 'reverted' && (int) ($r['post_id'] ?? 0) === $pid && ($r['trashed'] ?? null) === true, rt_brief($r));
                $c->ck('trashed', rt_status_of($pid) === 'trash', 'after the revert the post is ' . rt_status_of($pid));
                $again = $call('revert', $rid, ['input' => '{}']);
                $c->ck('revert-twice', ($again['ok'] ?? false) === true && ($again['outcome'] ?? null) === 'already_reverted', rt_brief($again));
            }
        }
        // A write the agent refused after it made the draft leaves that draft in the trash, and it is counted here too.
        $created += count(rt_posts_of($rid));
        ++$out['cases'];
        $report($name, $c);
    }

    // A plant that names no page would change nothing and prove nothing.
    $names = array_column($inputs, 'name');
    foreach ($GLOBALS['rtPlants'] ?? [] as $name => $kinds) {
        foreach ($kinds as $kind) {
            if (in_array($kind, RT_DESIGNED_PLANTS, true) && !in_array((string) $name, $names, true)) {
                ++$out['failed'];
                rt_out(sprintf('FAIL [%s %s] plant: no designed page is named %s', $tagBase, (string) $name, (string) $name));
            }
        }
    }

    // Nothing the designed pages made may be left outside the trash, and they made exactly what this run asked for.
    $c    = new RtChecks();
    $left = $wpdb->get_col($wpdb->prepare("SELECT p.ID FROM {$wpdb->posts} p INNER JOIN {$wpdb->postmeta} m ON m.post_id = p.ID AND m.meta_key = %s WHERE p.post_status <> 'trash' ORDER BY p.ID", RT_MARKER));
    $c->ck('left-behind', is_array($left) && $left === [], 'post(s) the agent created are not in the trash: [' . implode(',', is_array($left) ? $left : []) . ']');
    $c->ck('created', $marked() - $before === $created, 'the designed run created ' . $created . ' post(s), the database gained ' . ($marked() - $before));
    $report('leftover', $c);

    return $out;
}

// ---------------------------------------------------------------------------
// The agent's edit path
// ---------------------------------------------------------------------------

/** The scenarios the edit phase must have run besides its cases, by name. A run that ran fewer is red. */
const RT_EDIT_SCENARIOS = ['restore', 'after-view'];

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
 * The elements of a tree in page order, each as id, element type and widget
 * type: what a page holds, whatever form its settings are stored in.
 *
 * @param array<mixed> $nodes Elements.
 * @param list<string> $out   Gets one entry per element.
 */
function rt_shape(array $nodes, array &$out): void
{
    foreach ($nodes as $node) {
        if (!is_array($node)) {
            $out[] = '(not an element)';
            continue;
        }
        $out[] = (string) ($node['id'] ?? '') . ':' . (string) ($node['elType'] ?? '') . ':' . (string) ($node['widgetType'] ?? '');
        rt_shape(is_array($node['elements'] ?? null) ? $node['elements'] : [], $out);
    }
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
 * The ids of the elements of a tree, at any depth.
 *
 * @param array<mixed>       $nodes Elements.
 * @param array<string,true> $out   Gets one entry per id.
 */
function rt_node_ids(array $nodes, array &$out): void
{
    foreach ($nodes as $node) {
        if (!is_array($node)) {
            continue;
        }
        if (is_string($node['id'] ?? null) && $node['id'] !== '') {
            $out[$node['id']] = true;
        }
        if (is_array($node['elements'] ?? null)) {
            rt_node_ids($node['elements'], $out);
        }
    }
}

/**
 * Whether an element an operation wrote is on the page after the call: the
 * element a set_text changed, or any element an insert or a replace made. A
 * change that names no element it wrote is held to the page, and so is every
 * change when the page after the call is not known.
 *
 * @param array<mixed>            $change One entry of the golden fixture's changes.
 * @param array<string,true>|null $live   The ids of the elements on the page after the call, or null.
 */
function rt_wrote_on_page(array $change, ?array $live): bool
{
    if ($live === null) {
        return true;
    }
    $op = $change['op'] ?? '';
    if ($op === 'set_text') {
        $wrote = [$change['ref'] ?? null];
    } elseif ($op === 'insert' || $op === 'replace') {
        $wrote = is_array($change['new_refs'] ?? null) ? $change['new_refs'] : [];
    } else {
        $wrote = [];
    }
    $named = false;
    foreach ($wrote as $id) {
        if (!is_string($id) || $id === '') {
            continue;
        }
        $named = true;
        if (isset($live[$id])) {
            return true;
        }
    }

    return !$named;
}

/**
 * The texts and the links the page of an edit case is held to: those the
 * operations wrote, without the ones of an operation whose elements are off the
 * page after the call (a later operation of the batch removed or replaced the
 * section that held them).
 *
 * @param list<array<string,mixed>> $ops     The operations, as the AI sends them.
 * @param array<mixed>              $changes The golden fixture's changes, one per operation, in order, or none.
 * @param array<string,true>|null   $live    The ids of the elements on the page after the call, or null when the case has no such tree.
 * @return array{texts:list<string>,links:list<string>,off:int} What the page must show, and how many written texts are off the page.
 */
function rt_held_to_page(array $ops, array $changes, ?array $live): array
{
    $held = ['texts' => [], 'links' => [], 'off' => 0];
    $objs = json_decode((string) json_encode($ops, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES), false);
    // The changes name the elements of their operation only when they come one per operation, in order.
    $paired = count($changes) === count($ops);
    foreach (is_array($objs) ? array_values($objs) : [] as $i => $op) {
        $texts = [];
        $links = [];
        if (($op->op ?? '') === 'set_text' && in_array($op->field ?? '', ['text', 'caption'], true) && is_string($op->text ?? null)) {
            $texts[] = $op->text;
        }
        rt_texts($op->outline ?? [], $texts);
        rt_links($op, $links);
        $change = $paired && is_array($changes[$i] ?? null) && ($changes[$i]['op'] ?? null) === ($op->op ?? '') ? $changes[$i] : null;
        if ($change !== null && !rt_wrote_on_page($change, $live)) {
            $held['off'] += count($texts);
            continue;
        }
        array_push($held['texts'], ...$texts);
        array_push($held['links'], ...$links);
    }

    return $held;
}

/**
 * Render a page with Elementor's frontend and hold it to what an edit wrote.
 *
 * @param list<string> $texts Texts the page must show as written.
 * @param list<string> $gone  Texts the page must no longer show.
 * @param list<string> $links Absolute links an anchor of the page must carry.
 * @param list<string> $kinds The plants of the case.
 * @param int          $off   How many texts the operations wrote are off the page after the call, and so not in $texts.
 */
function rt_render_checks(RtChecks $c, int $postId, array $texts, array $gone, array $links, array $kinds, int $off = 0): void
{
    $dropped = ['text' => 0, 'link' => 0];
    $plant   = static function (string $html) use ($kinds, $texts, $links, &$dropped): string {
        if (in_array('edit_render_script', $kinds, true)) {
            $html .= '<script>alert(1)</script>';
        }
        if (in_array('edit_render_text', $kinds, true)) {
            $html = str_replace('&amp;', '&amp;amp;', $html);
        }
        if (in_array('edit_render_drop_text', $kinds, true) && $texts !== []) {
            $html = str_replace($texts[0], '', $html, $n);
            $dropped['text'] += $n;
        }
        if (in_array('edit_render_drop_link', $kinds, true)) {
            foreach ($links as $link) {
                if (strpos($link, 'https://') === 0) {
                    $html = str_replace($link, '#', $html, $n);
                    $dropped['link'] += $n;
                    break;
                }
            }
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
    if (in_array('edit_render_drop_text', $kinds, true)) {
        $c->ck('plant', $dropped['text'] > 0, 'no text to drop in the rendered page');
    }
    if (in_array('edit_render_drop_link', $kinds, true)) {
        $c->ck('plant', $dropped['link'] > 0, 'no link to drop in the rendered page');
    }

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
    $none = $off > 0 ? 'every text the operations wrote is off the page after the call, so the page is held to none of them' : 'the harness found no text the operations wrote';
    $c->ck('render-text', $texts !== [] && $miss === [], $texts === [] ? $none : count($miss) . ' text(s) not shown as written, first: ' . ($miss[0] ?? ''));
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

// ---------------------------------------------------------------------------
// An edit of a page that has been looked at
// ---------------------------------------------------------------------------

/** The prefix of the options Elementor keeps the validity of its style caches in, one option per kind of cache. */
const RT_STYLE_CACHE_PREFIX = 'elementor_atomic_cache_validity__';

/** Elementor's class for those options. */
const RT_STYLE_CACHE_CLASS = 'Elementor\\Modules\\AtomicWidgets\\Styles\\CacheValidity\\Cache_Validity';

/** The oldest pinned Elementor that keeps them: from there on, a version without the class is red and not skipped. */
const RT_STYLE_CACHE_SINCE = '3.35.9';

/**
 * The style-cache entries a page holds, read with SQL: for each option that holds
 * one, the entry as stored.
 *
 * @return array<string,string> Option name => the page's entry, serialized; empty when the page has none.
 */
function rt_style_cache_of(int $postId): array
{
    global $wpdb;
    $rows = $wpdb->get_results($wpdb->prepare("SELECT option_name, option_value FROM {$wpdb->options} WHERE option_name LIKE %s ORDER BY option_name", $wpdb->esc_like(RT_STYLE_CACHE_PREFIX) . '%'), ARRAY_A);
    if (!is_array($rows) || (string) $wpdb->last_error !== '') {
        rt_broken('the style-cache options cannot be read');
    }
    $out = [];
    foreach ($rows as $row) {
        $root = maybe_unserialize((string) $row['option_value']);
        if (is_array($root) && is_array($root['children'] ?? null) && array_key_exists($postId, $root['children'])) {
            $out[(string) $row['option_name']] = serialize($root['children'][$postId]);
        }
    }

    return $out;
}

/**
 * Preview a draft the way a person's browser does, as an administrator: the
 * WordPress main query for ?page_id=<id>&preview=true, then Elementor's frontend
 * registering its scripts and styles and enqueuing the styles of the page the
 * main query holds. Elementor enqueues page styles once per request, so this is
 * the one look of a run. What the look sets up in the request (the query
 * variables, the main query, the current post and user) is put back, as the next
 * request of a site would start without it.
 *
 * @param bool $styles Plant: false leaves out Elementor's styles, so only the main query is made.
 * @return array{preview:bool,queried:int,threw:string} What the main query held, and the class of what was thrown, if anything.
 */
function rt_view_page(int $postId, bool $styles = true): array
{
    $get   = $_GET;
    $query = $_SERVER['QUERY_STRING'] ?? null;
    $uri   = $_SERVER['REQUEST_URI'] ?? null;
    $seen  = ['preview' => false, 'queried' => 0, 'threw' => ''];
    wp_set_current_user(1);
    $_GET['page_id']         = (string) $postId;
    $_GET['preview']         = 'true';
    $_SERVER['QUERY_STRING'] = 'page_id=' . $postId . '&preview=true';
    $_SERVER['REQUEST_URI']  = '/?page_id=' . $postId . '&preview=true';
    try {
        wp();
        $seen['preview'] = is_preview();
        $seen['queried'] = (int) get_queried_object_id();
        if ($styles) {
            $frontend = Plugin::$instance->frontend;
            $frontend->register_scripts();
            $frontend->register_styles();
            $frontend->enqueue_styles();
        }
    } catch (Throwable $e) {
        $seen['threw'] = get_class($e);
    } finally {
        $_GET = $get;
        foreach (['QUERY_STRING' => $query, 'REQUEST_URI' => $uri] as $key => $was) {
            if ($was === null) {
                unset($_SERVER[$key]);
            } else {
                $_SERVER[$key] = $was;
            }
        }
        $GLOBALS['wp_query']     = new WP_Query();
        $GLOBALS['wp_the_query'] = $GLOBALS['wp_query'];
        $GLOBALS['post']         = null;
        setlocale(LC_NUMERIC, 'C');
        wp_set_current_user(0);
    }

    return $seen;
}

/**
 * The agent's edit path on a page that has been looked at. See the file header.
 *
 * @param array{0:string,1:string} $createEntry The page-create entry.
 * @param array{0:string,1:string} $editEntry   The page-edit entry.
 * @param array<string,mixed>      $spec        The case: "seed" and "ops".
 * @param list<string>             $kinds       The plants of the case.
 * @param string                   $tag         What the notes of the case say they are about.
 * @return array{0:RtChecks,1:list<int>} The checks, and the drafts the case made.
 */
function rt_edit_after_view(AbilityRunCommand $cmd, array $createEntry, array $editEntry, array $spec, string $stored, array $kinds, string $tag): array
{
    $c    = new RtChecks();
    $made = [];
    if (!is_array($spec['seed'] ?? null) || !is_array($spec['ops'] ?? null)) {
        $c->ck('case', false, 'the case has no seed or no operations');

        return [$c, $made];
    }

    // Whether this Elementor keeps style-cache entries, and so whether the look leaves one to check.
    $cacheClass = class_exists(RT_STYLE_CACHE_CLASS) ? RT_STYLE_CACHE_CLASS : null;
    $expected   = version_compare(ELEMENTOR_VERSION, RT_STYLE_CACHE_SINCE, '>=');
    if (!$c->ck('style-cache', $cacheClass !== null || !$expected, 'Elementor ' . ELEMENTOR_VERSION . ' is expected to keep style-cache entries and has no ' . RT_STYLE_CACHE_CLASS)) {
        return [$c, $made];
    }

    // The page, and a second one whose style-cache entries the edit must leave as they are.
    [$pid, $why] = rt_make_draft($cmd, $createEntry, $spec['seed']);
    if (!$c->ck('seed', $pid > 0, $why)) {
        return [$c, $made];
    }
    $made[] = $pid;
    $other  = 0;
    if ($cacheClass !== null) {
        [$other, $why] = rt_make_draft($cmd, $createEntry, $spec['seed']);
        if (!$c->ck('seed-other', $other > 0, $why)) {
            return [$c, $made];
        }
        $made[] = $other;
    }

    // Look at the page.
    $noStyles = in_array('edit_view_no_styles', $kinds, true);
    if ($noStyles && $cacheClass === null) {
        $c->ck('plant', false, 'this Elementor keeps no style-cache entries for the look to leave');
    }
    $seen = rt_view_page($pid, !$noStyles);
    $c->ck('view', $seen['threw'] === '' && $seen['preview'] && $seen['queried'] === $pid, $seen['threw'] !== '' ? 'the preview threw ' . $seen['threw'] : 'the preview of page ' . $pid . ' resolved to ' . ($seen['preview'] ? 'post ' . $seen['queried'] : 'no preview'));
    $otherBefore = [];
    if ($cacheClass === null) {
        rt_out(sprintf('note [%s] this Elementor keeps no style-cache entries: the page is previewed and no entry is checked', $tag));
    } else {
        // The control: a look that left no entry for the page would make the edit below one of a page nobody looked at.
        $held = rt_style_cache_of($pid);
        if ($c->ck('viewed', $held !== [], 'the preview left no style-cache entry for page ' . $pid)) {
            rt_out(sprintf('note [%s] the preview left style-cache entries for page %d in: %s', $tag, $pid, implode(', ', array_map(static fn (string $option): string => substr($option, strlen(RT_STYLE_CACHE_PREFIX)), array_keys($held)))));
        }
        // The second page's entries, made through Elementor's own store.
        $store = new $cacheClass();
        $store->validate(['local', (string) $other, 'preview'], 'rt-other');
        $store->validate(['global', (string) $other, 'preview'], 'rt-other');
        $store->validate(['component-styles-related-posts', (string) $other], []);
        $otherBefore = rt_style_cache_of($other);
        $c->ck('other-seeded', isset($otherBefore[RT_STYLE_CACHE_PREFIX . 'local']), 'the second page holds no entry in the local style cache after it was stored');
    }

    // The edit, prechecked and written as the router calls it.
    [$ops, $refWhy] = rt_resolve_refs($spec['ops'], rt_stored_tree($pid)[1] ?? []);
    if (!$c->ck('refs', $ops !== null, $refWhy)) {
        return [$c, $made];
    }
    $fp = BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS);
    try {
        $input = json_encode(['post_id' => $pid, 'base_fingerprint' => (string) $fp, 'operations' => $ops], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    } catch (JsonException $e) {
        rt_broken('the operations of the edit after a view cannot be encoded');
    }
    $rid  = wp_generate_uuid4();
    $more = ['input' => $input, 'allowed_draft_ids' => [$pid]];
    $pre  = rt_call($cmd, $editEntry, 'precheck', $rid, $more);
    if (!$c->ck('precheck', ($pre['ok'] ?? false) === true && ($pre['outcome'] ?? null) === 'prechecked', rt_brief($pre))) {
        return [$c, $made];
    }
    $preview = is_array($pre['preview'] ?? null) ? $pre['preview'] : [];
    $tree    = $preview['tree'] ?? null;
    $c->ck('preview-tree', is_array($tree) && $tree !== [], 'the precheck previewed no tree');

    // The plant: while the page is saved, the style-cache entry of the second page is cleared.
    $planted = in_array('edit_view_other_cache', $kinds, true);
    $plant   = null;
    if ($planted && $cacheClass !== null) {
        $plant = static function () use ($cacheClass, $other): void {
            (new $cacheClass())->invalidate(['local', (string) $other]);
        };
        add_action('elementor/document/after_save', $plant, 30);
    }
    try {
        $w = rt_call($cmd, $editEntry, 'write', $rid, $more + ['expected' => rt_digests($pre)]);
    } finally {
        if ($plant !== null) {
            remove_action('elementor/document/after_save', $plant, 30);
        }
    }
    $answered = 'the write was answered ok=' . rt_brief($w['ok'] ?? null) . ' outcome=' . rt_brief($w['outcome'] ?? null) . ' code=' . rt_brief($w['code'] ?? null) . ' detail=' . rt_brief($w['detail'] ?? null);
    if ($c->ck('write', ($w['ok'] ?? false) === true && ($w['outcome'] ?? null) === 'applied', $answered)) {
        [$rows, $decoded] = rt_stored_tree($pid);
        $expect           = $stored === 'strings' && is_array($tree) ? rt_scalars_as_strings($tree) : $tree;
        $d                = $decoded !== null && is_array($expect) ? rt_diff($expect, $decoded, 'tree') : 'expected one _elementor_data row holding a tree, found ' . $rows;
        $c->ck('stored-tree', $d === null, (string) $d);
        if ($cacheClass !== null) {
            $c->ck('other-entries', rt_style_cache_of($other) === $otherBefore, 'the edit changed the style-cache entries of the second page');
        }
    }
    if ($planted) {
        $c->ck('plant', $cacheClass !== null && !isset(rt_style_cache_of($other)[RT_STYLE_CACHE_PREFIX . 'local']), $cacheClass === null ? 'this Elementor keeps no style-cache entries to clear' : 'the plant cleared nothing: the second page still holds its entry');
    }

    return [$c, $made];
}

/**
 * Run the agent's edit path on this site: every edit case that names the layout
 * of this boot, then the full restore and the edit after a view. See the steps 8
 * to 10, the restore and the edit after a view in the file header.
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
    $specs     = [];
    $restore   = null;
    $afterView = null;
    $media     = [];
    foreach (['/rt/fixtures/elementor-edit-cases.json', '/rt/harness/edit-cases-extra.json'] as $file) {
        $loaded = rt_load($file);
        $shared = strpos($file, '/rt/fixtures/') === 0;
        if ($shared) {
            $media = is_array($loaded['arrays']['media'] ?? null) ? $loaded['arrays']['media'] : [];
        } else {
            $restore   = is_array($loaded['arrays']['restore'] ?? null) ? $loaded['arrays']['restore'] : null;
            $afterView = is_array($loaded['arrays']['after_view'] ?? null) ? $loaded['arrays']['after_view'] : null;
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
                'golden' => $shared ? ['request_id' => (string) ($case['request_id'] ?? ''), 'after' => $case['after_tree'], 'changes' => is_array($case['changes'] ?? null) ? $case['changes'] : []] : null,
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
            // Elementor stores each element in its own form (a hand-made one is reordered), so the page it
            // holds is held to the case's page by its elements, not by its bytes.
            $was = [];
            $has = [];
            rt_shape($spec['before'], $was);
            rt_shape($before, $has);
            $c->ck('before-shape', $decoded !== null && $was === $has, 'Elementor holds ' . count($has) . ' element(s) of the case\'s ' . count($was) . ', or in another order');
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

                // 10. Render the edited page. A later operation of the batch can take an earlier one off the page,
                // so a case with a golden after tree is held to the operations whose elements that tree still holds,
                // as the fixture's own changes name them (the precheck's new elements have ids of its own request).
                $live  = null;
                $wrote = [];
                if (is_array($spec['golden'])) {
                    $live  = [];
                    $wrote = $spec['golden']['changes'];
                    rt_node_ids($spec['golden']['after'], $live);
                }
                $held = rt_held_to_page($ops, $wrote, $live);
                $gone = [];
                foreach ($changes as $change) {
                    if (is_array($change)) {
                        array_push($gone, ...rt_replaced_texts($change));
                    }
                }
                rt_render_checks($c, $pid, $held['texts'], array_values(array_diff($gone, $held['texts'])), $held['links'], $kind, $held['off']);
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

    // An edit after a view. It runs after the other scenarios, so the look it makes does not touch theirs.
    if (!$noCases && is_array($afterView)) {
        $name = (string) ($afterView['name'] ?? 'after-view');
        [$c, $ids] = rt_edit_after_view($cmd, $createEntry, $editEntry, $afterView, $stored, $plants[$name] ?? [], $tagBase . ' ' . $name);
        foreach ($ids as $id) {
            $made[] = $id;
            rt_trash($id);
        }
        $out['scenarios'][] = 'after-view';
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
// Render probes
// ---------------------------------------------------------------------------

/** The probe file, beside the harness. */
const RT_PROBES_FILE = '/rt/harness/render-probes.json';

/** The layouts a probe may carry a tree for. */
const RT_PROBE_LAYOUTS = ['containers', 'sections'];

/**
 * Split a string at every top-level occurrence of a separator: one that is not
 * inside parentheses, brackets or a quoted string.
 *
 * @return list<string>
 */
function rt_css_split(string $s, string $sep): array
{
    $parts = [];
    $buf   = '';
    $depth = 0;
    $quote = '';
    $len   = strlen($s);
    for ($i = 0; $i < $len; ++$i) {
        $ch = $s[$i];
        if ($quote !== '') {
            $buf .= $ch;
            if ($ch === '\\' && $i + 1 < $len) {
                $buf .= $s[++$i];
            } elseif ($ch === $quote) {
                $quote = '';
            }
            continue;
        }
        if ($ch === '"' || $ch === "'") {
            $quote = $ch;
        } elseif ($ch === '(' || $ch === '[') {
            ++$depth;
        } elseif (($ch === ')' || $ch === ']') && $depth > 0) {
            --$depth;
        } elseif ($ch === $sep && $depth === 0) {
            $parts[] = $buf;
            $buf     = '';
            continue;
        }
        $buf .= $ch;
    }
    $parts[] = $buf;

    return $parts;
}

/** The index of the brace that closes the block opened at $open, or the last index when it never closes. */
function rt_css_block_end(string $css, int $open): int
{
    $len   = strlen($css);
    $depth = 0;
    $quote = '';
    for ($i = $open; $i < $len; ++$i) {
        $ch = $css[$i];
        if ($quote !== '') {
            if ($ch === '\\') {
                ++$i;
            } elseif ($ch === $quote) {
                $quote = '';
            }
            continue;
        }
        if ($ch === '"' || $ch === "'") {
            $quote = $ch;
        } elseif ($ch === '{') {
            ++$depth;
        } elseif ($ch === '}' && --$depth === 0) {
            return $i;
        }
    }

    return $len - 1;
}

/** Comparable form of a media condition: no whitespace, lower case, one pair of enclosing parentheses dropped. */
function rt_css_media_norm(string $media): string
{
    $m = (string) preg_replace('/\s+/', '', strtolower($media));
    if ($m !== '' && $m[0] === '(' && substr($m, -1) === ')' && strpbrk(substr($m, 1, -1), '()') === false) {
        $m = substr($m, 1, -1);
    }

    return $m;
}

/** Comparable form of a declaration value: lower case, single spaces, none around commas, colons or parentheses. */
function rt_css_value_norm(string $v): string
{
    $v = (string) preg_replace('/\s+/', ' ', strtolower(trim($v)));

    return (string) preg_replace('/\s*([,():])\s*/', '$1', $v);
}

/**
 * The style rules of a stylesheet made of plain rules and conditional groups, which is
 * what Elementor writes for a post. A rule carries the media condition it sits in
 * (null outside any), its selectors and its declarations; a property declared twice
 * keeps both values. Blocks of other at-rules (font faces, keyframes) are skipped whole.
 *
 * @return list<array{media:?string,selectors:list<string>,decl:array<string,list<string>>}>
 */
function rt_css_rules(string $css): array
{
    $css   = (string) preg_replace('~/\*.*?\*/~s', '', $css);
    $len   = strlen($css);
    $rules = [];
    $open  = [];
    $buf   = '';
    $i     = 0;
    while ($i < $len) {
        $ch = $css[$i];
        if ($ch === ';' && strpos(ltrim($buf), '@') === 0) {
            $buf = '';
            ++$i;
            continue;
        }
        if ($ch === '}') {
            array_pop($open);
            $buf = '';
            ++$i;
            continue;
        }
        if ($ch !== '{') {
            $buf .= $ch;
            ++$i;
            continue;
        }
        $prelude = trim($buf);
        $buf     = '';
        $end     = rt_css_block_end($css, $i);
        if (strpos($prelude, '@') === 0) {
            if (preg_match('/^@(media|supports|layer|container)\b/i', $prelude) === 1) {
                $open[] = stripos($prelude, '@media') === 0 ? rt_css_media_norm(substr($prelude, 6)) : '';
                ++$i;
                continue;
            }
            $i = $end + 1;
            continue;
        }
        $decl = [];
        foreach (rt_css_split(substr($css, $i + 1, $end - $i - 1), ';') as $d) {
            $colon = strpos($d, ':');
            $prop  = $colon === false ? '' : strtolower(trim(substr($d, 0, $colon)));
            if ($prop !== '') {
                $decl[$prop][] = rt_css_value_norm(substr($d, (int) $colon + 1));
            }
        }
        $media = implode(' ', array_filter($open, static fn (string $m): bool => $m !== ''));
        if ($prelude !== '') {
            $rules[] = ['media' => $media === '' ? null : $media, 'selectors' => array_map('trim', rt_css_split($prelude, ',')), 'decl' => $decl];
        }
        $i = $end + 1;
    }

    return $rules;
}

/** Whether one selector of a rule is about the element, and carries $sel after the element's class. */
function rt_css_selects(string $selector, string $el, string $sel): bool
{
    if (preg_match('/\.elementor-element-' . preg_quote($el, '/') . '(?![A-Za-z0-9_-])/', $selector, $m, PREG_OFFSET_CAPTURE) !== 1) {
        return false;
    }

    return $sel === '' || strpos(substr($selector, $m[0][1] + strlen($m[0][0])), $sel) !== false;
}

/**
 * Hold the rules of an element to a set of declarations.
 *
 * @param list<array{media:?string,selectors:list<string>,decl:array<string,list<string>>}> $rules
 * @param array<string,string> $decl     Property => value, every one of which a rule of the element must declare.
 * @param ?string              $media    The media condition they must sit in; null for none.
 * @param bool                 $anyMedia True to accept them in any media condition.
 * @return array{0:list<string>,1:list<string>} The pairs that are declared, and the pairs that are not (each with what the element does carry).
 */
function rt_css_hold(array $rules, string $el, string $sel, ?string $media, bool $anyMedia, array $decl): array
{
    $here  = [];
    $other = [];
    foreach ($rules as $rule) {
        $about = false;
        foreach ($rule['selectors'] as $s) {
            if (rt_css_selects($s, $el, $sel)) {
                $about = true;
                break;
            }
        }
        if (!$about) {
            continue;
        }
        $inside = $anyMedia || $rule['media'] === $media;
        foreach ($rule['decl'] as $prop => $values) {
            foreach ($values as $v) {
                if ($inside) {
                    $here[$prop][] = $v;
                } else {
                    $other[$prop][] = '[' . ($rule['media'] ?? 'no media') . '] ' . $v;
                }
            }
        }
    }
    $held = [];
    $miss = [];
    foreach ($decl as $prop => $want) {
        $p    = strtolower((string) $prop);
        $pair = $prop . ':' . $want;
        if (in_array(rt_css_value_norm((string) $want), $here[$p] ?? [], true)) {
            $held[] = $pair;
            continue;
        }
        $has = isset($here[$p]) ? 'has ' . implode(' | ', array_values(array_unique($here[$p]))) : 'has no ' . $prop;
        if (isset($other[$p])) {
            $has .= ', elsewhere ' . implode(' | ', array_values(array_unique($other[$p])));
        }
        $miss[] = $pair . ' (' . $has . ')';
    }

    return [$held, $miss];
}

/**
 * Hold the rules to one expectation of a probe: its "decl" in full, or one of its "any" alternatives in full.
 *
 * @param list<array{media:?string,selectors:list<string>,decl:array<string,list<string>>}> $rules
 * @param array<string,mixed> $exp
 * @return array{0:bool,1:string,2:int} Whether it holds, why not, and the number of the alternative that held.
 */
function rt_css_expect(array $rules, array $exp): array
{
    $alts  = is_array($exp['any'] ?? null) ? $exp['any'] : [$exp['decl']];
    $media = isset($exp['media']) && is_string($exp['media']) ? rt_css_media_norm($exp['media']) : null;
    $why   = [];
    foreach (array_values($alts) as $n => $decl) {
        [, $miss] = rt_css_hold($rules, (string) $exp['el'], (string) ($exp['sel'] ?? ''), $media, false, $decl);
        if ($miss === []) {
            return [true, '', $n];
        }
        $why[] = (count($alts) > 1 ? 'alternative ' . ($n + 1) . ': ' : '') . implode('; ', $miss);
    }

    return [false, implode(' || ', $why), -1];
}

/**
 * How a css expectation names what it is about in a failure line: the element and the part of its selector.
 *
 * @param array<string,mixed> $exp
 */
function rt_css_label(array $exp): string
{
    $sel = isset($exp['sel']) && is_string($exp['sel']) && $exp['sel'] !== '' ? ' ' . $exp['sel'] : '';
    $at  = isset($exp['media']) && is_string($exp['media']) ? ' @media ' . $exp['media'] : '';

    return 'element ' . (string) $exp['el'] . $sel . $at;
}

/**
 * Check the shape of one css expectation of a probe, or say the probe file cannot be used.
 *
 * @param mixed $exp
 */
function rt_probe_check_expectation($exp, string $where, bool $absent): void
{
    if (!is_array($exp) || array_is_list($exp)) {
        rt_broken($where . ' is not an object');
    }
    if (!is_string($exp['el'] ?? null) || preg_match('/^[a-z0-9]{1,16}$/D', $exp['el']) !== 1) {
        rt_broken($where . ' names no element ("el": the id of a node of the tree)');
    }
    if (isset($exp['sel']) && !is_string($exp['sel'])) {
        rt_broken($where . ' has a "sel" that is not a string');
    }
    if (array_key_exists('media', $exp) && $exp['media'] !== null && !is_string($exp['media'])) {
        rt_broken($where . ' has a "media" that is neither null nor a string');
    }
    if (isset($exp['any'])) {
        if ($absent || isset($exp['decl']) || !is_array($exp['any']) || !array_is_list($exp['any']) || $exp['any'] === []) {
            rt_broken($where . ' must carry either "decl" or a non-empty list "any" (a css_absent entry carries "decl" only)');
        }
        $maps = $exp['any'];
    } else {
        $maps = [$exp['decl'] ?? null];
    }
    foreach ($maps as $map) {
        if (!is_array($map) || $map === [] || array_is_list($map)) {
            rt_broken($where . ' declares nothing');
        }
        foreach ($map as $prop => $value) {
            if (!is_string($prop) || $prop === '' || !is_string($value) || $value === '') {
                rt_broken($where . ' has a declaration that is not a property with a string value');
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Render probes: the elements, texts, widgets and fonts of the page
// ---------------------------------------------------------------------------

/**
 * XPath for a probe selector: tag names or "*", ".class", "#id" and "[attr]", "[attr=v]",
 * "[attr~=v]", "[attr^=v]", "[attr$=v]", "[attr*=v]" (v bare, or quoted without a quote inside),
 * joined by whitespace (descendant) or ">" (child), and alternatives joined by ",". Any other
 * syntax is a probe file that cannot be used, never a selector that quietly matches nothing.
 */
function rt_sel_xpath(string $selector): string
{
    $paths = [];
    foreach (rt_css_split($selector, ',') as $alt) {
        $alt = trim($alt);
        $len = strlen($alt);
        $xp  = '';
        $op  = '//';
        $pos = 0;
        if ($len === 0) {
            rt_broken('a probe selector has an empty alternative: ' . rt_brief($selector));
        }
        while ($pos < $len) {
            if ($xp !== '' && preg_match('/\G\s*>\s*/', $alt, $m, 0, $pos) === 1 && $m[0] !== '') {
                $op   = '/';
                $pos += strlen($m[0]);
                continue;
            }
            if ($xp !== '' && preg_match('/\G\s+/', $alt, $m, 0, $pos) === 1) {
                $op   = '//';
                $pos += strlen($m[0]);
                continue;
            }
            $compound = '/\G(\*|[A-Za-z][A-Za-z0-9-]*)?((?:[.#][A-Za-z0-9_-]+|\[[A-Za-z_][A-Za-z0-9_:.-]*(?:[~^$*]?=(?:"[^"\']*"|\'[^"\']*\'|[^\]\s"\']+))?\])*)/';
            if (preg_match($compound, $alt, $m, 0, $pos) !== 1 || $m[0] === '') {
                rt_broken('a probe selector uses syntax the harness does not read, at ' . rt_brief(substr($alt, $pos)) . ' of ' . rt_brief($selector));
            }
            $pos += strlen($m[0]);
            $pred = '';
            preg_match_all('/([.#])([A-Za-z0-9_-]+)|\[([A-Za-z_][A-Za-z0-9_:.-]*)(?:([~^$*]?)=("[^"\']*"|\'[^"\']*\'|[^\]\s"\']+))?\]/', $m[2], $parts, PREG_SET_ORDER);
            foreach ($parts as $p) {
                if (($p[1] ?? '') === '.') {
                    $pred .= "[contains(concat(' ',normalize-space(@class),' '),' " . $p[2] . " ')]";
                } elseif (($p[1] ?? '') === '#') {
                    $pred .= "[@id='" . $p[2] . "']";
                } else {
                    $name = $p[3];
                    if (!isset($p[5])) {
                        $pred .= '[@' . $name . ']';
                        continue;
                    }
                    $v = trim($p[5], '"\'');
                    switch ($p[4] ?? '') {
                        case '~':
                            $pred .= "[contains(concat(' ',normalize-space(@" . $name . "),' '),' " . $v . " ')]";
                            break;
                        case '^':
                            $pred .= '[starts-with(@' . $name . ",'" . $v . "')]";
                            break;
                        case '$':
                            $pred .= '[substring(@' . $name . ',string-length(@' . $name . ")-string-length('" . $v . "')+1)='" . $v . "']";
                            break;
                        case '*':
                            $pred .= '[contains(@' . $name . ",'" . $v . "')]";
                            break;
                        default:
                            $pred .= '[@' . $name . "='" . $v . "']";
                    }
                }
            }
            $xp .= $op . (($m[1] ?? '') === '' ? '*' : $m[1]) . $pred;
        }
        $paths[] = $xp;
    }

    return implode(' | ', $paths);
}

/** A whole HTML document as a DOM, as it is: no wrapper, unlike rt_dom() which holds a fragment. */
function rt_dom_page(string $html): DOMDocument
{
    $dom  = new DOMDocument();
    $prev = libxml_use_internal_errors(true);
    $dom->loadHTML('<?xml encoding="UTF-8">' . $html, LIBXML_NONET);
    libxml_clear_errors();
    libxml_use_internal_errors($prev);

    return $dom;
}

/**
 * Check the page-level members of one probe, or say the probe file cannot be used:
 * html_text, html_has, widgets, fonts, since, theme, render and page.
 *
 * @param array<string,mixed> $probe
 */
function rt_probe_check_page(array $probe, string $at): void
{
    foreach ($probe['html_text'] ?? [] as $t) {
        if (!is_string($t) || trim($t) === '') {
            rt_broken($at . ' has an html text that is not a non-empty string');
        }
    }
    foreach ($probe['html_has'] ?? [] as $n => $want) {
        $w = $at . ' html_has[' . $n . ']';
        if (!is_array($want) || array_is_list($want) || !is_string($want['sel'] ?? null) || trim($want['sel']) === '') {
            rt_broken($w . ' names no selector ("sel")');
        }
        rt_sel_xpath($want['sel']);
        foreach (['count', 'min'] as $k) {
            if (isset($want[$k]) && (!is_int($want[$k]) || $want[$k] < 0)) {
                rt_broken($w . ' has a "' . $k . '" that is not a whole number of 0 or more');
            }
        }
        if (isset($want['count'], $want['min'])) {
            rt_broken($w . ' has both "count" and "min"');
        }
        if (isset($want['text']) && (!is_string($want['text']) || trim($want['text']) === '')) {
            rt_broken($w . ' has a "text" that is not a non-empty string');
        }
    }
    foreach ($probe['widgets'] ?? [] as $type => $want) {
        if (!is_string($type) || $type === '' || !is_bool($want)) {
            rt_broken($at . ' has a widgets entry that is not a widget type with true or false');
        }
    }
    foreach ($probe['fonts'] ?? [] as $family) {
        if (!is_string($family) || preg_match('/^[A-Za-z0-9 ]{1,60}$/D', $family) !== 1) {
            rt_broken($at . ' has a font family that is not letters, digits and spaces');
        }
    }
    if (isset($probe['since']) && (!is_string($probe['since']) || preg_match('/^[0-9]+\.[0-9]+(\.[0-9]+)?$/D', $probe['since']) !== 1)) {
        rt_broken($at . ' has a "since" that is not an Elementor version');
    }
    if (isset($probe['theme']) && (!is_string($probe['theme']) || preg_match('/^[a-z0-9-]{1,60}$/D', $probe['theme']) !== 1)) {
        rt_broken($at . ' has a "theme" that is not a theme folder name');
    }
    foreach ($probe['plants'] ?? [] as $kind => $target) {
        if (!in_array($kind, RT_PROBE_PLANTS, true) || !is_array($target) || array_is_list($target)
            || !is_string($target['el'] ?? null) || preg_match('/^[a-z0-9]{1,16}$/D', $target['el']) !== 1
            || !is_string($target['key'] ?? null) || $target['key'] === '') {
            rt_broken($at . ' has a plant target that is not one of ' . implode(', ', RT_PROBE_PLANTS) . ' with an element id and a settings key');
        }
    }
    if (isset($probe['render']) && !in_array($probe['render'], ['content', 'page'], true)) {
        rt_broken($at . ' has a "render" that is neither content nor page');
    }
    if (isset($probe['page'])) {
        $pg = $probe['page'];
        if (!is_array($pg) || array_is_list($pg) || array_diff(array_keys($pg), ['template', 'settings']) !== []) {
            rt_broken($at . ' has a "page" that is not an object of template and settings');
        }
        if (isset($pg['template']) && !in_array($pg['template'], ['', 'elementor_header_footer', 'elementor_canvas'], true)) {
            rt_broken($at . ' has a page template that is not one of "", elementor_header_footer, elementor_canvas');
        }
        if (isset($pg['settings']) && (!is_array($pg['settings']) || array_is_list($pg['settings']))) {
            rt_broken($at . ' has page settings that are not an object');
        }
    }
}

/**
 * The settings of the node of a tree with an id, by reference, or null.
 *
 * @param array<mixed> $nodes
 * @return ?array<mixed>
 */
function &rt_probe_node(array &$nodes, string $id)
{
    $none = null;
    foreach ($nodes as &$node) {
        if (($node['id'] ?? null) === $id) {
            return $node;
        }
        if (is_array($node['elements'] ?? null)) {
            $found = &rt_probe_node($node['elements'], $id);
            if ($found !== null) {
                return $found;
            }
        }
    }

    return $none;
}

/**
 * The ids of every node of a tree.
 *
 * @param array<mixed> $nodes
 * @return list<string>
 */
function rt_probe_ids(array $nodes): array
{
    $ids = [];
    foreach ($nodes as $node) {
        if (!is_array($node)) {
            continue;
        }
        if (is_string($node['id'] ?? null)) {
            $ids[] = $node['id'];
        }
        if (is_array($node['elements'] ?? null)) {
            $ids = array_merge($ids, rt_probe_ids($node['elements']));
        }
    }

    return $ids;
}

/**
 * The rules of a post stylesheet that are about one of the elements, one line each:
 * the media condition, the selectors and the declarations.
 *
 * @param list<array{media:?string,selectors:list<string>,decl:array<string,list<string>>}> $rules
 * @param list<string> $ids
 * @return list<string>
 */
function rt_probe_dump_rules(array $rules, array $ids): array
{
    $lines = [];
    foreach ($rules as $rule) {
        $about = false;
        foreach ($rule['selectors'] as $s) {
            foreach ($ids as $id) {
                $about = $about || rt_css_selects($s, $id, '');
            }
        }
        if (!$about) {
            continue;
        }
        $decl = [];
        foreach ($rule['decl'] as $prop => $values) {
            $decl[] = $prop . ': ' . implode(' | ', $values);
        }
        $lines[] = '[' . ($rule['media'] ?? 'no media') . '] ' . implode(', ', $rule['selectors']) . ' { ' . implode('; ', $decl) . ' }';
    }

    return $lines;
}

/**
 * Plant a defect in a probe's tree before it is saved: the setting key the probe names for the plant is
 * misspelt (last two letters swapped) or removed. Null when it was applied, else why it could not be: a
 * plant that changed nothing would prove nothing.
 *
 * @param array<mixed>        $tree
 * @param array<string,mixed> $probe
 */
function rt_probe_plant(array &$tree, array $probe, string $kind): ?string
{
    $target = $probe['plants'][$kind] ?? null;
    if (!is_array($target)) {
        return 'the probe names no target for the plant ' . $kind;
    }
    $node = &rt_probe_node($tree, (string) $target['el']);
    if ($node === null || !is_array($node['settings'] ?? null) || !array_key_exists($target['key'], $node['settings'])) {
        return 'the element ' . $target['el'] . ' has no setting ' . $target['key'] . ' to plant ' . $kind . ' on';
    }
    $key = (string) $target['key'];
    if ($kind === 'drop_activator') {
        unset($node['settings'][$key]);

        return null;
    }
    $new = substr($key, 0, -2) . substr($key, -1) . substr($key, -2, 1);
    if ($new === $key) {
        $new .= 'x';
    }
    $settings = [];
    foreach ($node['settings'] as $k => $v) {
        $settings[$k === $key ? $new : $k] = $v;
    }
    $node['settings'] = $settings;

    return null;
}

/**
 * Hold the registry of widget types to a probe's "widgets": a type that must be registered, or one that must not be.
 *
 * @param array<string,mixed> $probe
 */
function rt_probe_widgets(RtChecks $c, array $probe): void
{
    foreach ($probe['widgets'] ?? [] as $type => $want) {
        $have = Plugin::$instance->widgets_manager->get_widget_types((string) $type) !== null;
        $c->ck('widget', $have === $want, 'the widget type ' . $type . ' is ' . ($have ? 'registered' : 'not registered') . ' on this site, the probe says it is ' . ($want ? '' : 'not ') . 'registered');
    }
}

/**
 * Hold the rendered page to a probe's "html_text" (each text shown on the page) and "html_has"
 * (the elements a selector finds: exactly "count" of them, default one, or at least "min", and
 * when "text" is given, one of them shows it).
 *
 * @param array<string,mixed> $probe
 */
function rt_probe_page_checks(RtChecks $c, string $html, array $probe): void
{
    if (($probe['html_text'] ?? []) === [] && ($probe['html_has'] ?? []) === []) {
        return;
    }
    $dom  = (($probe['render'] ?? 'content') === 'page') ? rt_dom_page($html) : rt_dom($html);
    $page = rt_norm((string) $dom->textContent);
    foreach ($probe['html_text'] ?? [] as $t) {
        $c->ck('html-text', strpos($page, rt_norm($t)) !== false, 'the rendered page does not show the text ' . rt_brief($t));
    }
    $xpath = new DOMXPath($dom);
    foreach ($probe['html_has'] ?? [] as $n => $want) {
        $nodes = $xpath->query(rt_sel_xpath($want['sel']));
        $found = $nodes === false ? 0 : $nodes->length;
        $label = 'html_has[' . $n . '] ' . $want['sel'];
        if (isset($want['min'])) {
            $held = $found >= $want['min'];
            $why  = $label . ' matches ' . $found . ' element(s), wanted at least ' . $want['min'];
        } else {
            $count = $want['count'] ?? 1;
            $held  = $found === $count;
            $why   = $label . ' matches ' . $found . ' element(s), wanted ' . $count;
        }
        if (!$c->ck('html-has', $held, $why) || !isset($want['text'])) {
            continue;
        }
        $shown = false;
        foreach ($nodes ?: [] as $node) {
            if (strpos(rt_norm((string) $node->textContent), rt_norm($want['text'])) !== false) {
                $shown = true;
                break;
            }
        }
        $c->ck('html-has', $shown, $label . ' shows no element with the text ' . rt_brief($want['text']));
    }
}

/**
 * The font families Elementor puts on the page for a post: the post's stylesheet is enqueued the way
 * the front end enqueues it, then the fonts it asked for are printed, and the styles queued for
 * Google's font service are read back from the style registry.
 *
 * @return list<string> The sources of the queued font stylesheets.
 */
function rt_font_sources(int $postId): array
{
    RtPostCss::create($postId)->enqueue();
    ob_start();
    try {
        Plugin::$instance->frontend->print_fonts_links();
    } finally {
        ob_end_clean();
    }
    $out = [];
    $reg = wp_styles();
    foreach ($reg->queue as $handle) {
        $src = $reg->registered[$handle]->src ?? '';
        if (is_string($src) && stripos($src, 'fonts.googleapis.com') !== false) {
            $out[] = $src;
        }
    }

    return $out;
}

/**
 * Make a probe's page what the probe says before it is rendered as a visitor sees it: published,
 * with its template and its page settings written as plain post meta (never through the
 * document's own settings save, which replaces the settings wholesale).
 *
 * @param array<string,mixed> $probe
 */
function rt_probe_page_setup(int $postId, array $probe, int $principal): void
{
    $page = $probe['page'] ?? [];
    if (isset($page['template'])) {
        update_post_meta($postId, '_wp_page_template', $page['template']);
    }
    if (isset($page['settings'])) {
        update_post_meta($postId, '_elementor_page_settings', $page['settings']);
    }
    wp_set_current_user(1);
    wp_update_post(['ID' => $postId, 'post_status' => 'publish']);
    wp_set_current_user($principal);
}

/**
 * The page a visitor gets for a published page: the main query for it, then the theme's template
 * as WordPress loads it, in an output buffer. What the request sets up is put back afterwards.
 */
function rt_front_page(int $postId): string
{
    $get   = $_GET;
    $query = $_SERVER['QUERY_STRING'] ?? null;
    $uri   = $_SERVER['REQUEST_URI'] ?? null;
    $level = ob_get_level();
    $html  = '';
    $seen  = 0;
    wp_set_current_user(0);
    $_GET['page_id']         = (string) $postId;
    $_SERVER['QUERY_STRING'] = 'page_id=' . $postId;
    $_SERVER['REQUEST_URI']  = '/?page_id=' . $postId;
    add_filter('wp_using_themes', '__return_true');
    remove_action('template_redirect', 'redirect_canonical');
    remove_action('template_redirect', 'wp_old_slug_redirect');
    ob_start();
    try {
        wp();
        $seen = (int) get_queried_object_id();
        if ($seen === $postId) {
            require ABSPATH . WPINC . '/template-loader.php';
        }
    } finally {
        while (ob_get_level() > $level) {
            $html = (string) ob_get_clean() . $html;
        }
        remove_filter('wp_using_themes', '__return_true');
        add_action('template_redirect', 'redirect_canonical');
        add_action('template_redirect', 'wp_old_slug_redirect');
        $_GET = $get;
        foreach (['QUERY_STRING' => $query, 'REQUEST_URI' => $uri] as $key => $was) {
            if ($was === null) {
                unset($_SERVER[$key]);
            } else {
                $_SERVER[$key] = $was;
            }
        }
        $GLOBALS['wp_query']     = new WP_Query();
        $GLOBALS['wp_the_query'] = $GLOBALS['wp_query'];
        $GLOBALS['post']         = null;
        setlocale(LC_NUMERIC, 'C');
    }
    if ($seen !== $postId) {
        throw new RuntimeException('the main query holds ' . $seen . ', not page ' . $postId);
    }

    return $html;
}

/**
 * The probe file, checked: a file that cannot be used is a run that cannot give a verdict.
 *
 * @return list<array<string,mixed>>
 */
function rt_probe_load(): array
{
    $raw = is_file(RT_PROBES_FILE) ? file_get_contents(RT_PROBES_FILE) : false;
    if (!is_string($raw) || $raw === '') {
        rt_broken('probe file missing or empty: ' . RT_PROBES_FILE);
    }
    try {
        $data = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
    } catch (JsonException $e) {
        rt_broken('probe file is not JSON: ' . RT_PROBES_FILE);
    }
    if (!is_array($data) || !is_array($data['probes'] ?? null) || !array_is_list($data['probes']) || $data['probes'] === []) {
        rt_broken('probe file holds no probes');
    }
    $seen = [];
    foreach ($data['probes'] as $i => $probe) {
        $at = 'probe #' . $i;
        if (!is_array($probe) || !is_string($probe['name'] ?? null) || preg_match('/^[a-z0-9]+(-[a-z0-9]+)*$/D', $probe['name']) !== 1) {
            rt_broken($at . ' has no name (lower case words joined by hyphens)');
        }
        $at = 'probe ' . $probe['name'];
        if (isset($seen[$probe['name']])) {
            rt_broken($at . ' is listed twice');
        }
        $seen[$probe['name']] = true;
        if (!is_string($probe['about'] ?? null) || trim($probe['about']) === '') {
            rt_broken($at . ' says nothing about what it proves ("about")');
        }
        if (!is_array($probe['trees'] ?? null) || $probe['trees'] === [] || array_is_list($probe['trees'])) {
            rt_broken($at . ' has no "trees" (an object of raw Elementor trees by layout)');
        }
        foreach ($probe['trees'] as $layout => $tree) {
            if (!in_array($layout, RT_PROBE_LAYOUTS, true) || !is_array($tree) || $tree === [] || !array_is_list($tree)) {
                rt_broken($at . ' has a tree for "' . $layout . '" that is not a non-empty list of elements for containers or sections');
            }
        }
        // The positive control: a probe that expects nothing to be styled looks at nothing, and its absences prove nothing.
        if (!is_array($probe['css'] ?? null) || !array_is_list($probe['css']) || $probe['css'] === []) {
            rt_broken($at . ' expects no declaration ("css" must list at least one)');
        }
        foreach ($probe['css'] as $n => $exp) {
            rt_probe_check_expectation($exp, $at . ' css[' . $n . ']', false);
        }
        foreach ($probe['css_absent'] ?? [] as $n => $exp) {
            rt_probe_check_expectation($exp, $at . ' css_absent[' . $n . ']', true);
        }
        foreach ($probe['html_ids'] ?? [] as $id) {
            if (!is_string($id) || $id === '') {
                rt_broken($at . ' has an html id that is not a string');
            }
        }
        foreach ($probe['html_tags'] ?? [] as $id => $tag) {
            if (!is_string($id) || !is_string($tag) || $tag === '') {
                rt_broken($at . ' has an html tag that is not a string');
            }
        }
        rt_probe_check_page($probe, $at);
    }

    return $data['probes'];
}

/**
 * Run the render probes of this layout: each probe's raw tree is saved with Elementor's
 * own document save as the agent's service user, read back, rendered, and the page and
 * the post CSS file Elementor wrote for it are held to what the probe expects. A key
 * Elementor ignores shows as a declaration that is not there.
 *
 * @param list<array<string,mixed>> $probes  The checked probes, from rt_probe_load().
 * @param bool                      $noCases Plant: hand the phase no probe at all.
 * @return array{probes:int,failed:int,checks:int}
 */
function rt_probe_phase(array $probes, string $layout, string $stored, int $principal, bool $noCases): array
{
    $out = ['probes' => 0, 'failed' => 0, 'checks' => 0];
    if (!class_exists(RtPostCss::class)) {
        rt_broken('Elementor has no post CSS class to read the probes\' style sheets with');
    }
    foreach ($noCases ? [] : $probes as $probe) {
        $tree = $probe['trees'][$layout] ?? null;
        if (!is_array($tree)) {
            continue;
        }
        $tag = ELEMENTOR_VERSION . ' ' . $layout . ' probe-' . $probe['name'];
        // A probe about a release's behaviour is skipped, out loud, on the older releases it does not apply to.
        if (isset($probe['since']) && version_compare(ELEMENTOR_VERSION, $probe['since'], '<')) {
            rt_out(sprintf('note [%s] skipped: the probe applies from Elementor %s', $tag, $probe['since']));
            continue;
        }
        ++$out['probes'];
        $c   = new RtChecks();
        foreach ($GLOBALS['rtPlants'][$probe['name']] ?? [] as $kind) {
            if (in_array($kind, RT_PROBE_PLANTS, true)) {
                $why = rt_probe_plant($tree, $probe, $kind);
                $c->ck('plant', $why === null, (string) $why);
            }
        }
        $pid = wp_insert_post([
            'post_type'    => 'page',
            'post_status'  => 'draft',
            'post_title'   => 'probe ' . $probe['name'],
            'post_content' => '',
            'post_author'  => $principal,
            'meta_input'   => ['_elementor_edit_mode' => 'builder', '_elementor_template_type' => 'wp-page'],
        ], true);
        if ($c->ck('draft-insert', is_int($pid) && $pid > 0, is_wp_error($pid) ? $pid->get_error_message() : 'no id') && $c->ck('save', rt_save_tree($pid, $tree, $principal), 'Elementor did not save the probe\'s tree')) {
            // What Elementor stored is the tree that was saved.
            [$rows, $decoded] = rt_stored_tree($pid);
            $expect           = $stored === 'strings' ? rt_scalars_as_strings($tree) : $tree;
            $d                = $decoded !== null ? rt_diff($expect, $decoded, 'tree') : 'expected one _elementor_data row holding a tree, found ' . $rows;
            $c->ck('stored', $d === null, (string) $d);
            rt_probe_widgets($c, $probe);
            $onPage = ($probe['render'] ?? 'content') === 'page';
            if (isset($probe['theme'])) {
                $c->ck('theme', get_stylesheet() === $probe['theme'] && wp_is_block_theme(), 'the active theme is ' . get_stylesheet() . (wp_is_block_theme() ? ' (a block theme)' : ' (not a block theme)') . ', the probe needs the block theme ' . $probe['theme']);
            }
            if ($onPage) {
                rt_probe_page_setup($pid, $probe, $principal);
            }

            // Render, which is what makes Elementor write the post's CSS file.
            try {
                $html = $onPage ? rt_front_page($pid) : Plugin::$instance->frontend->get_builder_content($pid, false);
            } catch (Throwable $e) {
                $html = '';
                $c->ck('render', false, 'render threw ' . get_class($e) . ': ' . $e->getMessage());
            }
            setlocale(LC_NUMERIC, 'C');
            // Authoring aid, not a check: dump=<probe> prints the stored tree and the page a probe rendered.
            if (($GLOBALS['rtArgs']['dump'] ?? '') === $probe['name']) {
                rt_out('dump-stored ' . (string) json_encode($decoded, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
                // A whole page is dumped from its body, since the head holds the scripts and styles of the site.
                $from = strpos((string) $html, '<body');
                foreach (explode("\n", substr($from === false ? (string) $html : substr((string) $html, $from), 0, 20000)) as $dumped) {
                    rt_out('dump ' . $dumped);
                }
            }
            if ($c->ck('render-nonempty', is_string($html) && trim($html) !== '', 'the probe rendered nothing')) {
                $ids  = [];
                $tags = [];
                foreach (rt_dom($html)->getElementsByTagName('*') as $el) {
                    $id = $el->getAttribute('id');
                    if ($id !== '') {
                        $ids[$id]  = ($ids[$id] ?? 0) + 1;
                        $tags[$id] = strtolower($el->nodeName);
                    }
                }
                foreach ($probe['html_ids'] ?? [] as $id) {
                    $c->ck('html-id', ($ids[$id] ?? 0) === 1, 'the rendered page carries id="' . $id . '" on ' . ($ids[$id] ?? 0) . ' element(s), not one');
                }
                foreach ($probe['html_tags'] ?? [] as $id => $want) {
                    $c->ck('html-tag', ($tags[$id] ?? null) === $want, 'the element with id="' . $id . '" is ' . ($tags[$id] ?? '(not on the page)') . ', not ' . $want);
                }
                rt_probe_page_checks($c, (string) $html, $probe);
            }
            if (($probe['fonts'] ?? []) !== []) {
                $sources = rt_font_sources($pid);
                foreach ($probe['fonts'] as $family) {
                    $hit = false;
                    foreach ($sources as $src) {
                        $hit = $hit || stripos(str_replace(['+', '%20'], ' ', $src), $family) !== false;
                    }
                    $c->ck('font', $hit, 'no stylesheet for the family ' . $family . ' is queued for Google\'s font service (queued: ' . ($sources === [] ? 'none' : implode(' | ', $sources)) . ')');
                }
            }

            // The post CSS file: the stylesheet the page is served with.
            $file = RtPostCss::create($pid);
            $path = (string) $file->get_path();
            if (!is_file($path)) {
                $file->update();
                clearstatcache();
            }
            $css = is_file($path) ? (string) file_get_contents($path) : '';
            if ($c->ck('css-file', $css !== '', 'no post CSS file at ' . $path . ', or it is empty')) {
                $rules = rt_css_rules($css);
                if ($c->ck('css-rules', $rules !== [], 'the post CSS file holds no rule')) {
                    // Authoring aid, not a check: dumpcss=<probe name, or its first words> prints each rule of the post CSS that is about an element of the tree.
                    $dumpCss = (string) ($GLOBALS['rtArgs']['dumpcss'] ?? '');
                    if ($dumpCss !== '' && strpos($probe['name'], $dumpCss) === 0) {
                        foreach (rt_probe_dump_rules($rules, rt_probe_ids($tree)) as $dumped) {
                            rt_out('dumpcss ' . $probe['name'] . ' ' . $dumped);
                        }
                    }
                    foreach ($probe['css'] as $n => $exp) {
                        [$ok, $why, $alt] = rt_css_expect($rules, $exp);
                        $c->ck('css', $ok, 'css[' . $n . '] ' . rt_css_label($exp) . ': want ' . $why);
                        if ($ok && is_array($exp['any'] ?? null) && $alt > 0) {
                            rt_out(sprintf('note [%s] css[%d] %s holds by alternative %d of %d', $tag, $n, $exp['el'], $alt + 1, count($exp['any'])));
                        }
                    }
                    foreach ($probe['css_absent'] ?? [] as $n => $exp) {
                        [$held] = rt_css_hold($rules, (string) $exp['el'], (string) ($exp['sel'] ?? ''), null, true, $exp['decl']);
                        $c->ck('css-absent', $held === [], 'css_absent[' . $n . '] ' . rt_css_label($exp) . ' is styled after all: ' . implode(', ', $held));
                    }
                }
            }
        }
        $out['checks'] += $c->count;
        if ($c->fails === []) {
            rt_out(sprintf('ok   [%s] checks=%d', $tag, $c->count));
        } else {
            ++$out['failed'];
            foreach ($c->fails as $f) {
                rt_out(sprintf('FAIL [%s] %s', $tag, $f));
            }
            rt_out(sprintf('note [%s] about: %s', $tag, rt_brief($probe['about'])));
        }
        if (is_int($pid)) {
            rt_trash($pid);
        }
    }
    // A plant that names no probe would change nothing and prove nothing.
    $known = array_column($probes, 'name');
    foreach ($GLOBALS['rtPlants'] ?? [] as $name => $kinds) {
        foreach ($kinds as $kind) {
            if (in_array($kind, RT_PROBE_PLANTS, true) && !in_array((string) $name, $known, true)) {
                ++$out['failed'];
                rt_out(sprintf('FAIL [%s %s probe-%s] plant: no probe is named %s', ELEMENTOR_VERSION, $layout, $name, $name));
            }
        }
    }

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

// The render probes are read and checked before anything runs: a probe file that cannot be used is no verdict.
$rtProbeSet = rt_probe_load();

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
// The images the designed golden names become attachments too: every image a case resolved, with the library alt text
// the golden declares for it (an image the golden declares none for has none).
$rtDesignedGolden = rt_load(RT_DESIGNED_FILE)['arrays'];
$rtLibrary        = $rtDesignedGolden['library_alt'] ?? null;
if (!is_array($rtLibrary) || $rtLibrary === []) {
    rt_broken('the designed golden declares no library images');
}
$rtDesignedImages = [];
foreach ($rtDesignedGolden['cases'] as $rtCase) {
    foreach ((array) ($rtCase['facts']['media'] ?? []) as $rtImage) {
        $rtDesignedImages[(int) ($rtImage['id'] ?? 0)] = (string) ($rtLibrary[(string) ($rtImage['id'] ?? 0)] ?? '');
    }
}
foreach ($rtLibrary as $mid => $alt) {
    $rtDesignedImages[(int) $mid] = (string) $alt;
}
unset($rtDesignedImages[0]);
ksort($rtDesignedImages);
foreach ($rtDesignedImages as $mid => $alt) {
    $up   = wp_upload_dir();
    $file = $up['path'] . '/rt-designed-' . $mid . '.png';
    $gd   = imagecreatetruecolor(800, 450);
    imagefill($gd, 0, 0, (int) imagecolorallocate($gd, 200, 120, 30));
    imagepng($gd, $file);
    imagedestroy($gd);
    $att = wp_insert_attachment(['import_id' => $mid, 'post_mime_type' => 'image/png', 'post_title' => 'rt designed ' . $mid, 'post_content' => '', 'post_status' => 'inherit'], $file, 0, true);
    if (is_wp_error($att) || (int) $att !== $mid) {
        rt_broken('attachment ' . $mid . ' cannot be created with that id: ' . (is_wp_error($att) ? $att->get_error_message() : 'got id ' . (int) $att));
    }
    wp_update_attachment_metadata($mid, wp_generate_attachment_metadata($mid, $file));
    update_post_meta($mid, '_wp_attachment_image_alt', (string) $alt);
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

// The agent's create path for designed pages, on the same site.
$rtDesigned = rt_designed_phase($rtArgs['layout'], $rtArgs['stored'], $principal, in_array('designed_no_cases', $rtPlants['all'] ?? [], true));
$rtChecks  += $rtDesigned['checks'];
$rtFailed  += $rtDesigned['failed'];

// The agent's edit path and the full restore, on the same site.
$rtEdit    = rt_edit_phase($rtArgs['layout'], $rtArgs['stored'], $principal, $rtPlants, in_array('edit_no_cases', $rtPlants['all'] ?? [], true));
$rtChecks += $rtEdit['checks'];
$rtFailed += $rtEdit['failed'];

// The render probes, on the same site.
$rtProbes  = rt_probe_phase($rtProbeSet, $rtArgs['layout'], $rtArgs['stored'], $principal, in_array('probes_no_cases', $rtPlants['all'] ?? [], true));
$rtChecks += $rtProbes['checks'];
$rtFailed += $rtProbes['failed'];

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
// So must the designed path: a page through it, and on a containers site the key check and the heading colours it holds the pages to.
if ($rtDesigned['cases'] < 1) {
    ++$rtFailed;
    rt_out('FAIL the designed path ran no case');
}
if ($rtArgs['layout'] === 'containers' && ($rtDesigned['keys'] < 1 || $rtDesigned['css'] < 1)) {
    ++$rtFailed;
    rt_out(sprintf('FAIL the designed pages were held to %d settings key(s) and %d heading colour(s)', $rtDesigned['keys'], $rtDesigned['css']));
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
// So must the render probes: a boot that probed nothing proved no render fact.
if ($rtProbes['probes'] < 1) {
    ++$rtFailed;
    rt_out('FAIL the probe phase ran no probe');
}
rt_out(sprintf(
    'SUMMARY elementor=%s layout=%s cases=%d checks=%d texts=%d alts=%d agent=%d refused=%d designed=%d edit=%d scenarios=%d probes=%d probes_failed=%d failed=%d',
    ELEMENTOR_VERSION,
    $rtArgs['layout'],
    $rtCases,
    $rtChecks,
    $rtTextsSeen,
    $rtAltsSeen,
    $rtAgent['cases'],
    count($rtAgent['scenarios']),
    $rtDesigned['cases'],
    $rtEdit['cases'],
    count($rtEdit['scenarios']),
    $rtProbes['probes'],
    $rtProbes['failed'],
    $rtFailed
));
rt_out($rtFailed === 0 ? 'RESULT OK' : 'RESULT FAIL');
exit($rtFailed === 0 ? 0 : 1);
