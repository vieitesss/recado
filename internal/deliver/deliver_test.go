package deliver

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

// deadPID is above Linux's and macOS's pid ceilings, so it is never live.
const deadPID = 1 << 22

type fixture struct {
	svc  *Service
	st   *store.Store
	fake *harness.Fake
}

func setup(t *testing.T, queue bool) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &harness.Fake{Dir: t.TempDir(), Queue: queue}
	return &fixture{
		svc:  &Service{Store: st, Harnesses: harness.Set{fake}},
		st:   st,
		fake: fake,
	}
}

func (f *fixture) register(t *testing.T, id string, pid int, state string) {
	t.Helper()
	err := f.st.Register(store.Agent{
		ID:       id,
		Harness:  "fake",
		Endpoint: "/tmp/" + id + ".sock",
		PID:      pid,
		Started:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := f.st.SetState(id, state); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) env(t *testing.T, from, to, kind, body string, meta map[string]string) envelope.Envelope {
	t.Helper()
	env, err := envelope.New(addr.Local(from), addr.Local(to), kind, body, meta)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (f *fixture) state(t *testing.T, mailbox, msgID string) string {
	t.Helper()
	msg, err := f.st.Resolve(mailbox, msgID)
	if err != nil {
		t.Fatal(err)
	}
	return msg.State
}

func TestPushDecision(t *testing.T) {
	cases := []struct {
		name       string
		queues     bool
		pid        int
		state      string
		wantPushed bool
		wantState  string
	}{
		{"live harness that queues, busy", true, os.Getpid(), store.AgentBusy, true, store.StateQueued},
		{"live harness that queues, no state", true, os.Getpid(), "", true, store.StateQueued},
		{"live harness that cannot queue, idle", false, os.Getpid(), store.AgentIdle, true, store.StateQueued},
		{"live harness that cannot queue, busy", false, os.Getpid(), store.AgentBusy, false, store.StateStored},
		{"live harness that cannot queue, no state", false, os.Getpid(), "", false, store.StateStored},
		{"dead harness that queues", true, deadPID, store.AgentIdle, false, store.StateStored},
		{"dead harness that cannot queue", false, deadPID, store.AgentIdle, false, store.StateStored},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, c.queues)
			f.register(t, "worker", c.pid, c.state)

			res, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "hi", nil))
			if err != nil {
				t.Fatal(err)
			}
			if res.Pushed != c.wantPushed {
				t.Errorf("Pushed = %v, want %v", res.Pushed, c.wantPushed)
			}
			if got := f.state(t, "worker", res.Message.Envelope.ID); got != c.wantState {
				t.Errorf("state = %q, want %q", got, c.wantState)
			}
			pushes, err := f.fake.Pushes()
			if err != nil {
				t.Fatal(err)
			}
			if pushed := len(pushes) == 1; pushed != c.wantPushed {
				t.Errorf("harness saw %d pushes, want pushed=%v", len(pushes), c.wantPushed)
			}
			if res.Short == "" {
				t.Error("no short id returned")
			}
		})
	}
}

func TestPushIsNotDeliveredAgainByDrain(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)

	res, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "hi", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed {
		t.Fatal("message was not pushed to a live, queueing harness")
	}

	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pushed) != 0 || len(drained.Pending) != 0 {
		t.Errorf("Drain = %d pushed, %d pending; want nothing: the push already delivered it", len(drained.Pushed), len(drained.Pending))
	}
	pushes, err := f.fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 {
		t.Errorf("harness saw %d pushes, want 1", len(pushes))
	}
}

func TestDrainDeliversStoredMessagesInOrder(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)

	for _, body := range []string{"first", "second"} {
		if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, body, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes before drain = %d, %v; want none", len(pushes), err)
	}

	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pushed) != 2 || len(drained.Pending) != 0 {
		t.Fatalf("Drain = %d pushed, %d pending; want 2 and 0", len(drained.Pushed), len(drained.Pending))
	}
	pushes, err := f.fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 2 || pushes[0].Body != "first" || pushes[1].Body != "second" {
		t.Fatalf("pushed = %+v, want first then second", pushes)
	}

	again, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pushed)+len(again.Pending) != 0 {
		t.Errorf("second Drain delivered %d messages again", len(again.Pushed)+len(again.Pending))
	}
}

func TestRejectedPushStaysStored(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	f.fake.Fail = true

	res, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "hi", nil))
	if err != nil {
		t.Fatalf("Deliver: %v; a rejected push is not a send failure", err)
	}
	if res.Pushed {
		t.Error("Pushed = true, want false")
	}
	if got := f.state(t, "worker", res.Message.Envelope.ID); got != store.StateStored {
		t.Errorf("state = %q, want %q", got, store.StateStored)
	}

	f.fake.Fail = false
	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pushed) != 1 {
		t.Errorf("Drain pushed %d, want 1: the stored message is retried", len(drained.Pushed))
	}
}

func TestDrainReturnsWhatItCouldNotPush(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	f.fake.Fail = true

	for _, body := range []string{"first", "second"} {
		if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, body, nil)); err != nil {
			t.Fatal(err)
		}
	}
	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pending) != 2 {
		t.Fatalf("Pending = %d, want 2 for the shim to deliver", len(drained.Pending))
	}
	if len(drained.Pushed) != 0 {
		t.Errorf("Pushed = %d, want 0", len(drained.Pushed))
	}
	again, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pushed)+len(again.Pending) != 0 {
		t.Error("a drain returned messages a previous drain already claimed")
	}
}

func TestDrainOfGoneAgentClaimsNothing(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", deadPID, "")
	if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "hi", nil)); err != nil {
		t.Fatal(err)
	}
	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pending) != 1 || len(drained.Pushed) != 0 {
		t.Errorf("Drain = %d pending, %d pushed; want 1 pending for the caller", len(drained.Pending), len(drained.Pushed))
	}
}

func TestConcurrentDrainsClaimEachMessageOnce(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	for range 10 {
		env := f.env(t, "asker", "worker", envelope.KindMessage, "hi", nil)
		if err := f.st.Append("worker", env); err != nil {
			t.Fatal(err)
		}
	}

	const drains = 10
	var wg sync.WaitGroup
	seen := make(chan string, 2*drains)
	for range drains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drained, err := f.svc.Drain("worker")
			if err != nil {
				t.Errorf("Drain: %v", err)
				return
			}
			for _, m := range append(drained.Pushed, drained.Pending...) {
				seen <- m.Envelope.ID
			}
		}()
	}
	wg.Wait()
	close(seen)

	unique := map[string]bool{}
	for id := range seen {
		if unique[id] {
			t.Errorf("message %s was handed out by two drains", id)
		}
		unique[id] = true
	}
	if len(unique) != 10 {
		t.Errorf("%d messages handed out, want 10", len(unique))
	}
}

func TestUnknownRecipient(t *testing.T) {
	f := setup(t, true)
	_, err := f.svc.Deliver(f.env(t, "asker", "nobody", envelope.KindMessage, "hi", nil))
	if !errors.Is(err, store.ErrUnknownAgent) {
		t.Errorf("Deliver(unknown) error = %v, want store.ErrUnknownAgent", err)
	}
}

func TestRemoteRecipientIsRejected(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", os.Getpid(), store.AgentIdle)
	remote, err := addr.Parse("recado://rpi/worker")
	if err != nil {
		t.Fatal(err)
	}
	env, err := envelope.New(addr.Local("asker"), remote, "", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Deliver(env); !errors.Is(err, ErrRemote) {
		t.Errorf("Deliver(remote) error = %v, want ErrRemote", err)
	}
}

func TestRepliesReachTheRightAsker(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentIdle)
	f.register(t, "asker-a", os.Getpid(), store.AgentIdle)
	f.register(t, "asker-b", os.Getpid(), store.AgentIdle)

	questions := map[string]envelope.Envelope{}
	for _, asker := range []string{"asker-a", "asker-b"} {
		env := f.env(t, asker, "worker", envelope.KindQuestion, asker+" asks", map[string]string{
			"item": asker, "report": ".agents/reports/" + asker + ".md",
		})
		if _, err := f.svc.Deliver(env); err != nil {
			t.Fatal(err)
		}
		questions[asker] = env
	}

	for _, asker := range []string{"asker-a", "asker-b"} {
		short, err := f.st.ShortID("worker", questions[asker].ID)
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.svc.Reply(addr.Local("worker"), short, "answer for "+asker)
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.Envelope.To != addr.Local(asker).String() {
			t.Errorf("reply to %s went to %s", asker, res.Message.Envelope.To)
		}
	}

	for _, asker := range []string{"asker-a", "asker-b"} {
		msgs, err := f.st.Messages(asker)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("%s has %d messages, want 1 answer", asker, len(msgs))
		}
		answer := msgs[0].Envelope
		if answer.Kind != envelope.KindAnswer {
			t.Errorf("%s got kind %q, want answer", asker, answer.Kind)
		}
		if answer.Body != "answer for "+asker {
			t.Errorf("%s got %q, want its own answer", asker, answer.Body)
		}
		if answer.From != addr.Local("worker").String() {
			t.Errorf("%s got from %q, want the worker", asker, answer.From)
		}
		question := questions[asker]
		if answer.ReplyTo != question.ID {
			t.Errorf("%s answer reply_to = %q, want %q", asker, answer.ReplyTo, question.ID)
		}
		for k, v := range question.Meta {
			if answer.Meta[k] != v {
				t.Errorf("%s answer meta[%s] = %q, want %q copied from the question", asker, k, answer.Meta[k], v)
			}
		}
	}
}

func TestReplyToUnknownID(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentIdle)
	if _, err := f.svc.Reply(addr.Local("worker"), "abcd", "x"); !errors.Is(err, store.ErrUnknownID) {
		t.Errorf("Reply(unknown id) error = %v, want store.ErrUnknownID", err)
	}
}
