import { Archive } from "lucide-react";

import { Button } from "@/components/ui/button";

// Surface 4.12 — inline empty state for a site's Backups section when no
// snapshots exist yet. Calmer than a "set up backups" call to action: in WPMgr
// nightly backups are the default, so the headline states the schedule rather
// than prompting the operator to configure something. The "Or run one now."
// secondary line gives them an immediate verb-first action without making it
// feel like setup work.

export interface NoBackupsEmptyProps {
  /**
   * Override the default schedule summary. Defaults to "nightly at 02:00 UTC"
   * — most tenants run on a UTC nightly window, callers with a custom
   * schedule pass a pre-formatted human string ("every 6 hours", "Mon/Wed/Fri
   * at 14:00 UTC", etc).
   */
  scheduleSummary?: string;
  /**
   * Invoked when the operator activates "Run backup now". Wire to
   * `useCreateBackup(siteId).mutate({ kind: "full" })` at the call site.
   */
  onRunNow: () => void;
  /** Disable the action while a mutation is in flight. */
  isPending?: boolean;
}

export function NoBackupsEmpty({
  scheduleSummary = "nightly at 02:00 UTC",
  onRunNow,
  isPending,
}: NoBackupsEmptyProps) {
  return (
    <div
      role="status"
      aria-label="No backups yet"
      className="flex flex-col items-center gap-3 py-12 text-center"
    >
      <Archive
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-[var(--color-muted-foreground)]/50"
      />
      <div className="space-y-1">
        <p className="text-balance text-sm font-medium text-[var(--color-foreground)]">
          Backups run {scheduleSummary}.
        </p>
        <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
          Or run one now.
        </p>
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={onRunNow}
        disabled={isPending}
      >
        Run backup now
      </Button>
    </div>
  );
}
