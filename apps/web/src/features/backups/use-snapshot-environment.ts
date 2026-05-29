import { useQuery, type UseQueryResult } from "@tanstack/react-query";

// ADR-037 Sprint 1, 1D — environment fingerprint hook.
//
// The endpoint is the agent-shipped synthetic environment.json manifest entry
// served as a raw JSON pass-through. It's NOT in the OpenAPI spec yet
// (additive feature on a sprint cycle that doesn't regen the strict ogen
// types), so we fetch via plain fetch instead of the generated SDK.
//
// Shape (from class-encrypt-and-upload.php#collectEnvironmentFingerprint):
//   {
//     "schema_version": 1,
//     "php_version": "8.1.27",
//     "mysql_version": "8.0.36",
//     "wp_version": "6.4.3",
//     "site_url": "https://example.com",
//     "home_url": "https://example.com",
//     "is_multisite": false,
//     "file_count": 12847,
//     "db_table_count": 24,
//     "plugin_slugs": ["akismet", ...],
//     "theme_slugs": ["twentytwentyfour", ...],
//     "table_names": ["wp_posts", ...],
//     "total_size_bytes": 2147483648,
//     "captured_at": "2026-05-29T10:00:00Z"
//   }
//
// Terminal shapes:
//   200 — payload ready: phase="ready", env=<EnvFingerprint>
//   404 — snapshot pre-dates the feature: phase="not_recorded"
//   503 — CP wiring missing: phase="unwired"
//   any other → phase="error"

export type EnvFingerprint = {
  schema_version: number;
  php_version: string;
  mysql_version: string;
  wp_version: string;
  site_url: string;
  home_url: string;
  is_multisite: boolean;
  file_count: number;
  db_table_count: number;
  plugin_slugs: string[];
  theme_slugs: string[];
  table_names: string[];
  total_size_bytes: number;
  captured_at: string;
};

export type EnvFingerprintState =
  | { phase: "loading" }
  | { phase: "ready"; env: EnvFingerprint }
  | { phase: "not_recorded" }
  | { phase: "unwired" }
  | { phase: "error"; message: string };

export const snapshotEnvironmentKey = (snapshotId: string) =>
  ["backups", "environment", snapshotId] as const;

export function useSnapshotEnvironment(
  snapshotId: string,
): UseQueryResult<EnvFingerprintState, Error> {
  return useQuery({
    queryKey: snapshotEnvironmentKey(snapshotId),
    queryFn: async (): Promise<EnvFingerprintState> => {
      const res = await fetch(`/api/v1/backups/${snapshotId}/environment`, {
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (res.status === 404) {
        return { phase: "not_recorded" };
      }
      if (res.status === 503) {
        return { phase: "unwired" };
      }
      if (!res.ok) {
        return {
          phase: "error",
          message: `HTTP ${res.status} from environment endpoint`,
        };
      }
      try {
        const env = (await res.json()) as EnvFingerprint;
        return { phase: "ready", env };
      } catch (e) {
        return {
          phase: "error",
          message:
            e instanceof Error ? e.message : "Could not parse environment JSON",
        };
      }
    },
    // No retry on terminal codes; the hook surfaces them as phases.
    retry: false,
    // Env fingerprint is a snapshot of the past — never changes once recorded.
    staleTime: Infinity,
  });
}
