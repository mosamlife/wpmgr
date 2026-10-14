<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/page-create for a page builder: the precheck, the write and the undo
 * guard of a builder draft. The command keeps idempotency, the claims, the
 * digest re-check and the ledger row; everything between the ledger row and
 * the answer is here.
 *
 * Elementor is the one builder compiled in, and only its classic format is
 * built: elementor_format "site_default" and "classic" build classic, and
 * "atomic" is refused with builder_not_available / atomic_unavailable.
 *
 * precheck() writes nothing. It refuses what the site's Elementor facts
 * refuse, builds the tree with node ids derived from the request id, checks
 * the tree as Elementor would store it, and answers the preview:
 *
 *   {post_type, editor, format, elementor_version, layout, status: "draft",
 *    title, tree} plus "media" (the resolved image facts) when the outline
 *   has an image
 *
 *   preview_digest   = sha256(json_encode([editor, format, elementor_version,
 *                      post_type, "draft", title, tree_json]))
 *   base_fingerprint = PageCreateBuilder::baseFingerprint(post_type, media facts)
 *
 * json_encode with default flags; tree_json is the tree's canonical bytes.
 * The same request, input and site give the same bytes, so the write's
 * re-check matches the approved precheck.
 *
 * An outline version 3 page is built from containers. precheck() refuses it
 * with containers_required, before anything is built, when the site builds
 * sections and columns or cannot say; a plain outline is built there as it
 * always was.
 *
 * An outline version 3 page also has a page frame (pageFrame()), stored as
 * the page template ElementorDocument::pageTemplate() names. The frames a
 * site admits depend on its theme (pageFrames()): a block theme admits only
 * canvas, and a frame it does not admit is refused page_frame_unavailable
 * with the frames it does. precheck() runs again when the write runs, so a
 * frame the site stopped admitting since the approval is refused, never
 * replaced. The precheck answers the frame it resolved and the theme fact
 * in "builder_facts": {block_theme, page_frame}, page_frame null for an
 * outline before version 3. For an outline version 3 it also answers
 * "active_breakpoints", the breakpoints the site has active as the tree was
 * compiled for them: a list in Elementor's own order, [] for none, null when
 * the site could not say (the compile then writes no override). precheck()
 * refuses page_template_unavailable when the site does not offer that
 * template for the post type, since core's post save would put it back to
 * the default on the next update of the post row. The template is a plain
 * row of the insert, never written through the document's settings save,
 * and the read-back requires it to have survived the save. An outline
 * before version 3 gets no template row.
 *
 * write() inserts the draft, records it on the ledger, saves the tree through
 * the adapter and reads the page back. A draft that fails any step after the
 * insert is moved to the trash and the answer says so ("trashed"). A created
 * draft records its builder_document_v1 fingerprint and the revisions its own
 * save made, which is what revertProblem() checks before an undo; a draft
 * whose record of those cannot be written is moved to the trash too, since
 * no undo could check it.
 *
 * The undo of a builder draft also covers the wpmgr/page-edit changes WPMgr
 * made to it afterwards: the control plane names them in the undo's signed
 * parameters (revertChain()), and revertProblem() accepts the draft only
 * when those changes, read from this site's ledger, account for every
 * difference between the draft as created and the draft now.
 */
final class BuilderPageCreate
{
    /** elementor_format: the format the site builds by default. */
    public const FORMAT_SITE_DEFAULT = 'site_default';

    /** The attachment meta holding the media library's alt text. */
    public const ALT_META_KEY = '_wp_attachment_image_alt';

    /** Preview layout of a tree of containers. */
    public const LAYOUT_CONTAINERS = 'containers';

    /** Preview layout of a tree of sections and columns. */
    public const LAYOUT_SECTIONS = 'sections';

    /** Refusal: the page frame asked for cannot be built with the site's theme. */
    public const CODE_FRAME_UNAVAILABLE = 'page_frame_unavailable';

    /**
     * The page frames a block theme shows a designed page in as built:
     * canvas, the page content only. Its own template and full width show
     * neither its header and footer parts nor a hidden title as built.
     */
    public const BLOCK_THEME_FRAMES = ['canvas'];

    /** The reason page_frame_unavailable gives for a block theme. */
    public const REASON_FRAME_BLOCK_THEME = 'the site\'s theme is a block theme, which shows a designed page only as canvas: send canvas or leave page_frame out';

    /** Codes a failed save may answer with; anything else is builder_save_refused. */
    private const SAVE_CODES = [
        ElementorDocument::CODE_REFUSED,
        ElementorDocument::CODE_CRASHED,
        ElementorDocument::CODE_SIDE_EFFECT,
    ];

    /** The fingerprinted rows of a draft, by the builder that made it. */
    private const DESCRIPTOR_KEYS = [
        ElementorAdapter::ID => ElementorDocument::DESCRIPTOR_KEYS,
    ];

    /** Most revisions an undo guard reads from the ledger row. */
    private const MAX_OWN_REVISIONS = 50;

    /** The member of an undo's signed parameters naming the later page-edit changes. */
    public const REVERT_CHAIN = 'chain';

    /** Most page-edit changes an undo's chain names. */
    public const MAX_CHAIN = 100;

    /**
     * created_post_touched detail, with a chain: the named changes do not
     * lead from the draft as created to the draft as it is now.
     */
    public const CHAIN_BROKEN = 'chain_broken';

    /** created_post_touched detail, with a chain: a revision neither the creation nor a named change made. */
    public const FOREIGN_REVISION = 'foreign_revision';

    /** created_post_touched detail, with a chain: someone has an autosave of the draft. */
    public const AUTOSAVE = 'autosave';

    /** created_post_touched detail, with a chain: someone holds the draft's edit lock. */
    public const LOCKED = 'locked';

    /** What an undo with no chain answers for each refusal, as it did before chains. */
    private const UNCHAINED = [
        self::CHAIN_BROKEN     => 'someone edited this draft after it was created',
        self::FOREIGN_REVISION => 'this draft has revisions its creation did not make; open it in WordPress',
        self::AUTOSAVE         => 'someone has unsaved changes to this draft; open it in WordPress',
        self::LOCKED           => 'someone is editing this draft right now',
    ];

    /** A request id in a chain: a lowercase UUID (the $ matches only at the very end). */
    private const RE_CHAIN_ID = '/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/D';

    /** A builder_document_v1 fingerprint. */
    private const RE_FP = '/^[0-9a-f]{64}$/D';

    /**
     * Build and check the page, and answer the preview. Writes nothing.
     *
     * On a refusal only "refusal" carries meaning. Otherwise "doc" is the
     * built document and "title" the post_title bytes the write stores.
     *
     * @param array<string, mixed>             $spec       Validated page-create spec with a builder editor.
     * @param BuilderAdapter                   $a          The resolved adapter.
     * @param array<string, mixed>             $facts      The adapter's site facts (ElementorFacts::collect()).
     * @param list<array<string, mixed>>       $mediaFacts Resolved image facts, in outline order.
     * @param string                           $requestId  The request; node ids derive from it.
     * @return array{refusal?: array<string, mixed>, preview: array<string, mixed>, preview_digest: string, base_fingerprint: string, tree: array<mixed>, doc: NativeDocument|null, title: string, builder_facts?: array{block_theme: bool, page_frame: string|null, active_breakpoints?: list<string>|null}}
     */
    public static function precheck(array $spec, BuilderAdapter $a, array $facts, array $mediaFacts, string $requestId): array
    {
        if (!$a instanceof ElementorAdapter) {
            return self::refusedPrecheck(AdapterStatus::CODE, 'not_compiled');
        }
        $postType = $spec['post_type'] ?? null;
        $editor   = $spec['editor'] ?? null;
        $title    = $spec['title'] ?? null;
        if (!is_string($postType) || !is_string($title) || $editor !== 'builder:' . $a->id()) {
            return self::refusedPrecheck('bad_input', 'the input does not ask for a page built with this page builder');
        }
        $format = self::format($spec['elementor_format'] ?? null);
        if ($format === null) {
            return self::refusedPrecheck('bad_input', 'elementor_format must be site_default, classic or atomic');
        }
        $themeType = $spec['template'] ?? null;
        if ($themeType !== null) {
            $problem = $a->templates()->readinessRefusal($spec['shows_on']);
            if ($problem !== null) {
                return self::refusedPrecheck($problem['code'], $problem['detail']);
            }
        }
        // Theme Builder registers its own library document independently of the page editor's post-type setting.
        $createFacts = $facts;
        if ($themeType !== null) {
            $createFacts['post_types'] = ['elementor_library'];
        }
        $refusal = ElementorFacts::createRefusal($createFacts, $postType, $format);
        if ($refusal !== null) {
            return self::refusedPrecheck($refusal['code'], $refusal['detail']);
        }
        $version = $facts['version'] ?? null;
        if (!is_string($version) || $version === '') {
            return self::refusedPrecheck(AdapterStatus::CODE, 'version_unverified');
        }
        // A designed page (outline version 3) is built from containers; a site
        // that builds sections and columns, or cannot say, is refused before
        // anything is built.
        if (($spec['outline_version'] ?? null) === OutlineV3Schema::VERSION && ($facts['containers'] ?? null) !== true) {
            return self::refusedEnvelope(self::containersRequired());
        }
        $blockTheme = ElementorFacts::blockTheme($facts);
        $frame      = $themeType !== null ? null : self::pageFrame($spec, $blockTheme);
        $template   = is_string($frame) ? ElementorDocument::pageTemplate($frame) : null;
        if ($frame === false || (is_string($frame) && $template === null)) {
            return self::refusedPrecheck('bad_input', 'page_frame must be theme, full_width or canvas, with outline_version 3');
        }
        $unfit = is_string($frame) ? self::frameRefusal($frame, $blockTheme) : null;
        if ($unfit !== null) {
            return self::refusedEnvelope($unfit);
        }
        if ($template !== null) {
            $why = ElementorDocument::pageTemplateProblem($postType, $template);
            if ($why !== null) {
                return self::refusedPrecheck(ElementorDocument::CODE_TEMPLATE_UNAVAILABLE, $why);
            }
        }
        // A design's fonts are judged before anything is built: the closed
        // family pattern first, then Elementor's font registry.
        if (array_key_exists('design', $spec)) {
            $design = $spec['design'];
            $font   = FontRegistry::refusal(is_array($design) ? ($design['fonts'] ?? null) : null, static fn (): ?array => $a->fontFamilies());
            if ($font !== null) {
                return self::refusedEnvelope($font);
            }
        }

        $byId = [];
        foreach ($mediaFacts as $fact) {
            if (!is_array($fact) || PageCreateBuilder::mediaFactProblem($fact) !== null) {
                return self::refusedPrecheck('image_not_available', 'the image facts are incomplete');
            }
            $alt = get_post_meta($fact['id'], self::ALT_META_KEY, true);
            if (is_string($alt)) {
                $fact['alt'] = $alt;
            }
            $byId[$fact['id']] = $fact;
        }
        // A logo's alt text is the one saved with its image in the library.
        if (($spec['outline_version'] ?? null) === OutlineV3Schema::VERSION) {
            $logoAlt = ElementorPatternCompiler::logoAltRefusal($spec['outline'] ?? null, $byId);
            if ($logoAlt !== null) {
                return self::refusedEnvelope($logoAlt);
            }
        }

        try {
            $built = $a->buildCreate($spec, new IdSeed($requestId), $byId);
        } catch (\Throwable $e) {
            return self::refusedPrecheck('bad_input', 'the page could not be built');
        }
        $doc = $built['doc'] ?? null;
        if (!$doc instanceof NativeDocument) {
            return self::refusedPrecheck((string) ($built['code'] ?? 'bad_input'), (string) ($built['detail'] ?? 'the page could not be built'));
        }
        if ($template !== null) {
            // Plain rows of the insert, never the document's settings save:
            // the template, and the page settings a page is created with
            // (written once, here).
            $rows     = [ElementorDocument::KEY_PAGE_TEMPLATE => $template];
            $settings = ElementorDocument::createdPageSettings((string) $frame, self::opensWithHero($spec['outline'] ?? null));
            if ($settings !== null) {
                $rows[ElementorDocument::KEY_PAGE_SETTINGS] = $settings;
            }
            try {
                $doc = new NativeDocument($doc->tree, array_merge($doc->meta, $rows));
            } catch (\Throwable $e) {
                return self::refusedPrecheck('bad_input', 'the page could not be encoded');
            }
        }
        $problem = $a->document()->precheckTree($doc->tree);
        if ($problem !== null) {
            return self::refusedPrecheck($problem['code'], $problem['detail']);
        }

        // The stored title is a fixed function of the plain title the digest
        // binds; the save this principal gets must keep it byte for byte.
        $storedTitle = PageCreateBuilder::storedTitle($title);
        $simTitle    = wp_unslash(apply_filters('title_save_pre', wp_slash($storedTitle))); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- core's own save filter, applied to simulate exactly what wp_insert_post will do
        if ($simTitle !== $storedTitle) {
            return self::refusedPrecheck('sanitiser_changed_new_content', 'the site would change the title on save');
        }

        try {
            $digest = self::previewDigest($editor, $format, $version, $postType, $title, $doc->canonicalBytes, $themeType, $spec['shows_on'] ?? []);
            $base   = PageCreateBuilder::baseFingerprint($postType, $mediaFacts);
        } catch (\JsonException $e) {
            return self::refusedPrecheck('bad_input', 'the page could not be encoded');
        }

        $preview = [
            'post_type'         => $postType,
            'editor'            => $editor,
            'format'            => $format,
            'elementor_version' => $version,
            'layout'            => self::layout($doc->tree),
            'status'            => 'draft',
            'title'             => $title,
            'tree'              => $doc->tree,
        ];
        if ($themeType !== null) {
            $preview['template'] = $themeType;
            $preview['shows_on'] = $spec['shows_on'];
        }
        // Only an outline with an image carries media, as on the block editor.
        if ($mediaFacts !== []) {
            $preview['media'] = array_values($mediaFacts);
        }

        return [
            'preview'          => $preview,
            'preview_digest'   => $digest,
            'base_fingerprint' => $base,
            'tree'             => $doc->tree,
            'doc'              => $doc,
            'title'            => $storedTitle,
            'builder_facts'    => self::builderFacts($spec, $a, $blockTheme, $frame),
        ];
    }

    /**
     * The facts the precheck answers beside the preview: the theme fact and
     * the page frame it resolved, and for an outline version 3 the active
     * breakpoints its tree was compiled for (ElementorAdapter::activeBreakpoints(),
     * the value the compile read), which with the input, the resolved images
     * and the request id are everything that tree is a function of.
     *
     * @param array<string, mixed> $spec       The page-create spec.
     * @param ElementorAdapter     $a          The adapter that built the tree.
     * @param bool                 $blockTheme Whether the site's theme is a block theme.
     * @param string|null          $frame      The resolved page frame, null before outline version 3.
     * @return array{block_theme: bool, page_frame: string|null, active_breakpoints?: list<string>|null}
     */
    private static function builderFacts(array $spec, ElementorAdapter $a, bool $blockTheme, ?string $frame): array
    {
        $facts = ['block_theme' => $blockTheme, 'page_frame' => $frame];
        if (($spec['outline_version'] ?? null) === OutlineV3Schema::VERSION) {
            $facts['active_breakpoints'] = $a->activeBreakpoints();
        }

        return $facts;
    }

    /**
     * The refusal of a designed page on a site that builds sections and columns.
     *
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}
     */
    private static function containersRequired(): array
    {
        return Refusal::make(ElementorClassicMapper::CODE_CONTAINERS_REQUIRED, 'A designed page needs Elementor containers, and this site builds sections and columns.', [[
            'path'   => 'input',
            'key'    => 'outline_version',
            'reason' => 'a designed page needs Elementor containers; turn on the Container feature in Elementor, or send a plain outline without outline_version',
        ]]);
    }

    /**
     * The page frame $spec is built in: its page_frame, or, for an outline
     * version 3 without one, canvas on a block theme and on a classic theme
     * full_width when the outline opens with a hero and theme otherwise.
     * Null for an outline before version 3, whose page gets no template row.
     * False when page_frame is not one of OutlineV3Schema::PAGE_FRAMES or
     * comes without outline version 3. A page_frame sent is answered as
     * sent; whether the site admits it is frameRefusal()'s answer.
     *
     * @param array<string, mixed> $spec       A page-create spec.
     * @param bool                 $blockTheme Whether the site's theme is a block theme (ElementorFacts::blockTheme()).
     * @return string|false|null
     */
    public static function pageFrame(array $spec, bool $blockTheme): string|false|null
    {
        $v3 = ($spec['outline_version'] ?? null) === OutlineV3Schema::VERSION;
        if (array_key_exists('page_frame', $spec)) {
            $frame = $spec['page_frame'];

            return $v3 && is_string($frame) && in_array($frame, OutlineV3Schema::PAGE_FRAMES, true) ? $frame : false;
        }
        if (!$v3) {
            return null;
        }
        if ($blockTheme) {
            return self::BLOCK_THEME_FRAMES[0];
        }

        return self::opensWithHero($spec['outline'] ?? null) ? 'full_width' : 'theme';
    }

    /**
     * The page frames a designed page may be built in on a site whose theme
     * is ($blockTheme) or is not a block theme.
     *
     * @param bool $blockTheme Whether the site's theme is a block theme.
     * @return list<string>
     */
    public static function pageFrames(bool $blockTheme): array
    {
        return $blockTheme ? self::BLOCK_THEME_FRAMES : OutlineV3Schema::PAGE_FRAMES;
    }

    /**
     * The page_frame_unavailable refusal for $frame on this site, or null
     * when the site admits it. The failure lists the frames it admits.
     *
     * @param string $frame      A page frame pageFrame() answered.
     * @param bool   $blockTheme Whether the site's theme is a block theme.
     * @return array<string, mixed>|null A refusal from Refusal::make().
     */
    public static function frameRefusal(string $frame, bool $blockTheme): ?array
    {
        $admitted = self::pageFrames($blockTheme);
        if (in_array($frame, $admitted, true)) {
            return null;
        }

        return Refusal::make(self::CODE_FRAME_UNAVAILABLE, 'the page frame cannot be built with the site\'s theme.', [[
            'path'    => 'input',
            'key'     => 'page_frame',
            'reason'  => self::REASON_FRAME_BLOCK_THEME,
            'allowed' => $admitted,
        ]]);
    }

    /**
     * Whether the first block of the first section is a hero pattern, the
     * one place the outline grammar lets a hero stand. Nodes may be arrays
     * or decoded JSON objects.
     *
     * @param mixed $outline The outline.
     * @return bool
     */
    private static function opensWithHero(mixed $outline): bool
    {
        $section = is_array($outline) ? ($outline[0] ?? null) : null;
        $section = is_object($section) ? get_object_vars($section) : $section;
        $blocks  = is_array($section) ? ($section['children'] ?? null) : null;
        $block   = is_array($blocks) ? ($blocks[0] ?? null) : null;
        $block   = is_object($block) ? get_object_vars($block) : $block;

        return is_array($block) && ($block['type'] ?? null) === 'pattern' && ($block['pattern'] ?? null) === 'hero';
    }

    /**
     * Create the draft, after the command recorded the ledger row for the
     * request and re-checked that $built matches what was approved. Runs as
     * the principal.
     *
     * $ledgerUpdate merges fields into this request's ledger row and answers
     * whether it was recorded.
     *
     * @param int                                $principal    The WPMgr service user.
     * @param array<string, mixed>               $spec         The spec $built was made from.
     * @param BuilderAdapter                     $a            The resolved adapter.
     * @param string                             $requestId    The request.
     * @param array<string, mixed>               $built        What precheck() answered for this write.
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @return array<string, mixed>
     */
    public static function write(int $principal, array $spec, BuilderAdapter $a, string $requestId, array $built, callable $ledgerUpdate): array
    {
        $doc      = $built['doc'] ?? null;
        $title    = $built['title'] ?? null;
        $digest   = $built['preview_digest'] ?? null;
        $preview  = $built['preview'] ?? null;
        $postType = $spec['post_type'] ?? null;
        $editor   = $spec['editor'] ?? null;
        $format   = is_array($preview) ? ($preview['format'] ?? null) : null;
        $version  = is_array($preview) ? ($preview['elementor_version'] ?? null) : null;
        if (!$a instanceof ElementorAdapter || !$doc instanceof NativeDocument || !is_string($title) || !is_string($digest)
            || !is_string($postType) || !is_string($editor) || !is_string($format) || !is_string($version) || $principal < 1) {
            $result = self::fail('bad_input', 'the page was not built for this write');
            $ledgerUpdate(['phase' => 'failed', 'result' => $result]);

            return $result;
        }

        if (isset($spec['template'])) {
            $unavailable = $a->templates()->availabilityRefusal();
            if ($unavailable !== null) {
                $result = self::fail($unavailable['code'], $unavailable['detail']);
                $ledgerUpdate(['phase' => 'failed', 'result' => $result]);
                return $result;
            }
        }

        // Always a draft, always the principal's, the content left to the
        // builder's save. The meta is the request marker and the rows the
        // builder needs to treat the post as its page.
        $postId = wp_insert_post(wp_slash([
            'post_type'    => $postType,
            'post_status'  => 'draft',
            'post_author'  => $principal,
            'post_title'   => $title,
            'post_content' => '',
            'meta_input'   => [ElementorDocument::MARKER_KEY => $requestId] + $doc->meta,
        ]), true);
        if (!is_int($postId) || $postId < 1) {
            $result = self::fail('refused_by_site', 'WordPress did not create the draft');
            $ledgerUpdate(['phase' => 'failed', 'result' => $result]);

            return $result;
        }
        if (!$ledgerUpdate(['phase' => 'post_created', 'created_post_id' => $postId])) {
            // Undo could never find this draft, so it is not left behind.
            $trashed = self::trashOwn($postId);
            $result  = self::fail('snapshot_failed', 'the ledger could not record the created draft; it was moved to the trash', ['post_id' => $postId, 'trashed' => $trashed]);
            $ledgerUpdate([
                'phase'           => 'failed',
                'created_post_id' => $postId,
                'undo_state'      => $trashed ? 'trashed' : 'none',
                'result'          => $result,
            ]);

            return $result;
        }

        $target = $a->document()->targetProblem($postId, $a->facts(), $spec['template'] ?? null);
        if ($target === 'not_editable') {
            return self::failTrashed($postId, AdapterStatus::CODE, 'role_excluded', $ledgerUpdate);
        }
        if ($target !== null) {
            return self::failTrashed($postId, ElementorDocument::CODE_REFUSED, 'the new draft cannot take the page builder\'s save: ' . $target, $ledgerUpdate);
        }

        $saved = $a->write($postId, $doc);
        if (($saved['ok'] ?? null) !== true) {
            $code = $saved['code'] ?? null;
            $code = is_string($code) && in_array($code, self::SAVE_CODES, true) ? $code : ElementorDocument::CODE_REFUSED;
            $why  = $saved['detail'] ?? null;

            return self::failTrashed($postId, $code, is_string($why) && $why !== '' ? $why : 'the page builder did not save the page', $ledgerUpdate);
        }

        $themeType = $spec['template'] ?? null;
        $rules = [];
        $titles = [];
        if ($themeType !== null) {
            try {
                $rulesResult = $a->templates()->saveRules($postId, $spec['shows_on']);
                if (!isset($rulesResult['rules'])) {
                    return self::failTrashed($postId, $rulesResult['code'], $rulesResult['detail'], $ledgerUpdate);
                }
                $rules = $rulesResult['rules'];
                $titles = ThemeTemplateCreate::ruleTitles($rules);
            } catch (\Throwable $e) {
                return self::failTrashed($postId, 'verify_mismatch', 'shows_on could not be verified', $ledgerUpdate);
            }
        }

        $problem = $a->verifyCreated($postId, $doc, $principal, $requestId, $spec['shows_on'] ?? []);
        $afterFp = null;
        if ($problem === null) {
            $keys = $a->descriptor()->exactKeys;
            if ($themeType !== null) {
                $keys[] = ElementorRuntime::KEY_CONDITIONS;
            }
            try {
                $stored = BuilderDocumentFingerprint::read($postId, $keys);
                if ($stored === null) {
                    $problem = 'post_missing';
                } elseif ($stored['post']['post_title'] !== $title) {
                    $problem = 'title';
                } else {
                    $afterFp = BuilderDocumentFingerprint::compute($stored['post'], $stored['rows'], $keys);
                }
            } catch (\Throwable $e) {
                $problem = 'post_unreadable';
            }
        }
        if ($problem !== null || $afterFp === null) {
            return self::failTrashed($postId, 'verify_mismatch', 'the created draft is not what was built: ' . ($problem ?? 'fingerprint'), $ledgerUpdate);
        }

        $result = [
            'ok'                => true,
            'outcome'           => 'created',
            'mode'              => 'write',
            'ability'           => OwnAbilities::NAME_PAGE_CREATE,
            'request_id'        => $requestId,
            'post_id'           => $postId,
            'post_type'         => $postType,
            'editor'            => $editor,
            'format'            => $format,
            'elementor_version' => $version,
            'status'            => 'draft',
            'preview_digest'    => $digest,
            'after_fp'          => $afterFp,
            'verify'            => ['tree_equal' => true, 'status' => 'draft'],
        ];
        if ($themeType !== null) {
            $result['template'] = $themeType;
            $result['shows_on'] = $rules;
            $result['rule_titles'] = $titles;
            $result['also_live'] = $a->templates()->alsoLive($themeType);
        }
        // A stylesheet that was not regenerated does not undo the page; the
        // person is told.
        $warnings = ElementorDocument::warningsOf($saved);
        if ($warnings !== []) {
            $result['warnings'] = $warnings;
        }
        // Undo checks a builder draft against this record: the fingerprint
        // its save left and the revisions that save made. A draft without
        // the record could never be undone, so it is not left behind.
        $recorded = $ledgerUpdate([
            'phase'            => 'completed',
            'after_fp'         => $afterFp,
            'undo_state'       => 'available',
            'builder'          => $a->id(),
            'builder_version'  => $version,
            'format'           => $format,
            'own_revision_ids' => self::scopeRevisions($saved['scope'] ?? null),
            'result'           => $result,
        ]);
        if (!$recorded) {
            return self::failTrashed($postId, 'snapshot_failed', 'the ledger could not record the created draft', $ledgerUpdate);
        }

        return $result;
    }

    /**
     * The page-edit requests an undo's signed parameters name, or null when
     * p.revert is not one of: absent, {} or {"chain": [...]}, the list at
     * most MAX_CHAIN distinct lowercase request ids. Absent and {} name none.
     *
     * The list only names which ledger rows to read; nothing an undo checks
     * or trashes is taken from it.
     *
     * @param mixed $revert The decoded p.revert.
     * @return list<string>|null
     */
    public static function revertChain(mixed $revert): ?array
    {
        if ($revert === null) {
            return [];
        }
        if (!is_object($revert)) {
            return null;
        }
        $members = get_object_vars($revert);
        if ($members === []) {
            return [];
        }
        $chain = $members[self::REVERT_CHAIN] ?? null;
        if (array_keys($members) !== [self::REVERT_CHAIN] || !is_array($chain) || !ArrayShape::isList($chain) || count($chain) > self::MAX_CHAIN) {
            return null;
        }
        $seen = [];
        foreach ($chain as $id) {
            if (!is_string($id) || preg_match(self::RE_CHAIN_ID, $id) !== 1 || isset($seen[$id])) {
                return null;
            }
            $seen[$id] = true;
        }

        return $chain;
    }

    /**
     * Why undo may not trash this builder draft, or null.
     *
     * Undo trashes the draft only when its builder_document_v1 fingerprint
     * (its content, summary, slug and password, its builder rows and where
     * it sits, parent and order) is where WPMgr's own changes left it, it
     * has no revision but the ones those changes made, no autosave, and no
     * one holds its edit lock. Fails closed when any of that cannot be read.
     *
     * With no chain, WPMgr's own changes are the creation alone: the
     * fingerprint is the one recorded when the draft was created and the
     * revisions are the ones its save made, answered as before chains
     * existed.
     *
     * With a chain, each named request's ledger row must be a wpmgr/page-edit
     * change of this draft: a completed one by the same builder, or a failed
     * one whose row says the site put the page back (restored true). A
     * change that was undone, and a failed one put back, count only for
     * their revisions. The others, in the chain's order, must each start
     * from the fingerprint the one before left (the first from the
     * creation's), and the last must have left the draft as it is now. The
     * revisions allowed are the creation's and every named change's. A
     * refusal then answers one of CHAIN_BROKEN, FOREIGN_REVISION, AUTOSAVE
     * or LOCKED.
     *
     * @param int                  $postId    The created draft, from the ledger row.
     * @param array<string, mixed> $ledgerRow The request's ledger row.
     * @param list<string>         $chain     The page-edit requests the undo names (revertChain()), read inside the caller's claim on the draft.
     * @return string|null
     */
    public static function revertProblem(int $postId, array $ledgerRow, array $chain = []): ?string
    {
        $builder = $ledgerRow['builder'] ?? null;
        $keys    = is_string($builder) ? (self::DESCRIPTOR_KEYS[$builder] ?? null) : null;
        if (!is_string($builder) || $keys === null) {
            return 'this draft was built with a page builder this agent cannot check';
        }
        $themeType = $ledgerRow['result']['template'] ?? null;
        if ($themeType !== null) {
            if (!in_array($themeType, ThemeTemplateCreate::TYPES, true) || !ThemeTemplateCreate::termMatches($postId, $themeType)) {
                return 'the template type changed after creation';
            }
            $keys[] = ElementorRuntime::KEY_CONDITIONS;
        }
        $afterFp = $ledgerRow['after_fp'] ?? null;
        if (!is_string($afterFp) || preg_match(self::RE_FP, $afterFp) !== 1) {
            return 'the created draft has no recorded fingerprint';
        }
        $own = self::ledgerRevisions($ledgerRow['own_revision_ids'] ?? null);
        if ($own === null) {
            return 'the created draft has no record of its own revisions';
        }
        if ($postId < 1) {
            return 'the created draft no longer exists';
        }
        $chained = $chain !== [];
        if ($chained) {
            $end = self::chainEnd($postId, $builder, $afterFp, $own, $chain);
            if ($end === null) {
                return self::CHAIN_BROKEN;
            }
            [$afterFp, $own] = $end;
        }

        try {
            $current = BuilderDocumentFingerprint::ofPost($postId, $keys);
        } catch (\Throwable $e) {
            return 'the draft could not be read';
        }
        if ($current === null) {
            return 'the created draft no longer exists';
        }
        if (!hash_equals($afterFp, $current)) {
            return self::touched(self::CHAIN_BROKEN, $chained);
        }

        // User id 0 (the int) means an autosave by any user.
        if (wp_get_post_autosave($postId, 0) !== false) {
            return self::touched(self::AUTOSAVE, $chained);
        }
        $revisions = wp_get_post_revisions($postId, ['check_enabled' => false]);
        if (!is_array($revisions)) {
            return 'the revisions of this draft could not be read';
        }
        $allowed = array_fill_keys($own, true);
        foreach ($revisions as $key => $revision) {
            $id = is_object($revision) ? (get_object_vars($revision)['ID'] ?? null) : (is_int($revision) ? $revision : $key);
            if (!is_int($id) || !isset($allowed[$id])) {
                return self::touched(self::FOREIGN_REVISION, $chained);
            }
        }

        if (!function_exists('wp_check_post_lock') && defined('ABSPATH') && is_readable(ABSPATH . 'wp-admin/includes/post.php')) {
            require_once ABSPATH . 'wp-admin/includes/post.php';
        }
        if (!function_exists('wp_check_post_lock')) {
            return 'whether someone is editing this draft could not be checked';
        }
        if (wp_check_post_lock($postId) !== false) {
            return self::touched(self::LOCKED, $chained);
        }

        return null;
    }

    /**
     * Where the chain's changes left the draft, and the revisions the
     * creation and those changes made; null when the chain does not hold
     * (revertProblem()).
     *
     * @param int          $postId  The created draft.
     * @param string       $builder The builder that created it.
     * @param string       $afterFp The fingerprint its creation left.
     * @param list<int>    $own     The revisions its creation made.
     * @param list<string> $chain   The page-edit requests, in the order they were made.
     * @return array{0: string, 1: list<int>}|null
     */
    private static function chainEnd(int $postId, string $builder, string $afterFp, array $own, array $chain): ?array
    {
        $fp      = $afterFp;
        $allowed = array_fill_keys($own, true);
        foreach ($chain as $requestId) {
            $row = AbilityLedger::get($requestId);
            if ($row === null || ($row['ability'] ?? null) !== OwnAbilities::NAME_PAGE_EDIT
                || ($row['target_post_id'] ?? null) !== $postId) {
                return null;
            }

            // A failed change the site put the page back from left only the
            // revisions its save made.
            if (($row['phase'] ?? null) === 'failed' && ($row['restored'] ?? null) === true) {
                $revisions = self::ledgerRevisions($row['own_revision_ids'] ?? null);
                if ($revisions === null) {
                    return null;
                }
                $allowed += array_fill_keys($revisions, true);
                continue;
            }
            if (($row['phase'] ?? null) !== 'completed' || ($row['builder'] ?? null) !== $builder) {
                return null;
            }
            $before    = $row['before_fp'] ?? null;
            $after     = $row['after_fp'] ?? null;
            $revisions = self::ledgerRevisions($row['own_revision_ids'] ?? null);
            if (!is_string($before) || preg_match(self::RE_FP, $before) !== 1
                || !is_string($after) || preg_match(self::RE_FP, $after) !== 1 || $revisions === null) {
                return null;
            }
            $allowed += array_fill_keys($revisions, true);

            // An undone change put back what it wrote: only its revisions remain.
            $state = $row['undo_state'] ?? null;
            if ($state === 'restored') {
                continue;
            }
            if ($state !== 'available' || !hash_equals($fp, $before)) {
                return null;
            }
            $fp = $after;
        }

        return [$fp, array_keys($allowed)];
    }

    /**
     * A refusal's detail: the fixed token with a chain, the words an undo
     * answered before chains without one.
     *
     * @param string $token   CHAIN_BROKEN, FOREIGN_REVISION, AUTOSAVE or LOCKED.
     * @param bool   $chained Whether the undo named a chain.
     * @return string
     */
    private static function touched(string $token, bool $chained): string
    {
        return $chained ? $token : self::UNCHAINED[$token];
    }

    /**
     * The preview digest.
     *
     * @param string $editor   Editor value.
     * @param string $format   Builder format.
     * @param string $version  Builder version.
     * @param string $postType Post type.
     * @param string $title    The plain title.
     * @param string $treeJson The tree's canonical bytes.
     * @param string|null $themeType Header or footer; null preserves page/post digest bytes.
     * @param list<string> $showsOn Rules in the exact requested order.
     * @return string
     * @throws \JsonException When a value is not valid UTF-8.
     */
    public static function previewDigest(string $editor, string $format, string $version, string $postType, string $title, string $treeJson, ?string $themeType = null, array $showsOn = []): string
    {
        $parts = [$editor, $format, $version, $postType, 'draft', $title, $treeJson];
        if ($themeType !== null) {
            $parts[] = $themeType;
            $parts[] = $showsOn;
        }
        return hash('sha256', json_encode($parts, JSON_THROW_ON_ERROR));
    }

    /**
     * The format an elementor_format value builds, or null when it is not one.
     *
     * @param mixed $value elementor_format; absent means the site default.
     * @return string|null
     */
    private static function format(mixed $value): ?string
    {
        if ($value === null || $value === self::FORMAT_SITE_DEFAULT || $value === ElementorFacts::FORMAT_CLASSIC) {
            return ElementorFacts::FORMAT_CLASSIC;
        }

        return $value === ElementorFacts::FORMAT_ATOMIC ? ElementorFacts::FORMAT_ATOMIC : null;
    }

    /**
     * containers or sections, from the built tree's first element.
     *
     * @param array<mixed> $tree Built tree.
     * @return string
     */
    private static function layout(array $tree): string
    {
        $first = $tree[0] ?? null;

        return is_array($first) && ($first['elType'] ?? null) === 'container' ? self::LAYOUT_CONTAINERS : self::LAYOUT_SECTIONS;
    }

    /**
     * The revision ids a save's write scope recorded.
     *
     * @param mixed $scope The save's scope outcome.
     * @return list<int>
     */
    private static function scopeRevisions(mixed $scope): array
    {
        $ids = is_array($scope) ? ($scope['revision_ids'] ?? []) : [];
        $out = [];
        foreach (is_array($ids) ? $ids : [] as $id) {
            if (is_int($id) && $id > 0 && !in_array($id, $out, true)) {
                $out[] = $id;
            }
        }

        return $out;
    }

    /**
     * The own revision ids a ledger row holds, or null when they are not a
     * list of post ids.
     *
     * @param mixed $ids Stored ids.
     * @return list<int>|null
     */
    private static function ledgerRevisions(mixed $ids): ?array
    {
        if (!is_array($ids) || !ArrayShape::isList($ids) || count($ids) > self::MAX_OWN_REVISIONS) {
            return null;
        }
        foreach ($ids as $id) {
            if (!is_int($id) || $id < 1) {
                return null;
            }
        }

        return $ids;
    }

    /**
     * Trash the created draft, record the failure, and answer it.
     *
     * @param int                                  $postId       The created draft.
     * @param string                               $code         Refusal code.
     * @param string                               $detail       Reason, WPMgr's own words.
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @return array<string, mixed>
     */
    private static function failTrashed(int $postId, string $code, string $detail, callable $ledgerUpdate): array
    {
        $trashed = self::trashOwn($postId);
        $result  = self::fail($code, $detail, ['post_id' => $postId, 'trashed' => $trashed]);
        $ledgerUpdate([
            'phase'      => 'failed',
            'undo_state' => $trashed ? 'trashed' : 'none',
            'result'     => $result,
        ]);

        return $result;
    }

    /**
     * Trash a post this request created (the ledger proves it), then confirm.
     *
     * @param int $postId Post id.
     * @return bool
     */
    private static function trashOwn(int $postId): bool
    {
        wp_trash_post($postId);
        clean_post_cache($postId);
        $after = get_post($postId);

        // With trash disabled core deletes outright; gone counts as undone.
        return !is_object($after) || (get_object_vars($after)['post_status'] ?? null) === 'trash';
    }

    /**
     * A precheck refusal.
     *
     * @param string $code   Refusal code.
     * @param string $detail Reason, WPMgr's own words.
     * @return array{refusal: array<string, mixed>, preview: array<string, mixed>, preview_digest: string, base_fingerprint: string, tree: array<mixed>, doc: null, title: string}
     */
    private static function refusedPrecheck(string $code, string $detail): array
    {
        return [
            'refusal'          => self::fail($code, $detail),
            'preview'          => [],
            'preview_digest'   => '',
            'base_fingerprint' => '',
            'tree'             => [],
            'doc'              => null,
            'title'            => '',
        ];
    }

    /**
     * A precheck refusal that carries the refusal envelope's failures.
     *
     * @param array{code:string,detail:string,failures:list<array<string,mixed>>} $refusal From Refusal::make().
     * @return array{refusal: array<string, mixed>, preview: array<string, mixed>, preview_digest: string, base_fingerprint: string, tree: array<mixed>, doc: null, title: string}
     */
    private static function refusedEnvelope(array $refusal): array
    {
        return ['refusal' => Refusal::command($refusal)] + self::refusedPrecheck($refusal['code'], $refusal['detail']);
    }

    /**
     * A refusal in the command's shape.
     *
     * @param string               $code   Refusal code.
     * @param string               $detail Reason.
     * @param array<string, mixed> $extra  Extra fields.
     * @return array<string, mixed>
     */
    private static function fail(string $code, string $detail, array $extra = []): array
    {
        return [
            'ok'        => false,
            'outcome'   => 'refused',
            'code'      => $code,
            'detail'    => $detail,
            'retryable' => false,
        ] + $extra;
    }
}
