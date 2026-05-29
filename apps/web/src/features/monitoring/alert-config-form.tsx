import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";

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
import { FieldError } from "@/components/forms/field-error";
import { FormSection } from "@/components/forms/form-section";
import { StickySaveBar } from "@/components/forms/sticky-save-bar";
import {
  useAlertConfig,
  usePutAlertConfig,
} from "@/features/monitoring/use-uptime";
import type { AlertConfigUpdate } from "@wpmgr/api";

// Downtime alert configuration editor (operator+). The tenant gets a comma- or
// newline-separated list of email recipients notified on downtime plus an
// optional webhook URL. GETs the current config (or null when none) and PUTs
// changes via react-hook-form + Zod, with an optimistic cache update in the
// mutation hook.
//
// Sprint 4 (forms): per-section "Save" button removed in favor of a global
// `StickySaveBar`. Validation runs on blur and surfaces through `FieldError`
// in the what/why/how shape from DESIGN.md.

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

  const {
    register,
    handleSubmit,
    reset,
    formState: { errors, isDirty },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: { recipients: "", webhook_url: "" },
    mode: "onBlur",
  });

  // Seed the form once the config loads (or stays empty when none configured).
  useEffect(() => {
    if (isPending) return;
    reset({
      recipients: config ? config.email_recipients.join("\n") : "",
      webhook_url: config?.webhook_url ?? "",
    });
  }, [config, isPending, reset]);

  function onSubmit(values: FormValues) {
    const body: AlertConfigUpdate = {
      email_recipients: splitRecipients(values.recipients),
      webhook_url: values.webhook_url?.trim() ? values.webhook_url.trim() : "",
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
        <CardTitle>Downtime alerts</CardTitle>
        <CardDescription>
          Who to notify when a monitored site goes down. Applies to all sites in
          this tenant.
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
          <form
            onSubmit={(e) => void handleSubmit(onSubmit)(e)}
            noValidate
            // Bottom padding clears the sticky save bar so the last input
            // stays visible above the floating chrome.
            className="space-y-0 pb-24"
          >
            <FormSection
              title="Email recipients"
              description="Operators who should receive downtime emails. Applies tenant-wide."
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
            </FormSection>

            <FormSection
              title="Webhook"
              description="Optional. Receives a JSON payload on every downtime event."
            >
              <div className="space-y-1">
                <Label htmlFor="webhook_url">Webhook URL (optional)</Label>
                <Input
                  id="webhook_url"
                  type="url"
                  {...register("webhook_url")}
                  aria-invalid={errors.webhook_url ? "true" : undefined}
                  aria-describedby="webhook-help"
                  placeholder="https://hooks.example.com/wpmgr"
                />
                <p
                  id="webhook-help"
                  className="text-sm text-muted-foreground"
                >
                  Must use https. Leave blank to disable webhooks.
                </p>
                <FieldError
                  what={errors.webhook_url?.message}
                  why="It must start with https:// or be blank."
                  how="Edit the URL above."
                />
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
        )}
      </CardContent>
    </Card>
  );
}
