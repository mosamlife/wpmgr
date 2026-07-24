import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronDown, LogIn, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu";
import { TooltipProvider, Tooltip } from "@/components/ui/tooltip";
import { useMe } from "@/features/auth/use-auth";
import {
  autoLoginErrorMessage,
  canAutoLogin,
  useAutoLogin,
  type AutoLoginInput,
} from "@/features/sites/use-autologin";
import { UserPickerModal } from "@/features/sites/user-picker-modal";
import { toast } from "@/components/toast";
import { cn } from "@/lib/utils";

// Phase 5.5 — One-click login control.
//
// Primary action ("Log in to site") POSTs autologin with empty body: the
// agent picks the first administrator (or the site's configured default, GH
// #286) and lands on /wp-admin/. The dropdown exposes deep links to common
// admin screens and the freeform user picker.
//
// Visibility is gated on `canAutoLogin(me)` (admin+); the backend re-checks
// the role on every call so this is defense-in-depth, not the security
// boundary.
//
// Verb-first labels throughout (DESIGN.md "verb-first actions"). No path
// strings shown raw in UI; the `redirect_to` values stay in the onSelect
// handlers only.

export interface AutoLoginButtonProps {
  siteId: string;
  /** Used to label the user-picker modal heading. */
  siteName: string;
  /** Compact variant for table rows. */
  size?: "sm" | "default";
  /** Optional extra class on the primary button (for layout). */
  className?: string;
  /**
   * The site's configured default login user (GH #286
   * `SiteAutologinPolicy.default_wp_user_login`), when the caller already
   * holds it (via `useAutologinPolicy`). Three states:
   *   - absent (`undefined`) or `null`: unknown here, so this renders
   *     exactly as before, with no tooltip, no dropdown header row, and no
   *     extra fetch. This is the default for list surfaces (sites-list
   *     rows, the bulk "Open in wp-admin" drawer, the uptime incident
   *     dialog): mint time injection on the control plane means they get
   *     the right username with zero wiring here.
   *   - `""`: a policy exists but no default is configured, so the agent
   *     falls back to the first administrator.
   *   - non-empty string: the configured default WordPress username.
   */
  defaultLoginUser?: string | null;
  /**
   * Persists a new default login user for the site (fired from the "Make
   * this the default for this site" checkbox in the user picker). Only
   * passed by callers that already hold the policy; without it, the picker
   * hides the checkbox entirely.
   */
  onSaveDefaultUser?: (username: string) => void;
}

export function AutoLoginButton({
  siteId,
  siteName,
  size = "default",
  className,
  defaultLoginUser,
  onSaveDefaultUser,
}: AutoLoginButtonProps) {
  const { data: me } = useMe();
  const mutation = useAutoLogin();
  const [pickerOpen, setPickerOpen] = useState(false);

  const openTab = useCallback((url: string) => {
    // Cross-origin: must use noopener,noreferrer so the WP site cannot grab a
    // handle back to the dashboard window.
    window.open(url, "_blank", "noopener,noreferrer");
  }, []);

  // runRef holds the latest runAutoLogin so the toast "Try again" action can
  // call it without the callback depending on itself (breaks the rules-of-hooks
  // immutability requirement when the function is listed in its own dep array).
  const runRef = useRef((_input: Omit<AutoLoginInput, "siteId">) => {});

  const runAutoLogin = useCallback(
    (input: Omit<AutoLoginInput, "siteId">) => {
      // Transient progress notice — operators expect a beat of "we heard you"
      // before the new tab pops. info() carries no action: it's purely a
      // status read.
      toast.info("Opening site");
      mutation.mutate(
        { siteId, ...input },
        {
          onSuccess: (data) => {
            openTab(data.redirect_url);
          },
          onError: (err) => {
            // The mutation only ever throws AutoLoginError (or, defensively,
            // a network-shape one — also AutoLoginError). Verb action retries
            // with the same input so the operator can recover in one click.
            toast.error(autoLoginErrorMessage(err), {
              action: {
                label: "Try again",
                onClick: () => runRef.current(input),
              },
            });
          },
        },
      );
    },
    [mutation, openTab, siteId],
  );

  // Keep the ref in sync so any already-rendered toast "Try again" button
  // always calls the latest version of the callback.
  useEffect(() => {
    runRef.current = runAutoLogin;
  }, [runAutoLogin]);

  // Gate visibility on role. Render nothing if the user cannot autologin —
  // the action is invisible (not just disabled) for clarity.
  if (!canAutoLogin(me)) return null;

  const pending = mutation.isPending;

  // GH #286: the default user is "known" only when the caller explicitly
  // resolved the policy and handed us a string (possibly empty). `null`
  // covers both "prop not passed" and "still loading", and both render
  // exactly like today, with no tooltip or dropdown header row.
  const knownDefaultUser =
    typeof defaultLoginUser === "string" ? defaultLoginUser : null;
  const defaultUserLabel =
    knownDefaultUser === "" ? (
      "the first administrator"
    ) : (
      <span className="font-mono">{knownDefaultUser}</span>
    );

  const primaryButton = (
    <Button
      type="button"
      size={size}
      onClick={() => runAutoLogin({})}
      disabled={pending}
      className="rounded-r-none"
      aria-label="Log in to site"
    >
      {pending ? (
        <Loader2 aria-hidden="true" className="animate-spin" />
      ) : (
        <LogIn aria-hidden="true" />
      )}
      Log in to site
    </Button>
  );

  return (
    <>
      <div className={cn("inline-flex items-stretch", className)}>
        {knownDefaultUser !== null ? (
          <TooltipProvider>
            <Tooltip content={<>Logs in as {defaultUserLabel}</>}>
              {primaryButton}
            </Tooltip>
          </TooltipProvider>
        ) : (
          primaryButton
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              type="button"
              size={size}
              disabled={pending}
              className="rounded-l-none border-l border-primary-foreground/20 px-2"
              aria-label="More log-in options"
            >
              <ChevronDown aria-hidden="true" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {knownDefaultUser !== null ? (
              <>
                <DropdownMenuLabel>
                  Logs in as {defaultUserLabel}
                </DropdownMenuLabel>
                <DropdownMenuSeparator />
              </>
            ) : null}
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                runAutoLogin({ redirect_to: "/wp-admin/" });
              }}
            >
              Open Dashboard
            </DropdownMenuItem>
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                runAutoLogin({ redirect_to: "/wp-admin/plugins.php" });
              }}
            >
              Open Plugins
            </DropdownMenuItem>
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                runAutoLogin({ redirect_to: "/wp-admin/themes.php" });
              }}
            >
              Open Themes
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                setPickerOpen(true);
              }}
            >
              Log in as different user
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <UserPickerModal
        open={pickerOpen}
        siteName={siteName}
        pending={pending}
        defaultLoginUser={defaultLoginUser}
        onSaveDefault={onSaveDefaultUser}
        onClose={() => setPickerOpen(false)}
        onSubmit={(target_wp_user_login) => {
          setPickerOpen(false);
          runAutoLogin({ target_wp_user_login });
        }}
      />
    </>
  );
}
