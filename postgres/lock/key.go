package lock

import (
	"fmt"
	"hash/fnv"
)

type Key struct {
	x, y uint32
	z    uint64
	pair bool
	repr string
}

func NewIntKey(z uint64) *Key {
	return &Key{
		z:    z,
		repr: fmt.Sprintf("Key(%d)", z),
	}
}

func NewIntKeyPair(x, y uint32) *Key {
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
		z:    Hash64(z),
		repr: fmt.Sprintf("Key(%q)", z),
	}
}

func NewStrKeyPair(x, y string) *Key {
	return &Key{
		x:    Hash32(x),
		y:    Hash32(y),
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
