import { Toaster as SonnerToaster } from "sonner";
import { CheckCircle2, XCircle, AlertCircle, Info } from "lucide-react";

import { useThemeStore } from "@/lib/theme-store";

// Surface 4.15 — Toasts via Sonner. The container chrome is wired to the same
// DESIGN.md tokens we use for popovers and dropdowns, so a toast always reads
// as "the same elevated surface that just gave you the menu". Position is
// bottom-right because top-right collides with the topbar and notification
// bell (Sprint 1). closeButton is intentionally OFF: the verb action IS the
// dismissal — we never offer an "OK" or a stand-alone X. The action button is
// the one CTA per toast (Undo / View details / Open snapshot / Try again).
//
// Color choices are token-driven (not Sonner's `richColors`) so light/dark
// switching stays consistent with the rest of the shell. The semantic icon at
// 16px disambiguates success vs error vs destructive vs info without leaning
// on hue alone (WCAG: information not by color).
//
// Motion: Sonner ships a slight slide+fade on its own, and the global
// `prefers-reduced-motion` rule in `globals.css` collapses any third-party
// animation to ~0ms. We do not override Sonner's keyframes — staying with the
// library default keeps SSR/CSR parity and lets us upgrade cleanly.
export function Toaster() {
  const theme = useThemeStore((s) => s.theme);

  return (
    <SonnerToaster
      position="bottom-right"
      theme={theme}
      richColors={false}
      closeButton={false}
      visibleToasts={5}
      gap={8}
      // 360px max — matches DESIGN.md popover sizing. We set it via inline
      // style on the toast classname slot rather than a Tailwind arbitrary
      // value so Sonner's internal mobile-stack behavior keeps working.
      toastOptions={{
        // The toast container itself: popover bg, border, shadow-md, rounded.
        // 12px padding (DESIGN.md "popover" elevation tier).
        // `!` prefix on token classes overrides Sonner's default inline styles.
        classNames: {
          toast: [
            "!bg-[var(--color-popover)]",
            "!text-[var(--color-popover-foreground)]",
            "!border",
            "!border-[var(--color-border)]",
            "!shadow-md",
            "!rounded-md",
            "!p-3",
            "!w-[360px]",
            "!max-w-[360px]",
            "!font-sans",
            "!items-start",
            "!gap-3",
          ].join(" "),
          title: "!text-sm !font-medium !leading-5",
          description:
            "!text-xs !text-[var(--color-muted-foreground)] !mt-1 !leading-4",
          actionButton: [
            "!bg-transparent",
            "!text-[var(--color-primary)]",
            "hover:!text-[var(--color-primary-hover)]",
            "focus-visible:!outline-none",
            "focus-visible:!ring-2",
            "focus-visible:!ring-[var(--color-primary)]",
            "focus-visible:!ring-offset-2",
            "!text-sm",
            "!font-medium",
            "!ml-auto",
            "!px-2",
            "!py-1",
            "!rounded-md",
          ].join(" "),
          icon: "!mt-0.5",
          // Per-type icon containers stay neutral; the lucide icon below
          // carries the semantic color via text-* utility class.
          success: "",
          error: "",
          info: "",
          warning: "",
        },
      }}
      icons={{
        // 16px lucide icons (DESIGN.md "Icons from lucide-react at 16/20/24px").
        // Color is mapped to the semantic token so a destructive and an error
        // both read red but a success reads emerald — the same hues the status
        // dots use elsewhere in the shell.
        success: (
          <CheckCircle2
            aria-hidden="true"
            className="size-4 text-[var(--color-success)]"
          />
        ),
        error: (
          <XCircle
            aria-hidden="true"
            className="size-4 text-[var(--color-destructive)]"
          />
        ),
        warning: (
          <AlertCircle
            aria-hidden="true"
            className="size-4 text-[var(--color-warning)]"
          />
        ),
        info: (
          <Info
            aria-hidden="true"
            className="size-4 text-[var(--color-muted-foreground)]"
          />
        ),
      }}
      containerAriaLabel="Notifications"
    />
  );
}
