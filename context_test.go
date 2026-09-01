package dbtx_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

func TestContext(t *testing.T) {
	t.Run("tx options", func(t *testing.T) {
		for _, iso := range []sql.IsolationLevel{
			sql.LevelDefault,
			sql.LevelReadUncommitted,
			sql.LevelReadCommitted,
			sql.LevelWriteCommitted,
			sql.LevelRepeatableRead,
			sql.LevelSnapshot,
			sql.LevelSerializable,
			sql.LevelLinearizable,
		} {
			for _, readOnly := range []bool{false, true} {
				want := &sql.TxOptions{
					Isolation: iso,
					ReadOnly:  readOnly,
				}
				ctx := dbtx.WithTxOptions(context.Background(), want)
				got := dbtx.TxOptions(ctx)
				assert.Equal(t, want, got)
			}
		}
	})
}

func TestContextNamed(t *testing.T) {
	a := dbtx.New(dbtest.DB(t))
	a.SetID("a")
	b := dbtx.New(dbtest.DB(t))
	b.SetID("b")

	is := assert.New(t)

	err := a.RunInTx(t.Context(), func(aCtx context.Context) error {
		b.RunInTx(aCtx, func(bCtx context.Context) error {
			is.True(dbtx.IsNamedTx(bCtx, "a"))
			is.True(dbtx.IsNamedTx(bCtx, "b"))
			return nil
		})

		is.True(dbtx.IsNamedTx(aCtx, "a"))
		is.False(dbtx.IsNamedTx(aCtx, "b"))
		return nil
	})
	is.NoError(err)
}
