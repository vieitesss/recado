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
// the message now, claims and pushes it.
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
	agent, err := s.Store.Lookup(to.Agent)
	if err != nil {
		return Result{}, err
	}
	if err := s.Store.Append(to.Agent, env); err != nil {
		return Result{}, err
	}
	short, err := s.Store.ShortID(to.Agent, env.ID)
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

	msg, claimed, err := s.Store.Claim(to.Agent, env.ID)
	if err != nil {
		return Result{}, err
	}
	if !claimed { // a concurrent drain took it; it owns delivery now
		res.Message = msg
		return res, nil
	}
	if err := h.Push(agent, []envelope.Envelope{env}); err != nil {
		// The message stays durable: the shim's drain retries it.
		if rerr := s.Store.Release(to.Agent, env.ID); rerr != nil {
			return Result{}, rerr
		}
		return res, nil
	}
	res.Message.State = store.StateQueued
	res.Pushed = true
	return res, nil
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
	envs := make([]envelope.Envelope, len(msgs))
	for i, m := range msgs {
		envs[i] = m.Envelope
	}
	if err := h.Push(agent, envs); err != nil {
		return Drained{Pending: msgs}, nil
	}
	return Drained{Pushed: msgs}, nil
}
