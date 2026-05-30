import { useEffect } from "react";
import { useForm, useWatch, Controller } from "react-hook-form";
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
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { FieldError } from "@/components/forms/field-error";
import { FormSection } from "@/components/forms/form-section";
import {
  useBackupSchedule,
  usePutBackupSchedule,
} from "@/features/backups/use-backups";
import { relativeTime } from "@/lib/utils";
import type { BackupScheduleUpdate } from "@wpmgr/api";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/**
 * Format an absolute date-time string in the site's timezone using
 * Intl.DateTimeFormat. Falls back to UTC when timezone is empty or invalid.
 */
function formatInSiteTz(iso: string, timezone: string): string {
  const tz = timezone.trim() || "UTC";
  try {
    return new Intl.DateTimeFormat("en-GB", {
      timeZone: tz,
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      timeZoneName: "short",
    }).format(new Date(iso));
  } catch {
    return new Intl.DateTimeFormat("en-GB", {
      timeZone: "UTC",
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      timeZoneName: "short",
    }).format(new Date(iso));
  }
}


// ---------------------------------------------------------------------------
// Form types + Zod schema
// ---------------------------------------------------------------------------

// Define FormValues explicitly so react-hook-form's TFieldValues is concrete.
// z.coerce.number() in Zod v4 has input type `unknown`, which would widen
// FormValues to unknown if we used z.infer directly. By declaring the type
// separately and casting the resolver we keep full type-safety.
export type FormValues = {
  enabled: boolean;
  cadence: "hourly" | "every_n_hours" | "daily" | "weekly" | "monthly";
  kind: "files" | "db" | "full";
  retention_days: number;
  monthly_archive_keep: number;
  keep_last: number;
  run_hour: number;
  run_minute: number;
  day_of_week: number | null;
  day_of_month: number | null;
  frequency_hours: number | null;
};

const formSchema = z
  .object({
    enabled: z.boolean(),
    cadence: z.enum(["hourly", "every_n_hours", "daily", "weekly", "monthly"]),
    kind: z.enum(["files", "db", "full"]),
    retention_days: z.coerce.number().int().min(1).max(3650),
    monthly_archive_keep: z.coerce.number().int().min(0).max(120),
    keep_last: z.coerce.number().int().min(0).max(9999),
    run_hour: z.coerce.number().int().min(0).max(23),
    run_minute: z.coerce.number().int().min(0).max(59),
    day_of_week: z.number().int().min(0).max(6).nullable(),
    day_of_month: z.number().int().min(1).max(28).nullable(),
    frequency_hours: z.number().int().min(1).max(24).nullable(),
  })
  .superRefine((val, ctx) => {
    if (val.cadence === "weekly" && val.day_of_week === null) {
      ctx.addIssue({
        code: "custom",
        path: ["day_of_week"],
        message: "Day of week is required for weekly cadence.",
      });
    }
    if (val.cadence === "monthly" && val.day_of_month === null) {
      ctx.addIssue({
        code: "custom",
        path: ["day_of_month"],
        message: "Day of month is required for monthly cadence.",
      });
    }
    if (val.cadence === "every_n_hours" && val.frequency_hours === null) {
      ctx.addIssue({
        code: "custom",
        path: ["frequency_hours"],
        message: "Frequency (hours) is required for every-N-hours cadence.",
      });
    }
  });

const DEFAULTS: FormValues = {
  enabled: true,
  cadence: "daily",
  kind: "full",
  retention_days: 30,
  monthly_archive_keep: 12,
  keep_last: 7,
  run_hour: 2,
  run_minute: 0,
  day_of_week: null,
  day_of_month: null,
  frequency_hours: null,
};

const DAY_NAMES = [
  "Sunday",
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
];

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export function BackupScheduleEditor({ siteId }: { siteId: string }) {
  const { data: schedule, isPending, isError, error, refetch } =
    useBackupSchedule(siteId);
  const save = usePutBackupSchedule(siteId);

  const {
    register,
    handleSubmit,
    reset,
    control,
    formState: { errors, isDirty },
  } = useForm<FormValues>({
    // Cast required: zodResolver infers its generic from the Zod schema's
    // own output type (which uses `unknown` for coerce fields); our FormValues
    // type is the concrete equivalent, so the cast is safe.
    resolver: zodResolver(formSchema) as import("react-hook-form").Resolver<FormValues>,
    defaultValues: DEFAULTS,
    mode: "onBlur",
  });

  // Seed form from the server response once loaded.
  useEffect(() => {
    if (isPending) return;
    if (!schedule) {
      reset(DEFAULTS);
      return;
    }
    reset({
      enabled: schedule.enabled,
      cadence: schedule.cadence,
      kind: schedule.kind,
      retention_days: schedule.retention_days,
      monthly_archive_keep: schedule.monthly_archive_keep,
      keep_last: schedule.keep_last,
      run_hour: schedule.run_hour,
      run_minute: schedule.run_minute,
      day_of_week: schedule.day_of_week ?? null,
      day_of_month: schedule.day_of_month ?? null,
      frequency_hours: schedule.frequency_hours ?? null,
    });
  }, [schedule, isPending, reset]);

  const cadence = useWatch({ control, name: "cadence" });
  const enabled = useWatch({ control, name: "enabled" });

  const showTimeOfDay =
    cadence === "daily" ||
    cadence === "weekly" ||
    cadence === "monthly" ||
    cadence === "every_n_hours";
  const showFrequencyHours = cadence === "every_n_hours";
  const showDayOfWeek = cadence === "weekly";
  const showDayOfMonth = cadence === "monthly";

  function onSubmit(values: FormValues) {
    const body: BackupScheduleUpdate = {
      enabled: values.enabled,
      cadence: values.cadence,
      kind: values.kind,
      retention_days: Number(values.retention_days),
      monthly_archive_keep: Number(values.monthly_archive_keep),
      keep_last: Number(values.keep_last),
      run_hour: Number(values.run_hour),
      run_minute: Number(values.run_minute),
      // Send null explicitly to clear fields when switching cadences.
      day_of_week: values.cadence === "weekly" ? (values.day_of_week ?? 0) : undefined,
      day_of_month: values.cadence === "monthly" ? (values.day_of_month ?? 1) : undefined,
      frequency_hours: values.cadence === "every_n_hours" ? (values.frequency_hours ?? 6) : undefined,
    };
    save.mutate(body, {
      onSuccess: () => {
        reset(values);
      },
      onError: () => {},
    });
  }

  // Run times are displayed in the operator's OWN (browser) timezone so they're
  // readable, while the schedule is anchored server-side to the site's
  // WordPress timezone. siteTz is the resolved site zone ("" or "UTC" until
  // WordPress reports one via diagnostics).
  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const siteTz = (schedule?.timezone ?? "").trim();
  const siteTzKnown = siteTz !== "" && siteTz !== "UTC";
  const anchorLabel = siteTzKnown ? siteTz : "UTC";

  return (
    <Card>
      <CardHeader>
        <CardTitle>Backup schedule</CardTitle>
        <CardDescription>
          Automatic backups and how long to keep them.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isPending ? (
          <p role="status" className="text-sm text-muted-foreground">
            Loading schedule…
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
            {/* Timezone notice — display tz vs anchor tz */}
            <p className="mb-4 rounded-md border border-border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
              Times below are shown in your timezone{" "}
              <span className="font-medium text-foreground">({browserTz})</span>.{" "}
              {siteTzKnown ? (
                <>
                  Backups run on the site's WordPress clock{" "}
                  <span className="font-medium text-foreground">({siteTz})</span>
                  ; the run time you set is interpreted there.
                </>
              ) : (
                <>
                  The site's WordPress timezone hasn't been reported yet, so the
                  run time you set is interpreted as{" "}
                  <span className="font-medium text-foreground">UTC</span> for
                  now.
                </>
              )}
            </p>

            {/* Next-run status strip */}
            {schedule && (
              <div className="mb-6 space-y-2">
                {schedule.next_run_at && (
                  <NextRunLine
                    label="Next run"
                    iso={schedule.next_run_at}
                    timezone={browserTz}
                  />
                )}
                {schedule.last_run_at && (
                  <NextRunLine
                    label="Last run"
                    iso={schedule.last_run_at}
                    timezone={browserTz}
                  />
                )}
                {schedule.next_runs.length > 0 && (
                  <div className="space-y-1">
                    <p className="text-xs font-medium text-muted-foreground">
                      Next 3 runs
                    </p>
                    <ol className="space-y-0.5">
                      {schedule.next_runs.map((iso) => (
                        <li key={iso} className="text-xs">
                          <NextRunLine
                            label=""
                            iso={iso}
                            timezone={browserTz}
                            compact
                          />
                        </li>
                      ))}
                    </ol>
                  </div>
                )}
              </div>
            )}

            <form
              onSubmit={(e) => void handleSubmit(onSubmit)(e)}
              noValidate
              className="space-y-0"
            >
              {/* Enable toggle */}
              <FormSection
                title="Run scheduled backups"
                description="When enabled, backups run on the chosen cadence and follow the retention policy below."
              >
                <div className="flex items-center gap-3">
                  <Controller
                    control={control}
                    name="enabled"
                    render={({ field }) => (
                      <Switch
                        id="schedule-enabled"
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    )}
                  />
                  <Label htmlFor="schedule-enabled" className="cursor-pointer">
                    Enable scheduled backups
                  </Label>
                </div>
              </FormSection>

              {/* Cadence + kind */}
              <FormSection
                title="Cadence"
                description="How often backups are taken and what they include."
              >
                <fieldset
                  disabled={!enabled}
                  className="grid grid-cols-1 gap-4 sm:grid-cols-2"
                >
                  <div className="space-y-1">
                    <Label htmlFor="cadence">Cadence</Label>
                    <Select id="cadence" {...register("cadence")}>
                      <option value="hourly">Hourly</option>
                      <option value="every_n_hours">Every N hours</option>
                      <option value="daily">Daily</option>
                      <option value="weekly">Weekly</option>
                      <option value="monthly">Monthly</option>
                    </Select>
                    <p className="text-xs text-muted-foreground">
                      Daily fits production sites. Weekly suits staging.
                    </p>
                  </div>

                  <div className="space-y-1">
                    <Label htmlFor="schedule-kind">What to back up</Label>
                    <Select id="schedule-kind" {...register("kind")}>
                      <option value="full">Full (files + database)</option>
                      <option value="files">Files only</option>
                      <option value="db">Database only</option>
                    </Select>
                    <p className="text-xs text-muted-foreground">
                      Full is safest. Database-only is fastest.
                    </p>
                  </div>
                </fieldset>

                {/* Frequency hours — every_n_hours only */}
                {showFrequencyHours && (
                  <fieldset disabled={!enabled} className="space-y-1">
                    <Label htmlFor="frequency-hours">
                      Run every (hours)
                    </Label>
                    <Select
                      id="frequency-hours"
                      {...register("frequency_hours")}
                      aria-invalid={errors.frequency_hours ? "true" : undefined}
                    >
                      {[1, 2, 3, 4, 6, 8, 12, 24].map((h) => (
                        <option key={h} value={h}>
                          {h === 1 ? "Every hour" : `Every ${h} hours`}
                        </option>
                      ))}
                    </Select>
                    <FieldError
                      what={errors.frequency_hours?.message}
                      why="Frequency must be between 1 and 24 hours."
                      how="Select a frequency above."
                    />
                  </fieldset>
                )}

                {/* Time of day — daily, weekly, monthly, every_n_hours anchor */}
                {showTimeOfDay && (
                  <fieldset
                    disabled={!enabled}
                    className="grid grid-cols-1 gap-4 sm:grid-cols-2"
                  >
                    <div className="space-y-1">
                      <Label htmlFor="run-hour">
                        {cadence === "every_n_hours"
                          ? "Anchor hour (0-23)"
                          : "Run hour (0-23)"}
                      </Label>
                      <Input
                        id="run-hour"
                        type="number"
                        min={0}
                        max={23}
                        {...register("run_hour")}
                        aria-invalid={errors.run_hour ? "true" : undefined}
                        aria-describedby="run-hour-help"
                      />
                      <p id="run-hour-help" className="text-xs text-muted-foreground">
                        {cadence === "every_n_hours"
                          ? "The sequence of N-hour slots is anchored to this hour."
                          : `Interpreted on the ${anchorLabel} clock; the run shows in your local time above.`}
                      </p>
                      <FieldError
                        what={errors.run_hour?.message}
                        why="Hour must be between 0 and 23."
                        how="Enter a whole number."
                      />
                    </div>

                    <div className="space-y-1">
                      <Label htmlFor="run-minute">Run minute (0-59)</Label>
                      <Input
                        id="run-minute"
                        type="number"
                        min={0}
                        max={59}
                        {...register("run_minute")}
                        aria-invalid={errors.run_minute ? "true" : undefined}
                        aria-describedby="run-minute-help"
                      />
                      <p id="run-minute-help" className="text-xs text-muted-foreground">
                        Minute within the hour. Defaults to 0.
                      </p>
                      <FieldError
                        what={errors.run_minute?.message}
                        why="Minute must be between 0 and 59."
                        how="Enter a whole number."
                      />
                    </div>
                  </fieldset>
                )}

                {/* Day of week — weekly */}
                {showDayOfWeek && (
                  <fieldset disabled={!enabled} className="space-y-1">
                    <Label htmlFor="day-of-week">Day of week</Label>
                    <Select
                      id="day-of-week"
                      {...register("day_of_week")}
                      aria-invalid={errors.day_of_week ? "true" : undefined}
                    >
                      {DAY_NAMES.map((name, i) => (
                        <option key={i} value={i}>
                          {name}
                        </option>
                      ))}
                    </Select>
                    <FieldError
                      what={errors.day_of_week?.message}
                      why="Day of week is required for weekly cadence."
                      how="Select a day above."
                    />
                  </fieldset>
                )}

                {/* Day of month — monthly */}
                {showDayOfMonth && (
                  <fieldset disabled={!enabled} className="space-y-1">
                    <Label htmlFor="day-of-month">Day of month (1–28)</Label>
                    <Input
                      id="day-of-month"
                      type="number"
                      min={1}
                      max={28}
                      {...register("day_of_month")}
                      aria-invalid={errors.day_of_month ? "true" : undefined}
                      aria-describedby="day-of-month-help"
                    />
                    <p
                      id="day-of-month-help"
                      className="text-xs text-muted-foreground"
                    >
                      Capped at 28 to avoid month-end ambiguity.
                    </p>
                    <FieldError
                      what={errors.day_of_month?.message}
                      why="Day of month must be between 1 and 28."
                      how="Enter a whole number above."
                    />
                  </fieldset>
                )}
              </FormSection>

              {/* Retention */}
              <FormSection
                title="Retention"
                description="How long snapshots stay in storage before they are pruned. The stricter of the two limits applies."
              >
                <fieldset
                  disabled={!enabled}
                  className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3"
                >
                  <div className="space-y-1">
                    <Label htmlFor="retention-days">Retention (days)</Label>
                    <Input
                      id="retention-days"
                      type="number"
                      min={1}
                      max={3650}
                      {...register("retention_days")}
                      aria-invalid={errors.retention_days ? "true" : undefined}
                      aria-describedby="retention-days-help"
                    />
                    <p
                      id="retention-days-help"
                      className="text-xs text-muted-foreground"
                    >
                      Rolling window. 30 days is the default.
                    </p>
                    <FieldError
                      what={errors.retention_days?.message}
                      why="Retention must be between 1 and 3650 days."
                      how="Enter a whole number above."
                    />
                  </div>

                  <div className="space-y-1">
                    <Label htmlFor="keep-last">Keep at least (count)</Label>
                    <Input
                      id="keep-last"
                      type="number"
                      min={0}
                      max={9999}
                      {...register("keep_last")}
                      aria-invalid={errors.keep_last ? "true" : undefined}
                      aria-describedby="keep-last-help"
                    />
                    <p
                      id="keep-last-help"
                      className="text-xs text-muted-foreground"
                    >
                      Minimum snapshots retained regardless of age. Default 7.
                    </p>
                    <FieldError
                      what={errors.keep_last?.message}
                      why="Must be a non-negative whole number."
                      how="Enter a whole number above."
                    />
                  </div>

                  <div className="space-y-1">
                    <Label htmlFor="monthly-keep">Monthly archives</Label>
                    <Input
                      id="monthly-keep"
                      type="number"
                      min={0}
                      max={120}
                      {...register("monthly_archive_keep")}
                      aria-invalid={
                        errors.monthly_archive_keep ? "true" : undefined
                      }
                      aria-describedby="monthly-keep-help"
                    />
                    <p
                      id="monthly-keep-help"
                      className="text-xs text-muted-foreground"
                    >
                      Long-term archives beyond the rolling window.
                    </p>
                    <FieldError
                      what={errors.monthly_archive_keep?.message}
                      why="Monthly archives must be between 0 and 120."
                      how="Enter a whole number above."
                    />
                  </div>
                </fieldset>
              </FormSection>

              {/* Always-visible action row pinned inside the card so the Save
                  control is never hidden behind a floating bar. */}
              <div className="mt-6 flex flex-wrap items-center justify-end gap-3 border-t border-border pt-4">
                {save.isError ? (
                  <span role="alert" className="mr-auto text-sm text-destructive">
                    {save.error.message}
                  </span>
                ) : isDirty ? (
                  <span className="mr-auto text-sm text-muted-foreground">
                    You have unsaved changes.
                  </span>
                ) : save.isSuccess ? (
                  <span role="status" className="mr-auto text-sm text-muted-foreground">
                    Schedule saved.
                  </span>
                ) : null}
                {isDirty ? (
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => reset()}
                    disabled={save.isPending}
                  >
                    Discard changes
                  </Button>
                ) : null}
                <Button type="submit" disabled={save.isPending}>
                  {save.isPending ? "Saving…" : "Update schedule"}
                </Button>
              </div>
            </form>
          </>
        )}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// NextRunLine — renders one scheduled time as absolute (site tz) + relative
// ---------------------------------------------------------------------------

interface NextRunLineProps {
  label: string;
  iso: string;
  timezone: string;
  compact?: boolean;
}

function NextRunLine({ label, iso, timezone, compact = false }: NextRunLineProps) {
  const abs = formatInSiteTz(iso, timezone);
  const rel = relativeTime(iso);

  if (compact) {
    return (
      <time
        dateTime={iso}
        title={iso}
        className="text-xs tabular-nums text-muted-foreground"
      >
        {abs}
        {rel ? (
          <span className="ml-1.5 font-normal text-muted-foreground/70">
            ({rel})
          </span>
        ) : null}
      </time>
    );
  }

  return (
    <p className="text-xs text-muted-foreground">
      {label}:{" "}
      <time
        dateTime={iso}
        title={iso}
        className="font-medium text-foreground tabular-nums"
      >
        {abs}
      </time>
      {rel ? (
        <span className="ml-1.5 text-muted-foreground/70">({rel})</span>
      ) : null}
    </p>
  );
}
