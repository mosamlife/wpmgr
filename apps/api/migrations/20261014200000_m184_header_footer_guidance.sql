-- m184: header and footer draft guidance in wpmgr/page-create's
-- description and wpmgr/page-edit's usage, including the widget operations
-- and the widget schema's write_rules. page-create's usage stays as m177
-- leaves it; page-edit's description stays as m169 leaves it.
--
-- Each row changes only its named copy column, entry_sha256 and updated_at.
-- Every other column, including the kill switch, limits and agent floor,
-- stays as the operator and the preceding migrations left it.
--
-- Description and usage are part of the canonical entry bytes sent to the
-- agent. A changed row must clear entry_sha256 to NULL so m157's boot stamp
-- fills it from the new bytes. Approvals against the previous hash close
-- not_sent / entry_changed rather than sending a changed entry. The clear
-- follows m177 and m178: the migration does not write an audit row, and the
-- boot stamp records its own NULL-actor audit row.
--
-- Under m155's per-name advisory lock, then the row lock, the current text
-- decides independently for each row:
--   * already this migration's text -> nothing changes, including the hash
--                                     and timestamp on a re-run.
--   * exactly the preceding text    -> one guarded UPDATE, clearing the hash
--                                     and setting updated_at to now().
--   * anything else, or no row      -> RAISE WARNING and change nothing.
-- The preceding text is m177's description for page-create and m178's usage
-- for page-edit. An operator's text is theirs to keep, and does not prevent
-- the other row from converging. WARNING reaches the default server log;
-- NOTICE has no handler in the migration runner.
--
-- To bring a skipped row up to date, set its copy to this migration's text
-- through the superadmin catalogue write, which stamps the hash in that
-- write. This is a guarded data change, with no DDL, definer or grant.

DO $$
DECLARE
    v_name constant text := 'wpmgr/page-create';

    v_m177_description constant text :=
        'Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, ' ||
        'separators, images already in the site''s media library, and, in the block editor or Elementor, ' ||
        'buttons, spacing, sections and columns. With editor builder:elementor the draft is built in ' ||
        'Elementor, from Elementor''s own layout elements and widgets. With outline_version 3 it is a designed ' ||
        'page: sections with tones, backgrounds and anchors, and patterns such as a hero, features, stats, ' ||
        'pricing, testimonials, questions and a call to action, styled from one design of colours, fonts and ' ||
        'spacing. Nothing is published. Undo moves the draft to the trash.';

    v_description constant text :=
        'Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, ' ||
        'separators, images already in the site''s media library, and, in the block editor or Elementor, ' ||
        'buttons, spacing, sections and columns. With editor builder:elementor the draft is built in ' ||
        'Elementor, from Elementor''s own layout elements and widgets. With outline_version 3 it is a designed ' ||
        'page: sections with tones, backgrounds and anchors, and patterns such as a hero, features, stats, ' ||
        'pricing, testimonials, questions and a call to action, styled from one design of colours, fonts and ' ||
        'spacing. Nothing is published. Undo moves the draft to the trash. With template header or footer ' ||
        'instead of post_type it makes an Elementor Pro header or footer draft; read ' ||
        'wpmgr/elementor-theme-templates first.';

    v_rows     int;
    v_entry    uuid;
    v_copy_now text;
    v_updated  int;
BEGIN
    -- The superadmin write and the stamp take these locks in this order.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));

    SELECT count(*) INTO v_rows
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr';
    IF v_rows <> 1 THEN
        RAISE WARNING 'm184: expected exactly one wpmgr catalogue row named %, found %; its copy was not changed',
            v_name, v_rows;
        RETURN;
    END IF;

    SELECT "entry_id", "description"
      INTO v_entry, v_copy_now
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr'
       FOR UPDATE;

    IF v_copy_now IS NOT DISTINCT FROM v_description THEN
        RETURN;
    END IF;

    IF v_copy_now IS DISTINCT FROM v_m177_description THEN
        RAISE WARNING 'm184: the description of % was changed by a person; leaving it as it is', v_name
            USING HINT = 'To describe header and footer drafts, set its description to the m184 text ' ||
                         'through the superadmin catalogue write.';
        RETURN;
    END IF;

    UPDATE "public"."ability_catalogue"
       SET "description" = v_description,
           "entry_sha256" = NULL,
           "updated_at" = now()
     WHERE "entry_id" = v_entry
       AND "source" = 'wpmgr'
       AND "description" IS NOT DISTINCT FROM v_m177_description;

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    IF v_updated <> 1 THEN
        RAISE EXCEPTION 'm184: updated % rows of %, expected 1', v_updated, v_name
            USING ERRCODE = 'P0002';
    END IF;
END;
$$;

DO $$
DECLARE
    v_name constant text := 'wpmgr/page-edit';

    v_m178_usage constant text :=
        'First read the page with wpmgr/page-structure. Send post_id, base_fingerprint (from that read) and ' ||
        'operations: 1 to 25 changes, applied in order. set_text sets one field (text, url, alt or caption) ' ||
        'of a node whose editable list names it; in a pattern, name the field by its path, such as ' ||
        'plans.1.price or faq.3.answer. insert adds outline items after or before a node, or into a node, ' ||
        'first or last. replace puts outline items in place of a node; to add or remove items in a pattern, ' ||
        'replace the pattern. remove deletes a node. move puts a node after or before another. Outline items ' ||
        'use the wpmgr/page-create outline, at most 50 in one change, and all text follows its rules: plain ' ||
        'text only, and links that are https:// addresses or paths on this site that start with /. On a ' ||
        'designed page, insert and replace also take version 3 sections and patterns, built with the design ' ||
        'the page was created with unless you send design, complete, as wpmgr/page-create takes it; they need ' ||
        'the WPMgr plugin 0.61.165 or later. A #anchor link must name an anchor on the page after the whole ' ||
        'request (link_anchor_missing), and a section whose anchor is still linked cannot be removed ' ||
        '(anchor_in_use). Change each node at most once in a request, and never name a node an earlier change ' ||
        'in the same request removed or replaced. Locked nodes cannot be changed, moved or removed. If the ' ||
        'page changed after your read, the request is refused with conflict: read the page again and send new ' ||
        'changes. Only a draft WPMgr created with wpmgr/page-create, still a draft, can be changed; any other ' ||
        'page is refused with target_not_eligible.';

    v_edit_usage constant text :=
        'First read the page with wpmgr/page-structure. Send post_id, base_fingerprint (from that read) and ' ||
        'operations: 1 to 25 changes, applied in order. set_text sets one field (text, url, alt or caption) ' ||
        'of a node whose editable list names it; in a pattern, name the field by its path, such as ' ||
        'plans.1.price or faq.3.answer. insert adds outline items after or before a node, or into a node, ' ||
        'first or last. replace puts outline items in place of a node; to add or remove items in a pattern, ' ||
        'replace the pattern. remove deletes a node. move puts a node after or before another. Outline items ' ||
        'use the wpmgr/page-create outline, at most 50 in one change, and all text follows its rules: plain ' ||
        'text only, and links that are https:// addresses or paths on this site that start with /. On a ' ||
        'designed page, insert and replace also take version 3 sections and patterns, built with the design ' ||
        'the page was created with unless you send design, complete, as wpmgr/page-create takes it; they need ' ||
        'the WPMgr plugin 0.61.165 or later. A #anchor link must name an anchor on the page after the whole ' ||
        'request (link_anchor_missing), and a section whose anchor is still linked cannot be removed ' ||
        '(anchor_in_use). Change each node at most once in a request, and never name a node an earlier change ' ||
        'in the same request removed or replaced. Locked nodes cannot be changed, moved or removed. If the ' ||
        'page changed after your read, the request is refused with conflict: read the page again and send new ' ||
        'changes. Only a draft WPMgr created with wpmgr/page-create, still a draft, can be changed; any other ' ||
        'page is refused with target_not_eligible. set_native, add_native and duplicate change Elementor ' ||
        'widgets: read wpmgr/elementor-widget-schema and follow its write_rules, and send them apart from ' ||
        'text or outline changes. A WPMgr header or footer draft is changed the same way; set_shows_on ' ||
        '{rules} replaces where it shows, and without it the places stay.';

    v_rows     int;
    v_entry    uuid;
    v_copy_now text;
    v_updated  int;
BEGIN
    -- The superadmin write and the stamp take these locks in this order.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));

    SELECT count(*) INTO v_rows
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr';
    IF v_rows <> 1 THEN
        RAISE WARNING 'm184: expected exactly one wpmgr catalogue row named %, found %; its copy was not changed',
            v_name, v_rows;
        RETURN;
    END IF;

    SELECT "entry_id", "usage"
      INTO v_entry, v_copy_now
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr'
       FOR UPDATE;

    IF v_copy_now IS NOT DISTINCT FROM v_edit_usage THEN
        RETURN;
    END IF;

    IF v_copy_now IS DISTINCT FROM v_m178_usage THEN
        RAISE WARNING 'm184: the usage of % was changed by a person; leaving it as it is', v_name
            USING HINT = 'To describe header and footer drafts, set its usage to the m184 text ' ||
                         'through the superadmin catalogue write.';
        RETURN;
    END IF;

    UPDATE "public"."ability_catalogue"
       SET "usage" = v_edit_usage,
           "entry_sha256" = NULL,
           "updated_at" = now()
     WHERE "entry_id" = v_entry
       AND "source" = 'wpmgr'
       AND "usage" IS NOT DISTINCT FROM v_m178_usage;

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    IF v_updated <> 1 THEN
        RAISE EXCEPTION 'm184: updated % rows of %, expected 1', v_updated, v_name
            USING ERRCODE = 'P0002';
    END IF;
END;
$$;
