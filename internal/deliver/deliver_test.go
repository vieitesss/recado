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

func (f *fixture) register(t *testing.T, id string, pid int, state store.AgentState) {
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

func (f *fixture) state(t *testing.T, mailbox, msgID string) store.MessageState {
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
		state      store.AgentState
		wantPushed bool
		wantState  store.MessageState
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
			if res.Message.Short == "" {
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
	f.register(t, "worker", os.Getpid(), store.AgentIdle)

	// Stored while the agent was busy, as a mid-turn send leaves them.
	for _, body := range []string{"first", "second"} {
		if err := f.st.Append("worker", f.env(t, "asker", "worker", envelope.KindMessage, body, nil)); err != nil {
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

func TestDrainReleasesWhatTheHarnessRefuses(t *testing.T) {
	f := setup(t, true)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	f.fake.Fail = true

	var ids []string
	for _, body := range []string{"first", "second"} {
		env := f.env(t, "asker", "worker", envelope.KindMessage, body, nil)
		ids = append(ids, env.ID)
		if _, err := f.svc.Deliver(env); err != nil {
			t.Fatal(err)
		}
	}
	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pending)+len(drained.Pushed) != 0 {
		t.Fatalf("Drain = %+v, want refused messages left for retry", drained)
	}
	for _, id := range ids {
		if got := f.state(t, "worker", id); got != store.StateStored {
			t.Errorf("state = %q, want stored", got)
		}
	}
	f.fake.Fail = false
	again, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pushed) != 2 {
		t.Errorf("retry pushed %d messages, want both", len(again.Pushed))
	}
}

func TestDrainOfGoneAgentReturnsPendingWithoutPush(t *testing.T) {
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

func TestDeliverPushesOlderStoredMessagesFirst(t *testing.T) {
	f := setup(t, false) // this harness cannot queue mid-turn
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	older := f.env(t, "asker", "worker", envelope.KindMessage, "older", nil)
	if err := f.st.Append("worker", older); err != nil {
		t.Fatal(err)
	}
	// The agent is idle now, but an older message is still stored.
	if err := f.st.SetState("worker", store.AgentIdle); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "newer", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed {
		t.Fatal("the new message was not pushed to the idle agent")
	}
	pushes, err := f.fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 2 || pushes[0].Body != "older" || pushes[1].Body != "newer" {
		t.Fatalf("pushed = %+v, want the stored backlog first, oldest first", pushes)
	}
	for _, m := range []envelope.Envelope{older, res.Message.Envelope} {
		if got := f.state(t, "worker", m.ID); got != store.StateQueued {
			t.Errorf("%s state = %q, want %q", m.ID, got, store.StateQueued)
		}
	}
}

// gatedHarness is a Fake whose Push blocks until release, so a test can hold a
// delivery in flight. failFirst rejects the first push after release.
type gatedHarness struct {
	harness.Fake
	started   chan struct{}
	release   chan struct{}
	failFirst bool

	mu    sync.Mutex
	calls int
}

func (g *gatedHarness) Push(agent store.Agent, msg harness.Outgoing) error {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	select {
	case g.started <- struct{}{}:
	default:
	}
	<-g.release
	if g.failFirst && call == 1 {
		return errors.New("push rejected")
	}
	return g.Fake.Push(agent, msg)
}

func TestTurnEndWaitsForInFlightFailedPush(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	h := &gatedHarness{
		Fake:      harness.Fake{Dir: t.TempDir()},
		started:   make(chan struct{}, 1),
		release:   make(chan struct{}),
		failFirst: true,
	}
	f := &fixture{svc: &Service{Store: st, Harnesses: harness.Set{h}}, st: st}
	f.register(t, "worker", os.Getpid(), store.AgentIdle)

	delivered := make(chan error, 1)
	go func() {
		_, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "wake", nil))
		delivered <- err
	}()
	<-h.started

	type result struct {
		drained Drained
		err     error
	}
	turnEnd := make(chan result, 1)
	go func() {
		d, err := f.svc.TurnEnd("worker")
		turnEnd <- result{d, err}
	}()

	select {
	case got := <-turnEnd:
		// The turn end ran while the failed push was still in flight: the
		// agent goes idle with the message stranded in stored.
		close(h.release)
		if err := <-delivered; err != nil {
			t.Fatal(err)
		}
		if len(got.drained.Pushed)+len(got.drained.Pending) != 1 {
			t.Fatalf("the turn-end drain delivered %d messages, want the one released by the failed push", len(got.drained.Pushed)+len(got.drained.Pending))
		}
	case <-time.After(200 * time.Millisecond):
		// The turn end waited for the delivery, so it sees the released message.
		close(h.release)
		if err := <-delivered; err != nil {
			t.Fatal(err)
		}
		got := <-turnEnd
		if got.err != nil {
			t.Fatal(got.err)
		}
		if len(got.drained.Pushed) != 1 || len(got.drained.Pending) != 0 {
			t.Fatalf("turn-end drain = %d pushed, %d pending; want the failed push retried", len(got.drained.Pushed), len(got.drained.Pending))
		}
	}
}

// queueGate is a harness whose QueuesWhileBusy blocks, so a test can hold a
// delivery between reading the recipient's state and pushing.
type queueGate struct {
	harness.Fake
	entered chan struct{}
	resume  chan struct{}
}

func (g *queueGate) QueuesWhileBusy() bool {
	close(g.entered)
	<-g.resume
	return g.Fake.Queue
}

func TestTurnStartWaitsForInFlightDelivery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	h := &queueGate{
		Fake:    harness.Fake{Dir: t.TempDir()},
		entered: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	f := &fixture{svc: &Service{Store: st, Harnesses: harness.Set{h}}, st: st}
	f.register(t, "worker", os.Getpid(), store.AgentIdle)

	delivered := make(chan error, 1)
	go func() {
		_, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "mid-turn", nil))
		delivered <- err
	}()
	<-h.entered

	turnDone := make(chan error, 1)
	go func() { turnDone <- f.svc.TurnStart("worker") }()

	select {
	case err := <-turnDone:
		// The old code recorded busy while the delivery was still deciding,
		// then pushed to a harness that cannot take messages mid-turn.
		if err != nil {
			t.Fatal(err)
		}
		close(h.resume)
		if err := <-delivered; err != nil {
			t.Fatal(err)
		}
		if pushes, err := h.Pushes(); err != nil || len(pushes) != 0 {
			t.Fatalf("pushes = %d, %v; a non-queueing harness was pushed to after turn-start recorded busy", len(pushes), err)
		}
	case <-time.After(200 * time.Millisecond):
		// The turn start waited for the delivery to finish, which is ordered
		// before the turn; releasing the gate lets the push complete.
		close(h.resume)
		if err := <-delivered; err != nil {
			t.Fatal(err)
		}
		if err := <-turnDone; err != nil {
			t.Fatal(err)
		}
		if pushes, err := h.Pushes(); err != nil || len(pushes) != 1 {
			t.Fatalf("pushes = %d, %v; want the delivery completed before the turn was recorded", len(pushes), err)
		}
	}
}

func TestDeliverRendersTheMessageForTheRecipient(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentIdle)
	env := f.env(t, "asker", "worker", envelope.KindQuestion, "which branch?", map[string]string{"item": "login"})
	env.ID = "01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7"
	if _, err := f.svc.Deliver(env); err != nil {
		t.Fatal(err)
	}
	texts, err := f.fake.Texts()
	if err != nil {
		t.Fatal(err)
	}
	want := "[recado] from asker · question · item=login (id v6t7)\n" +
		"which branch?\n" +
		"reply with: recado reply v6t7 \"…\""
	if len(texts) != 1 || texts[0] != want {
		t.Fatalf("harness got %q, want the message rendered with the recipient's short id:\n%s", texts, want)
	}
}

func TestDrainReportsEachMessageItHandedOver(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentIdle)
	// Stored while the agent was busy, as a mid-turn send leaves them.
	var second string
	for _, body := range []string{"first", "second"} {
		env := f.env(t, "asker", "worker", envelope.KindMessage, body, nil)
		if body == "second" {
			second = env.ID
		}
		if err := f.st.Append("worker", env); err != nil {
			t.Fatal(err)
		}
	}
	f.fake.FailAfter = 2 // the first message is written, the second is rejected

	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pushed) != 1 || len(drained.Pending) != 0 {
		t.Fatalf("Drain = %d pushed, %d pending; want only the first handed over", len(drained.Pushed), len(drained.Pending))
	}
	if got := f.state(t, "worker", second); got != store.StateStored {
		t.Errorf("second message state = %q, want stored", got)
	}
	pushes, err := f.fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 || pushes[0].Body != "first" {
		t.Fatalf("pushes = %+v, want only the first message", pushes)
	}

	// The next drain retries only the rejected message, never the pushed prefix.
	f.fake.FailAfter = 0
	again, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pushed) != 1 || again.Pushed[0].Envelope.ID != second {
		t.Fatalf("second Drain = %+v, want only the second message", again.Pushed)
	}
	pushes, err = f.fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 2 || pushes[0].Body != "first" || pushes[1].Body != "second" {
		t.Fatalf("pushes = %+v, want first then second with no duplicate", pushes)
	}
}

func TestDrainLeavesMessagesForABusyNonQueueingHarness(t *testing.T) {
	f := setup(t, false) // cannot take a message mid-turn
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "hi", nil)); err != nil {
		t.Fatal(err)
	}

	drained, err := f.svc.Drain("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Pushed) != 0 || len(drained.Pending) != 1 {
		t.Fatalf("Drain = %d pushed, %d pending; want the message left for the turn-end hook", len(drained.Pushed), len(drained.Pending))
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes = %d, %v; want none: the harness was busy", len(pushes), err)
	}
}

func TestHoldHandsPendingMessagesBackWithoutPushing(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	for _, body := range []string{"first", "second"} {
		if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, body, nil)); err != nil {
			t.Fatal(err)
		}
	}

	held, err := f.svc.Hold("worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Pushed) != 0 {
		t.Errorf("Pushed = %d, want none: a held turn never pushes", len(held.Pushed))
	}
	if len(held.Pending) != 2 || held.Pending[0].Envelope.Body != "first" || held.Pending[1].Envelope.Body != "second" {
		t.Fatalf("Pending = %+v, want both messages in order", held.Pending)
	}
	if pushes, err := f.fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushes = %d, %v; want none", len(pushes), err)
	}
	for _, msg := range held.Pending {
		if got := f.state(t, "worker", msg.Envelope.ID); got != store.StateQueued {
			t.Errorf("state = %q, want %q: the messages belong to the caller now", got, store.StateQueued)
		}
	}
	agent, err := f.st.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if agent.State != store.AgentBusy {
		t.Errorf("agent state = %q, want busy: the held messages start a new turn", agent.State)
	}

	// The new turn marks them read, and a later hold finds nothing.
	if err := f.svc.TurnStart("worker"); err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.Hold("worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pushed)+len(again.Pending) != 0 {
		t.Errorf("second Hold delivered %d messages again", len(again.Pushed)+len(again.Pending))
	}
}

func TestHoldRecordsIdleWhenNothingIsPending(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)

	held, err := f.svc.Hold("worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Pushed)+len(held.Pending) != 0 {
		t.Errorf("Hold = %+v, want nothing to hand back", held)
	}
	agent, err := f.st.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if agent.State != store.AgentIdle {
		t.Errorf("agent state = %q, want idle: nothing was waiting, so the turn really ended", agent.State)
	}
}

// TestHoldKeepsAForeignContinuationBusy: a stop hook that starts another turn
// is a turn recado never saw a prompt for, and it may start as many of them as
// it likes. Publishing idle when it ends would let a push land inside a turn
// that is still going, so only a continuation recado asked for settles.
func TestHoldKeepsAForeignContinuationBusy(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)

	held, err := f.svc.Hold("worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Pushed)+len(held.Pending) != 0 {
		t.Fatalf("Hold = %+v, want nothing to hand back", held)
	}
	agent, err := f.st.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if agent.State != store.AgentBusy {
		t.Fatalf("agent state = %q, want busy: the turn was someone else's continuation", agent.State)
	}
}

// TestHoldKeepsHandingOverDuringAContinuation: a continuation never settles the
// agent, but it is still a turn, so the messages waiting in the mailbox go out
// at each of its ends; only a turn that began with a prompt publishes idle.
func TestHoldKeepsHandingOverDuringAContinuation(t *testing.T) {
	f := setup(t, false)
	f.register(t, "worker", os.Getpid(), store.AgentBusy)
	if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "first", nil)); err != nil {
		t.Fatal(err)
	}
	held, err := f.svc.Hold("worker", false)
	if err != nil || len(held.Pending) != 1 || held.Pending[0].Envelope.Body != "first" {
		t.Fatalf("first Hold = %+v, %v; want the first message handed over", held, err)
	}

	if _, err := f.svc.Deliver(f.env(t, "asker", "worker", envelope.KindMessage, "second", nil)); err != nil {
		t.Fatal(err)
	}
	cont, err := f.svc.Hold("worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cont.Pending) != 1 || cont.Pending[0].Envelope.Body != "second" {
		t.Fatalf("Hold = %+v, want the next message handed over by the continuation", cont)
	}
	if agent, err := f.st.Lookup("worker"); err != nil || agent.State != store.AgentBusy {
		t.Fatalf("agent state = %q, %v; want busy", agent.State, err)
	}

	// The next prompt's end is recado's proof the turn really settled.
	if _, err := f.svc.Hold("worker", false); err != nil {
		t.Fatal(err)
	}
	if agent, err := f.st.Lookup("worker"); err != nil || agent.State != store.AgentIdle {
		t.Fatalf("agent state = %q, %v; want idle after a prompt's turn end", agent.State, err)
	}
}
