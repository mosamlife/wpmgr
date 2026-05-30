import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** Merge conditional class names and de-dupe conflicting Tailwind utilities. */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}

/**
 * Format an ISO-8601 timestamp as a short relative string ("just now", "2m
 * ago", "3h ago", "5d ago"). Returns null for missing/invalid input so callers
 * can render their own placeholder. `now` is injectable for testing.
 *
 * For future instants (seconds < 0), returns a forward-relative label such as
 * "in 6h" or "in 3d" so schedule previews render correctly.
 */
export function relativeTime(
  iso: string | null | undefined,
  now: number = Date.now(),
): string | null {
  if (!iso) return null;
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return null;
  const seconds = Math.round((now - then) / 1000);
  if (seconds < 0) {
    // Future instant: show a forward-relative label.
    const abs = Math.abs(seconds);
    if (abs < 45) return "in a moment";
    const minutes = Math.round(abs / 60);
    if (minutes < 60) return `in ${minutes}m`;
    const hours = Math.round(minutes / 60);
    if (hours < 24) return `in ${hours}h`;
    const days = Math.round(hours / 24);
    if (days < 30) return `in ${days}d`;
    const months = Math.round(days / 30);
    if (months < 12) return `in ${months}mo`;
    return `in ${Math.round(months / 12)}y`;
  }
  if (seconds < 45) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.round(days / 30);
  if (months < 12) return `${months}mo ago`;
  return `${Math.round(months / 12)}y ago`;
}

/**
 * Format a byte count as a human-readable size ("1.2 MB", "512 KB"). Returns
 * "—" for missing/invalid input so callers can render a placeholder inline.
 */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || Number.isNaN(bytes) || bytes < 0) return "—";
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const exp = Math.min(
    Math.floor(Math.log(bytes) / Math.log(1024)),
    units.length - 1,
  );
  const value = bytes / Math.pow(1024, exp);
  return `${exp === 0 ? value : value.toFixed(1)} ${units[exp]}`;
}
