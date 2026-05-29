import { useState } from "react";

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
import { relativeTime } from "@/lib/utils";
import type { PhpError } from "@wpmgr/api";

// Detail "drawer" for one fingerprint. The UI primitive library does not yet
// ship a Drawer/Sheet — we render the existing Dialog wider so the file path
// + message + request_path don't have to wrap aggressively. The spec
// references a drawer but the visual goal is just "modal containing the
// detail body"; substituting Dialog keeps Sprint 2 self-contained.
//
// Actions: silence, copy fingerprint. Backtrace surface is reserved for when
// the agent ships compressed backtraces (BLOB column is in place; ErrorMonitor
// currently writes NULL until a backtrace-capture sprint lands).

export interface ErrorDetailDrawerProps {
  error: PhpError | null;
  onClose: () => void;
  onSilence: (silenced: boolean) => void;
}

export function ErrorDetailDrawer({
  error,
  onClose,
  onSilence,
}: ErrorDetailDrawerProps) {
  const [copied, setCopied] = useState(false);
  if (!error) return null;

  const copy = () => {
    void navigator.clipboard.writeText(error.md5).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <Dialog open={true} onClose={onClose}>
      <DialogContent
        ariaLabelledBy="error-detail-title"
        className="max-w-[720px]"
      >
        <DialogHeader>
          <DialogTitle id="error-detail-title">
            <span className="font-mono text-sm">{error.severity}</span>{" "}
            <span className="text-base">{shortMessage(error.message)}</span>
          </DialogTitle>
          <DialogDescription>
            First seen {relativeTime(error.first_seen_at) ?? "recently"}, last
            seen {relativeTime(error.last_seen_at) ?? "recently"}, occurred{" "}
            <span className="font-medium tabular-nums">
              {error.occurrence_count}
            </span>{" "}
            times.
          </DialogDescription>
        </DialogHeader>

        <DialogBody>
          <Field label="Fingerprint" value={error.md5} mono />
          <Field label="File" value={error.file || "—"} mono />
          <Field
            label="Line"
            value={error.line > 0 ? String(error.line) : "—"}
            mono
          />
          <Field label="Code" value={String(error.code)} mono />
          <Field label="Request path" value={error.request_path || "—"} mono />
          <Field
            label="Message"
            value={<pre className="whitespace-pre-wrap break-words font-mono text-xs">{error.message}</pre>}
          />
        </DialogBody>

        <DialogFooter>
          <Button
            type="button"
            variant="ghost"
            onClick={copy}
            aria-label="Copy fingerprint"
          >
            {copied ? "Copied" : "Copy fingerprint"}
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={() => onSilence(!error.silenced)}
          >
            {error.silenced ? "Unsilence" : "Silence"}
          </Button>
          <Button type="button" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Field({
  label,
  value,
  mono,
}: {
  label: string;
  value: React.ReactNode;
  mono?: boolean;
}) {
  return (
    <div className="grid grid-cols-[120px_1fr] gap-2 text-sm">
      <div className="text-muted-foreground">{label}</div>
      <div className={mono ? "font-mono break-all" : "break-words"}>
        {value}
      </div>
    </div>
  );
}

function shortMessage(m: string): string {
  if (m.length <= 80) return m;
  return m.slice(0, 80) + "…";
}
