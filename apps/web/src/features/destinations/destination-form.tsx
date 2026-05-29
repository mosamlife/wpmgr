// Destination form (ADR-036 P1 storage adapter). React-hook-form +
// Zod, with per-kind conditional fields and a "Re-enter to save changes"
// affordance on the secret field once a destination is persisted.

import { useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Check, X, TriangleAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import type { SiteDestination, SiteDestinationKind } from "@wpmgr/api";

import {
  useCreateDestination,
  useUpdateDestination,
  useTestConnection,
} from "./use-destinations";

// ---- Validation -----------------------------------------------------------
//
// We allow the operator to TEST a destination before persisting it, so the
// test schema is the minimum-viable subset of the create schema. The actual
// API enforces tighter validation server-side; this layer is just for
// "obvious typos before we hit the wire" feedback.
const baseSchema = z.object({
  kind: z.enum(["cp", "local", "s3_compat"]),
  label: z.string().min(1, "Label is required").max(200),
  endpoint: z.string().optional().default(""),
  region: z.string().optional().default(""),
  bucket: z.string().optional().default(""),
  path_prefix: z.string().optional().default(""),
  access_key_id: z.string().optional().default(""),
  secret_key: z.string().optional().default(""),
  force_path_style: z.boolean().optional().default(false),
  is_default: z.boolean().optional().default(false),
});

const createSchema = baseSchema.refine(
  (v) =>
    v.kind !== "s3_compat" ||
    (v.bucket.length > 0 && v.access_key_id.length > 0 && v.secret_key.length > 0),
  {
    message: "S3-compatible destinations require bucket, access key, and secret",
    path: ["bucket"],
  },
);

type Values = z.infer<typeof baseSchema>;

// ---- Provider presets -----------------------------------------------------
//
// Operators rarely know the exact endpoint URL for a given S3-compatible
// provider off the top of their head. We pre-fill the well-known endpoints
// (and the force_path_style flag where it matters) so the operator just picks
// "Backblaze B2", drops in their key, and goes.

interface ProviderPreset {
  id: string;
  label: string;
  endpoint?: string;
  region?: string;
  forcePathStyle?: boolean;
  help?: string;
}

const PROVIDER_PRESETS: ProviderPreset[] = [
  {
    id: "aws",
    label: "Amazon S3",
    endpoint: "",
    region: "us-east-1",
    forcePathStyle: false,
    help: "The AWS SDK default endpoint resolver picks the right host for your region.",
  },
  {
    id: "wasabi",
    label: "Wasabi",
    endpoint: "https://s3.us-east-1.wasabisys.com",
    region: "us-east-1",
    forcePathStyle: false,
  },
  {
    id: "b2",
    label: "Backblaze B2",
    endpoint: "https://s3.us-west-002.backblazeb2.com",
    region: "us-west-002",
    forcePathStyle: true,
    help: "Set Region to match your bucket's region (us-west-002 / eu-central-003 / ...).",
  },
  {
    id: "do",
    label: "DigitalOcean Spaces",
    endpoint: "https://nyc3.digitaloceanspaces.com",
    region: "nyc3",
    forcePathStyle: false,
  },
  {
    id: "minio",
    label: "MinIO",
    endpoint: "http://localhost:9000",
    region: "us-east-1",
    forcePathStyle: true,
    help: "Path-style addressing is required for MinIO clusters without DNS magic.",
  },
  {
    id: "custom",
    label: "Custom / other",
    help: "Set the endpoint manually for any other S3-compatible provider.",
  },
];

export interface DestinationFormProps {
  siteId: string;
  initial?: SiteDestination | null;
  onSaved?: (d: SiteDestination) => void;
  onCancel?: () => void;
}

export function DestinationForm({
  siteId,
  initial,
  onSaved,
  onCancel,
}: DestinationFormProps) {
  const create = useCreateDestination(siteId);
  const update = useUpdateDestination(siteId);
  const test = useTestConnection(siteId);

  const editing = !!initial;
  const hasStoredSecret = !!initial?.has_secret;

  const {
    register,
    handleSubmit,
    control,
    getValues,
    setValue,
    formState: { errors, isSubmitting },
    reset,
  } = useForm<Values>({
    resolver: zodResolver(createSchema) as never,
    defaultValues: {
      kind: initial?.kind ?? "cp",
      label: initial?.label ?? "",
      endpoint: initial?.endpoint ?? "",
      region: initial?.region ?? "",
      bucket: initial?.bucket ?? "",
      path_prefix: initial?.path_prefix ?? "",
      access_key_id: initial?.access_key_id ?? "",
      secret_key: "",
      force_path_style: initial?.force_path_style ?? false,
      is_default: initial?.is_default ?? false,
    },
  });

  const kind = useWatch({ control, name: "kind" });
  const [providerId, setProviderId] = useState("custom");

  function applyPreset(id: string) {
    setProviderId(id);
    const p = PROVIDER_PRESETS.find((x) => x.id === id);
    if (!p) return;
    if (p.endpoint !== undefined) setValue("endpoint", p.endpoint);
    if (p.region !== undefined) setValue("region", p.region);
    if (p.forcePathStyle !== undefined) setValue("force_path_style", p.forcePathStyle);
  }

  async function onSubmit(values: Values) {
    if (editing) {
      // PATCH: skip secret_key when blank so the existing one stays put.
      const body: Record<string, unknown> = {
        label: values.label,
        endpoint: values.endpoint,
        region: values.region,
        bucket: values.bucket,
        path_prefix: values.path_prefix,
        access_key_id: values.access_key_id,
        force_path_style: values.force_path_style,
        is_default: values.is_default,
      };
      if (values.secret_key && values.secret_key.length > 0) {
        body.secret_key = values.secret_key;
      }
      const d = await update.mutateAsync({
        destinationId: initial.id,
        body,
      });
      if (onSaved) onSaved(d);
      return;
    }
    const d = await create.mutateAsync({
      kind: values.kind,
      label: values.label,
      endpoint: values.endpoint,
      region: values.region,
      bucket: values.bucket,
      path_prefix: values.path_prefix,
      access_key_id: values.access_key_id,
      secret_key: values.secret_key,
      force_path_style: values.force_path_style,
      is_default: values.is_default,
    });
    reset();
    if (onSaved) onSaved(d);
  }

  async function onTest() {
    const v = getValues();
    await test.mutateAsync({
      kind: v.kind,
      endpoint: v.endpoint,
      region: v.region,
      bucket: v.bucket,
      path_prefix: v.path_prefix,
      access_key_id: v.access_key_id,
      // For an existing row with a stored secret, the operator may want to
      // re-test without re-entering it -- but the test endpoint doesn't have
      // access to the at-rest ciphertext. We require an explicit secret.
      secret_key: v.secret_key,
      force_path_style: v.force_path_style,
    });
  }

  const isS3 = kind === "s3_compat";

  return (
    <div
      role="form"
      aria-label={editing ? "Edit destination" : "Add destination"}
      className="space-y-5"
    >
      {/* Kind radio group --------------------------------------------------*/}
      <fieldset>
        <legend className="text-sm font-medium">Destination type</legend>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          Where should this site&apos;s backup chunks land?
        </p>
        <div className="mt-2 grid gap-2 sm:grid-cols-3">
          {(
            [
              { id: "cp", label: "CP storage", hint: "WPMgr-managed bucket (default)" },
              { id: "local", label: "Local folder", hint: "wp-content/wpmgr-backups on this site" },
              { id: "s3_compat", label: "S3-compatible", hint: "Your bucket (AWS / Wasabi / B2 / ...)" },
            ] satisfies { id: SiteDestinationKind; label: string; hint: string }[]
          ).map((opt) => (
            <label
              key={opt.id}
              className="flex cursor-pointer items-start gap-3 rounded-md border border-[var(--color-border)] p-3 text-sm hover:bg-[var(--color-muted)]/50"
            >
              <input
                type="radio"
                value={opt.id}
                {...register("kind")}
                disabled={editing}
                className="mt-0.5"
              />
              <span>
                <span className="block font-medium">{opt.label}</span>
                <span className="block text-xs text-[var(--color-muted-foreground)]">
                  {opt.hint}
                </span>
              </span>
            </label>
          ))}
        </div>
        {editing ? (
          <p className="mt-1 text-xs text-[var(--color-muted-foreground)]">
            Destination type cannot be changed after creation. Delete and recreate
            to switch.
          </p>
        ) : null}
      </fieldset>

      {/* Label --------------------------------------------------------------*/}
      <div className="space-y-1.5">
        <Label htmlFor="label">Label</Label>
        <Input id="label" aria-invalid={!!errors.label} {...register("label")} />
        {errors.label ? (
          <p role="alert" className="text-sm text-[var(--color-destructive)]">
            {errors.label.message}
          </p>
        ) : null}
      </div>

      {/* S3-only fields ----------------------------------------------------*/}
      {isS3 ? (
        <fieldset className="space-y-4 rounded-md border border-[var(--color-border)] p-4">
          <legend className="text-sm font-medium px-1">S3-compatible bucket</legend>

          <div className="space-y-1.5">
            <Label htmlFor="provider">Provider preset</Label>
            <select
              id="provider"
              className="h-9 w-full rounded-md border border-[var(--color-border)] bg-transparent px-3 text-sm"
              value={providerId}
              onChange={(e) => applyPreset(e.target.value)}
            >
              {PROVIDER_PRESETS.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.label}
                </option>
              ))}
            </select>
            {PROVIDER_PRESETS.find((p) => p.id === providerId)?.help ? (
              <p className="text-xs text-[var(--color-muted-foreground)]">
                {PROVIDER_PRESETS.find((p) => p.id === providerId)?.help}
              </p>
            ) : null}
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="endpoint">Endpoint URL</Label>
              <Input
                id="endpoint"
                placeholder="https://s3.amazonaws.com"
                className="font-mono text-xs"
                {...register("endpoint")}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="region">Region</Label>
              <Input
                id="region"
                placeholder="us-east-1"
                className="font-mono text-xs"
                {...register("region")}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="bucket">Bucket</Label>
              <Input
                id="bucket"
                className="font-mono text-xs"
                {...register("bucket")}
              />
              {errors.bucket ? (
                <p role="alert" className="text-sm text-[var(--color-destructive)]">
                  {errors.bucket.message}
                </p>
              ) : null}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="path_prefix">Path prefix</Label>
              <Input
                id="path_prefix"
                placeholder="wpmgr/"
                className="font-mono text-xs"
                {...register("path_prefix")}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="access_key_id">Access key ID</Label>
              <Input
                id="access_key_id"
                autoComplete="off"
                className="font-mono text-xs"
                {...register("access_key_id")}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="secret_key">Secret access key</Label>
              <Input
                id="secret_key"
                type="password"
                autoComplete="off"
                placeholder={hasStoredSecret ? "••••••••" : ""}
                {...register("secret_key")}
              />
              {hasStoredSecret ? (
                <p className="flex items-center gap-1.5 text-xs text-[var(--color-warning,theme(colors.amber.600))]">
                  <TriangleAlert aria-hidden="true" className="size-3.5 shrink-0" />
                  A secret is already stored. Re-enter only to replace it.
                </p>
              ) : null}
            </div>
          </div>

          <label className="flex items-center gap-2 text-sm">
            <Checkbox {...register("force_path_style")} />
            <span>
              Force path-style addressing
              <span className="ml-1 text-xs text-[var(--color-muted-foreground)]">
                (required for MinIO and most B2 buckets)
              </span>
            </span>
          </label>
        </fieldset>
      ) : null}

      {/* Default toggle ----------------------------------------------------*/}
      <label className="flex items-center gap-2 text-sm">
        <Checkbox {...register("is_default")} />
        <span>Make this the default destination for new backups</span>
      </label>

      {/* Test connection result -------------------------------------------*/}
      {test.data ? (
        <div
          role="status"
          className={`flex items-start gap-2 rounded-md border p-3 text-sm ${
            test.data.ok
              ? "border-[var(--color-success,theme(colors.green.500))]/40 text-[var(--color-foreground)]"
              : "border-[var(--color-destructive)]/40 text-[var(--color-destructive)]"
          }`}
        >
          {test.data.ok ? (
            <Check aria-hidden="true" className="mt-0.5 size-4 text-[var(--color-success,theme(colors.green.600))]" />
          ) : (
            <X aria-hidden="true" className="mt-0.5 size-4" />
          )}
          <span className="flex-1">{test.data.message}</span>
        </div>
      ) : null}
      {test.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {test.error.message}
        </p>
      ) : null}

      {/* Buttons -----------------------------------------------------------*/}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          disabled={isSubmitting || create.isPending || update.isPending}
          onClick={() => void handleSubmit(onSubmit)()}
        >
          {editing ? "Save changes" : "Add destination"}
        </Button>
        {isS3 ? (
          <Button
            type="button"
            variant="outline"
            onClick={() => void onTest()}
            disabled={test.isPending}
          >
            {test.isPending ? "Testing…" : "Test connection"}
          </Button>
        ) : null}
        {onCancel ? (
          <Button type="button" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        ) : null}
      </div>

      {create.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {create.error.message}
        </p>
      ) : null}
      {update.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {update.error.message}
        </p>
      ) : null}
    </div>
  );
}
