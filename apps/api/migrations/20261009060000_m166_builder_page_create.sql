-- m166: wpmgr/page-create's copy, usage and limits for drafts built in
-- Elementor, and a hash clear so the boot stamp re-stamps the entry.
--
-- Slice BF-C (WPMgr's own builder support, Elementor, create). One row, one
-- guarded UPDATE.
--
-- ===========================================================================
-- WHAT CHANGES
-- ===========================================================================
--
-- The ability_catalogue row named wpmgr/page-create (source wpmgr), and only
-- these columns:
--
--   * description  OUR words for what the ability does, now naming drafts
--                  built in Elementor.
--   * usage        OUR instructions to the AI: m162's text unchanged, then
--                  how to ask for editor builder:elementor and the optional
--                  elementor_format, the Elementor node rules, and the agent
--                  and Elementor versions it needs.
--   * limits       m162's integer limits unchanged, plus builders_enabled:
--                  the builders page-create may build with, ["elementor"].
--                  The entry carries it to the agent inside the signed entry
--                  bytes; the agent builds with a builder only when its own
--                  code carries that builder and this list names it.
--   * entry_sha256 cleared to NULL.
--   * updated_at   now().
--
-- Untouched: enabled (a kill switch stays as an operator left it),
-- min_agent_version (stays at the text-only floor, so sites on an earlier
-- agent keep the editors they have; the Elementor floor is enforced per input
-- by the control plane), title, status, approval, snapshot, and
-- updated_by_user_id. The input schema itself is not a catalogue column; it
-- ships in code.
--
-- The agent version named in usage is the first agent release that builds
-- Elementor drafts: it must equal the control plane's Elementor floor
-- constant.
--
-- ===========================================================================
-- WHY THE HASH CLEAR IS MANDATORY
-- ===========================================================================
--
-- entry_sha256 is the sha256 of the canonical entry bytes the control plane
-- sends, and those bytes include description, usage and limits. A stored hash
-- that no longer matches the bytes refuses every send of the entry
-- (entry changed). Clearing it lets the boot stamp (m157's
-- stamp_wpmgr_ability_entry_hash, which only moves NULL to a hash on a wpmgr
-- row) fill it from the new bytes when the new revision starts. m159 and m162
-- are the precedent. Like them, the clear is recorded by this migration and
-- not in ability_catalogue_audit: the audit's NULL-actor CHECK admits only the
-- stamp shape, and the stamp that follows writes its own audit row.
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
-- superadmin write and the stamp take them), the row's copy must be in one of
-- two states:
--
--   * already this migration's copy  -> nothing to do. A re-run changes
--                                       nothing, so it cannot clear the hash
--                                       again and close in-flight approvals a
--                                       second time.
--   * still m162's copy              -> the UPDATE runs (exactly one row).
--
-- Anything else means a person replaced our copy through the superadmin
-- catalogue write, and the migration raises SQLSTATE 55000 rather than
-- overwrite it or skip silently. A missing row raises P0002. Either way the
-- boot stops with the message, and nothing is changed.
--
-- The guard keys on the copy itself, not on updated_by_user_id: the kill
-- switch (enabled = false) is a superadmin write too and sets that column
-- while leaving the copy as m162 wrote it. Such a row gets the new copy and
-- stays disabled.
--
-- To reconcile a refused row: put description, usage and limits back to
-- m162's values, or set them to this migration's values, through the
-- superadmin catalogue write on the running revision, then start the new
-- revision again.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m166 is new and is a guarded data change. No DDL, no table,
-- no definer, no grant.

DO $$
DECLARE
    v_name constant text := 'wpmgr/page-create';

    -- m162's copy: the state m166 meets on every install that has not run it.
    v_m162_description constant text :=
        'Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, separators, ' ||
        'images already in the site''s media library, and, in the block editor, buttons, spacing, sections and ' ||
        'columns. Nothing is published. Undo moves the draft to the trash.';

    v_m162_usage constant text :=
        'Build the page as an outline. A top-level item can be any block, a group (a section) or columns. A group ' ||
        'holds blocks or columns; a column holds blocks only. Use 2 to 4 columns; widths are optional whole ' ||
        'percentages that add up to 100. Images must already be in the media library: find an attachment id with ' ||
        'wpmgr/rest-read route wp-v2-media-list, and write alt text that describes the picture, or an empty alt for ' ||
        'a decorative one. Button links are https:// addresses or paths on this site that start with /. All text is ' ||
        'plain: no HTML, shortcodes or template syntax; square brackets only around a number such as [1], and never ' ||
        'in alt text. On a site that uses the classic editor, send editor wordpress_classic and only headings, ' ||
        'paragraphs, lists, quotes, tables, separators and images without captions. Layout blocks need the WPMgr ' ||
        'plugin 0.61.160 or later on the site.';

    v_m162_limits constant jsonb := (
        '{"max_top_level_nodes":200,"max_nodes":400,"max_columns":4,"max_children":50,"max_images":20,' ||
        '"max_buttons":12,"max_tables":10,"max_table_rows":50,"max_table_columns":6,"max_title_chars":200,' ||
        '"max_text_chars":5000,"max_total_chars":60000,"max_input_bytes":65536}'
    )::jsonb;

    v_description constant text :=
        'Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, separators, ' ||
        'images already in the site''s media library, and, in the block editor or Elementor, buttons, spacing, ' ||
        'sections and columns. With editor builder:elementor the draft is built in Elementor, from its own ' ||
        'containers and widgets. Nothing is published. Undo moves the draft to the trash.';

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
        'draft in Elementor from the same outline. You may add elementor_format: site_default (the default, which ' ||
        'follows the site''s own setting and is classic unless the site chose Atomic), classic for Elementor''s ' ||
        'classic widgets, or atomic for its Atomic editor, which works only where that editor is switched on and ' ||
        'WPMgr supports the site''s Elementor version. In Elementor, buttons cannot use the outline style, a ' ||
        'paragraph cannot be only a web address, and an image''s alt text must be exactly the alt text it has in the ' ||
        'media library. Elementor pages need the WPMgr plugin 0.61.162 or later and Elementor 3.20 or later on the ' ||
        'site.';

    v_limits constant jsonb := (
        '{"max_top_level_nodes":200,"max_nodes":400,"max_columns":4,"max_children":50,"max_images":20,' ||
        '"max_buttons":12,"max_tables":10,"max_table_rows":50,"max_table_columns":6,"max_title_chars":200,' ||
        '"max_text_chars":5000,"max_total_chars":60000,"max_input_bytes":65536,"builders_enabled":["elementor"]}'
    )::jsonb;

    v_rows       int;
    v_entry      uuid;
    v_desc       text;
    v_usage_now  text;
    v_limits_now jsonb;
    v_updated    int;
BEGIN
    -- m155's writer lock for this name, then the row: the superadmin write
    -- and the stamp take the same two locks in the same order.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));

    SELECT count(*) INTO v_rows
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr';
    IF v_rows <> 1 THEN
        RAISE EXCEPTION 'm166: expected exactly one wpmgr catalogue row named %, found %', v_name, v_rows
            USING ERRCODE = 'P0002';
    END IF;

    SELECT "entry_id", "description", "usage", "limits"
      INTO v_entry, v_desc, v_usage_now, v_limits_now
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr'
       FOR UPDATE;

    IF (v_desc, v_usage_now, v_limits_now) IS NOT DISTINCT FROM (v_description, v_usage, v_limits) THEN
        RETURN;
    END IF;

    IF (v_desc, v_usage_now, v_limits_now) IS DISTINCT FROM (v_m162_description, v_m162_usage, v_m162_limits) THEN
        RAISE EXCEPTION 'm166: the copy of % was changed by a person; refusing to overwrite it', v_name
            USING ERRCODE = '55000',
                  HINT = 'Set its description, usage and limits back to the m162 values, or to the m166 values, ' ||
                         'through the superadmin catalogue write, then start the new revision again.';
    END IF;

    UPDATE "public"."ability_catalogue"
       SET "description"  = v_description,
           "usage"        = v_usage,
           "limits"       = v_limits,
           "entry_sha256" = NULL,
           "updated_at"   = now()
     WHERE "entry_id" = v_entry
       AND "source" = 'wpmgr'
       AND ("description", "usage", "limits") IS DISTINCT FROM (v_description, v_usage, v_limits);

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    IF v_updated <> 1 THEN
        RAISE EXCEPTION 'm166: updated % rows of %, expected 1', v_updated, v_name
            USING ERRCODE = 'P0002';
    END IF;
END;
$$;
