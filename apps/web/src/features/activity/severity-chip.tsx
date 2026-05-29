// SeverityChip moved to /components/shared so Errors, Health, and Backup can
// import it without depending on the activity feature (ADR-037 Batch 0). This
// re-export keeps the activity feature's existing imports working.
export { SeverityChip } from "@/components/shared/severity-chip";
