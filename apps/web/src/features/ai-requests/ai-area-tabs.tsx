import { Link } from "@tanstack/react-router";

import { useAssistantRequests } from "./use-ai-requests";

// The "AI connections [Connections] [Requests · 2]" tab bar (§2.6). Two real
// TanStack Router links, same pattern as the site-detail tab bar
// ($siteId.tsx's TABS) -- the router owns which one is active, this
// component only supplies the count.
//
// THE BADGE READS pending_count, NEVER THE PAGE LENGTH. The queue route caps
// what it returns per page; pending_count is a server-computed total over
// every row still waiting, so the badge cannot under-count once a fleet has
// more waiting requests than fit on one page.
//
// A FAILED OR STILL-LOADING COUNT RENDERS AS NO BADGE, NEVER AS "0". Zero is
// a claim -- "nothing is waiting" -- and this component has not established
// that when the query has not resolved.

const TAB_LINK_CLASS =
  "inline-flex h-10 items-center border-b-2 border-transparent px-1 text-sm text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2";
const TAB_LINK_ACTIVE_CLASS =
  "inline-flex h-10 items-center -mb-px border-b-2 border-primary px-1 text-sm font-medium text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2";

export function AiAreaTabs() {
  const query = useAssistantRequests();
  const pendingCount = query.isSuccess ? query.data.pending_count : null;

  return (
    <nav aria-label="AI connections" className="flex items-center gap-6 border-b border-border">
      <Link
        to="/ai"
        activeOptions={{ exact: true }}
        className={TAB_LINK_CLASS}
        activeProps={{ className: TAB_LINK_ACTIVE_CLASS }}
      >
        Connections
      </Link>
      <Link to="/ai/requests" className={TAB_LINK_CLASS} activeProps={{ className: TAB_LINK_ACTIVE_CLASS }}>
        Requests{pendingCount !== null ? ` · ${pendingCount}` : ""}
      </Link>
    </nav>
  );
}
