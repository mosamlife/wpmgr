// Package contract holds the spec-versus-code contract gates: the tests that
// diff packages/openapi/openapi.yaml against the REAL production Gin engine.
//
// It is deliberately a separate package from apps/api/tests. Those are
// container-backed integration tests: 344 test functions, most of which stand
// up their own ephemeral Postgres through testcontainers, which is far too slow
// for the fast per-PR lane and is why .github/workflows/ci.yml excludes that
// package from `go test`. They are not unrun: they have their own lane in
// .github/workflows/api-integration.yml, which executes them on every PR that
// touches apps/api, on main, and nightly.
//
// The gates in here need no container at all (they build the engine against an
// empty *db.Pool and only read engine.Routes()), so they live one directory
// down, where the CI package filter is anchored to exclude only
// `/apps/api/tests` itself and every gate in this package runs on every PR, in
// about a second, in the fast lane. That placement is the point and should not
// change: a spec-versus-code mismatch must be caught by the cheap lane that
// every PR pays for, not only by the expensive one.
//
// The rule that keeps this working: nothing in this package may start a
// container or open a database connection. If a test here ever needs one, it
// belongs in apps/api/tests instead.
//
// This file exists only so the package has a non-test Go file and is therefore
// buildable by `go build ./...`; all the actual gates live in _test.go files.
package contract
