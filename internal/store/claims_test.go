package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/envelope"
)

func TestClaimPendingClaimsOnce(t *testing.T) {
	s, root := open(t)
	for _, id := range []string{ulidA, ulidB, ulidC} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := s.ClaimPending("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Fatalf("ClaimPending = %d messages, want 3", len(claimed))
	}
	for i, want := range []string{ulidA, ulidB, ulidC} {
		if claimed[i].Envelope.ID != want || claimed[i].State != StateQueued {
			t.Errorf("claimed[%d] = %s %s, want %s %s", i, claimed[i].Envelope.ID, claimed[i].State, want, StateQueued)
		}
	}

	again, err := s.ClaimPending("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second ClaimPending = %d messages, want none: a message is delivered once", len(again))
	}

	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.State != StateQueued {
			t.Errorf("%s state = %q, want %q", m.Envelope.ID, m.State, StateQueued)
		}
	}
	requireMode(t, filepath.Join(root, "mail", "sub-1", "receipts.jsonl"), FileMode)

	receipts := readReceipts(t, s, "sub-1")
	if len(receipts) != 3 {
		t.Fatalf("receipts = %d lines, want 3", len(receipts))
	}
	for i, want := range []string{ulidA, ulidB, ulidC} {
		if receipts[i]["id"] != want || receipts[i]["state"] != StateQueued {
			t.Errorf("receipt %d = %v, want id %s state %s", i, receipts[i], want, StateQueued)
		}
		if _, err := envelope.ParseTime(receipts[i]["ts"]); err != nil {
			t.Errorf("receipt %d ts %q: %v", i, receipts[i]["ts"], err)
		}
	}
}

func TestClaimSpecificMessageAndRelease(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	msg, ok, err := s.Claim("sub-1", ulidB)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || msg.State != StateQueued || msg.Envelope.ID != ulidB {
		t.Fatalf("Claim = %+v, %v; want %s queued", msg, ok, ulidB)
	}
	if _, ok, err := s.Claim("sub-1", ulidB); err != nil || ok {
		t.Fatalf("second Claim = %v, %v; want false, nil", ok, err)
	}
	if _, ok, err := s.Claim("sub-1", "01JB7Q4W2Z6ZC9M1Z3X4Y5V6T9"); !errors.Is(err, ErrUnknownID) || ok {
		t.Fatalf("Claim(unknown) = %v, %v; want ErrUnknownID", ok, err)
	}

	if err := s.Release("sub-1", ulidB); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPending("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].Envelope.ID != ulidA || claimed[1].Envelope.ID != ulidB {
		t.Fatalf("after release ClaimPending = %+v, want both in append order", claimed)
	}
}

func TestMarkQueuedRead(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimPending("sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQueuedRead("sub-1"); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.State != StateRead {
			t.Errorf("%s state = %q, want %q", m.Envelope.ID, m.State, StateRead)
		}
	}
}

func TestRenderUsesMailboxShortID(t *testing.T) {
	s, _ := open(t)
	if err := s.Append("sub-1", message(ulidA, "hello")); err != nil {
		t.Fatal(err)
	}
	msg, err := s.Resolve("sub-1", "5v6t7")
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.Render("sub-1", msg.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "[recado] from asker · message (id v6t7)\nhello") {
		t.Errorf("Render = %q", text)
	}
}

func readReceipts(t *testing.T, s *Store, id string) []map[string]string {
	t.Helper()
	data, err := os.ReadFile(s.receiptsPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r map[string]string
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("receipt line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}
