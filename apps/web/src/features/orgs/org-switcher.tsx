import { useId, useState } from "react";
import { Building2, Check, ChevronDown, Loader2, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { useMe } from "@/features/auth/use-auth";
import { useCreateOrg, useActivateOrg, useOrgs } from "@/features/orgs/use-orgs";
import { useSharedWithMe } from "@/features/sharing/use-shared-with-me";

// OrgSwitcher — dropdown listing all orgs the user can access.
//
// Org list = me.memberships (full org members) UNION orgs present in
// /shared-with-me (site-scoped collaborators). This ensures a share-only
// collaborator can still see and switch to that org.
//
// Switching calls POST /api/v1/orgs/{id}/activate which sets the session's
// active_tenant_id; useActivateOrg then clears ALL server state so every
// query refetches in the new org context.

interface OrgEntry {
  id: string;
  name: string;
  role: string;
}

export function OrgSwitcher() {
  const { data: me } = useMe();
  const { data: orgs } = useOrgs();
  const { data: sharedWithMe } = useSharedWithMe();
  const activateOrg = useActivateOrg();
  const [showCreate, setShowCreate] = useState(false);

  if (!me) return null;

  // Real org names come from GET /orgs (id → name). The membership list gives
  // the set of orgs; we resolve each name from /orgs, falling back to the id
  // only until that query resolves.
  const nameById = new Map((orgs ?? []).map((o) => [o.id, o.name]));

  // Build org list: membership orgs first, then share-only orgs.
  const membershipOrgs: OrgEntry[] = me.memberships.map((m) => ({
    id: m.tenant_id,
    name: nameById.get(m.tenant_id) ?? m.tenant_id,
    role: m.role,
  }));

  // Overlay org_name from shared-with-me where known.
  const sharedOrgIds = new Set<string>();
  const sharedOrgs: OrgEntry[] = [];
  for (const s of sharedWithMe ?? []) {
    const orgId = s.org_id;
    if (!orgId || sharedOrgIds.has(orgId)) continue;
    // Skip orgs already covered by a full membership.
    if (membershipOrgs.some((o) => o.id === orgId)) {
      // We may be able to fill in the name.
      const mo = membershipOrgs.find((o) => o.id === orgId);
      if (mo && s.org_name) mo.name = s.org_name;
      continue;
    }
    sharedOrgIds.add(orgId);
    sharedOrgs.push({
      id: orgId,
      name: s.org_name ?? orgId,
      role: s.role,
    });
  }

  const allOrgs = [...membershipOrgs, ...sharedOrgs];
  const activeId = me.active_tenant_id;
  const activeOrg = allOrgs.find((o) => o.id === activeId);
  const displayName = activeOrg?.name ?? activeId ?? "Select org";

  function handleSwitch(orgId: string) {
    if (orgId === activeId) return;
    activateOrg.mutate(orgId);
  }

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-label={`Current organisation: ${displayName}. Click to switch.`}
            className="max-w-[11rem] gap-1.5 font-medium"
          >
            <Building2 aria-hidden="true" className="size-4 shrink-0" />
            <span className="truncate">{displayName}</span>
            {activateOrg.isPending ? (
              <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
            ) : (
              <ChevronDown aria-hidden="true" className="size-3.5 opacity-60" />
            )}
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="min-w-[14rem]">
          <DropdownMenuLabel>Organisations</DropdownMenuLabel>
          <DropdownMenuSeparator />
          {allOrgs.map((org) => (
            <DropdownMenuItem
              key={org.id}
              onSelect={() => handleSwitch(org.id)}
              className="gap-2"
              disabled={activateOrg.isPending}
            >
              <span className="flex-1 truncate">{org.name}</span>
              <span className="shrink-0 text-xs capitalize text-muted-foreground">
                {org.role}
              </span>
              {org.id === activeId ? (
                <Check
                  aria-label="active"
                  className="size-4 shrink-0 text-primary"
                />
              ) : null}
            </DropdownMenuItem>
          ))}
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onSelect={() => setShowCreate(true)}
            className="gap-2"
          >
            <Plus aria-hidden="true" className="size-4" />
            Create organisation
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <CreateOrgDialog
        open={showCreate}
        onClose={() => setShowCreate(false)}
      />
    </>
  );
}

// ---------------------------------------------------------------------------
// CreateOrgDialog
// ---------------------------------------------------------------------------

function CreateOrgDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const titleId = useId();
  const createOrg = useCreateOrg();
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);

  function reset() {
    setName("");
    setError(null);
  }

  function handleClose() {
    reset();
    onClose();
  }

  async function handleCreate() {
    const trimmed = name.trim();
    if (!trimmed) {
      setError("Name is required");
      return;
    }
    setError(null);
    try {
      await createOrg.mutateAsync({ name: trimmed });
      handleClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create organisation");
    }
  }

  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Create organisation</DialogTitle>
        </DialogHeader>

        <DialogBody className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="org-name">Organisation name</Label>
            <Input
              id="org-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Acme Corp"
              data-autofocus
              aria-invalid={error ? true : undefined}
              disabled={createOrg.isPending}
              onKeyDown={(e) => {
                if (e.key === "Enter") void handleCreate();
              }}
            />
            {error ? (
              <p role="alert" className="text-sm text-[var(--color-destructive)]">
                {error}
              </p>
            ) : null}
          </div>
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={handleClose}
            disabled={createOrg.isPending}
          >
            Cancel
          </Button>
          <Button
            type="button"
            onClick={() => void handleCreate()}
            disabled={createOrg.isPending || !name.trim()}
          >
            {createOrg.isPending ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                <span>Creating…</span>
              </>
            ) : (
              "Create"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

