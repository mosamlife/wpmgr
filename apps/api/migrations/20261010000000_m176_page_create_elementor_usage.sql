-- m176: wpmgr/page-create's usage names the Elementor versions and the
-- outline values WPMgr builds in Elementor, and a hash clear so the boot
-- stamp re-stamps the entry. (#890)
--
-- One row, one copy column, one guarded UPDATE.
--
-- ===========================================================================
-- WHAT CHANGES
-- ===========================================================================
--
-- The ability_catalogue row named wpmgr/page-create (source wpmgr), and only
-- these columns:
--
--   * usage         m166's text with its Elementor part corrected. Every
--                   sentence before "On a site with Elementor" is m166's,
--                   unchanged. The Elementor part now says:
--                     - the Elementor versions the agent builds classic pages
--                       for, 3.20 to 4.3, and that a later Elementor is
--                       refused until WPMgr verifies it;
--                     - that an Atomic request is always refused (unchanged);
--                     - that an image's align can only be none or center;
--                     - that a button link cannot contain "&".
--                   The agent enforces each of these, and the control plane
--                   checks the last two before a request is sent. The WPMgr
--                   plugin floor it names, 0.61.161, is unchanged.
--   * entry_sha256  cleared to NULL.
--   * updated_at    now().
--
-- Every other column is left as it is, description and limits included: a
-- person's change to them, the kill switch, and any column a later migration
-- classifies stay exactly as they are.
--
-- The contract gate apps/api/tests/contract/page_create_usage_agent_parity_test.go
-- reads the agent's Elementor range, image alignments and link rule from the
-- agent's source and fails when the usage the newest page-create copy
-- migration writes says otherwise.
--
-- ===========================================================================
-- WHY THE HASH CLEAR IS MANDATORY
-- ===========================================================================
--
-- entry_sha256 is the sha256 of the canonical entry bytes the control plane
-- sends, and those bytes include usage. A stored hash that no longer matches
-- the bytes refuses every send of the entry (entry changed). Clearing it lets
-- the boot stamp (m157's stamp_wpmgr_ability_entry_hash, which only moves NULL
-- to a hash on a wpmgr row) fill it from the new bytes when the new revision
-- starts. m159, m162 and m166 are the precedent, and like them the clear is
-- recorded by this migration and not in ability_catalogue_audit: the audit's
-- NULL-actor CHECK admits only the stamp shape, and the stamp that follows
-- writes its own audit row.
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
-- superadmin write and the stamp take them), the row's usage decides:
--
--   * already this migration's usage -> nothing to do. A re-run changes
--                                       nothing, so it cannot clear the hash
--                                       again and close in-flight approvals a
--                                       second time.
--   * still m166's usage             -> the UPDATE runs (exactly one row).
--   * anything else, or no such row  -> RAISE WARNING and change nothing.
--
-- Anything else means a person replaced the usage through the superadmin
-- catalogue write, and that text is theirs to keep. This migration corrects
-- guidance text only, and the agent and the control plane enforce every rule
-- it describes whatever the text says, so a row it does not recognise is no
-- reason to stop the boot. The skip is a WARNING, not a NOTICE, because the
-- migration runner sets no notice handler, so a NOTICE reaches no log, while
-- PostgreSQL writes a WARNING to the server log at its default
-- log_min_messages.
--
-- The guard keys on usage alone, the only column this migration writes.
--
-- To bring a skipped row up to date: set its usage to this migration's text
-- through the superadmin catalogue write, which stamps the hash in the same
-- write.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m176 is new and is a guarded data change. No DDL, no table,
-- no definer, no grant. A database that ran m166 converges through the second
-- branch of the guard. A later migration that rewrites page-create's usage
-- converges from this migration's text, not m166's.

DO $$
DECLARE
    v_name constant text := 'wpmgr/page-create';

    -- m166's usage: the state m176 meets on every install that has not run it.
    v_m166_usage constant text :=
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
        'cannot use the outline style, a ' ||
        'paragraph cannot be only a web address, and an image''s alt text must be exactly the alt text it has in the ' ||
        'media library. Elementor pages need the WPMgr plugin 0.61.161 or later and Elementor 3.20 or later on the ' ||
        'site.';

    v_usage constant text :=
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

    v_rows      int;
    v_entry     uuid;
    v_usage_now text;
    v_updated   int;
BEGIN
    -- m155's writer lock for this name, then the row: the superadmin write
    -- and the stamp take the same two locks in the same order.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));

    SELECT count(*) INTO v_rows
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr';
    IF v_rows <> 1 THEN
        RAISE WARNING 'm176: expected exactly one wpmgr catalogue row named %, found %; its usage was not corrected',
            v_name, v_rows;
        RETURN;
    END IF;

    SELECT "entry_id", "usage"
      INTO v_entry, v_usage_now
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr'
       FOR UPDATE;

    IF v_usage_now IS NOT DISTINCT FROM v_usage THEN
        RETURN;
    END IF;

    IF v_usage_now IS DISTINCT FROM v_m166_usage THEN
        RAISE WARNING 'm176: the usage of % was changed by a person; leaving it as it is', v_name
            USING HINT = 'To describe the Elementor versions and limits WPMgr builds, set its usage to the m176 text ' ||
                         'through the superadmin catalogue write.';
        RETURN;
    END IF;

    UPDATE "public"."ability_catalogue"
       SET "usage"        = v_usage,
           "entry_sha256" = NULL,
           "updated_at"   = now()
     WHERE "entry_id" = v_entry
       AND "source" = 'wpmgr'
       AND "usage" IS NOT DISTINCT FROM v_m166_usage;

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    IF v_updated <> 1 THEN
        RAISE EXCEPTION 'm176: updated % rows of %, expected 1', v_updated, v_name
            USING ERRCODE = 'P0002';
    END IF;
END;
$$;
