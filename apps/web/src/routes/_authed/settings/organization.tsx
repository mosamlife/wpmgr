import { useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { useMe } from "@/features/auth/use-auth";
import {
  useOrgs,
  useRenameOrg,
  useDeleteOrg,
  useActivateOrg,
  type Org,
} from "@/features/orgs/use-orgs";

export const Route = createFileRoute("/_authed/settings/organization")({
  component: OrganizationSettingsPage,
});

function OrganizationSettingsPage() {
  const { data: me } = useMe();
  const { data: orgs, isPending, isError, error, refetch } = useOrgs();

  const activeId = me?.active_tenant_id ?? null;
  const activeOrg = orgs?.find((o) => o.id === activeId) ?? null;

  if (isPending) {
    return (
      <section className="max-w-2xl space-y-6">
        <PageHeader
          title="Organisation"
          subline="Manage your organisation's name and details."
        />
        <div
          role="status"
          aria-label="Loading organisation settings"
          className="h-44 animate-pulse rounded-xl bg-muted/50"
        />
      </section>
    );
  }

  if (isError) {
    return (
      <section className="max-w-2xl space-y-6">
        <PageHeader
          title="Organisation"
          subline="Manage your organisation's name and details."
        />
        <PageError
          what="Could not load organisation details."
          why={error?.message}
          onRetry={() => void refetch()}
          retryLabel="Reload"
        />
      </section>
    );
  }

  return (
    <section className="max-w-2xl space-y-6">
      <PageHeader
        title="Organisation"
        subline="Manage your organisation's name and details."
      />
      {activeOrg ? (
        <>
          <OrgCard key={activeOrg.id} org={activeOrg} />
          {activeOrg.role === "owner" ? (
            <DangerZoneCard
              key={`${activeOrg.id}-danger`}
              org={activeOrg}
              orgs={orgs ?? []}
            />
          ) : null}
        </>
      ) : (
        <Card>
          <CardContent className="py-8 text-center text-sm text-muted-foreground">
            No active organisation. Pick one from the switcher in the top bar.
          </CardContent>
        </Card>
      )}
    </section>
  );
}

function OrgCard({ org }: { org: Org }) {
  const rename = useRenameOrg();
  const [name, setName] = useState(org.name);
  const [nameError, setNameError] = useState<string | null>(null);

  // Only admins + owners can rename; viewers/operators see a read-only name.
  const canRename = org.role === "owner" || org.role === "admin";
  const dirty = name.trim() !== org.name && name.trim().length > 0;

  function handleSave() {
    const trimmed = name.trim();
    if (!trimmed) {
      setNameError("Name is required");
      return;
    }
    if (trimmed.length > 200) {
      setNameError("Name must be 200 characters or fewer");
      return;
    }
    setNameError(null);
    rename.mutate({ orgId: org.id, name: trimmed }, { onError: () => {} });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Organisation details</CardTitle>
        <CardDescription>
          The organisation name appears in the switcher and on shared-site
          invitations.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="org-name">Name</Label>
          <Input
            id="org-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={!canRename || rename.isPending}
            aria-invalid={nameError !== null ? true : undefined}
            aria-describedby={nameError ? "org-name-error" : undefined}
            className="max-w-sm"
            placeholder="Acme Corp"
          />
          {nameError ? (
            <p
              id="org-name-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {nameError}
            </p>
          ) : null}
          {!canRename ? (
            <p className="text-xs text-muted-foreground">
              Only an admin or owner can rename the organisation.
            </p>
          ) : null}
        </div>

        <div className="space-y-1.5">
          <Label>Organisation ID</Label>
          <CopyableMono
            value={org.id}
            label="Copy organisation ID"
            className="max-w-sm"
          />
        </div>

        {rename.isError ? (
          <PageError what="Could not rename organisation." why={rename.error.message} />
        ) : null}

        {canRename ? (
          <div>
            <Button
              type="button"
              onClick={handleSave}
              disabled={rename.isPending || !dirty}
            >
              {rename.isPending ? "Saving…" : "Save name"}
            </Button>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// DangerZoneCard: owner-only organisation deletion (GH #152 part 2).
//
// Only rendered for org.role === "owner" (stricter than rename's admin+
// gate, matching the server's own owner-only check). Deletion is soft with a
// grace window: the org disappears from GET /orgs immediately but stays
// recoverable server-side until a background job purges it, so the copy
// below never says "deleted immediately".
//
// This page only ever shows the caller's ACTIVE organisation, so a
// successful delete here always means the org just deleted was the one the
// session was operating in. Staying on this page would leave it pointing at
// an org that no longer resolves, so on success we switch the session to
// another organisation the caller belongs to (if any) and navigate to
// /sites; with none left, /sites itself falls back to the no-org onboarding
// screen once `me` reflects zero memberships (see _authed.tsx's useHasNoOrg).
// ---------------------------------------------------------------------------

function DangerZoneCard({ org, orgs }: { org: Org; orgs: Org[] }) {
  const navigate = useNavigate();
  const deleteOrg = useDeleteOrg();
  const activateOrg = useActivateOrg();
  const [confirmOpen, setConfirmOpen] = useState(false);

  async function performDelete() {
    try {
      await deleteOrg.mutateAsync({ orgId: org.id, confirmName: org.name });
      setConfirmOpen(false);
      const nextOrg = orgs.find((o) => o.id !== org.id);
      if (nextOrg) {
        try {
          await activateOrg.mutateAsync(nextOrg.id);
        } catch {
          // Best effort: still navigate away below either way regardless.
        }
      }
      void navigate({ to: "/sites" });
    } catch {
      // Error surfaces inside the dialog body via deleteOrg's mutation state.
    }
  }

  return (
    <Card className="border-destructive/30">
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium text-destructive">
          Danger zone
        </CardTitle>
        <CardDescription>
          Permanently remove this organisation and everything in it.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-wrap items-center gap-3">
        <p className="flex-1 min-w-[16rem] text-sm text-muted-foreground">
          Disconnects every site in this organisation and schedules all data,
          including backups, for permanent deletion. Recoverable during a
          grace window before it&apos;s purged for good.
        </p>
        <Button
          type="button"
          variant="destructive"
          onClick={() => setConfirmOpen(true)}
        >
          Delete organisation
        </Button>
      </CardContent>

      <DestructiveConfirm
        open={confirmOpen}
        onClose={() => {
          setConfirmOpen(false);
          deleteOrg.reset();
        }}
        onConfirm={performDelete}
        title={`Delete "${org.name}"`}
        consequencesBody={
          <div className="space-y-2">
            <p>Deleting this organisation will:</p>
            <ul className="list-disc space-y-1 pl-5">
              <li>Disconnect every site in this organisation from WPMgr</li>
              <li>
                Schedule all data, including backups, for permanent deletion
              </li>
              <li>Remove access for every member of this organisation</li>
            </ul>
            <p>
              This organisation stays recoverable for about 7 days (the grace
              window) before it&apos;s purged for good.
            </p>
          </div>
        }
        resourceName={org.name}
        confirmLabel="Delete organisation"
        cancelLabel="Keep organisation"
        isPending={deleteOrg.isPending}
        errorMessage={deleteOrg.isError ? deleteOrg.error.message : null}
      />
    </Card>
  );
}
