import { useMutation, useQueryClient, type UseMutationResult } from "@tanstack/react-query";
import { rotateRumBeaconKey, type RumBeaconRotateResult } from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";

import { perfKeys } from "../perf-keys";

// Mutation for POST /api/v1/sites/{siteId}/perf/rum/rotate-key (GH #174).
//
// This is the operator-triggered recovery path for the RUM beacon-key stuck
// state: the CP mints a fresh key, rotates the previous hash into a
// grace-window column, and pushes the new plaintext key to the agent in this
// one request only -- it is NEVER returned in the response (the response is
// the pinned {ok, beacon_key_set} confirmation shape only).
//
// Pattern mirrors useClearRucss (useCacheStats.ts): the hook owns the error
// toast + cache invalidation; the call site owns the success toast (its copy
// needs no dynamic response data here, but keeping the split consistent with
// the rest of this feature avoids two different conventions in one file).
export function useRotateBeaconKey(
  siteId: string,
): UseMutationResult<RumBeaconRotateResult, Error, void> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error } = await rotateRumBeaconKey({ path: { siteId } });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      // Refresh perf/config so beacon_key_set / beacon_key_acked_present
      // reflect the rotation (beacon_key_acked_present goes false again until
      // the agent's next config-ack confirms the new key).
      void qc.invalidateQueries({ queryKey: perfKeys.config(siteId) });
    },
    onError: (err) => {
      toast.error("Could not rotate the beacon key.", {
        description: err.message,
      });
    },
  });
}
