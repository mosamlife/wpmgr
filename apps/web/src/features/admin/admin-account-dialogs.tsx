import { useId, useState, type ReactNode } from "react";

import { useNow } from "@/lib/use-now";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { PLAN_CATALOG } from "@/features/billing/plan-catalog";
import { planStatusLabel } from "@/features/billing/billing-status";
import type { BillingPlanId, BillingPlanStatus } from "@/features/billing/use-billing";
import {
  useAdminCompAccount,
  useAdminRevokeComp,
  useAdminSetOverrides,
  useAdminExtendGrace,
  useAdminSuspendAccount,
  useAdminRestoreAccount,
  useAdminForceBillingState,
  type AdminAccountDetail,
  type AdminAccountMeter,
} from "./use-admin-accounts";

// M16 Phase C1 — manual controls for a single account. Every mutation here
// requires an operator-supplied reason (superadmin audit trail). Dialogs are
// parent-controlled (open/onClose props), matching the vuln-feed admin page's
// Dialog usage exactly.

// ---------------------------------------------------------------------------
// Shared reason field
// ---------------------------------------------------------------------------

function ReasonField({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>Reason</Label>
      <textarea
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        rows={2}
        placeholder="Why are you making this change? Shown in the account timeline."
        disabled={disabled}
        className="w-full rounded-md border border-[var(--color-input)] bg-transparent p-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50"
      />
    </div>
  );
}

/** True while `open` is transitioning from closed -> open on THIS render, so callers can reset local state without a useEffect. */
function useJustOpened(open: boolean): boolean {
  const [prevOpen, setPrevOpen] = useState(open);
  const justOpened = open && !prevOpen;
  if (open !== prevOpen) setPrevOpen(open);
  return justOpened;
}

// ---------------------------------------------------------------------------
// Comp / Revoke comp
// ---------------------------------------------------------------------------

export function CompAccountDialog({
  open,
  onClose,
  tenantId,
  hasActiveSubscription,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
  hasActiveSubscription: boolean;
}) {
  const titleId = useId();
  const [tier, setTier] = useState<BillingPlanId>("starter");
  const [reason, setReason] = useState("");
  const mutation = useAdminCompAccount(tenantId);

  if (useJustOpened(open)) {
    setTier("starter");
    setReason("");
  }

  const canSubmit = reason.trim().length > 0 && !mutation.isPending;
  const handleClose = () => {
    if (mutation.isPending) return;
    onClose();
  };

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Comp this account</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-4">
          {hasActiveSubscription ? (
            <p
              role="alert"
              className="rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-sm text-warning-subtle-fg"
            >
              This account has an active paid subscription. Comping overrides
              billing until the comp is revoked.
            </p>
          ) : null}

          <div className="space-y-1.5">
            <Label htmlFor="comp-tier">Tier</Label>
            <Select
              id="comp-tier"
              value={tier}
              onChange={(e) => setTier(e.target.value as BillingPlanId)}
              disabled={mutation.isPending}
            >
              {PLAN_CATALOG.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </Select>
          </div>

          <ReasonField value={reason} onChange={setReason} disabled={mutation.isPending} />

          {mutation.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {mutation.error.message}
            </p>
          ) : null}
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" disabled={mutation.isPending} onClick={handleClose}>
            Keep current plan
          </Button>
          <Button
            type="button"
            disabled={!canSubmit}
            onClick={() =>
              mutation.mutate(
                { tier, reason: reason.trim() },
                { onSuccess: onClose },
              )
            }
          >
            {mutation.isPending ? "Comping…" : "Comp account"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function RevokeCompDialog({
  open,
  onClose,
  tenantId,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
}) {
  const titleId = useId();
  const [reason, setReason] = useState("");
  const mutation = useAdminRevokeComp(tenantId);

  if (useJustOpened(open)) setReason("");

  const canSubmit = reason.trim().length > 0 && !mutation.isPending;
  const handleClose = () => {
    if (mutation.isPending) return;
    onClose();
  };

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Revoke comp</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <p className="text-sm text-muted-foreground">
            The account returns to its underlying subscription state (or Free,
            if none exists).
          </p>
          <ReasonField value={reason} onChange={setReason} disabled={mutation.isPending} />
          {mutation.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {mutation.error.message}
            </p>
          ) : null}
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" disabled={mutation.isPending} onClick={handleClose}>
            Cancel
          </Button>
          <Button
            type="button"
            variant="destructive"
            disabled={!canSubmit}
            onClick={() =>
              mutation.mutate({ reason: reason.trim() }, { onSuccess: onClose })
            }
          >
            {mutation.isPending ? "Revoking…" : "Revoke comp"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Overrides editor
// ---------------------------------------------------------------------------

interface OverrideRowConfig {
  field: "sites" | "storage_gb" | "seats";
  label: string;
  meterKey: string;
}

const OVERRIDE_ROWS: readonly OverrideRowConfig[] = [
  { field: "sites", label: "Sites", meterKey: "sites" },
  { field: "storage_gb", label: "Storage (GB)", meterKey: "storage" },
  { field: "seats", label: "Seats", meterKey: "seats" },
];

/** Best-effort lookup of "who last changed this override, and when" from the generic timeline — the pinned contract has no dedicated overrides-history field, so this degrades to nothing shown if the backend's event kind naming doesn't match. */
function findLastOverrideEvent(
  timeline: AdminAccountDetail["timeline"],
  field: string,
) {
  return timeline.find(
    (e) =>
      e.kind.toLowerCase().includes("override") &&
      e.kind.toLowerCase().includes(field),
  ) ?? timeline.find((e) => e.kind.toLowerCase().includes("override"));
}

function meterFor(meters: AdminAccountMeter[], key: string) {
  return meters.find((m) => m.key === key);
}

export function OverridesEditorDialog({
  open,
  onClose,
  tenantId,
  detail,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
  detail: AdminAccountDetail;
}) {
  const titleId = useId();
  const [values, setValues] = useState<Record<string, string>>({});
  const mutation = useAdminSetOverrides(tenantId);
  const [reason, setReason] = useState("");

  if (useJustOpened(open)) {
    setValues({});
    setReason("");
  }

  const anyRowTouched = Object.keys(values).length > 0;
  const canSubmit = anyRowTouched && reason.trim().length > 0 && !mutation.isPending;

  function setRow(field: string, raw: string) {
    setValues((prev) => ({ ...prev, [field]: raw }));
  }

  function clearRow(field: string) {
    // Empty string is the tri-state marker for "explicitly clear this override".
    setValues((prev) => ({ ...prev, [field]: "" }));
  }

  function untouchRow(field: string) {
    setValues((prev) => {
      const next = { ...prev };
      delete next[field];
      return next;
    });
  }

  function handleClose() {
    if (mutation.isPending) return;
    onClose();
  }

  function handleSave() {
    const body: {
      sites?: number | null;
      storage_gb?: number | null;
      seats?: number | null;
      reason: string;
    } = { reason: reason.trim() };

    for (const row of OVERRIDE_ROWS) {
      if (!(row.field in values)) continue;
      const raw = values[row.field];
      body[row.field] = raw === "" ? null : Number(raw);
    }

    mutation.mutate(body, { onSuccess: onClose });
  }

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Edit overrides</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Each row shows the current effective cap. Enter a value to
            override it, or clear a row to remove a previously-set override.
          </p>

          <div className="space-y-3">
            {OVERRIDE_ROWS.map((row) => {
              const meter = meterFor(detail.meters, row.meterKey);
              const touched = row.field in values;
              const cleared = touched && values[row.field] === "";
              const lastEvent = findLastOverrideEvent(detail.timeline, row.field);

              return (
                <div key={row.field} className="rounded-md border border-border p-3">
                  <div className="flex items-center justify-between gap-2">
                    <Label htmlFor={`override-${row.field}`}>{row.label}</Label>
                    <span className="text-xs text-muted-foreground">
                      Effective: {meter ? meter.cap.toLocaleString() : "–"}
                    </span>
                  </div>
                  <div className="mt-1.5 flex items-center gap-2">
                    <Input
                      id={`override-${row.field}`}
                      type="number"
                      inputMode="numeric"
                      min={0}
                      placeholder="No change"
                      value={cleared ? "" : (values[row.field] ?? "")}
                      onChange={(e) => setRow(row.field, e.target.value)}
                      disabled={mutation.isPending || cleared}
                      className="max-w-[10rem]"
                    />
                    {cleared ? (
                      <span className="text-xs font-medium text-destructive">
                        Will be cleared
                      </span>
                    ) : null}
                    {touched ? (
                      <Button
                        type="button"
                        size="sm"
                        variant="ghost"
                        disabled={mutation.isPending}
                        onClick={() => untouchRow(row.field)}
                      >
                        Undo
                      </Button>
                    ) : (
                      <Button
                        type="button"
                        size="sm"
                        variant="ghost"
                        disabled={mutation.isPending}
                        onClick={() => clearRow(row.field)}
                      >
                        Clear override
                      </Button>
                    )}
                  </div>
                  {lastEvent ? (
                    <p className="mt-1.5 text-xs text-muted-foreground">
                      Last set {lastEvent.actor ? `by ${lastEvent.actor} ` : ""}
                      &middot; {new Date(lastEvent.at).toLocaleString()}
                    </p>
                  ) : null}
                </div>
              );
            })}
          </div>

          <ReasonField value={reason} onChange={setReason} disabled={mutation.isPending} />

          {mutation.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {mutation.error.message}
            </p>
          ) : null}
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" disabled={mutation.isPending} onClick={handleClose}>
            Cancel
          </Button>
          <Button type="button" disabled={!canSubmit} onClick={handleSave}>
            {mutation.isPending ? "Saving…" : "Save overrides"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Extend grace
// ---------------------------------------------------------------------------

const MAX_GRACE_DAYS = 90;

export function ExtendGraceDialog({
  open,
  onClose,
  tenantId,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
}) {
  const titleId = useId();
  const [until, setUntil] = useState("");
  const [reason, setReason] = useState("");
  const mutation = useAdminExtendGrace(tenantId);

  if (useJustOpened(open)) {
    setUntil("");
    setReason("");
  }

  // useNow (not Date.now() inline) keeps this render pure — see
  // src/lib/use-now.ts and the react-hooks/purity rule it exists to satisfy.
  const now = useNow(60_000);
  const untilMs = until ? Date.parse(until) : NaN;
  const withinBudget =
    until.length > 0 &&
    !Number.isNaN(untilMs) &&
    untilMs > now &&
    untilMs - now <= MAX_GRACE_DAYS * 24 * 60 * 60 * 1000;

  const canSubmit = withinBudget && reason.trim().length > 0 && !mutation.isPending;
  const handleClose = () => {
    if (mutation.isPending) return;
    onClose();
  };

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Extend grace period</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="grace-until">Grace until</Label>
            <DateTimePicker
              id="grace-until"
              value={until}
              onChange={setUntil}
              min={new Date(now).toISOString()}
              disabled={mutation.isPending}
              aria-invalid={until.length > 0 && !withinBudget ? true : undefined}
            />
            {until.length > 0 && !withinBudget ? (
              <p role="alert" className="text-xs text-destructive">
                Grace period must be in the future and no more than {MAX_GRACE_DAYS} days out.
              </p>
            ) : null}
          </div>
          <ReasonField value={reason} onChange={setReason} disabled={mutation.isPending} />
          {mutation.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {mutation.error.message}
            </p>
          ) : null}
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" disabled={mutation.isPending} onClick={handleClose}>
            Cancel
          </Button>
          <Button
            type="button"
            disabled={!canSubmit}
            onClick={() =>
              mutation.mutate({ until, reason: reason.trim() }, { onSuccess: onClose })
            }
          >
            {mutation.isPending ? "Extending…" : "Extend grace"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Force billing state — "reconcile with provider only"
// ---------------------------------------------------------------------------

// Excludes "comped" — comping/revoking comp are their own dedicated actions
// with their own audit trail; this tool only reconciles provider-driven
// states (what Stripe/webhooks already say), so offering "comped" here would
// create a second, confusing path to the same effect.
const FORCE_STATE_STATUSES: readonly BillingPlanStatus[] = [
  "none",
  "trialing",
  "active",
  "past_due",
  "canceled",
  "paused",
];

export function ForceBillingStateDialog({
  open,
  onClose,
  tenantId,
  currentPlan,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
  currentPlan: string;
}) {
  const titleId = useId();
  const [plan, setPlan] = useState<BillingPlanId>("free");
  const [status, setStatus] = useState<BillingPlanStatus>("active");
  const [reason, setReason] = useState("");
  const mutation = useAdminForceBillingState(tenantId);

  if (useJustOpened(open)) {
    setPlan((PLAN_CATALOG.find((p) => p.id === currentPlan)?.id ?? "free"));
    setStatus("active");
    setReason("");
  }

  const canSubmit = reason.trim().length > 0 && !mutation.isPending;
  const handleClose = () => {
    if (mutation.isPending) return;
    onClose();
  };

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Force billing state</DialogTitle>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <p
            role="alert"
            className="rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-sm text-warning-subtle-fg"
          >
            Reconciles this account's plan and status with the payment
            provider only. It does not grant free service — use Comp for that.
          </p>

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="force-plan">Plan</Label>
              <Select
                id="force-plan"
                value={plan}
                onChange={(e) => setPlan(e.target.value as BillingPlanId)}
                disabled={mutation.isPending}
              >
                {PLAN_CATALOG.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="force-status">Status</Label>
              <Select
                id="force-status"
                value={status}
                onChange={(e) => setStatus(e.target.value as BillingPlanStatus)}
                disabled={mutation.isPending}
              >
                {FORCE_STATE_STATUSES.map((s) => (
                  <option key={s} value={s}>
                    {planStatusLabel(s)}
                  </option>
                ))}
              </Select>
            </div>
          </div>

          <ReasonField value={reason} onChange={setReason} disabled={mutation.isPending} />

          {mutation.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {mutation.error.message}
            </p>
          ) : null}
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" disabled={mutation.isPending} onClick={handleClose}>
            Cancel
          </Button>
          <Button
            type="button"
            disabled={!canSubmit}
            onClick={() =>
              mutation.mutate(
                { plan, plan_status: status, reason: reason.trim() },
                { onSuccess: onClose },
              )
            }
          >
            {mutation.isPending ? "Reconciling…" : "Force state"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Danger zone — Suspend / Restore. Two-step: collect the required reason
// first, then require typing the org slug (DestructiveConfirm), mirroring
// the existing admin user-delete flow.
// ---------------------------------------------------------------------------

interface TwoStepDangerProps {
  open: boolean;
  onClose: () => void;
  orgSlug: string;
  title: string;
  reasonPrompt: string;
  consequencesBody: ReactNode;
  confirmLabel: string;
  mutate: (
    input: { reason: string },
    opts: { onSuccess: () => void },
  ) => void;
  isPending: boolean;
  isError: boolean;
  errorMessage?: string;
}

function TwoStepDangerAction({
  open,
  onClose,
  orgSlug,
  title,
  reasonPrompt,
  consequencesBody,
  confirmLabel,
  mutate,
  isPending,
  isError,
  errorMessage,
}: TwoStepDangerProps) {
  const titleId = useId();
  const [step, setStep] = useState<"reason" | "confirm">("reason");
  const [reason, setReason] = useState("");

  if (useJustOpened(open)) {
    setStep("reason");
    setReason("");
  }

  function handleFullClose() {
    if (isPending) return;
    onClose();
  }

  function handleBackToReason() {
    if (isPending) return;
    setStep("reason");
  }

  return (
    <>
      <Dialog open={open && step === "reason"} onClose={handleFullClose}>
        <DialogContent ariaLabelledBy={titleId}>
          <DialogHeader>
            <DialogTitle id={titleId}>{title}</DialogTitle>
          </DialogHeader>
          <DialogBody className="space-y-4">
            <p className="text-sm text-muted-foreground">{reasonPrompt}</p>
            <ReasonField value={reason} onChange={setReason} />
          </DialogBody>
          <DialogFooter className="pt-2">
            <Button type="button" variant="outline" onClick={handleFullClose}>
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              disabled={reason.trim().length === 0}
              onClick={() => setStep("confirm")}
            >
              Continue
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <DestructiveConfirm
        open={open && step === "confirm"}
        onClose={handleBackToReason}
        onConfirm={() =>
          mutate({ reason: reason.trim() }, { onSuccess: onClose })
        }
        title={title}
        consequencesBody={consequencesBody}
        resourceName={orgSlug}
        confirmLabel={confirmLabel}
        cancelLabel="Back"
        isPending={isPending}
        errorMessage={isError ? (errorMessage ?? "Request failed.") : null}
      />
    </>
  );
}

export function SuspendAccountDialog({
  open,
  onClose,
  tenantId,
  orgSlug,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
  orgSlug: string;
}) {
  const mutation = useAdminSuspendAccount(tenantId);
  return (
    <TwoStepDangerAction
      open={open}
      onClose={onClose}
      orgSlug={orgSlug}
      title="Suspend account"
      reasonPrompt="Suspending immediately blocks every member of this org from operating their sites. Explain why before continuing."
      consequencesBody={
        <p>
          All sites owned by <strong className="font-mono text-xs">{orgSlug}</strong>{" "}
          become inaccessible to their operators until this account is
          restored. This does not delete any data.
        </p>
      }
      confirmLabel="Suspend account"
      mutate={(input, opts) => mutation.mutate(input, opts)}
      isPending={mutation.isPending}
      isError={mutation.isError}
      errorMessage={mutation.error?.message}
    />
  );
}

export function RestoreAccountDialog({
  open,
  onClose,
  tenantId,
  orgSlug,
}: {
  open: boolean;
  onClose: () => void;
  tenantId: string;
  orgSlug: string;
}) {
  const mutation = useAdminRestoreAccount(tenantId);
  return (
    <TwoStepDangerAction
      open={open}
      onClose={onClose}
      orgSlug={orgSlug}
      title="Restore account"
      reasonPrompt="Restoring immediately re-enables every member of this org. Explain why before continuing."
      consequencesBody={
        <p>
          Every member of <strong className="font-mono text-xs">{orgSlug}</strong>{" "}
          regains normal access to their sites.
        </p>
      }
      confirmLabel="Restore account"
      mutate={(input, opts) => mutation.mutate(input, opts)}
      isPending={mutation.isPending}
      isError={mutation.isError}
      errorMessage={mutation.error?.message}
    />
  );
}
