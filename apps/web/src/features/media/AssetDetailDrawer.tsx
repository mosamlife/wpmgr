import type { ReactNode } from "react";
import { RotateCcw, Sparkles } from "lucide-react";

import { cn, formatBytes } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { StatusDot } from "@/components/status/status-dot";

import { AssetStatusChip } from "./AssetStatusChip";
import { FormatBadge } from "./FormatBadge";
import { SavingsBadge } from "./SavingsBadge";
import { useMediaJob } from "./hooks/useMediaJobs";
import type { MediaAsset, MediaVariant, VariantState } from "./types";

// AssetDetailDrawer — opens on an assets-table row click. Shows the asset's
// metadata, the per-variant breakdown (sourced from the asset's latest job),
// and re-optimize / restore actions. The UI library has no Sheet primitive
// (see error-detail-drawer.tsx), so we widen the shared Dialog to 720px — the
// established convention.

export interface AssetDetailDrawerProps {
  siteId: string;
  asset: MediaAsset | null;
  /** Latest job id for this asset, when known (drives the variant breakdown). */
  jobId: string | null;
  onClose: () => void;
  canOperate: boolean;
  onReoptimize: (asset: MediaAsset) => void;
  onRestore: (asset: MediaAsset) => void;
}

const VARIANT_TONE: Record<VariantState, "success" | "destructive" | "muted"> = {
  succeeded: "success",
  failed: "destructive",
  skipped: "muted",
};

export function AssetDetailDrawer({
  siteId,
  asset,
  jobId,
  onClose,
  canOperate,
  onReoptimize,
  onRestore,
}: AssetDetailDrawerProps) {
  const job = useMediaJob(siteId, asset ? jobId : null);

  if (!asset) return null;

  const canRestore =
    canOperate &&
    (asset.status === "optimized" || asset.status === "failed");
  const canReoptimize =
    canOperate &&
    asset.status !== "optimizing" &&
    asset.status !== "restoring" &&
    asset.status !== "originals_deleted" &&
    asset.status !== "excluded";

  return (
    <Dialog open={true} onClose={onClose}>
      <DialogContent
        ariaLabelledBy="asset-detail-title"
        ariaDescribedBy="asset-detail-desc"
        className="max-w-[min(720px,calc(100vw-2rem))]"
      >
        <DialogHeader>
          <DialogTitle id="asset-detail-title">
            <span className="flex flex-wrap items-center gap-2">
              <span className="truncate text-sm font-medium text-[var(--color-foreground)]">
                {asset.title || `Attachment #${asset.wp_attachment_id}`}
              </span>
              <AssetStatusChip status={asset.status} />
            </span>
          </DialogTitle>
          <DialogDescription id="asset-detail-desc">
            <span className="font-mono text-xs tabular-nums">
              #{asset.wp_attachment_id}
            </span>{" "}
            · <FormatBadge source={asset.original_mime} current={asset.current_format} />
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-5">
          {/* Size summary */}
          <div className="grid grid-cols-3 gap-px overflow-hidden rounded-md border border-[var(--color-border)] bg-[var(--color-border)]">
            <SizeTile label="Original" value={formatBytes(asset.original_size_bytes)} />
            <SizeTile label="Current" value={formatBytes(asset.current_size_bytes)} />
            <SizeTile
              label="Saved"
              valueNode={
                <SavingsBadge
                  originalBytes={asset.original_size_bytes}
                  currentBytes={asset.current_size_bytes}
                  className="text-sm"
                />
              }
            />
          </div>

          {/* Variant breakdown */}
          <div className="space-y-2">
            <h3 className="text-xs font-medium uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]">
              Variant breakdown
            </h3>
            {jobId === null ? (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                No optimization job has run for this attachment yet.
              </p>
            ) : job.isPending ? (
              <div className="space-y-2">
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
              </div>
            ) : job.isError ? (
              <PageError
                what="Could not load the variant breakdown."
                why={job.error.message}
                onRetry={() => void job.refetch()}
                retryLabel="Reload variants"
              />
            ) : job.data && job.data.variants.length > 0 ? (
              <ul className="divide-y divide-[var(--color-border)] rounded-md border border-[var(--color-border)]">
                {job.data.variants.map((v) => (
                  <VariantRow key={v.variant_name} variant={v} />
                ))}
              </ul>
            ) : (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                No variant results recorded for this job.
              </p>
            )}
          </div>
        </DialogBody>

        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onClose}>
            Close
          </Button>
          {canRestore ? (
            <Button
              type="button"
              variant="outline"
              onClick={() => onRestore(asset)}
            >
              <RotateCcw aria-hidden="true" className="size-4" />
              Restore
            </Button>
          ) : null}
          {canReoptimize ? (
            <Button type="button" onClick={() => onReoptimize(asset)}>
              <Sparkles aria-hidden="true" className="size-4" />
              {asset.status === "optimized" ? "Re-optimize" : "Optimize"}
            </Button>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function SizeTile({
  label,
  value,
  valueNode,
}: {
  label: string;
  value?: string;
  valueNode?: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-0.5 bg-[var(--color-card)] p-3">
      <span className="text-[11px] uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]">
        {label}
      </span>
      {valueNode ?? (
        <span className="text-sm font-medium tabular-nums text-[var(--color-foreground)]">
          {value}
        </span>
      )}
    </div>
  );
}

function VariantRow({ variant }: { variant: MediaVariant }) {
  const tone = VARIANT_TONE[variant.state];
  const saved =
    typeof variant.optimized_size_bytes === "number"
      ? variant.source_size_bytes - variant.optimized_size_bytes
      : null;
  return (
    <li className="flex items-center gap-3 px-3 py-2">
      <StatusDot tone={tone} />
      <span className="w-24 shrink-0 truncate font-mono text-xs text-[var(--color-foreground)]">
        {variant.variant_name}
      </span>
      <span className="flex-1 text-xs text-[var(--color-muted-foreground)]">
        {variant.state === "failed" && variant.reason
          ? variant.reason
          : variant.state === "skipped"
            ? variant.reason || "Skipped"
            : null}
      </span>
      <span className="shrink-0 font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]">
        {formatBytes(variant.source_size_bytes)}
        {typeof variant.optimized_size_bytes === "number" ? (
          <>
            {" → "}
            <span className={cn(tone === "success" && "text-[var(--color-foreground)]")}>
              {formatBytes(variant.optimized_size_bytes)}
            </span>
          </>
        ) : null}
      </span>
      {saved !== null && saved > 0 ? (
        <SavingsBadge
          originalBytes={variant.source_size_bytes}
          currentBytes={variant.optimized_size_bytes ?? variant.source_size_bytes}
          className="w-12 shrink-0 text-right"
        />
      ) : (
        <span className="w-12 shrink-0" aria-hidden="true" />
      )}
    </li>
  );
}
