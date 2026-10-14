// Package repository contains the lease schema. Install Schema through your
// application's migrations; constructors never execute DDL.
package repository

import _ "embed"

// Schema creates the table and functions. Existing deployments need explicit
// migrations for table changes. Never delete rows to clean up expired leases.
//
//go:embed schema.sql
var Schema string
