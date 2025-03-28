package redistest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	dockertest "github.com/ory/dockertest/v3"
	redis "github.com/redis/go-redis/v9"
)

type Options struct {
	Image  string
	Expiry time.Duration
}

func (o *Options) Merge(other Options) Options {
	if other.Image != "" {
		o.Image = other.Image
	}

	if other.Expiry != 0 {
		o.Expiry = other.Expiry
	}

	return *o
}

type InitOptions = Options

var once sync.Once
var c *Container

func Init(opts ...InitOptions) func() error {
	var stop func() error
	once.Do(func() {
		var err error
		c, err = newContainer(opts...)
		if err != nil {
			panic(err)
		}
		stop = c.close
	})

	return stop
}

func Addr() string {
	if c == nil || c.addr == "" {
		panic(fmt.Errorf("redistest: Init must be called at TestMain"))
	}

	return c.addr
}

func Client(t *testing.T) *redis.Client {
	if c == nil {
		panic(fmt.Errorf("redistest: Init must be called at TestMain"))
	}

	return c.Client(t)
}

type Option func(c *config) error

type config struct {
	Repository string
	Tag        string
}

func newConfig() *config {
	return &config{
		Repository: "redis",
		Tag:        "latest",
	}
}

func (c *config) apply(opts ...Option) error {
	for _, o := range opts {
		if err := o(c); err != nil {
			return err
		}
	}

	return nil
}

type Container struct {
	addr  string
	close func() error
}

func New(t *testing.T, opts ...Options) *Container {
	t.Helper()
	c, err := newContainer(opts...)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := c.close(); err != nil {
			t.Error(err)
		}
	})

	return c
}

func newContainer(opts ...Options) (*Container, error) {
	opt := &Options{
		Image:  "redis:latest",
		Expiry: 10 * time.Minute,
	}
	// Only take the first option.
	if len(opts) > 0 {
		opt.Merge(opts[0])
	}

	addr, stop, err := Run(opt.Image, opt.Expiry)
	if err != nil {
		return nil, err
	}

	return &Container{
		addr:  addr,
		close: stop,
	}, nil
}

func (c *Container) Addr() string {
	return c.addr
}

func (c *Container) Client(t *testing.T) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr: c.addr,
	})

	t.Cleanup(func() {
		client.Close()
	})

	return client
}

func Run(image string, expiry time.Duration) (string, func() error, error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return "", nil, fmt.Errorf("dockertest: could not construct pool: %s", err)
	}

	err = pool.Client.Ping()
	if err != nil {
		return "", nil, fmt.Errorf("dockertest: could not connect to Docker: %s", err)
	}

	repo, tag, ok := strings.Cut(image, ":")
	if !ok {
		tag = "latest"
	}

	resource, err := pool.Run(repo, tag, nil)
	if err != nil {
		return "", nil, fmt.Errorf("dockertest: could not start resource: %s", err)
	}
	_ = resource.Expire(uint(expiry.Seconds())) // Tell docker to kill the container after the specified expiry period.

	addr := resource.GetHostPort("6379/tcp")
	if err = pool.Retry(func() error {
		db := redis.NewClient(&redis.Options{
			Addr: addr,
		})

		ctx := context.Background()
		return db.Ping(ctx).Err()
	}); err != nil {
		return "", nil, fmt.Errorf("could not connect to docker: %s", err)
	}

	return addr, func() error {
		// When you're done, kill and remove the container
		return pool.Purge(resource)
	}, nil
}
