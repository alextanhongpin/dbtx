// Command ab serves the read-only A/B dashboard on a loopback address.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/ab"
	"github.com/alextanhongpin/dbtx/postgres/ab/dashboard"
	"github.com/alextanhongpin/dbtx/postgres/ab/repository"
	_ "github.com/lib/pq"
)

type demoReader struct{}

func (demoReader) Results(_ context.Context, id string) (ab.Results, error) {
	if id != "checkout-v1" {
		return ab.Results{}, ab.ErrNotExist
	}
	ended := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	return ab.Results{Experiment: ab.Experiment{ID: id + " · DEMO / sample data", Control: "Original checkout", Treatment: "Simplified checkout", CreatedAt: ended.AddDate(0, 0, -14), ClosedAt: &ended}, Control: ab.Counts{Subjects: 1000, Conversions: 100}, Treatment: ab.Counts{Subjects: 1000, Conversions: 200}}, nil
}
func run() error {
	addr := flag.String("addr", "127.0.0.1:8092", "loopback listen address")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN (defaults to DATABASE_URL)")
	demo := flag.Bool("demo", false, "show sample checkout-v1 results without a database")
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen address must be loopback")
	}
	var reader dashboard.Reader
	if *demo {
		reader = demoReader{}
	} else {
		if *dsn == "" {
			return fmt.Errorf("set DATABASE_URL or -dsn, or use -demo")
		}
		db, err := sql.Open("postgres", *dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("connect to PostgreSQL: %w", err)
		}
		reader = repository.New(db)
	}
	log.Printf("A/B dashboard: http://%s/?experiment=checkout-v1 (demo=%t)", *addr, *demo)
	server := &http.Server{Addr: *addr, Handler: dashboard.New(reader), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	return server.ListenAndServe()
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
