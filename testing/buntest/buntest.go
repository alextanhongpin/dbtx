package buntest

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-txdb"
	"github.com/alextanhongpin/dbtx/testing/testcontainer"
	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bundebug"
)

var once sync.Once
var client *Client

type Options struct {
	Driver   string
	Duration time.Duration
	Hook     func(dsn string) error
	Image    string
}

func NewOptions() *Options {
	return &Options{
		Driver:   "postgres",
		Duration: 10 * time.Minute,
		Image:    "postgres:latest",
		Hook: func(dsn string) error {
			return nil
		},
	}
}

func (o *Options) Merge(opts ...Options) *Options {
	for _, opt := range opts {
		if opt.Driver != "" {
			o.Driver = opt.Driver
		}

		if opt.Duration != 0 {
			o.Duration = opt.Duration
		}

		if opt.Hook != nil {
			o.Hook = opt.Hook
		}

		if opt.Image != "" {
			o.Image = opt.Image
		}
	}

	return o
}

type InitOptions = Options

func Init(opts ...InitOptions) (close func() error) {
	once.Do(func() {
		var err error
		client, err = newClient(opts...)
		if err != nil {
			panic(err)
		}

		close = client.close
	})

	return
}

func DB(t *testing.T) *bun.DB {
	return client.DB(t)
}

func Tx(t *testing.T) *bun.DB {
	return client.Tx(t)
}

func DSN() string {
	return client.DSN()
}

type Client struct {
	close  func() error
	driver string
	dsn    string
	once   sync.Once
	txdb   string
}

func New(t *testing.T, opts ...Options) *Client {
	t.Helper()

	// TODO: Add semaphore here to prevent excessive creation of database.
	client, err := newClient(opts...)
	if err != nil {
		t.Error(err)
	}
	t.Cleanup(func() {
		if err := client.close(); err != nil {
			t.Error(err)
		}
	})

	return client
}

func newClient(opts ...Options) (*Client, error) {
	opt := NewOptions().Merge(opts...)

	// Supports postgres based on driver type?
	dsn, close, err := testcontainer.Postgres(opt.Image, opt.Duration)
	if err != nil {
		return nil, err
	}

	if err := opt.Hook(dsn); err != nil {
		return nil, err
	}

	return &Client{
		close:  close,
		dsn:    dsn,
		driver: opt.Driver,
	}, nil
}

// DB returns a new connection to the database.
// By default, returns the pool.
func (c *Client) DB(t *testing.T) *bun.DB {
	t.Helper()

	db := NewBun(c.dsn)

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func (c *Client) Tx(t *testing.T) *bun.DB {
	t.Helper()

	c.once.Do(func() {
		// NOTE: We need to run this once to register the sql driver `pg`.
		// Otherwise txdb will not be able to register this driver.
		bunDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(c.dsn)))
		if err := bunDB.Ping(); err != nil {
			t.Fatalf("failed to ping: %v", err)
		}

		// NOTE: We can close this connection immediately, since we will be
		// creating a new one for every test.
		if err := bunDB.Close(); err != nil {
			t.Fatalf("failed to close bun: %v", err)
		}

		c.txdb = fmt.Sprintf("txdb_%s", uuid.New())
		// NOTE: We use `pg` driver, which bun uses instead of `postgres`.
		txdb.Register(c.txdb, "pg", c.dsn)
	})

	// Create a unique transaction for each connection.
	sqldb, err := sql.Open(c.txdb, uuid.NewString())
	if err != nil {
		t.Errorf("failed to open tx: %v", err)
	}

	db := bun.NewDB(sqldb, pgdialect.New())
	db.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(true),
	))
	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func (c *Client) DSN() string {
	return c.dsn
}

func NewBun(dsn string) *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))

	applyDefaults(sqldb)

	db := bun.NewDB(sqldb, pgdialect.New())
	db.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(true),
		bundebug.FromEnv("BUNDEBUG"),
	))

	return db
}

func applyDefaults(db *sql.DB) {
	// https://www.alexedwards.net/blog/configuring-sqldb
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(1 * time.Hour)
	db.SetConnMaxIdleTime(5 * time.Minute)
}
