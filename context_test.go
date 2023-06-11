package dbtx_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx"
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

			if want, got := lvl, opt.Isolation; want != got {
				t.Fatalf("want %d, got %d", want, got)
			}
		}
	})

	t.Run("readonly", func(t *testing.T) {
		ctx := context.Background()
		opt := dbtx.TxOptions(ctx)

		if want, got := false, opt.ReadOnly; want != got {
			t.Fatalf("want %t, got %t", want, got)
		}

		ctx = dbtx.ReadOnly(ctx, true)
		opt = dbtx.TxOptions(ctx)

		if want, got := true, opt.ReadOnly; want != got {
			t.Fatalf("want %t, got %t", want, got)
		}
	})

	t.Run("istx", func(t *testing.T) {
		ctx := context.Background()
		isTx := dbtx.IsTx(ctx)

		if want, got := false, isTx; want != got {
			t.Fatalf("want %t, got %t", want, got)
		}
	})
}
