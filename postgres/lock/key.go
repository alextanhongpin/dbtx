package lock

import (
	"fmt"
	"math/big"
)

type Key interface {
	fmt.Stringer
	isKey()
}

type bigIntKey struct {
	b *BigInt
}

func BigIntKey(n *big.Int) *bigIntKey {
	return &bigIntKey{&BigInt{n: n}}
}

func (*bigIntKey) isKey() {}
func (key *bigIntKey) String() string {
	return fmt.Sprintf("Key(%s)", key.b.n.String())
}

type intKey struct {
	m, n int
}

func IntKey(m, n int) *intKey {
	return &intKey{m, n}
}

func (*intKey) isKey() {}
func (key *intKey) String() string {
	return fmt.Sprintf("Key(%d, %d)", key.m, key.n)
}
