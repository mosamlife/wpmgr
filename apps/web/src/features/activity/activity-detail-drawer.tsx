import { useState } from "react";
import { ShieldCheck, ShieldAlert, Copy, Check } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn, relativeTime } from "@/lib/utils";
import type { SiteActivityEvent } from "@wpmgr/api";

import { SeverityChip } from "./severity-chip";

// Detail "drawer" for one activity event (ADR-037 redesign).
//
// The UI library ships no Sheet primitive yet, so (matching the Sprint 2 errors
// drawer) we render the shared Dialog wider. For forensic use the body now
// surfaces EVERYTHING: the summary headline, a full definition list, the raw
// meta as pretty-printed scrollable JSON, and the hash chain (prev + this) with
// per-hash copy buttons and a chain_valid badge. Tokens only; mono for hashes /
// IPs / object ids / event_type; no em-dashes.

export interface ActivityDetailDrawerProps {
  event: SiteActivityEvent | null;
  onClose: () => void;
}

export function ActivityDetailDrawer({
  event,
  onClose,
}: ActivityDetailDrawerProps) {
  const [copied, setCopied] = useState<string | null>(null);
  if (!event) return null;

  const copy = (label: string, value: string) => {
    void navigator.clipboard.writeText(value).then(() => {
      setCopied(label);
      window.setTimeout(() => setCopied(null), 1500);
    });
  };

  const hasActor = event.actor_login !== "" && event.actor_user_id !== 0;
  const rel = relativeTime(event.occurred_at) ?? "just now";
  const placeholder = "not set";

  return (
    <Dialog open={true} onClose={onClose}>
      <DialogContent
        ariaLabelledBy="activity-detail-title"
        className="max-w-[720px]"
      >
        <DialogHeader>
          <DialogTitle id="activity-detail-title">
            <span className="flex flex-wrap items-center gap-2">
              <SeverityChip severity={event.severity} />
              <span className="text-sm text-foreground">{event.summary}</span>
            </span>
          </DialogTitle>
          <DialogDescription>
            <span className="tabular-nums">{rel}</span>
            <span aria-hidden="true"> · </span>
            <span className="font-mono tabular-nums">{event.occurred_at}</span>
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-5">
          <ChainBadge valid={event.chain_valid} />

          <DefinitionList>
            <Field
              label="Event type"
              value={event.event_type}
              mono
            />
            <Field label="Object type" value={event.object_type} />
            <Field
              label="Object label"
              value={event.object_label || placeholder}
            />
            <Field
              label="Object id"
              value={event.object_id || placeholder}
              mono
            />
            <Field
              label="Actor"
              value={hasActor ? event.actor_login : "system"}
            />
            <Field
              label="Actor user id"
              value={String(event.actor_user_id)}
              mono
            />
            <Field
              label="Actor IP"
              value={event.actor_ip || placeholder}
              mono
            />
            <Field label="Sequence" value={String(event.seq)} mono />
            <Field
              label="Occurred at"
              value={event.occurred_at}
              mono
            />
            <Field
              label="Received at"
              value={event.received_at}
              mono
            />
          </DefinitionList>

          <Section title="Metadata">
            <pre className="max-h-64 overflow-auto rounded-md border border-border bg-muted/50 p-3 font-mono text-xs text-foreground">
              {JSON.stringify(event.meta, null, 2)}
            </pre>
          </Section>

          <Section title="Hash chain">
            <div className="space-y-2">
              <HashRow
                label="Prev hash"
                value={event.prev_hash}
                copied={copied === "prev"}
                onCopy={() => copy("prev", event.prev_hash)}
              />
              <HashRow
                label="This hash"
                value={event.this_hash}
                copied={copied === "this"}
                onCopy={() => copy("this", event.this_hash)}
              />
            </div>
          </Section>
        </DialogBody>

        <DialogFooter>
          <Button type="button" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ChainBadge({ valid }: { valid: boolean }) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded px-2.5 py-1 text-xs font-medium",
        valid
          ? "bg-success-subtle text-success-subtle-fg"
          : "bg-destructive-subtle text-destructive-subtle-fg",
      )}
    >
      {valid ? (
        <ShieldCheck aria-hidden="true" className="size-3.5" />
      ) : (
        <ShieldAlert aria-hidden="true" className="size-3.5" />
      )}
      {valid ? "Chain link verified" : "Chain link broken"}
    </span>
  );
}

function DefinitionList({ children }: { children: React.ReactNode }) {
  return <dl className="space-y-2">{children}</dl>;
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-2">
      <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </h3>
      {children}
    </div>
  );
}

function Field({
  label,
  value,
  mono,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="grid grid-cols-[140px_1fr] gap-3 text-sm">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={cn("min-w-0 break-words text-foreground", mono && "font-mono tabular-nums")}>
        {value}
      </dd>
    </div>
  );
}

function HashRow({
  label,
  value,
  copied,
  onCopy,
}: {
  label: string;
  value: string;
  copied: boolean;
  onCopy: () => void;
}) {
  return (
    <div className="grid grid-cols-[140px_1fr] items-start gap-3 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <div className="flex items-start gap-2">
        <code className="min-w-0 flex-1 break-all rounded bg-muted/50 px-2 py-1 font-mono text-xs text-foreground">
          {value}
        </code>
        <Button
          size="sm"
          variant="ghost"
          type="button"
          onClick={onCopy}
          aria-label={`Copy ${label}`}
        >
          {copied ? (
            <>
              <Check aria-hidden="true" className="size-3.5" />
              Copied
            </>
          ) : (
            <>
              <Copy aria-hidden="true" className="size-3.5" />
              Copy
            </>
          )}
        </Button>
      </div>
    </div>
  );
}
