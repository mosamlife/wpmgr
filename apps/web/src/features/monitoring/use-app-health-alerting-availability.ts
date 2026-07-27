import { useAlertConfig } from "./use-uptime";

// GH #291 Phase 3 — application-health ALERTING (not detection).
//
// Phase 2 already ships detection + display: `FleetUptimeStatusItem.app_up`
// / `app_probe_reason` (see `use-fleet-uptime.ts`, `StatusChip.tsx`,
// `uptime-status.ts`). This module is about the thing Phase 2 explicitly did
// NOT ship: turning an `app_up=false` verdict into an email/webhook alert.
//
// As of this build the control plane DOES expose a real per-tenant switch
// for that: `AlertConfig.app_alerts_enabled` (packages/openapi-client/src/
// generated/types.gen.ts), readable through the existing `useAlertConfig()`
// and writable through the existing `usePutAlertConfig()` — both already in
// `use-uptime.ts`. No new endpoint was needed for the tenant-wide toggle
// this hook gates; `PUT /api/v1/alert-config` already accepted a partial
// `AlertConfigUpdate`, and `app_alerts_enabled` is just a new field on it.
// `AlertConfig.enabled` remains the separate master switch for the whole
// alert channel (downtime/security/vulnerability alerts,
// apps/api/internal/uptime/worker.go:523) and is left alone here — a tenant
// that already has downtime alerts on does not silently start receiving
// app-health alerts too.
//
// This hook answers exactly one question: is there something for the
// one-time upgrade prompt (`app-health-alert-prompt.tsx`) to offer? That is
// true only once the tenant's alert config has actually loaded AND
// application-health alerting is currently off for it. While the query is
// still loading, has errored, or alerting is already on, there is nothing
// to prompt, so this returns `false`. Role (`canOperate`) and one-time
// dismissal (`useAppHealthPromptDismissed`) are deliberately NOT folded in
// here — they are gated separately by the prompt component itself, matching
// this codebase's gate-and-fetch component convention (see that file).
//
// KNOWN GAP, stated honestly rather than papered over: the OTHER two Phase
// 3 controls — a per-site application-health probe-path override, and a
// per-site "mute this site's app alerts" switch — also now have a real
// control-plane surface (`AppHealthSettings` /
// `GET,PUT /api/v1/sites/{siteId}/app-health-settings`, already present in
// the generated client at packages/openapi-client/src/generated/
// sdk.gen.ts). But nothing in `apps/web` consumes them yet: they are not
// re-exported from the `@wpmgr/api` facade, there is no data hook, and no
// component renders them. That gap is unrelated to this hook (which only
// gates the tenant-wide prompt above) and is real follow-up work, not
// something silently pretended-done here.
export function useAppHealthAlertingAvailable(): boolean {
  const { data } = useAlertConfig();
  return data != null && !data.app_alerts_enabled;
}
