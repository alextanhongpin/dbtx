// Package retry provides PostgreSQL lifecycle functions for module-owned work rows.
// Functions operate through updatable views with a common column contract, in the
// caller's transaction. Domain selection, payloads and retention remain local.
package retry

import _ "embed"

//go:generate go run ./cmd/bundle

// Schema installs the shared functions. Install it before module lifecycle views.
// It is safe to apply repeatedly; constructors never execute DDL.
//
//go:embed schema.sql
var Schema string
