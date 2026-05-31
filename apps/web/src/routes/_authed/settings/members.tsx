import { useId, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { Loader2, ShieldCheck, Trash2, UserPlus } from "lucide-react";

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
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { toast } from "@/components/toast";
import { useMe, canManage, isOrgScoped } from "@/features/auth/use-auth";
import {
  useMembers,
  useUpdateMemberRole,
  useRemoveMember,
  useInviteMember,
  type Member,
  type MemberRole,
} from "@/features/orgs/use-members";

export const Route = createFileRoute("/_authed/settings/members")({
  component: MembersPage,
});

function MembersPage() {
  const { data: me } = useMe();
  const manage = canManage(me);
  const orgScoped = isOrgScoped(me);

  const {
    data: members,
    isPending,
    isError,
    error,
    refetch,
    isRefetching,
  } = useMembers();

  const [showInvite, setShowInvite] = useState(false);

  // Site-scoped collaborators have no business seeing this page at all.
  if (!orgScoped) {
    return (
      <section
        aria-labelledby="members-heading"
        className="max-w-3xl space-y-6"
      >
        <PageHeader
          title="Members"
          subline="Manage who belongs to this organisation."
        />
        <p className="text-sm text-muted-foreground">
          You are accessing this organisation as a site collaborator and do not
          have permission to view or manage members.
        </p>
      </section>
    );
  }

  return (
    <section aria-labelledby="members-heading" className="max-w-3xl space-y-6">
      <PageHeader
        title="Members"
        subline="Manage who belongs to this organisation and their roles."
        actions={
          manage ? (
            <Button
              type="button"
              onClick={() => setShowInvite(true)}
              className="gap-2"
            >
              <UserPlus aria-hidden="true" className="size-4" />
              Invite member
            </Button>
          ) : undefined
        }
      />

      {isPending ? (
        <p role="status" className="text-muted-foreground">
          Loading members…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load members."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload members"
          isRetrying={isRefetching}
        />
      ) : !members || members.length === 0 ? (
        <div
          role="status"
          className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-[var(--color-border)] py-12 text-center"
        >
          <ShieldCheck
            aria-hidden="true"
            strokeWidth={1.5}
            className="size-8 text-muted-foreground/50"
          />
          <p className="text-sm text-muted-foreground">No members found.</p>
        </div>
      ) : (
        <div className="rounded-xl border border-[var(--color-border)]">
          <Table>
            <caption className="sr-only">Organisation members</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Email / ID</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Role</TableHead>
                {manage ? (
                  <TableHead className="sr-only">Actions</TableHead>
                ) : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.map((m) => (
                <MemberRow
                  key={m.user_id}
                  member={m}
                  manage={manage}
                  currentUserId={me?.user.id}
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <InviteDialog open={showInvite} onClose={() => setShowInvite(false)} />
    </section>
  );
}

// ---------------------------------------------------------------------------
// MemberRow — inline role picker + remove button.
// ---------------------------------------------------------------------------

function MemberRow({
  member,
  manage,
  currentUserId,
}: {
  member: Member;
  manage: boolean;
  currentUserId?: string;
}) {
  const updateRole = useUpdateMemberRole();
  const removeMember = useRemoveMember();
  const isCurrentUser = member.user_id === currentUserId;

  async function handleRoleChange(newRole: MemberRole) {
    if (newRole === member.role) return;
    try {
      await updateRole.mutateAsync({ userId: member.user_id, role: newRole });
    } catch {
      // Error toast handled in the hook.
    }
  }

  async function handleRemove() {
    try {
      await removeMember.mutateAsync(member.user_id);
    } catch {
      // Error toast handled in the hook.
    }
  }

  const display = member.email ?? member.user_id;
  const isPending = updateRole.isPending || removeMember.isPending;

  return (
    <TableRow>
      <TableCell>
        {member.email ? (
          <span className="text-sm font-medium">{member.email}</span>
        ) : (
          <CopyableMono value={member.user_id} truncate label="Copy user ID" />
        )}
        {isCurrentUser ? (
          <span className="ml-2 text-xs text-muted-foreground">(you)</span>
        ) : null}
      </TableCell>
      <TableCell className="text-sm text-muted-foreground">
        {member.name ?? "—"}
      </TableCell>
      <TableCell>
        {manage && !isCurrentUser ? (
          <Select
            value={member.role}
            onChange={(e) =>
              void handleRoleChange(e.target.value as MemberRole)
            }
            disabled={isPending}
            className="w-32"
            aria-label={`Role for ${display}`}
          >
            <option value="owner">Owner</option>
            <option value="admin">Admin</option>
            <option value="operator">Operator</option>
            <option value="viewer">Viewer</option>
          </Select>
        ) : (
          <span className="capitalize text-sm">{member.role}</span>
        )}
      </TableCell>
      {manage ? (
        <TableCell className="text-right">
          {isCurrentUser ? null : (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={isPending}
              onClick={() => void handleRemove()}
              aria-label={`Remove ${display}`}
            >
              {removeMember.isPending ? (
                <Loader2 aria-hidden="true" className="size-4 animate-spin" />
              ) : (
                <Trash2 aria-hidden="true" className="size-4" />
              )}
              Remove
            </Button>
          )}
        </TableCell>
      ) : null}
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// InviteDialog
// ---------------------------------------------------------------------------

function InviteDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const titleId = useId();
  const inviteMember = useInviteMember();

  const [email, setEmail] = useState("");
  const [role, setRole] = useState<MemberRole>("operator");
  const [formError, setFormError] = useState<string | null>(null);
  const [inviteLink, setInviteLink] = useState<string | null>(null);
  const [linkCopied, setLinkCopied] = useState(false);

  function reset() {
    setEmail("");
    setRole("operator");
    setFormError(null);
    setInviteLink(null);
    setLinkCopied(false);
  }

  function handleClose() {
    reset();
    onClose();
  }

  async function handleInvite() {
    const trimmedEmail = email.trim();
    if (!trimmedEmail) {
      setFormError("Email is required");
      return;
    }
    setFormError(null);

    try {
      const result = await inviteMember.mutateAsync({
        email: trimmedEmail,
        role,
      });
      if (result.accept_link) {
        setInviteLink(result.accept_link);
        toast.success("Invitation created — copy the link below to share it.");
      } else {
        toast.success(`${trimmedEmail} has been invited as ${role}.`);
        handleClose();
      }
    } catch (err) {
      setFormError(
        err instanceof Error ? err.message : "Could not send invitation",
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

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Invite member</DialogTitle>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="invite-email">Email address</Label>
            <Input
              id="invite-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="colleague@example.com"
              data-autofocus
              aria-invalid={formError ? true : undefined}
              disabled={inviteMember.isPending || inviteLink !== null}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !inviteLink) void handleInvite();
              }}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="invite-role">Role</Label>
            <Select
              id="invite-role"
              value={role}
              onChange={(e) => setRole(e.target.value as MemberRole)}
              disabled={inviteMember.isPending || inviteLink !== null}
            >
              <option value="admin">Admin</option>
              <option value="operator">Operator</option>
              <option value="viewer">Viewer</option>
            </Select>
          </div>

          {formError ? (
            <p role="alert" className="text-sm text-[var(--color-destructive)]">
              {formError}
            </p>
          ) : null}

          {inviteLink ? (
            <div
              role="alert"
              aria-live="polite"
              className="space-y-2 rounded-lg border border-[var(--color-primary)]/30 bg-[var(--color-primary)]/5 p-3"
            >
              <p className="text-sm font-medium">Invite link (shown once)</p>
              <div className="flex gap-2">
                <Input
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
                      <span className="text-xs">Copied</span>
                    </>
                  ) : (
                    <>
                      <span className="text-xs">Copy invite link</span>
                    </>
                  )}
                </Button>
              </div>
            </div>
          ) : null}
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={handleClose}
            disabled={inviteMember.isPending}
          >
            {inviteLink ? "Close" : "Cancel"}
          </Button>
          {!inviteLink ? (
            <Button
              type="button"
              onClick={() => void handleInvite()}
              disabled={inviteMember.isPending || !email.trim()}
            >
              {inviteMember.isPending ? (
                <>
                  <Loader2 aria-hidden="true" className="animate-spin" />
                  <span>Inviting…</span>
                </>
              ) : (
                "Send invite"
              )}
            </Button>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
