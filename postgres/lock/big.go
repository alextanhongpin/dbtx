package lock

import (
	"database/sql/driver"
	"fmt"
	"math/big"
)

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
