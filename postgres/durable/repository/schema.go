// Package repository holds the durable schema. Install it through your
// application's migrations; constructors never execute DDL.
package repository

import "embed"

// Schema creates every durable table and function. It can be applied more
// than once; existing tables are not altered by reapplying it.
//
//go:embed schema.sql
var Schema string

// Migrations holds numbered migration files (migrations/0001_init.sql, ...)
// for migration tools that read an fs.FS.
//
//go:embed migrations/*.sql
var Migrations embed.FS
