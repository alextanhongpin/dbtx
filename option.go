package dbtx

type Option interface {
	isOption()
}

type Middleware func(DBTX) DBTX

func (Middleware) isOption() {}
