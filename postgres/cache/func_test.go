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

	idfunc := cache.Func(fn, &cache.FuncConfig[string, *User]{
		Cache: c,
		KeyFn: func(ctx context.Context, id string) (string, error) {
			return fmt.Sprintf("user:%s", id), nil
		},
	})

	t.Run("ok", func(t *testing.T) {
		// Given that the user does not exists,
		// When calling func,
		// It should create the user.
		res, loaded, err := idfunc(ctx, t.Name())

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
		res, loaded, err := idfunc(ctx, t.Name())

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)

		// When calling func,
		// It should load the user.
		res, loaded, err = idfunc(ctx, t.Name())
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
		res, loaded, err := idfunc(ctx, t.Name())

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
		res, loaded, err := idfunc(ctx, t.Name())

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

func TestIdempotent(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	type User struct {
		// Non-empty id means a user exists.
		ID   string
		Name string
	}

	type CreateUserDto struct {
		IID  string // Idempotent ID.
		Name string
	}

	fn := func(ctx context.Context, dto CreateUserDto) (*User, time.Duration, error) {
		if strings.Contains(dto.IID, "error") {
			return nil, 0, assert.AnError
		}
		if strings.Contains(dto.IID, "none") {
			return &User{}, 10 * time.Second, nil
		}
		return &User{ID: dto.IID, Name: dto.Name}, time.Second, nil
	}

	idfunc := cache.Idempotent(fn, &cache.FuncConfig[CreateUserDto, *User]{
		Cache: c,
		KeyFn: func(ctx context.Context, dto CreateUserDto) (string, error) {
			return fmt.Sprintf("user:%s", dto.IID), nil
		},
	})

	t.Run("ok", func(t *testing.T) {
		// Given that the user does not exists,
		// When calling func,
		// It should create the user.
		res, loaded, err := idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)
		// And the key will be cached.

		exists, err := c.Exists(ctx, "user:TestFunc/ok")
		is.NoError(err)
		is.True(exists)
	})

	t.Run("exists", func(t *testing.T) {
		// Given that the user exists,
		res, loaded, err := idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)

		// When calling func,
		// It should load the user.
		res, loaded, err = idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})
		is.NoError(err)
		is.True(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)

		// And the key should be created.
		exists, err := c.Exists(ctx, "user:TestFunc/exists")
		is.NoError(err)
		is.True(exists)
	})

	t.Run("none", func(t *testing.T) {
		// Given that the user is not found in the create function,
		// When calling the function, it should be created
		res, loaded, err := idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		// And the response should be empty.
		is.Empty(res.ID)
		is.Empty(res.Name)

		// And the key should be created.
		exists, err := c.Exists(ctx, "user:TestFunc/none")
		is.NoError(err)
		is.True(exists)
	})

	t.Run("error", func(t *testing.T) {
		// Given that the create returns error,
		// When calling the function, it should return error.
		res, loaded, err := idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

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

	t.Run("mismatch", func(t *testing.T) {
		// Given that the user exists,
		res, loaded, err := idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)

		// When calling with wrong name,
		res, loaded, err = idfunc(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name() + ":edited",
		})
		// It should return error mismatch.
		is.ErrorIs(err, cache.ErrConflict)
		is.False(loaded)
		is.Empty(res)
	})
}
