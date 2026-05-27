import { useEffect, useRef, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Copy, Check, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
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
import { useMe, canManage } from "@/features/auth/use-auth";
import {
  useApiKeys,
  useCreateApiKey,
  useRevokeApiKey,
} from "@/features/api-keys/use-api-keys";
import type { ApiKeyCreated } from "@wpmgr/api";

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
                          onClick={() => revokeMutation.mutate(k.id)}
                          aria-label={`Revoke ${k.name}`}
                        >
                          <Trash2 aria-hidden="true" />
                          Revoke
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
    </section>
  );
}

// Modal that surfaces the full token exactly once. Uses the native <dialog>
// element (showModal) so it traps focus and is dismissable, without pulling in
// an extra UI dependency.
function ShowOnceDialog({
  created,
  onClose,
}: {
  created: ApiKeyCreated | null;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (created && !el.open) el.showModal();
    if (!created && el.open) el.close();
    setCopied(false);
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
    <dialog
      ref={ref}
      onClose={onClose}
      aria-labelledby="key-created-title"
      className="m-auto w-full max-w-md rounded-xl border border-[var(--color-border)] bg-[var(--color-background)] p-6 text-[var(--color-foreground)] backdrop:bg-black/50"
    >
      {created ? (
        <div className="space-y-4">
          <h2 id="key-created-title" className="text-lg font-semibold">
            API key created
          </h2>
          <p role="alert" className="text-sm text-[var(--color-destructive)]">
            Copy this key now. For security it will <strong>not</strong> be shown
            again.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-md border border-[var(--color-border)] px-3 py-2 text-sm">
              {created.token}
            </code>
            <Button type="button" variant="outline" size="sm" onClick={() => void copy()}>
              {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div className="flex justify-end">
            <Button type="button" onClick={onClose}>
              Done
            </Button>
          </div>
        </div>
      ) : null}
    </dialog>
  );
}
