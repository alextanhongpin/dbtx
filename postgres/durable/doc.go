// Package durable holds what every durable feature shares: the error
// vocabulary, retry classification, observability and fault-injection hooks.
//
// Features live in subpackages: lease, txlock, keys, jobs, outbox, inbox,
// schedule and cache. All of them store state in the PostgreSQL schema
// "durable" (see the repository package) and use database/sql through
// github.com/alextanhongpin/dbtx.
package durable
