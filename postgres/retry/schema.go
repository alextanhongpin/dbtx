// Package retry provides PostgreSQL lifecycle functions for module-owned work rows.
// Functions operate through updatable views with a common column contract, in the
// caller's transaction. Domain selection, payloads and retention remain local.
package retry

import _ "embed"

//go:generate go run ../lease/ownership/cmd/bundle/main.go -root ../..
//go:generate go run ./cmd/bundle

//go:embed schema.sql
var retrySchema string

//go:embed shared_ownership.sql
var ownershipSchema string

// Schema installs the shared ownership primitives and retry transitions.
// Install before module lifecycle views. Safe to apply repeatedly.
var Schema = ownershipSchema + retrySchema
