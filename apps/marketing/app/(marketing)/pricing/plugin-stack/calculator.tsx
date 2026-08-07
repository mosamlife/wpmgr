"use client";

import { useEffect, useReducer, useRef, useState } from "react";
import { Icon } from "@/components/ui/icon";
import { cn } from "@/lib/utils";
import { signupHref } from "@/lib/site";
import {
  PLUGIN_COST_CATEGORIES,
  PER_SITE_KEYS,
  SITE_PRESETS,
  annualCost,
  resolveTier,
  resolveWpmgrTier,
} from "@/lib/content/plugin-costs";

/**
 * The plugin-stack calculator.
 *
 * THE ILLUSTRATION IS THE ASYMMETRY. Two bills sit side by side, top-aligned,
 * against the same fleet size: one runs seven lines, the other runs one. No
 * drawing makes that argument faster than the two columns themselves, which is
 * why there is no drawing. It also cannot go stale, because both columns are
 * computed from the same figures the page already has to be honest about.
 *
 * EVERY LINE IS THE READER'S TO REMOVE. Categories toggle off, and the
 * performance line lets them pick which product represents it. An argument a
 * reader can disassemble is one they end up trusting; a fixed total that
 * assumes they buy all seven is one they dismiss, correctly, as a strawman.
 *
 * MOTION IS RESPONSE, NEVER ENTRANCE. The total tweens between values so a
 * change in fleet size is felt rather than just read, and a deselected line
 * fades. Nothing here reveals on scroll, and that is a deliberate reversal: an
 * earlier draft staggered the seven rows in on entry, and a screenshot caught
 * six of them still at opacity 0 while sitting fully inside the viewport. They
 * did resolve for a real reader, but a bill's line items ARE the content, and
 * content that waits on an IntersectionObserver is content that ships blank in
 * every renderer where the observer never fires. The rows are now plain markup
 * that is legible before any script runs, with JavaScript off, and in a
 * crawler. The only scripted change is the total's digits.
 */

/** "$1,234" and "$99.90", never "$1234.00". */
function money(n: number): string {
  const exact = Math.round(n * 100) / 100;
  return `$${exact.toLocaleString("en-US", {
    minimumFractionDigits: Number.isInteger(exact) ? 0 : 2,
    maximumFractionDigits: 2,
  })}`;
}

function usePrefersReducedMotion() {
  const [reduced, setReduced] = useState(false);
  useEffect(() => {
    const mq = window.matchMedia("(prefers-reduced-motion: reduce)");
    setReduced(mq.matches);
    const on = () => setReduced(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return reduced;
}

/**
 * A number that tweens toward its target, and is never WRONG on the way.
 *
 * THE TWEEN CANNOT BE THE SOURCE OF TRUTH. `tween` is null whenever no
 * animation is in flight, and the span falls back to the real `value`, so the
 * figure is correct on first paint, with JavaScript off, under reduced motion,
 * and in any renderer where requestAnimationFrame never runs.
 *
 * That last case is not hypothetical. An earlier version stored the displayed
 * amount in state and only ever updated it from inside a rAF callback. In a
 * backgrounded tab, where rAF is throttled to zero frames, every line item
 * updated correctly while the headline total sat frozen at a stale number
 * indefinitely. The page's whole argument is that total, so it is exactly the
 * thing that must not depend on an animation frame.
 *
 * ACCESSIBILITY. The animated digits are deliberately NOT in a live region: a
 * tween fires dozens of renders and would announce every intermediate frame.
 * A visually hidden sibling carries the settled figure and is polite, so a
 * screen reader hears the total once per change.
 */
const TWEEN_MS = 420;

function TweenedMoney({ value, className }: { value: number; className?: string }) {
  const reduced = usePrefersReducedMotion();
  const [tween, setTween] = useState<number | null>(null);
  const fromRef = useRef(value);
  const rafRef = useRef<number | null>(null);

  useEffect(() => {
    const from = fromRef.current;
    fromRef.current = value;
    if (reduced || from === value) {
      setTween(null);
      return;
    }
    const start = performance.now();
    const step = (now: number) => {
      const t = Math.min(1, (now - start) / TWEEN_MS);
      if (t >= 1) {
        // Hand control back to `value` rather than parking on a computed
        // number, so the settled state is the truth and not a rounding of it.
        setTween(null);
        return;
      }
      // ease-out-quart, matching the site's motion curve.
      setTween(from + (value - from) * (1 - Math.pow(1 - t, 4)));
      rafRef.current = requestAnimationFrame(step);
    };
    rafRef.current = requestAnimationFrame(step);
    return () => {
      if (rafRef.current !== null) cancelAnimationFrame(rafRef.current);
      setTween(null);
    };
  }, [value, reduced]);

  return (
    <>
      <span className={className} aria-hidden>
        {money(Math.round(tween ?? value))}
      </span>
      <span className="sr-only" aria-live="polite">
        {money(Math.round(value))} per year
      </span>
    </>
  );
}

type State = {
  sites: number;
  off: string[];
  /** category key to index into that category's products */
  pick: Record<string, number>;
};

type Action =
  | { type: "sites"; value: number }
  | { type: "toggle"; key: string }
  | { type: "pick"; key: string; index: number };

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case "sites":
      return { ...state, sites: action.value };
    case "toggle":
      return {
        ...state,
        off: state.off.includes(action.key)
          ? state.off.filter((k) => k !== action.key)
          : [...state.off, action.key],
      };
    case "pick":
      return { ...state, pick: { ...state.pick, [action.key]: action.index } };
  }
}

const INITIAL: State = { sites: 25, off: [], pick: {} };

export function PluginStackCalculator() {
  const [state, dispatch] = useReducer(reducer, INITIAL);
  const { sites, off } = state;

  const lines = PLUGIN_COST_CATEGORIES.map((category) => {
    const index = state.pick[category.key] ?? 0;
    const product = category.products[index] ?? category.products[0]!;
    return {
      category,
      product,
      index,
      enabled: !off.includes(category.key),
      tier: resolveTier(product, sites),
      cost: annualCost(category, product, sites),
    };
  });

  const included = lines.filter((l) => l.enabled);
  const total = included.reduce((sum, l) => sum + (l.cost ?? 0), 0);
  const unpriced = included.filter((l) => l.cost === null);

  const wpmgrTier = resolveWpmgrTier(sites);
  const wpmgrYear = wpmgrTier ? wpmgrTier.perMonth * 12 : null;
  const saving = wpmgrYear === null ? null : total - wpmgrYear;

  return (
    <div>
      {/* Fleet size. The one control that changes every number below it, so it
          sits above both bills rather than inside either. */}
      <fieldset className="rounded-xl border border-[var(--border)] bg-card p-5 shadow-sm sm:p-6">
        <legend className="px-2 text-sm font-semibold text-foreground">How many sites?</legend>

        <div className="mt-2 flex flex-wrap items-center gap-2">
          {SITE_PRESETS.map((n) => (
            <button
              key={n}
              type="button"
              onClick={() => dispatch({ type: "sites", value: n })}
              aria-pressed={sites === n}
              className={cn(
                "h-10 min-w-[3.25rem] rounded-[var(--radius)] border px-3 text-sm font-medium transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                sites === n
                  ? "border-[var(--primary)] bg-[var(--primary)] text-[var(--primary-foreground)]"
                  : "border-[var(--border)] bg-card text-foreground hover:bg-[var(--accent)]",
              )}
            >
              {n}
            </button>
          ))}
        </div>

        <label className="mt-5 flex items-center gap-4">
          <span className="sr-only">Number of sites</span>
          <input
            type="range"
            min={1}
            max={500}
            step={1}
            value={sites}
            onChange={(e) => dispatch({ type: "sites", value: Number(e.target.value) })}
            className="h-2 w-full cursor-pointer appearance-none rounded-full bg-[var(--muted)] accent-[var(--primary)]"
          />
          <output className="w-24 shrink-0 text-right font-mono text-sm tabular-nums text-foreground">
            {sites} {sites === 1 ? "site" : "sites"}
          </output>
        </label>
      </fieldset>

      {/* The two bills. Top-aligned on purpose: the length difference is the
          argument, and it only reads if both start on the same line. */}
      <div className="mt-8 grid gap-6 lg:grid-cols-[1.35fr_1fr] lg:items-start lg:gap-8">
        {/* Bill one: the plugins. */}
        <section
          aria-labelledby="stack-bill"
          className="rounded-xl border border-[var(--border)] bg-card shadow-sm"
        >
          <header className="flex items-baseline justify-between gap-3 border-b border-[var(--border)] px-5 py-4 sm:px-6">
            <h3 id="stack-bill" className="text-sm font-semibold text-foreground">
              A premium plugin stack
            </h3>
            <span className="text-xs text-[var(--muted-foreground)]">
              {included.length} of {lines.length} selected
            </span>
          </header>

          <ul className="flex flex-col">
            {lines.map((line) => (
              <li
                key={line.category.key}
                className="border-b border-[var(--border)]/60 px-5 py-4 transition-opacity duration-[var(--duration-fast)] last:border-0 sm:px-6"
              >
                <div className="flex items-start justify-between gap-4">
                  <label className="flex min-w-0 flex-1 cursor-pointer items-start gap-3">
                    <input
                      type="checkbox"
                      checked={line.enabled}
                      onChange={() => dispatch({ type: "toggle", key: line.category.key })}
                      className="mt-0.5 h-4 w-4 shrink-0 cursor-pointer accent-[var(--primary)]"
                    />
                    <span className="min-w-0">
                      <span className="flex items-center gap-2">
                        <Icon
                          name={line.category.icon}
                          size={15}
                          className="shrink-0 text-[var(--muted-foreground)]"
                          aria-hidden
                        />
                        <span
                          className={cn(
                            "text-sm font-medium",
                            line.enabled ? "text-foreground" : "text-[var(--muted-foreground)]",
                          )}
                        >
                          {line.category.label}
                        </span>
                      </span>
                      <span className="mt-1 block text-xs text-[var(--muted-foreground)]">
                        <a
                          href={line.product.url}
                          target="_blank"
                          rel="noreferrer noopener nofollow"
                          className="underline underline-offset-2 hover:text-foreground"
                        >
                          {line.product.name}
                        </a>
                        {line.tier ? <>, {line.tier.label}</> : null}
                        {PER_SITE_KEYS.includes(line.category.key) && line.tier ? (
                          <> at {money(line.tier.perYear)} per site</>
                        ) : null}
                      </span>
                    </span>
                  </label>

                  <span
                    className={cn(
                      "shrink-0 font-mono text-sm tabular-nums",
                      line.enabled
                        ? "text-foreground"
                        : "text-[var(--muted-foreground)] line-through",
                    )}
                  >
                    {line.cost === null ? "on request" : money(line.cost)}
                  </span>
                </div>

                {/* Only rendered where a category genuinely has a second
                    non-overlapping option, rather than a control on every row
                    for symmetry's sake. */}
                {line.category.products.length > 1 && (
                  <div className="mt-3 flex flex-wrap gap-1.5 pl-7">
                    {line.category.products.map((p, pi) => (
                      <button
                        key={p.name}
                        type="button"
                        onClick={() =>
                          dispatch({ type: "pick", key: line.category.key, index: pi })
                        }
                        aria-pressed={line.index === pi}
                        className={cn(
                          "rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors duration-[var(--duration-fast)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                          line.index === pi
                            ? "border-[var(--primary)]/40 bg-[var(--primary-subtle)] text-[var(--primary-pressed)]"
                            : "border-[var(--border)] text-[var(--muted-foreground)] hover:bg-[var(--accent)]",
                        )}
                      >
                        {p.name}
                      </button>
                    ))}
                  </div>
                )}
              </li>
            ))}
          </ul>

          <div className="border-t-2 border-foreground/80 px-5 py-5 sm:px-6">
            <div className="flex items-baseline justify-between gap-4">
              <span className="text-sm font-semibold text-foreground">Per year</span>
              <TweenedMoney
                value={total}
                className="font-mono text-3xl font-semibold tabular-nums text-foreground"
              />
            </div>
            {unpriced.length > 0 && (
              <p className="mt-2 text-xs text-[var(--muted-foreground)]">
                Plus {unpriced.map((l) => l.product.name).join(" and ")}, which stop publishing
                prices at this fleet size and quote privately.
              </p>
            )}
          </div>
        </section>

        {/* Bill two: one line. */}
        <section
          aria-labelledby="wpmgr-bill"
          className="rounded-xl border border-[var(--primary)]/35 bg-[var(--primary-subtle)]/40 shadow-sm"
        >
          <header className="flex items-baseline justify-between gap-3 border-b border-[var(--primary)]/25 px-5 py-4 sm:px-6">
            <h3 id="wpmgr-bill" className="text-sm font-semibold text-foreground">
              WPMgr
            </h3>
            <span className="text-xs text-[var(--muted-foreground)]">1 line</span>
          </header>

          <div className="px-5 py-4 sm:px-6">
            <div className="flex items-start justify-between gap-4">
              <span className="flex items-start gap-2">
                <Icon
                  name="Layers"
                  size={15}
                  className="mt-0.5 shrink-0 text-[var(--primary)]"
                  aria-hidden
                />
                <span>
                  <span className="block text-sm font-medium text-foreground">
                    Everything on the left
                  </span>
                  <span className="mt-1 block text-xs text-[var(--muted-foreground)]">
                    {wpmgrTier
                      ? `Hosted, ${wpmgrTier.name} plan, up to ${wpmgrTier.sites} sites`
                      : "Above the largest published plan, so this is quoted"}
                  </span>
                </span>
              </span>
              <span className="shrink-0 font-mono text-sm tabular-nums text-foreground">
                {wpmgrYear === null ? "on request" : money(wpmgrYear)}
              </span>
            </div>

            <ul className="mt-5 flex flex-col gap-2 border-t border-[var(--primary)]/20 pt-4">
              {PLUGIN_COST_CATEGORIES.map((c) => (
                <li key={c.key} className="flex items-start gap-2 text-xs leading-relaxed">
                  <Icon
                    name="Check"
                    size={13}
                    className="mt-0.5 shrink-0 text-[var(--primary)]"
                    aria-hidden
                  />
                  <span className="text-[var(--muted-foreground)]">{c.wpmgr}</span>
                </li>
              ))}
            </ul>
          </div>

          <div className="border-t-2 border-[var(--primary)]/40 px-5 py-5 sm:px-6">
            <div className="flex items-baseline justify-between gap-4">
              <span className="text-sm font-semibold text-foreground">Per year</span>
              {/* Above the largest published plan there is no price, and the
                  null MUST NOT fall through to a number. An earlier version
                  passed `wpmgrYear ?? 0` straight into the tween, so a
                  500-site fleet was told this costs $0 a year, which is both
                  false and the single most damaging thing a pricing page can
                  say. No published figure means no figure. */}
              {wpmgrYear === null ? (
                <span className="font-mono text-2xl font-semibold text-foreground">
                  On request
                </span>
              ) : (
                <TweenedMoney
                  value={wpmgrYear}
                  className="font-mono text-3xl font-semibold tabular-nums text-foreground"
                />
              )}
            </div>
            {saving !== null && saving > 0 && (
              <p className="mt-2 text-xs text-[var(--muted-foreground)]">
                {/* Rounded to match the two headline totals it is the
                    difference of. Printing exact cents here against rounded
                    totals above produced a visible ten-cent contradiction. */}
                {money(Math.round(saving))} less per year at {sites}{" "}
                {sites === 1 ? "site" : "sites"}.
              </p>
            )}
            <div className="mt-4 flex items-start gap-2 rounded-lg bg-card px-3 py-2.5">
              <Icon
                name="GitFork"
                size={14}
                className="mt-0.5 shrink-0 text-[var(--primary-pressed)]"
                aria-hidden
              />
              <span className="text-[11px] leading-snug text-foreground">
                Or self-host it for nothing, on any number of sites, forever. Same features, and
                you run the server.
              </span>
            </div>
            <a
              href={signupHref("plugin-stack")}
              target="_blank"
              rel="noreferrer noopener"
              className="mt-4 inline-flex h-11 w-full items-center justify-center gap-2 rounded-[var(--radius)] bg-[var(--primary)] px-5 text-sm font-medium text-[var(--primary-foreground)] shadow-sm transition-colors duration-[var(--duration-fast)] hover:bg-[var(--primary-hover)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              Start free on 3 sites
              <Icon name="ArrowRight" size={16} aria-hidden />
            </a>
          </div>
        </section>
      </div>
    </div>
  );
}
