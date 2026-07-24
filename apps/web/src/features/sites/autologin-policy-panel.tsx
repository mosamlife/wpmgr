import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { toast } from "@/components/toast";
import { useMe } from "@/features/auth/use-auth";
import { canAutoLogin } from "@/features/sites/use-autologin";
import type { SiteAutologinPolicy, SiteAutologinPolicyUpdate } from "@wpmgr/api";

import { useAutologinPolicy, useUpdateAutologinPolicy } from "./use-autologin-policy";

// GH #286: default "Login As User" policy panel.
//
// Rendered inline on the Settings tab's Access section, directly under the
// AutoLoginButton (no dialog, same pattern as login-brand-panel.tsx /
// login-protection-panel.tsx). Follows the "key on loaded config" pattern so
// form state is always initialised from server data without setState-in-
// render or setState-in-effect.
//
// Two layers:
//   AutologinPolicyPanel: gates on role, fetches, renders loading/error
//   AutologinPolicyLoaded: keyed on config, owns all local state
//
// Design rules:
//   - "Allow one-click login on this site" switch bound to `enabled`. This is
//     the home for the policy_disabled toast's "Enable it in site settings"
//     hint.
//   - "Default login user" input: font-mono, max 60 chars, WordPress's
//     username charset. Blank clears the default (first-administrator
//     fallback). Caveat: the failure surfaces on the site itself, not here.
//   - `allowed_wp_roles` is read-only and shown only as a cheap informational
//     note (never editable; the API rejects it on PUT).
//   - Verb-first Save button; pending state; "Last saved <time>" footer.

const USERNAME_MAX = 60;
const USERNAME_PATTERN = /^[a-zA-Z0-9_.\-@]*$/;

function validateUsername(value: string): string | null {
  if (value.length > USERNAME_MAX) {
    return `Must be at most ${USERNAME_MAX} characters.`;
  }
  if (!USERNAME_PATTERN.test(value)) {
    return "Only letters, digits, and . _ - @ are allowed.";
  }
  return null;
}

// ---------------------------------------------------------------------------
// Shell: gates on role, fetches config, delegates to loaded or loading/error
// ---------------------------------------------------------------------------

export function AutologinPolicyPanel({ siteId }: { siteId: string }) {
  const { data: me } = useMe();
  const allowed = canAutoLogin(me);

  const { data, isPending, isError, error, refetch } = useAutologinPolicy(
    siteId,
    { enabled: allowed },
  );

  // Same role floor as the mint button (`site:autologin`, owner/admin). The
  // panel simply does not render for operators/viewers rather than firing a
  // GET that would 403.
  if (!allowed) return null;

  if (isPending) {
    return <AutologinPolicySkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load the default login user."
        why={error instanceof Error ? error.message : "Unknown error"}
        onRetry={() => void refetch()}
        retryLabel="Reload settings"
      />
    );
  }

  if (!data) return null;

  // Key on the editable fields so the form resets automatically if a
  // background refetch returns a different value (e.g. another operator
  // saved changes).
  const configKey = `${data.enabled}|${data.default_wp_user_login}`;

  return (
    <AutologinPolicyLoaded key={configKey} siteId={siteId} initialConfig={data} />
  );
}

// ---------------------------------------------------------------------------
// Loaded form
// ---------------------------------------------------------------------------

interface LoadedProps {
  siteId: string;
  initialConfig: SiteAutologinPolicy;
}

function AutologinPolicyLoaded({ siteId, initialConfig }: LoadedProps) {
  const update = useUpdateAutologinPolicy(siteId);

  const [enabled, setEnabled] = useState(initialConfig.enabled);
  const [defaultUser, setDefaultUser] = useState(
    initialConfig.default_wp_user_login,
  );
  const [defaultUserError, setDefaultUserError] = useState<string | null>(
    null,
  );
  const [saveError, setSaveError] = useState<string | null>(null);

  function handleSave() {
    const trimmed = defaultUser.trim();
    const err = validateUsername(trimmed);
    if (err) {
      setDefaultUserError(err);
      return;
    }
    setDefaultUserError(null);
    setSaveError(null);

    const body: SiteAutologinPolicyUpdate = {
      enabled,
      default_wp_user_login: trimmed,
    };

    update.mutate(body, {
      onSuccess: () => {
        toast.success("Login settings saved.");
      },
      onError: (err: Error) => {
        setSaveError(err.message);
      },
    });
  }

  return (
    <div className="space-y-6">
      {/* ── Enable switch ── */}
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0 flex-1">
          <label
            htmlFor="alp-enabled"
            className="block text-sm font-medium text-[var(--color-foreground)]"
          >
            Allow one-click login on this site
          </label>
          <p
            id="alp-enabled-help"
            className="mt-0.5 text-xs text-[var(--color-muted-foreground)]"
          >
            Turn off to block every one-click login attempt for this site,
            regardless of who requests it.
          </p>
        </div>
        <Switch
          id="alp-enabled"
          checked={enabled}
          onCheckedChange={setEnabled}
          aria-describedby="alp-enabled-help"
        />
      </div>

      {/* ── Default login user ── */}
      <div className="space-y-1.5">
        <label
          htmlFor="alp-default-user"
          className="block text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Default login user
        </label>
        <Input
          id="alp-default-user"
          value={defaultUser}
          onChange={(e) => {
            setDefaultUser(e.target.value);
            setDefaultUserError(null);
          }}
          placeholder="e.g. editor-jane"
          autoComplete="off"
          spellCheck={false}
          aria-describedby={
            defaultUserError ? "alp-default-user-err" : "alp-default-user-help"
          }
          aria-invalid={defaultUserError !== null}
          className="font-mono text-sm"
        />
        {defaultUserError ? (
          <p
            id="alp-default-user-err"
            role="alert"
            className="text-sm text-[var(--color-destructive)]"
          >
            {defaultUserError}
          </p>
        ) : (
          <p
            id="alp-default-user-help"
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            Used whenever an operator opens "Log in to site" without picking a
            user. Leave blank to log in as the site's first administrator.
          </p>
        )}
        <p className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/30 px-4 py-3 text-xs text-[var(--color-muted-foreground)]">
          If this user is removed or demoted on the site itself, one-click
          login fails until the default is updated here. That failure happens
          on the site, not in this dashboard.
        </p>
        {initialConfig.allowed_wp_roles.length > 0 ? (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Roles eligible as login targets:{" "}
            <span className="font-mono">
              {initialConfig.allowed_wp_roles.join(", ")}
            </span>
            . Managed by the server, not editable here.
          </p>
        ) : null}
      </div>

      {/* ── Save error ── */}
      {saveError ? (
        <PageError what="Could not save login settings." why={saveError} />
      ) : null}

      {/* ── Save footer ── */}
      <div className="flex items-center gap-3 border-t border-[var(--color-border)] pt-6">
        <Button
          type="button"
          onClick={handleSave}
          disabled={update.isPending}
          aria-busy={update.isPending}
        >
          {update.isPending ? "Saving..." : "Save login settings"}
        </Button>
        {initialConfig.updated_at ? (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Last saved{" "}
            <time dateTime={initialConfig.updated_at}>
              {new Date(initialConfig.updated_at).toLocaleString()}
            </time>
          </p>
        ) : (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Not yet saved. Using the first-administrator fallback.
          </p>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function AutologinPolicySkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading default login user"
      className="space-y-6"
    >
      <span className="sr-only">Loading default login user</span>
      <div className="flex items-center justify-between gap-4">
        <Skeleton className="h-4 w-56" />
        <Skeleton className="h-5 w-9 rounded-full" />
      </div>
      <div className="space-y-2">
        <Skeleton className="h-3 w-32" />
        <Skeleton className="h-9 w-full" />
      </div>
    </div>
  );
}
