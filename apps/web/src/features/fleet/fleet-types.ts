// Fleet-wide types shared across the three fleet dashboards (Uptime, Backups,
// Performance). These are hand-rolled to match the pinned API contract; the
// endpoints are NOT in the generated @wpmgr/api SDK yet.

// ---------------------------------------------------------------------------
// Uptime
// ---------------------------------------------------------------------------

export type UptimeStatusKind = "up" | "degraded" | "down" | "unknown";

export interface FleetStatusItem {
  site_id: string;
  name: string;
  url: string;
  connection_state: string;
  health_status: string;
  status: UptimeStatusKind;
  up: boolean | null;
  last_probe_at: string | null;
  uptime_pct_7d: number | null;
  avg_latency_ms: number | null;
  tls_expiry: string | null;
  latency_sparkline: number[];
}

export interface FleetStatusSummary {
  up: number;
  degraded: number;
  down: number;
  unknown: number;
}

export interface FleetStatusResponse {
  summary: FleetStatusSummary;
  items: FleetStatusItem[];
}

export interface FleetIncident {
  id: string;
  site_id: string;
  name: string;
  url: string;
  kind: "down" | "degraded";
  started_at: string;
  ended_at: string | null;
  duration_seconds: number | null;
  ongoing: boolean;
}

export interface FleetIncidentsResponse {
  items: FleetIncident[];
}

// ---------------------------------------------------------------------------
// Incident detail (GH #148) — GET /api/v1/fleet/incidents/:incidentId
// ---------------------------------------------------------------------------

/**
 * One probe result in an incident's probe window (`IncidentDetail.probes`).
 * Named to match the pinned contract; despite the name this is a probe
 * RECORD (timestamp + outcome), not an enum of status codes.
 */
export interface ProbeCode {
  probed_at: string;
  up: boolean;
  http_status: number;
  total_ms: number;
  error?: string;
}

export interface IncidentDetail {
  id: string;
  site_id: string;
  name: string;
  url: string;
  started_at: string;
  ended_at: string | null;
  duration_seconds: number | null;
  ongoing: boolean;
  peak_status: string;
  last_http_status: number;
  reason: string;
  incident_count_30d: number;
  /** Newest-first. May be `[]` when the incident is older than probe retention. */
  probes: ProbeCode[];
  probes_truncated: boolean;
}

// ---------------------------------------------------------------------------
// Backup health
// ---------------------------------------------------------------------------

export type BackupHealthStatus =
  | "protected"
  | "stale"
  | "failed"
  | "unprotected"
  | "in_flight";

export interface BackupHealthItem {
  site_id: string;
  site_name: string;
  site_url: string;
  last_completed_at: string | null;
  last_failed_at: string | null;
  latest_size_bytes: number | null;
  in_flight_count: number;
  schedule_cadence: "daily" | "weekly" | "monthly" | null;
  next_run_at: string | null;
  status: BackupHealthStatus;
}

export interface BackupHealthResponse {
  items: BackupHealthItem[];
}

export interface FleetBackupItem {
  // Reuses BackupSnapshot shape from the generated SDK but accessed via raw client.
  id: string;
  site_id: string;
  site_name?: string;
  status: string;
  created_at: string;
  completed_at?: string | null;
  size_bytes?: number | null;
}

export interface FleetBackupsResponse {
  items: FleetBackupItem[];
  next_offset: number | null;
}

// ---------------------------------------------------------------------------
// Performance — fleet RUM aggregate
// ---------------------------------------------------------------------------

export interface FleetRumMetric {
  p75: number | null;
  good_pct: number | null;
  ni_pct: number | null;
  poor_pct: number | null;
  sample_count: number;
}

export interface FleetRumOffender {
  site_id: string;
  name: string;
  url: string;
  lcp_p75: number | null;
  inp_p75: number | null;
  cls_p75: number | null;
  overall_rating: "good" | "needs-improvement" | "poor";
  sample_count: number;
}

export interface FleetRumTrendPoint {
  date: string;
  lcp_p75: number | null;
  inp_p75: number | null;
  cls_p75: number | null;
}

export interface FleetRumResponse {
  sites_reporting: number;
  sites_total: number;
  fleet_pass_pct: number | null;
  per_metric: {
    lcp: FleetRumMetric;
    inp: FleetRumMetric;
    cls: FleetRumMetric;
    fcp: FleetRumMetric;
    ttfb: FleetRumMetric;
  };
  worst_offenders: FleetRumOffender[];
  trend: FleetRumTrendPoint[];
}
