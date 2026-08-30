// Command dump-routes prints the COMPLETE production HTTP route surface of
// the API, one route per line, sorted, in "METHOD\tPATH" form (gin's :param
// syntax normalised to OpenAPI's {param} form).
//
// Why this exists: tests/contract/openapi_route_coverage_test.go's
// buildFullEngine builds the real engine via server.New but leaves several
// optional server.Deps fields nil (MCPDiscoveryH, MCPOAuthH, MCPTransportH,
// BillingSuspensionGate were the ones found on inspection). server.New guards
// every optional field with "if deps.X != nil" before registering its
// routes, so those routes are silently ABSENT from engine.Routes() — the
// contract test still passes because its allowlist never has to mention a
// route it never sees. A coverage gate built on that engine would report
// "fully covered" while the routes it can't see stay unmounted-in-the-gate's-
// eyes even though they are live in production.
//
// This command builds the same real engine, through the same server.New,
// but with every optional Deps field populated by a real or
// minimal-but-non-nil handler (see routes.go's buildEngine), so no
// nil-handler guard can hide a route from the dump.
//
// Usage (from apps/api, or any dir — it's a plain Go command):
//
//	go run ./cmd/dump-routes
//
// Output contract:
//   - stdout carries ONLY "METHOD\tPATH" route lines, sorted, nothing else —
//     safe to pipe directly into another tool.
//   - any diagnostic (a Deps field that could not be populated, a build
//     failure) goes to stderr, never stdout.
//   - exit code is non-zero, with a stderr message, if the engine fails to
//     build OR the route count is zero. An empty dump is always an error,
//     never a silent empty success.
//
// No database connection and no network call is made: buildEngine constructs
// every service against an empty, unconnected *db.Pool and this command only
// ever reads engine.Routes().
package main

import (
	"fmt"
	"os"
)

func main() {
	engine, omitted, err := buildEngine()
	for _, field := range omitted {
		fmt.Fprintf(os.Stderr, "dump-routes: server.Deps.%s is nil — its route(s) are NOT in this dump\n", field)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dump-routes: failed to build engine:", err)
		os.Exit(1)
	}

	lines := dumpRoutes(engine)
	if len(lines) == 0 {
		fmt.Fprintln(os.Stderr, "dump-routes: engine built but registered zero routes — this is always a bug, never treat it as a valid empty result")
		os.Exit(1)
	}

	for _, line := range lines {
		fmt.Println(line)
	}
}
