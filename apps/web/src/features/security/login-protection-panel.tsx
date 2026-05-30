import { useState } from "react";
import { Info, Plus, ShieldAlert, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { toast } from "@/components/toast/use-toast-helpers";
import type { SiteLoginProtectionConfig, SiteLoginProtectionConfigUpdate } from "@wpmgr/api";

import { useSecurityConfig, useUpdateSecurityConfig } from "./use-security";

// S2 — Login Protection config panel.
//
// Rendered inline on the security tab (NOT in a dialog). Follows the
// "key on loaded config" pattern from error-config-panel so form state is
// always initialised from server data without setState-in-render or
// setState-in-effect.
//
// Two layers:
//   LoginProtectionShell  — fetches; renders loading / error / loaded
//   LoginProtectionLoaded — keyed on config hash; owns all local state
//
// Design rules:
//   - Protect-mode banner is shown when mode === "protect".
//   - allow_cidrs editor is prominent with inline CIDR validation.
//   - Thresholds use tabular-nums + numeric inputs.
//   - ip_header uses a native <select> (no extra dep needed; token-styled).
//   - Verb-first Save button; pending state; PageError on failure.
//   - After save the query cache is updated from the PUT response so any
//     auto-added allow_cidr is immediately reflected.

export function LoginProtectionPanel({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } =
    useSecurityConfig(siteId);

  if (isPending) {
    return <LoginProtectionSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load login-protection config."
        why={error instanceof Error ? error.message : "Unknown error"}
        onRetry={() => void refetch()}
        retryLabel="Reload config"
      />
    );
  }

  if (!data) return null;

  // Key includes mode + CIDR lists + thresholds so the form resets if a
  // background refetch returns a different server value (e.g. another operator
  // changed the config).
  const configKey = [
    data.mode,
    data.ip_header,
    data.allow_cidrs.join(","),
    data.deny_cidrs.join(","),
    data.thresholds.captcha_limit,
    data.thresholds.temp_block_limit,
    data.thresholds.block_all_limit,
    data.thresholds.failed_login_gap,
    data.thresholds.success_login_gap,
    data.thresholds.all_blocked_gap,
  ].join("|");

  return (
    <LoginProtectionLoaded
      key={configKey}
      siteId={siteId}
      initialConfig={data}
    />
  );
}

// ---------------------------------------------------------------------------
// Loaded form
// ---------------------------------------------------------------------------

const MODES = [
  {
    value: "disabled" as const,
    label: "Disabled",
    description: "No login protection active. Events are not recorded.",
  },
  {
    value: "audit" as const,
    label: "Audit",
    description:
      "Record login events and thresholds are evaluated, but no IPs are blocked.",
  },
  {
    value: "protect" as const,
    label: "Protect",
    description:
      "Record events AND block IPs based on thresholds. This is the default for new installs.",
  },
];

const IP_HEADERS = [
  { value: "REMOTE_ADDR", label: "REMOTE_ADDR (default)" },
  { value: "HTTP_X_FORWARDED_FOR", label: "HTTP_X_FORWARDED_FOR" },
  { value: "HTTP_X_REAL_IP", label: "HTTP_X_REAL_IP" },
  { value: "HTTP_CF_CONNECTING_IP", label: "HTTP_CF_CONNECTING_IP (Cloudflare)" },
  { value: "HTTP_TRUE_CLIENT_IP", label: "HTTP_TRUE_CLIENT_IP" },
];

// Loose CIDR validation: IPv4/v6 with prefix. Allows bare IPs too for UX.
function isValidCidr(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return false;
  // IPv4 CIDR: x.x.x.x/n or x.x.x.x
  const ipv4Cidr =
    /^(\d{1,3}\.){3}\d{1,3}(\/([0-9]|[1-2][0-9]|3[0-2]))?$/;
  // IPv6 CIDR: simplified pattern
  const ipv6Cidr = /^[0-9a-fA-F:]+(::[0-9a-fA-F]*)?(\/([0-9]|[1-9][0-9]|1[01][0-9]|12[0-8]))?$/;
  return ipv4Cidr.test(trimmed) || ipv6Cidr.test(trimmed);
}

interface LoadedProps {
  siteId: string;
  initialConfig: SiteLoginProtectionConfig;
}

function LoginProtectionLoaded({ siteId, initialConfig }: LoadedProps) {
  const update = useUpdateSecurityConfig(siteId);

  // --- form state ---
  const [mode, setMode] = useState<SiteLoginProtectionConfig["mode"]>(
    initialConfig.mode,
  );
  const [ipHeader, setIpHeader] = useState(initialConfig.ip_header);
  const [allowCidrs, setAllowCidrs] = useState<string[]>(
    initialConfig.allow_cidrs,
  );
  const [denyCidrs, setDenyCidrs] = useState<string[]>(
    initialConfig.deny_cidrs,
  );
  const [thresholds, setThresholds] = useState({
    captcha_limit: initialConfig.thresholds.captcha_limit,
    temp_block_limit: initialConfig.thresholds.temp_block_limit,
    block_all_limit: initialConfig.thresholds.block_all_limit,
    failed_login_gap: initialConfig.thresholds.failed_login_gap,
    success_login_gap: initialConfig.thresholds.success_login_gap,
    all_blocked_gap: initialConfig.thresholds.all_blocked_gap,
  });

  // --- CIDR input state ---
  const [allowInput, setAllowInput] = useState("");
  const [allowInputError, setAllowInputError] = useState<string | null>(null);
  const [denyInput, setDenyInput] = useState("");
  const [denyInputError, setDenyInputError] = useState<string | null>(null);

  const [saveError, setSaveError] = useState<string | null>(null);

  function handleAddAllowCidr() {
    const val = allowInput.trim();
    if (!isValidCidr(val)) {
      setAllowInputError("Enter a valid CIDR (e.g. 203.0.113.0/24).");
      return;
    }
    if (allowCidrs.includes(val)) {
      setAllowInputError("This CIDR is already in the list.");
      return;
    }
    setAllowCidrs((prev) => [...prev, val]);
    setAllowInput("");
    setAllowInputError(null);
  }

  function handleAddDenyCidr() {
    const val = denyInput.trim();
    if (!isValidCidr(val)) {
      setDenyInputError("Enter a valid CIDR (e.g. 198.51.100.0/24).");
      return;
    }
    if (denyCidrs.includes(val)) {
      setDenyInputError("This CIDR is already in the list.");
      return;
    }
    setDenyCidrs((prev) => [...prev, val]);
    setDenyInput("");
    setDenyInputError(null);
  }

  function handleThreshold(
    key: keyof typeof thresholds,
    raw: string,
  ) {
    const n = parseInt(raw, 10);
    if (!isNaN(n) && n >= 0) {
      setThresholds((prev) => ({ ...prev, [key]: n }));
    }
  }

  function handleSave() {
    setSaveError(null);
    const body: SiteLoginProtectionConfigUpdate = {
      mode,
      ip_header: ipHeader,
      allow_cidrs: allowCidrs,
      deny_cidrs: denyCidrs,
      thresholds,
    };

    update.mutate(body, {
      onSuccess: () => {
        toast.success("Login protection saved.", {
          description: "Config pushed to the agent.",
        });
      },
      onError: (err: Error) => {
        setSaveError(err.message);
      },
    });
  }

  const isProtecting = mode === "protect";

  return (
    <div className="space-y-8">
      {/* ── Protect-mode enforcing banner ── */}
      {isProtecting && (
        <div
          role="status"
          className="flex items-start gap-3 rounded-lg border border-[var(--color-warning)]/40 bg-[var(--color-warning)]/8 p-4"
        >
          <ShieldAlert
            aria-hidden="true"
            className="mt-0.5 size-4 shrink-0 text-[var(--color-warning)]"
          />
          <div className="space-y-1 text-sm">
            <p className="font-semibold text-[var(--color-foreground)]">
              Protect mode is active and enforcing.
            </p>
            <p className="text-[var(--color-muted-foreground)]">
              IPs that exceed the thresholds below will be blocked from the
              WordPress login page. Ensure your IP is in the allow list so you
              cannot lock yourself out.
            </p>
          </div>
        </div>
      )}

      {/* ── Mode control ── */}
      <section aria-labelledby="lp-mode-heading">
        <h3
          id="lp-mode-heading"
          className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Mode
        </h3>
        <div className="space-y-2">
          {MODES.map((m) => (
            <label
              key={m.value}
              className="flex cursor-pointer items-start gap-3 rounded-lg border border-[var(--color-border)] p-3 transition-colors hover:bg-[var(--color-muted)]/40 has-[:checked]:border-[var(--color-ring)] has-[:checked]:bg-[var(--color-muted)]/20"
            >
              <input
                type="radio"
                name="lp-mode"
                value={m.value}
                checked={mode === m.value}
                onChange={() => setMode(m.value)}
                className="mt-0.5 accent-[var(--color-primary)]"
              />
              <div className="min-w-0">
                <span className="block text-sm font-medium text-[var(--color-foreground)]">
                  {m.label}
                </span>
                <span className="block text-xs text-[var(--color-muted-foreground)]">
                  {m.description}
                </span>
              </div>
            </label>
          ))}
        </div>
      </section>

      {/* ── Allow CIDRs ── */}
      <section aria-labelledby="lp-allow-heading">
        <h3
          id="lp-allow-heading"
          className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Allow list (never blocked)
        </h3>
        <p className="mb-3 text-xs text-[var(--color-muted-foreground)]">
          IPs in this list bypass all login-protection checks entirely. This is
          your lockout escape hatch — add your office or home CIDR here before
          enabling Protect mode. When you switch to Protect with an empty list,
          the server automatically adds your current IP.
        </p>

        {/* Chips */}
        {allowCidrs.length > 0 && (
          <ul
            aria-label="Allow list CIDRs"
            className="mb-3 flex flex-wrap gap-2"
          >
            {allowCidrs.map((cidr) => (
              <li key={cidr}>
                <span className="inline-flex items-center gap-1.5 rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-2.5 py-1 font-mono text-xs text-[var(--color-foreground)]">
                  {cidr}
                  <button
                    type="button"
                    onClick={() =>
                      setAllowCidrs((prev) => prev.filter((c) => c !== cidr))
                    }
                    aria-label={`Remove ${cidr} from allow list`}
                    className="rounded-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-destructive)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                  >
                    <X aria-hidden="true" className="size-3" />
                  </button>
                </span>
              </li>
            ))}
          </ul>
        )}

        {/* Add CIDR input */}
        <div className="flex items-start gap-2">
          <div className="flex-1 space-y-1">
            <Input
              type="text"
              value={allowInput}
              onChange={(e) => {
                setAllowInput(e.target.value);
                setAllowInputError(null);
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  handleAddAllowCidr();
                }
              }}
              placeholder="e.g. 203.0.113.0/24 or 2001:db8::/32"
              aria-label="Add IP or CIDR to allow list"
              aria-describedby={
                allowInputError ? "allow-input-error" : undefined
              }
              className="font-mono text-xs"
            />
            {allowInputError && (
              <p
                id="allow-input-error"
                className="text-xs text-[var(--color-destructive)]"
              >
                {allowInputError}
              </p>
            )}
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={handleAddAllowCidr}
            className="shrink-0"
          >
            <Plus aria-hidden="true" className="size-3.5" />
            Add
          </Button>
        </div>

        <p className="mt-2 flex items-start gap-1.5 text-xs text-[var(--color-muted-foreground)]">
          <Info aria-hidden="true" className="mt-0.5 size-3.5 shrink-0" />
          When switching to Protect with an empty list, the server
          auto-adds your current operator IP. You&apos;ll see it reflected
          here after saving.
        </p>
      </section>

      {/* ── Deny CIDRs ── */}
      <section aria-labelledby="lp-deny-heading">
        <h3
          id="lp-deny-heading"
          className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Deny list (always blocked)
        </h3>
        <p className="mb-3 text-xs text-[var(--color-muted-foreground)]">
          IPs matching these CIDRs are denied immediately, before threshold
          evaluation. Use for known bad actors or ranges you never want on your
          login page.
        </p>

        {denyCidrs.length > 0 && (
          <ul
            aria-label="Deny list CIDRs"
            className="mb-3 flex flex-wrap gap-2"
          >
            {denyCidrs.map((cidr) => (
              <li key={cidr}>
                <span className="inline-flex items-center gap-1.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-destructive)]/8 px-2.5 py-1 font-mono text-xs text-[var(--color-foreground)]">
                  {cidr}
                  <button
                    type="button"
                    onClick={() =>
                      setDenyCidrs((prev) => prev.filter((c) => c !== cidr))
                    }
                    aria-label={`Remove ${cidr} from deny list`}
                    className="rounded-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-destructive)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                  >
                    <X aria-hidden="true" className="size-3" />
                  </button>
                </span>
              </li>
            ))}
          </ul>
        )}

        <div className="flex items-start gap-2">
          <div className="flex-1 space-y-1">
            <Input
              type="text"
              value={denyInput}
              onChange={(e) => {
                setDenyInput(e.target.value);
                setDenyInputError(null);
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  handleAddDenyCidr();
                }
              }}
              placeholder="e.g. 198.51.100.0/24"
              aria-label="Add IP or CIDR to deny list"
              aria-describedby={denyInputError ? "deny-input-error" : undefined}
              className="font-mono text-xs"
            />
            {denyInputError && (
              <p
                id="deny-input-error"
                className="text-xs text-[var(--color-destructive)]"
              >
                {denyInputError}
              </p>
            )}
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={handleAddDenyCidr}
            className="shrink-0"
          >
            <Plus aria-hidden="true" className="size-3.5" />
            Add
          </Button>
        </div>
      </section>

      {/* ── Thresholds ── */}
      <section aria-labelledby="lp-thresholds-heading">
        <h3
          id="lp-thresholds-heading"
          className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Brute-force thresholds
        </h3>
        <p className="mb-4 text-xs text-[var(--color-muted-foreground)]">
          Defaults: captcha after 3 failures, temporary block after 10, permanent
          block after 100. Gaps reset the counter after 30 minutes (1800 s) of
          inactivity.
        </p>

        {/* Limits group */}
        <fieldset className="mb-4">
          <legend className="mb-2 text-xs font-medium text-[var(--color-foreground)]">
            Failure limits
          </legend>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <ThresholdField
              id="captcha-limit"
              label="CAPTCHA after"
              unit="failures"
              value={thresholds.captcha_limit}
              onChange={(v) => handleThreshold("captcha_limit", v)}
              helpId="captcha-limit-help"
              help="Show a CAPTCHA challenge after this many consecutive failures."
            />
            <ThresholdField
              id="temp-block-limit"
              label="Temporary block after"
              unit="failures"
              value={thresholds.temp_block_limit}
              onChange={(v) => handleThreshold("temp_block_limit", v)}
              helpId="temp-block-limit-help"
              help="Apply a temporary IP block after this many consecutive failures."
            />
            <ThresholdField
              id="block-all-limit"
              label="Permanent block after"
              unit="failures"
              value={thresholds.block_all_limit}
              onChange={(v) => handleThreshold("block_all_limit", v)}
              helpId="block-all-limit-help"
              help="Apply a permanent block after this many consecutive failures."
            />
          </div>
        </fieldset>

        {/* Gaps group */}
        <fieldset>
          <legend className="mb-2 text-xs font-medium text-[var(--color-foreground)]">
            Reset gaps (seconds)
          </legend>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <ThresholdField
              id="failed-login-gap"
              label="Failed-login gap"
              unit="seconds"
              value={thresholds.failed_login_gap}
              onChange={(v) => handleThreshold("failed_login_gap", v)}
              helpId="failed-login-gap-help"
              help="Seconds of inactivity that reset the consecutive failure counter."
            />
            <ThresholdField
              id="success-login-gap"
              label="Success-login gap"
              unit="seconds"
              value={thresholds.success_login_gap}
              onChange={(v) => handleThreshold("success_login_gap", v)}
              helpId="success-login-gap-help"
              help="Seconds after a successful login before a new failure series begins."
            />
            <ThresholdField
              id="all-blocked-gap"
              label="Block duration"
              unit="seconds"
              value={thresholds.all_blocked_gap}
              onChange={(v) => handleThreshold("all_blocked_gap", v)}
              helpId="all-blocked-gap-help"
              help="Seconds a permanent block remains active before auto-expiry."
            />
          </div>
        </fieldset>
      </section>

      {/* ── IP header ── */}
      <section aria-labelledby="lp-ipheader-heading">
        <h3
          id="lp-ipheader-heading"
          className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Real-IP header
        </h3>
        <p className="mb-3 text-xs text-[var(--color-muted-foreground)]">
          Only change this if your site is behind a trusted reverse proxy or CDN.
          An incorrect value lets attackers spoof their IP by sending a forged
          header.
        </p>
        <div className="max-w-sm">
          <label htmlFor="ip-header-select" className="sr-only">
            Real-IP header
          </label>
          <select
            id="ip-header-select"
            value={ipHeader}
            onChange={(e) => setIpHeader(e.target.value)}
            className="flex h-9 w-full rounded-md border border-[var(--color-input)] bg-transparent px-3 py-1 font-mono text-sm shadow-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50"
          >
            {IP_HEADERS.map((h) => (
              <option key={h.value} value={h.value}>
                {h.label}
              </option>
            ))}
            {/* Show the raw value if it's not in the preset list */}
            {!IP_HEADERS.some((h) => h.value === ipHeader) && (
              <option value={ipHeader}>{ipHeader}</option>
            )}
          </select>
        </div>
      </section>

      {/* ── Save error ── */}
      {saveError ? (
        <PageError
          what="Could not save login-protection config."
          why={saveError}
        />
      ) : null}

      {/* ── Save ── */}
      <div className="flex items-center gap-3 border-t border-[var(--color-border)] pt-6">
        <Button
          type="button"
          onClick={handleSave}
          disabled={update.isPending}
          aria-busy={update.isPending}
        >
          {update.isPending ? "Saving..." : "Save config"}
        </Button>
        {initialConfig.updated_at ? (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Last saved{" "}
            <time dateTime={initialConfig.updated_at}>
              {new Date(initialConfig.updated_at).toLocaleString()}
            </time>
          </p>
        ) : (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Using built-in defaults — not yet saved.
          </p>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// ThresholdField — labelled numeric input with help text
// ---------------------------------------------------------------------------

interface ThresholdFieldProps {
  id: string;
  label: string;
  unit: string;
  value: number;
  onChange: (raw: string) => void;
  helpId: string;
  help: string;
}

function ThresholdField({
  id,
  label,
  unit,
  value,
  onChange,
  helpId,
  help,
}: ThresholdFieldProps) {
  return (
    <div className="space-y-1.5">
      <label
        htmlFor={id}
        className="block text-xs font-medium text-[var(--color-foreground)]"
      >
        {label}
      </label>
      <div className="flex items-center gap-2">
        <Input
          id={id}
          type="number"
          min={1}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-describedby={helpId}
          className="w-24 tabular-nums font-mono text-sm"
        />
        <span className="text-xs text-[var(--color-muted-foreground)]">
          {unit}
        </span>
      </div>
      <p id={helpId} className="text-xs text-[var(--color-muted-foreground)]">
        {help}
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function LoginProtectionSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading login protection config"
      className="space-y-8"
    >
      <span className="sr-only">Loading login protection config</span>
      {/* Mode skeleton */}
      <div className="space-y-2">
        <Skeleton className="h-3 w-20 mb-3" />
        <Skeleton className="h-14 w-full rounded-lg" />
        <Skeleton className="h-14 w-full rounded-lg" />
        <Skeleton className="h-14 w-full rounded-lg" />
      </div>
      {/* CIDRs skeleton */}
      <div className="space-y-2">
        <Skeleton className="h-3 w-28 mb-3" />
        <Skeleton className="h-9 w-full" />
      </div>
      {/* Thresholds skeleton */}
      <div>
        <Skeleton className="h-3 w-36 mb-4" />
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className="space-y-1.5">
              <Skeleton className="h-3 w-24" />
              <Skeleton className="h-9 w-24" />
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
