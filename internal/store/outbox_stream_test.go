package store_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/store"
)

func streamStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func appendStreamMessage(t *testing.T, st *store.Store, host, body string) envelope.Envelope {
	t.Helper()
	to, err := addr.Parse("recado+upstream://" + host + "/manager")
	if err != nil {
		t.Fatal(err)
	}
	env, err := envelope.New(addr.Local("worker"), to, "message", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendOutbox(env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestOutboxStreamWireFormat(t *testing.T) {
	remote := streamStore(t)
	const line = `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","ts":"2024-01-02T03:04:05.000Z","from":"recado://local/worker","to":"recado+upstream://host/manager","kind":"message","body":"hello <world>","reply_to":"","meta":{}}` + "\n"
	if err := os.WriteFile(filepath.Join(remote.Root(), "outbox.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	const encoded = `{"v":1,"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","ts":"2024-01-02T03:04:05.000Z","from":"recado://local/worker","to":"recado+upstream://host/manager","kind":"message","body":"hello \u003cworld\u003e","reply_to":"","meta":{}}`
	for _, tc := range []struct {
		name            string
		json, watermark bool
		want            string
	}{
		{"follow", true, true, `{"offset":207,"envelope":` + encoded + "}\n" + `{"offset":207,"caught_up":true}` + "\n"},
		{"one-shot", true, false, `{"offset":207,"envelope":` + encoded + "}\n"},
		{"plain", false, true, encoded + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			end, err := remote.WriteOutboxBatch(&wire, 0, "host", tc.json, tc.watermark)
			if err != nil || end != 207 || wire.String() != tc.want {
				t.Fatalf("end=%d wire=%q want=%q err=%v", end, wire.String(), tc.want, err)
			}
		})
	}
}

func TestOutboxStreamSkipsOtherHostsAndAdvancesWatermark(t *testing.T) {
	remote := streamStore(t)
	wanted := appendStreamMessage(t, remote, "host", "wanted")
	appendStreamMessage(t, remote, "other", "skipped")
	info, err := os.Stat(filepath.Join(remote.Root(), "outbox.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	end, err := remote.WriteOutboxBatch(&wire, 0, "host", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "skipped") {
		t.Fatalf("other host in stream: %s", wire.String())
	}
	local := streamStore(t)
	if err := local.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	svc := &deliver.Service{Store: local}
	var cursor, saved int64
	consumer := store.OutboxConsumer{
		Machine: "rpi", Host: "host", Cursor: &cursor,
		Deliver:    func(env envelope.Envelope) error { _, err := svc.Deliver(env); return err },
		SaveCursor: func(n int64) error { saved = n; return nil },
		CaughtUp:   func() (bool, error) { return true, nil },
	}
	idle, progressed, err := consumer.Consume(&wire)
	if err != nil || !idle || !progressed || end != info.Size() || cursor != info.Size() || saved != info.Size() {
		t.Fatalf("idle=%v progressed=%v end=%d cursor=%d saved=%d size=%d err=%v", idle, progressed, end, cursor, saved, info.Size(), err)
	}
	messages, err := local.Messages("manager")
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	got := messages[0].Envelope
	if got.ID != wanted.ID || got.Body != "wanted" || got.From.String() != "recado://rpi/worker" || got.To.String() != "recado://local/manager" {
		t.Fatalf("delivered envelope = %+v", got)
	}
}

func TestOutboxStreamReplaysAfterSavedCursor(t *testing.T) {
	remote := streamStore(t)
	first := appendStreamMessage(t, remote, "host", "first")
	local := streamStore(t)
	if err := local.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	svc := &deliver.Service{Store: local}
	var cursor, saved int64
	consumer := store.OutboxConsumer{
		Machine: "rpi", Host: "host", Cursor: &cursor,
		Deliver:    func(env envelope.Envelope) error { _, err := svc.Deliver(env); return err },
		SaveCursor: func(n int64) error { saved = n; return nil },
		CaughtUp:   func() (bool, error) { return true, nil },
	}
	var wire bytes.Buffer
	firstEnd, err := remote.WriteOutboxBatch(&wire, 0, "host", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if idle, _, err := consumer.Consume(&wire); err != nil || !idle || saved != firstEnd {
		t.Fatalf("first pull idle=%v saved=%d end=%d err=%v", idle, saved, firstEnd, err)
	}
	second := appendStreamMessage(t, remote, "host", "second")
	cursor = saved // Restart from the persisted cursor.
	end, err := remote.WriteOutboxBatch(&wire, cursor, "host", true, true)
	if err != nil || strings.Contains(wire.String(), first.ID) || !strings.Contains(wire.String(), second.ID) {
		t.Fatalf("replay wire=%s err=%v", wire.String(), err)
	}
	idle, progressed, err := consumer.Consume(&wire)
	messages, messagesErr := local.Messages("manager")
	if err != nil || !idle || !progressed || cursor != end || saved != end || messagesErr != nil || len(messages) != 2 || messages[0].Envelope.ID != first.ID || messages[1].Envelope.ID != second.ID {
		t.Fatalf("replay idle=%v progressed=%v cursor=%d saved=%d end=%d messages=%+v err=%v messagesErr=%v", idle, progressed, cursor, saved, end, messages, err, messagesErr)
	}
}

func TestOutboxStreamDoesNotAdvancePastFailedDelivery(t *testing.T) {
	remote := streamStore(t)
	first := appendStreamMessage(t, remote, "host", "first")
	info, err := os.Stat(filepath.Join(remote.Root(), "outbox.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	to, err := addr.Parse("recado+upstream://host/missing")
	if err != nil {
		t.Fatal(err)
	}
	second, err := envelope.New(addr.Local("worker"), to, "message", "retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.AppendOutbox(second); err != nil {
		t.Fatal(err)
	}
	appendStreamMessage(t, remote, "other", "skipped")
	var wire bytes.Buffer
	end, err := remote.WriteOutboxBatch(&wire, 0, "host", true, true)
	if err != nil {
		t.Fatal(err)
	}
	local := streamStore(t)
	if err := local.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	svc := &deliver.Service{Store: local}
	var cursor, saved int64
	consumer := store.OutboxConsumer{
		Machine: "rpi", Host: "host", Cursor: &cursor,
		Deliver:    func(env envelope.Envelope) error { _, err := svc.Deliver(env); return err },
		SaveCursor: func(n int64) error { saved = n; return nil },
		CaughtUp:   func() (bool, error) { return true, nil },
	}
	idle, progressed, err := consumer.Consume(&wire)
	if !errors.Is(err, store.ErrUnknownAgent) || idle || !progressed || cursor != info.Size() || saved != info.Size() {
		t.Fatalf("idle=%v progressed=%v cursor=%d saved=%d first end=%d err=%v", idle, progressed, cursor, saved, info.Size(), err)
	}
	messages, err := local.Messages("manager")
	if err != nil || len(messages) != 1 || messages[0].Envelope.ID != first.ID {
		t.Fatalf("first delivery messages=%+v err=%v", messages, err)
	}
	if err := local.Register(store.Agent{ID: "missing"}); err != nil {
		t.Fatal(err)
	}
	wire.Reset()
	if _, err := remote.WriteOutboxBatch(&wire, saved, "host", true, true); err != nil {
		t.Fatal(err)
	}
	idle, progressed, err = consumer.Consume(&wire)
	messages, messagesErr := local.Messages("missing")
	if err != nil || !idle || !progressed || cursor != end || saved != end || messagesErr != nil || len(messages) != 1 || messages[0].Envelope.ID != second.ID {
		t.Fatalf("retry idle=%v progressed=%v cursor=%d saved=%d end=%d messages=%+v err=%v messagesErr=%v", idle, progressed, cursor, saved, end, messages, err, messagesErr)
	}
}

func TestOutboxStreamRetriesAfterCursorSaveFailure(t *testing.T) {
	remote := streamStore(t)
	wanted := appendStreamMessage(t, remote, "host", "retry")
	local := streamStore(t)
	if err := local.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	svc := &deliver.Service{Store: local}
	var cursor, saved int64
	saveErr := errors.New("cursor write failed")
	failSave := true
	consumer := store.OutboxConsumer{
		Machine: "rpi", Host: "host", Cursor: &cursor,
		Deliver: func(env envelope.Envelope) error { _, err := svc.Deliver(env); return err },
		SaveCursor: func(n int64) error {
			if failSave {
				return saveErr
			}
			saved = n
			return nil
		},
		CaughtUp: func() (bool, error) { return true, nil },
	}
	var wire bytes.Buffer
	end, err := remote.WriteOutboxBatch(&wire, 0, "host", true, true)
	if err != nil {
		t.Fatal(err)
	}
	idle, progressed, err := consumer.Consume(&wire)
	if !errors.Is(err, saveErr) || idle || progressed || cursor != 0 || saved != 0 {
		t.Fatalf("idle=%v progressed=%v cursor=%d saved=%d err=%v", idle, progressed, cursor, saved, err)
	}
	failSave = false
	wire.Reset()
	if _, err := remote.WriteOutboxBatch(&wire, saved, "host", true, true); err != nil {
		t.Fatal(err)
	}
	idle, progressed, err = consumer.Consume(&wire)
	messages, messagesErr := local.Messages("manager")
	if err != nil || !idle || !progressed || cursor != end || saved != end || messagesErr != nil || len(messages) != 1 || messages[0].Envelope.ID != wanted.ID {
		t.Fatalf("retry idle=%v progressed=%v cursor=%d saved=%d end=%d messages=%+v err=%v messagesErr=%v", idle, progressed, cursor, saved, end, messages, err, messagesErr)
	}
}

func TestOutboxStreamIgnoresPartialTrailingLine(t *testing.T) {
	remote := streamStore(t)
	wanted := appendStreamMessage(t, remote, "host", "complete")
	path := filepath.Join(remote.Root(), "outbox.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"body":"partial`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	end, err := remote.WriteOutboxBatch(&wire, 0, "host", true, true)
	if err != nil || end != info.Size() || strings.Contains(wire.String(), "partial") {
		t.Fatalf("end=%d complete=%d wire=%s err=%v", end, info.Size(), wire.String(), err)
	}
	local := streamStore(t)
	if err := local.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	svc := &deliver.Service{Store: local}
	var cursor int64
	consumer := store.OutboxConsumer{
		Machine: "rpi", Host: "host", Cursor: &cursor,
		Deliver:    func(env envelope.Envelope) error { _, err := svc.Deliver(env); return err },
		SaveCursor: func(int64) error { return nil },
		CaughtUp:   func() (bool, error) { return true, nil },
	}
	idle, _, err := consumer.Consume(&wire)
	messages, messagesErr := local.Messages("manager")
	if err != nil || !idle || cursor != info.Size() || messagesErr != nil || len(messages) != 1 || messages[0].Envelope.ID != wanted.ID {
		t.Fatalf("idle=%v cursor=%d messages=%+v err=%v messagesErr=%v", idle, cursor, messages, err, messagesErr)
	}
}
