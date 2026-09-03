package lock

import (
	"hash/fnv"
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

func Hash32(key string) int32 {
	hash := fnv.New32a()
	_, err := hash.Write([]byte(key))
	if err != nil {
		panic(err)
	}

	// Will overflow, but still in range.
	return int32(hash.Sum32())
}

func Hash64(key string) int64 {
	hash := fnv.New64a()
	_, err := hash.Write([]byte(key))
	if err != nil {
		panic(err)
	}

	// Will overflow, but still in range.
	return int64(hash.Sum64())
}
