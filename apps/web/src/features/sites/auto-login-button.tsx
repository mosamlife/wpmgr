import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronDown, LogIn, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu";
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
// Primary action ("Log in to site") POSTs autologin with empty body — the
// agent picks the first administrator and lands on /wp-admin/. The dropdown
// exposes deep links to common admin screens and the freeform user picker.
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
}

export function AutoLoginButton({
  siteId,
  siteName,
  size = "default",
  className,
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

  return (
    <>
      <div className={cn("inline-flex items-stretch", className)}>
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
        onClose={() => setPickerOpen(false)}
        onSubmit={(target_wp_user_login) => {
          setPickerOpen(false);
          runAutoLogin({ target_wp_user_login });
        }}
      />
    </>
  );
}
