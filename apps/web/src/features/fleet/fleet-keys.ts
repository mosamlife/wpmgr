// Query-key factory for the three fleet dashboards.

export const fleetKeys = {
  all: ["fleet"] as const,
  // Uptime
  status: () => [...fleetKeys.all, "status"] as const,
  incidents: (since?: string) =>
    [...fleetKeys.all, "incidents", since ?? ""] as const,
  incidentDetail: (incidentId: string) =>
    [...fleetKeys.all, "incidentDetail", incidentId] as const,
  // Backups
  backupHealth: (sites?: string) =>
    [...fleetKeys.all, "backupHealth", sites ?? "all"] as const,
  backupBrowser: (params: Record<string, string>) =>
    [...fleetKeys.all, "backupBrowser", params] as const,
  // Performance
  rumFleet: (windowDays: number, device: string) =>
    [...fleetKeys.all, "rumFleet", windowDays, device] as const,
  // Agent release freshness (agent-releases)
  agentLatest: () => [...fleetKeys.all, "agentLatest"] as const,
  agentVersions: () => [...fleetKeys.all, "agentVersions"] as const,
} as const;
