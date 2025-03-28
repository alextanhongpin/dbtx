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

func Init(opts ...Options) func() error {
	stop := func() error {
		return nil
	}

	once.Do(func() {
		var err error
		client, err = newClient(opts...)
		if err != nil {
			panic(err)
		}

		stop = client.stop
	})

	return stop
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

type Client struct {
	driver string
	dsn    string
	once   sync.Once
	stop   func() error
	txdb   string
}

func New(t *testing.T, opts ...Options) *Client {
	t.Helper()

	client, err := newClient(opts...)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := client.stop(); err != nil {
			t.Error(err)
		}
	})

	return client
}

func newClient(opts ...Options) (*Client, error) {
	opt := NewOptions().Merge(opts...)

	// Supports postgres based on driver type?
	res, err := testcontainer.Run(opt.Image, opt.Duration)
	if err != nil {
		return nil, err
	}

	if err := opt.Hook(res.DSN); err != nil {
		return nil, err
	}

	return &Client{
		stop:   res.Stop,
		dsn:    res.DSN,
		driver: opt.Driver,
	}, nil
}

// DB returns a new connection to the database.
// By default, returns the pool.
func (c *Client) DB(t *testing.T) *bun.DB {
	t.Helper()

	db := NewBun(c.dsn)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
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

		c.txdb = fmt.Sprintf("txdb:%s", uuid.New())

		// NOTE: We use `pg` driver, which bun uses instead of `postgres`.
		txdb.Register(c.txdb, "pg", c.dsn)
	})

	// Create a unique transaction for each connection.
	sqldb, err := sql.Open(c.txdb, uuid.NewString())
	if err != nil {
		t.Fatalf("failed to open tx: %v", err)
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

	db := bun.NewDB(sqldb, pgdialect.New())
	db.AddQueryHook(bundebug.NewQueryHook(
		bundebug.WithVerbose(true),
		bundebug.FromEnv("BUNDEBUG"),
	))

	return db
}
