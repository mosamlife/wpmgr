import { ShieldAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import { toast } from "@/components/toast";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { usePutAlertConfig } from "./use-uptime";
import { useAppHealthAlertingAvailable } from "./use-app-health-alerting-availability";
import { useAppHealthPromptDismissed } from "./use-app-health-prompt-dismissed";

// GH #291 Phase 3 — the reporter's requested "one-time, prominent upgrade
// prompt" for application-health alerting, built per the design doc section
// 5 rollout plan: collect and display from day one (Phase 2, already
// shipped), but alerting starts OFF on any upgraded install, and this is the
// prompt that tells the operator that and offers to turn it on.
//
// Two layers, matching the AutologinPolicyPanel / StatusChip split in this
// codebase (gate-and-fetch component delegates to a pure view):
//   AppHealthAlertUpgradePrompt      — gates on role + availability +
//                                       dismissal, wires real handlers.
//   AppHealthAlertUpgradePromptView  — presentational; renders given props.
//     Exported so tests can render the copy/interaction directly without
//     needing the four container-level gates to line up first.
//
// Requirements this satisfies (design ask, GH #291):
//   - Appears once, not a nag: gated on `useAppHealthPromptDismissed`
//     (localStorage, same pattern as the onboarding wizard).
//   - Only shown to operators who can act on it: gated on `canOperate`
//     (owner/admin/operator), the same floor as the alert-config editor
//     itself (`alert-config-form.tsx`).
//   - Only shown when the feature exists and there is something to enable:
//     gated on `useAppHealthAlertingAvailable()`, which reads the tenant's
//     real `AlertConfig.app_alerts_enabled` and is only true while it is
//     off. See that module's doc comment for exactly what it reads and the
//     one control-plane surface (per-site settings) still unwired.
//   - Calm and specific: no marketing language, no em/en dashes, and states
//     plainly why alerting defaults off — some sites may have been quietly
//     broken for a while, and turning this on could surface several at once.
//   - Two honest actions: enable (persists via the same alert-config PUT
//     the settings form uses), or dismiss and leave it off.

export function AppHealthAlertUpgradePrompt() {
  const { data: me } = useMe();
  const available = useAppHealthAlertingAvailable();
  const tenantId = me?.active_tenant_id ?? null;
  const { isDismissed, dismiss } = useAppHealthPromptDismissed(tenantId);
  const putAlertConfig = usePutAlertConfig();

  if (!available) return null;
  if (!tenantId) return null;
  if (!canOperate(me)) return null;
  if (isDismissed) return null;

  function handleEnable() {
    // A partial update: every other field on the tenant's alert config
    // (recipients, webhook, downtime/security/vulnerability settings) is
    // omitted, which `PUT /api/v1/alert-config` preserves as-is — see
    // `AlertConfigUpdate.app_alerts_enabled`'s own doc comment.
    putAlertConfig.mutate(
      { app_alerts_enabled: true },
      {
        onSuccess: () => {
          dismiss();
          toast.success("Application health alerts turned on.");
        },
        onError: (error) => {
          toast.error("Couldn't turn on application health alerts.", {
            description: error.message,
          });
        },
      },
    );
  }

  return (
    <AppHealthAlertUpgradePromptView
      onEnable={handleEnable}
      onDismiss={dismiss}
      isEnabling={putAlertConfig.isPending}
    />
  );
}

export interface AppHealthAlertUpgradePromptViewProps {
  onEnable: () => void;
  onDismiss: () => void;
  /** True while the enable action is in flight; disables both buttons. */
  isEnabling?: boolean;
}

export function AppHealthAlertUpgradePromptView({
  onEnable,
  onDismiss,
  isEnabling = false,
}: AppHealthAlertUpgradePromptViewProps) {
  return (
    <div
      role="region"
      aria-labelledby="app-health-alert-prompt-heading"
      className="flex gap-3 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
    >
      <ShieldAlert
        aria-hidden="true"
        className="mt-0.5 size-5 shrink-0 text-[var(--color-primary)]"
      />
      <div className="min-w-0 flex-1 space-y-3">
        <div className="space-y-1">
          <h2
            id="app-health-alert-prompt-heading"
            className="text-sm font-semibold text-[var(--color-foreground)]"
          >
            Application health alerting is available
          </h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            The dashboard already shows when a site's WordPress backend has
            stopped responding, even while a page cache keeps serving
            visitors. Alerting on this is off by default: on an existing
            install, some sites may have been quietly broken for a while,
            and turning this on could surface several of them at once.
          </p>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Turn it on when you are ready, or leave it off. You can change
            this later in Alert settings.
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" onClick={onEnable} disabled={isEnabling}>
            {isEnabling ? "Enabling…" : "Enable app health alerts"}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onDismiss}
            disabled={isEnabling}
          >
            Leave it off
          </Button>
        </div>
      </div>
    </div>
  );
}
