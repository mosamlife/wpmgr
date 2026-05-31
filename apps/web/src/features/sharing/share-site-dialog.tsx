import { useId, useRef, useState } from "react";
import { Check, Copy, Loader2, Trash2, Users } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusChip } from "@/components/status";
import { toast } from "@/components/toast";
import { relativeTime } from "@/lib/utils";
import {
  useSiteShares,
  useCreateShare,
  useRevokeShare,
  type ShareRole,
} from "@/features/sharing/use-shares";

// ShareSiteDialog — the "Share" action for a site.
//
// Shows the share form (email + role + optional expiry) and the current
// collaborators list (GET /sites/{siteId}/shares) with a Revoke button.
//
// On a 202 response (new user → invite created), the accept link is shown in a
// read-only input with a "Copy invite link" button. This is the primary sharing
// mechanism per the spec: the inviter copies the link and sends it directly;
// email delivery is a future addon.

interface ShareSiteDialogProps {
  siteId: string;
  siteName: string;
  open: boolean;
  onClose: () => void;
}

export function ShareSiteDialog({
  siteId,
  siteName,
  open,
  onClose,
}: ShareSiteDialogProps) {
  const titleId = useId();
  const { data: shares, isPending: sharesLoading } = useSiteShares(siteId);
  const createShare = useCreateShare(siteId);
  const revokeShare = useRevokeShare(siteId);

  const [email, setEmail] = useState("");
  const [role, setRole] = useState<ShareRole>("viewer");
  const [expiresAt, setExpiresAt] = useState("");
  const [formError, setFormError] = useState<string | null>(null);

  // Invite link state — shown once after a 202.
  const [inviteLink, setInviteLink] = useState<string | null>(null);
  const [linkCopied, setLinkCopied] = useState(false);
  const linkInputRef = useRef<HTMLInputElement>(null);

  function resetForm() {
    setEmail("");
    setRole("viewer");
    setExpiresAt("");
    setFormError(null);
  }

  function handleClose() {
    resetForm();
    setInviteLink(null);
    setLinkCopied(false);
    onClose();
  }

  async function handleShare() {
    const trimmedEmail = email.trim();
    if (!trimmedEmail) {
      setFormError("Email is required");
      return;
    }
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmedEmail)) {
      setFormError("Enter a valid email address");
      return;
    }
    setFormError(null);
    setInviteLink(null);

    try {
      const result = await createShare.mutateAsync({
        email: trimmedEmail,
        role,
        expires_at: expiresAt || null,
      });

      resetForm();

      if (result.status === 202 && result.accept_link) {
        // New user — show the one-time invite link.
        setInviteLink(result.accept_link);
        toast.success("Invitation created — copy the link below to share it.");
      } else {
        toast.success(`${trimmedEmail} has been added as a collaborator.`);
      }
    } catch (err) {
      setFormError(
        err instanceof Error ? err.message : "Could not create share",
      );
    }
  }

  function copyInviteLink() {
    if (!inviteLink) return;
    void navigator.clipboard.writeText(inviteLink).then(() => {
      setLinkCopied(true);
      window.setTimeout(() => setLinkCopied(false), 2000);
      toast.success("Invite link copied to clipboard");
    });
  }

  async function handleRevoke(userId: string, displayName: string) {
    try {
      await revokeShare.mutateAsync(userId);
    } catch {
      toast.error(`Could not revoke access for ${displayName}`);
    }
  }

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent
        ariaLabelledBy={titleId}
        className="max-w-[min(560px,calc(100vw-2rem))]"
      >
        <DialogHeader>
          <DialogTitle id={titleId}>Share "{siteName}"</DialogTitle>
        </DialogHeader>

        <DialogBody className="space-y-6">
          {/* Share form */}
          <div className="space-y-4 rounded-lg border border-[var(--color-border)] p-4">
            <div className="space-y-2">
              <Label htmlFor="share-email">Email address</Label>
              <Input
                id="share-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="collaborator@example.com"
                data-autofocus
                aria-invalid={formError ? true : undefined}
                disabled={createShare.isPending}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void handleShare();
                }}
              />
            </div>

            <div className="flex gap-3">
              <div className="flex-1 space-y-2">
                <Label htmlFor="share-role">Role</Label>
                <Select
                  id="share-role"
                  value={role}
                  onChange={(e) => setRole(e.target.value as ShareRole)}
                  disabled={createShare.isPending}
                >
                  <option value="viewer">Viewer</option>
                  <option value="operator">Operator</option>
                  <option value="admin">Admin</option>
                </Select>
              </div>

              <div className="flex-1 space-y-2">
                <Label htmlFor="share-expires">Expires (optional)</Label>
                <DateTimePicker
                  id="share-expires"
                  value={expiresAt}
                  onChange={setExpiresAt}
                  min={new Date().toISOString()}
                  disabled={createShare.isPending}
                />
                <p className="text-xs text-muted-foreground">
                  Leave empty for a durable share. The access auto-revokes at the
                  chosen date &amp; time (your local timezone).
                </p>
              </div>
            </div>

            {formError ? (
              <p role="alert" className="text-sm text-[var(--color-destructive)]">
                {formError}
              </p>
            ) : null}

            <Button
              type="button"
              onClick={() => void handleShare()}
              disabled={createShare.isPending || !email.trim()}
              className="w-full"
            >
              {createShare.isPending ? (
                <>
                  <Loader2 aria-hidden="true" className="animate-spin" />
                  <span>Sharing…</span>
                </>
              ) : (
                "Share"
              )}
            </Button>
          </div>

          {/* Invite link — shown after a 202 (new user invited). */}
          {inviteLink ? (
            <div
              role="alert"
              aria-live="polite"
              className="space-y-2 rounded-lg border border-[var(--color-primary)]/30 bg-[var(--color-primary)]/5 p-4"
            >
              <p className="text-sm font-medium text-[var(--color-foreground)]">
                Invite link created
              </p>
              <p className="text-sm text-[var(--color-muted-foreground)]">
                This link is shown once. Copy and share it with the invitee.
              </p>
              <div className="flex gap-2">
                <Input
                  ref={linkInputRef}
                  readOnly
                  value={inviteLink}
                  className="font-mono text-xs"
                  onFocus={(e) => e.target.select()}
                  aria-label="Invite link"
                />
                <Button
                  type="button"
                  variant="outline"
                  onClick={copyInviteLink}
                  aria-label="Copy invite link"
                  className="shrink-0 gap-1.5"
                >
                  {linkCopied ? (
                    <>
                      <Check aria-hidden="true" className="size-4" />
                      Copied
                    </>
                  ) : (
                    <>
                      <Copy aria-hidden="true" className="size-4" />
                      Copy invite link
                    </>
                  )}
                </Button>
              </div>
            </div>
          ) : null}

          {/* Collaborators list */}
          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Users aria-hidden="true" className="size-4 text-muted-foreground" />
              <h3 className="text-sm font-medium">
                Collaborators
                {shares && shares.length > 0 ? (
                  <span className="ml-1 text-muted-foreground">
                    ({shares.length})
                  </span>
                ) : null}
              </h3>
            </div>

            {sharesLoading ? (
              <p role="status" className="text-sm text-muted-foreground">
                Loading collaborators…
              </p>
            ) : !shares || shares.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No collaborators yet.
              </p>
            ) : (
              <div className="rounded-lg border border-[var(--color-border)]">
                <Table>
                  <caption className="sr-only">
                    Collaborators for {siteName}
                  </caption>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Email</TableHead>
                      <TableHead>Role</TableHead>
                      <TableHead>Expires</TableHead>
                      <TableHead className="sr-only">Actions</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {shares.map((share) => {
                      const display = share.email ?? share.user_id;
                      const isExpired =
                        share.expires_at != null &&
                        new Date(share.expires_at) < new Date();
                      return (
                        <TableRow key={share.id}>
                          <TableCell className="text-sm font-medium">
                            {display}
                          </TableCell>
                          <TableCell className="capitalize text-sm">
                            {share.role}
                          </TableCell>
                          <TableCell className="text-sm text-muted-foreground">
                            {share.expires_at ? (
                              isExpired ? (
                                <StatusChip tone="destructive" label="Expired" />
                              ) : (
                                relativeTime(share.expires_at) ?? share.expires_at
                              )
                            ) : (
                              <span className="text-muted-foreground">Never</span>
                            )}
                          </TableCell>
                          <TableCell className="text-right">
                            <Button
                              type="button"
                              variant="outline"
                              size="sm"
                              disabled={revokeShare.isPending}
                              onClick={() =>
                                void handleRevoke(share.user_id, display)
                              }
                              aria-label={`Revoke access for ${display}`}
                            >
                              <Trash2 aria-hidden="true" className="size-3.5" />
                              Revoke
                            </Button>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            )}
          </div>
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" onClick={handleClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
