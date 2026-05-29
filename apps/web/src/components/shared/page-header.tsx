import type { ReactNode } from "react";
import { ArrowLeft } from "lucide-react";
import { Link, type LinkProps } from "@tanstack/react-router";

import { cn } from "@/lib/utils";

import { CopyableMono } from "./copyable-mono";

// PageHeader — the consistent detail/list page header (ADR-037 Batch 0). NOT a
// card: it is the page's top strip, so no card wrapper (DESIGN.md "two surface
// levels max / don't nest cards"). Title block left, actions right, subline
// below. Tokens only; mono titles for snapshot/run ids; verb-first actions.
//
//   • title     — the headline; `mono` renders it in font-mono (ids/hashes).
//   • copyable  — shows a copy button beside the title for the raw value.
//   • badges    — slot for KindBadge / StatusBadge / SeverityChip.
//   • subline   — relative time + facts ("Full backup · 2.4 GB · 14 components").
//   • actions   — right-aligned primary-action slot (e.g. the Restore button).
//   • backTo    — optional typed TanStack Router back link.

// Back link props mirror TanStack Router's typed Link so `to` + `params` are
// validated together against the route tree (no `any`, no loosened params).
export type PageHeaderBackTo = Pick<LinkProps, "to" | "params"> & {
  label: string;
};

export interface PageHeaderProps {
  title: ReactNode;
  /** Render the title in font-mono (snapshot / run ids, hashes). */
  mono?: boolean;
  /** Show a copy button beside the title for this raw value. */
  copyable?: string;
  badges?: ReactNode;
  subline?: ReactNode;
  actions?: ReactNode;
  backTo?: PageHeaderBackTo;
  className?: string;
}

export function PageHeader({
  title,
  mono = false,
  copyable,
  badges,
  subline,
  actions,
  backTo,
  className,
}: PageHeaderProps) {
  return (
    <div className={cn("flex flex-col gap-2", className)}>
      {backTo ? (
        <Link
          to={backTo.to}
          params={backTo.params}
          className="inline-flex w-fit items-center gap-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          <ArrowLeft aria-hidden="true" className="size-3.5" />
          {backTo.label}
        </Link>
      ) : null}

      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-col gap-1.5">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <h1
              className={cn(
                "min-w-0 truncate text-lg font-semibold text-foreground",
                mono && "font-mono",
              )}
            >
              {title}
            </h1>
            {copyable != null ? (
              <CopyableMono value={copyable} label="Copy id" />
            ) : null}
            {badges}
          </div>
          {subline != null ? (
            <div className="text-sm text-muted-foreground">{subline}</div>
          ) : null}
        </div>

        {actions != null ? (
          <div className="flex shrink-0 items-center gap-2">{actions}</div>
        ) : null}
      </div>
    </div>
  );
}
