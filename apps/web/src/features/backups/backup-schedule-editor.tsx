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
import { Checkbox } from "@/components/ui/checkbox";
import {
  useBackupSchedule,
  usePutBackupSchedule,
} from "@/features/backups/use-backups";
import { relativeTime } from "@/lib/utils";
import type { BackupScheduleUpdate } from "@wpmgr/api";

// Backup schedule editor (operator+). GETs the current schedule (or null when
// none is configured) and PUTs changes via react-hook-form + Zod. Cadence
// defaults to daily; retention is a rolling-window day count plus a count of
// monthly archives to keep beyond the window.

const formSchema = z.object({
  enabled: z.boolean(),
  cadence: z.enum(["daily", "weekly", "monthly"]),
  kind: z.enum(["files", "db", "full"]),
  retention_days: z.coerce.number().int().min(1).max(3650),
  monthly_archive_keep: z.coerce.number().int().min(0).max(120),
});

type FormValues = z.input<typeof formSchema>;

const DEFAULTS: FormValues = {
  enabled: true,
  cadence: "daily",
  kind: "full",
  retention_days: 30,
  monthly_archive_keep: 12,
};

export function BackupScheduleEditor({ siteId }: { siteId: string }) {
  const { data: schedule, isPending, isError, error, refetch } =
    useBackupSchedule(siteId);
  const save = usePutBackupSchedule(siteId);

  const {
    register,
    handleSubmit,
    reset,
    watch,
    formState: { errors, isDirty },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: DEFAULTS,
  });

  // Seed the form once the schedule loads (or stays at defaults when none).
  useEffect(() => {
    if (isPending) return;
    reset(
      schedule
        ? {
            enabled: schedule.enabled,
            cadence: schedule.cadence,
            kind: schedule.kind,
            retention_days: schedule.retention_days,
            monthly_archive_keep: schedule.monthly_archive_keep,
          }
        : DEFAULTS,
    );
  }, [schedule, isPending, reset]);

  const enabled = watch("enabled");

  function onSubmit(values: FormValues) {
    const body: BackupScheduleUpdate = {
      enabled: values.enabled,
      cadence: values.cadence,
      kind: values.kind,
      retention_days: Number(values.retention_days),
      monthly_archive_keep: Number(values.monthly_archive_keep),
    };
    save.mutate(body, { onError: () => {} });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Backup schedule</CardTitle>
        <CardDescription>
          Automatic backups and how long to keep them.
          {schedule?.next_run_at
            ? ` Next run ${relativeTime(schedule.next_run_at) ?? "soon"}.`
            : ""}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isPending ? (
          <p
            role="status"
            className="text-sm text-[var(--color-muted-foreground)]"
          >
            Loading schedule…
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
            <label className="flex items-center gap-2 text-sm font-medium">
              <Checkbox {...register("enabled")} />
              Enable scheduled backups
            </label>

            <fieldset
              disabled={!enabled}
              className="grid grid-cols-1 gap-4 sm:grid-cols-2"
            >
              <div className="space-y-1">
                <Label htmlFor="cadence">Cadence</Label>
                <select
                  id="cadence"
                  {...register("cadence")}
                  className="h-9 w-full rounded-md border border-[var(--color-input)] bg-transparent px-3 text-sm focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none disabled:opacity-50"
                >
                  <option value="daily">Daily</option>
                  <option value="weekly">Weekly</option>
                  <option value="monthly">Monthly</option>
                </select>
              </div>

              <div className="space-y-1">
                <Label htmlFor="schedule-kind">What to back up</Label>
                <select
                  id="schedule-kind"
                  {...register("kind")}
                  className="h-9 w-full rounded-md border border-[var(--color-input)] bg-transparent px-3 text-sm focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none disabled:opacity-50"
                >
                  <option value="full">Full (files + database)</option>
                  <option value="files">Files only</option>
                  <option value="db">Database only</option>
                </select>
              </div>

              <div className="space-y-1">
                <Label htmlFor="retention-days">Retention (days)</Label>
                <Input
                  id="retention-days"
                  type="number"
                  min={1}
                  max={3650}
                  {...register("retention_days")}
                  aria-invalid={errors.retention_days ? "true" : undefined}
                />
                {errors.retention_days ? (
                  <p className="text-xs text-[var(--color-destructive)]">
                    {errors.retention_days.message}
                  </p>
                ) : null}
              </div>

              <div className="space-y-1">
                <Label htmlFor="monthly-keep">Monthly archives to keep</Label>
                <Input
                  id="monthly-keep"
                  type="number"
                  min={0}
                  max={120}
                  {...register("monthly_archive_keep")}
                  aria-invalid={errors.monthly_archive_keep ? "true" : undefined}
                />
                {errors.monthly_archive_keep ? (
                  <p className="text-xs text-[var(--color-destructive)]">
                    {errors.monthly_archive_keep.message}
                  </p>
                ) : null}
              </div>
            </fieldset>

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
                Schedule saved.
              </p>
            ) : null}

            <div className="flex justify-end">
              <Button type="submit" disabled={save.isPending}>
                {save.isPending ? "Saving…" : "Save schedule"}
              </Button>
            </div>
          </form>
        )}
      </CardContent>
    </Card>
  );
}
