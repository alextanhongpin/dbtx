// Package durabletest provides test helpers for code built on durable: an
// isolated PostgreSQL database with the schema applied, and a recording
// publisher for outbox tests.
//
// Databases come from DURABLE_TEST_DSN (or DATABASE_URL), which must point at
// a server where the user may CREATE DATABASE. Without it, tests are skipped.
// The repository compose.yaml database works:
//
//	DURABLE_TEST_DSN='postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable'
package durabletest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable/repository"
	_ "github.com/lib/pq"
)

// DSN returns the configured admin DSN, or "" when unset.
func DSN() string {
	if v := os.Getenv("DURABLE_TEST_DSN"); v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}

// NewDB creates a fresh database with the durable schema, returns a pool for
// it, and drops it when the test ends. It skips the test when no DSN is set.
func NewDB(tb testing.TB) *sql.DB {
	tb.Helper()
	dsn := DSN()
	if dsn == "" {
		tb.Skip("durabletest: set DURABLE_TEST_DSN to run database tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		tb.Fatal(err)
	}
	defer admin.Close()
	name := "durable_test_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		tb.Fatalf("durabletest: create database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		tb.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(20)
	if _, err := db.ExecContext(ctx, repository.Schema); err != nil {
		tb.Fatalf("durabletest: apply schema: %v", err)
	}
	tb.Cleanup(func() {
		_ = db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin, err := sql.Open("postgres", dsn)
		if err != nil {
			return
		}
		defer admin.Close()
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return db
}

// Gate blocks callers of Wait until Open is called. Use it inside a Hooks
// function to simulate a stop-the-world pause at a precise point.
type Gate struct {
	once    sync.Once
	ch      chan struct{}
	reached chan struct{}
	rOnce   sync.Once
}

func NewGate() *Gate { return &Gate{ch: make(chan struct{}), reached: make(chan struct{})} }

// Wait signals Reached and blocks until Open.
func (g *Gate) Wait() {
	g.rOnce.Do(func() { close(g.reached) })
	<-g.ch
}

// Reached is closed when the first caller arrives at Wait.
func (g *Gate) Reached() <-chan struct{} { return g.reached }

// Open releases every current and future waiter.
func (g *Gate) Open() { g.once.Do(func() { close(g.ch) }) }
