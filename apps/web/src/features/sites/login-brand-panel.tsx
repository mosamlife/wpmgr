import { useState } from "react";
import { ImageIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { toast } from "@/components/toast/use-toast-helpers";
import type { SiteLoginBrand, SiteLoginBrandUpdate } from "@wpmgr/api";

import { useLoginBrand, useUpdateLoginBrand } from "./use-login-brand";

// M14 — Login page branding panel.
//
// Rendered inline on the settings tab (no dialog — same pattern as
// login-protection-panel.tsx). Follows the "key on loaded config" pattern
// from error-config-panel to keep form state initialised from server data
// without setState-in-render or setState-in-effect.
//
// Two layers:
//   LoginBrandShell  — fetches; renders loading / error / loaded
//   LoginBrandLoaded — keyed on config values; owns all local state
//
// Design rules (per spec):
//   - Logo URL: font-mono input, inline <img> preview for valid http(s) URLs.
//   - Logo link: font-mono input, help text "defaults to the site home".
//   - Message: textarea, char counter toward 2000 cap, tabular-nums.
//   - Verb-first Save button; pending state; inline field validation; PageError.
//   - A muted note about what this does and does not do.

const MSG_MAX = 2000;

/** Returns true if the value is empty or a valid http/https URL. */
function isValidUrlOrEmpty(value: string): boolean {
  if (value === "") return true;
  try {
    const u = new URL(value);
    return u.protocol === "http:" || u.protocol === "https:";
  } catch {
    return false;
  }
}

// ---------------------------------------------------------------------------
// Shell — fetches config; delegates to loaded or renders loading/error
// ---------------------------------------------------------------------------

export function LoginBrandPanel({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } = useLoginBrand(siteId);

  if (isPending) {
    return <LoginBrandSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load login brand config."
        why={error instanceof Error ? error.message : "Unknown error"}
        onRetry={() => void refetch()}
        retryLabel="Reload config"
      />
    );
  }

  if (!data) return null;

  // Key on all three fields so the form resets automatically if a background
  // refetch returns a different value (e.g. another operator saved changes).
  const configKey = `${data.logo_url}|${data.logo_link}|${data.message}`;

  return (
    <LoginBrandLoaded key={configKey} siteId={siteId} initialConfig={data} />
  );
}

// ---------------------------------------------------------------------------
// Loaded form
// ---------------------------------------------------------------------------

interface LoadedProps {
  siteId: string;
  initialConfig: SiteLoginBrand;
}

function LoginBrandLoaded({ siteId, initialConfig }: LoadedProps) {
  const update = useUpdateLoginBrand(siteId);

  const [logoUrl, setLogoUrl] = useState(initialConfig.logo_url);
  const [logoLink, setLogoLink] = useState(initialConfig.logo_link);
  const [message, setMessage] = useState(initialConfig.message);
  const [saveError, setSaveError] = useState<string | null>(null);

  // Inline field-level validation errors (shown on save attempt).
  const [logoUrlError, setLogoUrlError] = useState<string | null>(null);
  const [logoLinkError, setLogoLinkError] = useState<string | null>(null);
  const [messageError, setMessageError] = useState<string | null>(null);

  // Preview: show the image element only when the logo URL is a valid http(s) URL.
  const showPreview = logoUrl !== "" && isValidUrlOrEmpty(logoUrl);

  function validate(): boolean {
    let valid = true;

    if (!isValidUrlOrEmpty(logoUrl)) {
      setLogoUrlError("Must be empty or an http/https URL.");
      valid = false;
    } else {
      setLogoUrlError(null);
    }

    if (!isValidUrlOrEmpty(logoLink)) {
      setLogoLinkError("Must be empty or an http/https URL.");
      valid = false;
    } else {
      setLogoLinkError(null);
    }

    if (message.length > MSG_MAX) {
      setMessageError(
        `Message must be at most ${MSG_MAX.toLocaleString()} characters.`,
      );
      valid = false;
    } else {
      setMessageError(null);
    }

    return valid;
  }

  function handleSave() {
    if (!validate()) return;

    setSaveError(null);

    const body: SiteLoginBrandUpdate = {
      logo_url: logoUrl,
      logo_link: logoLink,
      message,
    };

    update.mutate(body, {
      onSuccess: () => {
        toast.success("Login branding saved.", {
          description: "Config pushed to the agent.",
        });
      },
      onError: (err: Error) => {
        setSaveError(err.message);
      },
    });
  }

  const charCount = message.length;
  const charNearLimit = charCount > MSG_MAX * 0.85;
  const charOverLimit = charCount > MSG_MAX;

  return (
    <div className="space-y-6">
      {/* ── Logo URL ── */}
      <div className="space-y-2">
        <label
          htmlFor="lb-logo-url"
          className="block text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Logo URL
        </label>
        <Input
          id="lb-logo-url"
          type="url"
          value={logoUrl}
          onChange={(e) => {
            setLogoUrl(e.target.value);
            setLogoUrlError(null);
          }}
          placeholder="https://example.com/logo.png"
          aria-describedby={
            logoUrlError ? "lb-logo-url-err" : undefined
          }
          aria-invalid={logoUrlError !== null}
          className="font-mono text-sm"
        />
        {logoUrlError ? (
          <p
            id="lb-logo-url-err"
            role="alert"
            className="text-sm text-[var(--color-destructive)]"
          >
            <span className="font-medium">Invalid URL</span>
            {" · "}
            <span>{logoUrlError}</span>
            {" · "}
            <span>Enter a full http/https URL or leave blank.</span>
          </p>
        ) : null}
        {/* Inline preview */}
        {showPreview ? (
          <div className="flex items-start gap-3 rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/30 p-3">
            <img
              src={logoUrl}
              alt="Login logo preview"
              className="h-12 max-w-[160px] shrink-0 rounded object-contain"
              onError={(e) => {
                e.currentTarget.style.display = "none";
              }}
            />
            <p className="text-xs text-[var(--color-muted-foreground)]">
              Logo preview. The image is loaded from the URL you entered.
            </p>
          </div>
        ) : (
          <div className="flex items-center gap-2 rounded-md border border-dashed border-[var(--color-border)] bg-[var(--color-muted)]/20 px-3 py-4 text-xs text-[var(--color-muted-foreground)]">
            <ImageIcon aria-hidden="true" className="size-4 shrink-0" />
            <span>Enter a valid https URL above to preview the logo.</span>
          </div>
        )}
      </div>

      {/* ── Logo link ── */}
      <div className="space-y-1.5">
        <label
          htmlFor="lb-logo-link"
          className="block text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Logo link
        </label>
        <Input
          id="lb-logo-link"
          type="url"
          value={logoLink}
          onChange={(e) => {
            setLogoLink(e.target.value);
            setLogoLinkError(null);
          }}
          placeholder="https://example.com"
          aria-describedby={
            logoLinkError ? "lb-logo-link-err" : "lb-logo-link-help"
          }
          aria-invalid={logoLinkError !== null}
          className="font-mono text-sm"
        />
        {logoLinkError ? (
          <p
            id="lb-logo-link-err"
            role="alert"
            className="text-sm text-[var(--color-destructive)]"
          >
            <span className="font-medium">Invalid URL</span>
            {" · "}
            <span>{logoLinkError}</span>
            {" · "}
            <span>Enter a full http/https URL or leave blank.</span>
          </p>
        ) : (
          <p
            id="lb-logo-link-help"
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            Where the logo links to on the login page. Defaults to the site home.
          </p>
        )}
      </div>

      {/* ── Message ── */}
      <div className="space-y-1.5">
        <div className="flex items-baseline justify-between gap-2">
          <label
            htmlFor="lb-message"
            className="block text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
          >
            Message
          </label>
          <span
            className={[
              "tabular-nums text-xs",
              charOverLimit
                ? "font-medium text-[var(--color-destructive)]"
                : charNearLimit
                  ? "text-amber-600 dark:text-amber-400"
                  : "text-[var(--color-muted-foreground)]",
            ].join(" ")}
            aria-live="polite"
          >
            {charCount.toLocaleString()} / {MSG_MAX.toLocaleString()}
          </span>
        </div>
        <textarea
          id="lb-message"
          value={message}
          onChange={(e) => {
            setMessage(e.target.value);
            setMessageError(null);
          }}
          rows={4}
          placeholder="Welcome! Please sign in to continue."
          aria-describedby={
            messageError ? "lb-message-err" : "lb-message-help"
          }
          aria-invalid={messageError !== null}
          className="flex min-h-[96px] w-full resize-y rounded-md border border-[var(--color-input)] bg-transparent px-3 py-2 text-sm shadow-sm transition-colors placeholder:text-[var(--color-muted-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50"
        />
        {messageError ? (
          <p
            id="lb-message-err"
            role="alert"
            className="text-sm text-[var(--color-destructive)]"
          >
            <span className="font-medium">Message too long</span>
            {" · "}
            <span>{messageError}</span>
          </p>
        ) : (
          <p
            id="lb-message-help"
            className="text-xs text-[var(--color-muted-foreground)]"
          >
            Shown above the login form. Basic formatting only.
          </p>
        )}
      </div>

      {/* ── Muted caveat note ── */}
      <p className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/30 px-4 py-3 text-xs text-[var(--color-muted-foreground)]">
        This changes the wp-login.php logo, link, and message. It does not hide
        the login URL.
      </p>

      {/* ── Save error ── */}
      {saveError ? (
        <PageError what="Could not save login branding." why={saveError} />
      ) : null}

      {/* ── Save footer ── */}
      <div className="flex items-center gap-3 border-t border-[var(--color-border)] pt-6">
        <Button
          type="button"
          onClick={handleSave}
          disabled={update.isPending}
          aria-busy={update.isPending}
        >
          {update.isPending ? "Saving..." : "Save branding"}
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
            Using WordPress defaults. No custom branding saved yet.
          </p>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function LoginBrandSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading login brand config"
      className="space-y-6"
    >
      <span className="sr-only">Loading login brand config</span>
      <div className="space-y-2">
        <Skeleton className="h-3 w-20" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-16 w-full rounded-md" />
      </div>
      <div className="space-y-2">
        <Skeleton className="h-3 w-24" />
        <Skeleton className="h-9 w-full" />
      </div>
      <div className="space-y-2">
        <Skeleton className="h-3 w-20" />
        <Skeleton className="h-24 w-full" />
      </div>
    </div>
  );
}
