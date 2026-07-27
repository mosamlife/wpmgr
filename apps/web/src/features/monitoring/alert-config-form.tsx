import { useEffect } from "react";
import { useForm, useWatch, Controller } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { AlertTriangle } from "lucide-react";
import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { FieldError } from "@/components/forms/field-error";
import { FormSection } from "@/components/forms/form-section";
import { StickySaveBar } from "@/components/forms/sticky-save-bar";
import {
  useAlertConfig,
  usePutAlertConfig,
} from "@/features/monitoring/use-uptime";
import { useEmailNotifySettings } from "@/features/email/use-email";
import type { AlertConfigUpdate } from "@wpmgr/api";

// Tenant alert-channel editor (operator+). One shared channel, email
// recipients plus an optional webhook, feeds four signals: uptime
// downtime/recovery, application-health alerting (GH #291 Phase 3),
// high-severity activity-log security events, and vulnerability alerting
// (GH #247). GETs the current config (or null when none) and PUTs changes
// via react-hook-form + Zod, with an optimistic cache update in the
// mutation hook.
//
// Sprint 4 (forms): per-section "Save" button removed in favor of a global
// `StickySaveBar`. Validation runs on blur and surfaces through `FieldError`
// in the what/why/how shape from DESIGN.md.
//
// Recipients & webhook is the FIRST section (unchanged from the pre-#247
// layout) precisely so the Downtime / Application health / Security events /
// Vulnerability alerts sections below it can each say "the recipients above"
// and mean it.
//
// Application health is this form's only permanent home for
// `app_alerts_enabled`. The one-time upgrade prompt
// (`app-health-alert-prompt.tsx`) also writes this same field through
// `usePutAlertConfig`, but dismissing that prompt must never be the only way
// to turn the setting on, so it lives here too.

const VULN_SEVERITY_VALUES = ["critical", "high", "medium", "low"] as const;

const VULN_SEVERITY_OPTIONS: ReadonlyArray<{
  value: (typeof VULN_SEVERITY_VALUES)[number];
  label: string;
}> = [
  { value: "critical", label: "Critical only" },
  { value: "high", label: "High and above" },
  { value: "medium", label: "Medium and above" },
  { value: "low", label: "Low and above" },
];

const formSchema = z.object({
  // A textarea of recipients; validation happens after splitting (below).
  recipients: z
    .string()
    .refine(
      (raw) => {
        const list = splitRecipients(raw);
        return list.length > 0;
      },
      { message: "No recipients" },
    )
    .refine(
      (raw) => splitRecipients(raw).every((e) => z.string().email().safeParse(e).success),
      { message: "Invalid email address" },
    ),
  webhook_url: z
    .union([z.literal(""), z.string().url("Invalid URL")])
    .optional(),
  notify_security: z.boolean(),
  app_alerts_enabled: z.boolean(),
  notify_vulns: z.boolean(),
  vuln_min_severity: z.enum(VULN_SEVERITY_VALUES),
  vuln_include_in_digest: z.boolean(),
});

type FormValues = z.infer<typeof formSchema>;

/** Split a comma/whitespace/newline separated recipient list into trimmed emails. */
function splitRecipients(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

export function AlertConfigForm() {
  const { data: config, isPending, isError, error, refetch } = useAlertConfig();
  const save = usePutAlertConfig();

  // Secondary, informational fetch: drives the "instance mailer not
  // configured" warning banner only. Shares its cache with the Email page
  // (`emailKeys.notifySettings()`), so this never issues an extra request
  // once that page has been visited. Defaults `instanceMailerConfigured` to
  // `true` while loading so the banner never flashes a false negative.
  const emailSettingsQuery = useEmailNotifySettings();
  const instanceMailerConfigured =
    emailSettingsQuery.data?.instance_mailer_configured ?? true;

  const {
    register,
    handleSubmit,
    reset,
    control,
    formState: { errors, isDirty },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      recipients: "",
      webhook_url: "",
      notify_security: false,
      app_alerts_enabled: false,
      notify_vulns: false,
      vuln_min_severity: "high",
      vuln_include_in_digest: true,
    },
    mode: "onBlur",
  });

  const notifyVulns = useWatch({ control, name: "notify_vulns" });

  // Seed the form once the config loads (or stays empty when none configured).
  useEffect(() => {
    if (isPending) return;
    reset({
      recipients: config ? config.email_recipients.join("\n") : "",
      webhook_url: config?.webhook_url ?? "",
      notify_security: config?.notify_security ?? false,
      app_alerts_enabled: config?.app_alerts_enabled ?? false,
      notify_vulns: config?.notify_vulns ?? false,
      vuln_min_severity: config?.vuln_min_severity ?? "high",
      vuln_include_in_digest: config?.vuln_include_in_digest ?? true,
    });
  }, [config, isPending, reset]);

  function onSubmit(values: FormValues) {
    const body: AlertConfigUpdate = {
      email_recipients: splitRecipients(values.recipients),
      webhook_url: values.webhook_url?.trim() ? values.webhook_url.trim() : "",
      // Always sent explicitly (never omitted) so a save never silently
      // drops a signal the operator didn't touch this time around.
      notify_security: values.notify_security,
      app_alerts_enabled: values.app_alerts_enabled,
      notify_vulns: values.notify_vulns,
      vuln_min_severity: values.vuln_min_severity,
      vuln_include_in_digest: values.vuln_include_in_digest,
    };
    save.mutate(body, {
      onSuccess: () => {
        // Re-seed so isDirty drops and the sticky bar slides away.
        reset(values);
      },
      onError: () => {},
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Alert channel</CardTitle>
        <CardDescription>
          One shared channel for downtime, application health, security
          events, and vulnerability alerts across every site in this tenant.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isPending ? (
          <p role="status" className="text-sm text-muted-foreground">
            Loading alert settings…
          </p>
        ) : isError ? (
          <div role="alert" className="space-y-2">
            <p className="text-sm text-destructive">{error.message}</p>
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        ) : (
          <>
            {!instanceMailerConfigured ? (
              <div className="mb-6 flex gap-2 rounded-md border border-[var(--color-warning)]/50 bg-[var(--color-warning)]/10 px-3 py-2.5">
                <AlertTriangle
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-warning)]"
                />
                <div className="text-sm">
                  <span className="font-medium">
                    Instance mailer not configured.
                  </span>{" "}
                  Alerts cannot be delivered by email until instance-level SMTP
                  is set up. Webhook delivery is unaffected.{" "}
                  <Link
                    to="/settings/smtp"
                    className="font-medium text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                  >
                    Configure SMTP
                  </Link>
                </div>
              </div>
            ) : null}

            <form
              onSubmit={(e) => void handleSubmit(onSubmit)(e)}
              noValidate
              // Bottom padding clears the sticky save bar so the last input
              // stays visible above the floating chrome.
              className="space-y-0 pb-24"
            >
              <FormSection
                title="Recipients & webhook"
                description="Where every alert in this channel is delivered. Downtime, security events, and vulnerability alerts below all use these recipients and this webhook."
              >
                <div className="space-y-1">
                  <Label htmlFor="recipients">Email recipients</Label>
                  <textarea
                    id="recipients"
                    rows={3}
                    {...register("recipients")}
                    aria-invalid={errors.recipients ? "true" : undefined}
                    aria-describedby="recipients-help"
                    placeholder="ops@example.com, oncall@example.com"
                    className="w-full rounded-md border border-[var(--color-input)] bg-transparent px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                  />
                  <p
                    id="recipients-help"
                    className="text-sm text-muted-foreground"
                  >
                    One per line, or separated by commas.
                  </p>
                  <FieldError
                    what={errors.recipients?.message}
                    why="At least one valid email address is required."
                    how="Edit the list above."
                  />
                </div>

                <div className="space-y-1">
                  <Label htmlFor="webhook_url">Webhook URL (optional)</Label>
                  <Input
                    id="webhook_url"
                    type="url"
                    {...register("webhook_url")}
                    aria-invalid={errors.webhook_url ? "true" : undefined}
                    aria-describedby="webhook-help webhook-guarantee-help"
                    placeholder="https://hooks.example.com/wpmgr"
                  />
                  <p
                    id="webhook-help"
                    className="text-sm text-muted-foreground"
                  >
                    Must use https. Leave blank to disable webhooks.
                  </p>
                  <p
                    id="webhook-guarantee-help"
                    className="text-sm text-muted-foreground"
                  >
                    Webhook delivery is best effort. Email is the guaranteed
                    channel for alerts.
                  </p>
                  <FieldError
                    what={errors.webhook_url?.message}
                    why="It must start with https:// or be blank."
                    how="Edit the URL above."
                  />
                </div>
              </FormSection>

              <FormSection
                title="Downtime"
                description="Email and webhook whenever a monitored site goes down or comes back online. Applies to every site in this tenant."
              >
                <p className="text-sm text-muted-foreground">
                  Uses the recipients and webhook above. There is no separate
                  switch for downtime alerts.
                </p>
              </FormSection>

              <FormSection
                title="Application health"
                description="Alert when a site's own WordPress backend fails, not when the site is merely unreachable. Uses the recipients and webhook above."
              >
                <div className="space-y-1">
                  <div className="flex items-center gap-3">
                    <Controller
                      control={control}
                      name="app_alerts_enabled"
                      render={({ field }) => (
                        <Switch
                          id="app-alerts-enabled"
                          checked={field.value}
                          onCheckedChange={field.onChange}
                        />
                      )}
                    />
                    <Label htmlFor="app-alerts-enabled" className="cursor-pointer">
                      Enable application health alerts
                    </Label>
                  </div>
                  <p className="text-sm text-muted-foreground">
                    This alerts on WordPress itself failing, not on the site
                    being unreachable. Only a genuine WordPress error, such
                    as an HTTP 500 response or a page carrying WordPress's
                    own fatal-error signature, triggers an alert. Uncertain
                    results, such as a cached response or a site in
                    maintenance, are reported as unknown and never alert.
                  </p>
                  <p className="text-sm text-muted-foreground">
                    Off by default on an existing install. Turning this on
                    may surface sites that have been quietly broken for a
                    while.
                  </p>
                </div>
              </FormSection>

              <FormSection
                title="Security events"
                description="Route high-severity activity-log events, such as permission changes and suspicious logins, into this same channel."
              >
                <div className="flex items-center gap-3">
                  <Controller
                    control={control}
                    name="notify_security"
                    render={({ field }) => (
                      <Switch
                        id="notify-security"
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    )}
                  />
                  <Label htmlFor="notify-security" className="cursor-pointer">
                    Email on high-severity security events
                  </Label>
                </div>
              </FormSection>

              <FormSection
                title="Vulnerability alerts"
                description="Get notified when a new vulnerability affecting a plugin, theme, or WordPress core is found on a site in this tenant."
              >
                <div className="space-y-4">
                  <div className="space-y-1">
                    <div className="flex items-center gap-3">
                      <Controller
                        control={control}
                        name="notify_vulns"
                        render={({ field }) => (
                          <Switch
                            id="notify-vulns"
                            checked={field.value}
                            onCheckedChange={field.onChange}
                          />
                        )}
                      />
                      <Label htmlFor="notify-vulns" className="cursor-pointer">
                        Enable vulnerability alerts
                      </Label>
                    </div>
                    <p className="text-sm text-muted-foreground">
                      Email a summary when new vulnerabilities are found on
                      your sites.
                    </p>
                  </div>

                  <div className="space-y-1">
                    <Label htmlFor="vuln-min-severity">Minimum severity</Label>
                    <Controller
                      control={control}
                      name="vuln_min_severity"
                      render={({ field }) => (
                        <Select
                          id="vuln-min-severity"
                          value={field.value}
                          onChange={(e) => field.onChange(e.target.value)}
                          disabled={!notifyVulns}
                        >
                          {VULN_SEVERITY_OPTIONS.map((o) => (
                            <option key={o.value} value={o.value}>
                              {o.label}
                            </option>
                          ))}
                        </Select>
                      )}
                    />
                    <p className="text-sm text-muted-foreground">
                      Findings without a severity score yet are always
                      included.
                    </p>
                  </div>

                  <p className="text-sm text-muted-foreground">
                    Sent to the alert recipients above.
                  </p>

                  <div className="space-y-1">
                    <div className="flex items-center gap-3">
                      <Controller
                        control={control}
                        name="vuln_include_in_digest"
                        render={({ field }) => (
                          <Switch
                            id="vuln-include-in-digest"
                            checked={field.value}
                            onCheckedChange={field.onChange}
                            disabled={!notifyVulns}
                          />
                        )}
                      />
                      <Label
                        htmlFor="vuln-include-in-digest"
                        className="cursor-pointer"
                      >
                        Include open findings in the email digest
                      </Label>
                    </div>
                    <p className="text-sm text-muted-foreground">
                      The digest itself is configured on the Email page.
                    </p>
                  </div>
                </div>
              </FormSection>

              <StickySaveBar
                isDirty={isDirty}
                isPending={save.isPending}
                errorMessage={save.isError ? save.error.message : null}
                onSave={() => handleSubmit(onSubmit)()}
                onDiscard={() => reset()}
                saveLabel="Save changes"
                discardLabel="Discard changes"
              />
            </form>
          </>
        )}
      </CardContent>
    </Card>
  );
}
