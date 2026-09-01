package cache_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

func TestFunc(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	type User struct {
		// Non-empty id means a user exists.
		ID string
	}

	fn := func(ctx context.Context, id string) (*User, time.Duration, error) {
		if strings.Contains(id, "error") {
			return nil, 0, assert.AnError
		}
		if strings.Contains(id, "none") {
			return &User{}, 10 * time.Second, nil
		}
		return &User{ID: id}, time.Second, nil
	}

	wrapped := cache.Func(fn, &cache.FuncConfig[string, *User]{
		Cache: c,
		KeyFn: func(ctx context.Context, id string) (string, error) {
			return fmt.Sprintf("user:%s", id), nil
		},
	})

	t.Run("ok", func(t *testing.T) {
		// Given that the user does not exists,
		// When calling func,
		// It should create the user.
		res, loaded, err := wrapped(ctx, t.Name())

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)
		// And the key will be cached.

		exists, err := c.Exists(ctx, "user:TestFunc/ok")
		is.NoError(err)
		is.True(exists)
	})

	t.Run("exists", func(t *testing.T) {
		// Given that the user exists,
		res, loaded, err := wrapped(ctx, t.Name())

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)

		// When calling func,
		// It should load the user.
		res, loaded, err = wrapped(ctx, t.Name())
		is.NoError(err)
		is.True(loaded)
		is.Equal(t.Name(), res.ID)

		// And the key should be created.
		exists, err := c.Exists(ctx, "user:TestFunc/exists")
		is.NoError(err)
		is.True(exists)
	})

	t.Run("none", func(t *testing.T) {
		// Given that the user is not found in the create function,
		// When calling the function, it should be created
		res, loaded, err := wrapped(ctx, t.Name())

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		// And the response should be empty.
		is.Empty(res.ID)

		// And the key should be created.
		exists, err := c.Exists(ctx, "user:TestFunc/none")
		is.NoError(err)
		is.True(exists)
	})

	t.Run("error", func(t *testing.T) {
		// Given that the create returns error,
		// When calling the function, it should return error.
		res, loaded, err := wrapped(ctx, t.Name())

		is := assert.New(t)
		is.ErrorIs(err, assert.AnError)
		is.False(loaded)
		// And the response should be empty.
		is.Empty(res)

		// And no key should be created.
		exists, err := c.Exists(ctx, "user:TestFunc/error")
		is.NoError(err)
		is.False(exists)
	})
}
