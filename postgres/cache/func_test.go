package cache_test

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/postgres/cache/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

func TestFunc(t *testing.T) {
	ctx := t.Context()
	c := cache.New(repository.New(dbtest.DB(t)))

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

	idp := cache.Func(fn, &cache.FuncConfig[string, *User]{
		Cache: c,
		KeyFn: func(ctx context.Context, id string) (string, error) {
			return fmt.Sprintf("user:%s", id), nil
		},
	})

	t.Run("ok", func(t *testing.T) {
		// Given that the user does not exists,
		// When calling func,
		// It should create the user.
		res, loaded, err := idp(ctx, t.Name())

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
		res, loaded, err := idp(ctx, t.Name())

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)

		// When calling func,
		// It should load the user.
		res, loaded, err = idp(ctx, t.Name())
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
		res, loaded, err := idp(ctx, t.Name())

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
		res, loaded, err := idp(ctx, t.Name())

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
	c := cache.New(repository.New(dbtest.DB(t)))

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

	idp := cache.Idempotent(fn, &cache.FuncConfig[CreateUserDto, *User]{
		Cache: c,
		KeyFn: func(ctx context.Context, dto CreateUserDto) (string, error) {
			return fmt.Sprintf("user:%s", dto.IID), nil
		},
	})

	t.Run("ok", func(t *testing.T) {
		// Given that the user does not exists,
		// When calling func,
		// It should create the user.
		res, loaded, err := idp(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)
		// And the key will be cached.

		exists, err := c.Exists(ctx, fmt.Sprintf("user:%s", t.Name()))
		is.NoError(err)
		is.True(exists)
	})

	t.Run("exists", func(t *testing.T) {
		// Given that the user exists,
		res, loaded, err := idp(ctx, CreateUserDto{
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
		res, loaded, err = idp(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})
		is.NoError(err)
		is.True(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)

		// And the key should be created.
		exists, err := c.Exists(ctx, fmt.Sprintf("user:%s", t.Name()))
		is.NoError(err)
		is.True(exists)
	})

	t.Run("none", func(t *testing.T) {
		// Given that the user is not found in the create function,
		// When calling the function, it should be created
		res, loaded, err := idp(ctx, CreateUserDto{
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
		exists, err := c.Exists(ctx, fmt.Sprintf("user:%s", t.Name()))
		is.NoError(err)
		is.True(exists)
	})

	t.Run("error", func(t *testing.T) {
		// Given that the create returns error,
		// When calling the function, it should return error.
		res, loaded, err := idp(ctx, CreateUserDto{
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
		res, loaded, err := idp(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name(),
		})

		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal(t.Name(), res.ID)
		is.Equal(t.Name(), res.Name)

		// When calling with wrong name,
		res, loaded, err = idp(ctx, CreateUserDto{
			IID:  t.Name(),
			Name: t.Name() + ":edited",
		})
		// It should return error mismatch.
		is.ErrorIs(err, cache.ErrConflict)
		is.False(loaded)
		is.Empty(res)
	})

	t.Run("null", func(t *testing.T) {
		// Given that the key holds null, for example from a Store of a nil
		// value,
		dto := CreateUserDto{IID: t.Name(), Name: t.Name()}
		err := c.Store[*User](ctx, fmt.Sprintf("user:%s", dto.IID), nil, time.Minute)
		is := assert.New(t)
		is.NoError(err)

		// It should return a conflict instead of panicking.
		_, _, err = idp(ctx, dto)
		is.ErrorIs(err, cache.ErrConflict)
	})
}

func TestIdempotentRequestEquality(t *testing.T) {
	ctx := t.Context()
	c := cache.New(repository.New(dbtest.DB(t)))

	t.Run("large integers", func(t *testing.T) {
		type Transfer struct {
			AccountID int64
		}
		key := uuid.NewV7().String()
		idp := cache.Idempotent(func(ctx context.Context, req Transfer) (int64, time.Duration, error) {
			return req.AccountID, time.Minute, nil
		}, &cache.FuncConfig[Transfer, int64]{
			Cache: c,
			KeyFn: func(context.Context, Transfer) (string, error) { return key, nil },
		})

		_, _, err := idp(ctx, Transfer{AccountID: 9007199254740993})
		is := assert.New(t)
		is.NoError(err)

		// Differs from the first request only beyond float64 precision.
		_, _, err = idp(ctx, Transfer{AccountID: 9007199254740992})
		is.ErrorIs(err, cache.ErrConflict)
	})

	t.Run("raw json formatting", func(t *testing.T) {
		key := uuid.NewV7().String()
		var calls int
		idp := cache.Idempotent(func(ctx context.Context, req jsontext.Value) (string, time.Duration, error) {
			calls++
			return "ok", time.Minute, nil
		}, &cache.FuncConfig[jsontext.Value, string]{
			Cache: c,
			KeyFn: func(context.Context, jsontext.Value) (string, error) { return key, nil },
		})

		_, _, err := idp(ctx, jsontext.Value(`{"b": 1, "a": 9007199254740993}`))
		is := assert.New(t)
		is.NoError(err)

		// The same request, formatted differently.
		res, loaded, err := idp(ctx, jsontext.Value(`{ "a":9007199254740993,"b":1 }`))
		is.NoError(err)
		is.True(loaded)
		is.Equal("ok", res)
		is.Equal(1, calls)
	})
}
