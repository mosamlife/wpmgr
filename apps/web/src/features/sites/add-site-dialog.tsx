import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Copy, Check, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { usePairingCode } from "@/features/sites/use-sites";
import type { PairingCode } from "@wpmgr/api";

// Two-step enrollment UX (Sprint 3 chrome refresh — form logic unchanged):
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

      <Dialog open={formOpen} onClose={() => setFormOpen(false)}>
        <DialogContent ariaLabelledBy="add-site-title">
          <form onSubmit={(e) => void onSubmit(e)} noValidate>
            <DialogHeader>
              <DialogTitle id="add-site-title">Add site</DialogTitle>
              <DialogDescription>
                Generate a one-time pairing code for the WPMgr Agent plugin to
                enroll a WordPress site.
              </DialogDescription>
            </DialogHeader>

            <DialogBody>
              <div className="space-y-2">
                <Label htmlFor="site_name">Site name (optional)</Label>
                <Input
                  id="site_name"
                  placeholder="My WordPress site"
                  aria-invalid={errors.site_name ? true : undefined}
                  {...register("site_name")}
                />
                {errors.site_name ? (
                  <p
                    role="alert"
                    className="text-sm text-[var(--color-destructive)]"
                  >
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
                <p
                  id="tags-hint"
                  className="text-xs text-[var(--color-muted-foreground)]"
                >
                  Separate tags with commas or spaces.
                </p>
              </div>

              {pairing.isError ? (
                <p
                  role="alert"
                  className="text-sm text-[var(--color-destructive)]"
                >
                  {pairing.error.message}
                </p>
              ) : null}
            </DialogBody>

            <DialogFooter className="pt-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => setFormOpen(false)}
                disabled={pairing.isPending}
              >
                Close
              </Button>
              <Button type="submit" disabled={pairing.isPending}>
                {pairing.isPending ? "Generating…" : "Generate pairing code"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <PairingCodeDialog created={created} onClose={() => setCreated(null)} />
    </>
  );
}

// Shows the one-time pairing code with a live expiry countdown and install
// instructions. Sprint 3 chrome refresh — content is preserved.
function PairingCodeDialog({
  created,
  onClose,
}: {
  created: PairingCode | null;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (created) setCopied(false);
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
    <Dialog open={created !== null} onClose={onClose}>
      {created ? (
        <DialogContent ariaLabelledBy="pairing-code-title">
          <DialogHeader>
            <DialogTitle id="pairing-code-title">
              Pairing code created
            </DialogTitle>
          </DialogHeader>

          <DialogBody>
            <p
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              Copy this code now. For security it is shown{" "}
              <strong>once</strong> and cannot be retrieved again.
            </p>

            <div className="flex items-center gap-2">
              <code
                data-testid="pairing-code"
                className="flex-1 overflow-x-auto rounded-md border border-[var(--color-border)] px-3 py-2 font-mono text-sm"
              >
                {created.code}
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
                {copied ? "Copied" : "Copy code"}
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
