// Package gen holds the ogen-generated OpenAPI types, validation, and client.
//
// The OpenAPI document at packages/openapi/openapi.yaml is the single source of
// truth. We use the generated request/response models and their Validate()
// methods; Gin (not ogen's router) owns HTTP routing in internal/server.
//
// Regenerate with `go generate ./internal/api/gen` (see apps/api/README.md).
package gen

//go:generate go run github.com/ogen-go/ogen/cmd/ogen --config ../../../ogen.yaml --target . --package gen --clean ../../../../../packages/openapi/openapi.yaml
