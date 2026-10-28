import { useState, useId } from "react";
import {
  Eye,
  EyeOff,
  CheckCircle2,
  ExternalLink,
  RefreshCw,
  Building2,
  Trash2,
  AlertTriangle,
} from "lucide-react";

import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  CardDescription,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import type {
  PutEmailConfigRequest,
  EmailProviderSpec,
  EmailProviderField,
  RemoveSiteEmailConfigResult,
  SiteEmailConfig,
} from "@wpmgr/api";
import {
  useProviders,
  usePutEmailConfig,
  useEmailConfig,
  useOrgEmailConfig,
  useSyncEmailConfig,
  useRemoveSiteEmailConfig,
  removalNeedsSync,
  EmailConfigStaleError,
} from "./use-email";
import { EmailWebhookConfigCard } from "./email-webhook-config";

// ---------------------------------------------------------------------------
// Provider picker
// ---------------------------------------------------------------------------

export interface ProviderPickerProps {
  value: string;
  providers: EmailProviderSpec[];
  onChange: (slug: string) => void;
  disabled?: boolean;
}

export function ProviderPicker({
  value,
  providers,
  onChange,
  disabled,
}: ProviderPickerProps) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>Provider</Label>
      <Select
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
      >
        <option value="">Select a provider</option>
        {providers.map((p) => (
          <option key={p.slug} value={p.slug}>
            {p.label}
          </option>
        ))}
      </Select>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Dynamic field renderer — per-provider field schema from the catalog
// ---------------------------------------------------------------------------

export interface DynamicFieldProps {
  field: EmailProviderField;
  /** Current non-secret config values keyed by field.key */
  config: Record<string, unknown>;
  /** Current secret input value (write-only, never prefilled) */
  secretValue: string;
  /** Whether the secret is already stored (secret_set from the GET) */
  secretSet: boolean;
  /**
   * Whether the stored secret belongs to the organisation rather than this
   * site (`inherited` from the GET). When it is true, `secretSet` describes a
   * credential this site does not own, so the field says so instead of
   * claiming the site is configured.
   */
  inherited?: boolean;
  /**
   * Whether the endpoint has been edited away from the organisation's. The
   * organisation credential is only sent to the endpoint it was issued for, so
   * a diverged config needs a password of its own.
   */
  endpointDiverged?: boolean;
  /** Whether the operator has opened the "Replace" affordance */
  replacingSecret: boolean;
  onConfigChange: (key: string, value: unknown) => void;
  onSecretChange: (value: string) => void;
  onStartReplaceSecret: () => void;
  disabled?: boolean;
}

export function DynamicField({
  field,
  config,
  secretValue,
  secretSet,
  inherited = false,
  endpointDiverged = false,
  replacingSecret,
  onConfigChange,
  onSecretChange,
  onStartReplaceSecret,
  disabled,
}: DynamicFieldProps) {
  const id = useId();
  const [showSecret, setShowSecret] = useState(false);

  if (field.is_secret) {
    // Secret fields are write-only. Show an indicator when one is stored and an
    // affordance to enter a new value. The GET response NEVER returns the
    // actual value — only secret_set, plus inherited to say whose it is.
    const showStoredIndicator = secretSet && !replacingSecret;
    return (
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={id}>
          {field.label}
          {field.is_required && (
            <span className="ml-1 text-[var(--color-destructive)]">*</span>
          )}
        </Label>
        {showStoredIndicator ? (
          <div className="flex items-center gap-2">
            {inherited ? (
              <Badge variant="muted" className="gap-1">
                <Building2 aria-hidden="true" className="size-3" />
                From organisation
              </Badge>
            ) : (
              <Badge variant="success" className="gap-1">
                <CheckCircle2 aria-hidden="true" className="size-3" />
                Configured
              </Badge>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onStartReplaceSecret}
              disabled={disabled}
            >
              {inherited ? "Use a different one" : "Replace"}
            </Button>
          </div>
        ) : (
          <div className="relative">
            <Input
              id={id}
              type={showSecret ? "text" : "password"}
              value={secretValue}
              onChange={(e) => onSecretChange(e.target.value)}
              placeholder={
                field.help ?? `Enter ${field.label.toLowerCase()}`
              }
              autoComplete="new-password"
              className="pr-9"
              disabled={disabled}
              aria-describedby={field.help ? `${id}-help` : undefined}
            />
            <button
              type="button"
              aria-label={showSecret ? "Hide secret" : "Show secret"}
              onClick={() => setShowSecret((v) => !v)}
              className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            >
              {showSecret ? (
                <EyeOff aria-hidden="true" className="size-4" />
              ) : (
                <Eye aria-hidden="true" className="size-4" />
              )}
            </button>
          </div>
        )}
        {inherited && endpointDiverged && secretValue.trim() === "" ? (
          <p className="text-xs text-[var(--color-destructive)]">
            These settings no longer match the organisation&rsquo;s, so the
            organisation credential will not be sent to them. Enter a{" "}
            {field.label.toLowerCase()} for this site, or restore the
            organisation settings.
          </p>
        ) : inherited && showStoredIndicator ? (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            This site uses the organisation credential. It is only sent while
            the settings above still match the organisation&rsquo;s.
          </p>
        ) : null}
        {field.help ? (
          <p
            id={`${id}-help`}
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            {field.help}
          </p>
        ) : null}
      </div>
    );
  }

  const rawValue = config[field.key] ?? field.default ?? "";
  const currentValue =
    typeof rawValue === "string" ||
    typeof rawValue === "number" ||
    typeof rawValue === "boolean"
      ? rawValue
      : "";

  if (field.type === "select" && field.options && field.options.length > 0) {
    return (
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={id}>
          {field.label}
          {field.is_required && (
            <span className="ml-1 text-[var(--color-destructive)]">*</span>
          )}
        </Label>
        <Select
          id={id}
          value={String(currentValue)}
          onChange={(e) => onConfigChange(field.key, e.target.value)}
          disabled={disabled}
          aria-describedby={field.help ? `${id}-help` : undefined}
        >
          {field.options.map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </Select>
        {field.help ? (
          <p
            id={`${id}-help`}
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            {field.help}
          </p>
        ) : null}
      </div>
    );
  }

  if (field.type === "boolean") {
    const checked = Boolean(currentValue === "true" || currentValue === true);
    return (
      <div className="flex items-center justify-between gap-3">
        <div className="flex flex-col gap-0.5">
          <Label htmlFor={id} className="cursor-pointer">
            {field.label}
            {field.is_required && (
              <span className="ml-1 text-[var(--color-destructive)]">*</span>
            )}
          </Label>
          {field.help ? (
            <p className="text-xs text-[var(--color-muted-foreground)]">
              {field.help}
            </p>
          ) : null}
        </div>
        <Switch
          id={id}
          checked={checked}
          onCheckedChange={(v) => onConfigChange(field.key, v)}
          disabled={disabled}
          aria-label={field.label}
        />
      </div>
    );
  }

  if (field.type === "number") {
    return (
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={id}>
          {field.label}
          {field.is_required && (
            <span className="ml-1 text-[var(--color-destructive)]">*</span>
          )}
        </Label>
        <Input
          id={id}
          type="number"
          value={String(currentValue)}
          onChange={(e) => onConfigChange(field.key, e.target.value)}
          disabled={disabled}
          aria-describedby={field.help ? `${id}-help` : undefined}
        />
        {field.help ? (
          <p
            id={`${id}-help`}
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            {field.help}
          </p>
        ) : null}
      </div>
    );
  }

  // Default: text input
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>
        {field.label}
        {field.is_required && (
          <span className="ml-1 text-[var(--color-destructive)]">*</span>
        )}
      </Label>
      <Input
        id={id}
        type="text"
        value={String(currentValue)}
        onChange={(e) => onConfigChange(field.key, e.target.value)}
        disabled={disabled}
        aria-describedby={field.help ? `${id}-help` : undefined}
      />
      {field.help ? (
        <p
          id={`${id}-help`}
          className="text-xs text-[var(--color-muted-foreground)]"
        >
          {field.help}
        </p>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Inherited-credential rules (mirror of the control plane)
// ---------------------------------------------------------------------------

/**
 * The config fields that decide where a credential is sent and who it
 * authenticates as, per provider. This mirrors credentialAudienceFields in
 * apps/api/internal/email/service.go: the control plane only lends the
 * organisation credential to a site whose config still matches the
 * organisation's on every one of these, so this is what the page watches in
 * order to warn honestly rather than promise a credential that will not be sent.
 *
 * It is exported so that credential-audience-contract.test.ts can hold it
 * against the Go table it mirrors, in both directions. A copy of a security rule
 * that only has to agree with itself is how a warning drifts into promising a
 * credential the server will refuse to send, or into staying silent about one it
 * is about to revoke.
 */
export const CREDENTIAL_AUDIENCE_FIELDS: Record<string, string[]> = {
  smtp: ["host", "port", "username", "encryption", "auth"],
  ses: ["access_key", "region"],
  mailgun: ["domain_name", "region"],
  sendgrid: [],
  postmark: [],
};

/**
 * Normalises one config value for comparison. Every audience field in the
 * catalog is a string, number or boolean, and the two sides come from the same
 * JSONB column, so comparing their printed form is enough. It also absorbs the
 * one real mismatch: a number input writes back the string "587" over an
 * organisation row holding the number 587.
 */
function audienceValue(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  // No audience field is a structured value; comparing the serialised form is
  // the honest fallback rather than "[object Object]" for everything.
  return JSON.stringify(v) ?? "";
}

export function endpointDivergedFromOrg(
  provider: string,
  config: Record<string, unknown>,
  orgProvider: string,
  orgConfig: Record<string, unknown>,
): boolean {
  if (provider !== orgProvider) return true;
  const fields = CREDENTIAL_AUDIENCE_FIELDS[provider];
  // An unknown provider slug is never treated as a match, exactly as the
  // control plane refuses to share a credential it cannot reason about.
  if (fields === undefined) return true;
  return fields.some(
    (key) => audienceValue(config[key]) !== audienceValue(orgConfig[key]),
  );
}

// ---------------------------------------------------------------------------
// Provider Config Card (main export)
// ---------------------------------------------------------------------------

interface EmailProviderConfigProps {
  siteId: string;
}

export function EmailProviderConfig({ siteId }: EmailProviderConfigProps) {
  const configQuery = useEmailConfig(siteId);
  const providersQuery = useProviders();
  const save = usePutEmailConfig(siteId);
  const sync = useSyncEmailConfig(siteId);
  const remove = useRemoveSiteEmailConfig(siteId);

  // Local state mirrors the server config. We derive it from the query data
  // once it loads. Per-field "saving" state is tracked per field so row-level
  // feedback is possible without a global flip.
  const [selectedProvider, setSelectedProvider] = useState("");
  const [fromAddress, setFromAddress] = useState("");
  const [fromName, setFromName] = useState("");
  const [forceFromEmail, setForceFromEmail] = useState(false);
  const [forceFromName, setForceFromName] = useState(false);
  const [returnPath, setReturnPath] = useState(false);
  const [logEmails, setLogEmails] = useState(false);
  const [storeBody, setStoreBody] = useState(false);
  const [retentionDays, setRetentionDays] = useState(14);
  const [providerConfig, setProviderConfig] = useState<Record<string, unknown>>(
    {},
  );
  const [secretValue, setSecretValue] = useState("");
  const [replacingSecret, setReplacingSecret] = useState(false);
  const [initialized, setInitialized] = useState(false);
  /**
   * The organisation's provider + config as first loaded, kept only when this
   * site has no config of its own. An inherited GET response IS the org row, so
   * this needs no second request, and it is what the divergence warning
   * compares against as the operator edits.
   */
  const [orgBaseline, setOrgBaseline] = useState<{
    provider: string;
    config: Record<string, unknown>;
  } | null>(null);
  // The provider named by the open "Remove provider" confirmation. The label is
  // kept after the dialog closes so it does not go blank during its exit.
  const [removeDialog, setRemoveDialog] = useState({ open: false, label: "" });
  // The outcome of the last removal, kept while it is still waiting for a sync.
  const [removal, setRemoval] = useState<RemoveSiteEmailConfigResult | null>(
    null,
  );
  // Set once a removal, or a refusal that found this page out of date, is done:
  // the form is then seeded again from the server (see below).
  const [reseedPending, setReseedPending] = useState(false);

  /**
   * Seeds every field from the server's answer. `null` is "no config at all",
   * the empty form a site that was never configured starts with (the useState
   * defaults above).
   */
  function seedFrom(cfg: SiteEmailConfig | null) {
    setSelectedProvider(cfg?.provider ?? "");
    setFromAddress(cfg?.from_address ?? "");
    setFromName(cfg?.from_name ?? "");
    setForceFromEmail(cfg?.force_from_email ?? false);
    setForceFromName(cfg?.force_from_name ?? false);
    setReturnPath(cfg?.return_path ?? false);
    setLogEmails(cfg?.log_emails ?? false);
    setStoreBody(cfg?.store_body ?? false);
    setRetentionDays(cfg?.retention_days ?? 14);
    setProviderConfig(cfg?.config ?? {});
    setSecretValue("");
    setReplacingSecret(false);
    setOrgBaseline(
      cfg?.inherited === true
        ? { provider: cfg.provider ?? "", config: cfg.config ?? {} }
        : null,
    );
  }

  // Populate local state from server data once on first load
  const serverConfig = configQuery.data;
  if (serverConfig && !initialized) {
    seedFrom(serverConfig);
    setInitialized(true);
  }

  // The cached config is the server's current answer only while it is neither
  // being read again nor failed to be.
  const configSettled = !configQuery.isFetching && !configQuery.isError;

  // After a removal the form has to show what the server holds now, not the
  // settings that were just removed: the organisation's, or nothing at all.
  // useRemoveSiteEmailConfig settles only once the config has been read again,
  // so the answer is current as soon as it is settled. A failed read waits
  // here for the retry.
  if (reseedPending && configSettled && configQuery.data !== undefined) {
    setReseedPending(false);
    seedFrom(configQuery.data);
    setInitialized(configQuery.data !== null);
  }

  // True while the stored credential shown on this page belongs to the
  // organisation rather than to this site.
  const inherited = serverConfig?.inherited === true;
  // The site has a provider of its own, which is the only thing a removal can
  // remove. An inherited config is the organisation's and is never touched.
  const ownsProvider = Boolean(serverConfig) && !inherited;
  // A site with a provider of its own again has nothing left to sync.
  if (removal !== null && configSettled && ownsProvider) {
    setRemoval(null);
  }
  const endpointDiverged =
    orgBaseline !== null &&
    endpointDivergedFromOrg(
      selectedProvider,
      providerConfig,
      orgBaseline.provider,
      orgBaseline.config,
    );

  function handleProviderChange(slug: string) {
    setSelectedProvider(slug);
    // Reset provider-specific config when switching providers
    setProviderConfig({});
    setSecretValue("");
    setReplacingSecret(false);
  }

  function handleConfigChange(key: string, value: unknown) {
    setProviderConfig((prev) => ({ ...prev, [key]: value }));
  }

  function buildPayload(): PutEmailConfigRequest {
    const body: PutEmailConfigRequest = {
      provider: selectedProvider,
      from_address: fromAddress,
      from_name: fromName,
      force_from_email: forceFromEmail,
      force_from_name: forceFromName,
      return_path: returnPath,
      log_emails: logEmails,
      store_body: storeBody,
      retention_days: retentionDays,
      config: providerConfig,
    };
    // Nil-sentinel: only include `secret` when the operator has explicitly
    // typed a new value. An omitted `secret` preserves the stored credential.
    if (secretValue.trim() !== "") {
      body.secret = secretValue;
    } else if (replacingSecret && secretValue === "") {
      // Operator opened the Replace affordance but left it blank — preserve
      // existing credential (do not send empty string which clears it).
    }
    return body;
  }

  function openRemoveConfirmation(label: string) {
    // A refusal from an earlier try must not greet the next one.
    remove.reset();
    setRemoveDialog({ open: true, label });
  }

  async function confirmRemoval() {
    try {
      const result = await remove.mutateAsync();
      setRemoval(result);
      setReseedPending(true);
      setRemoveDialog((prev) => ({ ...prev, open: false }));
    } catch (err) {
      if (err instanceof EmailConfigStaleError) {
        // This page was out of date: the server holds something else now, and
        // the config has been read again. The confirmation was about the
        // provider this page used to show, so it is closed rather than left
        // standing with that name and the answer typed for it; the next removal
        // opens a new one about what is saved now. The server's message moves
        // to the page (staleRefusal), and the form is seeded again.
        setReseedPending(true);
        setRemoveDialog((prev) => ({ ...prev, open: false }));
      }
      // Any other refusal changed nothing: the dialog stays open and shows the
      // server's message (remove.error), ready for another try.
    }
  }

  function syncAfterRemoval() {
    sync.mutate(undefined, {
      onSuccess: (result) => {
        if (result?.ok) setRemoval(null);
      },
    });
  }

  const isPending = configQuery.isPending || providersQuery.isPending;
  const isError = configQuery.isError || providersQuery.isError;

  if (isPending) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
      </div>
    );
  }

  if (isError) {
    return (
      <PageError
        what="Could not load email configuration."
        why={
          (configQuery.error ?? providersQuery.error)?.message ??
          "Unknown error"
        }
        onRetry={() => {
          void configQuery.refetch();
          void providersQuery.refetch();
        }}
        retryLabel="Reload config"
      />
    );
  }

  const providers = providersQuery.data?.providers ?? [];
  const currentProviderSpec = providers.find(
    (p) => p.slug === selectedProvider,
  );
  // What a removal removes is the provider the site has SAVED, which is not
  // necessarily the one picked in the form and not yet saved.
  const savedProviderLabel = serverConfig
    ? (providers.find((p) => p.slug === serverConfig.provider)?.label ??
      serverConfig.provider)
    : "";
  // The server's message for a removal refused because this page was out of
  // date. The confirmation it came from is closed, so the page says it. It
  // lasts until the next removal is opened (openRemoveConfirmation resets it).
  const staleRefusal =
    remove.error instanceof EmailConfigStaleError ? remove.error.message : null;

  return (
    <div className="space-y-6">
      {/* The site's own provider was removed, and the organisation default has
          not reached it yet. Shown only while the site is inheriting it. */}
      {removal !== null && removalNeedsSync(removal) && inherited ? (
        <div
          role="status"
          aria-label="Email provider removed"
          className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle p-3 text-sm text-warning-subtle-fg"
        >
          <div className="flex min-w-0 items-start gap-2.5">
            <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
            <p>{removal.detail}</p>
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={syncAfterRemoval}
            disabled={sync.isPending}
          >
            <RefreshCw aria-hidden="true" className="mr-1.5 size-4" />
            {sync.isPending ? "Syncing…" : "Sync to site"}
          </Button>
        </div>
      ) : null}

      {/* A removal was refused because this page was out of date. The page has
          been read again and the confirmation closed; this says why. */}
      {staleRefusal !== null ? (
        <div
          role="alert"
          className="flex items-start gap-2.5 rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle p-3 text-sm text-warning-subtle-fg"
        >
          <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          <p>{staleRefusal}</p>
        </div>
      ) : null}

      {/* First-run hint: no per-site row and no org default yet. */}
      {!serverConfig ? (
        <div className="rounded-[var(--radius)] border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-4 py-3 text-sm text-[var(--color-muted-foreground)]">
          Email is not configured for this site yet. Choose a provider below and
          save to start routing this site&rsquo;s mail through it.
        </div>
      ) : null}

      {/* This site has no config of its own; everything below is the org's. */}
      {inherited ? (
        <div className="rounded-[var(--radius)] border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-4 py-3 text-sm text-[var(--color-muted-foreground)]">
          These are your organisation&rsquo;s email settings. This site has none
          of its own, and it sends using the organisation credential. Saving
          gives the site its own copy; it keeps using the organisation
          credential only while these settings still match the
          organisation&rsquo;s.
        </div>
      ) : null}

      {/* Provider selection */}
      <Card>
        <CardHeader>
          <CardTitle>Provider</CardTitle>
          <CardDescription>
            Choose which email service sends mail for this site.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <ProviderPicker
            value={selectedProvider}
            providers={providers}
            onChange={handleProviderChange}
            disabled={save.isPending}
          />
          {currentProviderSpec?.docs_url ? (
            <a
              href={currentProviderSpec.docs_url}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1 text-sm text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            >
              {currentProviderSpec.label} docs
              <ExternalLink aria-hidden="true" className="size-3.5" />
            </a>
          ) : null}
          {/* Only a provider the site owns can be removed; an inherited one
              is the organisation's. */}
          {ownsProvider ? (
            <div className="flex flex-col gap-3 border-t border-[var(--color-border)] pt-4 sm:flex-row sm:items-center sm:justify-between">
              <p className="text-xs text-[var(--color-muted-foreground)]">
                Stop using {savedProviderLabel} for this site. It then uses your
                organisation&rsquo;s default email settings if there are any,
                otherwise WordPress&rsquo;s own mailer.
              </p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="shrink-0 text-[var(--color-destructive)] hover:text-[var(--color-destructive)]"
                onClick={() => openRemoveConfirmation(savedProviderLabel)}
                disabled={save.isPending}
              >
                <Trash2 aria-hidden="true" className="mr-1.5 size-4" />
                Remove provider
              </Button>
            </div>
          ) : null}
        </CardContent>
      </Card>

      {/* Dynamic provider-specific fields */}
      {currentProviderSpec && currentProviderSpec.fields.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>{currentProviderSpec.label} settings</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            {currentProviderSpec.fields.map((field) => (
              <DynamicField
                key={field.key}
                field={field}
                config={providerConfig}
                secretValue={secretValue}
                secretSet={serverConfig?.secret_set ?? false}
                inherited={inherited}
                endpointDiverged={endpointDiverged}
                replacingSecret={replacingSecret}
                onConfigChange={handleConfigChange}
                onSecretChange={setSecretValue}
                onStartReplaceSecret={() => setReplacingSecret(true)}
                disabled={save.isPending}
              />
            ))}
          </CardContent>
        </Card>
      ) : null}

      {/* Webhook config (API providers only) */}
      {serverConfig ? (
        <EmailWebhookConfigCard siteId={siteId} config={serverConfig} />
      ) : null}

      {/* Sender identity */}
      <Card>
        <CardHeader>
          <CardTitle>Sender identity</CardTitle>
          <CardDescription>
            The From address and name used for outgoing mail.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <FromAddressField
            value={fromAddress}
            onChange={setFromAddress}
            disabled={save.isPending}
          />
          <FromNameField
            value={fromName}
            onChange={setFromName}
            disabled={save.isPending}
          />
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Force From address</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Override any plugin-supplied From address with the one above.
              </span>
            </div>
            <Switch
              checked={forceFromEmail}
              onCheckedChange={setForceFromEmail}
              disabled={save.isPending}
              aria-label="Force From address"
            />
          </div>
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Force From name</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Override any plugin-supplied From name.
              </span>
            </div>
            <Switch
              checked={forceFromName}
              onCheckedChange={setForceFromName}
              disabled={save.isPending}
              aria-label="Force From name"
            />
          </div>
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Set Return-Path</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Set the Return-Path / Bounce-To header to the From address.
              </span>
            </div>
            <Switch
              checked={returnPath}
              onCheckedChange={setReturnPath}
              disabled={save.isPending}
              aria-label="Set Return-Path header"
            />
          </div>
        </CardContent>
      </Card>

      {/* Email log settings */}
      <Card>
        <CardHeader>
          <CardTitle>Email log</CardTitle>
          <CardDescription>
            Control whether sent emails are recorded in the log.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <span className="text-sm font-medium">Log outgoing emails</span>
              <span className="text-xs text-[var(--color-muted-foreground)]">
                Record each send attempt in the email log.
              </span>
            </div>
            <Switch
              checked={logEmails}
              onCheckedChange={setLogEmails}
              disabled={save.isPending}
              aria-label="Log outgoing emails"
            />
          </div>
          {logEmails ? (
            <>
              <div className="flex items-center justify-between gap-3">
                <div className="flex flex-col gap-0.5">
                  <span className="text-sm font-medium">Store email body</span>
                  <span className="text-xs text-[var(--color-muted-foreground)]">
                    Persist the full message body (off by default for privacy).
                  </span>
                </div>
                <Switch
                  checked={storeBody}
                  onCheckedChange={setStoreBody}
                  disabled={save.isPending}
                  aria-label="Store email body"
                />
              </div>
              <RetentionDaysField
                value={retentionDays}
                onChange={setRetentionDays}
                disabled={save.isPending}
              />
            </>
          ) : null}
        </CardContent>
      </Card>

      {/* Save */}
      <div className="flex items-center justify-end gap-3">
        {save.isError ? (
          <p
            role="alert"
            className="text-sm text-[var(--color-destructive)]"
          >
            {save.error.message}
          </p>
        ) : null}
        <Button
          type="button"
          variant="outline"
          onClick={() => sync.mutate()}
          disabled={!serverConfig || sync.isPending}
        >
          <RefreshCw aria-hidden="true" className="mr-1.5 size-4" />
          {sync.isPending ? "Syncing…" : "Sync to site"}
        </Button>
        <Button
          type="button"
          onClick={() => save.mutate(buildPayload())}
          disabled={save.isPending || !selectedProvider}
        >
          {save.isPending ? "Saving…" : "Save email settings"}
        </Button>
      </div>

      {/* Removing a provider deletes what is saved for this site, so the
          operator types the provider's name first. */}
      <DestructiveConfirm
        open={removeDialog.open}
        onClose={() => setRemoveDialog((prev) => ({ ...prev, open: false }))}
        onConfirm={confirmRemoval}
        title={`Remove ${removeDialog.label} from this site`}
        resourceName={removeDialog.label}
        confirmLabel="Remove provider"
        cancelLabel="Keep provider"
        isPending={remove.isPending}
        pendingLabel="Removing…"
        errorMessage={
          remove.isError && staleRefusal === null ? remove.error.message : null
        }
        consequencesBody={
          <div className="space-y-2">
            <p>
              This site stops sending mail through {removeDialog.label}. It then
              uses your organisation&rsquo;s default email settings if there are
              any, otherwise WordPress&rsquo;s own mailer.
            </p>
            <p>
              What is saved for this site is deleted: the credential, sender
              details, log settings, connections and webhook address. The
              site&rsquo;s email log is kept.
            </p>
          </div>
        }
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Small sub-components for fields that exist outside the dynamic catalog
// ---------------------------------------------------------------------------

function FromAddressField({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>From address</Label>
      <Input
        id={id}
        type="email"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="no-reply@example.com"
        disabled={disabled}
      />
    </div>
  );
}

function FromNameField({
  value,
  onChange,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>From name</Label>
      <Input
        id={id}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="My Site"
        disabled={disabled}
      />
    </div>
  );
}

function RetentionDaysField({
  value,
  onChange,
  disabled,
}: {
  value: number;
  onChange: (v: number) => void;
  disabled?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>Log retention (days)</Label>
      <Input
        id={id}
        type="number"
        min={1}
        max={365}
        value={String(value)}
        onChange={(e) => onChange(Number(e.target.value))}
        disabled={disabled}
      />
      <p className="text-xs text-[var(--color-muted-foreground)]">
        Log entries older than this are pruned automatically (1 to 365 days).
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Org config panel (same shape, org-wide defaults)
// ---------------------------------------------------------------------------

export interface EmailOrgConfigPanelProps {
  /** Used as the owner label ("Org default") */
  label?: string;
}

export function EmailOrgConfigPanel({
  label = "Org default",
}: EmailOrgConfigPanelProps) {
  const configQuery = useOrgEmailConfig();
  const providersQuery = useProviders();

  if (configQuery.isPending || providersQuery.isPending) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-9 w-full" />
      </div>
    );
  }

  if (configQuery.isError || providersQuery.isError) {
    return (
      <PageError
        what="Could not load org email configuration."
        why={
          (configQuery.error ?? providersQuery.error)?.message ?? "Unknown error"
        }
        onRetry={() => {
          void configQuery.refetch();
          void providersQuery.refetch();
        }}
      />
    );
  }

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-muted)] px-4 py-3">
      <p className="text-sm font-medium text-[var(--color-foreground)]">
        {label}
      </p>
      <p className="mt-0.5 text-xs text-[var(--color-muted-foreground)]">
        Provider:{" "}
        <span className="font-medium">
          {providersQuery.data?.providers.find(
            (p) => p.slug === configQuery.data?.provider,
          )?.label ?? configQuery.data?.provider ?? "Not set"}
        </span>
        {" · "}
        From:{" "}
        <span className="font-medium">
          {configQuery.data?.from_address ?? "Not set"}
        </span>
      </p>
    </div>
  );
}

