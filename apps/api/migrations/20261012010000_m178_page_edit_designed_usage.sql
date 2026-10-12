-- m178: wpmgr/page-edit's usage describes changes to designed Elementor
-- pages, and a hash clear so the boot stamp re-stamps the entry.
--
-- One row, one copy column, one guarded UPDATE.
--
-- ===========================================================================
-- WHAT CHANGES
-- ===========================================================================
--
-- The ability_catalogue row named wpmgr/page-edit (source wpmgr), and only
-- these columns:
--
--   * usage         m169's text, with four additions: set_text names a
--                   field inside a pattern by its path (plans.1.price,
--                   faq.3.answer); items are added to or removed from a
--                   pattern by replacing the pattern; on a designed page,
--                   insert and replace take version 3 sections and patterns,
--                   built with the design the page was created with unless
--                   the request sends a complete design, and need the WPMgr
--                   plugin 0.61.165; a #anchor link must resolve after the
--                   whole request (link_anchor_missing), and a section whose
--                   anchor is still linked cannot be removed (anchor_in_use).
--                   The 0.61.165 it names must equal the control plane's
--                   designed-pages floor constant.
--   * entry_sha256  cleared to NULL.
--   * updated_at    now().
--
-- Every other column is left as it is: description, limits and
-- min_agent_version (the builder-edit floor; the designed-pages floor is
-- enforced per input by the control plane), the kill switch, and any column
-- a later migration classifies.
--
-- ===========================================================================
-- WHY THE HASH CLEAR IS MANDATORY
-- ===========================================================================
--
-- entry_sha256 is the sha256 of the canonical entry bytes the control plane
-- sends, and those bytes include usage. A stored hash that no longer matches
-- the bytes refuses every send of the entry (entry changed). Clearing it lets
-- the boot stamp (m157's stamp_wpmgr_ability_entry_hash, which only moves
-- NULL to a hash on a wpmgr row) fill it from the new bytes when the new
-- revision starts. m176 is the precedent, and like it the clear is recorded
-- by this migration and not in ability_catalogue_audit: the audit's
-- NULL-actor CHECK admits only the stamp shape, and the stamp that follows
-- writes its own audit row.
--
-- Consequence, by design and fail closed: a page-edit request approved
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
--   * still m169's usage             -> the UPDATE runs (exactly one row).
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
-- To bring a skipped row up to date: set its usage to this migration's text
-- through the superadmin catalogue write, which stamps the hash in the same
-- write.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m178 is new and is a guarded data change. No DDL, no table,
-- no definer, no grant. A database that ran m169 converges through the second
-- branch of the guard. A later migration that rewrites page-edit's usage
-- converges from this migration's text, not m169's.

DO $$
DECLARE
    v_name constant text := 'wpmgr/page-edit';

    -- m169's usage: the state m178 meets on every install that has not run it.
    v_m169_usage constant text :=
        'First read the page with wpmgr/page-structure. Send post_id, base_fingerprint (from that read) and ' ||
        'operations: 1 to 25 changes, applied in order. set_text sets one field (text, url, alt or caption) of a ' ||
        'node whose editable list names it. insert adds outline items after or before a node, or into a node, first ' ||
        'or last. replace puts outline items in place of a node. remove deletes a node. move puts a node after or ' ||
        'before another. Outline items use the wpmgr/page-create outline, at most 50 in one change, and all text ' ||
        'follows its rules: plain text only, and links that are https:// addresses or paths on this site that start ' ||
        'with /. Change each node at most once in a request, and never name a node an earlier change in the same ' ||
        'request removed or replaced. Locked nodes cannot be changed, moved or removed. If the page changed after ' ||
        'your read, the request is refused with conflict: read the page again and send new changes. Only a draft ' ||
        'WPMgr created with wpmgr/page-create, still a draft, can be changed; any other page is refused with ' ||
        'target_not_eligible.';

    v_edit_usage constant text :=
        'First read the page with wpmgr/page-structure. Send post_id, base_fingerprint (from that read) and ' ||
        'operations: 1 to 25 changes, applied in order. set_text sets one field (text, url, alt or caption) of a ' ||
        'node whose editable list names it; in a pattern, name the field by its path, such as plans.1.price or ' ||
        'faq.3.answer. insert adds outline items after or before a node, or into a node, first or last. replace ' ||
        'puts outline items in place of a node; to add or remove items in a pattern, replace the pattern. remove ' ||
        'deletes a node. move puts a node after or before another. Outline items use the wpmgr/page-create ' ||
        'outline, at most 50 in one change, and all text follows its rules: plain text only, and links that are ' ||
        'https:// addresses or paths on this site that start with /. On a designed page, insert and replace also ' ||
        'take version 3 sections and patterns, built with the design the page was created with unless you send ' ||
        'design, complete, as wpmgr/page-create takes it; they need the WPMgr plugin 0.61.165 or later. A ' ||
        '#anchor link must name an anchor on the page after the whole request (link_anchor_missing), and a ' ||
        'section whose anchor is still linked cannot be removed (anchor_in_use). Change each node at most once ' ||
        'in a request, and never name a node an earlier change in the same request removed or replaced. Locked ' ||
        'nodes cannot be changed, moved or removed. If the page changed after your read, the request is refused ' ||
        'with conflict: read the page again and send new changes. Only a draft WPMgr created with ' ||
        'wpmgr/page-create, still a draft, can be changed; any other page is refused with target_not_eligible.';

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
        RAISE WARNING 'm178: expected exactly one wpmgr catalogue row named %, found %; its usage was not changed',
            v_name, v_rows;
        RETURN;
    END IF;

    SELECT "entry_id", "usage"
      INTO v_entry, v_usage_now
      FROM "public"."ability_catalogue"
     WHERE "name" = v_name AND "source" = 'wpmgr'
       FOR UPDATE;

    IF v_usage_now IS NOT DISTINCT FROM v_edit_usage THEN
        RETURN;
    END IF;

    IF v_usage_now IS DISTINCT FROM v_m169_usage THEN
        RAISE WARNING 'm178: the usage of % was changed by a person; leaving it as it is', v_name
            USING HINT = 'To describe changes to designed pages, set its usage to the m178 text through the ' ||
                         'superadmin catalogue write.';
        RETURN;
    END IF;

    UPDATE "public"."ability_catalogue"
       SET "usage"        = v_edit_usage,
           "entry_sha256" = NULL,
           "updated_at"   = now()
     WHERE "entry_id" = v_entry
       AND "source" = 'wpmgr'
       AND "usage" IS NOT DISTINCT FROM v_m169_usage;

    GET DIAGNOSTICS v_updated = ROW_COUNT;
    IF v_updated <> 1 THEN
        RAISE EXCEPTION 'm178: updated % rows of %, expected 1', v_updated, v_name
            USING ERRCODE = 'P0002';
    END IF;
END;
$$;
