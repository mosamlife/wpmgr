import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";

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
import { useMe } from "@/features/auth/use-auth";
import { useOrgs, useRenameOrg, type Org } from "@/features/orgs/use-orgs";

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
        <OrgCard key={activeOrg.id} org={activeOrg} />
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
