"use client";

import { useEffect, useRef, useState } from "react";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import type { ComparisonPageData } from "@/lib/content/types";

/**
 * The consolidation visual: eight separately-bought tools wired into one core.
 *
 * THE RESTING STATE SHOWS ALL EIGHT TILES. That sentence is the whole fix. The
 * first version animated the tiles INTO the core, so the state every reader
 * ended on was eight tiles stacked invisibly underneath it, and the section
 * rendered as a lone teal square with a heading. Reduced-motion users got that
 * state immediately and never saw anything else.
 *
 * The lesson is worth keeping, because the verification was what failed: the
 * tiles were confirmed present in the static HTML, which was true and useless.
 * Being in the DOM is not being visible. Check the rendered state, not the
 * markup.
 *
 * SO THE MOTION IS A SETTLE, NOT A COLLAPSE. Tiles start a little further out
 * and ease inward to their ring positions, and the spokes grow from the core.
 * "One tool" is carried by the spokes converging, not by hiding anything.
 *
 * Only `transform` is animated. Never opacity, never a layout property, so
 * every tile is readable at its final position before any script runs, in any
 * renderer, including with JavaScript off.
 */

// Eight positions on a ring, as percentages of the panel box. Hand-placed
// rather than computed so labels never collide at the narrow breakpoint.
const RING = [
  { x: -32, y: -30 },
  { x: 0, y: -38 },
  { x: 32, y: -30 },
  { x: 40, y: 0 },
  { x: 32, y: 30 },
  { x: 0, y: 38 },
  { x: -32, y: 30 },
  { x: -40, y: 0 },
];

export function StackCollapse({ data }: { data: ComparisonPageData }) {
  const ref = useRef<HTMLDivElement | null>(null);
  // `settled` only controls a small inward drift. Both states are visible, so
  // a reader who never triggers it loses nothing.
  const [settled, setSettled] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      setSettled(true);
      return;
    }
    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) if (e.isIntersecting) setSettled(true);
      },
      { rootMargin: "-10% 0px -10% 0px" },
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

        {/* Desktop and tablet: the radial. */}
        <div
          ref={ref}
          className="relative mx-auto mt-16 hidden h-[420px] w-full max-w-3xl sm:block"
        >
          {/* Spokes, behind everything. Scale from the core outward. */}
          <svg
            className="absolute inset-0 h-full w-full"
            viewBox="0 0 100 100"
            preserveAspectRatio="none"
            aria-hidden
          >
            {RING.map((p, i) => (
              <line
                key={i}
                x1="50"
                y1="50"
                x2={50 + p.x}
                y2={50 + p.y}
                stroke="var(--border)"
                strokeWidth="0.35"
                vectorEffect="non-scaling-stroke"
                style={{
                  transformOrigin: "50% 50%",
                  transform: settled ? "scale(1)" : "scale(0.86)",
                  transition: "transform 800ms cubic-bezier(0.22,1,0.36,1)",
                  transitionDelay: `${i * 45}ms`,
                }}
              />
            ))}
          </svg>

          {/* The core. */}
          <div className="absolute left-1/2 top-1/2 z-10 -translate-x-1/2 -translate-y-1/2">
            <div className="flex h-28 w-28 flex-col items-center justify-center gap-1.5 rounded-2xl border border-[var(--primary)]/35 bg-[var(--primary-subtle)] text-[var(--primary-pressed)] shadow-sm">
              <Icon name="Layers" size={24} aria-hidden />
              <span className="text-xs font-semibold">WPMgr</span>
              <span className="text-[10px] leading-none opacity-80">one system</span>
            </div>
          </div>

          {items.map((item, i) => {
            const p = RING[i]!;
            // Settled sits ON the ring. Unsettled is 14% further out. Both are
            // fully visible; the difference is only a small drift.
            // POSITION comes from left/top, whose percentages resolve against
            // the PANEL. The drift comes from a transform in px.
            //
            // This split is the bug that shipped: the first version put the
            // ring offsets inside `transform: translate(...%)`, and percentages
            // there resolve against the ELEMENT's own box, not the container.
            // A w-32 tile at "45%" moved 58px, not 45% of a 768px panel, so all
            // eight sat within 58px of centre, behind a 112px core. The section
            // rendered as a lone teal square.
            const drift = settled ? 0 : 14;
            const dx = (p.x / 40) * drift;
            const dy = (p.y / 40) * drift;
            return (
              <div
                key={item.label}
                className="absolute w-32"
                style={{
                  left: `${50 + p.x}%`,
                  top: `${50 + p.y}%`,
                  transform: `translate(-50%, -50%) translate(${dx}px, ${dy}px)`,
                  transition: "transform 800ms cubic-bezier(0.22,1,0.36,1)",
                  transitionDelay: `${i * 45}ms`,
                }}
              >
                <div className="flex flex-col items-center gap-1.5 rounded-xl border border-[var(--border)] bg-card px-3 py-2.5 text-center shadow-sm">
                  <Icon
                    name={item.icon}
                    size={16}
                    className="text-[var(--muted-foreground)]"
                    aria-hidden
                  />
                  <span className="text-[11px] leading-tight text-foreground">{item.label}</span>
                </div>
              </div>
            );
          })}
        </div>

        {/* Mobile: the radial does not fit, so the same argument as a grid
            feeding one core. No absolute positioning, nothing to overlap. */}
        <div className="mt-10 sm:hidden">
          <ul className="grid grid-cols-2 gap-2.5">
            {items.map((item) => (
              <li
                key={item.label}
                className="flex items-center gap-2 rounded-xl border border-[var(--border)] bg-card px-3 py-2.5 shadow-sm"
              >
                <Icon
                  name={item.icon}
                  size={15}
                  className="shrink-0 text-[var(--muted-foreground)]"
                  aria-hidden
                />
                <span className="text-[11px] leading-tight text-foreground">{item.label}</span>
              </li>
            ))}
          </ul>
          <div className="mt-4 flex justify-center text-[var(--muted-foreground)]">
            <Icon name="ArrowDown" size={20} aria-hidden />
          </div>
          <div className="mt-4 flex items-center justify-center gap-2 rounded-xl border border-[var(--primary)]/35 bg-[var(--primary-subtle)] px-4 py-4 text-[var(--primary-pressed)]">
            <Icon name="Layers" size={20} aria-hidden />
            <span className="text-sm font-semibold">WPMgr, one system</span>
          </div>
        </div>
      </Container>
    </Section>
  );
}
