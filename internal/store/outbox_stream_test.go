package store_test

import (
	"bytes"
	"testing"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/store"
)

func TestOutboxConsumerHandsOffUnchangedEnvelopeBeforeSaving(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	from := addr.Local("worker")
	to := addr.Address{Machine: "host", Agent: "manager", Upstream: true}
	env, err := envelope.New(from, to, "message", "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendOutbox(env); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	end, err := st.WriteOutboxBatch(&wire, 0, "host", true, true)
	if err != nil {
		t.Fatal(err)
	}
	var cursor, saved int64
	delivered := false
	consumer := store.OutboxConsumer{
		Cursor: &cursor,
		Deliver: func(got envelope.Envelope) error {
			if got.ID != env.ID || got.From != from || got.To != to || got.Body != env.Body || cursor != 0 || saved != 0 {
				t.Errorf("delivery = %+v, cursor=%d saved=%d", got, cursor, saved)
			}
			delivered = true
			return nil
		},
		SaveCursor: func(n int64) error {
			if !delivered {
				t.Error("cursor saved before delivery")
			}
			saved = n
			return nil
		},
		CaughtUp: func() (bool, error) {
			if cursor != end || saved != end {
				t.Errorf("watermark cursor=%d saved=%d, want %d", cursor, saved, end)
			}
			return true, nil
		},
	}
	idle, progressed, err := consumer.Consume(&wire)
	if err != nil || !delivered || !idle || !progressed || cursor != end || saved != end {
		t.Fatalf("delivered=%v idle=%v progressed=%v cursor=%d saved=%d end=%d err=%v", delivered, idle, progressed, cursor, saved, end, err)
	}
}
