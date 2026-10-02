package lock_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/alextanhongpin/dbtx/postgres/lock"
	"github.com/stretchr/testify/assert"
)

func ExampleHash32() {
	fmt.Println(lock.Hash32("hello world"))

	// Output:
	// -712294489
}

func ExampleHash64() {
	fmt.Println(lock.Hash64("hello world"))

	// Output:
	// 8618312879776256743
}

func ExamplePair() {
	fmt.Println(fmt.Errorf("Key(%v)", lock.Pair[string]{"Foo", "Bar"}))

	// Output:
	// Key(Foo, Bar)
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
