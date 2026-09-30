package mcp

import "context"

// requestRowMarker is how a tool handler tells the transport that it wrote
// mcp.tool.called itself, inside the transaction that inserted a request row
// (R1). The transport skips its own RecordToolCall ONLY when the marker is
// set. Any other outcome of an EffectRequest tool (a read branch, a dedupe
// answer, anything that wrote no request row) is recorded by the transport,
// fail-closed, before its text is returned.
type requestRowMarker struct{ recorded bool }

type requestRowMarkerKey struct{}

// withRequestRowMarker attaches a fresh marker to ctx.
func withRequestRowMarker(ctx context.Context) (context.Context, *requestRowMarker) {
	m := &requestRowMarker{}
	return context.WithValue(ctx, requestRowMarkerKey{}, m), m
}

// markRequestRowRecorded records that this call's mcp.tool.called row was
// written in the request row's own transaction. Call it only after that
// RecordInTx succeeded; if the transaction then fails to commit, the handler
// returns an error and the marker is never consulted.
func markRequestRowRecorded(ctx context.Context) {
	if m, ok := ctx.Value(requestRowMarkerKey{}).(*requestRowMarker); ok {
		m.recorded = true
	}
}
