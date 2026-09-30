import { useEffect, useMemo, useState } from "react";
import { FileText, RefreshCw } from "lucide-react";

import { PageError } from "@/components/feedback/page-error";
import { PageHeader } from "@/components/shared/page-header";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { relativeTime } from "@/lib/utils";

import {
  AI_EDIT_NOT_ON,
  editorLabel,
  pageTitle,
  reasonCopy,
} from "./content-copy";
import {
  RefreshRateLimitedError,
  useContentInventory,
  useRefreshContentInventory,
} from "./use-content";

// Site Content tab (Track B slice S1). Read-only: lists the published pages the
// agent has checked and which editor owns each. Nothing is editable yet, so the
// third column is one fixed sentence with the later refusal reason beneath it.
// Every site-supplied string (title, builder name, version) is a React text
// node. Never dangerouslySetInnerHTML.

const CLASSIC = "classic";

export interface ContentTabProps {
  siteId: string;
  hostname: string;
  canOperate: boolean;
}

export function ContentTab({ siteId, hostname, canOperate }: ContentTabProps) {
  const [cursors, setCursors] = useState<(number | null)[]>([null]);
  const [editor, setEditor] = useState("");
  const [search, setSearch] = useState("");
  const [knownEditors, setKnownEditors] = useState<Record<string, string>>({});

  const after = cursors[cursors.length - 1] ?? null;
  const inv = useContentInventory(siteId, after, editor);
  const refresh = useRefreshContentInventory(siteId);

  const data = inv.data;
  useEffect(() => {
    if (!data) return;
    const found: Record<string, string> = {};
    for (const p of data.pages) {
      if (p.editor?.display_name) found[p.editor.integration_id] = p.editor.display_name;
    }
    if (Object.keys(found).length === 0) return;
    setKnownEditors((prev) => ({ ...prev, ...found }));
  }, [data]);

  const visible = useMemo(() => {
    if (!data) return [];
    const q = search.trim().toLowerCase();
    if (!q || !data.titles_included) return data.pages;
    return data.pages.filter((p) => (p.title ?? "").toLowerCase().includes(q));
  }, [data, search]);

  const rateLimited =
    refresh.error instanceof RefreshRateLimitedError ? refresh.error : null;
  const refreshFailed = refresh.isError && !rateLimited;

  const checked = data ? relativeTime(data.last_checked_at) : null;

  const refreshButton = canOperate ? (
    <Button
      size="sm"
      variant="outline"
      onClick={() => refresh.mutate()}
      disabled={refresh.isPending}
    >
      <RefreshCw aria-hidden="true" className="size-4" />
      Refresh
    </Button>
  ) : null;

  const header = (
    <PageHeader
      title="Content"
      subline={`Pages WPMgr has checked (published only)${hostname ? ` on ${hostname}` : ""}`}
      actions={
        <div className="flex items-center gap-3">
          {checked ? (
            <span className="text-sm text-[var(--color-muted-foreground)]">
              Checked {checked}
            </span>
          ) : null}
          {refreshButton}
        </div>
      }
    />
  );

  const notices = (
    <>
      {rateLimited ? (
        <p role="status" className="text-sm text-[var(--color-muted-foreground)]">
          Checked recently. Try again in {rateLimited.retryAfterSeconds} seconds.
        </p>
      ) : null}
      {refreshFailed ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          Could not start a check. Try again shortly.
        </p>
      ) : null}
      {refresh.isSuccess ? (
        <p role="status" className="text-sm text-[var(--color-muted-foreground)]">
          Check requested. The list updates when the site replies.
        </p>
      ) : null}
    </>
  );

  if (inv.isPending) {
    return (
      <div className="space-y-4">
        {header}
        <div role="status" aria-label="Loading pages" className="space-y-2">
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
        </div>
      </div>
    );
  }

  if (inv.isError) {
    return (
      <div className="space-y-4">
        {header}
        <PageError
          what="Could not load this site's pages"
          why="The control plane did not answer. Nothing was changed."
          onRetry={() => void inv.refetch()}
          isRetrying={inv.isFetching}
        />
      </div>
    );
  }

  if (data!.state === "agent_update_needed") {
    return (
      <div className="space-y-4">
        {header}
        <p
          role="status"
          className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4 text-sm"
        >
          Update the WPMgr plugin on this site to version {data!.min_agent_version} to
          see its pages.
        </p>
      </div>
    );
  }

  if (data!.state === "not_connected") {
    return (
      <div className="space-y-4">
        {header}
        <p
          role="status"
          className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4 text-sm"
        >
          This site is not connected right now. Reconnect it to see its pages.
        </p>
      </div>
    );
  }

  const neverChecked = data!.last_checked_at == null && data!.pages.length === 0;
  if (neverChecked && editor === "" && cursors.length === 1) {
    return (
      <div className="space-y-4">
        {header}
        {notices}
        <div
          role="status"
          aria-label="No pages checked yet"
          className="flex flex-col items-center gap-3 py-12 text-center"
        >
          <FileText aria-hidden="true" className="size-8 text-[var(--color-muted-foreground)]" />
          <p className="text-sm font-medium">This site's pages have not been checked yet</p>
          <p className="max-w-md text-sm text-[var(--color-muted-foreground)]">
            A check lists the published pages and which editor each one uses.
          </p>
          {canOperate ? (
            <Button onClick={() => refresh.mutate()} disabled={refresh.isPending}>
              Refresh
            </Button>
          ) : null}
        </div>
      </div>
    );
  }

  const titlesIncluded = data!.titles_included;
  const onEditor = (v: string) => {
    setEditor(v);
    setCursors([null]);
  };

  return (
    <div className="space-y-4">
      {header}
      {notices}
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-sm">
          Search
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search titles"
            disabled={!titlesIncluded}
            className="w-56"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          Show
          <Select
            value={editor}
            onChange={(e) => onEditor(e.target.value)}
            className="w-56"
          >
            <option value="">All editors</option>
            <option value={CLASSIC}>WordPress (classic)</option>
            {Object.entries(knownEditors).map(([id, name]) => (
              <option key={id} value={id}>
                {name}
              </option>
            ))}
          </Select>
        </label>
      </div>
      {!titlesIncluded ? (
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Page titles need operator access, so pages are shown by number.
        </p>
      ) : null}
      {visible.length === 0 ? (
        <p role="status" className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">
          No pages match.
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{titlesIncluded ? "Title (from the site)" : "Page"}</TableHead>
              <TableHead>Edited with</TableHead>
              <TableHead>AI can change text?</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {visible.map((row) => {
              const ed = editorLabel(row);
              return (
                <TableRow key={row.post_id}>
                  <TableCell>{pageTitle(row, titlesIncluded)}</TableCell>
                  <TableCell>
                    {ed.name}
                    {ed.version ? ` ${ed.version}` : ""}
                  </TableCell>
                  <TableCell>
                    <div>{AI_EDIT_NOT_ON}</div>
                    <div className="text-xs text-[var(--color-muted-foreground)]">
                      {reasonCopy(row.route_reason)}
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
      <div className="flex items-center justify-end gap-2">
        <span className="text-sm text-[var(--color-muted-foreground)]">
          Page {cursors.length}
        </span>
        <Button
          size="sm"
          variant="outline"
          disabled={cursors.length === 1}
          onClick={() => setCursors((c) => c.slice(0, -1))}
        >
          Previous
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={data!.next_after_post_id == null}
          onClick={() =>
            setCursors((c) =>
              data!.next_after_post_id != null ? [...c, data!.next_after_post_id] : c,
            )
          }
        >
          Next
        </Button>
      </div>
    </div>
  );
}
