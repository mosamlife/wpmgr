import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { AlertTriangle, Globe, Loader2 } from "lucide-react";
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
import { toError } from "@/features/auth/use-auth";

// Public accept-invitation page — /accept?token=...
//
// This route is NOT under _authed. It must be reachable by logged-out users.
// On success the backend sets a session cookie; we then navigate the user into
// the app (to the shared site or org dashboard depending on scope).
//
// Flow:
//   1. Read ?token from the URL.
//   2. Show a form: email (always) + name + password (if the token is for a
//      new account — detected when submit returns an error asking for password).
//      We optimistically show just email first, then upgrade the form on 422.
//   3. POST /api/v1/invitations/accept {token, email, name?, password?}
//   4. On success navigate to /sites/{site_id} (scope=site) or /sites (scope=org).
//   5. Clear errors for invalid/expired/already-used tokens with clear copy.

const searchSchema = z.object({
  token: z.string().optional(),
});

export const Route = createFileRoute("/accept")({
  validateSearch: searchSchema,
  // If the user is already logged in we still show the page — they may be
  // accepting on behalf of a different account. The backend handles this.
  component: AcceptPage,
});

// ---------------------------------------------------------------------------
// Domain types for the accept response
// ---------------------------------------------------------------------------

interface AcceptResult {
  tenant_id: string;
  scope: "org" | "site";
  site_id?: string;
}

// ---------------------------------------------------------------------------
// Page component
// ---------------------------------------------------------------------------

function AcceptPage() {
  const search = Route.useSearch();
  const token = search.token;
  const navigate = useNavigate();

  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);
  const [tokenError, setTokenError] = useState<string | null>(null);

  // If no token in the URL we show a clear error immediately.
  if (!token) {
    return (
      <InvitationLayout>
        <Card className="w-full max-w-sm">
          <CardHeader className="space-y-1">
            <CardTitle asChild>
              <h1>Invalid invitation</h1>
            </CardTitle>
            <CardDescription>
              This invitation link is missing the required token. Check that
              you copied the full link.
            </CardDescription>
          </CardHeader>
        </Card>
      </InvitationLayout>
    );
  }

  if (tokenError) {
    return (
      <InvitationLayout>
        <Card className="w-full max-w-sm">
          <CardHeader className="space-y-1">
            <CardTitle asChild>
              <h1>Invitation unavailable</h1>
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div
              role="alert"
              className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
            >
              <AlertTriangle
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
              />
              <p className="text-sm text-[var(--color-destructive)]">
                {tokenError}
              </p>
            </div>
          </CardContent>
        </Card>
      </InvitationLayout>
    );
  }

  async function handleSubmit() {
    const trimmedEmail = email.trim();
    if (!trimmedEmail) {
      setServerError("Email is required");
      return;
    }
    if (!password) {
      setServerError(
        "Enter your password — or choose one to create a new account.",
      );
      return;
    }

    setIsSubmitting(true);
    setServerError(null);

    try {
      const body: Record<string, string> = {
        token: token!,
        email: trimmedEmail,
        password,
      };
      if (name.trim()) body["name"] = name.trim();

      const raw = await fetch("/api/v1/invitations/accept", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });

      const json = (await raw.json().catch(() => ({}))) as Record<
        string,
        unknown
      >;

      if (!raw.ok) {
        // NOTE: error bodies are currently empty over the wire (a known session-
        // middleware issue), so branch on STATUS codes, not the body. The
        // optional code/message reads are a no-op today and a bonus once the
        // body is restored.
        const msg =
          typeof json["message"] === "string" ? json["message"] : "";
        const code = typeof json["code"] === "string" ? json["code"] : "";

        // Token gone — not found / expired / already used.
        if (raw.status === 404 || raw.status === 410 || raw.status === 409) {
          setTokenError(
            "This invitation link has expired or has already been used. Ask the sender to create a new one.",
          );
          return;
        }
        if (raw.status === 429) {
          setServerError(
            "Too many attempts. Please wait a moment before trying again.",
          );
          return;
        }
        if (raw.status === 401 || code === "invalid_credentials") {
          setServerError(
            "Incorrect password for this email. If you already have a WPMgr account, enter its password; otherwise choose any password to create one.",
          );
          return;
        }
        if (code === "password_login_unavailable") {
          setServerError(
            "This account uses single sign-on. Sign in first, then open this invite link again.",
          );
          return;
        }
        if (raw.status === 403) {
          setServerError(
            "This email doesn't match the invitation (or the invite has expired). Use the exact address it was sent to.",
          );
          return;
        }
        setServerError(
          msg ||
            "We couldn't accept this invitation. Check your email and password, then try again.",
        );
        return;
      }

      const result = json as unknown as AcceptResult;

      // The session cookie is now set. Navigate into the app.
      if (result.scope === "site" && result.site_id) {
        await navigate({
          to: "/sites/$siteId/health",
          params: { siteId: result.site_id },
        });
      } else {
        await navigate({ to: "/sites" });
      }
    } catch (err) {
      setServerError(toError(err).message);
    } finally {
      setIsSubmitting(false);
    }
  }

  return (
    <InvitationLayout>
      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Accept invitation</h1>
          </CardTitle>
          <CardDescription>
            Enter the email this invitation was sent to and your password. If you
            don't have a WPMgr account yet, the password you choose creates one.
          </CardDescription>
        </CardHeader>

        <CardContent className="space-y-4">
          {serverError ? (
            <div
              role="alert"
              className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
            >
              <AlertTriangle
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
              />
              <p className="text-sm text-[var(--color-destructive)]">
                {serverError}
              </p>
            </div>
          ) : null}

          <div className="space-y-2">
            <Label htmlFor="accept-email">Email address</Label>
            <Input
              id="accept-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="you@example.com"
              autoComplete="email"
              disabled={isSubmitting}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="accept-name">
              Display name{" "}
              <span className="text-xs text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="accept-name"
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Your name"
              autoComplete="name"
              disabled={isSubmitting}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="accept-password">Password</Label>
            <Input
              id="accept-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
              disabled={isSubmitting}
              onKeyDown={(e) => {
                if (e.key === "Enter") void handleSubmit();
              }}
            />
            <p className="text-xs text-muted-foreground">
              If you already have a WPMgr account, enter its password. Otherwise
              this sets the password for your new account.
            </p>
          </div>

          <Button
            type="button"
            className="w-full"
            disabled={isSubmitting || !email.trim() || !password}
            onClick={() => void handleSubmit()}
          >
            {isSubmitting ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                <span>Accepting…</span>
              </>
            ) : (
              "Accept invitation"
            )}
          </Button>
        </CardContent>
      </Card>
    </InvitationLayout>
  );
}

// ---------------------------------------------------------------------------
// Layout wrapper for the accept page (logged-out friendly, no app shell).
// ---------------------------------------------------------------------------

function InvitationLayout({ children }: { children: React.ReactNode }) {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-6 bg-[var(--color-background)] p-4">
      <div className="flex items-center gap-2">
        <Globe aria-hidden="true" className="size-5 text-[var(--color-primary)]" />
        <span className="text-sm font-semibold tracking-tight text-[var(--color-foreground)]">
          WPMgr
        </span>
      </div>
      {children}
    </main>
  );
}
