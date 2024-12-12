package lock_test

import (
	"math"
	"testing"

	"github.com/alextanhongpin/dbtx/postgres/lock"
	"github.com/stretchr/testify/assert"
)

func TestKey(t *testing.T) {
	is := assert.New(t)

	is.Equal("Key(42)", lock.NewIntKey(42).String())
	is.Equal("Key(2, 21)", lock.NewIntKeyPair(2, 21).String())
	is.Equal(`Key("hello world"|9065573210506989167)`, lock.NewStrKey("hello world").String())
	is.Equal(`Key("foo"|1083137555, "bar"|513390112)`, lock.NewStrKeyPair("foo", "bar").String())
}

func TestUint32ToInt32_Overflow(t *testing.T) {
	i := uint32(math.MaxUint32)
	is := assert.New(t)
	is.Equal(int32(-1), int32(i))

	i = uint32(math.MaxInt32)
	is.Equal(int32(2147483647), int32(i))

	i = uint32(math.MaxInt32) + 1
	is.Equal(int32(-2147483648), int32(i))
}

func TestUint64ToInt64_Overflow(t *testing.T) {
	i := uint64(math.MaxUint64)
	is := assert.New(t)
	is.Equal(int64(-1), int64(i))

	i = uint64(math.MaxInt64)
	is.Equal(int64(9223372036854775807), int64(i))

	i = uint64(math.MaxInt64) + 1
	is.Equal(int64(-9223372036854775808), int64(i))
}
