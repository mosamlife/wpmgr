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
  useBackupSettingsContents,
  usePutBackupSettingsContents,
  useBackupSettingsNotifications,
  usePutBackupSettingsNotifications,
  type SiteBackupSettingsContents,
  type SiteBackupSettingsNotifications,
} from "@/features/backups/use-backups";
import {
  BackupComponentsField,
  BackupExclusionsField,
} from "@/features/backups/backup-scope-fields";
import type { BackupComponent } from "@/features/backups/backup-scope-constants";
import { relativeTime } from "@/lib/utils";
import type { BackupSchedule, BackupScheduleUpdate } from "@wpmgr/api";

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

function parseEmailList(raw: string): string[] {
  return raw
    .split(/[,\s]+/)
    .map((s) => s.trim().toLowerCase())
    .filter((s) => s.includes("@"));
}

// ---------------------------------------------------------------------------
// Shared card-level save-row component
// ---------------------------------------------------------------------------

interface CardSaveRowProps {
  isDirty: boolean;
  isPending: boolean;
  isSuccess: boolean;
  isError: boolean;
  errorMessage?: string;
  onDiscard: () => void;
  saveLabel?: string;
  successLabel?: string;
}

function CardSaveRow({
  isDirty,
  isPending,
  isSuccess,
  isError,
  errorMessage,
  onDiscard,
  saveLabel = "Save",
  successLabel = "Saved.",
}: CardSaveRowProps) {
  return (
    <div className="mt-6 flex flex-wrap items-center justify-end gap-3 border-t border-border pt-4">
      {isError && errorMessage ? (
        <span role="alert" className="mr-auto text-sm text-destructive">
          {errorMessage}
        </span>
      ) : isDirty ? (
        <span className="mr-auto text-sm text-muted-foreground">
          Unsaved changes.
        </span>
      ) : isSuccess ? (
        <span role="status" className="mr-auto text-sm text-muted-foreground">
          {successLabel}
        </span>
      ) : null}
      {isDirty ? (
        <Button
          type="button"
          variant="outline"
          onClick={onDiscard}
          disabled={isPending}
        >
          Discard
        </Button>
      ) : null}
      <Button type="submit" disabled={isPending}>
        {isPending ? "Saving…" : saveLabel}
      </Button>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Card 1 — Schedule (enable, cadence, retention, incremental)
// ---------------------------------------------------------------------------

export type ScheduleCardValues = {
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
  incremental_enabled: boolean;
};

// Coerce-nullable helper: HTML <select> emits string values; these three
// conditional fields need to be numbers (or null when the field is absent
// because a different cadence is selected). `z.coerce.number()` converts
// the string option values (e.g. "0", "1") to numbers, while `.nullable()`
// allows the null default when the field is not shown.
const coerceNullableInt = z.preprocess(
  (v) => (v === null || v === "" || v === undefined ? null : Number(v)),
  z.number().nullable(),
);

const scheduleCardSchema = z
  .object({
    enabled: z.boolean(),
    cadence: z.enum(["hourly", "every_n_hours", "daily", "weekly", "monthly"]),
    kind: z.enum(["files", "db", "full"]),
    retention_days: z.coerce.number().int().min(1).max(3650),
    monthly_archive_keep: z.coerce.number().int().min(0).max(120),
    keep_last: z.coerce.number().int().min(0).max(9999),
    run_hour: z.coerce.number().int().min(0).max(23),
    run_minute: z.coerce.number().int().min(0).max(59),
    // These three fields are driven by <select> elements which emit string
    // values. coerceNullableInt handles the string→number coercion so "0"
    // (Sunday) is accepted as a valid day_of_week, and null remains valid
    // when the field is not shown for the chosen cadence.
    day_of_week: coerceNullableInt.refine(
      (v) => v === null || (Number.isInteger(v) && v >= 0 && v <= 6),
      { message: "Day of week must be between 0 and 6." },
    ),
    day_of_month: coerceNullableInt.refine(
      (v) => v === null || (Number.isInteger(v) && v >= 1 && v <= 28),
      { message: "Day of month must be between 1 and 28." },
    ),
    frequency_hours: coerceNullableInt.refine(
      (v) => v === null || (Number.isInteger(v) && v >= 1 && v <= 24),
      { message: "Frequency must be between 1 and 24 hours." },
    ),
    incremental_enabled: z.boolean(),
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

const SCHEDULE_DEFAULTS: ScheduleCardValues = {
  enabled: true,
  cadence: "daily",
  kind: "full",
  retention_days: 30,
  monthly_archive_keep: 12,
  keep_last: 7,
  run_hour: 2,
  run_minute: 0,
  // Sensible non-null defaults so <select> elements always reflect the form
  // value on first render. The cadence-specific superRefine only rejects null
  // when the cadence requires the field; with these defaults, selecting a
  // cadence and saving immediately is always valid.
  day_of_week: 0,
  day_of_month: 1,
  frequency_hours: 6,
  incremental_enabled: false,
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

interface ScheduleCardProps {
  siteId: string;
  schedule: BackupSchedule | null;
}

function ScheduleCard({ siteId, schedule }: ScheduleCardProps) {
  const save = usePutBackupSchedule(siteId);

  const {
    register,
    handleSubmit,
    reset,
    control,
    formState: { errors, isDirty },
  } = useForm<ScheduleCardValues>({
    resolver: zodResolver(
      scheduleCardSchema,
    ) as import("react-hook-form").Resolver<ScheduleCardValues>,
    defaultValues: SCHEDULE_DEFAULTS,
    mode: "onBlur",
  });

  useEffect(() => {
    if (!schedule) {
      reset(SCHEDULE_DEFAULTS);
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
      // Fall back to the SCHEDULE_DEFAULTS so <select> elements always have a
      // non-null value to display. The superRefine only rejects null when the
      // field's cadence is active, so a non-null default on a different cadence
      // is harmless — the value is excluded from the PUT body when not relevant.
      day_of_week: schedule.day_of_week ?? SCHEDULE_DEFAULTS.day_of_week,
      day_of_month: schedule.day_of_month ?? SCHEDULE_DEFAULTS.day_of_month,
      frequency_hours: schedule.frequency_hours ?? SCHEDULE_DEFAULTS.frequency_hours,
      incremental_enabled: schedule.incremental_enabled ?? false,
    });
  }, [schedule, reset]);

  const cadence = useWatch({ control, name: "cadence" });
  const showTimeOfDay =
    cadence === "daily" ||
    cadence === "weekly" ||
    cadence === "monthly" ||
    cadence === "every_n_hours";
  const showFrequencyHours = cadence === "every_n_hours";
  const showDayOfWeek = cadence === "weekly";
  const showDayOfMonth = cadence === "monthly";

  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const siteTz = (schedule?.timezone ?? "").trim();
  const siteTzKnown = siteTz !== "" && siteTz !== "UTC";
  const anchorLabel = siteTzKnown ? siteTz : "UTC";

  function onSubmit(values: ScheduleCardValues) {
    const body: BackupScheduleUpdate = {
      enabled: values.enabled,
      cadence: values.cadence,
      kind: values.kind,
      retention_days: Number(values.retention_days),
      monthly_archive_keep: Number(values.monthly_archive_keep),
      keep_last: Number(values.keep_last),
      run_hour: Number(values.run_hour),
      run_minute: Number(values.run_minute),
      day_of_week:
        values.cadence === "weekly" ? (values.day_of_week ?? 0) : undefined,
      day_of_month:
        values.cadence === "monthly" ? (values.day_of_month ?? 1) : undefined,
      frequency_hours:
        values.cadence === "every_n_hours"
          ? (values.frequency_hours ?? 6)
          : undefined,
      incremental_enabled: values.incremental_enabled,
    };

    save.mutate(body, {
      onSuccess: () => reset(values),
      onError: () => {},
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Backup schedule</CardTitle>
        <CardDescription>
          Automatic backups, when they run, and how long to keep them.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {/* Timezone notice */}
        <p className="mb-4 rounded-md border border-border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
          Times below are shown in your timezone{" "}
          <span className="font-medium text-foreground">({browserTz})</span>.{" "}
          {siteTzKnown ? (
            <>
              Backups run on the site&apos;s WordPress clock{" "}
              <span className="font-medium text-foreground">({siteTz})</span>;
              the run time you set is interpreted there.
            </>
          ) : (
            <>
              The site&apos;s WordPress timezone has not been reported yet, so
              the run time you set is interpreted as{" "}
              <span className="font-medium text-foreground">UTC</span> for now.
            </>
          )}
        </p>

        {/* Next-run status strip */}
        {schedule ? (
          <div className="mb-6 space-y-2">
            {schedule.next_run_at ? (
              <NextRunLine
                label="Next run"
                iso={schedule.next_run_at}
                timezone={browserTz}
              />
            ) : null}
            {schedule.last_run_at ? (
              <NextRunLine
                label="Last run"
                iso={schedule.last_run_at}
                timezone={browserTz}
              />
            ) : null}
            {schedule.next_runs.length > 0 ? (
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
            ) : null}
          </div>
        ) : null}

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
            <fieldset className="grid grid-cols-1 gap-4 sm:grid-cols-2">
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

            {showFrequencyHours ? (
              <fieldset className="space-y-1">
                <Label htmlFor="frequency-hours">Run every (hours)</Label>
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
            ) : null}

            {showTimeOfDay ? (
              <fieldset className="grid grid-cols-1 gap-4 sm:grid-cols-2">
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
            ) : null}

            {showDayOfWeek ? (
              <fieldset className="space-y-1">
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
            ) : null}

            {showDayOfMonth ? (
              <fieldset className="space-y-1">
                <Label htmlFor="day-of-month">Day of month (1-28)</Label>
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
            ) : null}
          </FormSection>

          {/* Retention */}
          <FormSection
            title="Retention"
            description="How long snapshots stay in storage before they are pruned. The stricter of the two limits applies."
          >
            <fieldset className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
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

          {/* Incremental backups */}
          <FormSection
            title="Incremental backups"
            description="Beta: store only changed files after the first full backup. Restore reassembles the chain automatically."
          >
            <div className="flex items-center gap-3">
              <Controller
                control={control}
                name="incremental_enabled"
                render={({ field }) => (
                  <Switch
                    id="schedule-incremental"
                    checked={field.value}
                    onCheckedChange={field.onChange}
                  />
                )}
              />
              <Label
                htmlFor="schedule-incremental"
                className="cursor-pointer"
              >
                Incremental backups (beta)
              </Label>
            </div>
          </FormSection>

          <CardSaveRow
            isDirty={isDirty}
            isPending={save.isPending}
            isSuccess={save.isSuccess}
            isError={save.isError}
            errorMessage={save.isError ? save.error.message : undefined}
            onDiscard={() => reset()}
            saveLabel="Save schedule"
            successLabel="Schedule saved."
          />
        </form>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Card 2 — Contents (components + exclusions)
// ---------------------------------------------------------------------------

export type ContentsCardValues = {
  backup_components: BackupComponent[];
  include_core: boolean;
  exclude_paths: string[];
  exclude_extensions: string[];
  exclude_file_size_mb: number;
};

const contentsCardSchema = z.object({
  backup_components: z.array(z.string()),
  include_core: z.boolean(),
  exclude_paths: z.array(z.string()),
  exclude_extensions: z.array(z.string()),
  exclude_file_size_mb: z.coerce.number().int().min(0),
});

const CONTENTS_DEFAULTS: ContentsCardValues = {
  backup_components: [],
  include_core: false,
  exclude_paths: [],
  exclude_extensions: [],
  exclude_file_size_mb: 0,
};

interface ContentsCardProps {
  siteId: string;
  contents: SiteBackupSettingsContents | null;
}

function ContentsCard({ siteId, contents }: ContentsCardProps) {
  const save = usePutBackupSettingsContents(siteId);

  const {
    handleSubmit,
    reset,
    control,
    formState: { isDirty },
  } = useForm<ContentsCardValues>({
    resolver: zodResolver(
      contentsCardSchema,
    ) as import("react-hook-form").Resolver<ContentsCardValues>,
    defaultValues: CONTENTS_DEFAULTS,
    mode: "onBlur",
  });

  useEffect(() => {
    if (!contents) {
      reset(CONTENTS_DEFAULTS);
      return;
    }
    const rawComponents = contents.backup_components ?? [];
    const validComponents = rawComponents.filter(
      (c): c is BackupComponent =>
        c === "plugin" ||
        c === "theme" ||
        c === "upload" ||
        c === "wp-content" ||
        c === "db" ||
        c === "core",
    );
    reset({
      backup_components: validComponents,
      include_core: contents.include_core ?? false,
      exclude_paths: contents.exclude_paths ?? [],
      exclude_extensions: contents.exclude_extensions ?? [],
      exclude_file_size_mb: contents.exclude_file_size_mb ?? 0,
    });
  }, [contents, reset]);

  function onSubmit(values: ContentsCardValues) {
    const backupComponents =
      values.backup_components.length > 0 ? values.backup_components : null;
    const excludeFileSizeMb =
      Number(values.exclude_file_size_mb) > 0
        ? Number(values.exclude_file_size_mb)
        : null;

    save.mutate(
      {
        backup_components: backupComponents,
        include_core: values.include_core,
        exclude_paths:
          values.exclude_paths.length > 0 ? values.exclude_paths : null,
        exclude_extensions:
          values.exclude_extensions.length > 0
            ? values.exclude_extensions
            : null,
        exclude_file_size_mb: excludeFileSizeMb,
      },
      {
        onSuccess: () => reset(values),
        onError: () => {},
      },
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Backup contents</CardTitle>
        <CardDescription>
          Which parts of the site are included and which files to skip.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          onSubmit={(e) => void handleSubmit(onSubmit)(e)}
          noValidate
          className="space-y-0"
        >
          <FormSection
            title="Components"
            description="Restrict which parts of the site are backed up. Leave all unchecked to include everything (recommended for full site recovery)."
          >
            <BackupComponentsField
              control={control}
              componentsName="backup_components"
              includeCoreNameProp="include_core"
            />
          </FormSection>

          <FormSection
            title="Exclusions"
            description="Skip files by path segment, extension, or size. The agent's built-in cache/temp excludes always apply in addition to these."
          >
            <BackupExclusionsField
              control={control}
              excludePathsName="exclude_paths"
              excludeExtensionsName="exclude_extensions"
              excludeFileSizeMbName="exclude_file_size_mb"
            />
          </FormSection>

          <CardSaveRow
            isDirty={isDirty}
            isPending={save.isPending}
            isSuccess={save.isSuccess}
            isError={save.isError}
            errorMessage={save.isError ? save.error.message : undefined}
            onDiscard={() => reset()}
            saveLabel="Save contents"
            successLabel="Contents saved."
          />
        </form>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Card 3 — Notifications
// ---------------------------------------------------------------------------

export type NotificationsCardValues = {
  notify_on_completion: "always" | "on_failure" | "never";
  notify_recipients_raw: string;
};

const notificationsCardSchema = z
  .object({
    notify_on_completion: z.enum(["always", "on_failure", "never"]),
    notify_recipients_raw: z.string(),
  })
  .superRefine((val, ctx) => {
    if (val.notify_on_completion !== "never") {
      const emails = parseEmailList(val.notify_recipients_raw);
      if (emails.length === 0) {
        ctx.addIssue({
          code: "custom",
          path: ["notify_recipients_raw"],
          message:
            "At least one recipient email is required when notifications are enabled.",
        });
      }
    }
  });

const NOTIFICATIONS_DEFAULTS: NotificationsCardValues = {
  notify_on_completion: "never",
  notify_recipients_raw: "",
};

interface NotificationsCardProps {
  siteId: string;
  notifications: SiteBackupSettingsNotifications | null;
}

function NotificationsCard({ siteId, notifications }: NotificationsCardProps) {
  const save = usePutBackupSettingsNotifications(siteId);

  const {
    register,
    handleSubmit,
    reset,
    control,
    formState: { errors, isDirty },
  } = useForm<NotificationsCardValues>({
    resolver: zodResolver(
      notificationsCardSchema,
    ) as import("react-hook-form").Resolver<NotificationsCardValues>,
    defaultValues: NOTIFICATIONS_DEFAULTS,
    mode: "onBlur",
  });

  useEffect(() => {
    if (!notifications) {
      reset(NOTIFICATIONS_DEFAULTS);
      return;
    }
    reset({
      notify_on_completion: notifications.notify_on_completion ?? "never",
      notify_recipients_raw: (notifications.notify_recipients ?? []).join(", "),
    });
  }, [notifications, reset]);

  const notifyOn = useWatch({ control, name: "notify_on_completion" });

  function onSubmit(values: NotificationsCardValues) {
    const recipients = parseEmailList(values.notify_recipients_raw);

    save.mutate(
      {
        notify_on_completion: values.notify_on_completion,
        notify_recipients:
          values.notify_on_completion !== "never" ? recipients : [],
      },
      {
        onSuccess: () => reset(values),
        onError: () => {},
      },
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Notifications</CardTitle>
        <CardDescription>
          Receive an email when a backup completes or fails (manual and scheduled runs).
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          onSubmit={(e) => void handleSubmit(onSubmit)(e)}
          noValidate
          className="space-y-0"
        >
          <FormSection
            title="Email notifications"
            description="Send an email after each backup run (manual or scheduled) based on the outcome."
          >
            <fieldset className="space-y-4">
              <div className="space-y-1">
                <Label htmlFor="notify-on">Notify on</Label>
                <Controller
                  control={control}
                  name="notify_on_completion"
                  render={({ field }) => (
                    <Select
                      id="notify-on"
                      value={field.value}
                      onChange={(e) => field.onChange(e.target.value)}
                    >
                      <option value="never">Never (off)</option>
                      <option value="on_failure">Failures only</option>
                      <option value="always">Every backup</option>
                    </Select>
                  )}
                />
                <p className="text-xs text-muted-foreground">
                  &ldquo;Failures only&rdquo; is the recommended setting for
                  production sites.
                </p>
              </div>

              {notifyOn !== "never" ? (
                <div className="space-y-1">
                  <Label htmlFor="notify-recipients">
                    Recipients{" "}
                    <span className="text-xs font-normal text-muted-foreground">
                      (comma-separated emails)
                    </span>
                  </Label>
                  <Input
                    id="notify-recipients"
                    type="text"
                    {...register("notify_recipients_raw")}
                    placeholder="alice@example.com, bob@example.com"
                    aria-invalid={
                      errors.notify_recipients_raw ? "true" : undefined
                    }
                    aria-describedby="notify-recipients-help"
                  />
                  <p
                    id="notify-recipients-help"
                    className="text-xs text-muted-foreground"
                  >
                    At least one valid email address is required when
                    notifications are enabled.
                  </p>
                  <FieldError
                    what={errors.notify_recipients_raw?.message}
                    why="At least one recipient is required when notifications are enabled."
                    how="Enter one or more email addresses separated by commas."
                  />
                </div>
              ) : null}
            </fieldset>
          </FormSection>

          <CardSaveRow
            isDirty={isDirty}
            isPending={save.isPending}
            isSuccess={save.isSuccess}
            isError={save.isError}
            errorMessage={save.isError ? save.error.message : undefined}
            onDiscard={() => reset()}
            saveLabel="Save notifications"
            successLabel="Notifications saved."
          />
        </form>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Root orchestrator — shared fetch, pass schedule down to each card
// ---------------------------------------------------------------------------

export function BackupScheduleEditor({ siteId }: { siteId: string }) {
  const { data: schedule, isPending, isError, error, refetch } =
    useBackupSchedule(siteId);
  const { data: contents } = useBackupSettingsContents(siteId);
  const { data: notifications } = useBackupSettingsNotifications(siteId);

  if (isPending) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Backup schedule</CardTitle>
        </CardHeader>
        <CardContent>
          <p role="status" className="text-sm text-muted-foreground">
            Loading schedule...
          </p>
        </CardContent>
      </Card>
    );
  }

  if (isError) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Backup schedule</CardTitle>
        </CardHeader>
        <CardContent>
          <div role="alert" className="space-y-2">
            <p className="text-sm text-destructive">{error.message}</p>
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        </CardContent>
      </Card>
    );
  }

  // schedule is BackupSchedule | null — all three cards handle null gracefully
  // by falling back to their own DEFAULTS. contents and notifications are
  // loaded independently from their own endpoints (m50 decouple).
  return (
    <>
      <ScheduleCard siteId={siteId} schedule={schedule} />
      <ContentsCard siteId={siteId} contents={contents ?? null} />
      <NotificationsCard siteId={siteId} notifications={notifications ?? null} />
    </>
  );
}

// ---------------------------------------------------------------------------
// NextRunLine — renders one scheduled time as absolute (site tz) + relative
// Exported so the "Backup schedule runs" upcoming panel can reuse the same
// formatting without duplicating the Intl/relativeTime logic.
// ---------------------------------------------------------------------------

export interface NextRunLineProps {
  label: string;
  iso: string;
  timezone: string;
  compact?: boolean;
}

export function NextRunLine({ label, iso, timezone, compact = false }: NextRunLineProps) {
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
