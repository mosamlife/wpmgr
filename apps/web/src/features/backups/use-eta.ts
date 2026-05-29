/**
 * useEta — small hook + helpers for computing an ETA from a sliding window
 * of (timestamp, percent) samples.
 *
 * Per the implementation research dossier:
 *   - Keep a sliding 15 s window of recent samples (oldest dropped on every push).
 *   - EWMA throughput: weight = 0.3 per new sample, so old samples decay.
 *   - Require ≥3 samples and throughput > 0.05 %/sec before showing a number.
 *   - Otherwise show "Calculating…" or "Still working…" (caller's choice).
 *   - iOS-Files-style buckets for the final label:
 *       <60s  → "Less than a minute left"
 *       <120s → "About a minute left"
 *       <60m  → "About N minutes left"
 *       ≥60m  → "About N hours left"
 *
 * Stateful design: we use a ref-stored ring buffer in the parent to avoid
 * re-rendering at SSE cadence. The parent passes the current samples into
 * `useEta(samples)` which returns a memoized label.
 */
import { useMemo, useRef } from "react";

export interface EtaSample {
  /** Unix ms timestamp of the sample. */
  ts: number;
  /** Percent 0–100. */
  pct: number;
}

/** Mutable ring buffer kept across renders. */
export function useEtaSamples(): {
  push: (pct: number) => EtaSample[];
  reset: () => void;
} {
  const ref = useRef<EtaSample[]>([]);
  return {
    push: (pct: number) => {
      const now = Date.now();
      ref.current = [...ref.current, { ts: now, pct }].filter(
        (s) => now - s.ts < 15_000,
      );
      return ref.current;
    },
    reset: () => {
      ref.current = [];
    },
  };
}

/**
 * Derive an ETA label string from the current ring buffer state.
 * Pure — only re-computes when `samples` reference changes.
 */
export function useEta(samples: EtaSample[]): string {
  return useMemo(() => {
    if (samples.length < 3) return "Calculating…";

    // EWMA throughput in percent/second.
    const sorted = [...samples].sort((a, b) => a.ts - b.ts);
    let throughput = 0;
    let weightSum = 0;
    for (let i = 1; i < sorted.length; i++) {
      const prev = sorted[i - 1];
      const curr = sorted[i];
      if (!prev || !curr) continue;
      const dt = (curr.ts - prev.ts) / 1000;
      if (dt <= 0) continue;
      const dp = curr.pct - prev.pct;
      if (dp < 0) continue; // resume / cursor reverse — skip
      const rate = dp / dt;
      const weight = 0.3 * Math.pow(0.7, sorted.length - 1 - i);
      throughput += rate * weight;
      weightSum += weight;
    }
    if (weightSum === 0) return "Still working…";
    throughput /= weightSum;

    if (throughput < 0.05) return "Still working…";

    const latest = sorted[sorted.length - 1];
    if (!latest) return "Calculating…";
    const remainingPct = 100 - latest.pct;
    const secondsLeft = remainingPct / throughput;

    if (secondsLeft < 60) return "Less than a minute left";
    if (secondsLeft < 120) return "About a minute left";
    if (secondsLeft < 3600) {
      const minutes = Math.round(secondsLeft / 60);
      return `About ${minutes} minutes left`;
    }
    const hours = Math.round(secondsLeft / 3600);
    return `About ${hours} hour${hours === 1 ? "" : "s"} left`;
  }, [samples]);
}

/**
 * Format elapsed seconds as a short readable string.
 *   < 60s   → "12s"
 *   < 60m   → "12m 34s"
 *   ≥ 60m   → "1h 02m"
 */
export function formatElapsed(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${String(m % 60).padStart(2, "0")}m`;
}
