import { useEffect, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Copy, Check, Trash2 } from "lucide-react";

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

  const { data: keys, isPending, isError, error, refetch } = useApiKeys();
  const createMutation = useCreateApiKey();
  const revokeMutation = useRevokeApiKey();

  // The full token is returned ONCE on create; we hold it in local state only
  // so it can be shown in a dialog and copied. It is never persisted.
  const [created, setCreated] = useState<ApiKeyCreated | null>(null);

  // Sprint 3: revoke is destructive, so it goes through the typed-confirmation
  // pattern. `revokeTarget` holds the key the operator is about to revoke.
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
      <div>
        <h1 id="api-keys-heading" className="text-2xl font-semibold">
          API keys
        </h1>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Programmatic access tokens for the active tenant.
        </p>
      </div>

      {manage ? (
        <form
          onSubmit={(e) => void onCreate(e)}
          noValidate
          className="flex flex-wrap items-end gap-3 rounded-xl border border-[var(--color-border)] p-4"
        >
          <div className="space-y-2">
            <Label htmlFor="name">New key name</Label>
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
          <Button type="submit" disabled={createMutation.isPending}>
            Create key
          </Button>
          {createMutation.isError ? (
            <p role="alert" className="basis-full text-sm text-[var(--color-destructive)]">
              {createMutation.error.message}
            </p>
          ) : null}
        </form>
      ) : null}

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading API keys…
        </p>
      ) : isError ? (
        <div role="alert" className="space-y-3">
          <p className="text-[var(--color-destructive)]">
            Failed to load API keys: {error.message}
          </p>
          <Button variant="outline" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      ) : keys.length === 0 ? (
        <p className="text-[var(--color-muted-foreground)]">No API keys yet.</p>
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
                    <code>{k.prefix}…</code>
                  </TableCell>
                  <TableCell className="capitalize">{k.role}</TableCell>
                  <TableCell className="text-[var(--color-muted-foreground)]">
                    {k.created_at}
                  </TableCell>
                  <TableCell>
                    {k.revoked_at ? (
                      <span className="text-[var(--color-muted-foreground)]">Revoked</span>
                    ) : (
                      "Active"
                    )}
                  </TableCell>
                  {manage ? (
                    <TableCell className="text-right">
                      {k.revoked_at ? null : (
                        <Button
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
          <div className="space-y-2">
            <p>
              Any service or script using this token will lose access
              immediately. The token cannot be reactivated; you&apos;ll need to
              create a new one and redeploy callers.
            </p>
            {revokeTarget ? (
              <p className="text-[var(--color-muted-foreground)]">
                Prefix:{" "}
                <code className="font-mono text-xs text-[var(--color-foreground)]">
                  {revokeTarget.prefix}…
                </code>
                <span className="mx-2">·</span>Role:{" "}
                <span className="capitalize text-[var(--color-foreground)]">
                  {revokeTarget.role}
                </span>
              </p>
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

// Modal that surfaces the full token exactly once (Sprint 3 chrome refresh).
function ShowOnceDialog({
  created,
  onClose,
}: {
  created: ApiKeyCreated | null;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (created) setCopied(false);
  }, [created]);

  async function copy() {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.token);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <Dialog open={created !== null} onClose={onClose}>
      {created ? (
        <DialogContent ariaLabelledBy="key-created-title">
          <DialogHeader>
            <DialogTitle id="key-created-title">API key created</DialogTitle>
          </DialogHeader>

          <DialogBody>
            <p role="alert" className="text-sm text-[var(--color-destructive)]">
              Copy this key now. For security it will <strong>not</strong> be
              shown again.
            </p>
            <div className="flex items-center gap-2">
              <code className="flex-1 overflow-x-auto rounded-md border border-[var(--color-border)] px-3 py-2 font-mono text-sm">
                {created.token}
              </code>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => void copy()}
              >
                {copied ? (
                  <Check aria-hidden="true" />
                ) : (
                  <Copy aria-hidden="true" />
                )}
                {copied ? "Copied" : "Copy token"}
              </Button>
            </div>
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
