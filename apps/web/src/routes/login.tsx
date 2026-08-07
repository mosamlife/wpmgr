import { createFileRoute, redirect, useNavigate, Link } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { AlertCircle, AlertTriangle, MailCheck } from "lucide-react";
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { getMe } from "@wpmgr/api";

import { AuthLayout } from "@/components/layout/auth-layout";
import { SocialButtons } from "@/features/auth/social-buttons";
import { socialErrorMessage } from "@/features/auth/social-errors";
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
    const me = await ensureMe(context.queryClient);
    if (me) {
      throw redirect({
        to: me.role === "client" ? "/portal" : (search.redirect ?? "/sites"),
      });
    }
  },
  component: LoginPage,
});

const loginSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(1, "Password is required"),
});

type LoginValues = z.infer<typeof loginSchema>;

function LoginPage() {
  const navigate = useNavigate();
  const search = Route.useSearch();
  const loginMutation = useLogin();
  const resendMutation = useResendVerification();
  const queryClient = useQueryClient();
  // Tracks when login failed because the email is not yet verified.
  const [unverifiedEmail, setUnverifiedEmail] = useState<string | null>(null);
  const [resendSent, setResendSent] = useState(false);

  const {
    register,
    handleSubmit,
    getValues,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({
    resolver: zodResolver(loginSchema),
    defaultValues: { email: "", password: "" },
  });

  const onSubmit = handleSubmit(async (values) => {
    // Clear any previous unverified state when the user tries again.
    setUnverifiedEmail(null);
    setResendSent(false);

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
              redirect: search.redirect,
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
              void navigate({ to: search.redirect ?? "/sites" });
            }
          });
      },
      onError: (err) => {
        if (err instanceof EmailNotVerifiedError) {
          setUnverifiedEmail(values.email);
        }
      },
    });
  });

  function handleResend() {
    const email = unverifiedEmail ?? getValues("email");
    void resendMutation.mutateAsync(
      { email },
      { onError: () => {}, onSuccess: () => setResendSent(true) },
    );
  }

  // Begin OIDC login via a full-page redirect to the backend, which 302s to the
  // provider. If OIDC is unconfigured the backend returns 501; the user simply
  // lands on that response and can navigate back — we keep the button always
  // visible to avoid a config probe on the login screen.
  function signInWithSso() {
    window.location.href = "/api/auth/oidc/login";
  }

  const isEmailNotVerified =
    unverifiedEmail !== null ||
    (loginMutation.isError && loginMutation.error instanceof EmailNotVerifiedError);

  const serverError =
    loginMutation.isError && !isEmailNotVerified
      ? loginMutation.error.message
      : null;

  return (
    <AuthLayout>
      <Card className="w-full max-w-sm">
        <CardHeader className="space-y-1">
          <CardTitle asChild>
            <h1>Sign in</h1>
          </CardTitle>
          <CardDescription>
            Use your email and password, or single sign-on.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form
            onSubmit={(e) => void onSubmit(e)}
            noValidate
            className="space-y-4"
          >
            {search.social_error ? (
              <div
                role="alert"
                className="flex items-start gap-2.5 rounded-md border border-[var(--color-warning)]/40 bg-[var(--color-warning-subtle)] px-3 py-2.5"
              >
                <AlertTriangle
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-warning-subtle-fg)]"
                />
                <p className="text-sm leading-relaxed text-[var(--color-warning-subtle-fg)]">
                  {socialErrorMessage(search.social_error)}
                </p>
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
              disabled={isSubmitting || loginMutation.isPending}
            >
              Sign in
            </Button>
          </form>

          <SocialButtons label="Sign in with" />

          <Button
            type="button"
            variant="outline"
            className="mt-2 w-full"
            onClick={signInWithSso}
          >
            Sign in with SSO
          </Button>

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
