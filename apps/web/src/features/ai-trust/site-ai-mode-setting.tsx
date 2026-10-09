import { useId, useState } from "react";
import { toast } from "sonner";
import type { AiMode, SiteAiMode } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";
import { cn } from "@/lib/utils";

import {
  ALWAYS_WAITS_LINE,
  FULL_AUTO_LATER,
  KEEP_ASKING,
  KEEP_AUTO,
  MIGRATED_NOTICE,
  MODE_DESCRIPTION,
  MODE_HEADING,
  MODE_NAME,
  MODE_SUBHEADING,
  NOT_OPERATOR_LINE,
  PAUSED_LINE,
  SAVE_FAILED_LINE,
  SETTER_INVALID_LINE,
  modeStateLine,
  optionReasonLine,
  savedToast,
  setByLine,
  staleVersionLine,
} from "./ai-trust-copy";
import {
  CODE_AGENT_OUTDATED,
  CODE_PAUSED,
  StaleSiteModeError,
  usePutSiteAiMode,
  useSiteAiMode,
  type AiTrustError,
} from "./use-ai-trust";

// The site's AI mode, inside the "AI editing" card on the Content tab (design
// §8.1). Two modes can be chosen here: Ask every time, and Auto for AI drafts
// (the default). Full auto is listed as coming later and is never clickable.
// What each mode covers is a table rendered from the server's `kinds`, never
// from literals, and who may choose is the server's `options[].choosable`
// on top of the operator gate the enable button uses.

type ChoosableMode = "ask" | "ai_drafts";
const CHOOSABLE: readonly ChoosableMode[] = ["ask", "ai_drafts"];

export interface SiteAiModeSettingProps {
  siteId: string;
  /** operator+ on this site (site.content.edit), the enable button's gate. */
  canOperate: boolean;
  currentUserId: string | null;
}

export function SiteAiModeSetting({ siteId, canOperate, currentUserId }: SiteAiModeSettingProps) {
  const query = useSiteAiMode(siteId);

  if (query.isPending) {
    return <Skeleton aria-label="Loading AI editing" className="h-40 w-full" />;
  }
  if (query.isError) {
    return (
      <PageError
        what="Could not load the AI editing setting."
        why={query.error.message}
        onRetry={() => void query.refetch()}
        isRetrying={query.isFetching}
      />
    );
  }
  return (
    <ModeCard
      siteId={siteId}
      mode={query.data}
      canOperate={canOperate}
      currentUserId={currentUserId}
    />
  );
}

function saveErrorLine(err: AiTrustError, minAgentVersion: string): string {
  if (err instanceof StaleSiteModeError) return staleVersionLine(err.currentMode);
  if (err.code === CODE_PAUSED) return PAUSED_LINE;
  if (err.code === CODE_AGENT_OUTDATED) {
    return `Update the WPMgr plugin on this site to ${minAgentVersion} or later, then choose this.`;
  }
  return SAVE_FAILED_LINE;
}

function ModeCard({
  siteId,
  mode: m,
  canOperate,
  currentUserId,
}: {
  siteId: string;
  mode: SiteAiMode;
  canOperate: boolean;
  currentUserId: string | null;
}) {
  const save = usePutSiteAiMode(siteId);
  // A choice carries the version it was made against, so a save after the
  // setting moved underneath is refused as stale rather than overwriting it.
  const [choice, setChoice] = useState<{ mode: ChoosableMode; version: number } | null>(null);
  const [showCovers, setShowCovers] = useState(false);
  const groupName = useId();
  const coversId = useId();

  const stored = m.mode;
  const selected: AiMode = choice?.mode ?? stored;
  const option = (mode: AiMode) => m.options.find((o) => o.mode === mode);
  const mayChoose = (mode: AiMode) =>
    canOperate && mode !== "full" && option(mode)?.choosable === true;
  const dirty = selected !== stored;
  // A setting whose person lost the access it needs is chosen again by
  // saving it unchanged, so Save stays enabled for it.
  const canSave = mayChoose(selected) && (dirty || !m.setter_valid) && !save.isPending;
  const footer = setByLine(m, currentUserId);
  const migrated = m.source === "launch_default";

  function submit(next: ChoosableMode, version: number) {
    save.mutate(
      { mode: next, version },
      {
        onSuccess: (saved) => {
          setChoice(null);
          toast.success(savedToast(saved.mode));
        },
        onError: () => setChoice(null),
      },
    );
  }

  const modeColumns = CHOOSABLE.filter((mode) => m.kinds.some((k) => k.decisions.some((d) => d.mode === mode)));

  return (
    <div className="space-y-4 rounded-lg border border-border bg-card p-4" data-testid="ai-mode-card">
      <div className="space-y-0.5">
        <h2 className="text-sm font-semibold text-foreground">AI editing</h2>
        <p data-testid="ai-editing-state" className="text-sm text-muted-foreground">
          {modeStateLine(stored)}
        </p>
      </div>

      {m.ai_paused ? (
        <p role="status" className="rounded-md border border-warning/40 bg-warning-subtle px-3 py-2 text-sm text-warning-subtle-fg">
          {PAUSED_LINE}
        </p>
      ) : null}

      {migrated ? (
        <div
          role="region"
          aria-label="New AI editing default"
          data-testid="ai-mode-migrated"
          className="space-y-3 rounded-md border border-info/40 bg-info-subtle p-3 text-sm text-info-subtle-fg"
        >
          <p>{MIGRATED_NOTICE}</p>
          {canOperate ? (
            <div className="flex flex-wrap justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={save.isPending || !mayChoose("ask")}
                onClick={() => submit("ask", m.version)}
              >
                {KEEP_ASKING}
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={save.isPending || !mayChoose("ai_drafts")}
                onClick={() => submit("ai_drafts", m.version)}
              >
                {KEEP_AUTO}
              </Button>
            </div>
          ) : (
            <p>{NOT_OPERATOR_LINE}</p>
          )}
        </div>
      ) : null}

      {!m.setter_valid ? (
        <p role="status" data-testid="ai-mode-setter-invalid" className="rounded-md border border-warning/40 bg-warning-subtle px-3 py-2 text-sm text-warning-subtle-fg">
          {SETTER_INVALID_LINE}
        </p>
      ) : null}

      <form
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          if (canSave && selected !== "full") submit(selected, choice?.version ?? m.version);
        }}
        className="space-y-3"
      >
        <fieldset className="space-y-2">
          <legend className="text-sm font-medium text-foreground">{MODE_HEADING}</legend>
          <p className="text-xs text-muted-foreground">{MODE_SUBHEADING}</p>
          {CHOOSABLE.map((mode) => {
            const opt = option(mode);
            const reason = opt ? optionReasonLine(opt, m.min_agent_version) : null;
            const disabled = !mayChoose(mode) || save.isPending;
            const reasonId = `${groupName}-${mode}-reason`;
            return (
              <label
                key={mode}
                className={cn(
                  "flex items-start gap-3 rounded-md border border-border px-3 py-2",
                  disabled ? "cursor-not-allowed opacity-80" : "cursor-pointer hover:bg-muted/40",
                  selected === mode && "border-ring bg-muted/20",
                )}
              >
                <input
                  type="radio"
                  name={groupName}
                  value={mode}
                  checked={selected === mode}
                  disabled={disabled}
                  aria-describedby={reason && canOperate ? reasonId : undefined}
                  onChange={() => setChoice({ mode, version: choice?.version ?? m.version })}
                  className="mt-1 shrink-0 accent-primary"
                />
                <span className="min-w-0 space-y-0.5">
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="text-sm font-medium text-foreground">{MODE_NAME[mode]}</span>
                    {mode === "ai_drafts" ? <Badge variant="muted">Default</Badge> : null}
                  </span>
                  <span className="block text-xs text-muted-foreground">{MODE_DESCRIPTION[mode]}</span>
                  {reason && canOperate ? (
                    <span id={reasonId} className="block text-xs text-muted-foreground">
                      {reason}
                    </span>
                  ) : null}
                </span>
              </label>
            );
          })}
          <label className="flex cursor-not-allowed items-start gap-3 rounded-md border border-dashed border-border px-3 py-2 opacity-80">
            <input
              type="radio"
              name={groupName}
              value="full"
              checked={selected === "full"}
              disabled
              readOnly
              className="mt-1 shrink-0"
            />
            <span className="min-w-0 space-y-0.5">
              <span className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-medium text-foreground">{MODE_NAME.full}</span>
                <Badge variant="outline">{FULL_AUTO_LATER}</Badge>
              </span>
              <span className="block text-xs text-muted-foreground">{MODE_DESCRIPTION.full}</span>
            </span>
          </label>
        </fieldset>

        <p className="text-xs text-muted-foreground">{ALWAYS_WAITS_LINE}</p>

        {!canOperate ? <p className="text-xs text-muted-foreground">{NOT_OPERATOR_LINE}</p> : null}

        {save.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {saveErrorLine(save.error, m.min_agent_version)}
          </p>
        ) : null}

        <div className="flex flex-wrap items-center justify-between gap-2">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-expanded={showCovers}
            aria-controls={coversId}
            onClick={() => setShowCovers((v) => !v)}
          >
            What each setting covers
          </Button>
          {canOperate ? (
            <div className="flex flex-wrap gap-2">
              <Button
                type="button"
                variant="outline"
                disabled={!dirty || save.isPending}
                onClick={() => setChoice(null)}
              >
                Keep current
              </Button>
              <Button type="submit" disabled={!canSave}>
                {save.isPending ? "Saving…" : "Save change"}
              </Button>
            </div>
          ) : null}
        </div>

        {showCovers ? (
          <div id={coversId} className="overflow-x-auto" data-testid="ai-mode-covers">
            <table className="w-full text-left text-xs">
              <thead>
                <tr className="text-muted-foreground">
                  <th scope="col" className="py-1 pr-3 font-medium">
                    Kind of change
                  </th>
                  {modeColumns.map((mode) => (
                    <th key={mode} scope="col" className="py-1 pr-3 font-medium">
                      {MODE_NAME[mode]}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {m.kinds.map((k) => (
                  <tr key={k.change_class} className="border-t border-border align-top">
                    <th scope="row" className="py-1.5 pr-3 font-normal text-foreground">
                      <span className="block">{k.name}</span>
                      {k.abilities.length > 0 ? (
                        <span className="block text-muted-foreground">
                          {k.abilities.map((a) => a.title).join(", ")}
                        </span>
                      ) : null}
                    </th>
                    {modeColumns.map((mode) => {
                      const outcome = k.decisions.find((d) => d.mode === mode)?.outcome;
                      return (
                        <td key={mode} className="py-1.5 pr-3 text-foreground">
                          {outcome === "auto" ? "Runs at once" : outcome === "ask" ? "Waits for you" : "Not stated"}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </form>

      {footer ? (
        <p data-testid="ai-mode-set-by" className="text-xs text-muted-foreground">
          {footer}
        </p>
      ) : null}
    </div>
  );
}
