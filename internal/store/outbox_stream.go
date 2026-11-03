package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// outboxFrame is the JSON-lines protocol shared by the producer and consumer.
type outboxFrame struct {
	Offset   int64           `json:"offset"`
	Envelope json.RawMessage `json:"envelope,omitempty"`
	CaughtUp bool            `json:"caught_up,omitempty"`
}

// WriteOutboxBatch emits the complete records for host after the given offset.
// A watermark, when requested, covers all complete lines, including other hosts.
// Non-JSON output contains envelopes only, without watermarks.
func (s *Store) WriteOutboxBatch(w io.Writer, after int64, host string, jsonOutput, watermark bool) (int64, error) {
	records, end, err := s.ReadOutbox(after, host)
	if err != nil {
		return after, err
	}
	enc := json.NewEncoder(w)
	for _, record := range records {
		if !jsonOutput {
			err = enc.Encode(record.Envelope)
		} else {
			var raw []byte
			raw, err = json.Marshal(record.Envelope)
			if err == nil {
				err = enc.Encode(outboxFrame{Offset: record.Offset, Envelope: raw})
			}
		}
		if err != nil {
			return after, err
		}
	}
	if jsonOutput && watermark {
		if err := enc.Encode(outboxFrame{Offset: end, CaughtUp: true}); err != nil {
			return after, err
		}
	}
	return end, nil
}

// OutboxConsumer delivers a stream and persists its cursor only after delivery.
// CaughtUp decides whether a watermark ends consumption; false keeps reading.
type OutboxConsumer struct {
	Machine    string
	Host       string
	Cursor     *int64
	Deliver    func(envelope.Envelope) error
	SaveCursor func(int64) error
	CaughtUp   func() (bool, error)
}

// Consume reads complete frames until CaughtUp requests a stop or an error occurs.
// Progress reports whether at least one cursor update was saved.
func (c OutboxConsumer) Consume(r io.Reader) (idle, progressed bool, err error) {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false, progressed, fmt.Errorf("outbox connection: %w", err)
		}
		var f outboxFrame
		if err = json.Unmarshal(line, &f); err != nil {
			return false, progressed, fmt.Errorf("outbox frame: %w", err)
		}
		if f.Offset < *c.Cursor {
			return false, progressed, fmt.Errorf("outbox offset moved backwards")
		}
		if f.Envelope != nil {
			env, err := envelope.Decode(f.Envelope)
			if err != nil {
				return false, progressed, err
			}
			if !env.From.IsLocal() {
				return false, progressed, fmt.Errorf("invalid remote sender %q", env.From)
			}
			if !env.To.Upstream || env.To.Machine != c.Host {
				return false, progressed, fmt.Errorf("invalid upstream recipient %q", env.To)
			}
			env.From = addr.Address{Machine: c.Machine, Agent: env.From.Agent}
			env.To = addr.Local(env.To.Agent)
			if err = c.Deliver(env); err != nil {
				return false, progressed, err
			}
		} else if !f.CaughtUp {
			return false, progressed, fmt.Errorf("invalid outbox frame")
		}
		if f.Offset > *c.Cursor {
			if err = c.SaveCursor(f.Offset); err != nil {
				return false, progressed, err
			}
			*c.Cursor = f.Offset
			progressed = true
		}
		if f.CaughtUp {
			yes, err := c.CaughtUp()
			if err != nil {
				return false, progressed, err
			}
			if yes {
				return true, progressed, nil
			}
		}
	}
}
