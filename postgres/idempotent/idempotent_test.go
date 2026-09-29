package idempotent_test

import (
	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

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

func TestCompareAndDelete(t *testing.T) {
	key := t.Name()
	req := Request{
		Name: "foo",
	}

	t.Run("created", func(t *testing.T) {
		res, loaded, err := sut(t, key, req)
		is := assert.New(t)
		is.NoError(err)
		is.False(loaded)
		is.Equal("hi, foo", res.Msg)
	})

	t.Run("loaded", func(t *testing.T) {
		res, loaded, err := sut(t, key, req)
		is := assert.New(t)
		is.NoError(err)
		is.True(loaded)
		is.Equal("hi, foo", res.Msg)
	})

	t.Run("conflict", func(t *testing.T) {
		req.Name = t.Name()
		res, loaded, err := sut(t, key, req)
		is := assert.New(t)
		is.ErrorIs(err, idempotent.ErrRequestConflict)
		is.False(loaded)
		is.Nil(res)
	})
	t.Run("concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		n := 10
		var load, store atomic.Int64
		ch := make(chan struct{})
		for range n {
			wg.Go(func() {
				<-ch
				_, loaded, err := sut(t, "foo", Request{Name: t.Name()})
				if err != nil {
					t.Fatal(err)
				}
				if loaded {
					load.Add(1)
				} else {
					store.Add(1)
				}
			})
		}
		close(ch)
		wg.Wait()
		is := assert.New(t)
		is.Equal(int64(n-1), load.Load())
		is.Equal(int64(1), store.Load())
	})
}

type Request struct {
	Name string
}
type Response struct {
	Msg string
}

func sut(t *testing.T, key string, req Request) (*Response, bool, error) {
	t.Helper()
	ctx := t.Context()
	idb := idempotent.New(idempotent.NewRepository(dbtest.DB(t)))
	idp := idb.Func(func(ctx context.Context, req Request) (*Response, error) {
		return &Response{
			Msg: fmt.Sprintf("hi, %s", req.Name),
		}, nil
	})
	return idp.LoadOrCreate(ctx, key, req)
}
