import { useId, useState } from "react";
import { toast } from "sonner";
import type { AiAuto, AiConnectionUsage } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";

import {
  AUTO_CHOICE_NAME,
  AUTO_KEY_MINTED_LINE,
  AUTO_LIMITS_LINE,
  AUTO_QUESTION,
  AUTO_REVOKED_LINE,
  AUTO_SAVE_FAILED_LINE,
  AUTO_SETTER_INVALID_LINE,
  PAUSED_LINE,
  draftUsageLine,
} from "./ai-trust-copy";
import { CODE_PAUSED, useAiConnectionUsage, usePutAiConnectionAuto, type AiTrustError } from "./use-ai-trust";

// One AI connection's automatic changes (design §8.7): read-only usage against
// the limits WPMgr sets, and the one switch. The switch is offered to owners
// and admins only, the people the server lets set it; anyone else sees the
// usage alone. Saving "Where each site allows it" records the person saving as
// the one who allowed it, which is how a connection whose person lost access
// is allowed again.

const CHOICES: readonly AiAuto[] = ["site_setting", "never"];

export interface ConnectionAutoPanelProps {
  grantId: string;
  connectionName: string;
  revoked: boolean;
  /** Owner or admin with full membership: the switch's permission. */
  canManage: boolean;
}

export function ConnectionAutoPanel({ grantId, connectionName, revoked, canManage }: ConnectionAutoPanelProps) {
  const usage = useAiConnectionUsage(grantId);
  const headingId = useId();

  return (
    <section aria-labelledby={headingId} className="space-y-3 text-sm" data-testid="connection-auto-panel">
      <h3 id={headingId} className="font-medium text-foreground">
        Automatic changes: <span className="break-words">{connectionName}</span>
      </h3>
      {usage.isPending ? (
        <Skeleton aria-label="Loading this connection's usage" className="h-10 w-full" />
      ) : usage.isError ? (
        <PageError
          what="Could not load this connection's usage."
          why={usage.error.message}
          onRetry={() => void usage.refetch()}
          isRetrying={usage.isFetching}
        />
      ) : (
        <div className="space-y-1">
          <p data-testid="connection-draft-usage" className="text-foreground">
            {draftUsageLine(usage.data)}
          </p>
          <p className="text-muted-foreground">{AUTO_LIMITS_LINE}</p>
        </div>
      )}
      {revoked ? (
        <p data-testid="connection-auto-revoked" className="text-muted-foreground">
          {AUTO_REVOKED_LINE}
        </p>
      ) : canManage && !usage.isPending ? (
        // The switch waits for the read to settle, so it never opens with
        // nothing chosen. When the read failed it still works: the stored value
        // is unknown there, so any choice can be saved.
        <AutoSwitch grantId={grantId} usage={usage.isSuccess ? usage.data : null} />
      ) : null}
    </section>
  );
}

function saveErrorLine(err: AiTrustError): string {
  return err.code === CODE_PAUSED ? PAUSED_LINE : AUTO_SAVE_FAILED_LINE;
}

function AutoSwitch({ grantId, usage }: { grantId: string; usage: AiConnectionUsage | null }) {
  const save = usePutAiConnectionAuto(grantId);
  const [choice, setChoice] = useState<AiAuto | null>(null);
  const groupName = useId();
  const noteId = useId();
  const stored = usage?.ai_auto ?? null;
  const selected = choice ?? stored;
  const setterInvalid = usage !== null && !usage.auto_setter_valid;
  const keyMinted = usage !== null && usage.created_with_api_key && usage.ai_auto === "never";
  // A lapsed permission is saved again unchanged to restore it, so Save stays
  // enabled for that case even when the choice equals the stored value.
  const canSave = selected !== null && (selected !== stored || setterInvalid) && !save.isPending;
  const hasNote = keyMinted || setterInvalid;

  return (
    <form
      noValidate
      className="space-y-2"
      onSubmit={(e) => {
        e.preventDefault();
        if (!canSave || selected === null) return;
        save.mutate(selected, {
          onSuccess: (saved) => {
            setChoice(null);
            toast.success(`Saved. ${AUTO_CHOICE_NAME[saved.ai_auto]}.`);
          },
        });
      }}
    >
      <fieldset className="space-y-2" aria-describedby={hasNote ? noteId : undefined}>
        <legend className="font-medium text-foreground">{AUTO_QUESTION}</legend>
        <div className="flex flex-wrap items-center gap-x-6 gap-y-2">
          {CHOICES.map((value) => (
            <label key={value} className="flex cursor-pointer items-center gap-2">
              <input
                type="radio"
                name={groupName}
                value={value}
                checked={selected === value}
                disabled={save.isPending}
                onChange={() => {
                  save.reset();
                  setChoice(value);
                }}
                className="shrink-0 accent-primary"
              />
              <span className="text-foreground">{AUTO_CHOICE_NAME[value]}</span>
            </label>
          ))}
          <Button type="submit" size="sm" disabled={!canSave}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </fieldset>
      {hasNote ? (
        <div id={noteId} className="space-y-1">
          {keyMinted ? (
            <p data-testid="connection-auto-key-minted" className="text-muted-foreground">
              {AUTO_KEY_MINTED_LINE}
            </p>
          ) : null}
          {setterInvalid ? (
            <p role="status" data-testid="connection-auto-setter-invalid" className="text-warning-subtle-fg">
              {AUTO_SETTER_INVALID_LINE}
            </p>
          ) : null}
        </div>
      ) : null}
      {save.isError ? (
        <p role="alert" data-testid="connection-auto-save-failed" className="text-destructive">
          {saveErrorLine(save.error)}
        </p>
      ) : null}
    </form>
  );
}
