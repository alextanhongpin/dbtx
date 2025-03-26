package testcontainer

import (
	"errors"
	"fmt"
	"strings"
	"time"

	dockertest "github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

func Postgres(image string, expiry time.Duration) (dsn string, close func() error, err error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return "", nil, fmt.Errorf("dockertest: could not construct pool: %w", err)
	}

	err = pool.Client.Ping()
	if err != nil {
		return "", nil, fmt.Errorf("dockertest: could not connect to docker: %w", err)
	}

	repo, tag, ok := strings.Cut(image, ":")
	if !ok {
		tag = "latest"
	}

	// Pulls an image, creates a container based on it and run it.
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: repo,
		Tag:        tag,
		Env: []string{
			"POSTGRES_PASSWORD=123456",
			"POSTGRES_USER=test",
			"POSTGRES_DB=test",
			"listen_addresses = '*'",
		},
	}, func(config *docker.HostConfig) {
		// Set AutoRemove to true so that stopped container goes away by itself.
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		return "", nil, fmt.Errorf("dockertest: could not start resources: %w", err)
	}

	// https://www.postgresql.org/docs/current/non-durability.html
	code, err := resource.Exec([]string{"postgres",
		// No need to flush data to disk.
		"-c", "fsync=off",

		// No need to force WAL writes to disk on every commit.
		"-c", "synchronous_commit=off",

		// No need to guard against partial page writes.
		"-c", "full_page_writes=off",
	}, dockertest.ExecOptions{})
	if err != nil {
		return "", nil, err
	}
	if code != 1 {
		return "", nil, fmt.Errorf("dockertest: exec code is not 1")
	}

	hostAndPort := resource.GetHostPort("5432/tcp")
	dsn = fmt.Sprintf("postgres://test:123456@%s/test?sslmode=disable", hostAndPort)

	_ = resource.Expire(uint(expiry.Seconds())) // Tell docker to kill the container after the specified expiry period.

	// Exponential backoff-retry, because the application in the container might
	// not be ready to accept connections yet.
	pool.MaxWait = 120 * time.Second
	if err := pool.Retry(func() error {
		exitCode, err := resource.Exec([]string{"pg_isready", "-h", "localhost", "-p", "5432", "-U", "test", "-d", "test"}, dockertest.ExecOptions{})
		if err != nil {
			return err
		}

		switch exitCode {
		case 0: // Accepting connections.
			return nil
		case 1: // Rejecting connections.
			return errors.New("dockertest: postgres is rejecting connections")
		case 2: // No response.
			return errors.New("dockertest: postgres is not responding")
		default:
			return errors.New("dockertest: unknown exit code")
		}
	}); err != nil {
		return "", nil, fmt.Errorf("dockertest: could not connect to docker: %s", err)
	}

	close = func() error {
		return pool.Purge(resource)
	}

	return dsn, close, nil
}
