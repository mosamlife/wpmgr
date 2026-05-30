import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { z } from "zod";

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
import {
  useMe,
  useUpdateProfile,
  useChangePassword,
} from "@/features/auth/use-auth";
import type { Me } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/settings/account")({
  component: AccountSettingsPage,
});

// ---------------------------------------------------------------------------
// Validation schemas
// ---------------------------------------------------------------------------

const profileSchema = z.object({
  name: z.string().min(1, "Name is required").max(200, "Name is too long"),
});

const passwordSchema = z
  .object({
    current_password: z.string().min(1, "Current password is required"),
    new_password: z.string().min(8, "Password must be at least 8 characters"),
    confirm_password: z.string().min(1, "Please confirm your new password"),
  })
  .refine((v) => v.new_password === v.confirm_password, {
    message: "Passwords do not match",
    path: ["confirm_password"],
  });

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

function AccountSettingsPage() {
  const { data: me, isPending, isError, error, refetch } = useMe();

  if (isPending) {
    return (
      <section aria-labelledby="account-heading" className="max-w-2xl space-y-6">
        <PageHeader title="Account" subline="Manage your personal profile and credentials." />
        <div
          role="status"
          aria-label="Loading account settings"
          className="space-y-4"
        >
          <div className="h-36 animate-pulse rounded-xl bg-muted/50" />
          <div className="h-48 animate-pulse rounded-xl bg-muted/50" />
        </div>
      </section>
    );
  }

  if (isError || !me) {
    return (
      <section aria-labelledby="account-heading" className="max-w-2xl space-y-6">
        <PageHeader title="Account" subline="Manage your personal profile and credentials." />
        <PageError
          what="Could not load account details."
          why={error?.message}
          onRetry={() => void refetch()}
          retryLabel="Reload account"
        />
      </section>
    );
  }

  return (
    <section aria-labelledby="account-heading" className="max-w-2xl space-y-6">
      <PageHeader
        title="Account"
        subline="Manage your personal profile and credentials."
      />

      {/* Key the child components on the user id so their form state resets
          if the user ever changes (e.g. admin switching tenants). This keeps
          the form hooks clean without storing server state in local state. */}
      <ProfileCard key={`profile-${me.user.id}`} me={me} />
      <PasswordCard key={`password-${me.user.id}`} />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Profile section
// ---------------------------------------------------------------------------

function ProfileCard({ me }: { me: Me }) {
  const update = useUpdateProfile();

  const [name, setName] = useState(me.user.name ?? "");
  const [nameError, setNameError] = useState<string | null>(null);

  function handleSave() {
    const result = profileSchema.safeParse({ name });
    if (!result.success) {
      setNameError(result.error.issues[0]?.message ?? "Invalid name");
      return;
    }
    setNameError(null);
    update.mutate({ name: result.data.name }, { onError: () => {} });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Profile</CardTitle>
        <CardDescription>
          Your display name is shown in restore history and team member lists.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Email — read-only, shown with copy affordance */}
        <div className="space-y-1.5">
          <Label>Email</Label>
          <div className="flex items-center gap-2">
            <CopyableMono
              value={me.user.email}
              label="Copy email"
              className="flex-1"
            />
          </div>
          <p className="text-xs text-muted-foreground">
            Your sign-in email. Contact support to change it.
          </p>
        </div>

        {/* Name — editable */}
        <div className="space-y-1.5">
          <Label htmlFor="account-name">Name</Label>
          <Input
            id="account-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            aria-invalid={nameError !== null ? true : undefined}
            aria-describedby={nameError ? "account-name-error" : undefined}
            className="max-w-xs"
            placeholder="Your display name"
          />
          {nameError ? (
            <p
              id="account-name-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {nameError}
            </p>
          ) : null}
        </div>

        {update.isError ? (
          <PageError
            what="Could not save profile."
            why={update.error.message}
          />
        ) : null}

        <div>
          <Button
            type="button"
            onClick={handleSave}
            disabled={update.isPending}
          >
            {update.isPending ? "Saving…" : "Save profile"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Change password section
// ---------------------------------------------------------------------------

function PasswordCard() {
  const change = useChangePassword();

  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  function handleUpdate() {
    const result = passwordSchema.safeParse({
      current_password: currentPassword,
      new_password: newPassword,
      confirm_password: confirmPassword,
    });

    if (!result.success) {
      const errs: Record<string, string> = {};
      for (const issue of result.error.issues) {
        const field = issue.path[0];
        if (typeof field === "string" && !errs[field]) {
          errs[field] = issue.message;
        }
      }
      setFieldErrors(errs);
      return;
    }

    setFieldErrors({});
    change.mutate(
      {
        current_password: result.data.current_password,
        new_password: result.data.new_password,
      },
      {
        onSuccess: () => {
          // Clear fields after a successful update so stale passwords
          // don't sit in the DOM.
          setCurrentPassword("");
          setNewPassword("");
          setConfirmPassword("");
        },
        onError: () => {},
      },
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Change password</CardTitle>
        <CardDescription>
          Update the password you use to sign in.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor="current-password">Current password</Label>
          <Input
            id="current-password"
            type="password"
            value={currentPassword}
            onChange={(e) => setCurrentPassword(e.target.value)}
            autoComplete="current-password"
            aria-invalid={fieldErrors["current_password"] ? true : undefined}
            aria-describedby={
              fieldErrors["current_password"]
                ? "current-password-error"
                : undefined
            }
            className="max-w-xs"
          />
          {fieldErrors["current_password"] ? (
            <p
              id="current-password-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["current_password"]}
            </p>
          ) : null}
        </div>

        <div className="space-y-1.5">
          <Label htmlFor="new-password">New password</Label>
          <Input
            id="new-password"
            type="password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            autoComplete="new-password"
            aria-invalid={fieldErrors["new_password"] ? true : undefined}
            aria-describedby={
              fieldErrors["new_password"] ? "new-password-error" : undefined
            }
            className="max-w-xs"
          />
          {fieldErrors["new_password"] ? (
            <p
              id="new-password-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["new_password"]}
            </p>
          ) : null}
        </div>

        <div className="space-y-1.5">
          <Label htmlFor="confirm-password">Confirm new password</Label>
          <Input
            id="confirm-password"
            type="password"
            value={confirmPassword}
            onChange={(e) => setConfirmPassword(e.target.value)}
            autoComplete="new-password"
            aria-invalid={fieldErrors["confirm_password"] ? true : undefined}
            aria-describedby={
              fieldErrors["confirm_password"]
                ? "confirm-password-error"
                : undefined
            }
            className="max-w-xs"
          />
          {fieldErrors["confirm_password"] ? (
            <p
              id="confirm-password-error"
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fieldErrors["confirm_password"]}
            </p>
          ) : null}
        </div>

        {change.isError ? (
          <PageError
            what="Could not update password."
            why={change.error.message}
          />
        ) : null}

        <div>
          <Button
            type="button"
            onClick={handleUpdate}
            disabled={change.isPending}
          >
            {change.isPending ? "Updating…" : "Update password"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
