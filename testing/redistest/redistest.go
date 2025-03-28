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

var once sync.Once
var inst *Instance

func Init(opts ...Options) func() error {
	stop := func() error {
		return nil
	}

	once.Do(func() {
		var err error
		inst, err = newPod(opts...)
		if err != nil {
			panic(err)
		}

		stop = inst.stop
	})

	return stop
}

func Addr() string {
	if inst == nil || inst.addr == "" {
		panic(fmt.Errorf("redistest: Init must be called at TestMain"))
	}

	return inst.addr
}

func Client(t *testing.T) *redis.Client {
	if inst == nil {
		panic(fmt.Errorf("redistest: Init must be called at TestMain"))
	}

	return inst.Client(t)
}

type Options struct {
	Image  string
	Expiry time.Duration
}

func NewOptions() *Options {
	return &Options{
		Image:  "redis:latest",
		Expiry: 10 * time.Minute,
	}
}

func (o *Options) Merge(opts ...Options) *Options {
	for _, opt := range opts {
		if opt.Image != "" {
			o.Image = opt.Image
		}

		if opt.Expiry != 0 {
			o.Expiry = opt.Expiry
		}
	}

	return o
}

type Instance struct {
	addr string
	stop func() error
}

func New(t *testing.T, opts ...Options) *Instance {
	t.Helper()

	inst, err := newPod(opts...)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := inst.stop(); err != nil {
			t.Error(err)
		}
	})

	return inst
}

func newPod(opts ...Options) (*Instance, error) {
	opt := NewOptions().Merge(opts...)
	res, err := Run(opt.Image, opt.Expiry)
	if err != nil {
		return nil, err
	}

	return &Instance{
		addr: res.Addr,
		stop: res.Stop,
	}, nil
}

func (inst *Instance) Addr() string {
	return inst.addr
}

func (inst *Instance) Client(t *testing.T) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr: inst.addr,
	})

	t.Cleanup(func() {
		client.Close()
	})

	return client
}

type RunResult struct {
	Addr string
	Stop func() error
}

func Run(image string, expiry time.Duration) (*RunResult, error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, fmt.Errorf("dockertest: could not construct pool: %w", err)
	}

	err = pool.Client.Ping()
	if err != nil {
		return nil, fmt.Errorf("dockertest: could not connect to Docker: %w", err)
	}

	repo, tag, ok := strings.Cut(image, ":")
	if !ok {
		tag = "latest"
	}

	resource, err := pool.Run(repo, tag, nil)
	if err != nil {
		return nil, fmt.Errorf("dockertest: could not start resource: %w", err)
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
		return nil, fmt.Errorf("dockertest: could not connect to docker: %w", err)
	}

	return &RunResult{
		Addr: addr,
		Stop: func() error {
			return pool.Purge(resource)
		},
	}, nil
}
