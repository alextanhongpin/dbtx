package dbtx_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx"
	"github.com/stretchr/testify/assert"
)

func TestContext(t *testing.T) {

	t.Run("isolation", func(t *testing.T) {
		for _, lvl := range []sql.IsolationLevel{
			sql.LevelDefault,
			sql.LevelReadUncommitted,
			sql.LevelReadCommitted,
			sql.LevelWriteCommitted,
			sql.LevelRepeatableRead,
			sql.LevelSnapshot,
			sql.LevelSerializable,
			sql.LevelLinearizable,
		} {
			ctx := context.Background()
			ctx = dbtx.IsolationLevel(ctx, lvl)
			opt := dbtx.TxOptions(ctx)

			assert.Equal(t, lvl, opt.Isolation)
		}
	})

	t.Run("readonly", func(t *testing.T) {
		ctx := context.Background()
		is := assert.New(t)

		opt := dbtx.TxOptions(ctx)
		is.False(opt.ReadOnly)

		opt = dbtx.TxOptions(dbtx.ReadOnly(ctx, true))
		is.True(opt.ReadOnly)
	})

	t.Run("tx", func(t *testing.T) {
		ctx := context.Background()
		assert.False(t, dbtx.IsTx(ctx))
	})
}
