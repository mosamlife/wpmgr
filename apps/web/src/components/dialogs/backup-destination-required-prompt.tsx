import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { planLabel } from "@/features/billing/plan-catalog";

// BackupDestinationRequiredPrompt — the ONE consistent surface for the 402
// byo_destination_required shape (see lib/api.ts's
// extractByoDestinationRequired, the sibling of the site-limit interceptor).
// A manual backup run that resolves to CP-managed storage on a plan that
// doesn't permit it renders this instead of a raw error string.
//
// Any operator can add a destination themselves (the /destinations page has
// no owner gate), so that action is always offered. Only the billing action
// is owner-only + hosted-only, mirroring UpgradePrompt's canUpgrade gate.

export interface BackupDestinationRequiredPromptProps {
  open: boolean;
  onClose: () => void;
  plan: string;
  /** True only for an owner on a hosted instance — the only principal who can act on billing. */
  canUpgrade: boolean;
}

export function BackupDestinationRequiredPrompt({
  open,
  onClose,
  plan,
  canUpgrade,
}: BackupDestinationRequiredPromptProps) {
  return (
    <Dialog open={open} onClose={onClose}>
      {open ? (
        <DialogContent ariaLabelledBy="backup-destination-required-title">
          <DialogHeader>
            <DialogTitle id="backup-destination-required-title">
              Add a backup destination
            </DialogTitle>
          </DialogHeader>

          <DialogBody>
            <p className="text-sm text-[var(--color-foreground)]">
              Your {planLabel(plan)} plan does not include managed backup
              storage. Add a local folder or your own S3-compatible bucket
              under Destinations to run backups, or upgrade your plan.
            </p>
            {!canUpgrade ? (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                Ask your organisation owner to upgrade.
              </p>
            ) : null}
          </DialogBody>

          <DialogFooter className="pt-2">
            <Button type="button" variant="outline" onClick={onClose}>
              Close
            </Button>
            <Button asChild variant="outline" onClick={onClose}>
              <Link to="/destinations">Add destination</Link>
            </Button>
            {canUpgrade ? (
              <Button asChild onClick={onClose}>
                <Link to="/settings/billing">View plans</Link>
              </Button>
            ) : null}
          </DialogFooter>
        </DialogContent>
      ) : null}
    </Dialog>
  );
}
