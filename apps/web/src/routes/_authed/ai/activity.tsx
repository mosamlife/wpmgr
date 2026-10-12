import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { z } from "zod";

import { PageHeader } from "@/components/shared/page-header";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { useMe } from "@/features/auth/use-auth";
import { useAiConnections } from "@/features/ai-connections/use-ai-connections";
import { AiAreaTabs } from "@/features/ai-requests/ai-area-tabs";
import { AiActivityList } from "@/features/ai-trust/ai-activity-list";
import type { ActivityFilter } from "@/features/ai-trust/use-ai-trust";
import { useSites } from "@/features/sites/use-sites";

// /ai/activity: everything an AI connection changed on the organisation's
// sites, who or which setting allowed it, and Undo (design §8.7). A tab beside
// Connections and Requests.
//
// ROUTE PLACEMENT. Under _authed/: every row names one of this tenant's sites.
// NO SIDEBAR ENTRY, the same as /ai/requests: it is reached from the AI area's
// tab bar, and the sidebar's AI entry stays active under /ai.

const FILTERS: ReadonlyArray<{ value: ActivityFilter; label: string }> = [
  { value: "all", label: "All" },
  { value: "ran_automatically", label: "Ran automatically" },
  { value: "approved_by_person", label: "Approved by a person" },
  { value: "failed_or_unknown", label: "Failed or result unknown" },
  { value: "undone", label: "Undone" },
];

const searchSchema = z.object({
  filter: z.enum(["all", "ran_automatically", "approved_by_person", "failed_or_unknown", "undone"]).optional(),
  site: z.string().optional(),
  connection: z.string().optional(),
});

type ActivitySearch = z.infer<typeof searchSchema>;

export const Route = createFileRoute("/_authed/ai/activity")({
  validateSearch: searchSchema,
  component: AiActivityPage,
});

function AiActivityPage() {
  const search = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });
  const { data: me } = useMe();
  const sites = useSites();
  const connections = useAiConnections();

  const filter: ActivityFilter = search.filter ?? "all";
  const siteId = search.site || undefined;
  const grantId = search.connection || undefined;

  function setSearch(patch: Partial<ActivitySearch>) {
    void navigate({
      search: (prev: ActivitySearch) => {
        const next: ActivitySearch = { ...prev, ...patch };
        if (next.filter === "all") delete next.filter;
        if (!next.site) delete next.site;
        if (!next.connection) delete next.connection;
        return next;
      },
      replace: true,
    });
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="AI connections"
        subline="Everything an AI connection changed, who or which setting allowed it, and Undo."
      />
      <AiAreaTabs />

      <div className="grid gap-3 sm:grid-cols-3">
        <div className="space-y-1">
          <Label htmlFor="ai-activity-filter">Show</Label>
          <Select
            id="ai-activity-filter"
            value={filter}
            onChange={(e) => setSearch({ filter: e.target.value as ActivityFilter })}
          >
            {FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </Select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="ai-activity-site">Site</Label>
          <Select
            id="ai-activity-site"
            value={siteId ?? ""}
            disabled={!sites.isSuccess}
            onChange={(e) => setSearch({ site: e.target.value })}
          >
            <option value="">All sites</option>
            {(sites.data ?? []).map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </Select>
        </div>
        {/* Connections are listed only to people who can manage them; for
            anyone else the filter is not offered rather than offered empty. */}
        {connections.isSuccess ? (
          <div className="space-y-1">
            <Label htmlFor="ai-activity-connection">Connection</Label>
            <Select
              id="ai-activity-connection"
              value={grantId ?? ""}
              onChange={(e) => setSearch({ connection: e.target.value })}
            >
              <option value="">All connections</option>
              {connections.data.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </Select>
          </div>
        ) : null}
      </div>

      <AiActivityList
        filters={{ filter, siteId, grantId }}
        currentUserId={me?.user?.id ?? null}
        refetchInterval={30_000}
      />
    </div>
  );
}
