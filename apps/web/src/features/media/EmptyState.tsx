import { ImageOff } from "lucide-react";

import { Button } from "@/components/ui/button";

// EmptyState — shown when a site has no synced media yet. Mirrors the calm
// NoBackupsEmpty pattern: state the next action plainly, offer a verb-first
// button, no "set up" framing. The headline tells the operator what a sync does.

export interface MediaEmptyStateProps {
  /** Invoked on "Run sync" — wire to useSyncMedia(siteId).mutate(). */
  onSync: () => void;
  /** Disable while a sync request is in flight. */
  isPending?: boolean;
  /** Operators only; viewers see the copy without the action. */
  canOperate?: boolean;
}

export function MediaEmptyState({
  onSync,
  isPending,
  canOperate = true,
}: MediaEmptyStateProps) {
  return (
    <div
      role="status"
      aria-label="No media synced yet"
      className="flex flex-col items-center gap-3 py-12 text-center"
    >
      <ImageOff
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-[var(--color-muted-foreground)]/50"
      />
      <div className="space-y-1">
        <p className="text-balance text-sm font-medium text-[var(--color-foreground)]">
          Run a sync to enumerate this site's media library.
        </p>
        <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
          We mirror each attachment's metadata and optimization state. No image
          bytes leave the site until you optimize.
        </p>
      </div>
      {canOperate ? (
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={onSync}
          disabled={isPending}
        >
          Run sync
        </Button>
      ) : null}
    </div>
  );
}
