package dbtx_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx"
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
