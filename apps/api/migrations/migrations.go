// Package migrations embeds the Atlas-authored versioned SQL migrations so the
// server can apply them on startup without shipping the Atlas binary. The .sql
// files here are the single source of truth and are also applied by
// `atlas migrate apply` in CLI-driven environments (see apps/api/README.md).
package migrations

import "embed"

// FS holds the embedded versioned migrations (the *.sql files in this dir).
// atlas.sum is not embedded: the pattern below matches only *.sql, and the
// runner never reads atlas.sum. It serves the atlas CLI alone.
//
//go:embed *.sql
var FS embed.FS
