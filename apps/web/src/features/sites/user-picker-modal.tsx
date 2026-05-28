import { useEffect, useRef } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

// V0 user picker for one-click login.
//
// The site agent does NOT currently sync the WordPress user list to the
// control plane, so we cannot render a real picker dropdown. Instead we
// collect the WP `user_login` as a freeform text field, with the same
// validation rules WordPress itself applies (alphanumerics + a small set of
// safe punctuation). On submit we pass it back to the parent which calls
// `useAutoLogin({ target_wp_user_login })`.
//
// When the agent grows a `users.sync` capability and the SDK exposes a list
// endpoint, this component becomes a real picker — the public `onSubmit`
// contract (a single `target_wp_user_login` string) stays the same.

const schema = z.object({
  // WordPress user_login: allow letters, digits, underscore, period, hyphen,
  // and @ (for email-style logins). Max 64 chars matches the backend cap.
  target_wp_user_login: z
    .string()
    .max(64, "Max 64 characters.")
    .regex(/^[a-zA-Z0-9_.\-@]*$/, "Only letters, digits, and . _ - @ are allowed.")
    .optional()
    .or(z.literal("")),
});

type FormValues = z.infer<typeof schema>;

export interface UserPickerModalProps {
  open: boolean;
  onClose: () => void;
  /** Called with the typed user login (or undefined for the default admin). */
  onSubmit: (target_wp_user_login: string | undefined) => void;
  /** Disable the submit button while the parent's mutation is in flight. */
  pending?: boolean;
  /** Site name to anchor the heading. */
  siteName: string;
}

export function UserPickerModal({
  open,
  onClose,
  onSubmit,
  pending = false,
  siteName,
}: UserPickerModalProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { target_wp_user_login: "" },
  });

  // Sync open prop with the native <dialog> open state. We never touch the
  // ref during render to keep eslint react-hooks happy.
  useEffect(() => {
    const el = dialogRef.current;
    if (!el) return;
    if (open && !el.open) {
      reset({ target_wp_user_login: "" });
      el.showModal();
    }
    if (!open && el.open) el.close();
  }, [open, reset]);

  const onValid = handleSubmit((values) => {
    const v = values.target_wp_user_login?.trim();
    onSubmit(v ? v : undefined);
  });

  return (
    <dialog
      ref={dialogRef}
      onClose={onClose}
      aria-labelledby="user-picker-title"
      className="m-auto w-full max-w-md rounded-xl border border-[var(--color-border)] bg-[var(--color-background)] p-6 text-[var(--color-foreground)] backdrop:bg-black/50"
    >
      <form onSubmit={(e) => void onValid(e)} noValidate className="space-y-4">
        <div>
          <h2 id="user-picker-title" className="text-lg font-semibold">
            Log in as a different user
          </h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Choose which WordPress user to log into <strong>{siteName}</strong>{" "}
            as.
          </p>
        </div>

        <div className="space-y-2">
          <Label htmlFor="target_wp_user_login">WordPress username</Label>
          <Input
            id="target_wp_user_login"
            placeholder="e.g. editor-jane"
            autoComplete="off"
            spellCheck={false}
            aria-invalid={errors.target_wp_user_login ? true : undefined}
            aria-describedby="user-picker-hint"
            {...register("target_wp_user_login")}
          />
          <p
            id="user-picker-hint"
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            We&apos;ll log you in as this WP user. Leave blank to use the first
            administrator.
          </p>
          {errors.target_wp_user_login ? (
            <p role="alert" className="text-sm text-[var(--color-destructive)]">
              {errors.target_wp_user_login.message}
            </p>
          ) : null}
        </div>

        <div className="flex justify-end gap-2">
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button type="submit" disabled={pending}>
            {pending ? "Opening…" : "Open site"}
          </Button>
        </div>
      </form>
    </dialog>
  );
}
