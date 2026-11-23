package keys_test

import (
	"context"
	"encoding/json"

	"github.com/alextanhongpin/dbtx/postgres/durable/keys"
)

// A request in two steps that commit separately. If the second step fails,
// the retry with the same key starts at "reserved" and the reservation is
// not made twice.
func ExampleKeys_DoSteps() {
	var k *keys.Keys // from keys.New
	req := keys.Request{Scope: "t1:orders.place", Key: "order-42", Body: json.RawMessage(`{"sku": "a"}`)}
	_, _ = k.DoSteps(context.Background(), req, func(txCtx context.Context, cp keys.Checkpoint) (keys.Result, error) {
		switch cp.Name {
		case "": // reserve stock through db.DBTx(txCtx)
			return keys.Result{Checkpoint: &keys.Checkpoint{Name: "reserved", Data: json.RawMessage(`{"reservation": 7}`)}}, nil
		default: // "reserved": create the order with cp.Data's reservation
			return keys.Result{Response: &keys.Response{Status: 201, Body: cp.Data}}, nil
		}
	})
}
