import { createFileRoute, redirect, useNavigate } from "@tanstack/react-router";
import { z } from "zod";
import { useState, useRef, useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertCircle, AlertTriangle, KeyRound, Loader2, ShieldCheck } from "lucide-react";

import { AuthLayout } from "@/components/layout/auth-layout";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { getMe } from "@wpmgr/api";

import { ensureMe, authKeys } from "@/features/auth/use-auth";
import {
  useTotpChallenge,
  useRecoveryChallenge,
  useWebAuthnBeginChallenge,
  useWebAuthnFinishChallenge,
} from "@/features/auth/use-2fa";

// ---------------------------------------------------------------------------
// Search-param schema — the login page passes challenge + factors
// ---------------------------------------------------------------------------

const searchSchema = z.object({
  challenge: z.string(),
  totp: z.boolean().optional().default(false),
  webauthn: z.boolean().optional().default(false),
  recovery_factor: z.boolean().optional().default(false),
  redirect: z.string().optional(),
});

export const Route = createFileRoute("/2fa-challenge")({
  validateSearch: searchSchema,
  // If already authenticated, skip the challenge page.
  beforeLoad: async ({ context, search }) => {
    const me = await ensureMe(context.queryClient);
    if (me) {
      throw redirect({ to: search.redirect ?? "/sites" });
    }
  },
  component: TwoFaChallengePage,
});

// ---------------------------------------------------------------------------
// View modes
// ---------------------------------------------------------------------------

type Mode = "totp" | "recovery" | "webauthn";

// Maps a challenge-mutation error `code` to house-style, actionable copy.
// Exported (module-level, pure) so GH #215's fix can be pinned in a unit test
// without mounting the whole route + form flow; mirrors `mapDeleteOrgError`
// in `features/orgs/use-orgs.ts`.
export function getErrorMessage(code: string): string {
  switch (code) {
    case "challenge_expired":
      return "This login session has expired. Please sign in again.";
    case "too_many_attempts":
      return "Too many failed attempts. This session is locked. Please sign in again.";
    case "invalid_code":
      return "Incorrect code. Please check and try again.";
    case "codes_exhausted":
      return "All recovery codes have been used. Contact your administrator.";
    case "recovery_code_already_used":
      return "That recovery code has already been used.";
    case "totp_decrypt_failed":
      // The server can no longer decrypt the stored TOTP secret (its
      // secrets-at-rest key changed on a self-host, GH #215) -- a server
      // configuration problem, not a wrong code. Recovery codes are hashed
      // (not encrypted), so they still work here; point the operator at the
      // "Use a recovery code" switcher already rendered on this screen.
      return "Your two-factor secret couldn't be decrypted because the server's encryption key changed. This is a server configuration issue, not a wrong code. Sign in with a recovery code below, then re-enroll two-factor after pinning a stable encryption key (WPMGR_SITE_DEST_AGE_SECRET).";
    default:
      return "Verification failed. Please try again.";
  }
}

function TwoFaChallengePage() {
  const search = Route.useSearch();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const factors = {
    totp: search.totp,
    webauthn: search.webauthn,
    recovery: search.recovery_factor,
  };

  // Pick a default mode based on available factors.
  const defaultMode: Mode =
    factors.webauthn ? "webauthn" :
    factors.totp ? "totp" :
    "recovery";

  const [mode, setMode] = useState<Mode>(defaultMode);
  const [rememberDevice, setRememberDevice] = useState(false);

  // Shared error / success handling
  const [errorCode, setErrorCode] = useState<string | null>(null);

  const isExpiredOrLocked =
    errorCode === "challenge_expired" || errorCode === "too_many_attempts";

  function handleSuccess(_me: import("@wpmgr/api").Me) {
    // Mirror the password-login path: force a FRESH /auth/me with the now-set
    // session cookie (the challenge response Me can omit middleware-resolved
    // role/scope fields), then navigate only after it resolves so the route
    // guards observe an authenticated session instead of racing it. Seeding the
    // cache and navigating immediately bounced to /login until a manual reload.
    void queryClient
      .fetchQuery({
        queryKey: authKeys.me,
        queryFn: async () => {
          const { data } = await getMe();
          return data ?? null;
        },
        staleTime: 0,
      })
      .then((freshMe) => {
        if (freshMe?.role === "client") {
          void navigate({ to: "/portal" });
        } else {
          void navigate({ to: search.redirect ?? "/sites" });
        }
      });
  }

  return (
    <AuthLayout>
      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Two-factor verification</h1>
          </CardTitle>
          <CardDescription>
            {mode === "totp" && "Enter the 6-digit code from your authenticator app."}
            {mode === "webauthn" && "Use your passkey or security key to verify."}
            {mode === "recovery" && "Enter one of your recovery codes."}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-5">
          {/* Error banner */}
          {errorCode ? (
            <div
              role="alert"
              className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
            >
              <AlertTriangle
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
              />
              <p className="text-sm text-[var(--color-destructive)]">
                {getErrorMessage(errorCode)}
              </p>
            </div>
          ) : null}

          {isExpiredOrLocked ? (
            <Button
              type="button"
              variant="outline"
              className="w-full"
              onClick={() => void navigate({ to: "/login" })}
            >
              Back to sign in
            </Button>
          ) : (
            <>
              {/* Active factor view */}
              {mode === "totp" && (
                <TotpView
                  challenge={search.challenge}
                  rememberDevice={rememberDevice}
                  onSuccess={handleSuccess}
                  onError={setErrorCode}
                />
              )}
              {mode === "webauthn" && (
                <WebAuthnView
                  challenge={search.challenge}
                  rememberDevice={rememberDevice}
                  onSuccess={handleSuccess}
                  onError={setErrorCode}
                />
              )}
              {mode === "recovery" && (
                <RecoveryView
                  challenge={search.challenge}
                  rememberDevice={rememberDevice}
                  onSuccess={handleSuccess}
                  onError={setErrorCode}
                />
              )}

              {/* Remember device checkbox */}
              <div className="flex items-center gap-2.5">
                <Checkbox
                  id="remember-device"
                  checked={rememberDevice}
                  onChange={(e) => setRememberDevice(e.target.checked)}
                />
                <Label htmlFor="remember-device" className="text-sm font-normal cursor-pointer">
                  Remember this device for 30 days
                </Label>
              </div>

              {/* Factor switcher */}
              <div className="border-t border-[var(--color-border)] pt-4 space-y-2">
                {factors.webauthn && mode !== "webauthn" && (
                  <button
                    type="button"
                    onClick={() => { setErrorCode(null); setMode("webauthn"); }}
                    className="flex w-full items-center gap-2 text-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] underline underline-offset-4"
                  >
                    <KeyRound aria-hidden="true" className="size-3.5" />
                    Use a passkey or security key
                  </button>
                )}
                {factors.totp && mode !== "totp" && (
                  <button
                    type="button"
                    onClick={() => { setErrorCode(null); setMode("totp"); }}
                    className="flex w-full items-center gap-2 text-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] underline underline-offset-4"
                  >
                    <ShieldCheck aria-hidden="true" className="size-3.5" />
                    Use an authenticator app
                  </button>
                )}
                {mode !== "recovery" && (
                  <button
                    type="button"
                    onClick={() => { setErrorCode(null); setMode("recovery"); }}
                    className="text-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] underline underline-offset-4"
                  >
                    Use a recovery code
                  </button>
                )}
              </div>

              <p className="text-center text-xs text-[var(--color-muted-foreground)]">
                <button
                  type="button"
                  onClick={() => void navigate({ to: "/login" })}
                  className="underline underline-offset-4 hover:text-[var(--color-foreground)]"
                >
                  Back to sign in
                </button>
              </p>
            </>
          )}
        </CardContent>
      </Card>
    </AuthLayout>
  );
}

// ---------------------------------------------------------------------------
// TOTP sub-view — numeric 6-digit code input
// ---------------------------------------------------------------------------

function TotpView({
  challenge,
  rememberDevice,
  onSuccess,
  onError,
}: {
  challenge: string;
  rememberDevice: boolean;
  onSuccess: (me: import("@wpmgr/api").Me) => void;
  onError: (code: string) => void;
}) {
  const [code, setCode] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const mutation = useTotpChallenge();

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = code.replace(/\s/g, "");
    if (trimmed.length !== 6) return;

    try {
      const result = await mutation.mutateAsync({
        challenge,
        code: trimmed,
        remember_device: rememberDevice,
      });
      onSuccess(result.me);
    } catch (err) {
      const e = err as Error & { code?: string };
      onError(e.code ?? "unknown");
    }
  }

  const codeError =
    code.length > 0 && code.replace(/\s/g, "").length !== 6
      ? "Enter the full 6-digit code"
      : null;

  return (
    <form onSubmit={(e) => void handleSubmit(e)} noValidate className="space-y-4">
      <div className="space-y-2">
        <Label htmlFor="totp-code">Authentication code</Label>
        <Input
          ref={inputRef}
          id="totp-code"
          type="text"
          inputMode="numeric"
          autoComplete="one-time-code"
          placeholder="123456"
          maxLength={6}
          value={code}
          onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
          aria-invalid={codeError ? true : undefined}
          aria-describedby={codeError ? "totp-code-error" : undefined}
          className="font-mono tabular-nums text-lg tracking-widest max-w-[180px]"
        />
        {codeError ? (
          <p id="totp-code-error" role="alert" className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]">
            <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
            {codeError}
          </p>
        ) : null}
      </div>
      <Button
        type="submit"
        className="w-full"
        disabled={code.replace(/\s/g, "").length !== 6 || mutation.isPending}
      >
        {mutation.isPending ? (
          <>
            <Loader2 aria-hidden="true" className="animate-spin" />
            Verifying…
          </>
        ) : (
          "Verify"
        )}
      </Button>
    </form>
  );
}

// ---------------------------------------------------------------------------
// Recovery sub-view
// ---------------------------------------------------------------------------

function RecoveryView({
  challenge,
  rememberDevice,
  onSuccess,
  onError,
}: {
  challenge: string;
  rememberDevice: boolean;
  onSuccess: (me: import("@wpmgr/api").Me) => void;
  onError: (code: string) => void;
}) {
  const [code, setCode] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const mutation = useRecoveryChallenge();

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = code.trim();
    if (!trimmed) return;

    try {
      const result = await mutation.mutateAsync({
        challenge,
        code: trimmed,
        remember_device: rememberDevice,
      });
      onSuccess(result.me);
    } catch (err) {
      const e = err as Error & { code?: string };
      onError(e.code ?? "unknown");
    }
  }

  return (
    <form onSubmit={(e) => void handleSubmit(e)} noValidate className="space-y-4">
      <div className="space-y-2">
        <Label htmlFor="recovery-code">Recovery code</Label>
        <Input
          ref={inputRef}
          id="recovery-code"
          type="text"
          autoComplete="off"
          placeholder="AAAAA-BBBBB"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          aria-invalid={mutation.isError ? true : undefined}
          className="font-mono tabular-nums"
        />
        <p className="text-xs text-[var(--color-muted-foreground)]">
          Each recovery code can only be used once.
        </p>
      </div>
      <Button
        type="submit"
        className="w-full"
        disabled={!code.trim() || mutation.isPending}
      >
        {mutation.isPending ? (
          <>
            <Loader2 aria-hidden="true" className="animate-spin" />
            Verifying…
          </>
        ) : (
          "Use recovery code"
        )}
      </Button>
    </form>
  );
}

// ---------------------------------------------------------------------------
// WebAuthn sub-view — browser ceremony via @github/webauthn-json
// ---------------------------------------------------------------------------

function WebAuthnView({
  challenge,
  rememberDevice,
  onSuccess,
  onError,
}: {
  challenge: string;
  rememberDevice: boolean;
  onSuccess: (me: import("@wpmgr/api").Me) => void;
  onError: (code: string) => void;
}) {
  const beginMutation = useWebAuthnBeginChallenge();
  const finishMutation = useWebAuthnFinishChallenge();
  const [browserSupported] = useState(() => typeof window !== "undefined" && !!window.PublicKeyCredential);
  const [isRunning, setIsRunning] = useState(false);

  // Auto-trigger if WebAuthn is the default (only) factor.
  const hasAutoTriggered = useRef(false);
  useEffect(() => {
    if (browserSupported && !hasAutoTriggered.current) {
      hasAutoTriggered.current = true;
      void handleAssert();
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function handleAssert() {
    if (!browserSupported) return;
    setIsRunning(true);

    try {
      // 1. Get options from server
      const options = await beginMutation.mutateAsync({ challenge });

      // 2. Run browser assertion ceremony using @github/webauthn-json
      const { get } = await import("@github/webauthn-json");
      // The server returns the standard `{ publicKey: {...} }` envelope already
      // (go-webauthn CredentialAssertion), so pass it straight through — wrapping
      // it again buries `rp`/`challenge` a level too deep ("Missing key" errors).
      const credential = await get(options);

      // 3. Serialize the assertion back to JSON and base64-encode it for the server.
      // The contract says `assertion` is `[]byte` (base64-encoded raw JSON).
      const assertionJson = JSON.stringify(credential);
      const assertionB64 = btoa(assertionJson);

      // 4. Finish on the server
      const me = await finishMutation.mutateAsync({
        challenge,
        assertion: assertionB64,
        remember_device: rememberDevice,
      });
      onSuccess(me);
    } catch (err) {
      setIsRunning(false);
      // DOMException: user cancelled or platform error — not a server error
      if (err instanceof DOMException) {
        if (err.name === "NotAllowedError") {
          // User cancelled — not an error we surface as a code
          return;
        }
        onError("webauthn_failed");
        return;
      }
      const e = err as Error & { code?: string };
      onError(e.code ?? "unknown");
    }
  }

  if (!browserSupported) {
    return (
      <div
        role="alert"
        className="flex items-start gap-2.5 rounded-md border border-[var(--color-border)] bg-[var(--color-card)] px-3 py-2.5"
      >
        <AlertCircle aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-[var(--color-muted-foreground)]" />
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Your browser does not support passkeys. Use a different factor to sign in.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <p className="text-sm text-[var(--color-muted-foreground)]">
        Your browser will prompt you to use your passkey or security key.
      </p>
      <Button
        type="button"
        className="w-full"
        onClick={() => void handleAssert()}
        disabled={isRunning || beginMutation.isPending || finishMutation.isPending}
      >
        {isRunning || beginMutation.isPending || finishMutation.isPending ? (
          <>
            <Loader2 aria-hidden="true" className="animate-spin" />
            Waiting for passkey…
          </>
        ) : (
          <>
            <KeyRound aria-hidden="true" />
            Use passkey or security key
          </>
        )}
      </Button>
      {(beginMutation.isError || finishMutation.isError) ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {beginMutation.error?.message ?? finishMutation.error?.message}
        </p>
      ) : null}
    </div>
  );
}
