import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
//
// GH #286: per-site default login user. When the caller (AutoLoginButton)
// already knows the site's configured default, the hint copy surfaces it,
// and a "Make this the default" checkbox lets the operator persist whatever
// they type as the new default without blocking the login itself on the
// save. The checkbox is disabled (and force-unchecked) while the username
// field is blank, so a blank login can never silently clear a configured
// default from this dialog. Clearing the default is a deliberate action,
// available only from the Settings panel's explicit empty-to-clear field.

const schema = z.object({
  // WordPress user_login: allow letters, digits, underscore, period, hyphen,
  // and @ (for email-style logins). WordPress's users.user_login column is
  // varchar(60), matched server-side by the autologin-policy endpoint.
  target_wp_user_login: z
    .string()
    .max(60, "Max 60 characters.")
    .regex(/^[a-zA-Z0-9_.\-@]*$/, "Only letters, digits, and . _ - @ are allowed.")
    .optional()
    .or(z.literal("")),
  make_default: z.boolean().optional(),
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
  /**
   * The site's current default login user, when the caller knows it.
   * `undefined`/`null` (unknown) keeps the original first-administrator
   * copy and hides the "Make this the default" checkbox; `""` means a
   * policy exists with no default set; a non-empty string is folded into
   * the hint text (never used as the input placeholder, so the field can
   * never be mistaken for pre-filled).
   */
  defaultLoginUser?: string | null;
  /**
   * Persists the submitted username as the site's new default. Fired only
   * when "Make this the default" is checked, which is only possible with a
   * non-blank username (the checkbox is disabled while the field is blank),
   * so this is never called with an empty string. Fired in parallel with
   * `onSubmit`; the login is never delayed waiting on this save. Only
   * offered when the caller passes this prop.
   */
  onSaveDefault?: (username: string) => void;
}

export function UserPickerModal({
  open,
  onClose,
  onSubmit,
  pending = false,
  siteName,
  defaultLoginUser,
  onSaveDefault,
}: UserPickerModalProps) {
  const {
    register,
    handleSubmit,
    reset,
    setValue,
    watch,
    formState: { errors },
  } = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { target_wp_user_login: "", make_default: false },
  });

  // Reset the form whenever the dialog re-opens so a previous unsuccessful
  // login attempt doesn't pre-fill the next operator's intent.
  useEffect(() => {
    if (open) reset({ target_wp_user_login: "", make_default: false });
  }, [open, reset]);

  // Narrowed once so both the placeholder and hint text below can read a
  // plain `string | null` instead of re-deriving the check inline.
  const knownDefault: string | null =
    typeof defaultLoginUser === "string" && defaultLoginUser !== ""
      ? defaultLoginUser
      : null;
  const canSetDefault = onSaveDefault !== undefined;

  // A blank username can never fire the save-default path. The checkbox is
  // disabled while the field is blank, and its value is force-cleared the
  // moment the field empties, so a checked-then-cleared field can't leave a
  // stale `true` sitting in form state.
  const usernameValue = watch("target_wp_user_login");
  const isUsernameBlank = !usernameValue || usernameValue.trim() === "";

  useEffect(() => {
    if (isUsernameBlank) setValue("make_default", false);
  }, [isUsernameBlank, setValue]);

  const onValid = handleSubmit((values) => {
    const v = values.target_wp_user_login?.trim();
    onSubmit(v ? v : undefined);
    if (v && values.make_default && onSaveDefault) {
      // Non-blocking: fired alongside onSubmit, never awaited before it.
      onSaveDefault(v);
    }
  });

  return (
    <Dialog open={open} onClose={onClose}>
      <DialogContent ariaLabelledBy="user-picker-title">
        <form onSubmit={(e) => void onValid(e)} noValidate>
          <DialogHeader>
            <DialogTitle id="user-picker-title">
              Open site as another user
            </DialogTitle>
            <DialogDescription>
              Choose which WordPress user to log into{" "}
              <strong className="text-[var(--color-foreground)]">
                {siteName}
              </strong>{" "}
              as.
            </DialogDescription>
          </DialogHeader>

          <DialogBody className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="target_wp_user_login">WordPress username</Label>
              <Input
                id="target_wp_user_login"
                placeholder="wp_username"
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
                {knownDefault ? (
                  <>
                    Leave blank to use the site default (
                    <span className="font-mono">{knownDefault}</span>).
                  </>
                ) : (
                  <>
                    We&apos;ll log you in as this WP user. Leave blank to use
                    the first administrator.
                  </>
                )}
              </p>
              {errors.target_wp_user_login ? (
                <p
                  role="alert"
                  className="text-sm text-[var(--color-destructive)]"
                >
                  {errors.target_wp_user_login.message}
                </p>
              ) : null}
            </div>

            {canSetDefault ? (
              <div className="space-y-1">
                <label className="flex items-center gap-2 text-sm">
                  <Checkbox
                    {...register("make_default")}
                    disabled={isUsernameBlank}
                    aria-describedby={
                      isUsernameBlank
                        ? "user-picker-make-default-help"
                        : undefined
                    }
                  />
                  <span
                    className={
                      isUsernameBlank
                        ? "text-[var(--color-muted-foreground)]"
                        : undefined
                    }
                  >
                    Make this the default for this site
                  </span>
                </label>
                {isUsernameBlank ? (
                  <p
                    id="user-picker-make-default-help"
                    className="text-xs text-[var(--color-muted-foreground)]"
                  >
                    Enter a username to set it as the default.
                  </p>
                ) : null}
              </div>
            ) : null}
          </DialogBody>

          <DialogFooter className="pt-2">
            <Button
              type="button"
              variant="outline"
              onClick={onClose}
              disabled={pending}
            >
              Close
            </Button>
            <Button type="submit" disabled={pending}>
              {pending ? "Opening…" : "Open site"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
