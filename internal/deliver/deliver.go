// Package deliver performs recado's one delivery operation: store the
// envelope, push it if the recipient can take it now, otherwise leave it for
// the shim's drain at turn end or session start.
package deliver

import (
	"errors"
	"fmt"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

// ErrRemote is returned for addresses on another machine: only local delivery
// exists so far.
var ErrRemote = errors.New("machines not supported yet")

// Service delivers messages through the store and the known harnesses.
type Service struct {
	Store     *store.Store
	Harnesses harness.Set
}

// Result is what a delivery did with one envelope.
type Result struct {
	Message store.Message
	Short   string // the id the recipient sees
	Pushed  bool   // handed to the live harness now, rather than left to drain
}

// Deliver stores env and, when the recipient is live and its harness can take
// messages now, hands it over behind any older stored messages, in order.
// Everything from reading the recipient's state to the push happens under the
// recipient's delivery lock, so a turn boundary cannot interleave.
func (s *Service) Deliver(env envelope.Envelope) (Result, error) {
	from, err := addr.Parse(env.From)
	if err != nil {
		return Result{}, err
	}
	to, err := addr.Parse(env.To)
	if err != nil {
		return Result{}, err
	}
	if !from.IsLocal() || !to.IsLocal() {
		return Result{}, fmt.Errorf("%w: %s -> %s", ErrRemote, from, to)
	}
	// Reject an unknown recipient before touching its mailbox; deliver reads
	// it again under the lock.
	if _, err := s.Store.Lookup(to.Agent); err != nil {
		return Result{}, err
	}
	release, err := s.Store.LockDelivery(to.Agent)
	if err != nil {
		return Result{}, err
	}
	defer release()
	return s.deliver(env, to.Agent)
}

// deliver runs one delivery with the recipient's delivery lock held.
func (s *Service) deliver(env envelope.Envelope, id string) (Result, error) {
	agent, err := s.Store.Lookup(id)
	if err != nil {
		return Result{}, err
	}
	if err := s.Store.Append(id, env); err != nil {
		return Result{}, err
	}
	short, err := s.Store.ShortID(id, env.ID)
	if err != nil {
		return Result{}, err
	}
	res := Result{Message: store.Message{Envelope: env, State: store.StateStored}, Short: short}

	h, ok := s.Harnesses.Lookup(agent.Harness)
	if !ok || !agent.Live() {
		return res, nil
	}
	// A harness that cannot queue during busy turns must wait for the shim's
	// drain. Never reported is not idle.
	if !h.QueuesWhileBusy() && agent.State != store.AgentIdle {
		return res, nil
	}

	// Claim every stored message, so an older backlog goes out first: a newer
	// message never overtakes one that was already waiting.
	claimed, err := s.Store.ClaimPending(id)
	if err != nil {
		// The envelope is stored and the failed claim recorded nothing, so a
		// later drain owns delivery. send still returns once stored.
		return res, nil
	}
	pushed, rest := s.push(agent, h, claimed)
	if err := releaseMessages(s.Store, id, rest); err != nil {
		return Result{}, err
	}
	for _, msg := range pushed {
		if msg.Envelope.ID == env.ID {
			res.Pushed = true
			res.Message.State = store.StateQueued
			return res, nil
		}
	}
	// A concurrent claim (a drain or inbox) already owns the envelope.
	msg, err := s.Store.Resolve(id, env.ID)
	if err != nil {
		return Result{}, err
	}
	res.Message = msg
	return res, nil
}

// push hands msgs to h in order, one turn per message, and stops at the first
// failure. It returns the messages handed over and the ones still claimed.
func (s *Service) push(agent store.Agent, h harness.Harness, msgs []store.Message) (pushed, rest []store.Message) {
	for i, msg := range msgs {
		text, err := s.Store.Render(agent.ID, msg.Envelope)
		if err != nil {
			return pushed, msgs[i:]
		}
		if err := h.Push(agent, harness.Outgoing{Envelope: msg.Envelope, Text: text}); err != nil {
			return pushed, msgs[i:]
		}
		pushed = append(pushed, msg)
	}
	return pushed, nil
}

// releaseMessages returns claimed messages to stored, so a later drain retries
// them.
func releaseMessages(st *store.Store, id string, msgs []store.Message) error {
	var first error
	for _, msg := range msgs {
		if err := st.Release(id, msg.Envelope.ID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Reply answers the message query in from's mailbox. The answer goes to that
// message's sender, carries its id in reply_to, and copies its meta.
func (s *Service) Reply(from addr.Address, query, body string) (Result, error) {
	if !from.IsLocal() {
		return Result{}, fmt.Errorf("%w: sending from %s", ErrRemote, from)
	}
	msg, err := s.Store.Resolve(from.Agent, query)
	if err != nil {
		return Result{}, err
	}
	orig := msg.Envelope
	to, err := addr.Parse(orig.From)
	if err != nil {
		return Result{}, err
	}
	env, err := envelope.New(from, to, envelope.KindAnswer, body, orig.Meta)
	if err != nil {
		return Result{}, err
	}
	env.ReplyTo = orig.ID
	return s.Deliver(env)
}

// Drained is what a drain did.
type Drained struct {
	Pushed  []store.Message // handed to the harness
	Pending []store.Message // claimed; the caller must hand these over
}

// Drain claims every pending message for id, in order, and pushes it through
// the agent's harness. A drain runs at session start and turn end, so it is
// allowed to push even when the recorded state is not idle. Whatever could not
// be pushed is returned claimed, for the calling shim to deliver itself.
func (s *Service) Drain(id string) (Drained, error) {
	release, err := s.Store.LockDelivery(id)
	if err != nil {
		return Drained{}, err
	}
	defer release()
	return s.drain(id)
}

// drain runs one drain with the recipient's delivery lock held.
func (s *Service) drain(id string) (Drained, error) {
	agent, err := s.Store.Lookup(id)
	if err != nil {
		return Drained{}, err
	}
	msgs, err := s.Store.ClaimPending(id)
	if err != nil || len(msgs) == 0 {
		return Drained{}, err
	}
	h, ok := s.Harnesses.Lookup(agent.Harness)
	if !ok || !agent.Live() {
		return Drained{Pending: msgs}, nil
	}
	pushed, rest := s.push(agent, h, msgs)
	return Drained{Pushed: pushed, Pending: rest}, nil
}

// TurnStart records that the harness started a turn: the agent is busy and
// the messages handed over for this turn are read. It waits for an in-flight
// delivery, so a non-queueing harness is never pushed to after this returns.
func (s *Service) TurnStart(id string) error {
	release, err := s.Store.LockDelivery(id)
	if err != nil {
		return err
	}
	defer release()
	if err := s.Store.SetState(id, store.AgentBusy); err != nil {
		return err
	}
	return s.Store.MarkQueuedRead(id)
}

// TurnEnd records the agent idle and drains its mailbox under one lock, so a
// send cannot slip between publishing idle and draining the backlog.
func (s *Service) TurnEnd(id string) (Drained, error) {
	release, err := s.Store.LockDelivery(id)
	if err != nil {
		return Drained{}, err
	}
	defer release()
	if err := s.Store.SetState(id, store.AgentIdle); err != nil {
		return Drained{}, err
	}
	return s.drain(id)
}
