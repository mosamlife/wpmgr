import { useState } from "react";
import { KeyRound, Link2, Loader2, Lock, Trash2 } from "lucide-react";
import type { ConnectedIdentity } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { PageError } from "@/components/feedback";
import { Skeleton } from "@/components/ui/skeleton";
import { useMe } from "@/features/auth/use-auth";
import { useSignInMethods } from "@/features/auth/social-buttons";
import {
  useConnectedAccounts,
  useUnlinkIdentity,
  useSetInitialPassword,
} from "@/features/auth/use-connected-accounts";

/**
 * Connected accounts: list, add, remove, plus the "add a password" path for an
 * account that only has a provider.
 *
 * THE CARD'S JOB IS TO MAKE THE ACCOUNT'S SIGN-IN METHODS VISIBLE, and to never
 * let someone walk into having none. It hides Disconnect when the server would
 * refuse it (`can_unlink`) and says why in its place, so the refusal arrives
 * before the click rather than after. The server re-decides regardless; this is
 * the explanation, not the enforcement.
 */
export function ConnectedAccountsCard() {
  const { data, isPending, isError, error, refetch } = useConnectedAccounts();

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-4">
          <div>
            <CardTitle>Connected accounts</CardTitle>
            <CardDescription className="mt-1">
              The ways you can sign in to this account.
            </CardDescription>
          </div>
          {data && !data.has_password ? (
            <Badge className="shrink-0 bg-[var(--color-muted)] text-[var(--color-muted-foreground)] border-[var(--color-border)]">
              No password
            </Badge>
          ) : null}
        </div>
      </CardHeader>
      <CardContent className="space-y-5">
        {isPending ? (
          <div className="space-y-2">
            <Skeleton className="h-14 w-full" />
            <Skeleton className="h-14 w-full" />
          </div>
        ) : isError || !data ? (
          <PageError
            what="Could not load connected accounts."
            why={error?.message}
            onRetry={() => void refetch()}
            retryLabel="Reload connected accounts"
          />
        ) : (
          <>
            <ul
              className="divide-y divide-[var(--color-border)]"
              aria-label="Connected sign-in methods"
            >
              <PasswordRow hasPassword={data.has_password} />
              {data.items.map((identity) => (
                <IdentityRow
                  key={identity.provider}
                  identity={identity}
                  canUnlink={data.can_unlink}
                  onlyMethod={!data.has_password && data.items.length === 1}
                />
              ))}
            </ul>

            {!data.has_password ? <SetPasswordForm /> : null}

            <ConnectMore connected={data.items.map((i) => i.provider)} />
          </>
        )}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/**
 * The password is a sign-in method too, so it is listed as one. Showing only
 * providers would leave a social-only account looking identical to one that
 * also has a password, which is the single fact that decides whether the
 * Disconnect buttons below are available.
 */
function PasswordRow({ hasPassword }: { hasPassword: boolean }) {
  return (
    <li className="flex items-center gap-3 py-3">
      <Lock
        aria-hidden="true"
        className="size-4 shrink-0 text-[var(--color-muted-foreground)]"
      />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-[var(--color-foreground)]">Password</p>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {hasPassword
            ? "Set. Change it from the account settings page."
            : "Not set. This account signs in with a provider only."}
        </p>
      </div>
    </li>
  );
}

function IdentityRow({
  identity,
  canUnlink,
  onlyMethod,
}: {
  identity: ConnectedIdentity;
  canUnlink: boolean;
  onlyMethod: boolean;
}) {
  const unlink = useUnlinkIdentity();
  const label = providerLabel(identity.provider);

  return (
    <li className="flex items-center gap-3 py-3">
      <Link2
        aria-hidden="true"
        className="size-4 shrink-0 text-[var(--color-muted-foreground)]"
      />
      <div className="min-w-0 flex-1">
        <p className="truncate text-sm font-medium text-[var(--color-foreground)]">
          {label}
        </p>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {identity.email || "No address reported"}
          {" · Connected "}
          {formatDate(identity.created_at)}
          {identity.last_login_at
            ? ` · Last used ${formatDate(identity.last_login_at)}`
            : " · Not used to sign in yet"}
        </p>
        {unlink.isError ? (
          <p role="alert" className="mt-1 text-xs text-[var(--color-destructive)]">
            {unlink.error.message}
          </p>
        ) : null}
      </div>

      {canUnlink ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => unlink.mutate(identity.provider)}
          disabled={unlink.isPending}
          aria-label={`Disconnect ${label}`}
          className="shrink-0 text-[var(--color-muted-foreground)] hover:text-[var(--color-destructive)]"
        >
          {unlink.isPending ? (
            <Loader2 aria-hidden="true" className="size-4 animate-spin" />
          ) : (
            <Trash2 aria-hidden="true" className="size-4" />
          )}
        </Button>
      ) : (
        // Not a disabled button. A disabled control invites hunting for the
        // thing that would enable it; a sentence names it.
        <p className="max-w-48 shrink-0 text-right text-xs text-[var(--color-muted-foreground)]">
          {onlyMethod
            ? "Your only sign-in method. Set a password to disconnect it."
            : "Cannot be disconnected."}
        </p>
      )}
    </li>
  );
}

// ---------------------------------------------------------------------------
// Add a password
// ---------------------------------------------------------------------------

/**
 * Only rendered for an account that has no password.
 *
 * There is no "current password" field because there is no current password,
 * and no email round trip because password reset deliberately refuses to send
 * one to an account like this: a reset link that can CREATE a password is an
 * account-takeover primitive for anyone who knows the address. Being signed in
 * is the whole authorisation, which is why this lives here and nowhere else.
 */
function SetPasswordForm() {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [mismatch, setMismatch] = useState(false);
  const setInitialPassword = useSetInitialPassword();

  const tooShort = password.length > 0 && password.length < 12;

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setMismatch(true);
      return;
    }
    setMismatch(false);
    setInitialPassword.mutate(password, {
      onSuccess: () => {
        setPassword("");
        setConfirm("");
      },
    });
  }

  return (
    <form
      onSubmit={handleSubmit}
      className="space-y-3 rounded-lg border border-[var(--color-border)] bg-[var(--color-muted)]/30 p-4"
    >
      <div>
        <p className="text-sm font-medium text-[var(--color-foreground)]">
          Set a password
        </p>
        <p className="mt-1 text-sm text-[var(--color-muted-foreground)]">
          A password lets you sign in without your provider, and it is what makes
          disconnecting a provider possible.
        </p>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="new-password">New password</Label>
        <Input
          id="new-password"
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          minLength={12}
          aria-describedby="new-password-hint"
        />
        <p id="new-password-hint" className="text-xs text-[var(--color-muted-foreground)]">
          At least 12 characters.
        </p>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="confirm-password">Confirm password</Label>
        <Input
          id="confirm-password"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          required
        />
      </div>

      {mismatch ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          The two passwords do not match.
        </p>
      ) : null}
      {setInitialPassword.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {setInitialPassword.error.message}
        </p>
      ) : null}

      <Button
        type="submit"
        size="sm"
        disabled={setInitialPassword.isPending || tooShort || password.length === 0}
      >
        {setInitialPassword.isPending ? (
          <>
            <Loader2 aria-hidden="true" className="animate-spin" />
            Setting password…
          </>
        ) : (
          <>
            <KeyRound aria-hidden="true" />
            Set password
          </>
        )}
      </Button>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Add a provider
// ---------------------------------------------------------------------------

/**
 * Connecting another provider is the ordinary sign-in handshake, not a separate
 * API: a full page navigation to the provider and back. Only providers this
 * install has actually configured are offered, and only ones not already
 * connected.
 *
 * THE ADDRESS CAVEAT IS STATED, NOT HIDDEN. A provider account is attached to
 * the WPMgr account holding the same verified address, so signing in with a
 * provider account that uses a DIFFERENT address lands you in a different WPMgr
 * account rather than adding a method to this one. That is surprising enough to
 * belong in the copy next to the button.
 */
function ConnectMore({ connected }: { connected: string[] }) {
  const { data: methods } = useSignInMethods();
  const { data: me } = useMe();

  const available = (methods?.providers ?? []).filter((p) => !connected.includes(p));
  if (available.length === 0) return null;

  return (
    <div className="space-y-2 border-t border-[var(--color-border)] pt-4">
      <p className="text-sm font-medium text-[var(--color-foreground)]">
        Connect another provider
      </p>
      <p className="text-sm text-[var(--color-muted-foreground)]">
        Use the provider account that has the same email address as this account
        {me?.user?.email ? ` (${me.user.email})` : ""}. A provider account with a
        different address signs you in to a different WPMgr account instead.
      </p>
      <div className="flex flex-wrap gap-2 pt-1">
        {available.map((p) => (
          <Button
            key={p}
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              window.location.href = `/auth/social/${p}/start`;
            }}
          >
            Connect {providerLabel(p)}
          </Button>
        ))}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/**
 * Provider keys are control-plane vocabulary. Anything unrecognised (a future
 * provider, or the operator-configured `oidc` issuer) still gets a readable
 * label rather than a raw key.
 */
export function providerLabel(provider: string): string {
  switch (provider) {
    case "google":
      return "Google";
    case "github":
      return "GitHub";
    case "oidc":
      return "Single sign-on";
    default:
      return provider.charAt(0).toUpperCase() + provider.slice(1);
  }
}

function formatDate(iso: string): string {
  try {
    return new Intl.DateTimeFormat("en", {
      day: "numeric",
      month: "short",
      year: "numeric",
    }).format(new Date(iso));
  } catch {
    return iso;
  }
}
