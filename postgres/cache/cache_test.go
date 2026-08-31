package cache_test

import (
	_ "embed"

	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

var (
	//go:embed internal/schema.sql
	schema      string
	ErrRollback = errors.New("rollback")
	dbtestOpts  = dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook:  migrate,
	}
)

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(schema)
	return err
}

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtestOpts)
	defer stop()

	m.Run()
}

func TestCache(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))
	t.Helper()

	t.Run("empty", func(t *testing.T) {
		key := t.Name()

		value, err := c.Load[any](ctx, key)
		is := assert.New(t)
		is.ErrorIs(err, cache.ErrNotExist)
		is.Empty(value)
	})

	t.Run("exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)

		is := assert.New(t)
		is.NoError(err)

		loaded, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(value, loaded)
	})

	t.Run("store once", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.StoreOnce(ctx, key, value, time.Second)

		is := assert.New(t)
		is.NoError(err)

		err = c.StoreOnce(ctx, key, value, time.Second)
		is.ErrorIs(err, cache.ErrExists)
	})

	t.Run("load or store empty", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		old, loaded, err := c.LoadOrStore(ctx, key, value, time.Second)

		is := assert.New(t)
		is.NoError(err)
		is.Equal(value, old)
		is.False(loaded)

		old, loaded, err = c.LoadOrStore(ctx, key, value, time.Second)
		is.NoError(err)
		is.Equal(value, old)
		is.True(loaded)
	})

	t.Run("load or store exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		old, loaded, err := c.LoadOrStore(ctx, key, value, time.Second)

		is.NoError(err)
		is.Equal(value, old)
		is.True(loaded)
	})

	t.Run("load and delete empty", func(t *testing.T) {
		key := t.Name()
		old, err := c.LoadAndDelete[any](ctx, key)
		is := assert.New(t)
		is.ErrorIs(err, cache.ErrNotExist)
		is.Empty(old)
	})

	t.Run("load and delete exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		old, err := c.LoadAndDelete[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(value, old)

		_, err = c.Load[any](ctx, key)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("compare and delete empty", func(t *testing.T) {
		key := t.Name()
		old := []byte("hello")
		err := c.CompareAndDelete(ctx, key, old)

		is := assert.New(t)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("compare and delete exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		err = c.CompareAndDelete(ctx, key, value)
		is.NoError(err)

		_, err = c.Load[any](ctx, key)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("compare and swap empty", func(t *testing.T) {
		key := t.Name()
		old := []byte("hello")
		value := []byte("hello")
		err := c.CompareAndSwap(ctx, key, old, value, time.Second)

		is := assert.New(t)
		is.ErrorIs(err, cache.ErrNotExist)

		_, err = c.Load[any](ctx, key)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("compare and swap exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		newValue := []byte("world")
		err = c.CompareAndSwap(ctx, key, value, newValue, time.Second)
		is.NoError(err)

		loaded, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(newValue, loaded)
	})
}

func TestCacheCoverage(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))
	t.Helper()

	t.Run("exists", func(t *testing.T) {
		key := t.Name()
		exists, err := c.Exists(ctx, key)
		assert.NoError(t, err)
		assert.False(t, exists)

		err = c.Store(ctx, key, []byte("hello"), time.Second)
		assert.NoError(t, err)

		exists, err = c.Exists(ctx, key)
		assert.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("ttl", func(t *testing.T) {
		key := t.Name()
		ttl, err := c.TTL(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, time.Duration(-2), ttl)

		err = c.Store(ctx, key, []byte("hello"), 2*time.Second)
		assert.NoError(t, err)
		ttl, err = c.TTL(ctx, key)
		assert.NoError(t, err)
		assert.True(t, ttl > 0 && ttl <= 2*time.Second)

		key2 := t.Name() + "2"
		err = c.Store(ctx, key2, []byte("hello"), 0)
		assert.NoError(t, err)
		ttl, err = c.TTL(ctx, key2)
		assert.NoError(t, err)
		assert.Equal(t, time.Duration(-1), ttl)
	})

	t.Run("expire", func(t *testing.T) {
		key := t.Name()
		err := c.Store(ctx, key, []byte("hello"), 0)
		assert.NoError(t, err)

		ttl, err := c.TTL(ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, time.Duration(-1), ttl)

		err = c.Expire(ctx, key, time.Second)
		assert.NoError(t, err)

		ttl, err = c.TTL(ctx, key)
		assert.NoError(t, err)
		assert.True(t, ttl > 0 && ttl <= time.Second)
	})

	t.Run("delete", func(t *testing.T) {
		key := t.Name()
		err := c.Delete(ctx, key)
		assert.ErrorIs(t, err, cache.ErrNotExist)

		err = c.Store(ctx, key, []byte("hello"), time.Second)
		assert.NoError(t, err)

		err = c.Delete(ctx, key)
		assert.NoError(t, err)

		_, err = c.Load[any](ctx, key)
		assert.ErrorIs(t, err, cache.ErrNotExist)
	})

	t.Run("load or create", func(t *testing.T) {
		key := t.Name()
		called := false
		curr, loaded, err := c.LoadOrCreate(ctx, key, func(ctx context.Context, key string) (string, time.Duration, error) {
			called = true
			return "value", time.Second, nil
		})
		assert.NoError(t, err)
		assert.False(t, loaded)
		assert.Equal(t, "value", curr)
		assert.True(t, called)

		called = false
		curr, loaded, err = c.LoadOrCreate(ctx, key, func(ctx context.Context, key string) (string, time.Duration, error) {
			called = true
			return "value2", time.Second, nil
		})
		assert.NoError(t, err)
		assert.True(t, loaded)
		assert.Equal(t, "value", curr)
		assert.False(t, called)
	})

	t.Run("compare and delete mismatch", func(t *testing.T) {
		key := t.Name()
		err := c.Store(ctx, key, []byte("hello"), time.Second)
		assert.NoError(t, err)

		err = c.CompareAndDelete(ctx, key, []byte("wrong"))
		assert.ErrorIs(t, err, cache.ErrNotExist)

		_, err = c.Load[[]byte](ctx, key)
		assert.NoError(t, err)
	})

	t.Run("compare and swap mismatch", func(t *testing.T) {
		key := t.Name()
		err := c.Store(ctx, key, []byte("hello"), time.Second)
		assert.NoError(t, err)

		err = c.CompareAndSwap(ctx, key, []byte("wrong"), []byte("world"), time.Second)
		assert.ErrorIs(t, err, cache.ErrNotExist)

		loaded, err := c.Load[[]byte](ctx, key)
		assert.NoError(t, err)
		assert.Equal(t, []byte("hello"), loaded)
	})
}
