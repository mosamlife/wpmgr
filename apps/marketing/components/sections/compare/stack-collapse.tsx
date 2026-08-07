"use client";

import { useEffect, useRef, useState } from "react";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import type { ComparisonPageData } from "@/lib/content/types";

/**
 * The consolidation visual: eight separately-bought tools converging into one.
 *
 * This is the page's hero visual because it IS the commercial argument. The
 * matrix proves parity row by row; this shows in one glance what the reader is
 * actually buying, which is one system instead of eight subscriptions.
 *
 * WHY IT IS NOT A CARD GRID. The tiles are not the content, the MOVEMENT is:
 * they start scattered on a ring and converge on a single core. A static grid
 * of icon-plus-label tiles would be the filler pattern the house rules ban, and
 * would say nothing a list could not.
 *
 * MOTION RULES OBSERVED:
 *   Only `transform` is animated. Never opacity, never a layout property. So
 *   there is no renderer anywhere, headless or print or JavaScript-off, in
 *   which any of this content is invisible. The tiles are in the DOM, readable
 *   and positioned, before any script runs; the animation only moves them.
 *   prefers-reduced-motion lands them in their final positions immediately.
 */

// Eight positions on a ring, as fractions of the panel box. Hand-placed rather
// than computed so the labels never collide at the narrow breakpoint.
const RING = [
  { x: -0.34, y: -0.30 },
  { x: 0.0, y: -0.38 },
  { x: 0.34, y: -0.30 },
  { x: 0.42, y: 0.0 },
  { x: 0.34, y: 0.30 },
  { x: 0.0, y: 0.38 },
  { x: -0.34, y: 0.30 },
  { x: -0.42, y: 0.0 },
];

export function StackCollapse({ data }: { data: ComparisonPageData }) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [collapsed, setCollapsed] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;

    // Reduced motion: land in the final state and never animate.
    const reduce = window.matchMedia("(prefers-reduced-motion: reduce)");
    if (reduce.matches) {
      setCollapsed(true);
      return;
    }

    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) if (e.isIntersecting) setCollapsed(true);
      },
      { rootMargin: "-15% 0px -15% 0px" },
    );
    io.observe(el);
    return () => io.disconnect();
  }, []);

  const items = data.replaces.items.slice(0, RING.length);

  return (
    <Section id="replaces">
      <Container>
        <div className="mx-auto max-w-2xl text-center">
          <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            {data.replaces.heading}
          </h2>
          <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
            {data.replaces.subhead}
          </p>
        </div>

        <div
          ref={ref}
          className="relative mx-auto mt-14 h-[340px] w-full max-w-3xl sm:h-[420px]"
        >
          {/* The core. Always visible, never animated in. */}
          <div className="absolute left-1/2 top-1/2 z-10 -translate-x-1/2 -translate-y-1/2">
            <div className="flex h-24 w-24 flex-col items-center justify-center gap-1 rounded-2xl border border-[var(--primary)]/30 bg-[var(--primary-subtle)] text-[var(--primary-pressed)] sm:h-28 sm:w-28">
              <Icon name="Layers" size={22} aria-hidden />
              <span className="text-xs font-semibold">WPMgr</span>
            </div>
          </div>

          {items.map((item, i) => {
            const pos = RING[i]!;
            // Collapsed: sit on the core. Expanded: out on the ring.
            const tx = collapsed ? 0 : pos.x * 100;
            const ty = collapsed ? 0 : pos.y * 100;
            const scale = collapsed ? 0.62 : 1;
            return (
              <div
                key={item.label}
                className="absolute left-1/2 top-1/2 w-28 sm:w-32"
                style={{
                  transform: `translate3d(calc(-50% + ${tx}%), calc(-50% + ${ty}%), 0) scale(${scale})`,
                  transition:
                    "transform 900ms cubic-bezier(0.22, 1, 0.36, 1)",
                  transitionDelay: `${i * 55}ms`,
                }}
              >
                <div className="flex flex-col items-center gap-1.5 rounded-xl border border-[var(--border)] bg-card px-3 py-2.5 text-center shadow-sm">
                  <Icon
                    name={item.icon}
                    size={16}
                    className="text-[var(--muted-foreground)]"
                    aria-hidden
                  />
                  <span className="text-[11px] leading-tight text-[var(--muted-foreground)]">
                    {item.label}
                  </span>
                </div>
              </div>
            );
          })}
        </div>
      </Container>
    </Section>
  );
}
