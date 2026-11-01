import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteDiagnostics,
  refreshSiteDiagnostics,
  type SiteDiagnosticsCard,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// ADR-037 Sprint 2 — TanStack Query hooks for the site Health tab.
//
// Backed by the generated @wpmgr/api SDK. The diagnostics endpoint returns 14
// cards every call (one per agent category); the UI's HealthTab renders 9 of
// them (PHP / MySQL / Filesystem / HTTP / Cron / Themes / Plugins / Users /
// Security per the headline spec) and groups the rest into a single Hosting
// ribbon up top.

export const diagnosticsKeys = {
  all: ["diagnostics"] as const,
  forSite: (siteId: string) => [...diagnosticsKeys.all, "site", siteId] as const,
};

/** Latest diagnostics by category for a site. */
export function useDiagnostics(
  siteId: string,
): UseQueryResult<SiteDiagnosticsCard[], Error> {
  return useQuery({
    queryKey: diagnosticsKeys.forSite(siteId),
    queryFn: async () => {
      const { data, error } = await getSiteDiagnostics({ path: { siteId } });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });
}

// The code this route answers with when the control plane has no signed-command
// client (HTTP 501). Unique to this route, so it is safe to key on by itself.
// Source: apps/api/internal/diagnostics/handler.go (refresh) and
// packages/openapi/openapi.yaml, operation refreshSiteDiagnostics.
const REFRESH_UNWIRED_CODE = "diagnostics_refresh_unwired";

/**
 * The control plane cannot send commands to sites, so it cannot run a check on
 * demand. The copy is fixed here because the server's own wording is written
 * for whoever deploys it, not for the person who clicked the button.
 */
export class DiagnosticsRefreshUnwiredError extends Error {
  constructor() {
    super(
      "This control plane is not set up to send commands to sites, so checks cannot be run on demand. " +
        "The diagnostics on this page still update when the site's agent sends its daily report.",
    );
    this.name = "DiagnosticsRefreshUnwiredError";
  }
}

/**
 * The site refused the check or could not be reached (HTTP 502). `message` is
 * the control plane's own wording for the operator, kept exactly as sent: it is
 * often two sentences and ends with what to do. `code` is one of site_refused,
 * site_timeout, site_unreachable, site_url_redirects, agent_command_failed,
 * site_error or site_bad_reply. Callers show the message and never branch on
 * the code, because site_unreachable also means something else (a 409) on other
 * routes.
 */
export class DiagnosticsSiteFailureError extends Error {
  readonly code: string;

  constructor(code: string, message: string) {
    super(message);
    this.name = "DiagnosticsSiteFailureError";
    this.code = code;
  }
}

/**
 * The `code` and `message` of an error body, when it has them. The generated
 * client hands back the parsed JSON body, or the raw text when the body is not
 * JSON (a proxy's HTML page), so nothing here can be assumed.
 */
function errorBody(error: unknown): { code?: string; message?: string } {
  if (typeof error !== "object" || error === null) return {};
  const { code, message } = error as { code?: unknown; message?: unknown };
  return {
    code: typeof code === "string" ? code : undefined,
    message:
      typeof message === "string" && message.trim() !== "" ? message : undefined,
  };
}

/**
 * Run the diagnostics checks on the site now, through its agent.
 *
 * The call is synchronous: the control plane waits for the site, stores the new
 * report and only then answers 202, so the report is readable as soon as this
 * resolves. A site that refuses or cannot be reached answers 502, which throws
 * DiagnosticsSiteFailureError carrying the control plane's message. An install
 * without the command client answers 501 diagnostics_refresh_unwired, which
 * throws DiagnosticsRefreshUnwiredError. Any other failure keeps the generic
 * handling.
 */
export function useRefreshDiagnostics(
  siteId: string,
): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await refreshSiteDiagnostics({
        path: { siteId },
      });
      const body = errorBody(error);
      if (body.code === REFRESH_UNWIRED_CODE) {
        throw new DiagnosticsRefreshUnwiredError();
      }
      // Keyed on this call's own status, not on the code: any 502 here is the
      // site's doing and its message is the answer.
      if (response?.status === 502 && body.message) {
        throw new DiagnosticsSiteFailureError(body.code ?? "", body.message);
      }
      if (error) throw toError(error);
    },
    // Returned, so the mutation settles only after the new report is in the
    // cache. The success message then never runs ahead of the data it names.
    // A failed refetch does not reject here, so it cannot turn a stored report
    // into a failed re-run.
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: diagnosticsKeys.forSite(siteId),
      }),
  });
}

/** Return the card for the given category, or `undefined`. */
export function cardFor(
  items: SiteDiagnosticsCard[] | undefined,
  category: string,
): SiteDiagnosticsCard | undefined {
  return items?.find((c) => c.category === category);
}
