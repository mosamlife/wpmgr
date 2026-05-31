import { useCallback, useMemo, useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { AnimatePresence, motion } from "motion/react";
import {
  AlertTriangle,
  Check,
  CircleCheck,
  Copy,
  Plus,
  RotateCw,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { fade, scaleIn } from "@/lib/motion-presets";
import { useNow } from "@/lib/use-now";
import { cn } from "@/lib/utils";
import {
  useCreateSiteFirst,
  useCreateEnrollmentCode,
  useArchiveSite,
} from "@/features/sites/use-site-connection";
import {
  useSiteEvents,
  parseStateChanged,
  type SiteEvent,
} from "@/features/sites/use-site-events";
import type { ConnectedSite } from "@/features/sites/connection-state";

// Phase 5.2 — the live "Add site" modal. Replaces the old code-display modal.
//
// State machine (one component, explicit `step`):
//
//   A "url"      Enter URL (+ optional name / tags) → POST /sites
//                 → { site_id, enrollment_code, expires_at } → step B
//   B "awaiting"  Show install instructions + the enrollment code (copy +
//                 countdown) + a "waiting for the agent" spinner. The SSE
//                 stream drives the transition: a `site.state_changed` whose
//                 `site_id` matches ours and whose `to === "connected"` flips
//                 us to step C. If the code expires first, swap the code panel
//                 to "Code expired" + "Request new code".
//   C "success"   "{host} is connected" + detected WP/PHP/plugin counts + a
//                 next-steps checklist (persisted to localStorage) +
//                 "Go to site" / "Add another".
//
// The single SSE subscription is shared app-wide via useSiteEvents; the modal
// subscribes BEFORE a site exists (handler is a no-op until we have a site_id).
//
// Reconnect entry point: pass `initialSite` to open directly at step B with a
// pre-minted code (see the row/detail "Reconnect" action).

export interface AddSiteDialogProps {
  /**
   * Controlled-open override. When provided, the dialog renders WITHOUT its own
   * "Add site" trigger button (the caller owns open/close) — used for the
   * reconnect flow and tests. When omitted, the component renders the trigger
   * button and owns its own open state.
   */
  open?: boolean;
  onClose?: () => void;
  /**
   * Pre-bind an existing site and open at step B (reconnect). The code/expiry
   * come from a fresh POST /sites/:id/enrollment-codes the caller already made.
   */
  initialSite?: {
    siteId: string;
    url: string;
    enrollmentCode: string;
    expiresAt: string;
  };
}

type Step = "url" | "awaiting" | "success";

interface PendingSite {
  siteId: string;
  url: string;
  enrollmentCode: string;
  expiresAt: string;
}

export function AddSiteDialog({
  open: controlledOpen,
  onClose: controlledOnClose,
  initialSite,
}: AddSiteDialogProps = {}) {
  const isControlled = controlledOpen !== undefined;
  const [internalOpen, setInternalOpen] = useState(false);
  const open = isControlled ? controlledOpen : internalOpen;

  const close = useCallback(() => {
    if (isControlled) controlledOnClose?.();
    else setInternalOpen(false);
  }, [isControlled, controlledOnClose]);

  return (
    <>
      {isControlled ? null : (
        <Button type="button" onClick={() => setInternalOpen(true)}>
          <Plus aria-hidden="true" />
          Add site
        </Button>
      )}
      <Dialog open={open} onClose={close}>
        {open ? (
          <AddSiteFlow
            key={initialSite?.siteId ?? "new"}
            onClose={close}
            initialSite={initialSite}
          />
        ) : null}
      </Dialog>
    </>
  );
}

// The flow lives in a child so it fully resets (via the `key`) every time the
// dialog opens — no stale step/code leaking between sessions.
function AddSiteFlow({
  onClose,
  initialSite,
}: {
  onClose: () => void;
  initialSite?: AddSiteDialogProps["initialSite"];
}) {
  const navigate = useNavigate();
  const [step, setStep] = useState<Step>(initialSite ? "awaiting" : "url");
  const [pending, setPending] = useState<PendingSite | null>(
    initialSite
      ? {
          siteId: initialSite.siteId,
          url: initialSite.url,
          enrollmentCode: initialSite.enrollmentCode,
          expiresAt: initialSite.expiresAt,
        }
      : null,
  );
  const [connectedSite, setConnectedSite] = useState<ConnectedSite | null>(null);

  // The SSE handler must see the latest `pending.siteId`. We close over `pending`
  // directly; useSiteEvents re-registers whenever the handler identity changes.
  const handleEvent = useCallback(
    (ev: SiteEvent) => {
      if (!pending) return;
      if (ev.site_id !== pending.siteId) return;
      const changed = parseStateChanged(ev);
      if (changed && changed.to === "connected") {
        setConnectedSite(changed.site);
        setStep("success");
      }
    },
    [pending],
  );
  useSiteEvents(handleEvent);

  const handleCreated = useCallback((site: PendingSite) => {
    setPending(site);
    setStep("awaiting");
  }, []);

  const reset = useCallback(() => {
    setPending(null);
    setConnectedSite(null);
    setStep("url");
  }, []);

  return (
    <DialogContent ariaLabelledBy="add-site-title">
      <DialogHeader>
        <DialogTitle id="add-site-title">
          {step === "success" ? "Site connected" : "Add site"}
        </DialogTitle>
        {step === "url" ? (
          <DialogDescription>
            Enter your WordPress site URL. We'll generate a one-time enrollment
            code and connect automatically once the agent enrolls.
          </DialogDescription>
        ) : null}
      </DialogHeader>

      {/* `fade` cross-fade between the three steps (≈180ms). The steps share
          the dialog bounds so a directionless opacity fade reads cleanest. */}
      <AnimatePresence mode="wait" initial={false}>
        {step === "url" ? (
          <motion.div
            key="step-url"
            variants={fade}
            initial="initial"
            animate="animate"
            exit="exit"
          >
            <UrlStep onCreated={handleCreated} onCancel={onClose} />
          </motion.div>
        ) : step === "awaiting" && pending ? (
          <motion.div
            key="step-awaiting"
            variants={fade}
            initial="initial"
            animate="animate"
            exit="exit"
          >
            <AwaitingStep
              pending={pending}
              onPendingUpdate={setPending}
              onClose={onClose}
            />
          </motion.div>
        ) : step === "success" && (connectedSite || pending) ? (
          <motion.div
            key="step-success"
            variants={fade}
            initial="initial"
            animate="animate"
            exit="exit"
          >
            <SuccessStep
              site={connectedSite}
              fallbackSiteId={pending?.siteId ?? null}
              fallbackUrl={pending?.url ?? connectedSite?.url ?? ""}
              onGoToSite={(siteId) => {
                onClose();
                void navigate({ to: "/sites/$siteId", params: { siteId } });
              }}
              onAddAnother={reset}
              onClose={onClose}
            />
          </motion.div>
        ) : null}
      </AnimatePresence>
    </DialogContent>
  );
}

// ---------------------------------------------------------------------------
// Step A — Enter URL
// ---------------------------------------------------------------------------

function UrlStep({
  onCreated,
  onCancel,
}: {
  onCreated: (site: PendingSite) => void;
  onCancel: () => void;
}) {
  const create = useCreateSiteFirst();
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const [tags, setTags] = useState("");
  const [urlError, setUrlError] = useState<string | null>(null);

  const submit = useCallback(() => {
    const trimmed = url.trim();
    const validation = validateUrl(trimmed);
    if (validation) {
      setUrlError(validation);
      return;
    }
    setUrlError(null);
    create.mutate(
      {
        url: trimmed,
        ...(name.trim() ? { name: name.trim() } : {}),
        ...(parseTags(tags).length > 0 ? { tags: parseTags(tags) } : {}),
      },
      {
        onSuccess: (result) => {
          onCreated({
            siteId: result.site_id,
            url: trimmed,
            enrollmentCode: result.enrollment_code,
            expiresAt: result.expires_at,
          });
        },
      },
    );
  }, [url, name, tags, create, onCreated]);

  return (
    <>
      <DialogBody>
        <div className="space-y-2">
          <Label htmlFor="add-site-url">Site URL</Label>
          <Input
            id="add-site-url"
            type="url"
            inputMode="url"
            placeholder="https://example.com"
            value={url}
            data-autofocus
            onChange={(e) => {
              setUrl(e.target.value);
              if (urlError) setUrlError(null);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                submit();
              }
            }}
            aria-invalid={urlError ? true : undefined}
            aria-describedby={urlError ? "add-site-url-error" : undefined}
          />
          {urlError ? (
            <p id="add-site-url-error" role="alert" className="text-sm text-destructive">
              {urlError}
            </p>
          ) : null}
        </div>

        <div className="space-y-2">
          <Label htmlFor="add-site-name">Name (optional)</Label>
          <Input
            id="add-site-name"
            placeholder="My WordPress site"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div className="space-y-2">
          <Label htmlFor="add-site-tags">Tags (optional)</Label>
          <Input
            id="add-site-tags"
            placeholder="production, client-a"
            value={tags}
            aria-describedby="add-site-tags-hint"
            onChange={(e) => setTags(e.target.value)}
          />
          <p id="add-site-tags-hint" className="text-xs text-muted-foreground">
            Separate tags with commas or spaces.
          </p>
        </div>

        {create.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {create.error.message}
          </p>
        ) : null}
      </DialogBody>

      <DialogFooter className="pt-2">
        <Button
          type="button"
          variant="outline"
          onClick={onCancel}
          disabled={create.isPending}
        >
          Cancel
        </Button>
        <Button type="button" onClick={submit} disabled={create.isPending}>
          {create.isPending ? "Creating…" : "Continue"}
        </Button>
      </DialogFooter>
    </>
  );
}

function validateUrl(value: string): string | null {
  if (!value) return "Enter a site URL.";
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    return "Enter a valid URL, including https://";
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    return "The URL must start with http:// or https://";
  }
  if (!parsed.hostname) return "Enter a valid URL, including a hostname.";
  return null;
}

function parseTags(input: string): string[] {
  return Array.from(
    new Set(
      input
        .split(/[,\s]+/)
        .map((t) => t.trim())
        .filter((t) => t.length > 0 && t.length <= 64),
    ),
  );
}

// ---------------------------------------------------------------------------
// Step B — Awaiting agent
// ---------------------------------------------------------------------------

function AwaitingStep({
  pending,
  onPendingUpdate,
  onClose,
}: {
  pending: PendingSite;
  onPendingUpdate: (site: PendingSite) => void;
  onClose: () => void;
}) {
  const now = useNow(1000);
  const archive = useArchiveSite();
  const newCode = useCreateEnrollmentCode();

  const expiry = Date.parse(pending.expiresAt);
  const remaining = Number.isNaN(expiry)
    ? 0
    : Math.max(0, Math.round((expiry - now) / 1000));
  const expired = remaining <= 0;

  const cancelEnrollment = useCallback(() => {
    archive.mutate(
      { siteId: pending.siteId, reason: "enrollment cancelled" },
      { onSettled: onClose },
    );
  }, [archive, pending.siteId, onClose]);

  const requestNewCode = useCallback(() => {
    newCode.mutate(
      { siteId: pending.siteId },
      {
        onSuccess: (result) => {
          onPendingUpdate({
            ...pending,
            enrollmentCode: result.enrollment_code,
            expiresAt: result.expires_at,
          });
        },
      },
    );
  }, [newCode, pending, onPendingUpdate]);

  return (
    <>
      <DialogBody>
        <ol className="list-decimal space-y-1 rounded-md border border-border p-3 pl-7 text-sm text-muted-foreground">
          <li>Install the WPMgr Agent plugin on your WordPress site.</li>
          <li>Open the plugin settings and enter your control-plane URL.</li>
          <li>Paste the enrollment code below and click Enroll.</li>
        </ol>

        {expired ? (
          <ExpiredPanel
            onRequest={requestNewCode}
            isPending={newCode.isPending}
            error={newCode.isError ? newCode.error.message : null}
          />
        ) : (
          <CodePanel code={pending.enrollmentCode} remaining={remaining} />
        )}

        {/* Live wait state — announced politely so SR users hear progress. */}
        <div
          role="status"
          aria-live="polite"
          className="flex items-center gap-2.5 rounded-md border border-border bg-muted/40 p-3 text-sm"
        >
          <Spinner />
          <span className="text-foreground">
            Waiting for the agent to enroll…
          </span>
        </div>
      </DialogBody>

      <DialogFooter className="pt-2">
        <Button
          type="button"
          variant="outline"
          onClick={cancelEnrollment}
          disabled={archive.isPending}
        >
          {archive.isPending ? "Cancelling…" : "Cancel enrollment"}
        </Button>
      </DialogFooter>
    </>
  );
}

function CodePanel({ code, remaining }: { code: string; remaining: number }) {
  const [copied, setCopied] = useState(false);
  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      setCopied(false);
    }
  }, [code]);

  const mins = Math.floor(remaining / 60);
  const secs = remaining % 60;

  return (
    // `scaleIn` on the code panel — it reads as a distinct floating artifact.
    <motion.div
      variants={scaleIn}
      initial="initial"
      animate="animate"
      className="space-y-2 rounded-md border border-border p-3"
    >
      <div className="flex items-center gap-2">
        <code
          data-testid="enrollment-code"
          className="flex-1 overflow-x-auto rounded-md border border-border bg-muted/40 px-3 py-2 font-mono text-sm tabular-nums"
        >
          {code}
        </code>
        <Button type="button" variant="outline" size="sm" onClick={() => void copy()}>
          {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <p role="status" aria-live="polite" className="text-xs text-muted-foreground">
        Expires in{" "}
        <span className="font-mono font-medium tabular-nums text-foreground">
          {mins}:{secs.toString().padStart(2, "0")}
        </span>
      </p>
    </motion.div>
  );
}

function ExpiredPanel({
  onRequest,
  isPending,
  error,
}: {
  onRequest: () => void;
  isPending: boolean;
  error: string | null;
}) {
  return (
    <div className="space-y-2 rounded-md border border-warning/40 bg-warning-subtle/40 p-3">
      <p className="flex items-center gap-1.5 text-sm font-medium text-warning-subtle-fg">
        <AlertTriangle aria-hidden="true" className="size-4" />
        Code expired
      </p>
      <p className="text-xs text-muted-foreground">
        Enrollment codes are short-lived for security. Request a fresh one to
        continue.
      </p>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      <Button type="button" variant="outline" size="sm" onClick={onRequest} disabled={isPending}>
        <RotateCw aria-hidden="true" />
        {isPending ? "Requesting…" : "Request new code"}
      </Button>
    </div>
  );
}

/**
 * CSS spinner. Under `prefers-reduced-motion` the `animate-spin` is collapsed
 * to ~0ms by the global reduced-motion rule, so we also render a static dot
 * fallback (the ring still reads as a "waiting" affordance when static).
 */
function Spinner() {
  return (
    <span
      aria-hidden="true"
      className="inline-block size-4 shrink-0 rounded-full border-2 border-muted-foreground/30 border-t-info motion-safe:animate-spin"
    />
  );
}

// ---------------------------------------------------------------------------
// Step C — Success
// ---------------------------------------------------------------------------

const CHECKLIST_KEY = "wpmgr.onboarding.checklist";

interface ChecklistItem {
  id: "backup" | "scan" | "uptime";
  label: string;
}

const CHECKLIST: ChecklistItem[] = [
  { id: "backup", label: "Run the first backup" },
  { id: "scan", label: "Run a security scan" },
  { id: "uptime", label: "Enable uptime monitoring" },
];

function SuccessStep({
  site,
  fallbackSiteId,
  fallbackUrl,
  onGoToSite,
  onAddAnother,
  onClose,
}: {
  site: ConnectedSite | null;
  fallbackSiteId: string | null;
  fallbackUrl: string;
  onGoToSite: (siteId: string) => void;
  onAddAnother: () => void;
  onClose: () => void;
}) {
  const siteId = site?.id ?? fallbackSiteId;
  const host = useMemo(
    () => hostOf(site?.url ?? fallbackUrl),
    [site?.url, fallbackUrl],
  );
  const checklistScopeKey = `${CHECKLIST_KEY}.${siteId ?? "unknown"}`;
  const [checked, setChecked] = useChecklistState(checklistScopeKey);

  const wp = site?.wp_version;
  const php = site?.php_version;
  const pluginCount = site?.components?.plugins?.length;
  const detected: string[] = [];
  if (wp) detected.push(`WordPress ${wp}`);
  if (php) detected.push(`PHP ${php}`);
  if (typeof pluginCount === "number")
    detected.push(`${pluginCount} plugin${pluginCount === 1 ? "" : "s"}`);

  return (
    <>
      <DialogBody>
        <div className="flex items-center gap-2.5">
          {/* statusPulse one-shot on the check via scaleIn entrance. */}
          <motion.span
            variants={scaleIn}
            initial="initial"
            animate="animate"
            className="text-success"
          >
            <CircleCheck aria-hidden="true" className="size-6" />
          </motion.span>
          <p className="text-base font-medium text-foreground">
            <span className="font-mono">{host}</span> is connected
          </p>
        </div>

        {detected.length > 0 ? (
          <p className="text-sm text-muted-foreground">
            Detected {detected.join(" · ")}.
          </p>
        ) : (
          <p className="text-sm text-muted-foreground">
            The agent enrolled successfully. Details will populate shortly.
          </p>
        )}

        <div className="space-y-2 rounded-md border border-border p-3">
          <p className="text-sm font-medium">Next steps</p>
          <ul className="space-y-1.5">
            {CHECKLIST.map((item) => {
              const isChecked = checked.has(item.id);
              return (
                <li key={item.id}>
                  <button
                    type="button"
                    onClick={() => setChecked(item.id, !isChecked)}
                    className="flex w-full items-center gap-2 rounded px-1 py-0.5 text-left text-sm text-foreground transition-colors hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    aria-pressed={isChecked}
                  >
                    <span
                      aria-hidden="true"
                      className={cn(
                        "flex size-4 shrink-0 items-center justify-center rounded border",
                        isChecked
                          ? "border-success bg-success text-success-foreground"
                          : "border-muted-foreground/40",
                      )}
                    >
                      {isChecked ? <Check className="size-3" /> : null}
                    </span>
                    <span className={cn(isChecked && "text-muted-foreground line-through")}>
                      {item.label}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
        </div>
      </DialogBody>

      <DialogFooter className="pt-2">
        <Button type="button" variant="outline" onClick={onAddAnother}>
          Add another
        </Button>
        {siteId ? (
          <Button type="button" onClick={() => onGoToSite(siteId)}>
            Go to site
          </Button>
        ) : (
          <Button type="button" onClick={onClose}>
            Done
          </Button>
        )}
      </DialogFooter>
    </>
  );
}

/** Per-site checklist persisted to localStorage. */
function useChecklistState(
  storageKey: string,
): [Set<ChecklistItem["id"]>, (id: ChecklistItem["id"], value: boolean) => void] {
  const [checked, setCheckedState] = useState<Set<ChecklistItem["id"]>>(() => {
    if (typeof localStorage === "undefined") return new Set();
    try {
      const raw = localStorage.getItem(storageKey);
      if (!raw) return new Set();
      const parsed = JSON.parse(raw) as unknown;
      if (!Array.isArray(parsed)) return new Set();
      const valid = parsed.filter(
        (x): x is ChecklistItem["id"] =>
          x === "backup" || x === "scan" || x === "uptime",
      );
      return new Set(valid);
    } catch {
      return new Set();
    }
  });

  const set = useCallback(
    (id: ChecklistItem["id"], value: boolean) => {
      setCheckedState((prev) => {
        const next = new Set(prev);
        if (value) next.add(id);
        else next.delete(id);
        try {
          localStorage.setItem(storageKey, JSON.stringify(Array.from(next)));
        } catch {
          // Storage full / unavailable — keep the in-memory state.
        }
        return next;
      });
    },
    [storageKey],
  );

  return [checked, set];
}

function hostOf(url: string): string {
  try {
    return new URL(url).hostname || url;
  } catch {
    return url.replace(/^https?:\/\//i, "").replace(/\/$/, "");
  }
}
