# apps/api — WPMgr control plane (Go + Gin)

Modular monolith. Domains live in `internal/<domain>/` with `handler/`,
`service/`, `repo/`, `model/` subpackages. Binary entrypoint:
`cmd/wpmgr/main.go`. Admin tooling: `cmd/wpmgr-cli/main.go`.

```bash
go build ./...
go test ./...
```

Real server, telemetry, and domains are built in Phase 4.
