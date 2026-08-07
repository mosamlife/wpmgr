import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  AlertTriangle,
  Check,
  Copy,
  Download,
  KeyRound,
  Loader2,
  Monitor,
  RefreshCw,
  ShieldCheck,
  ShieldOff,
  Smartphone,
  Trash2,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import { useMe, isSuperadmin } from "@/features/auth/use-auth";
import {
  useTwoFaStatus,
  useWebAuthnCredentials,
  useTrustedDevices,
  useTotpDisable,
  useDeleteWebAuthnCredential,
  useRegenerateRecoveryCodes,
  useRevokeTrustedDevice,
  useRevokeAllTrustedDevices,
  type TrustedDevice,
  type WebAuthnCredential,
} from "@/features/auth/use-2fa";
import { TotpSetupWizard } from "@/features/auth/totp-setup-wizard";
import { PasskeyRegisterWizard } from "@/features/auth/passkey-register-wizard";
import { ReauthDialog } from "@/features/auth/reauth-dialog";
import { ConnectedAccountsCard } from "@/features/auth/connected-accounts-card";
import { toast } from "@/components/toast";

export const Route = createFileRoute("/_authed/settings/security")({
  component: SecuritySettingsPage,
});

// ---------------------------------------------------------------------------
// Page shell
// ---------------------------------------------------------------------------

function SecuritySettingsPage() {
  const { data: me } = useMe();
  const superadmin = isSuperadmin(me);

  const {
    data: status,
    isPending: statusPending,
    isError: statusError,
    error: statusErr,
    refetch: refetchStatus,
  } = useTwoFaStatus();

  if (statusPending) {
    return (
      <section aria-labelledby="security-heading" className="max-w-2xl space-y-6">
        <PageHeader
          title="Account security"
          subline="Manage two-factor authentication, passkeys, trusted devices, and connected accounts."
        />
        <SkeletonCard />
        <SkeletonCard />
        <SkeletonCard />
      </section>
    );
  }

  if (statusError || !status) {
    return (
      <section aria-labelledby="security-heading" className="max-w-2xl space-y-6">
        <PageHeader
          title="Account security"
          subline="Manage two-factor authentication, passkeys, trusted devices, and connected accounts."
        />
        <PageError
          what="Could not load security settings."
          why={statusErr?.message}
          onRetry={() => void refetchStatus()}
          retryLabel="Reload security settings"
        />
      </section>
    );
  }

  return (
    <section aria-labelledby="security-heading" className="max-w-2xl space-y-6">
      <PageHeader
        title="Account security"
        subline="Manage two-factor authentication, passkeys, trusted devices, and connected accounts."
      />

      {/* Superadmin nudge — non-blocking, only shown when 2FA is not enabled */}
      {superadmin && !status.two_factor_enabled ? (
        <div
          role="note"
          className="flex items-start gap-3 rounded-lg border border-[var(--color-primary)]/30 bg-[var(--color-primary)]/5 px-4 py-3"
        >
          <AlertTriangle
            aria-hidden="true"
            className="mt-0.5 size-4 shrink-0 text-[var(--color-primary)]"
          />
          <div className="space-y-1">
            <p className="text-sm font-medium text-[var(--color-foreground)]">
              Enable two-factor authentication
            </p>
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Your account has superadmin access to all managed sites. Enabling 2FA protects every site in case your password is compromised.
            </p>
          </div>
        </div>
      ) : null}

      {/* 2FA status summary */}
      <TwoFaStatusCard enabled={status.two_factor_enabled} />

      {/* Authenticator app (TOTP) */}
      <TotpCard
        enabled={status.totp_enabled}
        twoFaEnabled={status.two_factor_enabled}
      />

      {/* Passkeys */}
      <PasskeysCard webauthnCount={status.webauthn_count} />

      {/* Recovery codes */}
      <RecoveryCodesCard codesRemaining={status.recovery_codes_remaining} twoFaEnabled={status.two_factor_enabled} />

      {/* Trusted devices */}
      <TrustedDevicesCard />

      {/* Connected accounts: what this account can sign in with */}
      <ConnectedAccountsCard />
    </section>
  );
}

// ---------------------------------------------------------------------------
// 2FA status card
// ---------------------------------------------------------------------------

function TwoFaStatusCard({ enabled }: { enabled: boolean }) {
  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between gap-4">
          <div>
            <CardTitle>Two-factor authentication</CardTitle>
            <CardDescription className="mt-1">
              Add a second layer of security to your account.
            </CardDescription>
          </div>
          <Badge
            className={
              enabled
                ? "bg-[var(--success,theme(colors.green.600))]/10 text-[var(--success,theme(colors.green.600))] border-[var(--success,theme(colors.green.600))]/20"
                : "bg-[var(--color-muted)] text-[var(--color-muted-foreground)] border-[var(--color-border)]"
            }
          >
            {enabled ? (
              <>
                <ShieldCheck aria-hidden="true" className="size-3" />
                Enabled
              </>
            ) : (
              <>
                <ShieldOff aria-hidden="true" className="size-3" />
                Disabled
              </>
            )}
          </Badge>
        </div>
      </CardHeader>
      {!enabled && (
        <CardContent>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Two-factor authentication is disabled. Add an authenticator app or passkey below to enable it.
          </p>
        </CardContent>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// TOTP card
// ---------------------------------------------------------------------------

function TotpCard({
  enabled,
  twoFaEnabled,
}: {
  enabled: boolean;
  twoFaEnabled: boolean;
}) {
  const [wizardOpen, setWizardOpen] = useState(false);
  const [reauthOpen, setReauthOpen] = useState(false);
  const { refetch: refetchStatus } = useTwoFaStatus();
  const disableMutation = useTotpDisable();

  async function handleDisable(password: string) {
    await disableMutation.mutateAsync({ current_password: password });
    void refetchStatus();
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-4">
          <div>
            <CardTitle>Authenticator app</CardTitle>
            <CardDescription className="mt-1">
              Generate time-based one-time codes with an app like Google Authenticator or Authy.
            </CardDescription>
          </div>
          <Badge
            className={
              enabled
                ? "shrink-0 bg-[var(--success,theme(colors.green.600))]/10 text-[var(--success,theme(colors.green.600))] border-[var(--success,theme(colors.green.600))]/20"
                : "shrink-0 bg-[var(--color-muted)] text-[var(--color-muted-foreground)] border-[var(--color-border)]"
            }
          >
            {enabled ? "Configured" : "Not configured"}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="flex gap-2">
        {!enabled ? (
          <Button type="button" size="sm" onClick={() => setWizardOpen(true)}>
            Set up authenticator app
          </Button>
        ) : (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setReauthOpen(true)}
          >
            Disable authenticator app
          </Button>
        )}
      </CardContent>

      <TotpSetupWizard open={wizardOpen} onClose={() => setWizardOpen(false)} />

      <ReauthDialog
        open={reauthOpen}
        onClose={() => setReauthOpen(false)}
        onConfirm={handleDisable}
        title="Disable authenticator app"
        confirmLabel="Disable TOTP"
        description={
          twoFaEnabled && !reauthOpen
            ? undefined
            : "Enter your current password to disable the authenticator app. All trusted devices will be revoked."
        }
      />
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Passkeys card
// ---------------------------------------------------------------------------

function PasskeysCard({ webauthnCount }: { webauthnCount: number }) {
  const [wizardOpen, setWizardOpen] = useState(false);
  const {
    data: credentials,
    isPending,
    isError,
    error,
    refetch,
  } = useWebAuthnCredentials();
  const { refetch: refetchStatus } = useTwoFaStatus();

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-4">
          <div>
            <CardTitle>Passkeys and security keys</CardTitle>
            <CardDescription className="mt-1">
              Sign in using your device's biometrics or a hardware security key.
            </CardDescription>
          </div>
          {webauthnCount > 0 && (
            <Badge className="shrink-0 bg-[var(--success,theme(colors.green.600))]/10 text-[var(--success,theme(colors.green.600))] border-[var(--success,theme(colors.green.600))]/20 font-mono tabular-nums">
              {webauthnCount} {webauthnCount === 1 ? "key" : "keys"}
            </Badge>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {isPending ? (
          <div className="space-y-2">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : isError ? (
          <PageError
            what="Could not load passkeys."
            why={error?.message}
            onRetry={() => void refetch()}
            retryLabel="Reload passkeys"
          />
        ) : credentials && credentials.length > 0 ? (
          <ul className="divide-y divide-[var(--color-border)]" aria-label="Your passkeys">
            {credentials.map((cred) => (
              <PasskeyRow
                key={cred.id}
                credential={cred}
                onRemoved={() => { void refetch(); void refetchStatus(); }}
              />
            ))}
          </ul>
        ) : (
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No passkeys added yet.
          </p>
        )}

        <Button type="button" size="sm" onClick={() => setWizardOpen(true)}>
          <KeyRound aria-hidden="true" />
          Add passkey
        </Button>
      </CardContent>

      <PasskeyRegisterWizard
        open={wizardOpen}
        onClose={() => { setWizardOpen(false); void refetch(); void refetchStatus(); }}
      />
    </Card>
  );
}

function PasskeyRow({
  credential,
  onRemoved,
}: {
  credential: WebAuthnCredential;
  onRemoved: () => void;
}) {
  const [reauthOpen, setReauthOpen] = useState(false);
  const deleteMutation = useDeleteWebAuthnCredential();

  async function handleRemove(password: string) {
    await deleteMutation.mutateAsync({
      id: credential.id,
      current_password: password,
    });
    onRemoved();
  }

  return (
    <li className="flex items-center gap-3 py-3">
      <KeyRound aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
      <div className="flex-1 min-w-0">
        <p className="text-sm font-medium text-[var(--color-foreground)] truncate">
          {credential.name}
        </p>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          Added {formatDate(credential.created_at)}
          {credential.last_used_at ? ` · Last used ${formatDate(credential.last_used_at)}` : ""}
        </p>
      </div>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={() => setReauthOpen(true)}
        aria-label={`Remove passkey ${credential.name}`}
        className="shrink-0 text-[var(--color-muted-foreground)] hover:text-[var(--color-destructive)]"
      >
        <Trash2 aria-hidden="true" className="size-4" />
      </Button>

      <ReauthDialog
        open={reauthOpen}
        onClose={() => setReauthOpen(false)}
        onConfirm={handleRemove}
        title={`Remove "${credential.name}"`}
        confirmLabel="Remove passkey"
        description="Enter your current password to remove this passkey."
      />
    </li>
  );
}

// ---------------------------------------------------------------------------
// Recovery codes card
// ---------------------------------------------------------------------------

function RecoveryCodesCard({
  codesRemaining,
  twoFaEnabled,
}: {
  codesRemaining: number;
  twoFaEnabled: boolean;
}) {
  const [reauthOpen, setReauthOpen] = useState(false);
  const [newCodes, setNewCodes] = useState<string[]>([]);
  const [allCopied, setAllCopied] = useState(false);
  const regenerateMutation = useRegenerateRecoveryCodes();
  const { refetch: refetchStatus } = useTwoFaStatus();

  async function handleRegenerate(password: string) {
    const result = await regenerateMutation.mutateAsync({ current_password: password });
    setNewCodes(result.recovery_codes);
    void refetchStatus();
    toast.success("Recovery codes regenerated");
  }

  function copyAll() {
    void navigator.clipboard.writeText(newCodes.join("\n")).then(() => {
      setAllCopied(true);
      window.setTimeout(() => setAllCopied(false), 1500);
    });
  }

  function downloadCodes() {
    const content = [
      "WPMgr Recovery Codes",
      "====================",
      "Keep these codes safe. Each code can only be used once.",
      "",
      ...newCodes,
    ].join("\n");
    const blob = new Blob([content], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "wpmgr-recovery-codes.txt";
    a.click();
    URL.revokeObjectURL(url);
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-4">
          <div>
            <CardTitle>Recovery codes</CardTitle>
            <CardDescription className="mt-1">
              Use recovery codes to access your account if you lose your second factor.
            </CardDescription>
          </div>
          {twoFaEnabled && (
            <Badge
              className={
                codesRemaining <= 2
                  ? "shrink-0 bg-[var(--color-destructive)]/10 text-[var(--color-destructive)] border-[var(--color-destructive)]/20 font-mono tabular-nums"
                  : "shrink-0 bg-[var(--color-muted)] text-[var(--color-muted-foreground)] border-[var(--color-border)] font-mono tabular-nums"
              }
            >
              {codesRemaining} of 10 remaining
            </Badge>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {!twoFaEnabled ? (
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Enable two-factor authentication to receive recovery codes.
          </p>
        ) : (
          <>
            {codesRemaining <= 2 && (
              <div
                role="alert"
                className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
              >
                <AlertTriangle
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
                />
                <p className="text-sm text-[var(--color-destructive)]">
                  You have only {codesRemaining} recovery {codesRemaining === 1 ? "code" : "codes"} left. Regenerate them now.
                </p>
              </div>
            )}

            {/* Show newly-regenerated codes once */}
            {newCodes.length > 0 ? (
              <div className="space-y-3">
                <div
                  role="alert"
                  className="flex items-start gap-2.5 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-card)] px-3 py-2.5"
                >
                  <AlertTriangle
                    aria-hidden="true"
                    className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
                  />
                  <p className="text-sm text-[var(--color-destructive)]">
                    These codes are shown once. Save them now.
                  </p>
                </div>

                <div className="grid grid-cols-2 gap-1.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-muted)]/30 p-3">
                  {newCodes.map((code, i) => (
                    <code
                      key={i}
                      className="font-mono text-sm tabular-nums tracking-wider text-[var(--color-foreground)]"
                    >
                      {code}
                    </code>
                  ))}
                </div>

                <div className="flex flex-wrap gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={copyAll}
                  >
                    {allCopied ? (
                      <>
                        <Check aria-hidden="true" className="size-3.5" />
                        Copied
                      </>
                    ) : (
                      <>
                        <Copy aria-hidden="true" className="size-3.5" />
                        Copy all
                      </>
                    )}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={downloadCodes}
                  >
                    <Download aria-hidden="true" className="size-3.5" />
                    Download .txt
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() => setNewCodes([])}
                  >
                    Dismiss
                  </Button>
                </div>
              </div>
            ) : (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setReauthOpen(true)}
                disabled={regenerateMutation.isPending}
              >
                {regenerateMutation.isPending ? (
                  <>
                    <Loader2 aria-hidden="true" className="animate-spin" />
                    Regenerating…
                  </>
                ) : (
                  <>
                    <RefreshCw aria-hidden="true" />
                    Regenerate recovery codes
                  </>
                )}
              </Button>
            )}
          </>
        )}
      </CardContent>

      <ReauthDialog
        open={reauthOpen}
        onClose={() => setReauthOpen(false)}
        onConfirm={handleRegenerate}
        title="Regenerate recovery codes"
        confirmLabel="Regenerate codes"
        description="Enter your current password to generate a new set of recovery codes. All previous codes will be invalidated immediately."
      />
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Trusted devices card
// ---------------------------------------------------------------------------

function TrustedDevicesCard() {
  const {
    data: devices,
    isPending,
    isError,
    error,
    refetch,
  } = useTrustedDevices();
  const { refetch: refetchStatus } = useTwoFaStatus();
  const revokeOneMutation = useRevokeTrustedDevice();
  const revokeAllMutation = useRevokeAllTrustedDevices();
  const [revokingId, setRevokingId] = useState<string | null>(null);

  async function handleRevokeOne(id: string) {
    setRevokingId(id);
    try {
      await revokeOneMutation.mutateAsync(id);
      void refetch();
      void refetchStatus();
    } finally {
      setRevokingId(null);
    }
  }

  async function handleRevokeAll() {
    await revokeAllMutation.mutateAsync();
    void refetch();
    void refetchStatus();
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-4">
          <div>
            <CardTitle>Trusted devices</CardTitle>
            <CardDescription className="mt-1">
              Devices that have been granted a 30-day bypass of two-factor verification.
            </CardDescription>
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {isPending ? (
          <div className="space-y-2">
            <Skeleton className="h-14 w-full" />
            <Skeleton className="h-14 w-full" />
          </div>
        ) : isError ? (
          <PageError
            what="Could not load trusted devices."
            why={error?.message}
            onRetry={() => void refetch()}
            retryLabel="Reload trusted devices"
          />
        ) : !devices || devices.length === 0 ? (
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No trusted devices. Check "Remember this device for 30 days" at sign-in to add one.
          </p>
        ) : (
          <>
            <ul className="divide-y divide-[var(--color-border)]" aria-label="Trusted devices">
              {devices.map((device) => (
                <TrustedDeviceRow
                  key={device.id}
                  device={device}
                  isRevoking={revokingId === device.id}
                  onRevoke={() => void handleRevokeOne(device.id)}
                />
              ))}
            </ul>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void handleRevokeAll()}
              disabled={revokeAllMutation.isPending}
              className="text-[var(--color-destructive)] hover:text-[var(--color-destructive)]"
            >
              {revokeAllMutation.isPending ? (
                <>
                  <Loader2 aria-hidden="true" className="animate-spin" />
                  Revoking all…
                </>
              ) : (
                "Revoke all trusted devices"
              )}
            </Button>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function TrustedDeviceRow({
  device,
  isRevoking,
  onRevoke,
}: {
  device: TrustedDevice;
  isRevoking: boolean;
  onRevoke: () => void;
}) {
  const isMobile = /Mobile|Android|iPhone|iPad/.test(device.user_agent);

  return (
    <li className="flex items-center gap-3 py-3">
      {isMobile ? (
        <Smartphone aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
      ) : (
        <Monitor aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
      )}
      <div className="flex-1 min-w-0">
        <p className="text-sm font-medium text-[var(--color-foreground)] truncate">
          {device.label || "Unknown device"}
        </p>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {device.ip} · Added {formatDate(device.created_at)}
          {device.last_used_at ? ` · Last used ${formatDate(device.last_used_at)}` : ""}
          {" · Expires " + formatDate(device.expires_at)}
        </p>
      </div>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={onRevoke}
        disabled={isRevoking}
        aria-label={`Revoke trusted device ${device.label || "unknown"}`}
        className="shrink-0 text-[var(--color-muted-foreground)] hover:text-[var(--color-destructive)]"
      >
        {isRevoking ? (
          <Loader2 aria-hidden="true" className="animate-spin size-4" />
        ) : (
          <Trash2 aria-hidden="true" className="size-4" />
        )}
      </Button>
    </li>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

function SkeletonCard() {
  return (
    <div className="rounded-xl border border-[var(--color-border)] p-6 space-y-3">
      <Skeleton className="h-5 w-48" />
      <Skeleton className="h-4 w-72" />
      <Skeleton className="h-8 w-36" />
    </div>
  );
}
