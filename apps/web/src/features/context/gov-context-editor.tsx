import { useEffect } from "react";
import { useForm, Controller } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { DefinitionList } from "@/components/shared/definition-list";
import type { GovContext } from "@wpmgr/api";

import { restrictionRows, guidanceRows } from "./context-rows";
import {
  ContextSecretDetectedError,
  ContextVersionConflictError,
  ContextWidenForbiddenError,
} from "./use-context";

// ADR-064 S5 Stage B — the shared org/site context editor (Decision 6/13).
// One form for both scopes: `PatchGovContextRequest` is identical for
// `/orgs/{orgId}/context` and `/sites/{siteId}/context`, so the org and site
// "screens" (org-context-section.tsx / site-context-section.tsx) are thin
// wrappers around this component that supply the right query/mutation pair.
//
// PATCH replaces `restrictions`/`guidance` WHOLESALE, never deep-merged
// (PatchGovContextRequest's own doc comment) — so this form always submits
// BOTH keys in full, never a partial subset of fields within them.

const restrictionListSchema = z.array(z.string().trim().min(1)).default([]);

const contextFormSchema = z.object({
  restrictions: z.object({
    forbidden_tools: restrictionListSchema,
    forbidden_domains: restrictionListSchema,
    forbidden_topics: restrictionListSchema,
  }),
  guidance: z.object({
    brand_voice: z.string().max(2000).default(""),
    audience: z.string().max(2000).default(""),
    terminology: z.string().max(2000).default(""),
    style: z.string().max(2000).default(""),
  }),
});

export type ContextFormValues = z.infer<typeof contextFormSchema>;

function toFormValues(current: GovContext): ContextFormValues {
  return {
    restrictions: {
      forbidden_tools: current.restrictions.forbidden_tools ?? [],
      forbidden_domains: current.restrictions.forbidden_domains ?? [],
      forbidden_topics: current.restrictions.forbidden_topics ?? [],
    },
    guidance: {
      brand_voice: current.guidance.brand_voice ?? "",
      audience: current.guidance.audience ?? "",
      terminology: current.guidance.terminology ?? "",
      style: current.guidance.style ?? "",
    },
  };
}

const RESTRICTION_FIELDS = ["forbidden_tools", "forbidden_domains", "forbidden_topics"] as const;
type RestrictionFieldKey = (typeof RESTRICTION_FIELDS)[number];

const RESTRICTION_FIELD_LABELS: Record<RestrictionFieldKey, string> = {
  forbidden_tools: "Forbidden tools",
  forbidden_domains: "Forbidden domains",
  forbidden_topics: "Forbidden topics",
};

// Maps the server's `details.field` (a RestrictionSet key, per
// use-context.ts's ContextWidenForbiddenError doc comment) onto the
// react-hook-form path it corresponds to, so a widen refusal can flag the
// SPECIFIC restriction it hit, not just a page-level banner.
const WIDEN_FIELD_PATH: Record<RestrictionFieldKey, `restrictions.${RestrictionFieldKey}`> = {
  forbidden_tools: "restrictions.forbidden_tools",
  forbidden_domains: "restrictions.forbidden_domains",
  forbidden_topics: "restrictions.forbidden_topics",
};

function isRestrictionFieldKey(v: string): v is RestrictionFieldKey {
  return (RESTRICTION_FIELDS as readonly string[]).includes(v);
}

export interface GovContextEditorProps {
  /** "organisation" | "site" — copy only, also namespaces element ids. */
  scopeLabel: string;
  current: GovContext;
  onSave: (values: ContextFormValues) => Promise<void>;
  /** Refetches the current context and returns the fresh snapshot, or
   *  `undefined` if the refetch itself failed. Used to recover from a 409
   *  context_version_conflict — ADR-064 forecloses merge-based conflict
   *  resolution, so the only correct move is reread-and-retry, never a
   *  client-side merge of the stale edit onto the new base. */
  onReloadLatest: () => Promise<GovContext | undefined>;
  saveError: Error | null;
  isSaving: boolean;
  /** False for a caller who can read but not write this scope (Decision 6:
   *  read follows fleet-read access, write is narrower) — renders the
   *  current snapshot read-only instead of a form. */
  canWrite: boolean;
}

export function GovContextEditor({
  scopeLabel,
  current,
  onSave,
  onReloadLatest,
  saveError,
  isSaving,
  canWrite,
}: GovContextEditorProps) {
  const {
    register,
    handleSubmit,
    control,
    reset,
    setError,
    formState: { errors, isSubmitting, isDirty },
  } = useForm<ContextFormValues>({
    resolver: zodResolver(contextFormSchema) as never,
    defaultValues: toFormValues(current),
  });

  // Re-baseline the form whenever the server's version changes under us (a
  // fresh GET, a successful save, or an explicit post-conflict reload).
  // Never re-baseline on any OTHER prop change — that would clobber an
  // operator's in-progress, not-yet-saved edit for no reason, so the
  // dependency array is deliberately just `current.version`, not `current`
  // or `reset` (`reset` is a stable react-hook-form identity; `current` as a
  // whole would re-fire on every parent render).
  useEffect(() => {
    reset(toFormValues(current));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current.version]);

  // Flag the SPECIFIC restriction a widen refusal named, per Decision 4's
  // "the reason names the restriction and the layer that blocked it" and the
  // coordinator's instruction that this refusal must name which restriction
  // was hit, not read as a generic validation error.
  useEffect(() => {
    if (saveError instanceof ContextWidenForbiddenError && isRestrictionFieldKey(saveError.field)) {
      setError(WIDEN_FIELD_PATH[saveError.field], {
        type: "server",
        message: saveError.message,
      });
    }
  }, [saveError, setError]);

  if (!canWrite) {
    return (
      <div className="space-y-4 rounded-lg border border-border bg-card p-4">
        <p className="text-xs text-muted-foreground">
          You can view this {scopeLabel}&apos;s context but do not have permission to
          change it.
        </p>
        <div className="space-y-1">
          <h4 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Restrictions
          </h4>
          <DefinitionList rows={restrictionRows(current.restrictions)} />
        </div>
        <div className="space-y-1">
          <h4 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Guidance
          </h4>
          <DefinitionList rows={guidanceRows(current.guidance)} />
        </div>
      </div>
    );
  }

  const widenFieldPath =
    saveError instanceof ContextWidenForbiddenError && isRestrictionFieldKey(saveError.field)
      ? WIDEN_FIELD_PATH[saveError.field]
      : undefined;

  async function handleReload() {
    const fresh = await onReloadLatest();
    if (fresh) reset(toFormValues(fresh));
  }

  return (
    <form
      onSubmit={(e) => void handleSubmit((values) => onSave(values))(e)}
      noValidate
      aria-label={`Edit ${scopeLabel} context`}
      className="space-y-5"
    >
      {saveError instanceof ContextVersionConflictError ? (
        <div
          role="alert"
          className="space-y-2 rounded-lg border border-[var(--color-destructive)]/30 bg-[var(--color-card)] p-4"
        >
          <p className="text-sm font-semibold text-foreground">
            Could not save — this context changed underneath you.
          </p>
          <p className="text-sm text-muted-foreground">{saveError.message}</p>
          <Button type="button" variant="outline" size="sm" onClick={() => void handleReload()}>
            Discard my changes and reload the latest version
          </Button>
        </div>
      ) : null}

      {saveError instanceof ContextSecretDetectedError ? (
        <p
          role="alert"
          className="rounded-lg border border-[var(--color-destructive)]/30 bg-[var(--color-card)] p-3 text-sm text-[var(--color-destructive)]"
        >
          {saveError.message}
        </p>
      ) : null}

      {saveError instanceof ContextWidenForbiddenError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {widenFieldPath
            ? "Could not save — see the highlighted restriction below."
            : saveError.message}
        </p>
      ) : null}

      {saveError &&
      !(saveError instanceof ContextVersionConflictError) &&
      !(saveError instanceof ContextSecretDetectedError) &&
      !(saveError instanceof ContextWidenForbiddenError) ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {saveError.message}
        </p>
      ) : null}

      <fieldset className="space-y-4">
        <legend className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Restrictions
        </legend>
        {RESTRICTION_FIELDS.map((field) => (
          <Controller
            key={field}
            control={control}
            name={`restrictions.${field}`}
            render={({ field: rhf }) => (
              <RestrictionListField
                id={`ctx-${scopeLabel}-${field}`}
                label={RESTRICTION_FIELD_LABELS[field]}
                values={rhf.value}
                onChange={rhf.onChange}
                errorMessage={errors.restrictions?.[field]?.message}
              />
            )}
          />
        ))}
      </fieldset>

      <fieldset className="space-y-4">
        <legend className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Guidance
        </legend>
        <GuidanceField
          id={`ctx-${scopeLabel}-brand_voice`}
          label="Brand voice"
          {...register("guidance.brand_voice")}
        />
        <GuidanceField
          id={`ctx-${scopeLabel}-audience`}
          label="Audience"
          {...register("guidance.audience")}
        />
        <GuidanceField
          id={`ctx-${scopeLabel}-terminology`}
          label="Terminology"
          {...register("guidance.terminology")}
        />
        <GuidanceField id={`ctx-${scopeLabel}-style`} label="Style" {...register("guidance.style")} />
      </fieldset>

      <div className="flex items-center gap-3">
        <Button type="submit" disabled={isSubmitting || isSaving || !isDirty}>
          {isSubmitting || isSaving ? "Saving…" : "Save changes"}
        </Button>
        <span className="text-xs text-muted-foreground tabular-nums">
          Version {current.version}
        </span>
      </div>
    </form>
  );
}

// ── Restriction list field: chip editor over a string array ────────────────

function RestrictionListField({
  id,
  label,
  values,
  onChange,
  errorMessage,
}: {
  id: string;
  label: string;
  values: string[];
  onChange: (next: string[]) => void;
  errorMessage?: string;
}) {
  function addFrom(input: HTMLInputElement) {
    const trimmed = input.value.trim();
    if (!trimmed) return;
    if (!values.includes(trimmed)) onChange([...values, trimmed]);
    input.value = "";
  }

  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      {values.length > 0 ? (
        <ul className="flex flex-wrap gap-1.5" aria-label={`${label} (current)`}>
          {values.map((v) => (
            <li
              key={v}
              className="inline-flex items-center gap-1 rounded-full border border-border bg-muted px-2 py-0.5 font-mono text-xs text-foreground"
            >
              {v}
              <button
                type="button"
                aria-label={`Remove ${v} from ${label}`}
                onClick={() => onChange(values.filter((x) => x !== v))}
                className="rounded-full text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <X aria-hidden="true" className="size-3" />
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <div className="flex gap-2">
        <Input
          id={id}
          placeholder="Add and press Enter"
          aria-invalid={errorMessage ? true : undefined}
          aria-describedby={errorMessage ? `${id}-err` : undefined}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              addFrom(e.currentTarget);
            }
          }}
        />
        <Button
          type="button"
          variant="outline"
          onClick={(e) => {
            const input = e.currentTarget.previousElementSibling as HTMLInputElement | null;
            if (input) addFrom(input);
          }}
        >
          Add
        </Button>
      </div>
      {errorMessage ? (
        <p id={`${id}-err`} role="alert" className="text-sm text-[var(--color-destructive)]">
          {errorMessage}
        </p>
      ) : null}
    </div>
  );
}

// ── Guidance field: a labelled textarea ─────────────────────────────────────

function GuidanceField({
  id,
  label,
  ...registerProps
}: { id: string; label: string } & React.ComponentPropsWithRef<"textarea">) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      <textarea
        id={id}
        rows={2}
        className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-2 text-sm placeholder:text-[var(--color-muted-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
        {...registerProps}
      />
    </div>
  );
}
