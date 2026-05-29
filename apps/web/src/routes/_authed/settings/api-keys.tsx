import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { KeyRound, Trash2 } from "lucide-react";

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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { DefinitionList, KvRow } from "@/components/shared/definition-list";
import { StatusChip } from "@/components/status";
import { useMe, canManage } from "@/features/auth/use-auth";
import {
  useApiKeys,
  useCreateApiKey,
  useRevokeApiKey,
} from "@/features/api-keys/use-api-keys";
import type { ApiKey, ApiKeyCreated } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/settings/api-keys")({
  component: ApiKeysPage,
});

const createSchema = z.object({
  name: z.string().min(1, "Name is required").max(200),
  role: z.enum(["owner", "admin", "operator", "viewer"]).optional(),
});

type CreateValues = z.infer<typeof createSchema>;

function ApiKeysPage() {
  const { data: me } = useMe();
  const manage = canManage(me);

  const { data: keys, isPending, isError, error, refetch, isRefetching } =
    useApiKeys();
  const createMutation = useCreateApiKey();
  const revokeMutation = useRevokeApiKey();

  // The full token is returned ONCE on create; we hold it in local state only
  // so it can be shown in a dialog and copied. It is never persisted.
  const [created, setCreated] = useState<ApiKeyCreated | null>(null);

  // Revoke is destructive, so it goes through the typed-confirmation pattern.
  // `revokeTarget` holds the key the operator is about to revoke.
  const [revokeTarget, setRevokeTarget] = useState<ApiKey | null>(null);

  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { name: "", role: "operator" },
  });

  const onCreate = handleSubmit(async (values) => {
    const result = await createMutation.mutateAsync(values, {
      onError: () => {},
    });
    setCreated(result);
    reset();
  });

  async function performRevoke() {
    if (!revokeTarget) return;
    try {
      await revokeMutation.mutateAsync(revokeTarget.id);
      setRevokeTarget(null);
    } catch {
      // Mutation error stays surfaced on the page; confirm dialog stays open
      // so the operator can retry.
    }
  }

  return (
    <section aria-labelledby="api-keys-heading" className="space-y-6">
      <PageHeader
        title="API keys"
        subline="Programmatic access tokens for the active tenant."
      />

      {manage ? (
        <div
          role="form"
          aria-label="Create API key"
          className="flex flex-wrap items-end gap-3 rounded-xl border border-[var(--color-border)] p-4"
        >
          <div className="space-y-2">
            <Label htmlFor="name">Key name</Label>
            <Input
              id="name"
              className="w-56"
              aria-invalid={errors.name ? true : undefined}
              {...register("name")}
            />
            {errors.name ? (
              <p role="alert" className="text-sm text-[var(--color-destructive)]">
                {errors.name.message}
              </p>
            ) : null}
          </div>
          <div className="space-y-2">
            <Label htmlFor="role">Role</Label>
            <select
              id="role"
              className="h-9 rounded-md border border-[var(--color-border)] bg-transparent px-3 text-sm"
              {...register("role")}
            >
              <option value="owner">Owner</option>
              <option value="admin">Admin</option>
              <option value="operator">Operator</option>
              <option value="viewer">Viewer</option>
            </select>
          </div>
          <Button
            type="button"
            onClick={() => void onCreate()}
            disabled={createMutation.isPending}
          >
            Create key
          </Button>
          {createMutation.isError ? (
            <p role="alert" className="basis-full text-sm text-[var(--color-destructive)]">
              {createMutation.error.message}
            </p>
          ) : null}
        </div>
      ) : null}

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading keys…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load API keys."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload keys"
          isRetrying={isRefetching}
        />
      ) : keys.length === 0 ? (
        <div
          role="status"
          aria-label="No API keys"
          className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-[var(--color-border)] py-12 text-center"
        >
          <KeyRound
            aria-hidden="true"
            strokeWidth={1.5}
            className="size-8 text-[var(--color-muted-foreground)]/50"
          />
          <div className="space-y-1">
            <p className="text-balance text-sm font-medium text-[var(--color-foreground)]">
              No API keys yet.
            </p>
            <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
              {manage
                ? "Create a key above to grant programmatic access."
                : "Ask an admin to create a key."}
            </p>
          </div>
        </div>
      ) : (
        <div className="rounded-xl border border-[var(--color-border)]">
          <Table>
            <caption className="sr-only">List of API keys</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Prefix</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Status</TableHead>
                {manage ? <TableHead className="sr-only">Actions</TableHead> : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="font-medium">{k.name}</TableCell>
                  <TableCell>
                    <span className="font-mono text-xs tabular-nums">
                      {k.prefix}&hellip;
                    </span>
                  </TableCell>
                  <TableCell className="capitalize">{k.role}</TableCell>
                  <TableCell className="tabular-nums text-[var(--color-muted-foreground)]">
                    {k.created_at}
                  </TableCell>
                  <TableCell>
                    {k.revoked_at ? (
                      <StatusChip tone="muted" label="Revoked" />
                    ) : (
                      <StatusChip tone="success" label="Active" pulse />
                    )}
                  </TableCell>
                  {manage ? (
                    <TableCell className="text-right">
                      {k.revoked_at ? null : (
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={revokeMutation.isPending}
                          onClick={() => setRevokeTarget(k)}
                          aria-label={`Revoke ${k.name}`}
                        >
                          <Trash2 aria-hidden="true" />
                          Revoke key
                        </Button>
                      )}
                    </TableCell>
                  ) : null}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <ShowOnceDialog created={created} onClose={() => setCreated(null)} />

      <DestructiveConfirm
        open={revokeTarget !== null}
        onClose={() => setRevokeTarget(null)}
        onConfirm={performRevoke}
        title={`Revoke API key "${revokeTarget?.name ?? ""}"`}
        consequencesBody={
          <div className="space-y-3">
            <p>
              Any service or script using this token will lose access
              immediately. The token cannot be reactivated; you&apos;ll need to
              create a new one and redeploy callers.
            </p>
            {revokeTarget ? (
              <DefinitionList>
                <KvRow
                  label="Prefix"
                  value={
                    <span className="font-mono text-xs">
                      {revokeTarget.prefix}&hellip;
                    </span>
                  }
                />
                <KvRow label="Role" value={<span className="capitalize">{revokeTarget.role}</span>} />
              </DefinitionList>
            ) : null}
          </div>
        }
        resourceName={revokeTarget?.name ?? ""}
        confirmLabel="Revoke key"
        cancelLabel="Keep key"
        isPending={revokeMutation.isPending}
        errorMessage={
          revokeMutation.isError ? revokeMutation.error.message : null
        }
      />
    </section>
  );
}

// Modal that surfaces the full token exactly once.
function ShowOnceDialog({
  created,
  onClose,
}: {
  created: ApiKeyCreated | null;
  onClose: () => void;
}) {
  // Reset the copied indicator whenever a new key is shown; preserve it across
  // re-renders while the dialog is open so the checkmark stays.
  const [prevCreated, setPrevCreated] = useState(created);
  if (created !== prevCreated) {
    setPrevCreated(created);
  }

  return (
    <Dialog open={created !== null} onClose={onClose}>
      {created ? (
        <DialogContent ariaLabelledBy="key-created-title">
          <DialogHeader>
            <DialogTitle id="key-created-title">API key created</DialogTitle>
          </DialogHeader>

          <DialogBody className="space-y-4">
            <p role="alert" className="text-sm text-[var(--color-destructive)]">
              Copy this key now. For security it will <strong>not</strong> be
              shown again.
            </p>
            <div className="space-y-1.5">
              <Label>Token</Label>
              <CopyableMono
                value={created.token}
                label="Copy token"
                className="w-full"
              />
            </div>
            <DefinitionList
              rows={[
                { label: "Name", value: created.api_key.name },
                { label: "Role", value: <span className="capitalize">{created.api_key.role}</span> },
                { label: "Prefix", value: created.api_key.prefix, mono: true },
              ]}
            />
          </DialogBody>

          <DialogFooter className="pt-2">
            <Button type="button" onClick={onClose}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      ) : null}
    </Dialog>
  );
}
