// /clients/$clientId/reports — report schedule configuration + generated report list.
//
// Two-card layout:
//   1. Schedule card — enable/disable, cadence, recipients, section toggles.
//   2. Reports table — past generated reports with download links + delete.
//
// Follows the same idioms as backup-schedule-editor.tsx and the email feature
// tab: react-hook-form + zod, Controller for Switch. The AlertDialog uses
// controlled open state (no AlertDialogTrigger — the custom alert-dialog.tsx
// component does not export a Trigger sub-component).
//
// Object storage is optional — when html_url/pdf_url are absent on a completed
// report the row still shows the period and status without download links.

import { useId, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useForm, useWatch, Controller } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Download, Trash2, RefreshCw, FileText } from "lucide-react";

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
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { FieldError } from "@/components/forms/field-error";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

import {
  useReportSchedule,
  useUpdateReportSchedule,
  useReports,
  useGenerateReport,
  useDeleteReport,
} from "@/features/clients/use-reports";
import type { ClientReportSchedule, ClientReport } from "@wpmgr/api";
import { relativeTime } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Route
// ---------------------------------------------------------------------------

export const Route = createFileRoute("/_authed/clients/$clientId/reports")({
  component: ClientReportsTab,
});

// ---------------------------------------------------------------------------
// Zod schema for the schedule form
//
// `recipients` is kept as `string` in the form (raw textarea value) and parsed
// into `string[]` only in the submit handler — this avoids the zod .transform()
// mismatch between the form's controlled string value and the output type.
// ---------------------------------------------------------------------------

const scheduleSchema = z.object({
  enabled: z.boolean(),
  cadence: z.enum(["weekly", "monthly"]),
  send_day: z.coerce.number().int().min(0).max(31),
  send_hour: z.coerce.number().int().min(0).max(23),
  recipients: z.string(),
  overview: z.boolean(),
  uptime: z.boolean(),
  backups: z.boolean(),
  updates: z.boolean(),
  performance: z.boolean(),
  email: z.boolean(),
  intro_text: z.string().max(2000),
  closing_text: z.string().max(2000),
  powered_by_removed: z.boolean(),
});

type ScheduleValues = z.infer<typeof scheduleSchema>;

function parseRecipients(raw: string): string[] {
  return raw
    .split(/[,\n]+/)
    .map((s) => s.trim().toLowerCase())
    .filter((s) => s.includes("@"));
}

function scheduleToValues(s: ClientReportSchedule): ScheduleValues {
  return {
    enabled: s.enabled,
    cadence: s.cadence,
    send_day: s.send_day,
    send_hour: s.send_hour,
    recipients: s.recipients.join(", "),
    // A flag the response leaves out reads as on: every section is on by
    // default on the server (DefaultSectionFlags in
    // apps/api/internal/report/reportdata/reportdata.go).
    overview: s.sections.overview ?? true,
    uptime: s.sections.uptime ?? true,
    backups: s.sections.backups ?? true,
    updates: s.sections.updates ?? true,
    performance: s.sections.performance ?? true,
    email: s.sections.email ?? true,
    intro_text: s.intro_text ?? "",
    closing_text: s.closing_text ?? "",
    powered_by_removed: s.powered_by_removed ?? false,
  };
}

// ---------------------------------------------------------------------------
// Page root
// ---------------------------------------------------------------------------

function ClientReportsTab() {
  const params = Route.useParams();
  const clientId = params.clientId;

  const {
    data: schedule,
    isPending: schedulePending,
    isError: scheduleError,
    error: scheduleErr,
    refetch: scheduleRefetch,
    isFetching: scheduleFetching,
  } = useReportSchedule(clientId);

  const {
    data: reportList,
    isPending: reportsPending,
    isError: reportsError,
    error: reportsErr,
    refetch: reportsRefetch,
    isFetching: reportsFetching,
  } = useReports(clientId);

  if (schedulePending || reportsPending) {
    return <ReportsTabSkeleton />;
  }

  if (scheduleError) {
    return (
      <PageError
        what="Could not load report schedule."
        why={scheduleErr.message}
        onRetry={() => void scheduleRefetch()}
        retryLabel="Reload"
        isRetrying={scheduleFetching}
      />
    );
  }

  if (reportsError) {
    return (
      <PageError
        what="Could not load reports."
        why={reportsErr.message}
        onRetry={() => void reportsRefetch()}
        retryLabel="Reload"
        isRetrying={reportsFetching}
      />
    );
  }

  return (
    <div className="space-y-6">
      <ScheduleCard clientId={clientId} schedule={schedule} />
      <ReportsCard
        clientId={clientId}
        reports={reportList?.items ?? []}
        isRefetching={reportsFetching}
        onRefetch={() => void reportsRefetch()}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Schedule card
// ---------------------------------------------------------------------------

interface ScheduleCardProps {
  clientId: string;
  schedule: ClientReportSchedule;
}

function ScheduleCard({ clientId, schedule }: ScheduleCardProps) {
  const uid = useId();
  const update = useUpdateReportSchedule();

  const {
    register,
    handleSubmit,
    control,
    formState: { errors, isDirty },
    reset,
  } = useForm<ScheduleValues>({
    resolver: zodResolver(scheduleSchema) as never,
    defaultValues: scheduleToValues(schedule),
  });

  const cadence = useWatch({ control, name: "cadence" });
  const enabled = useWatch({ control, name: "enabled" });

  const onSubmit = handleSubmit(async (values) => {
    const saved = await update.mutateAsync({
      clientId,
      body: {
        enabled: values.enabled,
        cadence: values.cadence,
        send_day: values.send_day,
        send_hour: values.send_hour,
        recipients: parseRecipients(values.recipients),
        sections: {
          overview: values.overview,
          uptime: values.uptime,
          backups: values.backups,
          updates: values.updates,
          performance: values.performance,
          email: values.email,
        },
        intro_text: values.intro_text,
        closing_text: values.closing_text,
        powered_by_removed: values.powered_by_removed,
      },
    });
    reset(scheduleToValues(saved));
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Automated Reports</CardTitle>
        <CardDescription>
          Configure when and how reports are delivered to your client.
          {!schedule.instance_mailer_configured && (
            <span className="ml-1 text-[var(--color-destructive)]">
              Instance mailer is not configured — recipients will not receive
              emails until SMTP is set up.
            </span>
          )}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          id={`${uid}-schedule-form`}
          onSubmit={(e) => void onSubmit(e)}
          noValidate
          className="space-y-6"
        >
          {/* Enable toggle --------------------------------------------------- */}
          <div className="flex items-center justify-between gap-4">
            <div>
              <Label htmlFor={`${uid}-enabled`} className="text-sm font-medium">
                Enable scheduled reports
              </Label>
              <p className="text-sm text-[var(--color-muted-foreground)]">
                Reports are generated and emailed to recipients on the chosen
                schedule.
              </p>
            </div>
            <Controller
              control={control}
              name="enabled"
              render={({ field }) => (
                <Switch
                  id={`${uid}-enabled`}
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  aria-label="Enable scheduled reports"
                />
              )}
            />
          </div>

          {/* Cadence + timing ------------------------------------------------- */}
          <div className="grid gap-4 sm:grid-cols-3">
            <div className="space-y-1.5">
              <Label htmlFor={`${uid}-cadence`}>Cadence</Label>
              <Select
                id={`${uid}-cadence`}
                disabled={!enabled}
                {...register("cadence")}
              >
                <option value="monthly">Monthly</option>
                <option value="weekly">Weekly</option>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor={`${uid}-send-day`}>
                {cadence === "weekly" ? "Day of week" : "Day of month"}
              </Label>
              {cadence === "weekly" ? (
                <Select
                  id={`${uid}-send-day`}
                  disabled={!enabled}
                  {...register("send_day")}
                >
                  <option value={0}>Sunday</option>
                  <option value={1}>Monday</option>
                  <option value={2}>Tuesday</option>
                  <option value={3}>Wednesday</option>
                  <option value={4}>Thursday</option>
                  <option value={5}>Friday</option>
                  <option value={6}>Saturday</option>
                </Select>
              ) : (
                <Input
                  id={`${uid}-send-day`}
                  type="number"
                  min={1}
                  max={28}
                  disabled={!enabled}
                  aria-invalid={!!errors.send_day}
                  {...register("send_day")}
                />
              )}
              <FieldError what={errors.send_day?.message} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor={`${uid}-send-hour`}>
                Hour (0–23, client timezone)
              </Label>
              <Input
                id={`${uid}-send-hour`}
                type="number"
                min={0}
                max={23}
                disabled={!enabled}
                aria-invalid={!!errors.send_hour}
                {...register("send_hour")}
              />
              <FieldError what={errors.send_hour?.message} />
              {schedule.timezone ? (
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  Timezone: {schedule.timezone}
                </p>
              ) : null}
            </div>
          </div>

          {/* Recipients ------------------------------------------------------- */}
          <div className="space-y-1.5">
            <Label htmlFor={`${uid}-recipients`}>Recipients</Label>
            <textarea
              id={`${uid}-recipients`}
              rows={2}
              disabled={!enabled}
              placeholder="client@example.com, manager@example.com"
              aria-invalid={!!errors.recipients}
              className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-2 text-sm placeholder:text-[var(--color-muted-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
              {...register("recipients")}
            />
            <p className="text-xs text-[var(--color-muted-foreground)]">
              Comma or newline separated email addresses (max 20).
            </p>
          </div>

          {/* Section toggles -------------------------------------------------- */}
          <div className="space-y-3">
            <Label className="text-sm font-medium">Report sections</Label>
            <div className="grid gap-3 sm:grid-cols-2">
              {(
                [
                  { name: "overview", label: "Overview" },
                  { name: "uptime", label: "Uptime" },
                  { name: "backups", label: "Backups" },
                  { name: "updates", label: "Updates" },
                  { name: "performance", label: "Performance" },
                  { name: "email", label: "Email" },
                ] as const
              ).map(({ name, label }) => (
                <div key={name} className="flex items-center gap-3">
                  <Controller
                    control={control}
                    name={name}
                    render={({ field }) => (
                      <Switch
                        id={`${uid}-section-${name}`}
                        checked={field.value}
                        onCheckedChange={field.onChange}
                        aria-label={`Include ${label} section`}
                      />
                    )}
                  />
                  <Label
                    htmlFor={`${uid}-section-${name}`}
                    className="cursor-pointer text-sm font-normal"
                  >
                    {label}
                  </Label>
                </div>
              ))}
            </div>
          </div>

          {/* Intro / closing text -------------------------------------------- */}
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor={`${uid}-intro`}>Intro text</Label>
              <textarea
                id={`${uid}-intro`}
                rows={3}
                placeholder="Optional opening paragraph shown at the top of the report."
                className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-2 text-sm placeholder:text-[var(--color-muted-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
                {...register("intro_text")}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor={`${uid}-closing`}>Closing text</Label>
              <textarea
                id={`${uid}-closing`}
                rows={3}
                placeholder="Optional closing paragraph shown at the bottom."
                className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-2 text-sm placeholder:text-[var(--color-muted-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
                {...register("closing_text")}
              />
            </div>
          </div>

          {/* White-label -------------------------------------------------------- */}
          <div className="flex items-center gap-3">
            <Controller
              control={control}
              name="powered_by_removed"
              render={({ field }) => (
                <Switch
                  id={`${uid}-whitelabel`}
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  aria-label="Remove powered by WPMgr footer"
                />
              )}
            />
            <Label
              htmlFor={`${uid}-whitelabel`}
              className="cursor-pointer text-sm font-normal"
            >
              Remove footer branding (white-label)
            </Label>
          </div>

          {/* Schedule meta ----------------------------------------------------- */}
          {schedule.next_run_at ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Next report:{" "}
              <span className="font-medium text-[var(--color-foreground)]">
                {relativeTime(schedule.next_run_at) ?? schedule.next_run_at}
              </span>
            </p>
          ) : null}
          {schedule.last_run_at ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Last run:{" "}
              <span className="font-medium text-[var(--color-foreground)]">
                {relativeTime(schedule.last_run_at) ?? schedule.last_run_at}
              </span>
            </p>
          ) : null}

          {/* Mutation error --------------------------------------------------- */}
          {update.error ? (
            <p role="alert" className="text-sm text-[var(--color-destructive)]">
              {update.error.message}
            </p>
          ) : null}

          {/* Save --------------------------------------------------------------- */}
          <div className="flex items-center justify-end gap-2 pt-1 border-t border-[var(--color-border)]">
            <Button
              type="submit"
              disabled={!isDirty || update.isPending}
              size="sm"
            >
              {update.isPending ? "Saving…" : "Save schedule"}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Reports card
// ---------------------------------------------------------------------------

interface ReportsCardProps {
  clientId: string;
  reports: ClientReport[];
  isRefetching: boolean;
  onRefetch: () => void;
}

function ReportsCard({
  clientId,
  reports,
  isRefetching,
  onRefetch,
}: ReportsCardProps) {
  const generate = useGenerateReport();

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div>
            <CardTitle>Generated Reports</CardTitle>
            <CardDescription className="mt-1">
              On-demand and scheduled reports. Completed reports include HTML
              and PDF download links (valid 7 days).
            </CardDescription>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button
              variant="ghost"
              size="sm"
              onClick={onRefetch}
              disabled={isRefetching}
              aria-label="Refresh reports list"
            >
              <RefreshCw
                className={`size-4 ${isRefetching ? "animate-spin" : ""}`}
                aria-hidden="true"
              />
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={generate.isPending}
              onClick={() => void generate.mutateAsync({ clientId })}
            >
              <FileText className="mr-2 size-4" aria-hidden="true" />
              {generate.isPending ? "Generating…" : "Generate now"}
            </Button>
          </div>
        </div>
      </CardHeader>
      <CardContent className="pt-0">
        {reports.length === 0 ? (
          <div
            role="status"
            aria-label="No reports"
            className="flex flex-col items-center gap-2 rounded-xl border border-dashed border-[var(--color-border)] py-10 text-center"
          >
            <p className="text-sm font-medium text-[var(--color-foreground)]">
              No reports yet.
            </p>
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Generate one now or enable the schedule above.
            </p>
          </div>
        ) : (
          <div className="rounded-xl border border-[var(--color-border)]">
            <Table>
              <caption className="sr-only">Generated reports</caption>
              <TableHeader>
                <TableRow>
                  <TableHead>Period</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right">Downloads</TableHead>
                  <TableHead className="w-10" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {reports.map((report) => (
                  <ReportRow key={report.id} clientId={clientId} report={report} />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
        {generate.error ? (
          <p role="alert" className="mt-2 text-sm text-[var(--color-destructive)]">
            {generate.error.message}
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Single report row
// ---------------------------------------------------------------------------

interface ReportRowProps {
  clientId: string;
  report: ClientReport;
}

function ReportRow({ clientId, report }: ReportRowProps) {
  const deleteReport = useDeleteReport();
  const [confirmOpen, setConfirmOpen] = useState(false);

  const periodStart = new Date(report.period_start).toLocaleDateString("en-GB", {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
  const periodEnd = new Date(report.period_end).toLocaleDateString("en-GB", {
    year: "numeric",
    month: "short",
    day: "numeric",
  });

  const statusVariant = (() => {
    switch (report.status) {
      case "completed":
        return "secondary" as const;
      case "failed":
        return "destructive" as const;
      default:
        return "muted" as const;
    }
  })();

  return (
    <TableRow>
      {/* Period */}
      <TableCell className="text-sm">
        <span className="font-medium">{periodStart}</span>
        <span className="mx-1 text-[var(--color-muted-foreground)]">–</span>
        <span>{periodEnd}</span>
      </TableCell>

      {/* Status */}
      <TableCell>
        <Badge variant={statusVariant} className="capitalize">
          {report.status === "generating" || report.status === "queued" ? (
            <span className="flex items-center gap-1">
              <RefreshCw className="size-3 animate-spin" aria-hidden="true" />
              {report.status === "queued" ? "Queued" : "Generating"}
            </span>
          ) : (
            report.status
          )}
        </Badge>
        {report.status === "failed" && report.error ? (
          <p
            title={report.error}
            className="mt-0.5 max-w-[180px] truncate text-xs text-[var(--color-destructive)]"
          >
            {report.error}
          </p>
        ) : null}
      </TableCell>

      {/* Created */}
      <TableCell className="text-sm text-[var(--color-muted-foreground)]">
        {relativeTime(report.created_at) ?? report.created_at}
      </TableCell>

      {/* Downloads */}
      <TableCell className="text-right">
        {report.status === "completed" ? (
          <div className="flex items-center justify-end gap-2">
            {report.html_url ? (
              <a
                href={report.html_url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-sm text-[var(--color-primary)] underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              >
                <Download className="size-3.5" aria-hidden="true" />
                HTML
              </a>
            ) : null}
            {report.pdf_url ? (
              <a
                href={report.pdf_url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-sm text-[var(--color-primary)] underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              >
                <Download className="size-3.5" aria-hidden="true" />
                PDF
              </a>
            ) : null}
            {!report.html_url && !report.pdf_url ? (
              <span className="text-sm text-[var(--color-muted-foreground)]">
                Storage not configured
              </span>
            ) : null}
          </div>
        ) : (
          <span
            aria-hidden="true"
            className="text-[var(--color-muted-foreground)]/50"
          >
            —
          </span>
        )}
      </TableCell>

      {/* Delete */}
      <TableCell>
        <Button
          variant="ghost"
          size="sm"
          className="h-7 w-7 p-0 text-[var(--color-muted-foreground)] hover:text-[var(--color-destructive)]"
          aria-label="Delete report"
          onClick={() => setConfirmOpen(true)}
        >
          <Trash2 className="size-4" aria-hidden="true" />
        </Button>

        <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete report?</AlertDialogTitle>
              <AlertDialogDescription>
                This permanently deletes the report record and its stored HTML
                and PDF files. This cannot be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel onClick={() => setConfirmOpen(false)} />
              <AlertDialogAction
                onClick={() => {
                  void deleteReport
                    .mutateAsync({ clientId, reportId: report.id })
                    .finally(() => setConfirmOpen(false));
                }}
                variant="destructive"
              >
                {deleteReport.isPending ? "Deleting…" : "Delete"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </TableCell>
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function ReportsTabSkeleton() {
  return (
    <div
      className="space-y-6"
      role="status"
      aria-busy="true"
      aria-label="Loading reports"
    >
      <div className="rounded-xl border border-[var(--color-border)] p-6 space-y-4">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-4 w-72" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
      </div>
      <div className="rounded-xl border border-[var(--color-border)] p-6 space-y-4">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-4 w-72" />
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-12 w-full" />
      </div>
    </div>
  );
}
