package model

import (
	"time"

	"github.com/google/uuid"
)

// VariantState is the per-variant encode outcome.
type VariantState string

const (
	// VariantSucceeded — the variant was encoded and the output PUT to storage.
	VariantSucceeded VariantState = "succeeded"
	// VariantFailed — the encode failed after retries (carries a human reason).
	VariantFailed VariantState = "failed"
	// VariantSkipped — the variant was not encoded (e.g. source too small).
	VariantSkipped VariantState = "skipped"
)

// VariantResult is one row of media_variant_results: the per-variant
// (full/thumbnail/medium/…) encode result for a job. The CP holds the result
// metadata only; the encoded bytes live in temp object storage until the agent
// applies them, then the temp objects are deleted (ADR-043 §2).
type VariantResult struct {
	ID                 uuid.UUID
	JobID              string
	TenantID           uuid.UUID
	VariantName        string
	SourceSizeBytes    int64
	OptimizedSizeBytes *int64
	SourceMime         string
	OptimizedMime      string
	EncodeMS           *int
	State              VariantState
	Reason             string
	CreatedAt          time.Time
}
