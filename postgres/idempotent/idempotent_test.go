package idempotent_test

import (
	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/idempotent"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(idempotent.Schema)
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

type Request struct {
	Name string
	ID   int64
	Tags map[string]string
}

type Response struct {
	Msg string
}

func greet(ctx context.Context, req Request) (*Response, error) {
	return &Response{
		Msg: fmt.Sprintf("hi, %s", req.Name),
	}, nil
}

func newIdempotent(t *testing.T) *idempotent.Idempotent {
	t.Helper()
	return idempotent.New(idempotent.NewRepository(dbtest.DB(t)))
}

func TestLoadOrCreate(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		h := newIdempotent(t).Func("greet", greet)
		res, loaded, err := h.LoadOrCreate(t.Context(), t.Name(), Request{Name: "foo"})
		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal("hi, foo", res.Msg)
	})

	t.Run("loaded", func(t *testing.T) {
		var calls atomic.Int64
		h := newIdempotent(t).Func("greet", func(ctx context.Context, req Request) (*Response, error) {
			calls.Add(1)
			return greet(ctx, req)
		})
		req := Request{Name: "foo"}
		_, _, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is := assert.New(t)
		is.NoError(err)

		res, loaded, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is.NoError(err)
		is.True(loaded)
		is.Equal("hi, foo", res.Msg)
		is.Equal(int64(1), calls.Load())
	})

	t.Run("loaded ignores map order", func(t *testing.T) {
		h := newIdempotent(t).Func("greet", greet)
		tags := make(map[string]string)
		for i := range 20 {
			tags[fmt.Sprint(i)] = fmt.Sprint(i)
		}
		req := Request{Name: "foo", Tags: tags}
		_, _, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is := assert.New(t)
		is.NoError(err)

		for range 10 {
			_, loaded, err := h.LoadOrCreate(t.Context(), t.Name(), req)
			is.NoError(err)
			is.True(loaded)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		h := newIdempotent(t).Func("greet", greet)
		_, _, err := h.LoadOrCreate(t.Context(), t.Name(), Request{Name: "foo"})
		is := assert.New(t)
		is.NoError(err)

		res, loaded, err := h.LoadOrCreate(t.Context(), t.Name(), Request{Name: "bar"})
		is.ErrorIs(err, idempotent.ErrRequestConflict)
		is.False(loaded)
		is.Nil(res)
	})

	t.Run("conflict on large integers", func(t *testing.T) {
		// Both values map to the same float64.
		h := newIdempotent(t).Func("greet", greet)
		_, _, err := h.LoadOrCreate(t.Context(), t.Name(), Request{ID: 9007199254740993})
		is := assert.New(t)
		is.NoError(err)

		_, _, err = h.LoadOrCreate(t.Context(), t.Name(), Request{ID: 9007199254740992})
		is.ErrorIs(err, idempotent.ErrRequestConflict)
	})

	t.Run("error is not stored", func(t *testing.T) {
		wantErr := errors.New("bad request")
		var calls atomic.Int64
		h := newIdempotent(t).Func("greet", func(ctx context.Context, req Request) (*Response, error) {
			if calls.Add(1) == 1 {
				return nil, wantErr
			}
			return greet(ctx, req)
		})
		req := Request{Name: "foo"}
		_, _, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is := assert.New(t)
		is.ErrorIs(err, wantErr)

		res, loaded, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is.NoError(err)
		is.False(loaded)
		is.Equal("hi, foo", res.Msg)
		is.Equal(int64(2), calls.Load())
	})

	t.Run("nil response", func(t *testing.T) {
		h := newIdempotent(t).Func("greet", func(ctx context.Context, req Request) (*Response, error) {
			return nil, nil
		})
		req := Request{Name: "foo"}
		_, _, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is := assert.New(t)
		is.NoError(err)

		res, loaded, err := h.LoadOrCreate(t.Context(), t.Name(), req)
		is.NoError(err)
		is.True(loaded)
		is.Nil(res)
	})

	t.Run("concurrent", func(t *testing.T) {
		h := newIdempotent(t).Func("greet", greet)
		key := t.Name()

		n := 10
		var wg sync.WaitGroup
		var load, store atomic.Int64
		errs := make(chan error, n)
		start := make(chan struct{})
		for range n {
			wg.Go(func() {
				<-start
				_, loaded, err := h.LoadOrCreate(t.Context(), key, Request{Name: "foo"})
				if err != nil {
					errs <- err
					return
				}
				if loaded {
					load.Add(1)
				} else {
					store.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)

		is := assert.New(t)
		for err := range errs {
			is.NoError(err)
		}
		is.Equal(int64(n-1), load.Load())
		is.Equal(int64(1), store.Load())
	})

	t.Run("concurrent retry after failure", func(t *testing.T) {
		wantErr := errors.New("bad request")
		var calls atomic.Int64
		inFn := make(chan struct{})
		release := make(chan struct{})
		h := newIdempotent(t).Func("greet", func(ctx context.Context, req Request) (*Response, error) {
			if calls.Add(1) == 1 {
				close(inFn)
				<-release
				return nil, wantErr
			}
			return greet(ctx, req)
		})
		key := t.Name()
		req := Request{Name: "foo"}

		var wg sync.WaitGroup
		var firstErr error
		wg.Go(func() {
			_, _, firstErr = h.LoadOrCreate(t.Context(), key, req)
		})

		// Wait for the first call to hold the key, then start a second call
		// that blocks on it.
		<-inFn
		var res *Response
		var loaded bool
		var secondErr error
		wg.Go(func() {
			res, loaded, secondErr = h.LoadOrCreate(t.Context(), key, req)
		})
		time.Sleep(100 * time.Millisecond)
		close(release)
		wg.Wait()

		is := assert.New(t)
		is.ErrorIs(firstErr, wantErr)
		is.NoError(secondErr)
		is.False(loaded)
		is.Equal("hi, foo", res.Msg)
		is.Equal(int64(2), calls.Load())
	})
}

func TestDelete(t *testing.T) {
	idb := newIdempotent(t)
	h := idb.Func("greet", greet)
	key := t.Name()
	_, _, err := h.LoadOrCreate(t.Context(), key, Request{Name: "foo"})
	is := assert.New(t)
	is.NoError(err)

	row, err := idb.Delete(t.Context(), "greet", key)
	is.NoError(err)
	is.Equal("greet", row.Scope)
	is.Equal(key, row.Key)
	is.JSONEq(`{"Msg": "hi, foo"}`, string(row.Response))

	_, err = idb.Delete(t.Context(), "greet", key)
	is.ErrorIs(err, idempotent.ErrNotFound)

	// The key can be reused after it is deleted.
	_, loaded, err := h.LoadOrCreate(t.Context(), key, Request{Name: "bar"})
	is.NoError(err)
	is.False(loaded)
}

func TestScope(t *testing.T) {
	idb := newIdempotent(t)
	greeter := idb.Func("greet", greet)
	echoer := idb.Func("echo", func(ctx context.Context, req Request) (*Response, error) {
		return &Response{Msg: req.Name}, nil
	})
	key := t.Name()

	is := assert.New(t)
	res, loaded, err := greeter.LoadOrCreate(t.Context(), key, Request{Name: "foo"})
	is.NoError(err)
	is.False(loaded)
	is.Equal("hi, foo", res.Msg)

	// Same key, different scope and request: no conflict.
	res, loaded, err = echoer.LoadOrCreate(t.Context(), key, Request{Name: "bar"})
	is.NoError(err)
	is.False(loaded)
	is.Equal("bar", res.Msg)

	// Deleting in one scope does not affect the other.
	_, err = idb.Delete(t.Context(), "echo", key)
	is.NoError(err)
	res, loaded, err = greeter.LoadOrCreate(t.Context(), key, Request{Name: "foo"})
	is.NoError(err)
	is.True(loaded)
	is.Equal("hi, foo", res.Msg)
}

func TestEmptyScope(t *testing.T) {
	assert.Panics(t, func() {
		newIdempotent(t).Func("", greet)
	})
}

func TestDeleteBefore(t *testing.T) {
	idb := newIdempotent(t)
	h := idb.Func("greet", greet)
	key := t.Name()
	_, _, err := h.LoadOrCreate(t.Context(), key, Request{Name: "foo"})
	is := assert.New(t)
	is.NoError(err)

	n, err := idb.DeleteBefore(t.Context(), time.Now().Add(time.Hour))
	is.NoError(err)
	is.GreaterOrEqual(n, int64(1))

	_, err = idb.Delete(t.Context(), "greet", key)
	is.ErrorIs(err, idempotent.ErrNotFound)
}
