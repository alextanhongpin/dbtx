package lock

import (
	"fmt"
	"hash/fnv"
	"math"
)

// https://www.postgresql.org/docs/current/datatype-numeric.html
// Go's equivalent of Postgres's integer and bigint
// integer -> int32
// bigint -> int64
//
// int32  : -2147483648 to 2147483647
// int64  : -9223372036854775808 to 9223372036854775807
//
// The advisory lock only accept pair integer, or single bigint.
// https://www.postgresql.org/docs/current/functions-admin.html#FUNCTIONS-ADVISORY-LOCKS
//
// uint8  : 0 to 255
// uint16 : 0 to 65535
// uint32 : 0 to 4294967295
// uint64 : 0 to 18446744073709551615
// int8   : -128 to 127
// int16  : -32768 to 32767
// int32  : -2147483648 to 2147483647
// int64  : -9223372036854775808 to 9223372036854775807

type Key struct {
	x, y int32
	z    int64
	pair bool
	repr string
}

func NewIntKey(z int64) *Key {
	return &Key{
		z:    z,
		repr: fmt.Sprintf("Key(%d)", z),
	}
}

func NewIntKeyPair(x, y int32) *Key {
	return &Key{
		x:    x,
		y:    y,
		pair: true,
		repr: fmt.Sprintf("Key(%d, %d)", x, y),
	}
}

func (k *Key) String() string {
	return k.repr
}

func NewStrKey(z string) *Key {
	return &Key{
		z:    IntHash64(z),
		repr: fmt.Sprintf("Key(%q)", z),
	}
}

func NewStrKeyPair(x, y string) *Key {
	return &Key{
		x:    IntHash32(x),
		y:    IntHash32(y),
		pair: true,
		repr: fmt.Sprintf("Key(%q, %q)", x, y),
	}
}

func Hash32(key string) uint32 {
	hash := fnv.New32()
	_, err := hash.Write([]byte(key))
	if err != nil {
		panic(err)
	}

	return hash.Sum32()
}

func Hash64(key string) uint64 {
	hash := fnv.New64()
	_, err := hash.Write([]byte(key))
	if err != nil {
		panic(err)
	}

	return hash.Sum64()
}

func IntHash32(key string) int32 {
	return Uint32ToInt32(Hash32(key))
}

func IntHash64(key string) int64 {
	return Uint64ToInt64(Hash64(key))
}

func Uint64ToInt64(u64 uint64) int64 {
	if u64 > uint64(math.MaxInt64) {
		return int64(u64 - uint64(math.MaxInt64) - 1)
	}

	return int64(u64) - math.MaxInt64 - 1
}

func Uint32ToInt32(u32 uint32) int32 {
	if u32 > uint32(math.MaxInt32) {
		return int32(u32 - uint32(math.MaxInt32) - 1)
	}

	return int32(u32) - math.MaxInt32 - 1
}
