package cache_test

import (
	_ "embed"

	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

var (
	//go:embed internal/schema.sql
	schema string
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
	opts := dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook:  migrate,
	}
	stop := dbtest.Init(opts)
	defer stop()

	m.Run()
}

func TestCompareAndDelete(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()
		old := []byte("hello")
		err := c.CompareAndDelete(ctx, key, old)

		// Given that the key does not exist,
		// When compare and delete,
		// Then it should return error not exist.
		is := assert.New(t)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)

		is := assert.New(t)
		is.NoError(err)

		err = c.CompareAndDelete(ctx, key, value)
		is.NoError(err)

		// Given that the key exists,
		// When compare and delete valid,
		// Then it should delete.

		_, err = c.Load[any](ctx, key)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("mismatch", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key exists,
		err := c.Store(ctx, key, value, time.Second)

		is := assert.New(t)
		is.NoError(err)

		// When compare and delete mismatch,
		newValue := []byte("world")
		err = c.CompareAndDelete(ctx, key, newValue)

		// Then it should return not exist.
		is.ErrorIs(err, cache.ErrNotExist)

		// And it should not delete.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.True(exists)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key expired,
		err := c.Store(ctx, key, value, -time.Second)

		is := assert.New(t)
		is.NoError(err)

		// When compare and delete,
		// Then it should invalidate cache,
		// And return not exist.
		err = c.CompareAndDelete(ctx, key, value)
		is.ErrorIs(err, cache.ErrNotExist)

		// And it should not delete.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.False(exists)
	})
}

func TestCompareAndSwap(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()
		old := []byte("hello")
		value := []byte("hello")
		// Given that the value does not exist.
		// When compare and swap,
		err := c.CompareAndSwap(ctx, key, old, value, time.Second)

		// Then it should return not exist.
		is := assert.New(t)
		is.ErrorIs(err, cache.ErrNotExist)

		_, err = c.Load[any](ctx, key)
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the value exists,
		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When compare and swap valid,
		// Then it should swap with new value.
		newValue := []byte("world")
		err = c.CompareAndSwap(ctx, key, value, newValue, time.Second)
		is.NoError(err)

		loaded, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(newValue, loaded)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the value expired,
		err := c.Store(ctx, key, value, -time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When compare and swap,
		newValue := []byte("world")
		err = c.CompareAndSwap(ctx, key, value, newValue, time.Second)
		// Then it should return err not exists.
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("mismatch", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the value exists,
		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When compare and swap mismatch,
		wrongOldValue := []byte("HELLO")
		newValue := []byte("world")
		err = c.CompareAndSwap(ctx, key, wrongOldValue, newValue, time.Second)
		// Then it should return err not exist.
		is.ErrorIs(err, cache.ErrNotExist)

		// And the old value should be the same.
		loaded, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(value, loaded)
	})
}

func TestDelete(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()
		// Given that the key does not exist,
		// When deleting,
		err := c.Delete(ctx, key)
		is := assert.New(t)
		// Then it should return err not exist.
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("exists", func(t *testing.T) {
		key := t.Name()
		// Given that the key exists.
		err := c.Store(ctx, key, []byte("hello"), time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When deleting,
		// Then it should be deleted.
		err = c.Delete(ctx, key)
		is.NoError(err)

		// And it should no longer exists.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.False(exists)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		// Given that the key expired.
		err := c.Store(ctx, key, []byte("hello"), -time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When deleting
		// Then it should invalidate
		// And it should return err not exists
		err = c.Delete(ctx, key)
		is.ErrorIs(err, cache.ErrNotExist)

		// And it should no longer exists.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.False(exists)
	})
}

func TestExists(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()
		// Given that the key does not exists,
		// When checking exists,
		exists, err := c.Exists(ctx, key)

		is := assert.New(t)
		is.NoError(err)
		// Then it should return false.
		is.False(exists)
	})

	t.Run("exists", func(t *testing.T) {
		key := t.Name()
		// Given that the key exists.
		err := c.Store(ctx, key, []byte("hello"), time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When checking exists,
		// Then it should return true.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.True(exists)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		// Given that the key expired.
		err := c.Store(ctx, key, []byte("hello"), -time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When checking exists,
		// Then it should return false.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.False(exists)
	})
}

func TestExpire(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		// Given that the key does not exists.
		key := t.Name()
		// When expiring.
		err := c.Expire(ctx, key, time.Second)
		is := assert.New(t)
		// Then it should return err not exist.
		is.ErrorIs(err, cache.ErrNotExist)
	})

	t.Run("exists", func(t *testing.T) {
		key := t.Name()
		// Given that the key exists.
		err := c.Store(ctx, key, []byte("hello"), time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When expiring.
		err = c.Expire(ctx, key, time.Minute)
		is.NoError(err)

		// Then it should extend the expiry.
		ttl, err := c.TTL(ctx, key)
		is.NoError(err)
		is.True(ttl > 0 && ttl <= time.Minute)
	})

	t.Run("expired", func(t *testing.T) {
		// Given that the key expired.
		key := t.Name()
		err := c.Store(ctx, key, []byte("hello"), -time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When expiring.
		err = c.Expire(ctx, key, time.Second)

		// Then it should return err not exist.
		is.ErrorIs(err, cache.ErrNotExist)
	})
}

func TestLoad(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()

		// Given that the key is does not exist,
		// When loading,
		value, err := c.Load[any](ctx, key)

		is := assert.New(t)
		// Then it should return error not exist.
		is.ErrorIs(err, cache.ErrNotExist)
		is.Empty(value)
	})

	t.Run("exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key exists,
		err := c.Store(ctx, key, value, time.Second)

		is := assert.New(t)
		is.NoError(err)

		// When loading,
		loaded, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		// Then it should load the value.
		is.Equal(value, loaded)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key expired,
		err := c.Store(ctx, key, value, -time.Second)

		is := assert.New(t)
		is.NoError(err)

		// When loading,
		loaded, err := c.Load[[]byte](ctx, key)
		// Then the cache will be invalidated,
		// And it should return not exist.
		is.ErrorIs(err, cache.ErrNotExist)
		is.Empty(loaded)
	})
}

func TestLoadAndDelete(t *testing.T) {

	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		// Given that the key does not exists,
		key := t.Name()

		// When load and delete,
		old, err := c.LoadAndDelete[any](ctx, key)

		is := assert.New(t)
		// It should return error not exist.
		is.ErrorIs(err, cache.ErrNotExist)
		is.Empty(old)
	})

	t.Run("exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key exists.
		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When load and delete,
		old, err := c.LoadAndDelete[[]byte](ctx, key)
		is.NoError(err)

		// Then it should return the old value.
		is.Equal(value, old)

		// And it should delete the key.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.False(exists)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key expired,
		err := c.Store(ctx, key, value, -time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When load and delete,
		old, err := c.LoadAndDelete[[]byte](ctx, key)

		// Then it should return err not exists,
		is.ErrorIs(err, cache.ErrNotExist)
		is.Empty(old)

		// And it should invalidate existing cache.
		exists, err := c.Exists(ctx, key)
		is.NoError(err)
		is.False(exists)
	})
}
func TestLoadOrCreate(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")
		called := false
		// Given that the key does not exists,
		// When load or create,
		curr, loaded, err := c.LoadOrCreate(ctx, key, func(ctx context.Context, key string) ([]byte, time.Duration, error) {
			called = true
			return value, time.Second, nil
		})

		is := assert.New(t)

		is.NoError(err)

		// Then it should not load old value,
		is.False(loaded)
		// And it should create new value.
		is.Equal(value, curr)
		is.True(called)
	})

	t.Run("exists", func(t *testing.T) {
		key := t.Name()
		called := false
		value := []byte("hello")

		// Given that the key exists,
		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		newValue := []byte("world")
		// When load or create,
		curr, loaded, err := c.LoadOrCreate(ctx, key, func(ctx context.Context, key string) ([]byte, time.Duration, error) {
			called = true
			return newValue, time.Second, nil
		})

		// Then it should load old value.
		is.True(loaded)
		is.Equal(value, curr)
		is.False(called)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		called := false
		value := []byte("hello")

		// Given that the key expired,
		err := c.Store(ctx, key, value, -time.Second)
		is := assert.New(t)
		is.NoError(err)

		newValue := []byte("world")
		// When load or create,
		curr, loaded, err := c.LoadOrCreate(ctx, key, func(ctx context.Context, key string) ([]byte, time.Duration, error) {
			called = true
			return newValue, time.Second, nil
		})

		// Then it should create new value.
		is.False(loaded)
		is.Equal(newValue, curr)
		is.True(called)
	})
}

func TestLoadOrStore(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		key := t.Name()
		// Given that the key does not exist
		value := []byte("hello")

		// When load or store
		val1, loaded, err := c.LoadOrStore(ctx, key, value, time.Second)

		is := assert.New(t)
		// Then it should store
		is.NoError(err)
		is.Equal(value, val1)
		is.False(loaded)
	})

	t.Run("exist", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key exists,
		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		newValue := []byte("world")
		// When load or store,
		old, loaded, err := c.LoadOrStore(ctx, key, newValue, time.Second)

		// Then it should load old value
		is.NoError(err)
		is.Equal(value, old)
		is.True(loaded)
	})

	t.Run("expired", func(t *testing.T) {
		key := t.Name()
		value := []byte("hello")

		// Given that the key expired
		err := c.Store(ctx, key, value, -time.Second)

		is := assert.New(t)
		is.NoError(err)

		// When load or store,
		newValue := []byte("world")
		old, loaded, err := c.LoadOrStore(ctx, key, newValue, time.Second)

		// Then it should store new value
		is.NoError(err)
		is.Equal(old, newValue)
		is.False(loaded)
	})
}

func TestStore(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		// Given that the key does not exists,
		key := t.Name()
		value := []byte("hello")

		// When storing,
		err := c.Store(ctx, key, value, time.Second)

		// Then it should succeed,
		is := assert.New(t)
		is.NoError(err)

		// And it should store the value.
		curr, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(value, curr)
	})

	t.Run("exists", func(t *testing.T) {
		// Given that the key exists,
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		newValue := []byte("world")
		// When storing,
		err = c.Store(ctx, key, newValue, time.Second)
		// Then it should succeed,
		is.NoError(err)

		// And it should store the value.
		curr, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(newValue, curr)
	})

	t.Run("expired", func(t *testing.T) {
		// Given that the key expired,
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, -time.Second)
		is := assert.New(t)
		is.NoError(err)

		newValue := []byte("world")
		// When storing,
		err = c.Store(ctx, key, newValue, time.Second)
		// Then it should succeed,
		is.NoError(err)

		// And it should store the value.
		curr, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(newValue, curr)
	})
}

func TestStoreOnce(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		// Given that the key does not exists
		key := t.Name()
		value := []byte("hello")

		// When storing,
		err := c.StoreOnce(ctx, key, value, time.Second)

		// Then it should store the value.
		is := assert.New(t)
		is.NoError(err)
	})

	t.Run("exists", func(t *testing.T) {
		// Given that the key exists
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When storing once
		newValue := []byte("world")
		err = c.StoreOnce(ctx, key, newValue, time.Second)

		// Then it should return error exist
		is.ErrorIs(err, cache.ErrExists)

		// And the old value is unchanged.
		curr, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(value, curr)
	})

	t.Run("expired", func(t *testing.T) {
		// Given that the key expired
		key := t.Name()
		value := []byte("hello")

		err := c.Store(ctx, key, value, -time.Second)

		is := assert.New(t)
		is.NoError(err)

		// When storing
		newValue := []byte("world")
		// Then it should store new value
		err = c.StoreOnce(ctx, key, newValue, time.Second)
		is.NoError(err)

		curr, err := c.Load[[]byte](ctx, key)
		is.NoError(err)
		is.Equal(newValue, curr)
	})
}

func TestTTL(t *testing.T) {
	ctx := t.Context()
	c := cache.New(dbtest.DB(t))

	t.Run("empty", func(t *testing.T) {
		// Given that the key does not exists,
		key := t.Name()
		// When checking ttl,
		ttl, err := c.TTL(ctx, key)

		is := assert.New(t)
		is.NoError(err)
		// It should return -2.
		is.Equal(time.Duration(-2), ttl)
	})

	t.Run("exists", func(t *testing.T) {
		// Given that the key exists,
		key := t.Name()
		err := c.Store(ctx, key, []byte("hello"), 2*time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When checking ttl,
		ttl, err := c.TTL(ctx, key)
		is.NoError(err)

		// Then it should return the ttl.
		is.True(ttl > 0 && ttl <= 2*time.Second)
	})

	t.Run("expired", func(t *testing.T) {
		// Given that the key expired,
		key := t.Name()
		err := c.Store(ctx, key, []byte("hello"), -time.Second)
		is := assert.New(t)
		is.NoError(err)

		// When checking ttl,
		ttl, err := c.TTL(ctx, key)

		// Then it should return -1.
		is.NoError(err)
		is.Equal(time.Duration(-1), ttl)
	})
}
