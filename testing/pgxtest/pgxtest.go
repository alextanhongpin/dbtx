package pgxtest

import (
	"cmp"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/pgxtx/pgxtest/internal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var once sync.Once
var client *Client

type Options struct {
	Image    string
	Duration time.Duration
	Hook     func(dsn string) error
}

type InitOptions = Options

func Init(opts InitOptions) (close func() error) {
	once.Do(func() {
		var err error
		client, err = newClient(opts)
		if err != nil {
			panic(err)
		}

		close = client.close
	})

	return
}

// DB is meant to keep the interface consistent.
func DB(t *testing.T) *pgxpool.Pool {
	return client.DB(t)
}

func Pool(t *testing.T) *pgxpool.Pool {
	return client.DB(t)
}

func Conn(t *testing.T) *pgx.Conn {
	return client.Conn(t)
}

func DSN() string {
	return client.DSN()
}

type Client struct {
	close func() error
	dsn   string
}

func New(t *testing.T, opts Options) *Client {
	t.Helper()

	client, err := newClient(opts)
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

func newClient(opts Options) (*Client, error) {
	var (
		duration = cmp.Or(opts.Duration, 10*time.Minute)
		hook     = opts.Hook
		image    = cmp.Or(opts.Image, "postgres:latest")
	)

	dsn, close, err := internal.Run(image, duration)
	if err != nil {
		return nil, err
	}

	if hook != nil {
		if err := hook(dsn); err != nil {
			return nil, err
		}
	}

	return &Client{
		close: close,
		dsn:   dsn,
	}, nil
}

// DB returns a new connection to the database.
// By default, returns the pool.
func (c *Client) DB(t *testing.T) *pgxpool.Pool {
	return c.Pool(t)
}

func (c *Client) Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	// TODO: Replace with t.Context() in go1.24
	pool, err := pgxpool.New(context.Background(), c.dsn)
	if err != nil {
		t.Error(err)
	}
	t.Cleanup(func() {
		pool.Close()
	})

	return pool
}

func (c *Client) Conn(t *testing.T) *pgx.Conn {
	t.Helper()

	// TODO: Replace with t.Context() in go1.24
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, c.dsn)
	if err != nil {
		t.Error(err)
	}
	t.Cleanup(func() {
		conn.Close(ctx)
	})

	return conn
}

func (c *Client) DSN() string {
	return c.dsn
}
