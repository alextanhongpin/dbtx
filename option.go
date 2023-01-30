package uow

import "database/sql"

type UowOption struct {
	Tx *sql.TxOptions
}

type Option func(o *UowOption)

func TxOptions(tx *sql.TxOptions) Option {
	return func(o *UowOption) {
		o.Tx = tx
	}
}
