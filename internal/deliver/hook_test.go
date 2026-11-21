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

func TestHookRejectsUnknownEvent(t *testing.T) {
	f := setup(t, false)
	err := f.svc.Hook(store.Agent{ID: "worker"}, harness.HookPlan{Event: "unexpected"}, io.Discard, false, writePending)
	var unknown UnknownHookEvent
	if !errors.As(err, &unknown) || unknown != "unexpected" || err.Error() != `unknown hook event "unexpected": want session-start, turn-start or turn-end` {
		t.Fatalf("unknown event error = %v", err)
	}
	if _, err := f.st.Lookup("worker"); !errors.Is(err, store.ErrUnknownAgent) {
		t.Fatalf("unknown event registered an agent: %v", err)
	}
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
	// Without an adapter response a turn start still answers in the standard
	// format, with nothing drained: shims read every hook's output.
	standard := func(w io.Writer, pushed, pending []store.Message, json bool) error {
		_, err := fmt.Fprintf(w, "standard pushed=%d pending=%d json=%t\n", len(pushed), len(pending), json)
		return err
	}
	if err := f.svc.Hook(agent, plan, &out, true, standard); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "standard pushed=0 pending=0 json=true\n" {
		t.Fatalf("default turn-start = %q, want the standard response with nothing drained", got)
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

func TestHookSessionStartRetiresTheOldIDsOfAOneSessionProcess(t *testing.T) {
	f := setup(t, false)
	f.fake.Single = true
	start := func(id string) {
		t.Helper()
		agent := store.Agent{ID: id, Harness: "fake", PID: os.Getpid()}
		if err := f.svc.Hook(agent, harness.HookPlan{Event: "session-start"}, io.Discard, false, writePending); err != nil {
			t.Fatal(err)
		}
	}
	start("old-1")
	start("old-2")
	start("new")

	for _, id := range []string{"old-1", "old-2"} {
		a, err := f.st.Lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		if a.Live() {
			t.Errorf("%s stayed live after a new id started in the same process", id)
		}
	}
	a, err := f.st.Lookup("new")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Live() {
		t.Error("the session that just started is not live")
	}
}

func TestHookSessionStartKeepsTheIDsOfAProcessThatServesSeveralSessions(t *testing.T) {
	f := setup(t, false)
	f.fake.Single = false
	start := func(id string) {
		t.Helper()
		agent := store.Agent{ID: id, Harness: "fake", PID: os.Getpid()}
		if err := f.svc.Hook(agent, harness.HookPlan{Event: "session-start"}, io.Discard, false, writePending); err != nil {
			t.Fatal(err)
		}
	}
	start("first")
	start("second")

	for _, id := range []string{"first", "second"} {
		a, err := f.st.Lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		if !a.Live() {
			t.Errorf("%s was retired, though this harness serves several sessions per process", id)
		}
	}
}

func TestDeliverDoesNotPushToARetiredID(t *testing.T) {
	f := setup(t, true)
	f.fake.Single = true
	for _, id := range []string{"old", "new"} {
		agent := store.Agent{ID: id, Harness: "fake", PID: os.Getpid()}
		if err := f.svc.Hook(agent, harness.HookPlan{Event: "session-start"}, io.Discard, false, writePending); err != nil {
			t.Fatal(err)
		}
	}

	msg, err := f.svc.Deliver(f.env(t, "asker", "old", envelope.KindMessage, "waiting", nil))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Pushed {
		t.Error("a message to a retired id was pushed")
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes = %+v, %v; want none for a retired id", pushes, err)
	}
	if got := f.state(t, "old", msg.Message.Envelope.ID); got != store.StateStored {
		t.Fatalf("state = %q, want the message waiting in the retired mailbox", got)
	}
}

func TestResumedSessionGetsTheMessagesThatWaitedForIt(t *testing.T) {
	f := setup(t, true)
	f.fake.Single = true
	start := func(id string) {
		t.Helper()
		agent := store.Agent{ID: id, Harness: "fake", PID: os.Getpid()}
		if err := f.svc.Hook(agent, harness.HookPlan{Event: "session-start"}, io.Discard, false, writePending); err != nil {
			t.Fatal(err)
		}
	}
	start("old")
	start("new")
	msg, err := f.svc.Deliver(f.env(t, "asker", "old", envelope.KindMessage, "waiting", nil))
	if err != nil {
		t.Fatal(err)
	}

	start("old")

	a, err := f.st.Lookup("old")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Live() {
		t.Error("the resumed id is not live")
	}
	if got := f.state(t, "old", msg.Message.Envelope.ID); got != store.StateQueued {
		t.Errorf("state after resume = %q, want the waiting message queued", got)
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 1 {
		t.Fatalf("pushes = %d, %v; want the waiting message handed to the resumed session", len(pushes), err)
	}
}

func TestHookRefuseForeignAgent(t *testing.T) {
	t.Run("rejects live agent owned by another harness", func(t *testing.T) {
		f := setup(t, false)
		if err := f.st.Register(store.Agent{ID: "worker", Harness: "other", PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		if err := f.st.SetState("worker", store.AgentBusy); err != nil {
			t.Fatal(err)
		}
		err := f.svc.Hook(store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}, harness.HookPlan{Event: "session-start"}, io.Discard, false, writePending)
		var foreign ForeignAgent
		if !errors.As(err, &foreign) {
			t.Fatalf("Hook = %v, want ForeignAgent", err)
		}
		if foreign.ID != "worker" || foreign.Harness != "fake" || foreign.Owner != "other" {
			t.Errorf("ForeignAgent = %+v, want {ID:worker Harness:fake Owner:other}", foreign)
		}
		got, err := f.st.Lookup("worker")
		if err != nil {
			t.Fatal(err)
		}
		if got.Harness != "other" || got.State != store.AgentBusy {
			t.Errorf("existing entry = %+v, want untouched busy other agent", got)
		}
	})

	t.Run("allows unowned legacy entry", func(t *testing.T) {
		f := setup(t, false)
		if err := f.st.Register(store.Agent{ID: "worker", Harness: "", PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Hook(store.Agent{ID: "worker", Harness: "fake", PID: os.Getpid()}, harness.HookPlan{Event: "session-start"}, io.Discard, false, writePending); err != nil {
			t.Fatalf("Hook over unowned entry = %v, want nil", err)
		}
		got, err := f.st.Lookup("worker")
		if err != nil {
			t.Fatal(err)
		}
		if got.Harness != "fake" {
			t.Errorf("harness after takeover = %q, want fake", got.Harness)
		}
	})

	t.Run("allows hook that names no harness", func(t *testing.T) {
		f := setup(t, false)
		if err := f.st.Register(store.Agent{ID: "worker", Harness: "other", PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Hook(store.Agent{ID: "worker", Harness: "", PID: os.Getpid()}, harness.HookPlan{Event: "turn-start"}, io.Discard, false, writePending); err != nil {
			t.Fatalf("Hook with empty agent harness = %v, want nil", err)
		}
	})
}
