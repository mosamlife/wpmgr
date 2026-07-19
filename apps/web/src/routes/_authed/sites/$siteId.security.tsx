import { useCallback } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  ShieldCheck,
  KeyRound,
  Lock,
  FileSearch,
  Ban,
  EyeOff,
  Info,
  ShieldAlert,
} from "lucide-react";

import { useMe, canOperate } from "@/features/auth/use-auth";
import { LoginProtectionPanel } from "@/features/security/login-protection-panel";
import { LoginEventsTable } from "@/features/security/login-events-table";
import { ScanPanel } from "@/features/security/scan-panel";
import { HardeningPanel } from "@/features/security/hardening-panel";
import { BanListPanel } from "@/features/security/ban-list-panel";
import { PolicyPanel } from "@/features/security/policy-panel";
import {
  SecurityCard,
  type CardStatus,
} from "@/features/security/security-card";
import { SecurityOverview } from "@/features/security/security-overview";
import { Button } from "@/components/ui/button";

import { useHardeningConfig } from "@/features/security/use-hardening";
import { useSecurityConfig } from "@/features/security/use-security";
import { useScanRuns, useStartScan } from "@/features/security/use-scan";
import { useSiteSecurityPolicy } from "@/features/security/use-policy";
import { useSiteVulnerabilities, countHighRisk } from "@/features/security/use-vuln";
import { VulnPanel } from "@/features/security/vuln-panel";
import { toast } from "@/components/toast";

// `/sites/$siteId/security` — six cards (Impeccable card-based layout).
//
// Card order (matches SecurityOverview tiles):
//   1. Login & Two-Factor   (ShieldCheck)  — policy-panel 2FA + password + hide sections split
//   2. Password policy      (KeyRound)     — password controls from policy-panel
//   3. Hardening            (Lock)         — 10 hardening toggles
//   4. File integrity       (FileSearch)   — scan-panel + findings; "Run scan" in header
//   5. Bans & login         (Ban)          — login-protection-panel + ban-list + events
//   6. Hide login           (EyeOff)       — hide-backend slug from policy-panel
//
// The Vulnerabilities stub (lines 108-131 in the old file) is removed entirely —
// it was a dead empty section with no data hook or CTA.
//
// Write access: canOperate(me) → owner / admin / operator.
// Viewers see all panels read-only (controls disabled or hidden by each panel).

export const Route = createFileRoute("/_authed/sites/$siteId/security")({
  component: SecurityTab,
});

// ---------------------------------------------------------------------------
// Helpers — derive card status pills from live query data
// ---------------------------------------------------------------------------

function hardeningStatus(
  config: ReturnType<typeof useHardeningConfig>["data"],
): CardStatus {
  if (!config) return { variant: "muted", label: "Loading" };
  const boolToggles = [
    config.disable_file_editor,
    config.disable_php_in_uploads,
    config.protect_system_files,
    config.disable_directory_browsing,
    config.force_unique_nickname,
    config.disable_author_archive_enum,
    config.force_ssl,
  ] as const;
  const boolOn = boolToggles.filter(Boolean).length;
  const xmlrpcOn = config.xmlrpc_mode !== "on" ? 1 : 0;
  const restOn = config.restrict_rest_api === "restricted" ? 1 : 0;
  const loginOn = config.restrict_login_identifier !== "both" ? 1 : 0;
  const total = boolOn + xmlrpcOn + restOn + loginOn;
  if (total >= 7) return { variant: "success", label: `${total} / 10 on` };
  if (total >= 4) return { variant: "warning", label: `${total} / 10 on` };
  return { variant: "destructive", label: `${total} / 10 on` };
}

function loginProtectionStatus(
  config: ReturnType<typeof useSecurityConfig>["data"],
): CardStatus {
  if (!config) return { variant: "muted", label: "Loading" };
  if (config.mode === "protect") return { variant: "success", label: "Protect" };
  if (config.mode === "audit") return { variant: "warning", label: "Audit" };
  return { variant: "muted", label: "Off" };
}

function scanStatus(
  runs: ReturnType<typeof useScanRuns>["data"],
): CardStatus {
  if (!runs) return { variant: "muted", label: "Loading" };
  const latest = runs[0];
  if (!latest) return { variant: "muted", label: "Never scanned" };
  if (latest.status === "done") {
    const total = Object.values(latest.finding_counts ?? {}).reduce(
      (s, n) => s + n,
      0,
    );
    if (total === 0) return { variant: "success", label: "Clean" };
    return {
      variant: "destructive",
      label: `${total} finding${total !== 1 ? "s" : ""}`,
    };
  }
  if (latest.status === "failed") return { variant: "warning", label: "Failed" };
  return { variant: "warning", label: "Scanning" };
}

function twoFactorStatus(
  policy: ReturnType<typeof useSiteSecurityPolicy>["data"],
): CardStatus {
  if (!policy) return { variant: "muted", label: "Loading" };
  if (!policy.two_factor_enabled) return { variant: "muted", label: "Off" };
  const required = policy.two_factor_required_roles.length > 0;
  return {
    variant: "success",
    label: required ? `Required · ${policy.two_factor_required_roles.length} role${policy.two_factor_required_roles.length !== 1 ? "s" : ""}` : "Optional",
  };
}

function passwordPolicyStatus(
  policy: ReturnType<typeof useSiteSecurityPolicy>["data"],
): CardStatus {
  if (!policy) return { variant: "muted", label: "Loading" };
  const hasAny =
    policy.password_min_zxcvbn_score > 0 ||
    policy.password_block_compromised ||
    policy.password_reuse_block_count > 0 ||
    policy.password_max_age_days > 0;
  return hasAny
    ? { variant: "success", label: "Active" }
    : { variant: "muted", label: "Off" };
}

function hideLoginStatus(
  policy: ReturnType<typeof useSiteSecurityPolicy>["data"],
): CardStatus {
  if (!policy) return { variant: "muted", label: "Loading" };
  return policy.hide_backend_enabled
    ? { variant: "success", label: "Active" }
    : { variant: "muted", label: "Off" };
}

// GH #245: `countHighRisk` treats `unknown`-severity findings as elevated
// (same as critical/high), so a site with only unrated findings correctly
// falls into the `destructive` branch below rather than `success`/`warning`.
// A fleet of unrated findings must never render as a benign card status.
function vulnStatus(
  vulnData: ReturnType<typeof useSiteVulnerabilities>["data"],
): CardStatus {
  if (!vulnData) return { variant: "muted", label: "Loading" };
  if (!vulnData.feed_ok) return { variant: "muted", label: "Feed not configured" };
  const openFindings = (vulnData.items ?? []).filter((f) => f.status === "open");
  if (openFindings.length === 0) return { variant: "success", label: "Clean" };
  const highRisk = countHighRisk(vulnData.items ?? []);
  if (highRisk > 0) {
    return {
      variant: "destructive",
      label: `${openFindings.length} finding${openFindings.length !== 1 ? "s" : ""}`,
    };
  }
  return {
    variant: "warning",
    label: `${openFindings.length} finding${openFindings.length !== 1 ? "s" : ""}`,
  };
}

// ---------------------------------------------------------------------------
// Run scan button (used in the File integrity card header action slot)
// ---------------------------------------------------------------------------

function RunScanButton({
  siteId,
  canWrite,
  isBusy,
}: {
  siteId: string;
  canWrite: boolean;
  isBusy: boolean;
}) {
  const start = useStartScan(siteId);

  if (!canWrite) return null;

  function handleStart() {
    start.mutate("core", {
      onSuccess: () => {
        toast.success("Scan started.", {
          description: "Comparing core files against WordPress.org checksums.",
        });
      },
      onError: (err: Error) => {
        toast.error("Could not start scan.", { description: err.message });
      },
    });
  }

  return (
    <Button
      type="button"
      size="sm"
      variant="outline"
      disabled={isBusy || start.isPending}
      aria-busy={start.isPending}
      onClick={handleStart}
      className="shrink-0 text-xs"
    >
      <FileSearch aria-hidden="true" className="size-3.5" />
      {start.isPending ? "Starting..." : "Run scan"}
    </Button>
  );
}

// ---------------------------------------------------------------------------
// SecurityTab
// ---------------------------------------------------------------------------

function SecurityTab() {
  const { siteId } = Route.useParams();
  const { data: me } = useMe();
  const canWrite = canOperate(me);

  // Pre-fetch data for the overview tiles; panels own their own hooks too
  // but TanStack Query deduplicates identical query keys.
  const hardeningQuery = useHardeningConfig(siteId);
  const loginProtQuery = useSecurityConfig(siteId);
  const scanRunsQuery = useScanRuns(siteId);
  const policyQuery = useSiteSecurityPolicy(siteId);
  const vulnQuery = useSiteVulnerabilities(siteId);

  const overviewLoading =
    hardeningQuery.isPending ||
    loginProtQuery.isPending ||
    scanRunsQuery.isPending ||
    policyQuery.isPending ||
    vulnQuery.isPending;

  const latestScanRun = scanRunsQuery.data?.[0] ?? null;
  const scanBusy =
    (scanRunsQuery.data ?? []).some(
      (r) => r.status === "queued" || r.status === "scanning" || r.status === "diffing",
    ) ?? false;

  // Scroll-to-card: find by data-card-id attribute and expand/focus.
  const handleTileClick = useCallback((cardId: string) => {
    const el = document.querySelector<HTMLElement>(`[data-card-id="${cardId}"]`);
    if (!el) return;
    // Expand the card if it isn't already (the SecurityCard manages its own state;
    // we trigger by dispatching a click on the header button inside it).
    const btn = el.querySelector<HTMLButtonElement>("button[aria-expanded]");
    if (btn && btn.getAttribute("aria-expanded") === "false") {
      btn.click();
    }
    el.scrollIntoView({ behavior: "smooth", block: "start" });
    btn?.focus();
  }, []);

  return (
    <div className="space-y-4 p-4 sm:p-6">
      {/* ── Overview strip ── */}
      <SecurityOverview
        hardeningConfig={hardeningQuery.data}
        loginProtectionConfig={loginProtQuery.data}
        latestScanRun={latestScanRun}
        policy={policyQuery.data}
        vulnData={vulnQuery.data}
        isLoading={overviewLoading}
        onTileClick={handleTileClick}
      />

      {/* ── Card 1: Login & Two-Factor ── */}
      <div data-card-id="card-login-2fa">
        <SecurityCard
          id="card-login-2fa"
          icon={<ShieldCheck className="size-5" />}
          iconTint="bg-[var(--color-primary)]/10 text-[var(--color-primary)]"
          title="Login & Two-Factor"
          purpose="Require a second login step for sensitive roles. Users set up an authenticator app on their first login after you turn this on."
          status={twoFactorStatus(policyQuery.data)}
        >
          {/* 2FA explainer — shown always, above the controls */}
          <div
            role="note"
            className="mb-6 flex items-start gap-3 rounded-lg border border-[var(--color-primary)]/20 bg-[var(--color-primary)]/6 px-4 py-3"
          >
            <Info
              aria-hidden="true"
              className="mt-0.5 size-4 shrink-0 text-[var(--color-primary)]"
            />
            <p className="text-xs text-[var(--color-foreground)]">
              Users enroll on their own site — on their next login (or from their
              WordPress profile) they scan a QR code and save backup codes. Their
              secret stays on the site; this dashboard turns the requirement on or
              off and shows which roles must comply.
            </p>
          </div>

          {/* PolicyPanel — renders only 2FA sub-section controls here */}
          <PolicyPanel
            siteId={siteId}
            canWrite={canWrite}
            section="two_factor"
          />
        </SecurityCard>
      </div>

      {/* ── Card 2: Password policy ── */}
      <div data-card-id="card-password">
        <SecurityCard
          id="card-password"
          icon={<KeyRound className="size-5" />}
          iconTint="bg-amber-100 text-amber-600 dark:bg-amber-950 dark:text-amber-400"
          title="Password policy"
          purpose="Stop weak and reused passwords. Controls below apply when a WordPress user sets or changes their password."
          status={passwordPolicyStatus(policyQuery.data)}
        >
          <PolicyPanel
            siteId={siteId}
            canWrite={canWrite}
            section="password"
          />
        </SecurityCard>
      </div>

      {/* ── Card 3: Hardening ── */}
      <div data-card-id="card-hardening">
        <SecurityCard
          id="card-hardening"
          icon={<Lock className="size-5" />}
          iconTint="bg-slate-100 text-slate-600 dark:bg-slate-900 dark:text-slate-400"
          title="Hardening"
          purpose="Turn off risky features attackers target. Most of these are safe to enable on any production site."
          status={hardeningStatus(hardeningQuery.data)}
        >
          <HardeningPanel siteId={siteId} canWrite={canWrite} />
        </SecurityCard>
      </div>

      {/* ── Card 4: File integrity ── */}
      <div data-card-id="card-file-integrity">
        <SecurityCard
          id="card-file-integrity"
          icon={<FileSearch className="size-5" />}
          iconTint="bg-indigo-100 text-indigo-600 dark:bg-indigo-950 dark:text-indigo-400"
          title="File integrity"
          purpose="Compares your core, plugin, and theme files against known-good versions to detect unexpected changes."
          status={scanStatus(scanRunsQuery.data)}
          action={
            <RunScanButton
              siteId={siteId}
              canWrite={canWrite}
              isBusy={scanBusy}
            />
          }
        >
          <ScanPanel siteId={siteId} canWrite={canWrite} />
        </SecurityCard>
      </div>

      {/* ── Card 5: Bans & login protection ── */}
      <div data-card-id="card-bans">
        <SecurityCard
          id="card-bans"
          icon={<Ban className="size-5" />}
          iconTint="bg-red-100 text-red-600 dark:bg-red-950 dark:text-red-400"
          title="Bans & login protection"
          purpose="Automatically lock out repeated failed logins, and block specific IPs or bots."
          status={loginProtectionStatus(loginProtQuery.data)}
        >
          <div className="space-y-8">
            {/* Login protection config */}
            <section aria-labelledby="bans-lp-heading">
              <h3
                id="bans-lp-heading"
                className="mb-4 text-sm font-medium text-[var(--color-foreground)]"
              >
                Login protection
              </h3>
              <LoginProtectionPanel siteId={siteId} />
            </section>

            {/* Ban list */}
            <section
              aria-labelledby="bans-banlist-heading"
              className="border-t border-[var(--color-border)] pt-6"
            >
              <h3
                id="bans-banlist-heading"
                className="mb-1 text-sm font-medium text-[var(--color-foreground)]"
              >
                Blocked IPs and user agents
              </h3>
              <p className="mb-4 text-xs text-[var(--color-muted-foreground)]">
                Block specific IPs, CIDR ranges, or user agents from reaching
                the site. Rules are applied at the application layer.
              </p>
              <BanListPanel siteId={siteId} canWrite={canWrite} />
            </section>

            {/* Login events */}
            <section
              aria-labelledby="bans-events-heading"
              className="border-t border-[var(--color-border)] pt-6"
            >
              <LoginEventsTable siteId={siteId} />
            </section>
          </div>
        </SecurityCard>
      </div>

      {/* ── Card 6: Hide login ── */}
      <div data-card-id="card-hide-login">
        <SecurityCard
          id="card-hide-login"
          icon={<EyeOff className="size-5" />}
          iconTint="bg-purple-100 text-purple-600 dark:bg-purple-950 dark:text-purple-400"
          title="Hide login"
          purpose="Move your login page to a secret address so bots can't find it. Bookmark the new URL after saving."
          status={hideLoginStatus(policyQuery.data)}
        >
          <PolicyPanel
            siteId={siteId}
            canWrite={canWrite}
            section="hide_backend"
          />
        </SecurityCard>
      </div>

      {/* ── Card 7: Vulnerabilities ── */}
      <div data-card-id="card-vulnerabilities">
        <SecurityCard
          id="card-vulnerabilities"
          icon={<ShieldAlert className="size-5" />}
          iconTint="bg-orange-100 text-orange-600 dark:bg-orange-950 dark:text-orange-400"
          title="Vulnerabilities"
          purpose="Checks your installed plugins, themes, and WordPress version against a database of known security vulnerabilities."
          status={vulnStatus(vulnQuery.data)}
        >
          <VulnPanel siteId={siteId} canWrite={canWrite} />
        </SecurityCard>
      </div>
    </div>
  );
}
