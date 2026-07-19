// export_test.go exposes package-private functions for white-box testing from
// the vuln_test package. This file is compiled only during `go test`.
package vuln

import "encoding/json"

// IsSafeURL is isSafeURL exposed for testing (F2).
func IsSafeURL(u string) bool { return isSafeURL(u) }

// FilterReferences is filterReferences exposed for testing (F2).
func FilterReferences(raw json.RawMessage) json.RawMessage { return filterReferences(raw) }

// NormSlug returns the normalised (lower-cased) form of a software slug (F3).
// This mirrors the normalisation applied on both the ingest path
// (UpsertFeedRecord) and the lookup path (LookupSoftware).
func NormSlug(slug string) string { return normSlug(slug) }

// ParseFeedRecord exposes parseFeedRecord for parser unit tests.
func ParseFeedRecord(vulnID string, raw json.RawMessage) (FeedRecord, string, string, string, error) {
	return parseFeedRecord(vulnID, raw)
}

// ErrNoUsableSoftware exposes errNoUsableSoftware so tests can assert the skip sentinel.
var ErrNoUsableSoftware = errNoUsableSoftware

// DedupSoftwareRows exposes dedupSoftwareRows for white-box unit tests.
func DedupSoftwareRows(rows []SoftwareRow) []SoftwareRow { return dedupSoftwareRows(rows) }

// MergeAffectedVersions exposes mergeAffectedVersions for unit tests.
func MergeAffectedVersions(a, b []byte) []byte { return mergeAffectedVersions(a, b) }

// MergePatchedVersions exposes mergePatchedVersions for unit tests.
func MergePatchedVersions(a, b []byte) []byte { return mergePatchedVersions(a, b) }

// StreamProductionRecords exposes (*FeedWorker).streamProductionRecords for
// white-box unit testing the Production streaming/batching/memory-bound
// behavior without any HTTP or DB dependency: dec must already be positioned
// just past the feed's opening '{' (matching how fetchAndIngestProduction
// calls it), and flush is invoked once per completed batch (plus once more
// for any remainder) with a slice that is never longer than batchSize.
func (w *FeedWorker) StreamProductionRecords(dec *json.Decoder, batchSize int, flush func(batch []FeedRecord) error) (n int, defiantNotice, defiantLicense, mitreNotice string, err error) {
	return w.streamProductionRecords(dec, batchSize, flush)
}
