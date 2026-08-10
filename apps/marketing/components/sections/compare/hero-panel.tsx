import { Icon } from "@/components/ui/icon";
import { cn } from "@/lib/utils";

/**
 * The hero visual: a compact fleet panel.
 *
 * WHY A PRODUCT SURFACE RATHER THAN AN ILLUSTRATION. The right half of the
 * hero was empty, and the obvious fill would have been an abstract graphic.
 * On a comparison page a reader is deciding what to run every day, and the
 * most persuasive thing to show them is the thing itself: a fleet, at a
 * glance, with live status. It also costs no illustrator and cannot go stale
 * the way a drawing of a product does.
 *
 * It reuses the status vocabulary from ops-status.tsx on purpose, so the page
 * and the dashboard look like the same product rather than two design systems.
 *
 * MOTION. The only animation is the status pulse, which is a scale and opacity
 * loop on a DECORATIVE ring behind each dot; the dot itself is solid and
 * static. Nothing here depends on animation to be legible, and the ping is
 * suppressed under prefers-reduced-motion by the global media query in
 * globals.css. No layout property is animated.
 *
 * Rows are illustrative and use example.com hosts on purpose. Real customer
 * domains never go in marketing artwork.
 */

type Row = {
  host: string;
  status: "up" | "degraded";
  meta: string;
};

const ROWS: Row[] = [
  { host: "shop.example.com", status: "up", meta: "backed up 2h ago" },
  { host: "blog.example.com", status: "up", meta: "backed up 3h ago" },
  { host: "client-a.example.com", status: "up", meta: "backed up 4h ago" },
  { host: "staging.example.com", status: "degraded", meta: "update pending" },
  { host: "legacy.example.com", status: "up", meta: "backed up 6h ago" },
];

function Pulse({ status }: { status: Row["status"] }) {
  return (
    <span className="relative inline-flex h-2 w-2 shrink-0" aria-hidden>
      <span
        className={cn(
          "absolute inline-flex h-full w-full rounded-full opacity-60",
          status === "up" && "animate-ping bg-[var(--success)]",
          status === "degraded" && "bg-[var(--warning-subtle-fg)]",
        )}
      />
      <span
        className={cn(
          "relative inline-flex h-2 w-2 rounded-full",
          status === "up" ? "bg-[var(--success)]" : "bg-[var(--warning-subtle-fg)]",
        )}
      />
    </span>
  );
}

export function CompareHeroPanel() {
  return (
    <div className="rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm">
      <div className="flex items-center justify-between gap-3 border-b border-[var(--border)] pb-3">
        <div className="flex items-center gap-2">
          <Icon name="Layers" size={16} className="text-[var(--primary)]" aria-hidden />
          <span className="text-sm font-semibold text-foreground">Your fleet</span>
        </div>
        <span className="text-xs text-[var(--muted-foreground)]">5 sites, one dashboard</span>
      </div>

      <ul className="mt-3 flex flex-col">
        {ROWS.map((r) => (
          <li
            key={r.host}
            className="flex items-center justify-between gap-3 border-b border-[var(--border)]/50 py-2.5 last:border-0"
          >
            <span className="flex min-w-0 items-center gap-2.5">
              <Pulse status={r.status} />
              {/* The dot is the only thing carrying up versus degraded, and it
                  carries it in colour alone. The wrapper stays decorative
                  rather than taking role="img", because labelling it would put
                  the pulse ring back into the accessibility tree; the state
                  goes in a sibling instead. */}
              <span className="sr-only">{r.status === "up" ? "Up" : "Degraded"}: </span>
              <span className="truncate font-mono text-xs text-foreground">{r.host}</span>
            </span>
            <span className="shrink-0 text-[11px] text-[var(--muted-foreground)]">{r.meta}</span>
          </li>
        ))}
      </ul>

      <div className="mt-3 flex items-center gap-2 rounded-lg bg-[var(--primary-subtle)] px-3 py-2.5">
        <Icon name="Lock" size={14} className="text-[var(--primary-pressed)]" aria-hidden />
        <span className="text-[11px] leading-snug text-[var(--primary-pressed)]">
          Backups encrypted on each site before they leave it, to storage you choose.
        </span>
      </div>
    </div>
  );
}
