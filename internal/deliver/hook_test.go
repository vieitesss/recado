package deliver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

type failedHookWriter struct{ err error }

func (w failedHookWriter) Write([]byte) (int, error) { return 0, w.err }

type partialHookWriter struct {
	bytes.Buffer
	err error
}

func (w *partialHookWriter) Write(p []byte) (int, error) {
	if w.Len() > 0 {
		return 0, w.err
	}
	return w.Buffer.Write(p)
}

func writePending(w io.Writer, _ []store.Message, pending []store.Message, _ bool) error {
	for _, msg := range pending {
		if _, err := fmt.Fprintln(w, msg.Envelope.Body); err != nil {
			return err
		}
	}
	return nil
}

func TestHookAdapterOutputFailureRetriesHeldMessages(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	msg, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "waiting", nil))
	if err != nil {
		t.Fatal(err)
	}
	agent := store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}
	plan := harness.DefaultHook(harness.HookRequest{Event: "turn-end", Hold: true})
	outputErr := errors.New("stdout closed")
	if err := f.svc.Hook(agent, plan, failedHookWriter{outputErr}, false, writePending); !errors.Is(err, outputErr) {
		t.Fatalf("failed hold = %v, want output error", err)
	}
	if got := f.state(t, "worker", msg.Message.Envelope.ID); got != store.StateStored {
		t.Fatalf("state after failed hold = %q, want stored", got)
	}
	var out bytes.Buffer
	if err := f.svc.Hook(agent, plan, &out, false, writePending); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "waiting") {
		t.Fatalf("retry response = %q, want waiting message", out.String())
	}
	if got := f.state(t, "worker", msg.Message.Envelope.ID); got != store.StateQueued {
		t.Fatalf("state after retry = %q, want queued", got)
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes = %+v, %v; want none", pushes, err)
	}
}

func TestHookPushFailureAndOutputFailurePreserveBacklogOrder(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	var ids []string
	for _, body := range []string{"first", "second", "third"} {
		msg, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, body, nil))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, msg.Message.Envelope.ID)
	}
	f.fake.FailAfter = 2
	agent := store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}
	plan := harness.HookPlan{Event: "turn-end"}
	writePushed := func(w io.Writer, pushed, _ []store.Message, _ bool) error {
		for _, msg := range pushed {
			if _, err := fmt.Fprintln(w, msg.Envelope.Body); err != nil {
				return err
			}
		}
		return nil
	}
	outputErr := errors.New("stdout closed")
	if err := f.svc.Hook(agent, plan, failedHookWriter{outputErr}, false, writePushed); !errors.Is(err, outputErr) {
		t.Fatalf("failed hook = %v, want output error", err)
	}
	for i, want := range []store.MessageState{store.StateQueued, store.StateStored, store.StateStored} {
		if got := f.state(t, "worker", ids[i]); got != want {
			t.Errorf("state[%d] = %q, want %q", i, got, want)
		}
	}

	f.fake.FailAfter = 0
	var out bytes.Buffer
	if err := f.svc.Hook(agent, plan, &out, false, writePushed); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "second\nthird\n" {
		t.Fatalf("retry report = %q, want the remaining messages in order", got)
	}
	pushes, err := f.fake.Pushes()
	if err != nil || len(pushes) != 3 || pushes[0].Body != "first" || pushes[1].Body != "second" || pushes[2].Body != "third" {
		t.Fatalf("pushes = %+v, %v; want each message once in order", pushes, err)
	}
}

func TestHookDoesNotHoldDeliveryLockDuringResponse(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "first", nil)); err != nil {
		t.Fatal(err)
	}
	agent := store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}
	started, resume := make(chan struct{}), make(chan struct{})
	plan := harness.HookPlan{Event: "turn-end", HoldTurn: true, WriteResponse: func(io.Writer, []store.Message, []store.Message, bool) error {
		close(started)
		<-resume
		return nil
	}}
	hookDone := make(chan error, 1)
	go func() { hookDone <- f.svc.Hook(agent, plan, io.Discard, false, writePending) }()
	select {
	case <-started:
	case err := <-hookDone:
		t.Fatalf("hook returned before writing its response: %v", err)
	case <-time.After(3 * time.Second):
		close(resume)
		t.Fatal("hook did not start writing its response")
	}

	env := f.env(t, "asker", "worker", envelope.KindMessage, "second", nil)
	delivered := make(chan error, 1)
	go func() { _, err := f.svc.Deliver(env); delivered <- err }()
	select {
	case err := <-delivered:
		close(resume)
		if hookErr := <-hookDone; err != nil || hookErr != nil {
			t.Fatalf("deliver = %v, hook = %v", err, hookErr)
		}
	case <-time.After(3 * time.Second):
		close(resume)
		<-hookDone
		<-delivered
		t.Fatal("delivery waited for response output")
	}
	if got := f.state(t, "worker", env.ID); got != store.StateStored {
		t.Fatalf("new message state = %q, want stored during the held turn", got)
	}
}

func TestHookTurnStartMarksHeldMessagesReadAndUsesAdapterResponse(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	msg, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "waiting", nil))
	if err != nil {
		t.Fatal(err)
	}
	agent := store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}
	hold := harness.DefaultHook(harness.HookRequest{Event: "turn-end", Hold: true})
	if err := f.svc.Hook(agent, hold, io.Discard, false, writePending); err != nil {
		t.Fatal(err)
	}

	plan := harness.HookPlan{Event: "turn-start", WriteResponse: func(w io.Writer, _, _ []store.Message, json bool) error {
		_, err := fmt.Fprintf(w, "started json=%t\n", json)
		return err
	}}
	var out bytes.Buffer
	if err := f.svc.Hook(agent, plan, &out, true, writePending); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "started json=true\n" {
		t.Fatalf("turn-start response = %q", got)
	}
	if got := f.state(t, "worker", msg.Message.Envelope.ID); got != store.StateRead {
		t.Fatalf("state after turn-start = %q, want read", got)
	}
	registered, err := f.st.Lookup("worker")
	if err != nil || registered.State != store.AgentBusy {
		t.Fatalf("agent after turn-start = %+v, %v; want busy", registered, err)
	}

	out.Reset()
	plan.WriteResponse = nil
	standard := func(io.Writer, []store.Message, []store.Message, bool) error {
		return errors.New("standard response must not be written at turn start")
	}
	if err := f.svc.Hook(agent, plan, &out, false, standard); err != nil || out.Len() != 0 {
		t.Fatalf("default turn-start = %q, %v; want silent", out.String(), err)
	}
}

func TestHookHoldHandsOverBacklogWithoutPushing(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	var ids []string
	for _, body := range []string{"first", "second"} {
		msg, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, body, nil))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, msg.Message.Envelope.ID)
	}

	agent := store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}
	plan := harness.DefaultHook(harness.HookRequest{Event: "turn-end", Hold: true})
	var out bytes.Buffer
	if err := f.svc.Hook(agent, plan, &out, false, writePending); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Decision != "block" || !strings.Contains(response.Reason, "first") || !strings.Contains(response.Reason, "second") || strings.Index(response.Reason, "first") > strings.Index(response.Reason, "second") {
		t.Fatalf("hold response = %+v, want both messages in order", response)
	}
	for _, id := range ids {
		if got := f.state(t, "worker", id); got != store.StateQueued {
			t.Errorf("state after hold = %q, want queued", got)
		}
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes = %+v, %v; want none", pushes, err)
	}
	registered, err := f.st.Lookup("worker")
	if err != nil || registered.State != store.AgentBusy {
		t.Fatalf("agent after hold = %+v, %v; want busy", registered, err)
	}

	out.Reset()
	if err := f.svc.Hook(agent, plan, &out, false, writePending); err != nil {
		t.Fatal(err)
	}
	registered, err = f.st.Lookup("worker")
	if err != nil || registered.State != store.AgentIdle || out.Len() != 0 {
		t.Fatalf("empty hold = %q, agent %+v, %v; want silent and idle", out.String(), registered, err)
	}
}

func TestHookSessionStartKeepsUnpushableBacklog(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", deadPID, store.AgentBusy)
	msg, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "waiting", nil))
	if err != nil {
		t.Fatal(err)
	}

	agent := store.Agent{ID: "worker", Harness: "fake", PID: deadPID}
	var out bytes.Buffer
	plan := harness.HookPlan{Event: "session-start", KeepPending: true}
	if err := f.svc.Hook(agent, plan, &out, false, writePending); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("session-start output = %q, want no pending messages", out.String())
	}
	if got := f.state(t, "worker", msg.Message.Envelope.ID); got != store.StateStored {
		t.Fatalf("state after session-start = %q, want stored", got)
	}

	plan.KeepPending = false
	if err := f.svc.Hook(agent, plan, &out, false, writePending); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "waiting\n" {
		t.Fatalf("later hook output = %q, want waiting message", got)
	}
}

func TestHookOutputFailureReleasesClaimsForRetry(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", deadPID, store.AgentBusy)
	first, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "first", nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "second", nil))
	if err != nil {
		t.Fatal(err)
	}

	agent := store.Agent{ID: "worker", Harness: "fake", PID: deadPID}
	plan := harness.HookPlan{Event: "session-start"}
	outputErr := errors.New("stdout closed")
	partial := &partialHookWriter{err: outputErr}
	if err := f.svc.Hook(agent, plan, partial, false, writePending); !errors.Is(err, outputErr) {
		t.Fatalf("failed hook = %v, want output error", err)
	}
	if got := partial.String(); got != "first\n" {
		t.Fatalf("partial output = %q, want first message before failure", got)
	}
	for _, msg := range []store.Message{first.Message, second.Message} {
		if got := f.state(t, "worker", msg.Envelope.ID); got != store.StateStored {
			t.Errorf("state after failed output = %q, want stored", got)
		}
	}

	var out bytes.Buffer
	if err := f.svc.Hook(agent, plan, &out, false, writePending); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "first\nsecond\n" {
		t.Fatalf("retry output = %q, want both messages in order", got)
	}
	for _, msg := range []store.Message{first.Message, second.Message} {
		if got := f.state(t, "worker", msg.Envelope.ID); got != store.StateQueued {
			t.Errorf("state after retry = %q, want queued", got)
		}
	}
}
