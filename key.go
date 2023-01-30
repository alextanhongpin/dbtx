package uow

import (
	"database/sql/driver"
	"fmt"
	"math/big"
)

type LockKey interface {
	fmt.Stringer
	isLockKey()
}

type bigIntLockKey struct {
	b *BigInt
}

func BigIntLockKey(n *big.Int) *bigIntLockKey {
	return &bigIntLockKey{&BigInt{n: n}}
}

func (*bigIntLockKey) isLockKey() {}
func (key *bigIntLockKey) String() string {
	return fmt.Sprintf("LockKey(%s)", key.b.n.String())
}

type intLockKey struct {
	m, n int
}

func IntLockKey(m, n int) *intLockKey {
	return &intLockKey{m, n}
}

func (*intLockKey) isLockKey() {}
func (key *intLockKey) String() string {
	return fmt.Sprintf("LockKey(%d, %d)", key.m, key.n)
}

type BigInt struct {
	n *big.Int
}

func (b *BigInt) Value() (driver.Value, error) {
	if b != nil {
		return b.n.String(), nil
	}
	return nil, nil
}

func (b *BigInt) Scan(value interface{}) error {
	if value == nil {
		b = nil
	}

	switch t := value.(type) {
	case []uint8:
		_, ok := b.n.SetString(string(value.([]uint8)), 10)
		if !ok {
			return fmt.Errorf("failed to load value to []uint8: %v", value)
		}
	default:
		return fmt.Errorf("Could not scan type %T into BigInt", t)
	}

	return nil
}
