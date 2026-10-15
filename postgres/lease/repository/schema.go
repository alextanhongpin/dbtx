// Package repository contains the native PostgreSQL lease schema.
package repository

import _ "embed"

//go:embed schema.sql
var Schema string

