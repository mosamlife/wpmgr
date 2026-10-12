-- m177: wpmgr/page-create's description and usage describe designed
-- Elementor pages (outline version 3), and a hash clear so the boot stamp
-- re-stamps the entry.
--
-- One row, two copy columns, one guarded UPDATE.
--
-- ===========================================================================
-- WHAT CHANGES
-- ===========================================================================
--
-- The ability_catalogue row named wpmgr/page-create (source wpmgr), and only
-- these columns:
--
--   * description   m166's text, plus one sentence: with outline_version 3
--                   the draft is a designed page, of sections and patterns
--                   styled from one design.
--   * usage         m176's rules for outline version 2, the block editor, the
--                   classic editor and Elementor, unchanged in substance, then
--                   designed pages: outline_version 3 with a complete design,
--                   the colour, contrast and font rules, page_frame and its
--                   default, sections, anchors and #anchor links, one hero
--                   holding the only level 1 heading, patterns, style by
--                   tokens only, that colours and fonts are written on the
--                   page and not into the site's kit (so Undo covers the
--                   whole change), and the WPMgr plugin release designed
--                   pages need, 0.61.165. It must equal the control plane's
--                   designed-pages floor constant. The text fits
--                   ability_catalogue_usage_check's 2000 characters; the
--                   input schema, which ships in code, carries each field's
--                   values, and every refusal names its own code and paths.
--   * entry_sha256  cleared to NULL.
--   * updated_at    now().
--
-- Untouched: min_agent_version stays at the text-only floor, so sites on an
-- earlier agent keep the editors they have; the designed-pages floor is
-- enforced per input by the control plane. limits, enabled (a kill switch
-- stays as an operator left it), title, status, approval, snapshot and every
-- other column stay as they are.
--
-- The contract gate apps/api/tests/contract/page_create_usage_agent_parity_test.go
-- reads the agent's rules from its source and fails when the usage the newest
-- page-create copy migration writes says otherwise; db/schema.sql's seed
-- carries the same text.
--
-- ===========================================================================
-- WHY THE HASH CLEAR IS MANDATORY
-- ===========================================================================
--
-- entry_sha256 is the sha256 of the canonical entry bytes the control plane
-- sends, and those bytes include description and usage. A stored hash that no
-- longer matches the bytes refuses every send of the entry (entry changed).
-- Clearing it lets the boot stamp (m157's stamp_wpmgr_ability_entry_hash,
-- which only moves NULL to a hash on a wpmgr row) fill it from the new bytes
-- when the new revision starts. m159, m162, m166 and m176 are the precedent,
-- and like them the clear is recorded by this migration and not in
-- ability_catalogue_audit: the audit's NULL-actor CHECK admits only the stamp
-- shape, and the stamp that follows writes its own audit row.
--
-- Consequence, by design and fail closed: a page-create request approved
-- against the old hash and not yet dispatched closes not_sent / entry_changed,
-- and a request still pending at deploy can be approved but then closes the
-- same way. Nothing is ever sent against an entry that changed.
--
-- ===========================================================================
-- THE GUARD
-- ===========================================================================
--
-- Under m155's per-name advisory lock and a row lock (the order the
-- superadmin write and the stamp take them), each column is decided on its
-- own:
--
--   * already this migration's text -> left as it is.
--   * still its predecessor's text  -> replaced (description: m166's;
--                                      usage: m176's).
--   * anything else                 -> left as it is, with a WARNING naming
--                                      the column.
--
-- When neither column moves there is nothing to do: a re-run changes
-- nothing, so it cannot clear the hash again and close in-flight approvals a
-- second time. When either moves, the one UPDATE writes both columns and
-- clears the hash.
--
-- Anything else means a person replaced the text through the superadmin
-- catalogue write, and that text is theirs to keep. This migration corrects
-- guidance text only, and the agent and the control plane enforce every rule
-- it describes whatever the text says, so a row it does not recognise is no
-- reason to stop the boot. The skip is a WARNING, not a NOTICE, because the
-- migration runner sets no notice handler, so a NOTICE reaches no log, while
-- PostgreSQL writes a WARNING to the server log at its default
-- log_min_messages. A missing row is a WARNING too, and changes nothing.
--
-- To bring a skipped column up to date: set it to this migration's text
-- through the superadmin catalogue write, which stamps the hash in the same
-- write.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m177 is new and is a guarded data change. No DDL, no table,
-- no definer, no grant. A database that ran m176 converges through the second
-- branch of each column's guard. A later migration that rewrites page-create's
-- copy converges from this migration's text.

DO $$
DECLARE
    v_name constant text := 'wpmgr/page-create';

    -- m166's description: the state m177 meets on every install that has
    -- not run it.
    v_m166_description constant text :=
        'Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, separators, ' ||
        'images already in the site''s media library, and, in the block editor or Elementor, buttons, spacing, ' ||
        'sections and columns. With editor builder:elementor the draft is built in Elementor, from Elementor''s own ' ||
        'layout elements and widgets. Nothing is published. Undo moves the draft to the trash.';

    -- m176's usage: the state m177 meets on every install that has not run it.
    v_m176_usage constant text :=
        'Build the page as an outline. A top-level item can be any block, a group (a section) or columns. A group ' ||
        'holds blocks or columns; a column holds blocks only. Use 2 to 4 columns; widths are optional whole ' ||
        'percentages that add up to 100. Images must already be in the media library: find an attachment id with ' ||
        'wpmgr/rest-read route wp-v2-media-list, and write alt text that describes the picture, or an empty alt for ' ||
        'a decorative one. Button links are https:// addresses or paths on this site that start with /. All text is ' ||
        'plain: no HTML, shortcodes or template syntax; square brackets only around a number such as [1], and never ' ||
        'in alt text. On a site that uses the classic editor, send editor wordpress_classic and only headings, ' ||
        'paragraphs, lists, quotes, tables, separators and images without captions. Layout blocks need the WPMgr ' ||
        'plugin 0.61.160 or later on the site. On a site with Elementor, send editor builder:elementor to build the ' ||
        'draft in Elementor from the same outline. The draft is built with Elementor''s classic widgets: leave ' ||
        'elementor_format out or send classic; site_default, the default, builds classic widgets too. Atomic is not ' ||
        'available yet: elementor_format atomic is always refused, whatever the site runs. In Elementor, buttons ' ||
        'cannot use the outline style, a button link cannot contain &, a paragraph cannot be only a web address, ' ||
        'an image''s align can only be none or center, and an image''s alt text must be exactly the alt text it has ' ||
        'in the media library. Elementor pages need the WPMgr plugin 0.61.161 or later and Elementor 3.20 to 4.3 on ' ||
        'the site; a later Elementor is refused until WPMgr verifies it.';

    v_description constant text :=
        'Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, ' ||
        'separators, images already in the site''s media library, and, in the block editor or Elementor, buttons, ' ||
        'spacing, sections and columns. With editor builder:elementor the draft is built in Elementor, from ' ||
        'Elementor''s own layout elements and widgets. With outline_version 3 it is a designed page: sections ' ||
        'with tones, backgrounds and anchors, and patterns such as a hero, features, stats, pricing, ' ||
        'testimonials, questions and a call to action, styled from one design of colours, fonts and spacing. ' ||
        'Nothing is published. Undo moves the draft to the trash.';

    v_usage constant text :=
        'Build the page as an outline. A top-level item can be any block, a group (a section) or columns. A ' ||
        'group holds blocks or columns; a column holds blocks only. Use 2 to 4 columns; widths are optional ' ||
        'whole percentages that add up to 100. Images must already be in the media library: find an attachment ' ||
        'id with wpmgr/rest-read route wp-v2-media-list, and write alt text that describes the picture, or an ' ||
        'empty alt for a decorative one. Button links are https:// addresses or paths on this site that start ' ||
        'with /. All text is plain: no HTML, shortcodes or template syntax; square brackets only around a number ' ||
        'such as [1], and never in alt text. On a site that uses the classic editor, send editor ' ||
        'wordpress_classic and only headings, paragraphs, lists, quotes, tables, separators and images without ' ||
        'captions. Layout blocks need the WPMgr plugin 0.61.160 or later on the site. With Elementor, send ' ||
        'editor builder:elementor: the draft uses Elementor''s classic widgets, and elementor_format atomic is ' ||
        'always refused. In Elementor, a button link cannot contain &, a paragraph cannot be only a web address, ' ||
        'an image''s align can only be none or center, and its alt text must be its media library alt text. ' ||
        'Elementor pages need the WPMgr plugin 0.61.161 or later and Elementor 3.20 to 4.3 on the site. For a ' ||
        'designed page, send outline_version 3 with a complete design. Colours are never #000000; ink on ground ' ||
        'and on surface, and on_accent on accent, need 4.5:1 contrast. Fonts are ones Elementor offers, or ' ||
        'system. page_frame is full_width, title hidden, when there is a hero, else theme; canvas only on ' ||
        'request. Use up to 20 sections, an anchor on each and #anchor links to them, one hero holding the only ' ||
        'level 1 heading, patterns for variety and one accent. Style by tokens, never HTML or CSS. Colours and ' ||
        'fonts go on the page, not the kit, so Undo covers all. Designed pages need the WPMgr plugin 0.61.165 or ' ||
        'later and Elementor containers; version 2 buttons cannot use the outline style.';

    v_rows      int;
    v_entry     uuid;
    v_desc_now  text;
    v_usage_now text;
    v_desc_new  text;
    v_usage_new text;
    v_updated   int;
BEGIN
    -- m155's writer lock for this name, then the row: the superadmin write
    -- and the stamp take the same two locks in the same order.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));

    SELECT count(*) INTO v_rows
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr';
    IF v_rows <> 1 THEN
        RAISE WARNING 'm177: expected exactly one wpmgr catalogue row named %, found %; its copy was not changed',
            v_name, v_rows;
        RETURN;
    END IF;

    SELECT "entry_id", "description", "usage"
      INTO v_entry, v_desc_now, v_usage_now
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr'
       FOR UPDATE;

    v_desc_new := v_desc_now;
    IF v_desc_now IS NOT DISTINCT FROM v_m166_description THEN
        v_desc_new := v_description;
    ELSIF v_desc_now IS DISTINCT FROM v_description THEN
        RAISE WARNING 'm177: the description of % was changed by a person; leaving it as it is', v_name
            USING HINT = 'To describe designed pages, set its description to the m177 text through the ' ||
                         'superadmin catalogue write.';
    END IF;

    v_usage_new := v_usage_now;
    IF v_usage_now IS NOT DISTINCT FROM v_m176_usage THEN
        v_usage_new := v_usage;
    ELSIF v_usage_now IS DISTINCT FROM v_usage THEN
        RAISE WARNING 'm177: the usage of % was changed by a person; leaving it as it is', v_name
            USING HINT = 'To describe designed pages, set its usage to the m177 text through the superadmin ' ||
                         'catalogue write.';
    END IF;

    IF (v_desc_new, v_usage_new) IS NOT DISTINCT FROM (v_desc_now, v_usage_now) THEN
        RETURN;
    END IF;

    UPDATE "public"."ability_catalogue"
       SET "description"  = v_desc_new,
           "usage"        = v_usage_new,
           "entry_sha256" = NULL,
           "updated_at"   = now()
     WHERE "entry_id" = v_entry
       AND "source" = 'wpmgr'
       AND ("description", "usage") IS NOT DISTINCT FROM (v_desc_now, v_usage_now);

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    IF v_updated <> 1 THEN
        RAISE EXCEPTION 'm177: updated % rows of %, expected 1', v_updated, v_name
            USING ERRCODE = 'P0002';
    END IF;
END;
$$;
