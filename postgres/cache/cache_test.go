package cache_test

import (
	_ "embed"

	_ "github.com/lib/pq"

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
