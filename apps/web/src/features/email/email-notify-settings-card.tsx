import { useState, useId } from "react";
import { AlertTriangle, Loader2, X } from "lucide-react";
import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  CardDescription,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { relativeTime } from "@/lib/utils";
import type { PutEmailNotifySettingsRequest } from "@wpmgr/api";
import { useEmailNotifySettings, usePutEmailNotifySettings } from "./use-email";

// ---------------------------------------------------------------------------
// Email notifications card (m62; digest cadence fixed 2026-07 — issue #123)
//
// Mounted on the /email fleet page between Deliverability and Fleet log.
//
// The backend keeps a single shared `recipients` list for both alerts and
// the digest, plus a master `enabled` switch that gates both sections
// (derived here as alertOnFailure || digestEnabled — there is no separate
// UI control for it). Two sub-sections:
//   1. Per-failure alerts — enabled toggle ("alert_on_failure"), throttle
//      select. There is no configurable failure-count threshold; the
//      backend alerts on the first failure once the throttle window has
//      elapsed.
//   2. Daily digest — enabled toggle ("digest_enabled"), UTC hour. The UI
//      only ever offers a daily cadence, so digest_cadence is always sent
//      as "daily" and digest_day (only meaningful for weekly/monthly) is
//      not collected.
//
// The instance_mailer_configured flag controls a warning banner: when false,
// neither alerts nor digests can deliver. The banner links to /settings/smtp.
//
// failure_detection (GH #381) is the OTHER half of the alerting path: even
// with a working instance mailer, WPMgr can only detect a delivery failure
// on a site that EITHER routes its mail through WPMgr (covered regardless of
// agent version — that capture path predates this feature and consults no
// version gate) OR whose agent is new enough to report a failure on mail it
// does not route (>= min_agent_version_unrouted). sites_routed says how many
// of the tenant's sites fall into the first bucket, so a high sites_covered
// count on a fleet of old agents is explained rather than looking wrong.
// A tenant whose sites are neither routed nor new enough sees "Per-failure
// alerts" as an armed toggle that will never fire — the two banners are
// independent (nothing can SEND vs. nothing can DETECT) and both can be
// showing at once. sites_total === 0 is an empty fleet, not a warning; the
// field is absent entirely on an older API (self-hosted instance not yet
// upgraded), in which case no coverage message is shown at all rather than
// guessed at.
//
// This endpoint never 404s (returns defaults). If the API pre-dates 0.36 and
// returns 404, useEmailNotifySettings maps it to null and the card is hidden.
// ---------------------------------------------------------------------------

const THROTTLE_OPTIONS = [
  { value: 15, label: "15 minutes" },
  { value: 30, label: "30 minutes" },
  { value: 60, label: "1 hour" },
  { value: 180, label: "3 hours" },
  { value: 360, label: "6 hours" },
  { value: 720, label: "12 hours" },
  { value: 1440, label: "24 hours" },
];

const HOUR_OPTIONS = Array.from({ length: 24 }, (_, i) => ({
  value: i,
  label: `${String(i).padStart(2, "0")}:00 UTC`,
}));

// ---------------------------------------------------------------------------
// Recipient chip input
// ---------------------------------------------------------------------------

interface RecipientChipInputProps {
  value: string[];
  onChange: (v: string[]) => void;
  max?: number;
  label: string;
  id: string;
  disabled?: boolean;
}

function RecipientChipInput({
  value,
  onChange,
  max = 20,
  label,
  id,
  disabled,
}: RecipientChipInputProps) {
  const [draft, setDraft] = useState("");

  function tryAdd() {
    const addr = draft.trim();
    if (!addr) return;
    if (value.length >= max) return;
    if (!addr.includes("@")) return; // basic gate; server validates properly
    if (value.includes(addr)) {
      setDraft("");
      return;
    }
    onChange([...value, addr]);
    setDraft("");
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      tryAdd();
    }
    if (e.key === "Backspace" && draft === "" && value.length > 0) {
      onChange(value.slice(0, -1));
    }
  }

  function remove(addr: string) {
    onChange(value.filter((a) => a !== addr));
  }

  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <div className="flex min-h-9 flex-wrap items-center gap-1.5 rounded-md border border-[var(--color-input)] bg-transparent px-2.5 py-1.5 focus-within:ring-2 focus-within:ring-[var(--color-ring)]">
        {value.map((addr) => (
          <span
            key={addr}
            className="inline-flex items-center gap-1 rounded-full bg-[var(--color-primary)]/15 px-2 py-0.5 text-xs text-[var(--color-primary)]"
          >
            {addr}
            {!disabled ? (
              <button
                type="button"
                aria-label={`Remove ${addr}`}
                onClick={() => remove(addr)}
                className="rounded-full hover:text-[var(--color-destructive)] focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-[var(--color-ring)]"
              >
                <X aria-hidden="true" className="size-3" />
              </button>
            ) : null}
          </span>
        ))}
        {value.length < max && !disabled ? (
          <input
            id={id}
            type="email"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={handleKeyDown}
            onBlur={tryAdd}
            placeholder={value.length === 0 ? "email@example.com" : "+add"}
            className="min-w-[140px] flex-1 bg-transparent text-sm text-[var(--color-foreground)] placeholder-[var(--color-muted-foreground)] focus-visible:outline-none"
          />
        ) : null}
      </div>
      <p className="text-xs text-[var(--color-muted-foreground)]">
        Press Enter or comma to add. {max - value.length} slot
        {max - value.length !== 1 ? "s" : ""} remaining.
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main card
// ---------------------------------------------------------------------------

export function EmailNotifySettingsCard() {
  const settingsQuery = useEmailNotifySettings();
  const save = usePutEmailNotifySettings();

  const s = settingsQuery.data;

  // Local state mirrors server values; populated once on first load
  const [alertsEnabled, setAlertsEnabled] = useState(false);
  const [alertThrottle, setAlertThrottle] = useState(60);
  const [digestEnabled, setDigestEnabled] = useState(false);
  const [digestHour, setDigestHour] = useState(8);
  // Recipients are a single list shared by both alerts and the digest.
  const [recipients, setRecipients] = useState<string[]>([]);
  const [initialized, setInitialized] = useState(false);

  if (s && !initialized) {
    setAlertsEnabled(s.alert_on_failure);
    setAlertThrottle(s.alert_throttle_minutes);
    setDigestEnabled(s.digest_enabled);
    setDigestHour(s.digest_hour);
    setRecipients(s.recipients ?? []);
    setInitialized(true);
  }

  const recipientsId = useId();
  const throttleId = useId();
  const digestHourId = useId();

  function buildPayload(): PutEmailNotifySettingsRequest {
    return {
      // Master switch: on whenever either section is enabled. There is no
      // dedicated UI control for it — each section owns its own toggle.
      enabled: alertsEnabled || digestEnabled,
      recipients,
      alert_on_failure: alertsEnabled,
      alert_throttle_minutes: alertThrottle,
      digest_enabled: digestEnabled,
      // The UI only ever offers a daily digest; digest_day is meaningless
      // for that cadence and is only validated when weekly/monthly is used.
      digest_cadence: "daily",
      digest_day: 0,
      digest_hour: digestHour,
      timezone: "UTC",
    };
  }

  if (settingsQuery.isPending) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    );
  }

  if (settingsQuery.isError) {
    return (
      <PageError
        what="Could not load notification settings."
        why={settingsQuery.error?.message}
        onRetry={() => void settingsQuery.refetch()}
      />
    );
  }

  // Pre-0.36 API returned 404 — feature not available; hide the card
  if (settingsQuery.data === null) {
    return null;
  }

  const instanceMailerConfigured = s?.instance_mailer_configured ?? false;

  // GH #381: WPMgr can only detect a delivery failure on a site that is
  // either routed through WPMgr (any agent version) or new enough to report
  // a failure on mail it does not route. `failure_detection` is absent on an
  // older API (self-hosted instance not yet upgraded) — in that case we have
  // no coverage numbers to show and say nothing rather than guess. A tenant
  // with zero sites also gets no coverage message; that is not a warning
  // state, just an empty fleet.
  const failureDetection = s?.failure_detection;
  const sitesTotal = failureDetection?.sites_total ?? 0;
  const sitesCovered = failureDetection?.sites_covered ?? 0;
  const sitesRouted = failureDetection?.sites_routed ?? 0;
  const minAgentVersionUnrouted = failureDetection?.min_agent_version_unrouted;
  const hasCoverageData = failureDetection != null && sitesTotal > 0;
  const fullCoverage = hasCoverageData && sitesCovered >= sitesTotal;
  const zeroCoverage = hasCoverageData && sitesCovered === 0;
  const partialCoverage = hasCoverageData && sitesCovered > 0 && sitesCovered < sitesTotal;
  // Of the sitesTotal - sitesRouted sites that are NOT routed, how many are
  // still uncovered (i.e. also below min_agent_version_unrouted). Because
  // coverage is routed OR new-enough, every uncovered site is necessarily
  // unrouted — sitesTotal - sitesCovered is exactly that unrouted-and-old
  // count, never larger, so this is safe to state without overclaiming.
  const uncoveredCount = sitesTotal - sitesCovered;

  return (
    <Card>
      <CardHeader>
        <CardTitle>Email notifications</CardTitle>
        <CardDescription>
          Get alerted when deliveries fail and receive a scheduled digest of
          email activity across your fleet.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        {/* Instance mailer warning */}
        {!instanceMailerConfigured ? (
          <div className="flex gap-2 rounded-md border border-[var(--color-warning)]/50 bg-[var(--color-warning)]/10 px-3 py-2.5">
            <AlertTriangle
              aria-hidden="true"
              className="mt-0.5 size-4 shrink-0 text-[var(--color-warning)]"
            />
            <div className="text-sm">
              <span className="font-medium">Instance mailer not configured.</span>{" "}
              Alerts and digests cannot be delivered until instance-level SMTP is set
              up.{" "}
              <Link
                to="/settings/smtp"
                className="font-medium text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
              >
                Configure SMTP
              </Link>
            </div>
          </div>
        ) : null}

        {/* Recipients — a single list shared by alerts and the digest */}
        {alertsEnabled || digestEnabled ? (
          <RecipientChipInput
            id={recipientsId}
            label="Recipients"
            value={recipients}
            onChange={setRecipients}
            max={20}
            disabled={save.isPending}
          />
        ) : null}

        {/* Per-failure alerts */}
        <section aria-labelledby="alerts-section-heading">
          <div className="mb-3 flex items-center justify-between gap-3">
            <div>
              <h3
                id="alerts-section-heading"
                className="text-sm font-semibold text-[var(--color-foreground)]"
              >
                Per-failure alerts
              </h3>
              <p className="text-xs text-[var(--color-muted-foreground)]">
                Send an alert email when a delivery failure is detected on a site.
              </p>
              {fullCoverage ? (
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  {sitesRouted >= sitesTotal ? (
                    <>
                      Covering all {sitesTotal} connected site{sitesTotal !== 1 ? "s" : ""}
                      {" — all of them route mail through WPMgr, so this holds regardless "}
                      of agent version.
                    </>
                  ) : sitesRouted > 0 ? (
                    <>
                      Covering all {sitesTotal} connected site{sitesTotal !== 1 ? "s" : ""}:{" "}
                      {sitesRouted} via mail routing, the rest by agent version.
                    </>
                  ) : (
                    <>
                      Covering all {sitesTotal} connected site{sitesTotal !== 1 ? "s" : ""}.
                    </>
                  )}
                </p>
              ) : null}
              {partialCoverage ? (
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  {sitesCovered} of {sitesTotal} connected sites can report a delivery
                  failure{sitesRouted > 0 ? ` (${sitesRouted} via mail routing)` : ""}. The
                  other {uncoveredCount}{" "}
                  {uncoveredCount !== 1 ? "aren't routed through WPMgr and are" : "isn't routed through WPMgr and is"}{" "}
                  on an agent older than {minAgentVersionUnrouted}; updating the agent
                  restores alerting.{" "}
                  <Link
                    to="/sites"
                    className="font-medium text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                  >
                    View sites
                  </Link>
                </p>
              ) : null}
            </div>
            <Switch
              checked={alertsEnabled}
              onCheckedChange={setAlertsEnabled}
              disabled={save.isPending}
              aria-label="Enable per-failure alerts"
            />
          </div>

          {zeroCoverage ? (
            <div className="mb-4 flex gap-2 rounded-md border border-[var(--color-warning)]/50 bg-[var(--color-warning)]/10 px-3 py-2.5">
              <AlertTriangle
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-[var(--color-warning)]"
              />
              <div className="text-sm">
                <span className="font-medium">
                  No connected site can report a delivery failure.
                </span>{" "}
                None of your {sitesTotal} connected site{sitesTotal !== 1 ? "s" : ""}{" "}
                route{sitesTotal !== 1 ? "" : "s"} mail through WPMgr, and{" "}
                {sitesTotal !== 1 ? "all are" : "it is"} on an agent older than{" "}
                {minAgentVersionUnrouted}, so this toggle will never fire. Update the
                agent on at least one site to restore alerting.{" "}
                <Link
                  to="/sites"
                  className="font-medium text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                >
                  View sites
                </Link>
              </div>
            </div>
          ) : null}

          {alertsEnabled ? (
            <div className="space-y-4 rounded-md border border-[var(--color-border)] bg-[var(--color-card)] p-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={throttleId}>Minimum time between alerts</Label>
                <Select
                  id={throttleId}
                  value={String(alertThrottle)}
                  onChange={(e) => setAlertThrottle(Number(e.target.value))}
                  disabled={save.isPending}
                >
                  {THROTTLE_OPTIONS.map((o) => (
                    <option key={o.value} value={String(o.value)}>
                      {o.label}
                    </option>
                  ))}
                </Select>
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  Prevents alert storms. Bounce events from webhooks are counted in the
                  digest only and do not trigger per-failure alerts.
                </p>
              </div>
            </div>
          ) : null}
        </section>

        {/* Daily digest */}
        <section aria-labelledby="digest-section-heading">
          <div className="mb-3 flex items-center justify-between gap-3">
            <div>
              <h3
                id="digest-section-heading"
                className="text-sm font-semibold text-[var(--color-foreground)]"
              >
                Daily digest
              </h3>
              <p className="text-xs text-[var(--color-muted-foreground)]">
                Receive a scheduled summary of email activity (sent, failed, bounced)
                across all sites.
              </p>
            </div>
            <Switch
              checked={digestEnabled}
              onCheckedChange={setDigestEnabled}
              disabled={save.isPending}
              aria-label="Enable daily digest"
            />
          </div>

          {digestEnabled ? (
            <div className="space-y-4 rounded-md border border-[var(--color-border)] bg-[var(--color-card)] p-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={digestHourId}>Send at (UTC)</Label>
                <Select
                  id={digestHourId}
                  value={String(digestHour)}
                  onChange={(e) => setDigestHour(Number(e.target.value))}
                  disabled={save.isPending}
                >
                  {HOUR_OPTIONS.map((o) => (
                    <option key={o.value} value={String(o.value)}>
                      {o.label}
                    </option>
                  ))}
                </Select>
              </div>
              {s?.next_digest_at ? (
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  Next digest:{" "}
                  <time dateTime={s.next_digest_at}>{relativeTime(s.next_digest_at)}</time>
                </p>
              ) : null}
            </div>
          ) : null}
        </section>

        {/* Meta badges */}
        {s ? (
          <div className="flex flex-wrap gap-2">
            {s.tenant_id ? (
              <Badge variant="muted" className="text-xs">
                Settings row exists
              </Badge>
            ) : (
              <Badge variant="muted" className="text-xs">
                Using defaults (not yet saved)
              </Badge>
            )}
            {s.updated_at ? (
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Last updated {relativeTime(s.updated_at)}
              </span>
            ) : null}
          </div>
        ) : null}

        {/* Save */}
        <div className="flex items-center justify-end gap-3">
          {save.isError ? (
            <p
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {save.error.message}
            </p>
          ) : null}
          <Button
            type="button"
            disabled={save.isPending}
            onClick={() => save.mutate(buildPayload())}
          >
            {save.isPending ? (
              <>
                <Loader2 aria-hidden="true" className="mr-1.5 size-4 animate-spin" />
                Saving…
              </>
            ) : (
              "Save notification settings"
            )}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

