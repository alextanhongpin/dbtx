// Package repository contains the native PostgreSQL lease schema.
package repository

import _ "embed"

//go:embed schema.sql
var baseSchema string

// Schema installs permanent lease rows and native ownership functions.
var Schema = baseSchema

// OwnershipMigrations replaces shared adapters with native functions without
// rewriting lease rows. Apply in one transaction before deploying new callers.
var OwnershipMigrations = baseSchema + "DROP VIEW IF EXISTS dbtx.resource_ownership;"
