package lock_test

import (
	"math"
	"testing"

	"github.com/alextanhongpin/dbtx/postgres/lock"
	"github.com/stretchr/testify/assert"
)

func TestKey(t *testing.T) {
	assert := assert.New(t)

	assert.Equal("Key(42)", lock.NewIntKey(42).String())
	assert.Equal("Key(2, 21)", lock.NewIntKeyPair(2, 21).String())
	assert.Equal(`Key("hello world")`, lock.NewStrKey("hello world").String())
	assert.Equal(`Key("foo", "bar")`, lock.NewStrKeyPair("foo", "bar").String())
}

func TestUint32ToInt32(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(int32(math.MaxInt32), lock.Uint32ToInt32(math.MaxUint32))
	assert.Equal(int32(math.MinInt32), lock.Uint32ToInt32(0))
}

func TestUint64ToInt64(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(int64(math.MaxInt64), lock.Uint64ToInt64(math.MaxUint64))
	assert.Equal(int64(math.MinInt64), lock.Uint64ToInt64(0))
}
