package lock_test

import (
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
