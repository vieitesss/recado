package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/envelope"
)

func TestMailboxClaimsPendingOnce(t *testing.T) {
	s, root := open(t)
	for _, id := range []string{ulidA, ulidB, ulidC} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Fatalf("Mailbox = %d messages, want 3", len(claimed))
	}
	for i, want := range []string{ulidA, ulidB, ulidC} {
		if claimed[i].Envelope.ID != want || claimed[i].State != StateQueued {
			t.Errorf("claimed[%d] = %s %s, want %s %s", i, claimed[i].Envelope.ID, claimed[i].State, want, StateQueued)
		}
	}

	again, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second claim = %d messages, want none: a message is delivered once", len(again))
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
		if receipts[i]["id"] != want || receipts[i]["state"] != string(StateQueued) {
			t.Errorf("receipt %d = %v, want id %s state %s", i, receipts[i], want, StateQueued)
		}
		if _, err := envelope.ParseTime(receipts[i]["ts"]); err != nil {
			t.Errorf("receipt %d ts %q: %v", i, receipts[i]["ts"], err)
		}
	}
}

func TestMailboxClaimsMatchingMessageAndRelease(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB} {
		env := message(id, id)
		if id == ulidB {
			env.Meta["item"] = "selected"
		}
		if err := s.Append("sub-1", env); err != nil {
			t.Fatal(err)
		}
	}

	opts := MailboxOptions{Meta: map[string]string{"item": "selected"}, StoredOnly: true, Claim: true}
	msgs, err := s.Mailbox("sub-1", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].State != StateQueued || msgs[0].Envelope.ID != ulidB {
		t.Fatalf("Mailbox = %+v, want %s queued", msgs, ulidB)
	}
	if again, err := s.Mailbox("sub-1", opts); err != nil || len(again) != 0 {
		t.Fatalf("second claim = %+v, %v; want none, nil", again, err)
	}
	if err := s.Release("sub-1", "01JB7Q4W2Z6ZC9M1Z3X4Y5V6T9"); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("Release(unknown) = %v, want ErrUnknownID", err)
	}

	if err := s.Release("sub-1", ulidB); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].Envelope.ID != ulidA || claimed[1].Envelope.ID != ulidB {
		t.Fatalf("after release claim = %+v, want both in append order", claimed)
	}
}

func TestMarkQueuedRead(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true}); err != nil {
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

func TestMessagesCarryTheirMailboxShortID(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	// The two ids share their last four characters, so each needs one more.
	if len(msgs) != 2 || msgs[0].Short != "5v6t7" || msgs[1].Short != "4v6t7" {
		t.Fatalf("Messages = %+v, want each message to carry its own short id", msgs)
	}
}

func TestAppendRepairsATornTrailingInboxRecord(t *testing.T) {
	s, root := open(t)
	if err := s.Append("sub-1", message(ulidA, "first")); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(root, "mail", "sub-1", "inbox.jsonl")
	f, err := os.OpenFile(inbox, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	// What a writer that crashed mid-append leaves behind.
	if _, err := f.WriteString(`{"v":1,"id":"01JB`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Append("sub-1", message(ulidB, "second")); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatalf("a torn trailing record made the mailbox unreadable: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Envelope.Body != "first" || msgs[1].Envelope.Body != "second" {
		t.Fatalf("Messages = %+v, want first then second: the next append must not concatenate onto the torn line", msgs)
	}
}

func TestAppendReceiptRepairsATornTrailingRecord(t *testing.T) {
	s, root := open(t)
	if err := s.Append("sub-1", message(ulidA, "first")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true}); err != nil {
		t.Fatal(err)
	}
	receipts := filepath.Join(root, "mail", "sub-1", "receipts.jsonl")
	f, err := os.OpenFile(receipts, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"01JB`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Releasing appends another receipt; it must repair the torn one first.
	if err := s.Release("sub-1", ulidA); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatalf("a torn receipt made the mailbox unreadable: %v", err)
	}
	if len(msgs) != 1 || msgs[0].State != StateStored {
		t.Fatalf("Messages = %+v, want the released message stored", msgs)
	}
}

func TestMailboxClaimsNothingWhenReceiptsFailPartway(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB, ulidC} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	// Allow the file to grow by one receipt line, then fail: the old
	// per-receipt loop recorded a claimed prefix before the second write.
	sample, err := json.Marshal(receipt{ID: ulidA, State: StateQueued, TS: envelope.FormatTime(time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	restore := failAfterFileWrites(t, s, len(sample)+1)
	if _, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true}); err == nil {
		t.Fatal("claim succeeded though its receipts could not be written")
	}
	restore()

	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.State != StateStored {
			t.Errorf("%s state = %q after a failed receipt write, want %q: a claim must not be left half-recorded", m.Envelope.ID, m.State, StateStored)
		}
	}
	claimed, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claim after the failure returned %d messages, want 3: no message may be stranded", len(claimed))
	}
}

// failAfterFileWrites lets s write n bytes in total, then fails every later
// write. It simulates a disk filling up partway through a claim.
func failAfterFileWrites(t *testing.T, s *Store, n int) (restore func()) {
	t.Helper()
	written := 0
	errDiskFull := errors.New("disk full")
	s.write = func(f *os.File, data []byte) (int, error) {
		room := n - written
		if room <= 0 {
			return 0, errDiskFull
		}
		short := len(data) > room
		if short {
			data = data[:room]
		}
		m, err := f.Write(data)
		written += m
		if err != nil {
			return m, err
		}
		if short {
			return m, errDiskFull
		}
		return m, nil
	}
	restore = func() { s.write = nil }
	t.Cleanup(restore)
	return restore
}

// TestAppendReceiptRejectsUnknownState keeps the store from recording a state
// outside the message model, however a caller asks for one.
func TestAppendReceiptRejectsUnknownState(t *testing.T) {
	s, _ := open(t)
	if err := s.Append("sub-1", message(ulidA, "first")); err != nil {
		t.Fatal(err)
	}
	f, err := s.lockMailbox("sub-1", syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	err = s.appendReceipts("sub-1", []receipt{{ID: ulidA, State: "lost", TS: envelope.FormatTime(time.Now())}})
	Unlock(f)
	if err == nil {
		t.Fatal("appendReceipts recorded a state recado does not have")
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].State != StateStored {
		t.Fatalf("Messages = %+v, want the message still stored", msgs)
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
