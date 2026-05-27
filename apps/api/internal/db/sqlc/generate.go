package sqlc

// The sqlc-generated repository layer is produced from db/schema.sql and the
// query files in db/query (see ../../../sqlc.yaml). Regenerate with:
//
//	go generate ./internal/db/sqlc
//
//go:generate go run github.com/sqlc-dev/sqlc/cmd/sqlc -f ../../../sqlc.yaml generate
