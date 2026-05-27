import { createFileRoute, redirect, useNavigate } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
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
import { getSession, useSessionStore } from "@/lib/session-store";

export const Route = createFileRoute("/login")({
  // If already "signed in", skip the login page.
  beforeLoad: () => {
    if (getSession()) {
      throw redirect({ to: "/sites" });
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
  const signIn = useSessionStore((s) => s.signIn);
  const navigate = useNavigate();

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({
    resolver: zodResolver(loginSchema),
    defaultValues: { email: "", password: "" },
  });

  // NOTE: There is no auth endpoint yet — real authentication lands in
  // Phase 5 / M1. For the skeleton we just stash a fake session in Zustand and
  // route to /sites. No network call, no credential validation.
  const onSubmit = handleSubmit((values) => {
    signIn(values.email);
    void navigate({ to: "/sites" });
  });

  return (
    <main className="flex min-h-dvh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle asChild>
            <h1>Sign in to WPMgr</h1>
          </CardTitle>
          <CardDescription>
            Skeleton login — auth backend arrives in Phase 5.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form
            onSubmit={(e) => void onSubmit(e)}
            noValidate
            className="space-y-4"
          >
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
                <p id="email-error" role="alert" className="text-sm text-[var(--color-destructive)]">
                  {errors.email.message}
                </p>
              ) : null}
            </div>

            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
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
                <p id="password-error" role="alert" className="text-sm text-[var(--color-destructive)]">
                  {errors.password.message}
                </p>
              ) : null}
            </div>

            <Button type="submit" className="w-full" disabled={isSubmitting}>
              Sign in
            </Button>
          </form>
        </CardContent>
      </Card>
    </main>
  );
}
