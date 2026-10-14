package durable_test

// These examples compile with the package and show the main scenarios from
// docs/durable-scenarios.md. They need a database, so they have no Output
// comment and are not run by go test.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/cache"
	"github.com/alextanhongpin/dbtx/postgres/durable/inbox"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
	"github.com/alextanhongpin/dbtx/postgres/durable/keys"
	"github.com/alextanhongpin/dbtx/postgres/durable/lease"
	"github.com/alextanhongpin/dbtx/postgres/durable/outbox"
	"github.com/alextanhongpin/dbtx/postgres/durable/schedule"
)

var db *dbtx.DB // dbtx.New(sqlDB), shared with your repositories

// S5: charge through an external API under a lease, then record the result
// in a fenced transaction.
func Example_leaseActivity() {
	ctx := context.Background()
	locker, _ := lease.New(lease.Config{DB: db, Namespace: "billing"})
	_ = locker.Activity(ctx, "order:123", func(ctx context.Context, held lease.Lease) error {
		receipt := "ch_1" // call the payment API with key "charge:order:123"
		return held.RunInFencedTx(ctx, func(txCtx context.Context) error {
			_, err := db.DBTx(txCtx).ExecContext(txCtx,
				`UPDATE orders SET payment_id = $2 WHERE id = $1`, "123", receipt)
			return err
		})
	})
}

// S6: an HTTP endpoint with an Idempotency-Key header.
func Example_keys() {
	k, _ := keys.New(keys.Config{DB: db})
	http.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
		resp, err := k.Do(r.Context(), keys.Request{
			Scope: "tenant-1:payments.create",
			Key:   r.Header.Get("Idempotency-Key"),
			Body:  json.RawMessage(`{"amount": 100}`), // the request body
		}, func(txCtx context.Context) (keys.Response, error) {
			// Business writes through db.DBTx(txCtx) commit with the response.
			return keys.Response{Status: http.StatusCreated, Body: json.RawMessage(`{"id": 1}`)}, nil
		})
		switch {
		case errors.Is(err, durable.ErrRequestMismatch):
			w.WriteHeader(http.StatusUnprocessableEntity)
		case errors.Is(err, durable.ErrInFlight):
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(resp.Status)
			_, _ = w.Write(resp.Body)
		}
	})
}

type chargeReq struct{ Amount int }
type chargeResp struct{ ID string }

// S12 + S14: submit a job with the order, then charge through an idempotent
// remote API in a step.
func Example_jobs() {
	ctx := context.Background()
	client, _ := jobs.NewClient(jobs.Config{DB: db, Kinds: []string{"order.fulfil"}})
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		// ... insert the order through db.DBTx(txCtx) ...
		_, err := client.Submit(txCtx, jobs.SubmitRequest{Kind: "order.fulfil", Key: "order:123",
			Payload: map[string]string{"order": "123"}})
		return err
	})

	worker, _ := jobs.NewWorker(jobs.WorkerConfig{DB: db})
	_ = worker.Register("order.fulfil", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		charge, err := x.RunStep(ctx, "charge",
			func(context.Context) (chargeReq, error) { return chargeReq{Amount: 100}, nil },
			func(ctx context.Context, idempotencyKey string, r chargeReq) (chargeResp, error) {
				return chargeResp{ID: "ch_1"}, nil // the payment API call
			})
		if err != nil {
			return jobs.Result{}, err
		}
		return jobs.Done(charge), nil
	}, jobs.KindOptions{Mode: jobs.ModeActivity, Timeout: time.Minute})
	_ = worker.Run(ctx)
}

// S20 + S26: ordered events from one service, applied in order by another.
func Example_outboxInbox() {
	ctx := context.Background()
	ob, _ := outbox.New(outbox.Config{DB: db, Topics: map[string]outbox.TopicConfig{
		"order.events": {Ordered: true, OnHeadFailure: outbox.HeadBlock},
	}})
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		_, err := ob.Publish(txCtx, outbox.Event{Topic: "order.events", Type: "order.paid",
			AggregateKey: "order:123", Payload: map[string]string{"id": "123"}})
		return err
	})
	relay, _ := outbox.NewRelay(ob, outbox.RelayConfig{Publisher: outbox.PublisherFunc(
		func(ctx context.Context, msgs []outbox.Message) (int, error) {
			return len(msgs), nil // send to Kafka with key = AggregateKey, wait for acks
		})})
	go func() { _ = relay.Run(ctx) }()

	in, _ := inbox.New(inbox.Config{DB: db})
	var m outbox.Message // received from the broker
	_, _ = in.Receive(ctx, inbox.ModeSequenced, inbox.Message{
		Source: "orders-service", MessageID: m.ID, AggregateKey: m.AggregateKey,
		Seq: m.Seq, Type: m.Type, Payload: m.Payload, IsGap: m.IsGap,
	}, func(txCtx context.Context, msg inbox.Message) error {
		return nil // update the projection through db.DBTx(txCtx)
	})
}

// S30: a nightly job in Singapore time, safe with many scheduler instances.
func Example_schedule() {
	ctx := context.Background()
	client, _ := jobs.NewClient(jobs.Config{DB: db, Kinds: []string{"report.royalties"}})
	s, _ := schedule.New(schedule.Config{DB: db, Jobs: client})
	_ = s.Register(ctx, schedule.Schedule{Name: "royalties.nightly", Spec: "0 2 * * *",
		TimeZone: "Asia/Singapore", Kind: "report.royalties"})
	_ = s.Run(ctx)
}

// S31: an expensive chart, computed once while others get the stale copy.
func Example_cache() {
	ctx := context.Background()
	c, _ := cache.New(cache.Config{DB: db, Prefix: "charts:"})
	_, _ = c.GetOrCompute(ctx, "top50:sg", cache.Options{TTL: 5 * time.Minute, StaleFor: time.Minute},
		func(ctx context.Context) ([]string, error) { return []string{"track-1"}, nil })
}
