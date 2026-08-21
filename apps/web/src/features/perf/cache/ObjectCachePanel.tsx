import {
  useState,
  useId,
  type ReactNode,
} from "react";
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  ChevronUp,
  Database,
  Loader2,
  MemoryStick,
  RefreshCw,
  Timer,
  XCircle,
  Zap,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import {
  ObjectCacheHitRatioChart,
  ObjectCacheMemoryChart,
  ObjectCacheLatencyChart,
  ObjectCacheOpsChart,
} from "@/components/charts/object-cache-charts";
import { toast } from "@/components/toast";
import { cn } from "@/lib/utils";
import { formatBytes } from "../format";
import { SelectField, NumberField, TextField } from "../components/Field";
import { SettingRow } from "../components/SettingRow";
import { SettingsCard } from "../components/SettingsCard";
import {
  useObjectCacheConfig,
  useUpdateObjectCacheConfig,
  useTestObjectCache,
  useEnableObjectCache,
  useDisableObjectCache,
  useFlushObjectCache,
  useObjectCacheStatsHistory,
} from "../hooks/useObjectCache";
import type { ObjectCacheConfigPut, ObjectCacheTestResult } from "@wpmgr/api";

// ---------------------------------------------------------------------------
// Interfaces
// ---------------------------------------------------------------------------

export interface ObjectCachePanelProps {
  siteId: string;
  /** Agent version string from the site record (may be empty). */
  agentVersion?: string;
  /** operator+ — change settings, test, enable/disable, flush */
  canOperate: boolean;
}

// Minimum agent version that ships the object cache engine.
// Anything older shows an upgrade nudge instead of the full panel.
const MIN_AGENT_VERSION = "0.39.0";

function isAgentSufficient(version: string | undefined): boolean {
  if (!version) return false;
  const parse = (v: string) =>
    v
      .replace(/^v/, "")
      .split(".")
      .map((p) => parseInt(p, 10) || 0);
  const a = parse(version);
  const b = parse(MIN_AGENT_VERSION);
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const av = a[i] ?? 0;
    const bv = b[i] ?? 0;
    if (av > bv) return true;
    if (av < bv) return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// Error-class (cause) vocabulary
// ---------------------------------------------------------------------------

interface CauseEntry {
  label: string;
  description: string;
}

const CAUSE_VOCAB: Record<string, CauseEntry> = {
  // --- live engine causes (set inside the drop-in on boot) ---
  config_empty: {
    label: "Configuration missing",
    description:
      "No Redis connection settings have been saved yet. Configure the object cache to get started.",
  },
  config_unreadable: {
    label: "Configuration unreadable",
    description:
      "The engine could not read its configuration file. Check file permissions or re-save the configuration.",
  },
  classes_missing: {
    label: "PHP extension not loaded",
    description:
      "The phpredis extension is not available in this PHP environment. Install and enable phpredis to use the object cache.",
  },
  unsupported_codec: {
    label: "Unsupported serializer or compression",
    description:
      "The configured serializer or compression codec is not available in this PHP environment. Switch to a supported option in the configuration.",
  },
  cooldown: {
    label: "Reconnect cool-down",
    description:
      "The connection is in a temporary retry cool-down after recent failures. The site is serving from the in-request array cache and will reconnect automatically.",
  },
  connect_budget_exhausted: {
    label: "Connection attempt limit reached",
    description:
      "The engine reached its per-request connection attempt limit and degraded safely for this request. It will retry on the next request.",
  },
  // --- heartbeat / non-activation causes (Phase B diagnosis) ---
  engine_not_loaded: {
    label: "Engine not loaded",
    description:
      "The drop-in is present but the engine class was not registered. This usually indicates an incomplete activation.",
  },
  engine_boot_incomplete: {
    label: "Engine boot incomplete",
    description:
      "The drop-in loaded but the engine did not finish initialising. Re-enabling the object cache or flushing the OPcache should resolve this.",
  },
  engine_replaced: {
    label: "Engine replaced",
    description:
      "A different object-cache implementation is active in place of the WPMgr engine. Disable the other plugin or drop-in before enabling the object cache here.",
  },
  dropin_missing: {
    label: "Drop-in not installed",
    description:
      "The object-cache drop-in file is missing from wp-content. Enable the object cache to reinstall it.",
  },
  dropin_outdated: {
    label: "Drop-in outdated",
    description:
      "The installed drop-in is from an older agent version. Re-enabling the object cache will update it to the current version.",
  },
  foreign_dropin: {
    label: "Third-party drop-in active",
    description:
      "A drop-in installed by another plugin is loaded. Remove or deactivate it before enabling the WPMgr object cache.",
  },
  stale_opcache_suspected: {
    label: "OPcache may be serving stale code",
    description:
      "The drop-in file was updated but the PHP OPcache appears to be serving the previous version. Flushing the OPcache should restore normal operation.",
  },
  filter_suppressed: {
    label: "Cache suppressed by filter",
    description:
      "A WordPress filter is disabling object caching at runtime. Check for plugins or code that return false from the enable_wp_debug_mode or wp_using_ext_object_cache filters.",
  },
  early_definition: {
    label: "Cache class defined too early",
    description:
      "Another plugin or must-use plugin defined the WP_Object_Cache class before the drop-in loaded. Deactivate conflicting plugins or adjust load order.",
  },
  bail_php_floor: {
    label: "PHP version too old",
    description:
      "The server's PHP version does not meet the minimum requirement for the object cache engine. Update PHP to use this feature.",
  },
  bail_installing: {
    label: "WordPress install in progress",
    description:
      "WordPress is in installation mode. The object cache will activate automatically once the site is fully installed.",
  },
  bail_killswitch: {
    label: "Cache disabled by constant",
    description:
      "The WPMGR_OBJECT_CACHE_DISABLE constant is set, preventing the engine from loading. Remove or set it to false to re-enable.",
  },
};

/**
 * Returns a human-readable label for a raw cause string from the agent.
 * Unknown causes fall back to a title-cased version of the raw string so
 * no information is lost and no raw identifier leaks unformatted into the UI.
 */
function formatCauseLabel(cause: string): string {
  return CAUSE_VOCAB[cause]?.label ?? cause.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

function formatCauseDescription(cause: string): string | undefined {
  return CAUSE_VOCAB[cause]?.description;
}

// ---------------------------------------------------------------------------
// Status helpers
// ---------------------------------------------------------------------------

type OcState = "connected" | "degraded" | "down" | "disabled" | "";

function StateLabel({ state }: { state: OcState }) {
  if (state === "connected") {
    return (
      <span className="inline-flex items-center gap-1.5 rounded-full bg-green-100 px-2.5 py-0.5 text-xs font-medium text-green-800 ring-1 ring-green-200 dark:bg-green-950 dark:text-green-300 dark:ring-green-900">
        <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-green-500" />
        Connected
      </span>
    );
  }
  if (state === "degraded") {
    return (
      <span className="inline-flex items-center gap-1.5 rounded-full bg-yellow-100 px-2.5 py-0.5 text-xs font-medium text-yellow-800 ring-1 ring-yellow-200 dark:bg-yellow-950 dark:text-yellow-300 dark:ring-yellow-900">
        <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-yellow-500" />
        Degraded
      </span>
    );
  }
  if (state === "down") {
    return (
      <span className="inline-flex items-center gap-1.5 rounded-full bg-red-100 px-2.5 py-0.5 text-xs font-medium text-red-800 ring-1 ring-red-200 dark:bg-red-950 dark:text-red-300 dark:ring-red-900">
        <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-red-500" />
        Down
      </span>
    );
  }
  // "disabled", "" — both render as "Disabled"
  return (
    <span className="inline-flex items-center gap-1.5 rounded-full bg-muted px-2.5 py-0.5 text-xs font-medium text-muted-foreground ring-1 ring-border">
      <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-muted-foreground" />
      Disabled
    </span>
  );
}

// ---------------------------------------------------------------------------
// Status header
// ---------------------------------------------------------------------------

interface StatusHeaderProps {
  oc_state: OcState;
  oc_latency_ms: number;
  oc_used_memory_bytes: number;
  oc_hit_ratio_pct?: number | null;
  oc_last_error_class?: string | null;
  enabled: boolean;
  canOperate: boolean;
  onEnable: () => void;
  onDisable: () => void;
  onFlush: () => void;
  onOpenConfig: () => void;
  isEnabling: boolean;
  isDisabling: boolean;
  isFlushing: boolean;
}

function StatusHeader({
  oc_state,
  oc_latency_ms,
  oc_used_memory_bytes,
  oc_hit_ratio_pct,
  oc_last_error_class,
  enabled,
  canOperate,
  onEnable,
  onDisable,
  onFlush,
  onOpenConfig,
  isEnabling,
  isDisabling,
  isFlushing,
}: StatusHeaderProps) {
  return (
    <div className="rounded-xl border border-border bg-card text-card-foreground shadow-sm">
      <div className="flex flex-wrap items-center justify-between gap-4 px-5 py-4">
        <div className="flex flex-wrap items-center gap-3">
          <StateLabel state={oc_state} />

          {oc_state === "connected" || oc_state === "degraded" ? (
            <>
              <StatChip
                icon={<Timer aria-hidden="true" className="size-3.5" />}
                label="Latency"
                value={`${oc_latency_ms.toFixed(1)} ms`}
              />
              <StatChip
                icon={<MemoryStick aria-hidden="true" className="size-3.5" />}
                label="Memory"
                value={formatBytes(oc_used_memory_bytes)}
              />
              {oc_hit_ratio_pct != null ? (
                <StatChip
                  icon={<Zap aria-hidden="true" className="size-3.5" />}
                  label="Hit ratio"
                  value={`${oc_hit_ratio_pct.toFixed(1)}%`}
                />
              ) : null}
            </>
          ) : null}

          {(oc_state === "degraded" || oc_state === "down") && oc_last_error_class ? (
            <span
              className="text-xs text-destructive"
              title={formatCauseDescription(oc_last_error_class)}
            >
              {formatCauseLabel(oc_last_error_class)}
            </span>
          ) : null}
        </div>

        {canOperate ? (
          <div className="flex flex-wrap items-center gap-2">
            {enabled ? (
              <>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={onFlush}
                  disabled={isFlushing}
                  aria-label="Flush object cache"
                >
                  {isFlushing ? (
                    <Loader2 aria-hidden="true" className="animate-spin" />
                  ) : (
                    <RefreshCw aria-hidden="true" />
                  )}
                  Flush
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={onDisable}
                  disabled={isDisabling}
                  aria-label="Disable object cache"
                >
                  {isDisabling ? (
                    <Loader2 aria-hidden="true" className="animate-spin" />
                  ) : null}
                  Disable
                </Button>
              </>
            ) : (
              <Button
                type="button"
                size="sm"
                onClick={onEnable}
                disabled={isEnabling}
                aria-label="Enable object cache"
              >
                {isEnabling ? (
                  <Loader2 aria-hidden="true" className="animate-spin" />
                ) : null}
                Enable
              </Button>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onOpenConfig}
            >
              Configure
            </Button>
          </div>
        ) : null}
      </div>
    </div>
  );
}

interface StatChipProps {
  icon: ReactNode;
  label: string;
  value: string;
}

function StatChip({ icon, label, value }: StatChipProps) {
  return (
    <span className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-0.5 text-xs font-medium tabular-nums text-foreground">
      {icon}
      <span className="text-muted-foreground">{label}:</span> {value}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Capability chips (server requirements row)
// ---------------------------------------------------------------------------

interface CapabilityChipProps {
  ok: boolean;
  label: string;
}

function CapabilityChip({ ok, label }: CapabilityChipProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs font-medium ring-1",
        ok
          ? "bg-green-50 text-green-800 ring-green-200 dark:bg-green-950 dark:text-green-300 dark:ring-green-900"
          : "bg-muted text-muted-foreground ring-border",
      )}
    >
      {ok ? (
        <CheckCircle2 aria-hidden="true" className="size-3 text-green-600 dark:text-green-400" />
      ) : (
        <XCircle aria-hidden="true" className="size-3 text-muted-foreground" />
      )}
      {label}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Test result card
// ---------------------------------------------------------------------------

interface TestResultCardProps {
  result: ObjectCacheTestResult;
}

function TestResultCard({ result }: TestResultCardProps) {
  const evictionOk =
    result.eviction_policy &&
    (result.eviction_policy.includes("allkeys") ||
      result.eviction_policy.includes("volatile"));

  return (
    <div
      className={cn(
        "rounded-lg border p-3 text-sm",
        result.ok
          ? "border-green-200 bg-green-50 dark:border-green-900 dark:bg-green-950"
          : "border-destructive/40 bg-destructive/10",
      )}
      role="status"
      aria-live="polite"
    >
      <div className="flex items-center gap-2 font-medium">
        {result.ok ? (
          <CheckCircle2 aria-hidden="true" className="size-4 text-green-600 dark:text-green-400" />
        ) : (
          <XCircle aria-hidden="true" className="size-4 text-destructive" />
        )}
        {result.ok ? "Connection succeeded" : "Connection failed"}
      </div>
      {result.detail ? (
        <p className="mt-1 text-xs text-muted-foreground">{result.detail}</p>
      ) : null}

      {result.ok ? (
        <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-3">
          {result.latency_ms !== undefined ? (
            <div>
              <dt className="text-muted-foreground">Latency</dt>
              <dd className="font-mono">{result.latency_ms.toFixed(1)} ms</dd>
            </div>
          ) : null}
          {result.server_version ? (
            <div>
              <dt className="text-muted-foreground">Server</dt>
              <dd className="font-mono">{result.server_version}</dd>
            </div>
          ) : null}
          {result.eviction_policy ? (
            <div>
              <dt className="text-muted-foreground">Eviction policy</dt>
              <dd className="flex items-center gap-1 font-mono">
                {result.eviction_policy}
                {!evictionOk ? (
                  <AlertTriangle
                    aria-label="Warning: noeviction policy may fill memory without reclaiming keys"
                    className="size-3 text-yellow-600 dark:text-yellow-400"
                  />
                ) : null}
              </dd>
            </div>
          ) : null}
        </dl>
      ) : null}

      {result.eviction_policy === "noeviction" ? (
        <p className="mt-2 text-xs text-yellow-800 dark:text-yellow-300">
          The server is configured with noeviction. Keys will never be
          reclaimed automatically. Consider switching to allkeys-lru.
        </p>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Server requirements (from last test result embedded in config)
// ---------------------------------------------------------------------------

interface ServerRequirementsRowProps {
  lastTestResult: ObjectCacheTestResult | null;
}

function ServerRequirementsRow({
  lastTestResult,
}: ServerRequirementsRowProps) {
  const caps = lastTestResult?.capabilities ?? null;
  const evictionOk =
    lastTestResult?.eviction_policy &&
    (lastTestResult.eviction_policy.includes("allkeys") ||
      lastTestResult.eviction_policy.includes("volatile"));
  const phpredisMissing = caps !== null && !caps.phpredis_version;
  const flushDedicated = lastTestResult?.flush_capability_class === "dedicated_db";

  return (
    <div className="px-5 py-4">
      <h4 className="mb-2 text-xs font-semibold uppercase tracking-[0.04em] text-muted-foreground">
        Server capabilities
      </h4>

      {!lastTestResult ? (
        <p className="text-sm text-muted-foreground">
          Run a connection test to detect server capabilities.
        </p>
      ) : phpredisMissing ? (
        <p className="text-sm text-[var(--color-destructive)]">
          The phpredis extension was not detected on this server. Install
          phpredis to use the object cache.
        </p>
      ) : (
        <div className="flex flex-wrap gap-2">
          <CapabilityChip
            ok={Boolean(caps?.phpredis_version)}
            label={
              caps?.phpredis_version
                ? `phpredis ${caps.phpredis_version}`
                : "phpredis not detected"
            }
          />
          <CapabilityChip
            ok={Boolean(caps?.igbinary_available)}
            label={caps?.igbinary_available ? "igbinary" : "igbinary not available"}
          />
          <CapabilityChip
            ok={Boolean(caps?.zstd_available || caps?.lz4_available || caps?.lzf_available)}
            label={
              caps?.zstd_available
                ? "zstd compression"
                : caps?.lz4_available
                  ? "lz4 compression"
                  : caps?.lzf_available
                    ? "lzf compression"
                    : "no compression extension"
            }
          />
          <CapabilityChip
            ok={Boolean(caps?.tls_supported)}
            label={caps?.tls_supported ? "TLS supported" : "TLS not supported"}
          />
          {lastTestResult.eviction_policy ? (
            <span
              className={cn(
                "inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs font-medium ring-1",
                evictionOk
                  ? "bg-[var(--color-success)]/10 text-[var(--color-success)] ring-[var(--color-success)]/30"
                  : "bg-[var(--color-warning)]/10 text-[var(--color-warning)] ring-[var(--color-warning)]/30",
              )}
            >
              {evictionOk ? (
                <CheckCircle2 aria-hidden="true" className="size-3" />
              ) : (
                <AlertTriangle aria-hidden="true" className="size-3" />
              )}
              {lastTestResult.eviction_policy}
            </span>
          ) : null}
          <CapabilityChip
            ok={flushDedicated}
            label={
              flushDedicated
                ? "Dedicated database"
                : "Shared database (prefix flush)"
            }
          />
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Connection config editor dialog
// ---------------------------------------------------------------------------

const SCHEME_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "tcp", label: "TCP" },
  { value: "unix", label: "Unix socket" },
  { value: "tls", label: "TCP + TLS" },
];

const SERIALIZER_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "php", label: "PHP (default)" },
  { value: "igbinary", label: "igbinary (faster, requires extension)" },
];

const COMPRESSION_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "none", label: "None" },
  { value: "lzf", label: "LZF" },
  { value: "lz4", label: "LZ4 (requires extension)" },
  { value: "zstd", label: "Zstd (recommended when available)" },
];

const FLUSH_STRATEGY_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "auto", label: "Auto (recommended)" },
  { value: "flushdb", label: "FLUSHDB (dedicated database only)" },
  { value: "scan", label: "SCAN + UNLINK (safe for shared instances)" },
];

interface ConfigDialogProps {
  open: boolean;
  onClose: () => void;
  siteId: string;
  config: import("@wpmgr/api").ObjectCacheConfig;
  canOperate: boolean;
  // Threads fresh test results up to the panel so the Server capabilities
  // row reflects the just-run test without waiting for the config refetch.
  onTestResult?: (result: ObjectCacheTestResult) => void;
}

function ConfigDialog({ open, onClose, siteId, config, canOperate, onTestResult }: ConfigDialogProps) {
  const update = useUpdateObjectCacheConfig(siteId);
  const testMutation = useTestObjectCache(siteId);

  // Local form state — draft separate from server config
  const [scheme, setScheme] = useState<string>(config.scheme ?? "tcp");
  const [host, setHost] = useState(config.host ?? "127.0.0.1");
  const [port, setPort] = useState(config.port ?? 6379);
  const [socketPath, setSocketPath] = useState(config.socket_path ?? "");
  const [database, setDatabase] = useState(config.database ?? 0);
  const [username, setUsername] = useState(config.username ?? "");
  const [password, setPassword] = useState(""); // always empty on open (write-only)
  const [prefix, setPrefix] = useState(config.prefix ?? "");
  const [shared, setShared] = useState(config.shared ?? true);
  const [flushStrategy, setFlushStrategy] = useState<string>(config.flush_strategy ?? "auto");
  const [serializer, setSerializer] = useState<string>(config.serializer ?? "php");
  const [compression, setCompression] = useState<string>(config.compression ?? "none");
  const [asyncFlush, setAsyncFlush] = useState(config.async_flush ?? false);
  const [flushOnFailback, setFlushOnFailback] = useState(config.flush_on_failback ?? true);
  const [analyticsEnabled, setAnalyticsEnabled] = useState(config.analytics_enabled ?? true);
  const [debugHeaderEnabled, setDebugHeaderEnabled] = useState(config.debug_header_enabled ?? false);
  const [maxttl, setMaxttl] = useState(config.maxttl_seconds ?? 604800);
  const [connectTimeout, setConnectTimeout] = useState(config.connect_timeout_ms ?? 1000);
  const [readTimeout, setReadTimeout] = useState(config.read_timeout_ms ?? 1000);
  const [retryCount, setRetryCount] = useState(config.retry_count ?? 3);
  const [retryInterval, setRetryInterval] = useState(config.retry_interval_ms ?? 25);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [testResult, setTestResult] = useState<ObjectCacheTestResult | null>(null);

  const isUnixScheme = scheme === "unix";
  const saving = update.isPending;
  const testing = testMutation.isPending;
  const disabled = !canOperate || saving;

  const titleId = useId();

  function buildPutBody(): ObjectCacheConfigPut {
    return {
      scheme: scheme as "tcp" | "unix" | "tls",
      host: isUnixScheme ? undefined : host,
      port: isUnixScheme ? undefined : port,
      socket_path: isUnixScheme ? socketPath : undefined,
      database,
      username: username || undefined,
      password: password || undefined, // empty = keep stored secret
      prefix: prefix || undefined,
      shared,
      flush_strategy: flushStrategy as "auto" | "flushdb" | "scan",
      serializer: serializer as "php" | "igbinary",
      compression: compression as "none" | "lzf" | "lz4" | "zstd",
      async_flush: asyncFlush,
      flush_on_failback: flushOnFailback,
      analytics_enabled: analyticsEnabled,
      debug_header_enabled: debugHeaderEnabled,
      maxttl_seconds: maxttl,
      connect_timeout_ms: connectTimeout,
      read_timeout_ms: readTimeout,
      retry_count: retryCount,
      retry_interval_ms: retryInterval,
    };
  }

  function handleSave() {
    update.mutate(buildPutBody(), {
      onSuccess: () => {
        toast.success("Configuration saved.");
        onClose();
      },
    });
  }

  function handleTest() {
    setTestResult(null);
    testMutation.mutate(password || undefined, {
      onSuccess: (result) => {
        setTestResult(result);
        onTestResult?.(result);
      },
      onError: (err) => {
        toast.error("Test failed.", { description: err.message });
      },
    });
  }

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Object cache configuration</DialogTitle>
        </DialogHeader>

        <DialogBody>
          <div className="space-y-5">
            {/* Connection type */}
            <SelectField
              label="Scheme"
              value={scheme}
              options={SCHEME_OPTIONS}
              onChange={setScheme}
              disabled={disabled}
            />

            {isUnixScheme ? (
              <TextField
                label="Socket path"
                value={socketPath}
                onChange={setSocketPath}
                placeholder="/var/run/redis/redis.sock"
                disabled={disabled}
                hint="Full path to the Unix domain socket."
              />
            ) : (
              <div className="grid grid-cols-3 gap-3">
                <div className="col-span-2">
                  <TextField
                    label="Host"
                    value={host}
                    onChange={setHost}
                    placeholder="127.0.0.1"
                    disabled={disabled}
                  />
                </div>
                <div>
                  <NumberField
                    label="Port"
                    value={port}
                    onCommit={setPort}
                    min={1}
                    max={65535}
                    disabled={disabled}
                  />
                </div>
              </div>
            )}

            <NumberField
              label="Database"
              value={database}
              onCommit={setDatabase}
              min={0}
              max={15}
              disabled={disabled}
              hint="Redis database number (0 is the default)."
            />

            <TextField
              label="Username (ACL)"
              value={username}
              onChange={setUsername}
              placeholder="optional"
              disabled={disabled}
              hint="Leave empty for password-only auth."
            />

            <div className="space-y-1.5">
              <label className="text-xs uppercase tracking-[0.02em] text-muted-foreground">
                Password
              </label>
              <Input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder={config.has_password ? "Stored password kept (leave empty to keep)" : "No password stored"}
                disabled={disabled}
                autoComplete="new-password"
                aria-label="Password"
              />
              <p className="text-xs text-muted-foreground">
                Write-only. Leave empty to keep the currently stored password.
              </p>
            </div>

            <TextField
              label="Key prefix"
              value={prefix}
              onChange={setPrefix}
              placeholder="auto (site-derived)"
              disabled={disabled}
              hint="Keys are scoped under this prefix. Leave empty to use the site-derived default."
            />

            <div className="space-y-3 rounded-lg border border-border p-3">
              <SettingRow
                label="Shared Redis instance"
                description="Uses SCAN+UNLINK prefix-scoped flush instead of FLUSHDB. Recommended for most managed hosting."
                checked={shared}
                onChange={setShared}
                disabled={disabled}
              />
              <SettingRow
                label="Flush on failback"
                description="Flush all keys when Redis recovers from a down state to avoid serving stale objects."
                checked={flushOnFailback}
                onChange={setFlushOnFailback}
                disabled={disabled}
              />
              <SettingRow
                label="Analytics"
                description="Track hit/miss ratios and memory usage over time."
                checked={analyticsEnabled}
                onChange={setAnalyticsEnabled}
                disabled={disabled}
              />
              <SettingRow
                label="Debug response header"
                description="Adds an x-wpmgr-object-cache header with live hit and miss counts to front end responses. Administrators always receive the header when logged in; this setting controls it for everyone else. Check a front end page to verify — the header does not appear on wp-admin pages, and pages served by the page cache will not carry it because WordPress does not run on those responses."
                checked={debugHeaderEnabled}
                onChange={setDebugHeaderEnabled}
                disabled={disabled}
              />
            </div>

            {/* Advanced collapsible */}
            <div>
              <button
                type="button"
                onClick={() => setShowAdvanced((v) => !v)}
                className="flex w-full items-center justify-between rounded-md border border-border px-3 py-2 text-sm font-medium hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                aria-expanded={showAdvanced}
              >
                Advanced settings
                {showAdvanced ? (
                  <ChevronUp aria-hidden="true" className="size-4 text-muted-foreground" />
                ) : (
                  <ChevronDown aria-hidden="true" className="size-4 text-muted-foreground" />
                )}
              </button>

              {showAdvanced ? (
                <div className="mt-3 space-y-4">
                  <SelectField
                    label="Serializer"
                    value={serializer}
                    options={SERIALIZER_OPTIONS}
                    onChange={setSerializer}
                    disabled={disabled}
                    hint="igbinary is faster but requires the igbinary PHP extension."
                  />
                  <SelectField
                    label="Compression"
                    value={compression}
                    options={COMPRESSION_OPTIONS}
                    onChange={setCompression}
                    disabled={disabled}
                    hint="Reduces memory usage. Requires the corresponding PHP extension."
                  />
                  <SelectField
                    label="Flush strategy"
                    value={flushStrategy}
                    options={FLUSH_STRATEGY_OPTIONS}
                    onChange={setFlushStrategy}
                    disabled={disabled}
                  />
                  <div className="space-y-3 rounded-lg border border-border p-3">
                    <SettingRow
                      label="Async flush (UNLINK)"
                      description="Use Redis UNLINK instead of DEL for non-blocking key deletion. Requires Redis 4.0+."
                      checked={asyncFlush}
                      onChange={setAsyncFlush}
                      disabled={disabled}
                    />
                  </div>
                  <NumberField
                    label="Max TTL"
                    value={maxttl}
                    onCommit={setMaxttl}
                    min={3600}
                    max={2592000}
                    unit="seconds"
                    disabled={disabled}
                    hint="Maximum time-to-live for any cached object. Default: 604800 (7 days)."
                  />
                  <NumberField
                    label="Connect timeout"
                    value={connectTimeout}
                    onCommit={setConnectTimeout}
                    min={100}
                    max={5000}
                    unit="ms"
                    disabled={disabled}
                    hint="Maximum time to wait for a TCP connection. Default: 1000 ms."
                  />
                  <NumberField
                    label="Read timeout"
                    value={readTimeout}
                    onCommit={setReadTimeout}
                    min={100}
                    max={5000}
                    unit="ms"
                    disabled={disabled}
                    hint="Maximum time to wait for a server response. Default: 1000 ms."
                  />
                  <NumberField
                    label="Retry count"
                    value={retryCount}
                    onCommit={setRetryCount}
                    min={0}
                    max={10}
                    disabled={disabled}
                    hint="Connect attempts with jitter backoff. Default: 3."
                  />
                  <NumberField
                    label="Retry interval"
                    value={retryInterval}
                    onCommit={setRetryInterval}
                    min={5}
                    max={5000}
                    unit="ms"
                    disabled={disabled}
                    hint="Base backoff between retries. Default: 25 ms."
                  />
                </div>
              ) : null}
            </div>

            {/* Test result */}
            {testResult ? <TestResultCard result={testResult} /> : null}

            {/* Stale-test warning */}
            {!config.last_test_config_hash && config.enabled ? (
              <p
                role="alert"
                className="rounded-md border border-yellow-200 bg-yellow-50 p-2 text-xs text-yellow-800 dark:border-yellow-900 dark:bg-yellow-950 dark:text-yellow-300"
              >
                Configuration changed since the last successful test. Run the
                test again before enabling.
              </p>
            ) : null}
          </div>
        </DialogBody>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={handleTest}
            disabled={!canOperate || testing || saving}
            aria-label="Test connection"
          >
            {testing ? (
              <Loader2 aria-hidden="true" className="animate-spin" />
            ) : null}
            Test connection
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={saving}
          >
            Cancel
          </Button>
          <Button
            type="button"
            onClick={handleSave}
            disabled={!canOperate || saving}
          >
            {saving ? (
              <Loader2 aria-hidden="true" className="animate-spin" />
            ) : null}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Flush confirm dialog (simple — discloses strategy)
// ---------------------------------------------------------------------------

interface FlushDialogProps {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  shared: boolean;
  isPending: boolean;
}

function FlushDialog({ open, onClose, onConfirm, shared, isPending }: FlushDialogProps) {
  const titleId = useId();
  return (
    <Dialog open={open} onClose={isPending ? () => {} : onClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Flush object cache</DialogTitle>
        </DialogHeader>
        <DialogBody>
          <p className="text-sm text-foreground">
            {shared
              ? "This will flush all keys matching this site's prefix using SCAN and UNLINK. Other sites sharing this Redis instance are not affected."
              : "This will flush the entire dedicated Redis database (FLUSHDB). All cached objects for this site will be removed."}
          </p>
          <p className="mt-2 text-xs text-muted-foreground">
            WordPress will regenerate cached objects on the next request. There
            is no data loss — only cached data is removed.
          </p>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose} disabled={isPending}>
            Cancel
          </Button>
          <Button type="button" onClick={onConfirm} disabled={isPending}>
            {isPending ? (
              <Loader2 aria-hidden="true" className="animate-spin" />
            ) : null}
            Flush cache
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Disable confirm dialog
// ---------------------------------------------------------------------------

interface DisableDialogProps {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  isPending: boolean;
}

function DisableDialog({ open, onClose, onConfirm, isPending }: DisableDialogProps) {
  const titleId = useId();
  return (
    <Dialog open={open} onClose={isPending ? () => {} : onClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Disable object cache</DialogTitle>
        </DialogHeader>
        <DialogBody>
          <p className="text-sm text-foreground">
            This removes the object-cache drop-in and flushes cached objects.
            WordPress will fall back to database queries until the object cache
            is re-enabled.
          </p>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose} disabled={isPending}>
            Keep enabled
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={onConfirm}
            disabled={isPending}
          >
            {isPending ? (
              <Loader2 aria-hidden="true" className="animate-spin" />
            ) : null}
            Disable cache
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Analytics section
// ---------------------------------------------------------------------------

type StatsWindow = 7 | 30 | 90;
const STATS_WINDOWS: ReadonlyArray<{ value: StatsWindow; label: string }> = [
  { value: 7, label: "7d" },
  { value: 30, label: "30d" },
  { value: 90, label: "90d" },
];

interface AnalyticsSectionProps {
  siteId: string;
}

function AnalyticsSection({ siteId }: AnalyticsSectionProps) {
  const [days, setDays] = useState<StatsWindow>(7);
  const { data, isLoading, isError, isFetching } = useObjectCacheStatsHistory(siteId, days);

  return (
    <section
      aria-label="Object cache analytics"
      className="rounded-xl border border-border bg-card text-card-foreground shadow-sm"
    >
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-5 py-3">
        <div>
          <h3 className="text-sm font-semibold text-foreground">Object cache analytics</h3>
          <p className="mt-0.5 text-xs text-muted-foreground">
            Hit ratio, memory, latency, and throughput over the selected window
          </p>
        </div>
        <div className="flex items-center gap-2">
          {isFetching && !isLoading ? (
            <Loader2 aria-hidden="true" className="size-3 animate-spin text-muted-foreground" />
          ) : null}
          <WindowToggle value={days} onChange={setDays} />
        </div>
      </div>

      {isLoading ? (
        <div className="grid grid-cols-1 gap-4 p-5 sm:grid-cols-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-[180px] w-full rounded-lg" />
          ))}
        </div>
      ) : isError ? (
        <div className="px-5 py-8 text-center text-sm text-muted-foreground">
          Could not load analytics history.
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-0 divide-y divide-border sm:grid-cols-2 sm:divide-x sm:divide-y-0">
          <ChartCard title="Hit ratio" icon={<Zap aria-hidden="true" className="size-3.5" />}>
            <ObjectCacheHitRatioChart points={data?.points ?? []} />
          </ChartCard>
          <ChartCard title="Memory used" icon={<MemoryStick aria-hidden="true" className="size-3.5" />}>
            <ObjectCacheMemoryChart points={data?.points ?? []} />
          </ChartCard>
          <ChartCard
            title="Avg latency"
            icon={<Timer aria-hidden="true" className="size-3.5" />}
            className="border-t border-border sm:border-t-0 sm:border-r-0"
          >
            <ObjectCacheLatencyChart points={data?.points ?? []} />
          </ChartCard>
          <ChartCard
            title="Ops / second"
            icon={<Database aria-hidden="true" className="size-3.5" />}
            className="border-t border-border"
          >
            <ObjectCacheOpsChart points={data?.points ?? []} />
          </ChartCard>
        </div>
      )}
    </section>
  );
}

interface ChartCardProps {
  title: string;
  icon?: ReactNode;
  children: ReactNode;
  className?: string;
}

function ChartCard({ title, icon, children, className }: ChartCardProps) {
  return (
    <div className={cn("p-4", className)}>
      <div className="mb-2 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
        {icon}
        {title}
      </div>
      {children}
    </div>
  );
}

interface WindowToggleProps {
  value: StatsWindow;
  onChange: (w: StatsWindow) => void;
}

function WindowToggle({ value, onChange }: WindowToggleProps) {
  return (
    <div role="group" aria-label="Stats window" className="inline-flex rounded-md border border-border">
      {STATS_WINDOWS.map((w) => {
        const active = w.value === value;
        return (
          <button
            key={w.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(w.value)}
            className={cn(
              "px-3 py-1.5 text-sm font-medium transition-colors first:rounded-l-md last:rounded-r-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
              active
                ? "bg-[var(--color-primary)] text-[var(--color-primary-foreground)]"
                : "hover:bg-[var(--color-accent)]",
            )}
          >
            {w.label}
          </button>
        );
      })}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Setup / not-configured empty state
// ---------------------------------------------------------------------------

interface SetupCardProps {
  onConfigure: () => void;
  canOperate: boolean;
}

function SetupCard({ onConfigure, canOperate }: SetupCardProps) {
  return (
    <div className="rounded-xl border border-border bg-card p-8 text-center shadow-sm">
      <Database aria-hidden="true" className="mx-auto mb-3 size-10 text-muted-foreground" />
      <h3 className="text-sm font-semibold text-foreground">Object cache not configured</h3>
      <p className="mt-1 text-xs text-muted-foreground">
        Connect a Redis instance to cache WordPress database queries and
        transients in memory, reducing page generation time.
      </p>
      {canOperate ? (
        <Button
          type="button"
          size="sm"
          className="mt-4"
          onClick={onConfigure}
        >
          Configure object cache
        </Button>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Agent version gate
// ---------------------------------------------------------------------------

function AgentVersionGate() {
  return (
    <div className="rounded-xl border border-border bg-card p-6 shadow-sm">
      <div className="flex items-start gap-3">
        <AlertTriangle aria-hidden="true" className="mt-0.5 size-5 shrink-0 text-yellow-600 dark:text-yellow-400" />
        <div>
          <h3 className="text-sm font-semibold text-foreground">Agent update required</h3>
          <p className="mt-1 text-xs text-muted-foreground">
            Update the agent to {MIN_AGENT_VERSION} or later to use the object
            cache. The current agent version does not include the Redis engine.
          </p>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main panel
// ---------------------------------------------------------------------------

export function ObjectCachePanel({
  siteId,
  agentVersion,
  canOperate,
}: ObjectCachePanelProps) {
  const [configOpen, setConfigOpen] = useState(false);
  const [flushOpen, setFlushOpen] = useState(false);
  const [disableOpen, setDisableOpen] = useState(false);
  const [freshTestResult, setFreshTestResult] = useState<ObjectCacheTestResult | null>(null);

  const { data, isPending, isError, error, refetch } = useObjectCacheConfig(siteId);
  // The stored result of the most recent test survives reloads via the config
  // DTO; a fresh in-session test takes precedence over it.
  const lastTestResult: ObjectCacheTestResult | null =
    freshTestResult ?? data?.last_test_result ?? null;
  const enableMutation = useEnableObjectCache(siteId);
  const disableMutation = useDisableObjectCache(siteId);
  const flushMutation = useFlushObjectCache(siteId);

  // Agent version gate
  if (!isAgentSufficient(agentVersion)) {
    return <AgentVersionGate />;
  }

  if (isPending) {
    return <ObjectCachePanelSkeleton />;
  }

  if (isError || !data) {
    return (
      <PageError
        what="Could not load object cache configuration."
        why={error?.message ?? "Unknown error"}
        onRetry={() => void refetch()}
        retryLabel="Retry"
      />
    );
  }

  const cfg = data;
  const oc_state = (cfg.oc_state || "disabled") as OcState;

  // Not yet configured: no host set and never tested
  const isConfigured = Boolean(
    (cfg.host && cfg.host !== "" && cfg.scheme !== "unix") ||
    (cfg.socket_path && cfg.scheme === "unix") ||
    cfg.last_test_config_hash,
  );

  function handleEnable() {
    if (!cfg.last_test_config_hash) {
      toast.error("Run a connection test first before enabling the object cache.");
      return;
    }
    enableMutation.mutate();
  }

  function handleDisableConfirmed() {
    disableMutation.mutate(undefined, {
      onSuccess: () => setDisableOpen(false),
    });
  }

  function handleFlushConfirmed() {
    flushMutation.mutate(undefined, {
      onSuccess: () => setFlushOpen(false),
    });
  }

  return (
    <>
      <div className="space-y-4">
        {/* Status header */}
        {isConfigured ? (
          <StatusHeader
            oc_state={oc_state}
            oc_latency_ms={cfg.oc_latency_ms ?? 0}
            oc_used_memory_bytes={cfg.oc_used_memory_bytes ?? 0}
            oc_hit_ratio_pct={cfg.oc_hit_ratio_pct}
            oc_last_error_class={cfg.oc_last_error_class}
            enabled={cfg.enabled}
            canOperate={canOperate}
            onEnable={handleEnable}
            onDisable={() => setDisableOpen(true)}
            onFlush={() => setFlushOpen(true)}
            onOpenConfig={() => setConfigOpen(true)}
            isEnabling={enableMutation.isPending}
            isDisabling={disableMutation.isPending}
            isFlushing={flushMutation.isPending}
          />
        ) : (
          <SetupCard onConfigure={() => setConfigOpen(true)} canOperate={canOperate} />
        )}

        {/* Server requirements row */}
        {isConfigured ? (
          <SettingsCard title="Server capabilities">
            <ServerRequirementsRow lastTestResult={lastTestResult} />
          </SettingsCard>
        ) : null}

        {/* Analytics charts — only when enabled with data */}
        {cfg.enabled && cfg.analytics_enabled ? (
          <AnalyticsSection siteId={siteId} />
        ) : null}
      </div>

      {/* Config dialog */}
      <ConfigDialog
        open={configOpen}
        onClose={() => {
          setConfigOpen(false);
          // A saved config change invalidates the stored test; the config
          // refetch carries the authoritative last_test_result, so clear the
          // in-session override rather than keeping a stale fresh result.
          setFreshTestResult(null);
        }}
        siteId={siteId}
        config={cfg}
        canOperate={canOperate}
        onTestResult={setFreshTestResult}
      />

      {/* Flush confirm */}
      <FlushDialog
        open={flushOpen}
        onClose={() => setFlushOpen(false)}
        onConfirm={handleFlushConfirmed}
        shared={cfg.shared}
        isPending={flushMutation.isPending}
      />

      {/* Disable confirm */}
      <DisableDialog
        open={disableOpen}
        onClose={() => setDisableOpen(false)}
        onConfirm={handleDisableConfirmed}
        isPending={disableMutation.isPending}
      />
    </>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function ObjectCachePanelSkeleton() {
  return (
    <div role="status" aria-busy="true" aria-label="Loading object cache" className="space-y-4">
      <span className="sr-only">Loading object cache…</span>
      <Skeleton className="h-[72px] w-full rounded-xl" />
      <Skeleton className="h-32 w-full rounded-xl" />
      <Skeleton className="h-[340px] w-full rounded-xl" />
    </div>
  );
}
