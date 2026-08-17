"use client";

import { motion } from "motion/react";
import { Icon } from "@/components/ui/icon";
import { Button } from "@/components/ui/button";
import { Container, Section } from "@/components/ui/primitives";
import { cn } from "@/lib/utils";
import { Reveal } from "@/components/motion/reveal";
import { newTabRel } from "@/lib/site";
import type { Cta, Chip } from "@/lib/content/types";

// ---------------------------------------------------------------------------
// Ported from apps/landing: CountUp (client component, uses useEffect)
// ---------------------------------------------------------------------------

import { useEffect, useRef, useState } from "react";

function prefersReducedMotion() {
  return (
    typeof window !== "undefined" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

function CountUp({
  value,
  durationMs = 1100,
  format = (n: number) => Math.round(n).toLocaleString("en-US"),
  suffix = "",
  className,
}: {
  value: number;
  durationMs?: number;
  format?: (n: number) => string;
  suffix?: string;
  className?: string;
}) {
  const ref = useRef<HTMLSpanElement>(null);
  const [display, setDisplay] = useState(() => (prefersReducedMotion() ? value : 0));

  useEffect(() => {
    const el = ref.current;
    if (!el || prefersReducedMotion()) {
      setDisplay(value);
      return;
    }
    let raf = 0;
    let start = 0;
    let done = false;

    const step = (t: number) => {
      if (!start) start = t;
      const p = Math.min(1, (t - start) / durationMs);
      const eased = 1 - Math.pow(1 - p, 5);
      setDisplay(value * eased);
      if (p < 1) raf = requestAnimationFrame(step);
    };

    const io = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting && !done) {
          done = true;
          raf = requestAnimationFrame(step);
          io.disconnect();
        }
      },
      { threshold: 0.4 },
    );
    io.observe(el);
    return () => {
      io.disconnect();
      cancelAnimationFrame(raf);
    };
  }, [value, durationMs]);

  return (
    <span ref={ref} className={className} style={{ fontVariantNumeric: "tabular-nums" }}>
      {format(display)}
      {suffix}
    </span>
  );
}

// ---------------------------------------------------------------------------
// ByteBar: horizontal byte comparison bar
// ---------------------------------------------------------------------------

const MB = (bytes: number) => `${(bytes / 1_000_000).toFixed(2)} MB`;

type LibrarySegment = { label: string; pct: number; tone: "success" | "warning" | "muted" };

const SEG_FILL: Record<LibrarySegment["tone"], string> = {
  success: "bg-[var(--success)]",
  warning: "bg-[var(--warning-subtle-fg)]",
  muted: "bg-[var(--muted-foreground)]/35",
};
const SEG_DOT: Record<LibrarySegment["tone"], string> = {
  success: "bg-[var(--success)]",
  warning: "bg-[var(--warning-subtle-fg)]",
  muted: "bg-[var(--muted-foreground)]/45",
};

function ByteBar({
  label,
  bytes,
  ratio,
  accent,
}: {
  label: string;
  bytes: number;
  ratio: number;
  accent: "neutral" | "teal";
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-baseline justify-between">
        <span className="font-mono text-xs font-medium text-[var(--muted-foreground)]">{label}</span>
        <span className="font-mono text-sm font-medium text-foreground" style={{ fontVariantNumeric: "tabular-nums" }}>
          {MB(bytes)}
        </span>
      </div>
      <div className="relative h-8 w-full overflow-hidden rounded-md border border-dashed border-[var(--border)] bg-[var(--background)]">
        <motion.div
          className={cn(
            "absolute inset-y-0 left-0 w-full origin-left rounded-[5px]",
            accent === "teal" ? "bg-primary" : "bg-[var(--muted-foreground)]/30",
          )}
          initial={{ scaleX: 0 }}
          whileInView={{ scaleX: ratio }}
          viewport={{ once: true, margin: "-60px" }}
          transition={{ duration: 0.8, ease: [0.22, 1, 0.36, 1] }}
        />
      </div>
    </div>
  );
}

function BeforeAfterCard({
  caption,
  originalLabel,
  originalBytes,
  optimizedLabel,
  optimizedBytes,
  library,
}: {
  caption: string;
  originalLabel: string;
  originalBytes: number;
  optimizedLabel: string;
  optimizedBytes: number;
  library: LibrarySegment[];
}) {
  const ratio = optimizedBytes / originalBytes;
  const savedBytes = originalBytes - optimizedBytes;
  const savedPct = Math.round((savedBytes / originalBytes) * 100);

  return (
    <div className="flex flex-col gap-6 rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm">
      <div className="flex items-center justify-between gap-3 border-b border-[var(--border)] pb-4">
        <span className="text-sm font-semibold text-foreground">Bytes saved on a sample upload</span>
        <span className="font-mono text-xs text-[var(--muted-foreground)]">{caption}</span>
      </div>
      <div className="flex flex-col gap-4">
        <ByteBar label={originalLabel} bytes={originalBytes} ratio={1} accent="neutral" />
        <ByteBar label={optimizedLabel} bytes={optimizedBytes} ratio={ratio} accent="teal" />
      </div>
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-t border-[var(--border)] pt-4">
        <CountUp
          value={savedPct}
          suffix="%"
          format={(n) => `${Math.round(n)}`}
          className="font-mono text-3xl font-semibold text-[var(--primary)]"
        />
        <span className="text-sm font-medium text-foreground">smaller on this sample,</span>
        <CountUp
          value={savedBytes / 1_000_000}
          suffix=" MB"
          format={(n) => n.toFixed(2)}
          className="font-mono text-sm font-medium text-[var(--muted-foreground)]"
        />
        <span className="text-sm text-[var(--muted-foreground)]">lighter, full image and thumbnails counted.</span>
      </div>
      <div className="flex flex-col gap-2.5">
        <span className="text-xs font-medium text-[var(--muted-foreground)]">Library coverage</span>
        <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-[var(--muted)]">
          {library.map((seg) => (
            <div key={seg.label} className={cn("h-full", SEG_FILL[seg.tone])} style={{ width: `${seg.pct}%` }} />
          ))}
        </div>
        <div className="flex flex-wrap gap-x-5 gap-y-1.5">
          {library.map((seg) => (
            <span key={seg.label} className="inline-flex items-center gap-1.5 text-xs text-[var(--muted-foreground)]">
              <span className={cn("h-2 w-2 rounded-full", SEG_DOT[seg.tone])} />
              {seg.label}
              <span className="font-mono" style={{ fontVariantNumeric: "tabular-nums" }}>{seg.pct}%</span>
            </span>
          ))}
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// MediaShowcase section
// ---------------------------------------------------------------------------

export function MediaShowcase({
  eyebrow,
  heading,
  subhead,
  chips,
  cta,
  demo,
}: {
  eyebrow: string;
  heading: string;
  subhead: string;
  chips: Chip[];
  cta: Cta;
  demo: {
    caption: string;
    originalLabel: string;
    originalBytes: number;
    optimizedLabel: string;
    optimizedBytes: number;
    library: LibrarySegment[];
  };
}) {
  const trailing = cta.icon === "ArrowRight";
  return (
    <Section id="media" tone="muted">
      <Container>
        <div className="grid gap-12 lg:grid-cols-2 lg:gap-16 items-center">
          {/* Left: copy */}
          <Reveal className="flex flex-col gap-6">
            <div className="flex flex-col gap-4">
              {eyebrow && (
                <span className="inline-flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
                  <span aria-hidden className="h-px w-6 bg-[var(--primary)]/60" />
                  {eyebrow}
                </span>
              )}
              <h2 className="text-3xl font-semibold text-foreground sm:text-4xl">{heading}</h2>
              <p className="text-lg leading-relaxed text-[var(--muted-foreground)]">{subhead}</p>
            </div>

            {/* Chip grid */}
            <div className="grid grid-cols-2 gap-3">
              {chips.map((chip) => (
                <div
                  key={chip.value}
                  className="flex items-start gap-3 rounded-[var(--radius)] border border-[var(--border)] bg-card p-4"
                >
                  <span className="mt-0.5 text-[var(--primary)]">
                    <Icon name={chip.icon} size={18} />
                  </span>
                  <div className="flex flex-col gap-0.5">
                    <span className="font-mono text-sm font-medium text-foreground" style={{ fontVariantNumeric: "tabular-nums" }}>
                      {chip.value}
                    </span>
                    <span className="text-xs leading-relaxed text-[var(--muted-foreground)]">{chip.label}</span>
                  </div>
                </div>
              ))}
            </div>

            <Button
              href={cta.href}
              variant={cta.variant ?? "primary"}
              size="lg"
              className="self-start"
              {...(cta.href.startsWith("http") ? { target: "_blank", rel: newTabRel(cta.href) } : {})}
            >
              {cta.icon && !trailing ? <Icon name={cta.icon} size={18} /> : null}
              {cta.label}
              {cta.icon && trailing ? <Icon name={cta.icon} size={18} /> : null}
            </Button>
          </Reveal>

          {/* Right: demo widget */}
          <Reveal delay={0.08}>
            <BeforeAfterCard {...demo} />
          </Reveal>
        </div>
      </Container>
    </Section>
  );
}
