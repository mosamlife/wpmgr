"use client";

import { useMemo, useState } from "react";
import { Container, Section } from "@/components/ui/primitives";
import { Icon } from "@/components/ui/icon";
import { signupHref } from "@/lib/site";
import type { ComparisonPageData, CostModel } from "@/lib/content/types";

/**
 * The cost widget. The reader sets their own fleet size and watches the number
 * compute from each vendor's published prices.
 *
 * WHY THIS EXISTS RATHER THAN A PRICE TABLE. Per-site pricing looks small at
 * ten sites and is the same multiplication at two hundred. A reader who moves
 * the slider to their own number does that multiplication themselves, and a
 * figure you worked out yourself is worth more than one we assert.
 *
 * IT MUST NOT OVERSTATE A COMPETITOR. ManageWP is charged at the CHEAPEST of
 * per-site, per-add-on bundles, and their all-in-one package, because that is
 * what a real customer would pay. Quietly dropping the cheapest option would
 * inflate a named company's published price, which is the one mistake this
 * page cannot survive.
 *
 * The CTA lives here on purpose: this is the highest-intent moment on the page,
 * the instant the reader sees their own annual number.
 */

const STOPS = [5, 10, 25, 50, 100, 250];

function annualCost(m: CostModel, sites: number): number {
  const perYear = (v: number) => (m.period === "month" ? v * 12 : v);

  if (m.flat !== undefined) return perYear(m.flat);

  const bySite = m.perSite * sites;
  const options = [bySite];
  if (m.bundle !== undefined && m.bundleCovers) {
    options.push(m.bundle * Math.ceil(sites / m.bundleCovers));
  }
  return perYear(Math.min(...options));
}

function money(n: number): string {
  return n === 0 ? "$0" : `$${n.toLocaleString("en-US")}`;
}

export function CostModelSection({ data }: { data: ComparisonPageData }) {
  const [idx, setIdx] = useState(2); // default 25 sites
  const sites = STOPS[idx] ?? 25;

  const rows = useMemo(() => {
    const computed = data.cost.models.map((m) => ({ m, total: annualCost(m, sites) }));
    const max = Math.max(...computed.map((r) => r.total), 1);
    return computed.map((r) => ({ ...r, pct: Math.max(2, Math.round((r.total / max) * 100)) }));
  }, [data.cost.models, sites]);

  return (
    <Section tone="muted" id="cost" className="border-y border-[var(--border)]">
      <Container>
        <div className="max-w-2xl">
          <h2 className="text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
            {data.cost.heading}
          </h2>
          <p className="mt-4 text-lg leading-relaxed text-[var(--muted-foreground)]">
            {data.cost.subhead}
          </p>
        </div>

        <div className="mt-10 rounded-xl border border-[var(--border)] bg-card p-6 shadow-sm sm:p-8">
          <label htmlFor="fleet-size" className="text-sm font-medium text-foreground">
            Fleet size
          </label>
          <div className="mt-3 flex items-center gap-4">
            <input
              id="fleet-size"
              type="range"
              min={0}
              max={STOPS.length - 1}
              step={1}
              value={idx}
              onChange={(e) => setIdx(Number(e.target.value))}
              className="h-2 w-full max-w-md cursor-pointer appearance-none rounded-full bg-[var(--border)] accent-[var(--primary)]"
              aria-valuetext={`${sites} sites`}
            />
            <span
              className="min-w-[5.5rem] font-mono text-lg font-medium text-foreground"
              style={{ fontVariantNumeric: "tabular-nums" }}
            >
              {sites} sites
            </span>
          </div>

          <ul className="mt-8 flex flex-col gap-5">
            {rows.map(({ m, total, pct }) => (
              <li key={m.productKey}>
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="text-sm font-medium text-foreground">{m.label}</span>
                  <span
                    className="font-mono text-lg font-medium text-foreground"
                    style={{ fontVariantNumeric: "tabular-nums" }}
                  >
                    {money(total)}
                    <span className="ml-1 text-xs font-normal text-[var(--muted-foreground)]">
                      per year
                    </span>
                  </span>
                </div>
                <div className="mt-2 h-2 w-full overflow-hidden rounded-full bg-[var(--muted)]">
                  <div
                    className={
                      m.productKey === "wpmgr"
                        ? "h-full rounded-full bg-[var(--primary)]"
                        : "h-full rounded-full bg-[var(--muted-foreground)]/45"
                    }
                    style={{ width: `${pct}%`, transition: "width 320ms cubic-bezier(0.22,1,0.36,1)" }}
                  />
                </div>
                <p className="mt-2 text-xs leading-relaxed text-[var(--muted-foreground)]">
                  {m.note}
                  {m.cites?.map((id) => (
                    <a
                      key={id}
                      href={`/compare/${data.slug}/sources#${id}`}
                      className="ml-1 align-super underline underline-offset-2 hover:text-foreground"
                      aria-label={`Source, reference ${id}`}
                    >
                      {id.split("-")[1]}
                    </a>
                  ))}
                </p>
              </li>
            ))}
          </ul>

          <div className="mt-8 flex flex-wrap items-center gap-3 border-t border-[var(--border)] pt-6">
            <a
              href={signupHref("compare")}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex h-11 items-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-6 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors duration-[var(--duration-fast)] hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              Start free at {sites} sites
              <Icon name="ArrowRight" size={16} aria-hidden />
            </a>
            <span className="text-sm text-[var(--muted-foreground)]">
              No card, and self-hosting stays free at any size.
            </span>
          </div>
        </div>
      </Container>
    </Section>
  );
}
