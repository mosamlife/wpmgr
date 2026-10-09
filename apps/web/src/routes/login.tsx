import { createFileRoute, redirect, useNavigate, Link } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { AlertCircle, AlertTriangle, MailCheck, Timer } from "lucide-react";
import { useState, useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { getMe } from "@wpmgr/api";

import { AuthLayout } from "@/components/layout/auth-layout";
import { SocialButtons, ensureSignInMethods } from "@/features/auth/social-buttons";
import { socialRefusal, sameOriginPath } from "@/features/auth/social-errors";
import { signInTarget } from "@/features/auth/sign-in-target";
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
import {
  ensureMe,
  useLogin,
  useResendVerification,
  EmailNotVerifiedError,
  LoginRateLimitedError,
  authKeys,
} from "@/features/auth/use-auth";

const searchSchema = z.object({
  // Where to land after a successful login (defaults to /sites).
  redirect: z.string().optional(),
  // Set by the social callback when it refuses or fails. `.catch(undefined)`
  // so a stray value can never error the whole route out: this page must
  // always render, it is the way back in.
  social_error: z.string().optional().catch(undefined),
});

export const Route = createFileRoute("/login")({
  validateSearch: searchSchema,
  // If a valid session already exists, skip the login page.
  // Portal users (role==="client") are sent to /portal; everyone else to /sites or
  // the requested redirect path.
  beforeLoad: async ({ context, search }) => {
    // Both in flight together: the method list decides what the page renders,
    // so having it before first paint is what stops buttons being injected
    // under someone's cursor a moment later. It is bounded and cannot reject
    // (see ensureSignInMethods), and it runs alongside the session check rather
    // than after it, so it costs no extra wall time.
    const [me] = await Promise.all([
      ensureMe(context.queryClient),
      ensureSignInMethods(context.queryClient),
    ]);
    if (me) {
      // The deep link keeps its query string and is narrowed to this origin
      // first (signInTarget): ?redirect= is the one search param on this page
      // that a link gets to choose, and every use of it goes through the same
      // narrowing or the one that does not is the hole.
      throw redirect(me.role === "client" ? { to: "/portal" } : signInTarget(search.redirect));
    }
  },
  component: LoginPage,
});

const loginSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(1, "Password is required"),
});

type LoginValues = z.infer<typeof loginSchema>;

// Maps a LoginRateLimitedError's `scope` (login_gate.go's wireScope: "pair" |
// "source" | "network") to the static half of the paused-sign-in message.
// Never mentions whether the account exists -- the gate itself cannot tell,
// and neither can this message. Exported (module-level, pure) so the copy is
// pinned in a unit test without mounting the whole route, mirroring
// `getErrorMessage` in 2fa-challenge.tsx. Kept separate from the countdown
// half (below) so the render tree can put the live-ticking number in its own
// node without string surgery.
export function loginRateLimitReason(scope: string): string {
  if (scope === "pair") return "Too many sign-in attempts for this account.";
  if (scope === "network") return "Too many sign-in attempts from your network.";
  return "Too many sign-in attempts.";
}

/** The full sentence, e.g. "Too many sign-in attempts. Try again in 42 seconds." */
export function loginRateLimitMessage(scope: string, secondsRemaining: number): string {
  const seconds = Math.max(0, secondsRemaining);
  const noun = seconds === 1 ? "second" : "seconds";
  return `${loginRateLimitReason(scope)} Try again in ${seconds} ${noun}.`;
}

function LoginPage() {
  const navigate = useNavigate();
  const search = Route.useSearch();
  const loginMutation = useLogin();
  const resendMutation = useResendVerification();
  const queryClient = useQueryClient();
  // Tracks when login failed because the email is not yet verified.
  const [unverifiedEmail, setUnverifiedEmail] = useState<string | null>(null);
  const [resendSent, setResendSent] = useState(false);
  // GH #718 Phase 1 -- set when the login admission gate refuses an attempt
  // (429 too_many_attempts). `rateLimitDeadline` is a fixed point in time
  // (Date.now() + the server's Retry-After), not a countdown of seconds to
  // decrement: a chain of 1s timeouts drifts behind real time in a
  // throttled background tab, where a deadline does not, because every tick
  // recomputes the remainder from the clock instead of trusting the last
  // tick's arithmetic. `secondsLeft` is that recomputed remainder, purely
  // for display. `rateLimitScope` outlives the countdown on purpose: it is
  // only cleared on the next submit, so the panel below can swap from the
  // countdown to a neutral "you can try again" message at 0 instead of
  // disappearing and letting `serverError` show through underneath.
  const [rateLimitScope, setRateLimitScope] = useState<string | null>(null);
  const [rateLimitDeadline, setRateLimitDeadline] = useState<number | null>(null);
  const [secondsLeft, setSecondsLeft] = useState(0);
  const isRateLimited = rateLimitScope !== null;
  const isPaused = isRateLimited && secondsLeft > 0;

  useEffect(() => {
    if (rateLimitDeadline === null) return;
    const tick = () => {
      const remaining = Math.max(0, Math.ceil((rateLimitDeadline - Date.now()) / 1000));
      setSecondsLeft(remaining);
      return remaining;
    };
    if (tick() <= 0) return;
    const id = window.setInterval(() => {
      if (tick() <= 0) window.clearInterval(id);
    }, 250);
    return () => window.clearInterval(id);
  }, [rateLimitDeadline]);

  // Narrowed once, here, and used for every navigation on this page: the
  // password path, the 2FA hand-off and the provider handshake all land the
  // browser somewhere the query string named.
  const deepLink = sameOriginPath(search.redirect);

  const {
    register,
    handleSubmit,
    getValues,
    watch,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({
    resolver: zodResolver(loginSchema),
    defaultValues: { email: "", password: "" },
  });

  const onSubmit = handleSubmit(async (values) => {
    // Clear any previous unverified/rate-limit state when the user tries
    // again -- a fresh attempt starts from a clean slate rather than
    // carrying the last one's leftover banner while the new one is pending.
    setUnverifiedEmail(null);
    setResendSent(false);
    setRateLimitScope(null);
    setRateLimitDeadline(null);
    setSecondsLeft(0);

    await loginMutation.mutateAsync(values, {
      onSuccess: (result) => {
        if (result.kind === "2fa_required") {
          // Navigate to the challenge page, passing the challenge UUID and
          // which factors are available as search params.
          void navigate({
            to: "/2fa-challenge",
            search: {
              challenge: result.challenge,
              totp: result.factors.totp,
              webauthn: result.factors.webauthn,
              recovery_factor: result.factors.recovery,
              redirect: deepLink,
            },
          });
          return;
        }

        // Force a fresh /auth/me so the middleware-resolved role/scope/portal
        // fields are present (the login response Me may not carry them yet).
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
              void navigate(signInTarget(search.redirect));
            }
          });
      },
      onError: (err) => {
        if (err instanceof EmailNotVerifiedError) {
          setUnverifiedEmail(values.email);
        } else if (err instanceof LoginRateLimitedError) {
          // Set the deadline and the initial `secondsLeft` together so the
          // first paint already shows the right number -- the ticking
          // effect above only refines it from there, it does not originate
          // it, so there is no frame where the countdown reads 0 before the
          // clock-driven tick corrects it.
          setRateLimitScope(err.scope);
          setRateLimitDeadline(Date.now() + err.retryAfterSeconds * 1000);
          setSecondsLeft(err.retryAfterSeconds);
        }
      },
    }).catch(() => {
      // `onError` above is where every failure is actually handled (it is
      // what sets the state each branch above reads); `mutateAsync` rejects
      // its own returned promise on top of calling `onError`, and nothing
      // downstream of this `await` needs that rejection, so left uncaught it
      // is only an unhandled promise rejection on every failed sign-in
      // attempt (any of invalid-credentials, unverified email, or a 429).
    });
  });

  function handleResend() {
    const email = unverifiedEmail ?? getValues("email");
    // Nothing to send to. Reachable from the social refusal, where the callback
    // deliberately does not tell us which address was refused.
    if (!email) return;
    void resendMutation.mutateAsync(
      { email },
      { onError: () => {}, onSuccess: () => setResendSent(true) },
    );
  }

  // The social refusal panel offers the same resend, so the button's enabled
  // state has to track what is typed in the email field.
  const typedEmail = watch("email");

  const refusal = search.social_error
    ? socialRefusal(search.social_error)
    : null;

  const isEmailNotVerified =
    unverifiedEmail !== null ||
    (loginMutation.isError && loginMutation.error instanceof EmailNotVerifiedError);

  const serverError =
    loginMutation.isError && !isEmailNotVerified && !isRateLimited
      ? loginMutation.error.message
      : null;

  return (
    <AuthLayout>
      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Sign in</h1>
          </CardTitle>
          {/* Only the method every install has. It used to name single
              sign-on, which the default install does not offer and now
              correctly does not show a button for, so the line was promising a
              way in that was not on the page. Kept static rather than composed
              from the method list: this sits above the form, so wording that
              changed when that list resolved would move the whole form down,
              which is the shift the block below was rebuilt to avoid. Whatever
              else is available is named on its own button a few lines down. */}
          <CardDescription>Use your email and password.</CardDescription>
        </CardHeader>
        <CardContent>
          <form
            onSubmit={(e) => void onSubmit(e)}
            noValidate
            className="space-y-4"
          >
            {/* A refusal that only a verification link can clear has to offer
                the link. The status-gate refusal sends no mail at all, so the
                page was telling people to open something that was never sent,
                and nothing else here would have sent it: only opening a
                verification link writes email_verified_at, so neither a
                password sign-in nor a reset gets past it. */}
            {refusal ? (
              <div
                role="alert"
                className="flex items-start gap-2.5 rounded-md border border-[var(--color-warning)]/40 bg-[var(--color-warning-subtle)] px-3 py-2.5"
              >
                <AlertTriangle
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-warning-subtle-fg)]"
                />
                <div className="space-y-1.5">
                  <p className="text-sm leading-relaxed text-[var(--color-warning-subtle-fg)]">
                    {refusal.message}
                  </p>
                  {refusal.canResend ? (
                    resendSent ? (
                      <p className="text-sm text-[var(--color-warning-subtle-fg)]">
                        Verification email sent. Open the link, then try again.
                      </p>
                    ) : (
                      <button
                        type="button"
                        className="text-sm font-medium text-[var(--color-warning-subtle-fg)] underline underline-offset-4 disabled:no-underline disabled:opacity-60"
                        disabled={!typedEmail || resendMutation.isPending}
                        onClick={handleResend}
                      >
                        Send the verification email
                      </button>
                    )
                  ) : null}
                </div>
              </div>
            ) : null}

            {isRateLimited ? (
              // role="status" (implicit aria-live="polite") so the pause is
              // announced once, calmly, when it appears -- never role="alert"
              // (assertive), and never every second. The nested
              // aria-live="off" span is what stops the per-second re-render:
              // a descendant's own aria-live overrides the ancestor's for
              // mutations that originate inside it (WAI-ARIA "computing the
              // live region politeness"), so the ticking number is silently
              // dropped from live announcements while the sentence around it
              // was already spoken once, on mount.
              //
              // At 0 this swaps to a neutral confirmation rather than
              // unmounting: the mutation's own error is still
              // LoginRateLimitedError at that point (nothing new has been
              // submitted), and `serverError` below permanently excludes
              // that error, so unmounting this panel would leave nothing in
              // its place while the raw "too_many_attempts" code sat right
              // behind it, ready to show through.
              <div
                role="status"
                className="flex items-start gap-2.5 rounded-md border border-[var(--color-warning)]/40 bg-[var(--color-warning-subtle)] px-3 py-2.5"
              >
                <Timer
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-warning-subtle-fg)]"
                />
                {isPaused ? (
                  <p className="text-sm leading-relaxed text-[var(--color-warning-subtle-fg)]">
                    {rateLimitScope ? loginRateLimitReason(rateLimitScope) : ""}
                    {" Try again in "}
                    <span aria-live="off" className="font-mono tabular-nums">
                      {secondsLeft}
                    </span>
                    {secondsLeft === 1 ? " second." : " seconds."}
                  </p>
                ) : (
                  <p className="text-sm leading-relaxed text-[var(--color-warning-subtle-fg)]">
                    You can try again now.
                  </p>
                )}
              </div>
            ) : null}

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

            {isEmailNotVerified ? (
              <div
                role="alert"
                className="flex items-start gap-2.5 rounded-md border border-[var(--color-primary)]/30 bg-[var(--color-card)] px-3 py-2.5"
              >
                <MailCheck
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-primary)]"
                />
                <div className="space-y-1">
                  <p className="text-sm text-[var(--color-foreground)]">
                    Your email address hasn't been verified yet. Check your
                    inbox for the verification link.
                  </p>
                  {resendSent ? (
                    <p className="text-sm text-[var(--color-muted-foreground)]">
                      Verification email sent.
                    </p>
                  ) : (
                    <button
                      type="button"
                      className="text-sm text-[var(--color-foreground)] underline underline-offset-4 disabled:opacity-50"
                      disabled={resendMutation.isPending}
                      onClick={handleResend}
                    >
                      Resend verification email
                    </button>
                  )}
                </div>
              </div>
            ) : null}

            <div className="space-y-2">
              <Label htmlFor="email">Email</Label>
              <Input
                id="email"
                type="email"
                autoComplete="email"
                aria-invalid={errors.email ? true : undefined}
                aria-describedby={errors.email ? "email-error" : undefined}
                {...register("email")}
              />
              {errors.email ? (
                <p
                  id="email-error"
                  role="alert"
                  className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]"
                >
                  <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
                  {errors.email.message}
                </p>
              ) : null}
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <Label htmlFor="password">Password</Label>
                <Link
                  to="/forgot-password"
                  className="text-xs text-[var(--color-muted-foreground)] underline underline-offset-4 hover:text-[var(--color-foreground)]"
                >
                  Forgot password?
                </Link>
              </div>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                aria-invalid={errors.password ? true : undefined}
                aria-describedby={
                  errors.password ? "password-error" : undefined
                }
                {...register("password")}
              />
              {errors.password ? (
                <p
                  id="password-error"
                  role="alert"
                  className="flex items-center gap-1.5 text-sm text-[var(--color-destructive)]"
                >
                  <AlertCircle aria-hidden="true" className="size-3.5 shrink-0" />
                  {errors.password.message}
                </p>
              ) : null}
            </div>

            <Button
              type="submit"
              className="w-full"
              disabled={isSubmitting || loginMutation.isPending || isPaused}
            >
              Sign in
            </Button>
          </form>

          {/* Providers AND SSO, from one snapshot of what the server offers.
              The SSO button used to be rendered here, below this component,
              which is what let the provider buttons appear above it after
              paint and push it down. `redirect` is the deep link the visitor
              arrived with: the password path above has always honoured it, and
              the social path used to drop it. */}
          <SocialButtons label="Sign in with" redirect={deepLink} sso />

          <p className="mt-4 text-center text-xs text-[var(--color-muted-foreground)]">
            Don't have an account?{" "}
            <Link
              to="/register"
              className="text-[var(--color-foreground)] underline underline-offset-4"
            >
              Sign up
            </Link>
          </p>
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
