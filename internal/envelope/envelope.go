// Package envelope defines recado's message format: one JSON object per line.
package envelope

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/vieitesss/recado/internal/addr"
)

// Version is the format version this binary writes and reads.
const Version = 1

// Well-known kinds. recado gives meaning to KindQuestion only: it adds the
// reply hint to a delivered message.
const (
	KindMessage  = "message"
	KindQuestion = "question"
	KindAnswer   = "answer"
	KindComplete = "complete"
	KindFollowup = "followup"
)

// TimeLayout is the timestamp format of envelopes and receipts.
const TimeLayout = "2006-01-02T15:04:05.000Z07:00"

// FormatTime formats t the way recado stores timestamps.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a stored timestamp.
func ParseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// Envelope is one message.
type Envelope struct {
	V       int               `json:"v"`
	ID      string            `json:"id"`
	TS      time.Time         `json:"ts"`
	From    addr.Address      `json:"from"`
	To      addr.Address      `json:"to"`
	Kind    string            `json:"kind"`
	Body    string            `json:"body"`
	ReplyTo string            `json:"reply_to"`
	Meta    map[string]string `json:"meta"`
}

// New builds an envelope with a fresh id and timestamp.
func New(from, to addr.Address, kind, body string, meta map[string]string) (Envelope, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = KindMessage
	}
	if err := validKind(kind); err != nil {
		return Envelope{}, err
	}
	if err := validMeta(meta); err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:    Version,
		ID:   NewID(),
		TS:   time.Now().UTC().Truncate(time.Millisecond),
		From: from,
		To:   to,
		Kind: kind,
		Body: body,
		Meta: copyMeta(meta),
	}, nil
}

// NewID returns a fresh ULID.
func NewID() string { return ulid.Make().String() }

// Line encodes e as one JSON line, newline included.
func (e Envelope) Line() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Decode parses one JSON line and validates it, normalizing ids, addresses and
// the timestamp.
func Decode(line []byte) (Envelope, error) {
	var wire wireEnvelope
	if err := json.Unmarshal(line, &wire); err != nil {
		return Envelope{}, fmt.Errorf("envelope: %w", err)
	}
	if wire.V != Version {
		return Envelope{}, fmt.Errorf("envelope: version %d, this recado speaks %d", wire.V, Version)
	}
	id, err := ulid.ParseStrict(wire.ID)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: id %q: %w", wire.ID, err)
	}
	ts, err := ParseTime(wire.TS)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: ts %q: %w", wire.TS, err)
	}
	from, err := addr.Parse(wire.From)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: from: %w", err)
	}
	to, err := addr.Parse(wire.To)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: to: %w", err)
	}
	env := Envelope{
		V:       wire.V,
		ID:      id.String(),
		TS:      ts.UTC(),
		From:    from,
		To:      to,
		Kind:    strings.TrimSpace(wire.Kind),
		Body:    wire.Body,
		ReplyTo: wire.ReplyTo,
		Meta:    copyMeta(wire.Meta),
	}
	if env.Kind == "" {
		env.Kind = KindMessage
	}
	if env.ReplyTo != "" {
		reply, err := ulid.ParseStrict(env.ReplyTo)
		if err != nil {
			return Envelope{}, fmt.Errorf("envelope: reply_to %q: %w", env.ReplyTo, err)
		}
		env.ReplyTo = reply.String()
	}
	if err := validKind(env.Kind); err != nil {
		return Envelope{}, err
	}
	if err := validMeta(env.Meta); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// validKind keeps a kind one header token: no control characters, so a
// delivered message cannot gain lines.
func validKind(kind string) error {
	for _, r := range kind {
		if r < ' ' || r == 0x7f {
			return fmt.Errorf("kind %q: control characters are not allowed", kind)
		}
	}
	return nil
}

// validMeta keeps metadata renderable as k=v pairs on one line.
func validMeta(meta map[string]string) error {
	for k, v := range meta {
		if k == "" {
			return fmt.Errorf("meta: empty key")
		}
		for _, s := range []string{k, v} {
			for _, r := range s {
				if r < ' ' || r == 0x7f {
					return fmt.Errorf("meta %q: control characters are not allowed", k)
				}
			}
		}
	}
	return nil
}

// MarshalJSON writes the documented field set, always present, with the
// timestamp in TimeLayout.
func (e Envelope) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireEnvelope{
		V:       e.V,
		ID:      e.ID,
		TS:      FormatTime(e.TS),
		From:    e.From.String(),
		To:      e.To.String(),
		Kind:    e.Kind,
		Body:    e.Body,
		ReplyTo: e.ReplyTo,
		Meta:    copyMeta(e.Meta),
	})
}

// UnmarshalJSON reads the wire form with the same validation as Decode.
func (e *Envelope) UnmarshalJSON(data []byte) error {
	env, err := Decode(data)
	if err != nil {
		return err
	}
	*e = env
	return nil
}

type wireEnvelope struct {
	V       int               `json:"v"`
	ID      string            `json:"id"`
	TS      string            `json:"ts"`
	From    string            `json:"from"`
	To      string            `json:"to"`
	Kind    string            `json:"kind"`
	Body    string            `json:"body"`
	ReplyTo string            `json:"reply_to"`
	Meta    map[string]string `json:"meta"`
}

// Render is how a message appears in the recipient's timeline. short is the
// message's short id in the recipient's mailbox.
func Render(e Envelope, short string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[recado] from %s · %s", e.From.Display(), e.Kind)
	if len(e.Meta) > 0 {
		b.WriteString(" ·")
		keys := make([]string, 0, len(e.Meta))
		for k := range e.Meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%s", k, e.Meta[k])
		}
	}
	fmt.Fprintf(&b, " (id %s)", short)
	if e.Body != "" {
		b.WriteString("\n")
		b.WriteString(e.Body)
	}
	if e.Kind == KindQuestion {
		fmt.Fprintf(&b, "\nreply with: recado reply %s \"…\"", short)
	}
	return b.String()
}

func copyMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
