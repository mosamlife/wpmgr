import { useId, useState } from "react";
import { Loader2 } from "lucide-react";

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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  MONITORING_PAUSE_CONTINUES,
  MONITORING_PAUSE_STOPS,
} from "@/features/sites/monitoring-pause";

// GH #414 phase 4b — the pause confirmation.
//
// NOT DestructiveConfirm: a pause is undone by a resume, so demanding the
// operator type a resource name would be friction over a reversible act. What
// this dialog owes them instead is SCOPE, which is the thing they cannot
// derive from the button label.

export interface PauseMonitoringDialogProps {
  open: boolean;
  onClose: () => void;
  /** Called with the operator's optional free-text reason. */
  onConfirm: (reason: string) => void | Promise<void>;
  /** How many sites this pause will touch. */
  count: number;
  isPending?: boolean;
  errorMessage?: string | null;
}

export function PauseMonitoringDialog({
  open,
  onClose,
  onConfirm,
  count,
  isPending = false,
  errorMessage,
}: PauseMonitoringDialogProps) {
  const titleId = useId();
  const descriptionId = useId();
  const reasonId = useId();
  const [reason, setReason] = useState("");

  const [prevOpen, setPrevOpen] = useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) setReason("");
  }

  const noun = count === 1 ? "site" : "sites";

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy={titleId} ariaDescribedBy={descriptionId}>
        <DialogHeader>
          <DialogTitle id={titleId}>
            Pause monitoring on {count} {noun}
          </DialogTitle>
          <DialogDescription id={descriptionId}>
            You stop hearing about these {noun}. They keep being looked after.
          </DialogDescription>
        </DialogHeader>

        <DialogBody>
          {/*
            THE SENTENCE. Someone pausing before a migration reasonably assumes
            everything stops, and backups silently stopping is the one failure
            people do not recover from. So the scope is stated in full, on both
            sides, in plain words, before the button is pressed rather than in
            a support ticket six weeks later.
          */}
          <div className="space-y-2 rounded-md border border-border bg-muted/40 p-3 text-sm">
            <p>
              <span className="font-medium">Stops:</span>{" "}
              {MONITORING_PAUSE_STOPS}
            </p>
            <p>
              <span className="font-medium">Keeps running:</span>{" "}
              {MONITORING_PAUSE_CONTINUES}
            </p>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor={reasonId}>Reason (optional)</Label>
            <Input
              id={reasonId}
              value={reason}
              maxLength={500}
              placeholder="Migrating to the new host"
              onChange={(e) => setReason(e.target.value)}
              disabled={isPending}
            />
            <p className="text-xs text-muted-foreground">
              Shown on every paused site, so whoever finds it next knows why.
            </p>
          </div>

          {errorMessage ? (
            <p role="alert" className="text-sm text-destructive">
              {errorMessage}
            </p>
          ) : null}
        </DialogBody>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={isPending}
          >
            Keep monitoring
          </Button>
          <Button
            type="button"
            onClick={() => void onConfirm(reason.trim())}
            disabled={isPending}
          >
            {isPending ? (
              <Loader2 aria-hidden="true" className="size-4 animate-spin" />
            ) : null}
            Pause monitoring
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
