// Package deliver performs recado's one delivery operation: store the
// envelope, push it if the recipient can take it now, otherwise leave it for
// the shim's drain at turn end or session start.
package deliver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/machine"
	"github.com/vieitesss/recado/internal/store"
)

// ErrNonLocalSender means the route requires an identity on this machine.
var ErrNonLocalSender = errors.New("sender must be local")

// Service delivers messages through the store and the known harnesses.
type Service struct {
	Store     *store.Store
	Harnesses harness.Set
	Machines  *machine.Registry
}

// Result is what a delivery did with one envelope. The message carries the
// state it has now and the short id the recipient sees.
type Result struct {
	Message store.Message
	Pushed  bool // handed to the live harness now, rather than left to drain
}

// Deliver stores env and, when the recipient is live and its harness can take
// messages now, hands it over behind any older stored messages, in order.
// Everything from reading the recipient's state to the push happens under the
// recipient's delivery lock, so a turn boundary cannot interleave.
func (s *Service) Deliver(env envelope.Envelope) (Result, error) {
	from, to := env.From, env.To
	if to.Upstream {
		if !from.IsLocal() {
			return Result{}, fmt.Errorf("%w for upstream send: %s", ErrNonLocalSender, from)
		}
		if err := s.Store.AppendOutbox(env); err != nil {
			return Result{}, err
		}
		// The recipient's shortest unique suffix is not known on this machine.
		return Result{Message: store.Message{Envelope: env, Short: strings.ToLower(env.ID), State: store.StateStored}}, nil
	}
	if !to.IsLocal() {
		if !from.IsLocal() {
			return Result{}, fmt.Errorf("%w for remote send: %s", ErrNonLocalSender, from)
		}
		return s.deliverRemote(env, from, to)
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
	return s.deliverLocked(env, to.Agent)
}

// deliverLocked runs one delivery with the recipient's delivery lock held.
func (s *Service) deliverLocked(env envelope.Envelope, id string) (Result, error) {
	agent, err := s.Store.Lookup(id)
	if err != nil {
		return Result{}, err
	}
	if err := s.Store.Append(id, env); err != nil {
		return Result{}, err
	}
	res := Result{Message: store.Message{Envelope: env, State: store.StateStored}}
	h, ok := s.Harnesses.Lookup(agent.Harness)
	// A harness that cannot queue during busy turns must wait for the shim's
	// drain. Never reported is not idle.
	if ok && agent.Live() && (h.QueuesWhileBusy() || agent.State == store.AgentIdle) {
		// Claim every stored message, so an older backlog goes out first: a newer
		// message never overtakes one that was already waiting.
		claimed, err := s.Store.Mailbox(id, store.MailboxOptions{StoredOnly: true, Claim: true})
		if err == nil {
			pushed, rest := s.push(agent, h, claimed)
			if err := s.release(id, rest); err != nil {
				return Result{}, err
			}
			for _, msg := range pushed {
				if msg.Envelope.ID == env.ID {
					res.Pushed = true
					res.Message.State = store.StateQueued
					res.Message.Short = msg.Short
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
		// The envelope is stored and the failed claim recorded nothing, so a
		// later drain owns delivery. send still returns once stored.
	}
	short, err := s.Store.ShortID(id, env.ID)
	if err != nil {
		return Result{}, err
	}
	res.Message.Short = short
	return res, nil
}

// push hands msgs to h in order, one turn per message, and stops at the first
// failure. It returns the messages handed over and the ones still claimed.
func (s *Service) push(agent store.Agent, h harness.Harness, msgs []store.Message) (pushed, rest []store.Message) {
	for i, msg := range msgs {
		text := envelope.Render(msg.Envelope, msg.Short)
		if err := h.Push(agent, harness.Outgoing{Envelope: msg.Envelope, Text: text}); err != nil {
			return pushed, msgs[i:]
		}
		pushed = append(pushed, msg)
	}
	return pushed, nil
}

// release returns claimed messages to stored, so a later drain retries them.
func (s *Service) release(id string, msgs []store.Message) error {
	var first error
	for _, msg := range msgs {
		if err := s.Store.Release(id, msg.Envelope.ID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Reply answers the message query in from's mailbox. The answer goes to that
// message's sender, carries its id in reply_to, and copies its meta.
func (s *Service) Reply(from addr.Address, query, body string) (Result, error) {
	if !from.IsLocal() {
		return Result{}, fmt.Errorf("%w for reply: %s", ErrNonLocalSender, from)
	}
	msg, err := s.Store.Resolve(from.Agent, query)
	if err != nil {
		return Result{}, err
	}
	orig := msg.Envelope
	env, err := envelope.New(from, orig.From, envelope.KindAnswer, body, orig.Meta)
	if err != nil {
		return Result{}, err
	}
	env.ReplyTo = orig.ID
	return s.Deliver(env)
}

// drainResult is what a drain did.
type drainResult struct {
	Pushed  []store.Message // handed to the harness
	Pending []store.Message // claimed; the caller must hand these over
}

// drain claims every pending message for id, in order, and pushes it through
// the agent's harness. A drain runs at session start and turn end, so it is
// allowed to push when the recorded state is idle or was never reported; a
// harness that cannot take a message mid-turn is left alone while it is busy.
// When no push can be attempted, the messages are returned claimed for the
// calling shim. A failed push releases the unhanded messages for a later retry.
func (s *Service) drain(id string) (drainResult, error) {
	release, err := s.Store.LockDelivery(id)
	if err != nil {
		return drainResult{}, err
	}
	defer release()
	return s.drainLocked(id)
}

// drainLocked runs one drain with the recipient's delivery lock held.
func (s *Service) drainLocked(id string) (drainResult, error) {
	agent, err := s.Store.Lookup(id)
	if err != nil {
		return drainResult{}, err
	}
	msgs, err := s.Store.Mailbox(id, store.MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil || len(msgs) == 0 {
		return drainResult{}, err
	}
	h, ok := s.Harnesses.Lookup(agent.Harness)
	if !ok || !agent.Live() || (!h.QueuesWhileBusy() && agent.State == store.AgentBusy) {
		return drainResult{Pending: msgs}, nil
	}
	pushed, rest := s.push(agent, h, msgs)
	if err := s.release(id, rest); err != nil {
		return drainResult{}, err
	}
	return drainResult{Pushed: pushed}, nil
}

// hold drains id's mailbox without pushing anything, for a harness that hands
// the messages to the agent itself. A turn-end hook is the case: the turn it
// runs in is still running, so pushing would land the message inside that turn
// instead of the next one. The messages come back claimed, in order, for the
// caller to hand over; the agent is recorded idle when nothing was waiting.
// That holds for the end of a turn a stop hook started too: recado's own
// hand-over starts one, and leaving the agent busy then would strand every
// later message until a prompt, since no turn end would come to drain it. A
// foreign stop hook that blocks again can make idle wrong, and a push then
// lands in the running turn, which Claude Code takes at its next step.
func (s *Service) hold(id string) (drainResult, error) {
	release, err := s.Store.LockDelivery(id)
	if err != nil {
		return drainResult{}, err
	}
	defer release()
	if _, err := s.Store.Lookup(id); err != nil {
		return drainResult{}, err
	}
	msgs, err := s.Store.Mailbox(id, store.MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil {
		return drainResult{}, err
	}
	if len(msgs) > 0 {
		return drainResult{Pending: msgs}, nil
	}
	return drainResult{}, s.Store.SetState(id, store.AgentIdle)
}

// turnStart records that the harness started a turn: the agent is busy and
// the messages handed over for this turn are read. It waits for an in-flight
// delivery, so a non-queueing harness is never pushed to after this returns.
func (s *Service) turnStart(id string) error {
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

// turnEnd records the agent idle and drains its mailbox under one lock, so a
// send cannot slip between publishing idle and draining the backlog.
func (s *Service) turnEnd(id string) (drainResult, error) {
	release, err := s.Store.LockDelivery(id)
	if err != nil {
		return drainResult{}, err
	}
	defer release()
	if err := s.Store.SetState(id, store.AgentIdle); err != nil {
		return drainResult{}, err
	}
	return s.drainLocked(id)
}
