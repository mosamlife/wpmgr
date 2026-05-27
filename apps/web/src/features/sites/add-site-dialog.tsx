import { useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Copy, Check, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { usePairingCode } from "@/features/sites/use-sites";
import type { PairingCode } from "@wpmgr/api";

// Two-step enrollment UX:
//   1. A small form collects an optional site name + tags and POSTs
//      /sites/pairing-codes (operator+).
//   2. The returned one-time pairing code is shown ONCE in a dialog with an
//      expiry countdown and concise install instructions for the agent plugin.

const formSchema = z.object({
  site_name: z.string().max(200).optional(),
  // Comma- or whitespace-separated tags, parsed into a string[] on submit.
  tags: z.string().optional(),
});

type FormValues = z.infer<typeof formSchema>;

function parseTags(input: string | undefined): string[] {
  if (!input) return [];
  return Array.from(
    new Set(
      input
        .split(/[,\s]+/)
        .map((t) => t.trim())
        .filter((t) => t.length > 0 && t.length <= 64),
    ),
  );
}

export function AddSiteDialog() {
  const formRef = useRef<HTMLDialogElement>(null);
  const pairing = usePairingCode();
  const [formOpen, setFormOpen] = useState(false);
  const [created, setCreated] = useState<PairingCode | null>(null);

  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: { site_name: "", tags: "" },
  });

  // Sync the native <dialog> open state with React state so we never touch the
  // ref during render (lint: react-hooks/refs).
  useEffect(() => {
    const el = formRef.current;
    if (!el) return;
    if (formOpen && !el.open) el.showModal();
    if (!formOpen && el.open) el.close();
  }, [formOpen]);

  function openForm() {
    pairing.reset();
    reset({ site_name: "", tags: "" });
    setFormOpen(true);
  }

  const onSubmit = handleSubmit(async (values) => {
    const tags = parseTags(values.tags);
    const name = values.site_name?.trim();
    const result = await pairing.mutateAsync(
      {
        ...(name ? { site_name: name } : {}),
        ...(tags.length > 0 ? { tags } : {}),
      },
      { onError: () => {} },
    );
    setFormOpen(false);
    setCreated(result);
  });

  return (
    <>
      <Button type="button" onClick={openForm}>
        <Plus aria-hidden="true" />
        Add site
      </Button>

      <dialog
        ref={formRef}
        onClose={() => setFormOpen(false)}
        aria-labelledby="add-site-title"
        className="m-auto w-full max-w-md rounded-xl border border-[var(--color-border)] bg-[var(--color-background)] p-6 text-[var(--color-foreground)] backdrop:bg-black/50"
      >
        <form onSubmit={(e) => void onSubmit(e)} noValidate className="space-y-4">
          <div>
            <h2 id="add-site-title" className="text-lg font-semibold">
              Add a site
            </h2>
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Generate a one-time pairing code for the WPMgr Agent plugin to
              enroll a WordPress site.
            </p>
          </div>

          <div className="space-y-2">
            <Label htmlFor="site_name">Site name (optional)</Label>
            <Input
              id="site_name"
              placeholder="My WordPress site"
              aria-invalid={errors.site_name ? true : undefined}
              {...register("site_name")}
            />
            {errors.site_name ? (
              <p role="alert" className="text-sm text-[var(--color-destructive)]">
                {errors.site_name.message}
              </p>
            ) : null}
          </div>

          <div className="space-y-2">
            <Label htmlFor="tags">Tags (optional)</Label>
            <Input
              id="tags"
              placeholder="production, client-a"
              aria-describedby="tags-hint"
              {...register("tags")}
            />
            <p id="tags-hint" className="text-xs text-[var(--color-muted-foreground)]">
              Separate tags with commas or spaces.
            </p>
          </div>

          {pairing.isError ? (
            <p role="alert" className="text-sm text-[var(--color-destructive)]">
              {pairing.error.message}
            </p>
          ) : null}

          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              onClick={() => setFormOpen(false)}
              disabled={pairing.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={pairing.isPending}>
              {pairing.isPending ? "Generating…" : "Generate code"}
            </Button>
          </div>
        </form>
      </dialog>

      <PairingCodeDialog
        created={created}
        onClose={() => setCreated(null)}
      />
    </>
  );
}

// Shows the one-time pairing code with a live expiry countdown and install
// instructions. Uses the native <dialog> (showModal) so it traps focus.
function PairingCodeDialog({
  created,
  onClose,
}: {
  created: PairingCode | null;
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
      await navigator.clipboard.writeText(created.code);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <dialog
      ref={ref}
      onClose={onClose}
      aria-labelledby="pairing-code-title"
      className="m-auto w-full max-w-md rounded-xl border border-[var(--color-border)] bg-[var(--color-background)] p-6 text-[var(--color-foreground)] backdrop:bg-black/50"
    >
      {created ? (
        <div className="space-y-4">
          <h2 id="pairing-code-title" className="text-lg font-semibold">
            Pairing code created
          </h2>
          <p role="alert" className="text-sm text-[var(--color-destructive)]">
            Copy this code now. For security it is shown <strong>once</strong>{" "}
            and cannot be retrieved again.
          </p>

          <div className="flex items-center gap-2">
            <code
              data-testid="pairing-code"
              className="flex-1 overflow-x-auto rounded-md border border-[var(--color-border)] px-3 py-2 font-mono text-sm"
            >
              {created.code}
            </code>
            <Button type="button" variant="outline" size="sm" onClick={() => void copy()}>
              {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>

          <Countdown expiresAt={created.expires_at} />

          <div className="rounded-md border border-[var(--color-border)] p-3 text-sm">
            <p className="font-medium">Next steps</p>
            <ol className="mt-2 list-decimal space-y-1 pl-5 text-[var(--color-muted-foreground)]">
              <li>Install the WPMgr Agent plugin on your WordPress site.</li>
              <li>Enter your control-plane URL in the plugin settings.</li>
              <li>Paste this code to enroll the site.</li>
            </ol>
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

// Live countdown to the pairing code's expiry. Ticks once per second while the
// dialog is open.
function Countdown({ expiresAt }: { expiresAt: string }) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);

  const expiry = Date.parse(expiresAt);
  if (Number.isNaN(expiry)) return null;
  const remaining = Math.max(0, Math.round((expiry - now) / 1000));

  if (remaining <= 0) {
    return (
      <p role="status" className="text-sm text-[var(--color-destructive)]">
        This code has expired. Generate a new one to continue.
      </p>
    );
  }

  const mins = Math.floor(remaining / 60);
  const secs = remaining % 60;
  return (
    <p role="status" className="text-sm text-[var(--color-muted-foreground)]">
      Expires in{" "}
      <span className="font-medium text-[var(--color-foreground)]">
        {mins}:{secs.toString().padStart(2, "0")}
      </span>
    </p>
  );
}
