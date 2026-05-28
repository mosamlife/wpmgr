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

const formSchema = z.object({
  // A textarea of recipients; validation happens after splitting (below).
  recipients: z
    .string()
    .refine(
      (raw) => {
        const list = splitRecipients(raw);
        return list.length > 0;
      },
      { message: "Add at least one email recipient." },
    )
    .refine(
      (raw) => splitRecipients(raw).every((e) => z.string().email().safeParse(e).success),
      { message: "One or more entries is not a valid email address." },
    ),
  webhook_url: z
    .union([z.literal(""), z.string().url("Enter a valid URL or leave blank.")])
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
    save.mutate(body, { onError: () => {} });
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
          <p
            role="status"
            className="text-sm text-[var(--color-muted-foreground)]"
          >
            Loading alert settings…
          </p>
        ) : isError ? (
          <div role="alert" className="space-y-2">
            <p className="text-sm text-[var(--color-destructive)]">
              {error.message}
            </p>
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        ) : (
          <form
            onSubmit={(e) => void handleSubmit(onSubmit)(e)}
            noValidate
            className="space-y-4"
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
                className="w-full rounded-md border border-[var(--color-input)] bg-transparent px-3 py-2 text-sm focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
              />
              <p
                id="recipients-help"
                className="text-xs text-[var(--color-muted-foreground)]"
              >
                One per line, or separated by commas.
              </p>
              {errors.recipients ? (
                <p className="text-xs text-[var(--color-destructive)]">
                  {errors.recipients.message}
                </p>
              ) : null}
            </div>

            <div className="space-y-1">
              <Label htmlFor="webhook_url">Webhook URL (optional)</Label>
              <Input
                id="webhook_url"
                type="url"
                {...register("webhook_url")}
                aria-invalid={errors.webhook_url ? "true" : undefined}
                placeholder="https://hooks.example.com/wpmgr"
              />
              {errors.webhook_url ? (
                <p className="text-xs text-[var(--color-destructive)]">
                  {errors.webhook_url.message}
                </p>
              ) : null}
            </div>

            {save.isError ? (
              <p role="alert" className="text-sm text-[var(--color-destructive)]">
                {save.error.message}
              </p>
            ) : null}
            {save.isSuccess && !isDirty ? (
              <p
                role="status"
                className="text-sm text-[var(--color-muted-foreground)]"
              >
                Alert settings saved.
              </p>
            ) : null}

            <div className="flex justify-end">
              <Button type="submit" disabled={save.isPending}>
                {save.isPending ? "Saving…" : "Save alert settings"}
              </Button>
            </div>
          </form>
        )}
      </CardContent>
    </Card>
  );
}
