import { useState } from "react";
import { Settings } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { PageError } from "@/components/feedback";
import { toast } from "@/components/toast/use-toast-helpers";
import { Skeleton } from "@/components/ui/skeleton";

import {
  useErrorConfig,
  useUpdateErrorConfig,
  bitmaskToToggles,
  togglesToBitmask,
} from "./use-error-config";

// S1.2 — Error config panel opened via "Configure" button on the Errors tab.
//
// Layout:
//   - Three checkbox toggles for non-fatal groups: Warnings, Notices, Deprecations.
//   - A muted note that Fatals are always captured (no toggle).
//   - Ignore-list: mono chips with a "Remove" button each; empty state when none.
//   - Verb-first Save button; pending state; PageError on save failure.
//
// Design contract:
//   - Read-modify-write: only the three known groups are flipped; unknown bits
//     in error_level are preserved.
//   - The ignore_md5s list is a full-replacement canonical list on each PATCH.
//   - Save success dismisses the dialog and fires a success toast.
//
// The dialog is only mounted after data has loaded (ErrorConfigLoaded). This
// avoids needing to sync server → local state via useEffect or refs during render.

export interface ErrorConfigPanelProps {
  siteId: string;
}

export function ErrorConfigPanel({ siteId }: ErrorConfigPanelProps) {
  const [open, setOpen] = useState(false);

  return (
    <>
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={() => setOpen(true)}
        aria-haspopup="dialog"
      >
        <Settings aria-hidden="true" />
        Configure
      </Button>

      {open && (
        <ErrorConfigShell
          siteId={siteId}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// Shell — fetches config; shows loading/error or delegates to the loaded form
// ---------------------------------------------------------------------------

interface ErrorConfigShellProps {
  siteId: string;
  onClose: () => void;
}

function ErrorConfigShell({ siteId, onClose }: ErrorConfigShellProps) {
  const { data, isPending, isError, error, refetch } = useErrorConfig(siteId);

  return (
    <Dialog open={true} onClose={onClose}>
      <DialogContent ariaLabelledBy="error-config-title">
        <DialogHeader>
          <DialogTitle id="error-config-title">Error capture config</DialogTitle>
          <DialogDescription>
            Controls which PHP error levels are captured and which fingerprints
            are suppressed on the agent.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-5 mt-4">
          {isPending ? (
            <div className="space-y-3" role="status" aria-busy="true">
              <span className="sr-only">Loading config</span>
              <Skeleton className="h-5 w-48" />
              <Skeleton className="h-5 w-40" />
              <Skeleton className="h-5 w-44" />
            </div>
          ) : isError ? (
            <PageError
              what="Could not load error config."
              why={error instanceof Error ? error.message : "Unknown error"}
              onRetry={() => void refetch()}
              retryLabel="Reload config"
            />
          ) : data ? (
            // Key on error_level + ignore_md5s join so the form resets if the
            // server data changes while the dialog is open (e.g. background refetch).
            <ErrorConfigLoaded
              key={`${data.error_level}-${data.ignore_md5s.join(",")}`}
              siteId={siteId}
              initialLevel={data.error_level}
              initialIgnoreMd5s={data.ignore_md5s}
              onClose={onClose}
            />
          ) : null}
        </DialogBody>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Loaded form — initialised from server data, manages its own local state
// ---------------------------------------------------------------------------

interface ErrorConfigLoadedProps {
  siteId: string;
  initialLevel: number;
  initialIgnoreMd5s: string[];
  onClose: () => void;
}

function ErrorConfigLoaded({
  siteId,
  initialLevel,
  initialIgnoreMd5s,
  onClose,
}: ErrorConfigLoadedProps) {
  const update = useUpdateErrorConfig(siteId);
  const toggles = bitmaskToToggles(initialLevel);

  const [warnings, setWarnings] = useState(toggles.warnings);
  const [notices, setNotices] = useState(toggles.notices);
  const [deprecations, setDeprecations] = useState(toggles.deprecations);
  const [ignoreList, setIgnoreList] = useState<string[]>(initialIgnoreMd5s);
  const [saveError, setSaveError] = useState<string | null>(null);

  function handleRemoveMd5(md5: string) {
    setIgnoreList((prev) => prev.filter((m) => m !== md5));
  }

  function handleSave() {
    setSaveError(null);
    const newLevel = togglesToBitmask(initialLevel, {
      warnings,
      notices,
      deprecations,
    });

    update.mutate(
      { error_level: newLevel, ignore_md5s: ignoreList },
      {
        onSuccess: () => {
          toast.success("Error config saved.", {
            description: "Settings pushed to the agent.",
          });
          onClose();
        },
        onError: (err: Error) => {
          setSaveError(err.message);
        },
      },
    );
  }

  return (
    <>
      {/* ── Error level toggles ── */}
      <section aria-labelledby="error-levels-label">
        <h3
          id="error-levels-label"
          className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Capture levels
        </h3>
        <div className="space-y-3">
          <CheckboxRow
            id="capture-warnings"
            label="Capture warnings"
            description="E_WARNING + E_USER_WARNING"
            checked={warnings}
            onCheckedChange={setWarnings}
          />
          <CheckboxRow
            id="capture-notices"
            label="Capture notices"
            description="E_NOTICE + E_USER_NOTICE"
            checked={notices}
            onCheckedChange={setNotices}
          />
          <CheckboxRow
            id="capture-deprecations"
            label="Capture deprecations"
            description="E_DEPRECATED + E_USER_DEPRECATED"
            checked={deprecations}
            onCheckedChange={setDeprecations}
          />
          {/* Fatals note — no toggle, always captured */}
          <p className="pl-6 text-xs text-muted-foreground">
            Fatal errors are always captured regardless of this setting.
          </p>
        </div>
      </section>

      {/* ── Ignore list ── */}
      <section aria-labelledby="ignore-list-label">
        <h3
          id="ignore-list-label"
          className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Ignored fingerprints
        </h3>
        {ignoreList.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            No fingerprints are currently ignored.
          </p>
        ) : (
          <ul aria-label="Ignored error fingerprints" className="space-y-1.5">
            {ignoreList.map((md5) => (
              <li
                key={md5}
                className="flex items-center justify-between gap-3 rounded-md border border-border bg-muted/40 px-3 py-2"
              >
                <span className="font-mono text-xs text-foreground break-all">
                  {md5}
                </span>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="shrink-0 text-destructive hover:text-destructive"
                  onClick={() => handleRemoveMd5(md5)}
                  aria-label={`Remove fingerprint ${md5}`}
                >
                  Remove
                </Button>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* Save error */}
      {saveError ? (
        <PageError what="Could not save error config." why={saveError} />
      ) : null}

      <DialogFooter>
        <Button
          type="button"
          variant="ghost"
          onClick={onClose}
          disabled={update.isPending}
        >
          Cancel
        </Button>
        <Button
          type="button"
          onClick={handleSave}
          disabled={update.isPending}
          aria-busy={update.isPending}
        >
          {update.isPending ? "Saving..." : "Save config"}
        </Button>
      </DialogFooter>
    </>
  );
}

// ---------------------------------------------------------------------------
// CheckboxRow — labelled checkbox with description line
// ---------------------------------------------------------------------------

interface CheckboxRowProps {
  id: string;
  label: string;
  description: string;
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
}

function CheckboxRow({
  id,
  label,
  description,
  checked,
  onCheckedChange,
}: CheckboxRowProps) {
  return (
    <div className="flex items-start gap-3">
      <Checkbox
        id={id}
        checked={checked}
        onChange={(e) => onCheckedChange(e.target.checked)}
        aria-describedby={`${id}-desc`}
        className="mt-0.5"
      />
      <div className="min-w-0">
        <label
          htmlFor={id}
          className="block cursor-pointer text-sm font-medium text-foreground"
        >
          {label}
        </label>
        <p id={`${id}-desc`} className="text-xs text-muted-foreground">
          {description}
        </p>
      </div>
    </div>
  );
}
