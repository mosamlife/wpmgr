/** The subset of `Site` the audit page needs to resolve a target_id to a name. */
export interface SiteMin {
  id: string;
  name?: string | null;
  url: string;
}
