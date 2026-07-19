import { useId, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  AlertCircle,
  AlertTriangle,
  CheckCircle2,
  ExternalLink,
  Info,
  RefreshCw,
  ShieldOff,
  Trash2,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import {
  useVulnFeedStatus,
  useVulnFeedSaveKey,
  useVulnFeedRemoveKey,
  useVulnFeedSync,
  type VulnFeedStatus,
} from "@/features/admin/use-admin-vuln-feed";
import { relativeTime, cn } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Route — auth gate is enforced by the parent /admin layout route (route.tsx);
// no additional beforeLoad guard is needed here.
// ---------------------------------------------------------------------------

export const Route = createFileRoute("/_authed/admin/vuln-feed")({
  component: VulnFeedAdminPage,
});

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

function VulnFeedAdminPage() {
  const {
    data: status,
    isPending,
    isError,
    error,
    refetch,
    isRefetching,
  } = useVulnFeedStatus();

  if (isPending) {
    return (
      <section aria-labelledby="vuln-feed-heading" className="space-y-6">
        <PageHeader
          title="Vulnerability feed"
          subline="Wordfence Intelligence feed configuration for this instance."
        />
        <FeedStatusSkeleton />
      </section>
    );
  }

  if (isError) {
    return (
      <section aria-labelledby="vuln-feed-heading" className="space-y-6">
        <PageHeader
          title="Vulnerability feed"
          subline="Wordfence Intelligence feed configuration for this instance."
        />
        <PageError
          what="Could not load feed status."
          why={error?.message}
          onRetry={() => void refetch()}
          isRetrying={isRefetching}
        />
      </section>
    );
  }

  return (
    <section aria-labelledby="vuln-feed-heading" className="space-y-6">
      <PageHeader
        title="Vulnerability feed"
        subline="Wordfence Intelligence feed configuration for this instance."
      />

      {/* Status card */}
      <FeedStatusCard status={status} />

      {/* Key management card */}
      <KeyManagementCard status={status} />

      {/* Help / attribution */}
      <HelpCard />
    </section>
  );
}

// ---------------------------------------------------------------------------
// FeedStatusCard
// ---------------------------------------------------------------------------

// Exported (alongside `Route`) so tests can render this card directly rather
// than through `Route.options.component`. Mirrors `SETTINGS_NAV_ITEMS` in
// routes/_authed/settings/route.tsx, a plain named export living beside the
// route registration without affecting the router vite plugin's
// autoCodeSplitting of the `component:` reference.
export function FeedStatusCard({ status }: { status: VulnFeedStatus }) {
  const sync = useVulnFeedSync();

  // Derive the connection state for display.
  const state: "connected" | "error" | "not-configured" = !status.configured
    ? "not-configured"
    : status.feed_ok
      ? "connected"
      : "error";

  const stateLabel = {
    connected: "Connected",
    error: "Error",
    "not-configured": "Not configured",
  }[state];

  const sourceLabel: Record<string, string> = {
    ui: "Key saved in admin console",
    env: "Key from environment variable",
    none: "No key configured",
  };

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium">Feed status</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* State row */}
        <div className="flex items-center gap-3">
          <StatusIcon state={state} />
          <div className="min-w-0 flex-1">
            <p
              className={cn(
                "text-sm font-semibold",
                state === "connected" && "text-green-700 dark:text-green-400",
                state === "error" && "text-destructive",
                state === "not-configured" && "text-muted-foreground",
              )}
            >
              {stateLabel}
            </p>
            {state === "connected" ? (
              <p className="mt-0.5 text-xs text-muted-foreground">
                {status.record_count.toLocaleString()} vulnerabilities
                {status.last_synced
                  ? `, synced ${relativeTime(status.last_synced) ?? "recently"}`
                  : null}
              </p>
            ) : state === "error" && status.last_error ? (
              <p className="mt-0.5 text-xs text-destructive">
                {status.last_error}
              </p>
            ) : null}
            {/* GH #245 degraded-enrichment sub-line: the header stays green
                "Connected" (the scanner IS healthy) but the last sync did not
                complete CVSS enrichment, so severities may be understated. */}
            {state === "connected" && !status.enrichment_available ? (
              <p className="mt-1 flex items-start gap-1 text-xs text-warning-subtle-fg">
                <AlertTriangle aria-hidden="true" className="mt-px size-3 shrink-0" />
                <span>
                  Last sync was scanner-only. CVSS enrichment was unavailable,
                  so severities may be understated.
                  {status.last_enrichment_at
                    ? ` Last enrichment ${relativeTime(status.last_enrichment_at) ?? "recently"}.`
                    : null}
                </span>
              </p>
            ) : null}
          </div>

          {/* Sync now — only available when a key is configured */}
          {status.configured ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={sync.isPending}
              onClick={() => sync.mutate()}
              aria-label="Sync feed now"
            >
              <RefreshCw
                aria-hidden="true"
                className={cn("mr-1.5 size-3.5", sync.isPending && "animate-spin")}
              />
              {sync.isPending ? "Syncing..." : "Sync now"}
            </Button>
          ) : null}
        </div>

        {/* Source label */}
        <div className="flex items-center gap-2 rounded-lg bg-muted/50 px-3 py-2">
          <Info aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
          <p className="text-xs text-muted-foreground">
            {sourceLabel[status.source] ?? "Unknown source"}
          </p>
        </div>
      </CardContent>
    </Card>
  );
}

function StatusIcon({ state }: { state: "connected" | "error" | "not-configured" }) {
  if (state === "connected")
    return (
      <CheckCircle2
        aria-hidden="true"
        className="size-5 shrink-0 text-green-600 dark:text-green-400"
      />
    );
  if (state === "error")
    return (
      <AlertCircle
        aria-hidden="true"
        className="size-5 shrink-0 text-destructive"
      />
    );
  return (
    <ShieldOff
      aria-hidden="true"
      className="size-5 shrink-0 text-muted-foreground"
    />
  );
}

// ---------------------------------------------------------------------------
// KeyManagementCard
// ---------------------------------------------------------------------------

function KeyManagementCard({ status }: { status: VulnFeedStatus }) {
  const keyInputId = useId();
  const keyErrorId = useId();
  const removeTitleId = useId();

  const [keyValue, setKeyValue] = useState("");
  const [validationError, setValidationError] = useState<string | null>(null);
  const [showRemoveDialog, setShowRemoveDialog] = useState(false);

  const saveKey = useVulnFeedSaveKey();
  const removeKey = useVulnFeedRemoveKey();

  function handleSave(e: React.FormEvent) {
    e.preventDefault();
    setValidationError(null);

    const trimmed = keyValue.trim();
    if (!trimmed) {
      setValidationError("Paste your Wordfence Intelligence API key to continue.");
      return;
    }

    saveKey.mutate(trimmed, {
      onSuccess: () => {
        // Key is write-only — clear the field immediately after save so
        // the plaintext value is never visible in the UI or dev tools.
        setKeyValue("");
        setValidationError(null);
      },
      onError: (err) => {
        // Surface 422 validation errors inline (bad key format).
        setValidationError(err.message);
      },
    });
  }

  function handleRemoveConfirm() {
    removeKey.mutate(undefined, {
      onSuccess: () => setShowRemoveDialog(false),
    });
  }

  // Show "remove" action only when the key was saved via the UI (not env).
  const canRemove = status.source === "ui";

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium">API key</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Paste your Wordfence Intelligence API key below. The key is stored
          securely and never displayed again after saving.
        </p>

        <form onSubmit={handleSave} noValidate className="space-y-3">
          <div className="space-y-1.5">
            <label
              htmlFor={keyInputId}
              className="text-xs font-medium text-foreground"
            >
              Wordfence Intelligence API key
            </label>
            <Input
              id={keyInputId}
              type="password"
              autoComplete="off"
              autoCorrect="off"
              spellCheck={false}
              placeholder="Paste API key here..."
              value={keyValue}
              onChange={(e) => {
                setKeyValue(e.target.value);
                if (validationError) setValidationError(null);
              }}
              aria-invalid={validationError !== null ? true : undefined}
              aria-describedby={validationError !== null ? keyErrorId : undefined}
              disabled={saveKey.isPending}
            />
            {validationError ? (
              <p
                id={keyErrorId}
                role="alert"
                className="text-xs text-destructive"
              >
                {validationError}
              </p>
            ) : null}
          </div>

          <div className="flex items-center gap-3">
            <Button
              type="submit"
              size="sm"
              disabled={saveKey.isPending || keyValue.trim().length === 0}
            >
              {saveKey.isPending ? "Saving..." : "Save key"}
            </Button>

            {canRemove ? (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="text-destructive hover:text-destructive"
                onClick={() => setShowRemoveDialog(true)}
                disabled={removeKey.isPending}
              >
                <Trash2 aria-hidden="true" className="mr-1.5 size-3.5" />
                Remove key
              </Button>
            ) : null}
          </div>
        </form>

        {/* Remove confirm dialog */}
        <Dialog
          open={showRemoveDialog}
          onClose={() => setShowRemoveDialog(false)}
        >
          <DialogContent ariaLabelledBy={removeTitleId}>
            <DialogHeader>
              <DialogTitle id={removeTitleId}>Remove vulnerability feed key?</DialogTitle>
            </DialogHeader>
            <DialogBody className="space-y-3">
              <p className="text-sm">
                The saved API key will be permanently removed. Vulnerability
                scanning will stop working unless an environment variable key is
                configured.
              </p>
              <p className="text-sm text-muted-foreground">
                This cannot be undone. You will need to paste the key again to
                re-enable the feed.
              </p>
            </DialogBody>
            <DialogFooter className="pt-2">
              <Button
                type="button"
                variant="outline"
                disabled={removeKey.isPending}
                onClick={() => setShowRemoveDialog(false)}
              >
                Cancel
              </Button>
              <Button
                type="button"
                variant="destructive"
                disabled={removeKey.isPending}
                onClick={handleRemoveConfirm}
              >
                {removeKey.isPending ? "Removing..." : "Remove key"}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// HelpCard
// ---------------------------------------------------------------------------

function HelpCard() {
  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium">About the feed</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm text-muted-foreground">
        <p>
          WPMgr uses the{" "}
          <a
            href="https://www.wordfence.com/products/intelligence/"
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-foreground underline underline-offset-2 hover:no-underline"
          >
            Wordfence Intelligence
            <ExternalLink aria-hidden="true" className="size-3" />
          </a>{" "}
          vulnerability database to detect known security issues across all
          connected sites. A free API key is available for eligible users.
        </p>
        <p>
          Once connected, the feed is shared across all sites on this instance.
          No key is sent to individual sites.
        </p>
        <p>
          Vulnerability data is attributed to Defiant Inc. and the MITRE
          Corporation CVE Program as required by the feed license. This
          attribution appears on vulnerability pages shown to users.
        </p>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Loading skeleton
// ---------------------------------------------------------------------------

function FeedStatusSkeleton() {
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="pb-3">
          <Skeleton className="h-4 w-24" />
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex items-center gap-3">
            <Skeleton className="size-5 rounded-full" />
            <Skeleton className="h-4 w-32" />
          </div>
          <Skeleton className="h-8 w-full rounded-lg" />
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="pb-3">
          <Skeleton className="h-4 w-16" />
        </CardHeader>
        <CardContent className="space-y-3">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-8 w-24" />
        </CardContent>
      </Card>
    </div>
  );
}
