import { useId, useState } from "react";
import { Loader2, RefreshCw, Trash2, Users } from "lucide-react";

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
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusChip } from "@/components/status";
import type { StatusTone } from "@/components/status/status-dot";
import { toast } from "@/components/toast";
import { relativeTime } from "@/lib/utils";
import { useMe } from "@/features/auth/use-auth";
import { InviteLinkReveal } from "@/features/sharing/invite-link-reveal";
import {
  useSiteShares,
  useCreateShare,
  useRevokeShare,
  useSiteInvitations,
  useRevokeInvitation,
  useRegenerateInvite,
  type ShareRole,
  type SiteInvitation,
  type InvitationStatus,
} from "@/features/sharing/use-shares";

// ShareSiteDialog — the "Share" hub for a site, organised as three tabs:
//   • Invite        — add a collaborator by email (existing form + reveal).
//   • Collaborators — live site_shares (revoke).
//   • Pending invites — invite-link history (regenerate / cancel), since the
//     raw accept link is shown once and otherwise lost. Regenerate rotates the
//     token (old link dies) and surfaces a fresh one-time reveal.
//
// Destructive actions use a lighter inline confirm (not the type-to-confirm
// modal) to keep these low-stakes actions one extra click, not punitive.

interface ShareSiteDialogProps {
  siteId: string;
  siteName: string;
  open: boolean;
  onClose: () => void;
}

type ShareTab = "invite" | "people" | "pending";

type Confirm =
  | { kind: "revoke-share"; userId: string; display: string }
  | { kind: "cancel-invite"; invitation: SiteInvitation }
  | { kind: "regenerate"; invitation: SiteInvitation }
  | null;

const STATUS_TONE: Record<InvitationStatus, StatusTone> = {
  pending: "warning",
  accepted: "success",
  expired: "muted",
  revoked: "destructive",
};

const STATUS_LABEL: Record<InvitationStatus, string> = {
  pending: "Pending",
  accepted: "Accepted",
  expired: "Expired",
  revoked: "Revoked",
};

export function ShareSiteDialog({
  siteId,
  siteName,
  open,
  onClose,
}: ShareSiteDialogProps) {
  const titleId = useId();
  const { data: me } = useMe();
  const { data: shares, isPending: sharesLoading } = useSiteShares(siteId);
  const { data: invitations, isPending: invitesLoading } =
    useSiteInvitations(siteId);
  const createShare = useCreateShare(siteId);
  const revokeShare = useRevokeShare(siteId);
  const revokeInvite = useRevokeInvitation(siteId);
  const regenerate = useRegenerateInvite(siteId);

  const [tab, setTab] = useState<ShareTab>("invite");

  // Invite form state.
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<ShareRole>("viewer");
  const [expiresAt, setExpiresAt] = useState("");
  const [formError, setFormError] = useState<string | null>(null);

  // Reveal state — one surface for fresh invites and rotated links.
  const [inviteLink, setInviteLink] = useState<string | null>(null);
  const [rotated, setRotated] = useState<{ link: string; email: string } | null>(
    null,
  );

  // Inline-confirm state for destructive actions.
  const [confirm, setConfirm] = useState<Confirm>(null);

  function resetForm() {
    setEmail("");
    setRole("viewer");
    setExpiresAt("");
    setFormError(null);
  }

  function clearTransient() {
    setInviteLink(null);
    setRotated(null);
    setConfirm(null);
  }

  function handleTabChange(next: string) {
    clearTransient();
    setTab(next as ShareTab);
  }

  function handleClose() {
    resetForm();
    clearTransient();
    setTab("invite");
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

  async function confirmAction() {
    if (!confirm) return;
    try {
      if (confirm.kind === "revoke-share") {
        await revokeShare.mutateAsync(confirm.userId);
        setConfirm(null);
      } else if (confirm.kind === "cancel-invite") {
        await revokeInvite.mutateAsync(confirm.invitation.id);
        setConfirm(null);
      } else if (confirm.kind === "regenerate") {
        const inv = confirm.invitation;
        const link = await regenerate.mutateAsync(inv.id);
        setConfirm(null);
        if (link) {
          setRotated({ link, email: inv.email });
          toast.success(
            `New link created for ${inv.email} — the old one no longer works.`,
          );
        }
      }
    } catch {
      // Mutations surface their own error toasts; close the confirm.
      setConfirm(null);
    }
  }

  const busy =
    revokeShare.isPending || revokeInvite.isPending || regenerate.isPending;

  // Summary header counts.
  const collaboratorCount = shares?.length ?? 0;
  const pendingCount =
    invitations?.filter((i) => i.status === "pending").length ?? 0;
  const expiredCount =
    invitations?.filter((i) => i.status === "expired").length ?? 0;
  // Accepted invites graduate to the Collaborators tab — the "Pending invites"
  // link history only shows still-actionable links (pending / expired / revoked).
  const historyInvites = (invitations ?? []).filter(
    (i) => i.status !== "accepted",
  );
  const summarySegments = [
    `${collaboratorCount} ${collaboratorCount === 1 ? "collaborator" : "collaborators"}`,
    `${pendingCount} pending`,
    ...(expiredCount > 0 ? [`${expiredCount} expired`] : []),
  ];

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent
        ariaLabelledBy={titleId}
        className="max-w-[min(600px,calc(100vw-2rem))]"
      >
        <DialogHeader>
          <DialogTitle id={titleId}>Share "{siteName}"</DialogTitle>
          <p className="text-xs text-muted-foreground">
            {summarySegments.join(" · ")}
          </p>
        </DialogHeader>

        <DialogBody>
          <Tabs value={tab} onValueChange={handleTabChange}>
            <TabsList aria-label={`Sharing options for ${siteName}`}>
              <TabsTrigger value="invite">Invite</TabsTrigger>
              <TabsTrigger value="people">
                Collaborators ({collaboratorCount})
              </TabsTrigger>
              <TabsTrigger value="pending">
                Pending invites ({historyInvites.length})
              </TabsTrigger>
            </TabsList>

            {/* ── Invite tab ─────────────────────────────────────────────── */}
            <TabsContent value="invite" className="space-y-4 pt-4">
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
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                Leave expiry empty for a durable share. Access auto-revokes at
                the chosen date &amp; time (your local timezone).
              </p>

              {formError ? (
                <p
                  role="alert"
                  className="text-sm text-[var(--color-destructive)]"
                >
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

              {inviteLink ? <InviteLinkReveal link={inviteLink} /> : null}
            </TabsContent>

            {/* ── Collaborators tab ──────────────────────────────────────── */}
            <TabsContent value="people" className="space-y-3 pt-4">
              {confirm?.kind === "revoke-share" ? (
                <InlineConfirm
                  message={`Revoke ${confirm.display}'s access to this site? They'll lose access immediately.`}
                  confirmLabel="Revoke access"
                  busy={busy}
                  onCancel={() => setConfirm(null)}
                  onConfirm={() => void confirmAction()}
                />
              ) : null}

              {sharesLoading ? (
                <p role="status" className="text-sm text-muted-foreground">
                  Loading collaborators…
                </p>
              ) : !shares || shares.length === 0 ? (
                <EmptyState
                  icon={<Users aria-hidden="true" className="size-5" />}
                  text="No collaborators yet. Invite someone from the Invite tab."
                />
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
                        const display =
                          share.name || share.email || share.user_id;
                        const isExpired =
                          share.expires_at != null &&
                          new Date(share.expires_at) < new Date();
                        return (
                          <TableRow key={share.id}>
                            <TableCell className="text-sm font-medium">
                              {display}
                            </TableCell>
                            <TableCell className="text-sm capitalize">
                              {share.role}
                            </TableCell>
                            <TableCell className="text-sm text-muted-foreground">
                              {share.expires_at ? (
                                isExpired ? (
                                  <StatusChip
                                    tone="destructive"
                                    label="Expired"
                                  />
                                ) : (
                                  (relativeTime(share.expires_at) ??
                                  share.expires_at)
                                )
                              ) : (
                                <span className="text-muted-foreground">
                                  Never
                                </span>
                              )}
                            </TableCell>
                            <TableCell className="text-right">
                              <Button
                                type="button"
                                variant="outline"
                                size="sm"
                                disabled={busy}
                                onClick={() =>
                                  setConfirm({
                                    kind: "revoke-share",
                                    userId: share.user_id,
                                    display,
                                  })
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
            </TabsContent>

            {/* ── Pending invites tab (link history) ─────────────────────── */}
            <TabsContent value="pending" className="space-y-3 pt-4">
              <p className="text-sm text-muted-foreground">
                Invite links you&apos;ve sent. Each is single-use and expires 7
                days after it&apos;s created. Regenerate to make a fresh link
                (the old one stops working).
              </p>

              {confirm?.kind === "cancel-invite" ? (
                <InlineConfirm
                  message={`Cancel the invite for ${confirm.invitation.email}? Their link will stop working.`}
                  confirmLabel="Cancel invite"
                  busy={busy}
                  onCancel={() => setConfirm(null)}
                  onConfirm={() => void confirmAction()}
                />
              ) : null}
              {confirm?.kind === "regenerate" ? (
                <InlineConfirm
                  message={`Regenerate the link for ${confirm.invitation.email}? The link you already sent stops working immediately, and a fresh one is created.`}
                  confirmLabel="Regenerate link"
                  busy={busy}
                  onCancel={() => setConfirm(null)}
                  onConfirm={() => void confirmAction()}
                />
              ) : null}

              {rotated ? (
                <InviteLinkReveal
                  link={rotated.link}
                  email={rotated.email}
                  rotated
                />
              ) : null}

              {invitesLoading ? (
                <p role="status" className="text-sm text-muted-foreground">
                  Loading invites…
                </p>
              ) : historyInvites.length === 0 ? (
                <EmptyState
                  icon={<RefreshCw aria-hidden="true" className="size-5" />}
                  text="No outstanding invite links. New-user invites appear here until accepted."
                />
              ) : (
                <div className="rounded-lg border border-[var(--color-border)]">
                  <Table>
                    <caption className="sr-only">
                      Invite link history for {siteName}
                    </caption>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Email</TableHead>
                        <TableHead>Role</TableHead>
                        <TableHead>Status</TableHead>
                        <TableHead className="sr-only">Actions</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {historyInvites.map((inv) => (
                        <InvitationRow
                          key={inv.id}
                          inv={inv}
                          isMine={inv.invited_by === me?.user.id}
                          busy={busy}
                          onCancel={() =>
                            setConfirm({ kind: "cancel-invite", invitation: inv })
                          }
                          onRegenerate={() =>
                            setConfirm({ kind: "regenerate", invitation: inv })
                          }
                        />
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}
            </TabsContent>
          </Tabs>
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

// ---------------------------------------------------------------------------
// Sub-components
// ---------------------------------------------------------------------------

function InvitationRow({
  inv,
  isMine,
  busy,
  onCancel,
  onRegenerate,
}: {
  inv: SiteInvitation;
  isMine: boolean;
  busy: boolean;
  onCancel: () => void;
  onRegenerate: () => void;
}) {
  // Contextual time per status.
  const timeIso =
    inv.status === "accepted"
      ? inv.accepted_at
      : inv.status === "revoked"
        ? inv.revoked_at
        : inv.expires_at;
  const time = timeIso ? (relativeTime(timeIso) ?? undefined) : undefined;

  const meta: string[] = [];
  meta.push(isMine ? "by you" : "invited");
  if (relativeTime(inv.created_at)) meta.push(relativeTime(inv.created_at)!);
  if (inv.attempts > 0) {
    meta.push(`${inv.attempts} attempt${inv.attempts === 1 ? "" : "s"}`);
  }

  const canCancel = inv.status === "pending";
  const canRegenerate = inv.status !== "accepted";

  return (
    <TableRow>
      <TableCell className="text-sm">
        <div className="font-medium">{inv.email}</div>
        <div className="text-xs text-muted-foreground">{meta.join(" · ")}</div>
      </TableCell>
      <TableCell className="text-sm capitalize">{inv.role}</TableCell>
      <TableCell>
        <StatusChip
          tone={STATUS_TONE[inv.status]}
          label={STATUS_LABEL[inv.status]}
          time={time}
        />
      </TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-1.5">
          {canRegenerate ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onRegenerate}
              aria-label={`Regenerate invite link for ${inv.email}`}
            >
              <RefreshCw aria-hidden="true" className="size-3.5" />
              <span className="sr-only sm:not-sr-only">Regenerate</span>
            </Button>
          ) : null}
          {canCancel ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={onCancel}
              aria-label={`Cancel invite for ${inv.email}`}
            >
              <Trash2 aria-hidden="true" className="size-3.5" />
              <span className="sr-only sm:not-sr-only">Cancel</span>
            </Button>
          ) : null}
        </div>
      </TableCell>
    </TableRow>
  );
}

function InlineConfirm({
  message,
  confirmLabel,
  busy,
  onCancel,
  onConfirm,
}: {
  message: string;
  confirmLabel: string;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div
      role="alertdialog"
      aria-label={confirmLabel}
      className="space-y-3 rounded-lg border border-[var(--color-destructive)]/30 bg-[var(--color-destructive)]/5 p-3"
    >
      <p className="text-sm text-[var(--color-foreground)]">{message}</p>
      <div className="flex justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={busy}
          onClick={onCancel}
        >
          Keep
        </Button>
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={busy}
          onClick={onConfirm}
        >
          {busy ? (
            <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
          ) : null}
          {confirmLabel}
        </Button>
      </div>
    </div>
  );
}

function EmptyState({
  icon,
  text,
}: {
  icon: React.ReactNode;
  text: string;
}) {
  return (
    <div className="flex flex-col items-center gap-2 rounded-lg border border-dashed border-[var(--color-border)] px-4 py-8 text-center text-muted-foreground">
      {icon}
      <p className="text-sm">{text}</p>
    </div>
  );
}
