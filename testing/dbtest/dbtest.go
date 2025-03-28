package dbtest

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-txdb"
	"github.com/alextanhongpin/dbtx/testing/testcontainer"
	"github.com/google/uuid"
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

// DB is meant to keep the interface consistent.
func DB(t *testing.T) *sql.DB {
	return client.DB(t)
}

func Tx(t *testing.T) *sql.DB {
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
func (c *Client) DB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open(c.driver, c.dsn)
	if err != nil {
		t.Error(err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})

	return db
}

func (c *Client) Tx(t *testing.T) *sql.DB {
	t.Helper()

	// Lazily initialize the txdb.
	c.once.Do(func() {
		c.txdb = fmt.Sprintf("txdb%s", uuid.New())
		txdb.Register(c.txdb, c.driver, c.dsn)
	})

	// Returns a new identifier for every open connection.
	db, err := sql.Open(c.txdb, uuid.New().String())
	if err != nil {
		t.Error(err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})

	return db
}

func (c *Client) DSN() string {
	return c.dsn
}
