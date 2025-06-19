package dbtest

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-txdb"
	"github.com/alextanhongpin/dbtx/testing/testcontainer"
	"github.com/alextanhongpin/testdump/sqldump"
	"github.com/alextanhongpin/testdump/yamldump"
	"github.com/google/uuid"
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

func DB(t *testing.T) *sql.DB {
	if client == nil {
		panic(fmt.Errorf("dbtest: Init must be called at TestMain"))
	}

	return client.DB(t)
}

func Tx(t *testing.T) *sql.DB {
	if client == nil {
		panic(fmt.Errorf("dbtest: Init must be called at TestMain"))
	}

	return client.Tx(t)
}

func DSN() string {
	if client == nil || client.DSN() == "" {
		panic(fmt.Errorf("dbtest: Init must be called at TestMain"))
	}

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
	stop   func() error
	driver string
	dsn    string
	once   sync.Once
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
		driver: opt.Driver,
		dsn:    res.DSN,
		stop:   res.Stop,
	}, nil
}

// DB returns a new connection to the database.
// By default, returns the pool.
func (c *Client) DB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open(c.driver, c.dsn)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	return db
}

func (c *Client) Tx(t *testing.T) *sql.DB {
	t.Helper()

	// Lazily initialize the txdb.
	c.once.Do(func() {
		c.txdb = fmt.Sprintf("txdb:%s", uuid.New())
		txdb.Register(c.txdb, c.driver, c.dsn)
	})

	// Returns a new identifier for every open connection.
	db, err := sql.Open(c.txdb, uuid.New().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	return db
}

func (c *Client) DSN() string {
	return c.dsn
}

func Dump(t *testing.T, db *sql.DB, query string, args []any, options ...yamldump.Option) {
	t.Helper()

	r, err := sqldump.Query(t.Context(), db, query, args...)
	if err != nil {
		t.Fatal(err)
	}

	yamldump.Dump(t, r, options...)
}

func WithDumper(t *testing.T, db *sql.DB, query string, args []any, dumper *yamldump.Dumper, options ...yamldump.Option) {
	t.Helper()

	r, err := sqldump.Query(t.Context(), db, query, args...)
	if err != nil {
		t.Fatal(err)
	}

	dumper.Dump(t, r, options...)
}
