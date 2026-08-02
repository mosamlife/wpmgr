import { useMutation, type UseMutationResult } from "@tanstack/react-query";
import { checkAgentMirrorNow, type ApiError } from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";

// GH #322: superadmin, install-level manual trigger for the upstream
// agent-release mirror.
//
// POST /api/v1/admin/agent-mirror/check is a generated SDK operation
// (checkAgentMirrorNow, from @wpmgr/api), so the URL, the 202 body shape,
// and the error codes/details below are all read from the real OpenAPI
// contract rather than a hand-rolled guess at one.
//
// Deliberately install-level, not tenant-scoped (internal/admin/agent_
// mirror.go: the mirror is ONE PER INSTALL, fetches one public GitHub
// release, and spends this install's SHARED unauthenticated GitHub request
// budget), so this hook lives here, next to the admin console page that
// renders it (routes/_authed/admin/agent-mirror.tsx), not in
// features/fleet (the tenant-scoped fleet rollup).
//
// None of the three ordinary outcomes (queued / already running / rate
// limited) are failures. In particular a 429 must NEVER read as an error:
// it is the mirror truthfully reporting that nothing was checked yet, which
// is the entire point of GH #322.

export type CheckAgentMirrorOutcome =
  | { kind: "queued"; message: string }
  | { kind: "already_running" }
  | { kind: "rate_limited"; retryAfterSeconds?: number };

function numberDetail(
  details: ApiError["details"],
  key: string,
): number | undefined {
  const raw = details?.[key];
  return typeof raw === "number" && Number.isFinite(raw)
    ? Math.max(0, Math.round(raw))
    : undefined;
}

function formatWait(seconds: number): string {
  if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"}`;
  const minutes = Math.max(1, Math.round(seconds / 60));
  return `${minutes} minute${minutes === 1 ? "" : "s"}`;
}

/**
 * POST /api/v1/admin/agent-mirror/check
 *
 * Queues one immediate mirror run instead of waiting for the next scheduled
 * tick (up to six hours away). Returns a typed outcome rather than throwing
 * for the two expected non-2xx cases (409 already running, 429 rate
 * limited) so the caller can show the honest, specific toast for each. Every
 * other case (401/403, 503 agent_mirror_disabled / agent_mirror_not_
 * configured / agent_mirror_not_wired, 5xx, network) is a genuine failure
 * and rejects.
 */
export function useCheckAgentMirrorNow(): UseMutationResult<
  CheckAgentMirrorOutcome,
  Error,
  void
> {
  return useMutation({
    mutationFn: async (): Promise<CheckAgentMirrorOutcome> => {
      const { data, error, response } = await checkAgentMirrorNow();
      const status = response?.status;

      if (status === 202) {
        return {
          kind: "queued",
          message: data?.message ?? "A mirror run has been queued.",
        };
      }
      if (status === 409 && error?.code === "agent_mirror_check_in_flight") {
        return { kind: "already_running" };
      }
      if (status === 429) {
        return {
          kind: "rate_limited",
          retryAfterSeconds: numberDetail(error?.details, "retry_after_seconds"),
        };
      }

      if (error) throw toError(error);
      throw new Error("Unexpected response from the agent mirror check.");
    },
    onSuccess: (outcome) => {
      if (outcome.kind === "queued") {
        toast.success(outcome.message);
      } else if (outcome.kind === "already_running") {
        toast.info(
          "A check is already running. Its result will appear on the fleet agent view when it finishes.",
        );
      } else {
        const seconds = outcome.retryAfterSeconds;
        toast.info(
          seconds != null
            ? `Not checked. The mirror must wait ${formatWait(seconds)} before its next upstream request. The scheduled check still runs.`
            : "Not checked. The mirror must wait before its next upstream request. The scheduled check still runs.",
        );
      }
    },
    onError: (err: Error) => {
      toast.error(
        err.message ||
          "Could not queue a check. Try again shortly, or see the control plane logs.",
      );
    },
  });
}
