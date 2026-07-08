import { useId, useState } from "react";
import { AlertTriangle, Loader2, RotateCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toast } from "@/components/toast";

import { SettingRow } from "../components/SettingRow";
import { SettingsCard } from "../components/SettingsCard";
import { useRotateBeaconKey } from "../hooks/useRotateBeaconKey";
import type { PerfConfig } from "../types";

// Real User Monitoring (RUM) settings section.
//
// Mirrors FontsSection: a SettingsCard with one or more SettingRows, each
// saving through the perf-config PUT (the parent OptimizeTab calls `save`).
//
// Why off by default: enabling RUM turns on a visitor data flow -- the
// collector injects a small script into every cached page that sends Core Web
// Vitals from the site visitor's browser to this control plane. Operators must
// make an active choice to enable it and disclose the data flow in their own
// site privacy policy.
//
// The sample-rate control appears only when rum_enabled is on, matching the
// FontsSection pattern of revealing dependent settings under the parent toggle.
//
// Beacon-key recovery (GH #174): the one best-effort mint+push that happens
// on first RUM-enable can be lost (agent down/unreachable), permanently
// stranding the beacon key empty on the agent with zero RUM samples ever
// collected and no visible error. `beacon_key_set` (a key is stored CP-side)
// and `beacon_key_acked_present` (the agent has confirmed holding it) let
// this section show the stuck state and offer the same recovery action
// (rotate) the control plane's own reconcile job uses.

export interface RumSectionProps {
  siteId: string;
  config: PerfConfig;
  save: (patch: Partial<PerfConfig>) => void;
  disabled: boolean;
  isSaving: (key: string) => boolean;
  /** operator+ (site.perf.config) -- can rotate the beacon key. Viewers never see the action. */
  canOperate: boolean;
}

export function RumSection({
  siteId,
  config,
  save,
  disabled,
  isSaving,
  canOperate,
}: RumSectionProps) {
  return (
    <SettingsCard
      title="Real User Monitoring"
      description="Measure actual visitor experience using Core Web Vitals collected from real browsers."
    >
      <SettingRow
        label="Enable Real User Monitoring"
        description={
          "Measure real visitors' Core Web Vitals (LCP, INP, CLS) from their browsers. " +
          "A small first-party script is injected into cached pages; results appear in the " +
          "Performance dashboard after enough pageviews accumulate. Off by default because " +
          "this turns on a visitor data flow -- enable only if your site's privacy policy " +
          "covers the collection of page-timing data from visitors."
        }
        checked={config.rum_enabled ?? false}
        onChange={(v) => save({ rum_enabled: v })}
        disabled={disabled || isSaving("rum_enabled")}
        saving={isSaving("rum_enabled")}
      >
        <SampleRateRow
          value={config.rum_sample_rate ?? 1.0}
          onChange={(v) => save({ rum_sample_rate: v })}
          disabled={disabled}
          saving={isSaving("rum_sample_rate")}
        />
        <MinSampleCountRow
          value={config.min_sample_count ?? 30}
          onCommit={(v) => save({ min_sample_count: v })}
          disabled={disabled}
          saving={isSaving("min_sample_count")}
        />
      </SettingRow>
      {config.beacon_key_set ? (
        <BeaconKeyRow
          siteId={siteId}
          stuck={Boolean(config.rum_enabled) && !config.beacon_key_acked_present}
          canOperate={canOperate}
        />
      ) : null}
    </SettingsCard>
  );
}

// ---------------------------------------------------------------------------
// Beacon-key row: the recovery action + stuck-state warning (GH #174)
// ---------------------------------------------------------------------------

interface BeaconKeyRowProps {
  siteId: string;
  /** rum_enabled && beacon_key_set && !beacon_key_acked_present. */
  stuck: boolean;
  canOperate: boolean;
}

function BeaconKeyRow({ siteId, stuck, canOperate }: BeaconKeyRowProps) {
  if (stuck) {
    return (
      <div className="px-5 py-4">
        <div
          role="status"
          className="flex items-start gap-3 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 dark:border-amber-800 dark:bg-amber-950/30"
        >
          <AlertTriangle
            aria-hidden="true"
            className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400"
          />
          <div className="min-w-0 flex-1">
            <p className="text-xs font-medium text-amber-800 dark:text-amber-300">
              Beacon key not yet confirmed
            </p>
            <p className="mt-0.5 text-xs text-amber-700 dark:text-amber-400">
              RUM is on, but the site hasn't confirmed it received the beacon
              key yet, so no data will arrive until it does. This usually
              resolves on its own the next time the site checks in.
            </p>
            {canOperate ? (
              <div className="mt-2">
                <RotateBeaconKeyButton siteId={siteId} />
              </div>
            ) : null}
          </div>
        </div>
      </div>
    );
  }

  if (!canOperate) return null;

  return (
    <div className="flex items-center justify-between gap-4 px-5 py-4">
      <div className="min-w-0">
        <p className="text-sm font-medium text-foreground">Beacon key</p>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Mint a fresh beacon key and push it to the site now. Beacons already
          in flight signed with the previous key still resolve during a short
          grace window.
        </p>
      </div>
      <RotateBeaconKeyButton siteId={siteId} />
    </div>
  );
}

interface RotateBeaconKeyButtonProps {
  siteId: string;
}

function RotateBeaconKeyButton({ siteId }: RotateBeaconKeyButtonProps) {
  const rotate = useRotateBeaconKey(siteId);

  function handleClick() {
    rotate.mutate(undefined, {
      onSuccess: () => {
        toast.success("Beacon key rotated.", {
          description: "The site will receive a fresh key on its next sync.",
        });
      },
    });
  }

  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      onClick={handleClick}
      disabled={rotate.isPending}
    >
      {rotate.isPending ? (
        <Loader2 aria-hidden="true" className="size-4 animate-spin" />
      ) : (
        <RotateCw aria-hidden="true" className="size-4" />
      )}
      Rotate beacon key
    </Button>
  );
}

// ---------------------------------------------------------------------------
// Sample-rate sub-control (revealed when rum_enabled is on)
// ---------------------------------------------------------------------------

interface SampleRateRowProps {
  value: number;
  onChange: (v: number) => void;
  disabled: boolean;
  saving: boolean;
}

const SAMPLE_RATE_OPTIONS = [
  { value: 1.0, label: "100% (all pageviews)" },
  { value: 0.5, label: "50%" },
  { value: 0.25, label: "25%" },
  { value: 0.1, label: "10%" },
] as const;

function SampleRateRow({ value, onChange, disabled, saving }: SampleRateRowProps) {
  const id = useId();
  // Nearest discrete option to the stored value (gracefully handles values
  // written by the API or a future finer-grained control).
  const selectedValue = SAMPLE_RATE_OPTIONS.reduce((best, opt) =>
    Math.abs(opt.value - value) < Math.abs(best.value - value) ? opt : best,
  ).value;

  return (
    <div className="flex items-center justify-between gap-4">
      <div className="min-w-0">
        <Label
          htmlFor={id}
          className="cursor-pointer text-sm font-medium text-foreground"
        >
          Sample rate
        </Label>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Fraction of pageviews to measure. A lower rate reduces data volume on
          high-traffic sites while keeping the sample statistically
          representative. 100% is recommended for low-to-medium traffic sites.
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {saving ? (
          <Loader2
            aria-hidden="true"
            className="size-4 animate-spin text-muted-foreground"
          />
        ) : null}
        <select
          id={id}
          value={selectedValue}
          onChange={(e) => onChange(Number(e.target.value))}
          disabled={disabled}
          aria-label="Sample rate"
          className="h-8 rounded-md border border-input bg-background px-2 py-1 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {SAMPLE_RATE_OPTIONS.map((opt) => (
            <option key={opt.value} value={opt.value}>
              {opt.label}
            </option>
          ))}
        </select>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Minimum-sample-count sub-control (revealed when rum_enabled is on)
// ---------------------------------------------------------------------------

interface MinSampleCountRowProps {
  value: number;
  onCommit: (v: number) => void;
  disabled: boolean;
  saving: boolean;
}

const MIN_SAMPLE_MIN = 1;
const MIN_SAMPLE_MAX = 1000;

/**
 * Numeric input for the minimum-samples-to-display floor.
 * Commits on blur / Enter (same pattern as NumberField in Field.tsx) so the
 * perf-config PUT fires once per edit, not once per keystroke. Local draft
 * state lets the operator clear and retype freely.
 */
function MinSampleCountRow({ value, onCommit, disabled, saving }: MinSampleCountRowProps) {
  const id = useId();
  const [draft, setDraft] = useState<string>(String(value));
  const [lastValue, setLastValue] = useState<number>(value);
  if (value !== lastValue) {
    setLastValue(value);
    setDraft(String(value));
  }

  function commit() {
    const parsed = parseInt(draft, 10);
    if (draft.trim() === "" || Number.isNaN(parsed)) {
      setDraft(String(value));
      return;
    }
    const clamped = Math.min(MIN_SAMPLE_MAX, Math.max(MIN_SAMPLE_MIN, parsed));
    setDraft(String(clamped));
    if (clamped !== value) onCommit(clamped);
  }

  return (
    <div className="mt-3 flex items-center justify-between gap-4 border-t border-border pt-3">
      <div className="min-w-0">
        <Label
          htmlFor={id}
          className="cursor-pointer text-sm font-medium text-foreground"
        >
          Minimum samples to display
        </Label>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Hide a metric's score until at least this many real-visitor samples are
          collected, so a noisy average over a handful of visits is never shown.
          Lower it to see scores sooner on low-traffic sites; raise it for
          stricter accuracy.
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {saving ? (
          <Loader2
            aria-hidden="true"
            className="size-4 animate-spin text-muted-foreground"
          />
        ) : null}
        <Input
          id={id}
          type="number"
          inputMode="numeric"
          min={MIN_SAMPLE_MIN}
          max={MIN_SAMPLE_MAX}
          step={1}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={commit}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              e.currentTarget.blur();
            }
          }}
          disabled={disabled}
          aria-label="Minimum samples to display"
          className="max-w-24"
        />
      </div>
    </div>
  );
}
