import { Lock, ShieldCheck, Ban, FileSearch, ShieldAlert } from "lucide-react";

import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

import type { HardeningConfig } from "./use-hardening";
import type { SiteLoginProtectionConfig } from "@wpmgr/api";
import type { ScanRun } from "./use-scan";
import type { SiteSecurityPolicy } from "./use-policy";
import { isHighRisk, type SiteVulnsResponse } from "./use-vuln";

// SecurityOverview — four status tiles at the top of the Security tab.
//
// Each tile: lucide icon + metric label + one-word state badge.
// Clicking a tile calls the supplied scroll handler so the corresponding
// card expands and scrolls into view.
//
// Data flows from the parent which already has the query results; the overview
// itself is stateless.
//
// Tile ordering matches the card order below:
//   1. Hardening   — N / 10 features on
//   2. Login       — mode (Disabled/Audit/Protect) + 24 h blocked count
//   3. File        — last scan status (Clean / N findings / Never scanned)
//   4. Auth policy — 2FA on/off

// ---------------------------------------------------------------------------
// Tile colours
// ---------------------------------------------------------------------------

type TileColor = "green" | "amber" | "red" | "muted";

const COLOR_CLASSES: Record<
  TileColor,
  { bg: string; icon: string; label: string }
> = {
  green: {
    bg: "border-green-200 bg-green-50 dark:border-green-800 dark:bg-green-950",
    icon: "text-green-600 dark:text-green-400",
    label: "text-green-700 dark:text-green-300",
  },
  amber: {
    bg: "border-amber-200 bg-amber-50 dark:border-amber-800 dark:bg-amber-950",
    icon: "text-amber-600 dark:text-amber-400",
    label: "text-amber-700 dark:text-amber-300",
  },
  red: {
    bg: "border-red-200 bg-red-50 dark:border-red-800 dark:bg-red-950",
    icon: "text-red-600 dark:text-red-400",
    label: "text-red-700 dark:text-red-300",
  },
  muted: {
    bg: "border-[var(--color-border)] bg-[var(--color-card)]",
    icon: "text-[var(--color-muted-foreground)]",
    label: "text-[var(--color-muted-foreground)]",
  },
};

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface SecurityOverviewProps {
  hardeningConfig: HardeningConfig | undefined;
  loginProtectionConfig: SiteLoginProtectionConfig | undefined;
  latestScanRun: ScanRun | null | undefined;
  policy: SiteSecurityPolicy | undefined;
  /** Vulnerability data — used for the 5th tile. */
  vulnData?: SiteVulnsResponse | undefined;
  isLoading: boolean;
  onTileClick: (cardId: string) => void;
}

// ---------------------------------------------------------------------------
// Derive tile data from query results
// ---------------------------------------------------------------------------

function countHardeningOn(config: HardeningConfig): number {
  const booleanToggles: (keyof HardeningConfig)[] = [
    "disable_file_editor",
    "disable_php_in_uploads",
    "protect_system_files",
    "disable_directory_browsing",
    "force_unique_nickname",
    "disable_author_archive_enum",
    "force_ssl",
  ];
  const xmlrpcOn = config.xmlrpc_mode !== "on"; // "off" or "limited" = hardened
  const restHardened = config.restrict_rest_api === "restricted";
  const loginHardened = config.restrict_login_identifier !== "both";
  const boolCount = booleanToggles.filter((k) => Boolean(config[k])).length;
  return boolCount + (xmlrpcOn ? 1 : 0) + (restHardened ? 1 : 0) + (loginHardened ? 1 : 0);
}

// ---------------------------------------------------------------------------
// Individual tile
// ---------------------------------------------------------------------------

function OverviewTile({
  icon,
  metric,
  stateLabel,
  color,
  onClick,
  ariaLabel,
}: {
  icon: React.ReactNode;
  metric: string;
  stateLabel: string;
  color: TileColor;
  onClick: () => void;
  ariaLabel: string;
}) {
  const c = COLOR_CLASSES[color];
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={ariaLabel}
      className={cn(
        "flex cursor-pointer flex-col gap-2 rounded-xl border p-4 text-left transition-opacity",
        "hover:opacity-80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-2",
        c.bg,
      )}
    >
      <span className={cn("size-5 shrink-0", c.icon)} aria-hidden="true">
        {icon}
      </span>
      <span className="space-y-0.5">
        <span className="block text-base font-semibold text-[var(--color-foreground)] tabular-nums">
          {metric}
        </span>
        <span className={cn("block text-xs font-medium", c.label)}>
          {stateLabel}
        </span>
      </span>
    </button>
  );
}

// ---------------------------------------------------------------------------
// SecurityOverview
// ---------------------------------------------------------------------------

export function SecurityOverview({
  hardeningConfig,
  loginProtectionConfig,
  latestScanRun,
  policy,
  vulnData,
  isLoading,
  onTileClick,
}: SecurityOverviewProps) {
  if (isLoading) {
    return (
      <div
        role="status"
        aria-label="Loading security overview"
        className="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-5"
      >
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-24 w-full rounded-xl" />
        ))}
      </div>
    );
  }

  // ── Hardening tile ──
  const hardeningOn = hardeningConfig ? countHardeningOn(hardeningConfig) : 0;
  const hardeningTotal = 10;
  const hardeningColor: TileColor =
    !hardeningConfig
      ? "muted"
      : hardeningOn >= 7
        ? "green"
        : hardeningOn >= 4
          ? "amber"
          : "red";

  // ── Login tile ──
  const loginMode = loginProtectionConfig?.mode ?? "disabled";
  const loginColor: TileColor =
    loginMode === "protect" ? "green" : loginMode === "audit" ? "amber" : "muted";
  const loginMetric =
    loginMode === "disabled" ? "Disabled" : loginMode === "audit" ? "Audit" : "Protect";

  // ── File integrity tile ──
  let scanMetric = "Never scanned";
  let scanStateLabel = "No baseline";
  let scanColor: TileColor = "muted";
  if (latestScanRun) {
    if (latestScanRun.status === "done") {
      const total = Object.values(latestScanRun.finding_counts ?? {}).reduce(
        (sum, n) => sum + n,
        0,
      );
      if (total === 0) {
        scanMetric = "Clean";
        scanStateLabel = "No issues";
        scanColor = "green";
      } else {
        scanMetric = `${total} issue${total !== 1 ? "s" : ""}`;
        scanStateLabel = "Review needed";
        scanColor = "red";
      }
    } else if (latestScanRun.status === "failed") {
      scanMetric = "Failed";
      scanStateLabel = "Last scan failed";
      scanColor = "amber";
    } else {
      scanMetric = "Scanning";
      scanStateLabel = "In progress";
      scanColor = "amber";
    }
  }

  // ── 2FA / policy tile ──
  const tfaEnabled = policy?.two_factor_enabled ?? false;
  const tfaMetric = tfaEnabled ? "2FA on" : "2FA off";
  const tfaColor: TileColor = tfaEnabled ? "green" : "muted";
  const tfaState = tfaEnabled
    ? (policy?.two_factor_required_roles?.length ?? 0) > 0
      ? `Required for ${policy!.two_factor_required_roles.length} role${policy!.two_factor_required_roles.length !== 1 ? "s" : ""}`
      : "Optional for all"
    : "Not configured";

  // ── Vulnerabilities tile ──
  let vulnMetric = "Not scanned";
  let vulnStateLabel = "Feed not configured";
  let vulnColor: TileColor = "muted";
  if (vulnData) {
    if (!vulnData.feed_ok) {
      vulnMetric = "No feed";
      vulnStateLabel = "Setup required";
      vulnColor = "muted";
    } else {
      const openCount = (vulnData.items ?? []).filter(
        (f) => f.status === "open",
      ).length;
      // GH #245: unknown-severity findings count as high risk too (NOT
      // mild). An unrated finding could be a CVSS 9.8 that enrichment
      // simply hasn't scored yet, so it must never fall into the calm
      // "amber" branch below alongside genuinely-confirmed medium/low.
      const highRiskCount = (vulnData.items ?? []).filter(
        (f) => f.status === "open" && isHighRisk(f.severity),
      ).length;
      if (openCount === 0) {
        vulnMetric = "Clean";
        vulnStateLabel = "No vulnerabilities";
        vulnColor = "green";
      } else {
        vulnMetric = `${openCount}`;
        vulnStateLabel =
          highRiskCount > 0
            ? `${highRiskCount} critical/high`
            : `${openCount} open`;
        vulnColor = highRiskCount > 0 ? "red" : "amber";
      }
    }
  }

  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-5">
      <OverviewTile
        icon={<Lock className="size-5" />}
        metric={`${hardeningOn} / ${hardeningTotal}`}
        stateLabel="Hardening on"
        color={hardeningColor}
        onClick={() => onTileClick("card-hardening")}
        ariaLabel={`Hardening: ${hardeningOn} of ${hardeningTotal} features active. Go to hardening.`}
      />
      <OverviewTile
        icon={<Ban className="size-5" />}
        metric={loginMetric}
        stateLabel="Login defense"
        color={loginColor}
        onClick={() => onTileClick("card-bans")}
        ariaLabel={`Login protection mode: ${loginMode}. Go to bans and login protection.`}
      />
      <OverviewTile
        icon={<FileSearch className="size-5" />}
        metric={scanMetric}
        stateLabel={scanStateLabel}
        color={scanColor}
        onClick={() => onTileClick("card-file-integrity")}
        ariaLabel={`File integrity: ${scanMetric}. Go to file integrity.`}
      />
      <OverviewTile
        icon={<ShieldCheck className="size-5" />}
        metric={tfaMetric}
        stateLabel={tfaState}
        color={tfaColor}
        onClick={() => onTileClick("card-login-2fa")}
        ariaLabel={`2FA: ${tfaMetric}. Go to login and two-factor settings.`}
      />
      <OverviewTile
        icon={<ShieldAlert className="size-5" />}
        metric={vulnMetric}
        stateLabel={vulnStateLabel}
        color={vulnColor}
        onClick={() => onTileClick("card-vulnerabilities")}
        ariaLabel={`Vulnerabilities: ${vulnMetric}. Go to vulnerability scanner.`}
      />
    </div>
  );
}
