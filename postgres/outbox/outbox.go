package outbox

import (
	_ "embed"

	_ "github.com/lib/pq"

	"context"
	"errors"

	"github.com/alextanhongpin/dbtx"
)

var (
	//go:embed queries/count.sql
	count string

	//go:embed queries/create.sql
	create string

	//go:embed queries/delete.sql
	delete string

	//go:embed queries/list.sql
	list string
)

var ErrNotInTx = errors.New("outbox: not in transaction")

type Outbox struct {
	dbtx.UnitOfWork
}

func New(uow dbtx.UnitOfWork) *Outbox {
	return &Outbox{
		UnitOfWork: uow,
	}
}

func (o *Outbox) Create(ctx context.Context, message ...*Message) error {
	_, err := o.DBTx(ctx).ExecContext(ctx, create, MessageList(message))
	return err
}

func (o *Outbox) Count(ctx context.Context) (int64, error) {
	var n int64
	err := o.DBTx(ctx).QueryRowContext(ctx, count).Scan(&n)
	return n, err
}

func (o *Outbox) List(ctx context.Context) ([]*Message, error) {
	var res MessageList
	err := o.DBTx(ctx).QueryRowContext(ctx, list).Scan(&res)
	return res, err
}

func (o *Outbox) LoadAndDelete(ctx context.Context) (*Message, error) {
	var res Message
	err := o.DBTx(ctx).QueryRowContext(ctx, delete).Scan(&res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
