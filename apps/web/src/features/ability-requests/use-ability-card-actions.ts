import { useState } from "react";
import { toast } from "sonner";
import type { AbilityRequest } from "@wpmgr/api";

import { CHANGED_COPY, UNDO_RETRY_COPY } from "./ability-card-model";
import { isPageEdit } from "./page-edit-model";
import {
  CODE_REQUEST_CHANGED,
  CODE_UNDO_RETRY,
  useApproveAbilityRequest,
  useDeclineAbilityRequest,
  useUndoAbilityRequest,
} from "./use-ability-requests";

/**
 * Approve, decline and undo handlers shared by the per-site list and the
 * organisation list. Each call is made for the request's own site. A refusal
 * becomes a per-card notice; the mutations refetch on settle, so a 503
 * "try again" leaves the card (and its Undo button) as the server now has it.
 */
export function useAbilityCardActions() {
  const approve = useApproveAbilityRequest();
  const decline = useDeclineAbilityRequest();
  const undo = useUndoAbilityRequest();
  const [notices, setNotices] = useState<Record<string, string>>({});

  const setNotice = (id: string, text: string | null) =>
    setNotices((prev) => {
      const next = { ...prev };
      if (text === null) delete next[id];
      else next[id] = text;
      return next;
    });

  function handleApprove(r: AbilityRequest) {
    if (r.presented_digest === undefined) {
      setNotice(r.id, "This request has no digest to approve with. Reload the page.");
      return;
    }
    setNotice(r.id, null);
    approve.mutate(
      { siteId: r.site_id, requestId: r.id, presentedDigest: r.presented_digest },
      {
        onSuccess: () =>
          toast.success(
            r.ability_name === "wpmgr/rest-write" || isPageEdit(r)
              ? "Approved. WPMgr will make the change shortly."
              : "Approved. WPMgr will create the draft shortly.",
          ),
        onError: (err) => setNotice(r.id, err.code === CODE_REQUEST_CHANGED ? CHANGED_COPY : err.message),
      },
    );
  }

  function handleDecline(r: AbilityRequest) {
    setNotice(r.id, null);
    decline.mutate(
      { siteId: r.site_id, requestId: r.id },
      {
        onSuccess: () => toast.success("Declined."),
        onError: (err) => setNotice(r.id, err.message),
      },
    );
  }

  function handleUndo(r: AbilityRequest) {
    setNotice(r.id, null);
    undo.mutate(
      { siteId: r.site_id, requestId: r.id },
      {
        onSuccess: (done) => {
          const rest = r.ability_name === "wpmgr/rest-write";
          const edit = isPageEdit(r);
          switch (done.undo_state) {
            case "undone":
              toast.success(edit ? "Change undone." : rest ? "Put back the way it was." : "Moved to the trash.");
              break;
            case "refused_published":
              toast.error(
                edit
                  ? "This page is published now. Change it in Elementor."
                  : rest
                    ? "WPMgr could not undo this change."
                    : "The draft has been published since, so WPMgr left it alone.",
              );
              break;
            case "refused_conflict":
              toast.error(
                edit
                  ? "WPMgr won't undo: the page changed after this change."
                  : rest
                    ? "Someone edited this page after the change, so WPMgr left it as it is."
                    : "The draft was edited since, so WPMgr left it alone.",
              );
              break;
            case "failed":
              toast.error(
                edit
                  ? "WPMgr could not undo this change. Check the page in Elementor."
                  : rest
                    ? "WPMgr could not put the old title back. Check the page in WordPress."
                    : "WPMgr could not move the draft to the trash.",
              );
              break;
            default:
              break;
          }
        },
        onError: (err) =>
          setNotice(r.id, err.status === 503 && err.code === CODE_UNDO_RETRY ? UNDO_RETRY_COPY : err.message),
      },
    );
  }

  return {
    notices,
    handleApprove,
    handleDecline,
    handleUndo,
    approvePendingId: approve.isPending ? approve.variables?.requestId : undefined,
    declinePendingId: decline.isPending ? decline.variables?.requestId : undefined,
    undoPendingId: undo.isPending ? undo.variables?.requestId : undefined,
  };
}
