// Package ownership exposes PostgreSQL ownership primitives over module-owned rows.
// It creates no work tables. Install Schema explicitly through migrations.
package ownership

import _ "embed"

//go:generate go run ./cmd/bundle -root ../../..

// Schema installs the shared grant, guard, renewal and release functions.
//
//go:embed schema.sql
var Schema string
