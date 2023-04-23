package dbtx

import "database/sql"

type AtomicOption struct {
	Tx *sql.TxOptions
}

type Option func(o *AtomicOption)

func TxOptions(tx *sql.TxOptions) Option {
	return func(o *AtomicOption) {
		o.Tx = tx
	}
}
