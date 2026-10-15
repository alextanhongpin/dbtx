// Package repository contains the lease schema. Install Schema through your
// application's migrations; constructors never execute DDL.
package repository

import (
	_ "embed"

	"github.com/alextanhongpin/dbtx/postgres/lease/ownership"
)

//go:embed schema.sql
var baseSchema string

//go:embed ownership.sql
var ownershipSchema string

// Schema installs the base objects and shared ownership adapters.
// Never delete rows to clean up expired leases: they retain token history.
var Schema = ownership.Schema + baseSchema + ownershipSchema

// OwnershipMigrations adds shared functions and views to an existing installation.
// Apply explicitly in one transaction before deploying new callers.
var OwnershipMigrations = ownership.Schema + ownershipSchema
