package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/envelope"
)

func TestMailboxClaimsMatchingMetadata(t *testing.T) {
	s, _ := open(t)
	for _, tc := range []struct {
		id   string
		meta map[string]string
	}{
		{ulidA, map[string]string{"item": "login", "stage": "build"}},
		{ulidB, map[string]string{"item": "login", "stage": "review"}},
		{ulidC, map[string]string{"item": "login", "stage": "build"}},
	} {
		env := message(tc.id, tc.id)
		env.Meta = tc.meta
		if err := s.Append("sub-1", env); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := s.Mailbox("sub-1", MailboxOptions{
		Meta:  map[string]string{"item": "login", "stage": "build"},
		Claim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Envelope.ID != ulidA || msgs[1].Envelope.ID != ulidC ||
		msgs[0].State != StateQueued || msgs[1].State != StateQueued {
		t.Fatalf("Mailbox = %+v, want the two matching messages queued in append order", msgs)
	}
	all, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if all[1].State != StateStored {
		t.Fatalf("nonmatching message state = %s, want stored", all[1].State)
	}
}

func TestMailboxEmptyMetadataFilterMatchesMissingKey(t *testing.T) {
	s, _ := open(t)
	env := message(ulidA, "missing key")
	if err := s.Append("sub-1", env); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Mailbox("sub-1", MailboxOptions{Meta: map[string]string{"missing": ""}, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].State != StateQueued {
		t.Fatalf("Mailbox = %+v, want the message without that key queued", msgs)
	}
}

func TestMailboxShortIDsIncludeUnselectedMessages(t *testing.T) {
	s, _ := open(t)
	for _, tc := range []struct {
		id, item string
	}{
		{ulidA, "selected"},
		{ulidB, "other"},
		{"11JB7Q4W2Z6ZC9M1Z3X4Y5V6T7", "other"},
		{ulidC, "selected"},
	} {
		env := message(tc.id, tc.id)
		env.Meta = map[string]string{"item": tc.item}
		if err := s.Append("sub-1", env); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.Mailbox("sub-1", MailboxOptions{Meta: map[string]string{"item": "selected"}, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Short != strings.ToLower(ulidA) || msgs[1].Short != "w6t7" {
		t.Fatalf("short ids = %+v, want the full lowercase id and w6t7", msgs)
	}
}

func TestMailboxConcurrentClaimsDoNotOverlap(t *testing.T) {
	s, root := open(t)
	for _, id := range []string{ulidA, ulidB, ulidC} {
		env := message(id, id)
		env.Meta = map[string]string{"item": "login"}
		if err := s.Append("sub-1", env); err != nil {
			t.Fatal(err)
		}
	}
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 12
	type result struct {
		msgs []Message
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	for i := 0; i < workers; i++ {
		st := s
		if i%2 == 0 {
			st = other
		}
		go func() {
			<-start
			msgs, err := st.Mailbox("sub-1", MailboxOptions{
				Meta: map[string]string{"item": "login"}, StoredOnly: true, Claim: true,
			})
			results <- result{msgs, err}
		}()
	}
	close(start)
	seen := map[string]int{}
	for i := 0; i < workers; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		for _, msg := range r.msgs {
			if msg.State != StateQueued {
				t.Errorf("claimed state = %s, want queued", msg.State)
			}
			seen[msg.Envelope.ID]++
		}
	}
	for _, id := range []string{ulidA, ulidB, ulidC} {
		if seen[id] != 1 {
			t.Errorf("message %s claimed %d times, want once", id, seen[id])
		}
	}
}

func TestMailboxFilteredClaimRollsBackPartialReceiptWrite(t *testing.T) {
	s, root := open(t)
	for _, tc := range []struct{ id, item string }{
		{ulidA, "login"}, {ulidB, "other"}, {ulidC, "login"},
	} {
		env := message(tc.id, tc.id)
		env.Meta = map[string]string{"item": tc.item}
		if err := s.Append("sub-1", env); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Mailbox("sub-1", MailboxOptions{Meta: map[string]string{"item": "other"}, Claim: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "mail", "sub-1", "receipts.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := json.Marshal(receipt{ID: ulidA, State: StateQueued, TS: envelope.FormatTime(time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	restore := failAfterFileWrites(t, s, len(sample)+1)
	opts := MailboxOptions{Meta: map[string]string{"item": "login"}, Claim: true}
	if _, err := s.Mailbox("sub-1", opts); err == nil {
		t.Fatal("claim succeeded despite a partial receipt write")
	}
	restore()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("receipts changed after failed claim: before %q, after %q", before, after)
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].State != StateStored || msgs[1].State != StateQueued || msgs[2].State != StateStored {
		t.Fatalf("after failed claim = %+v, want stored, queued, stored", msgs)
	}
	claimed, err := s.Mailbox("sub-1", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].State != StateQueued || claimed[1].State != StateQueued {
		t.Fatalf("retry = %+v, want both matching messages queued", claimed)
	}
}

func TestMailboxFiltersStatesAndPeeks(t *testing.T) {
	s, _ := open(t)
	if err := s.Append("sub-1", message(ulidA, "read")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mailbox("sub-1", MailboxOptions{Claim: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQueuedRead("sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("sub-1", message(ulidB, "queued")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mailbox("sub-1", MailboxOptions{Claim: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("sub-1", message(ulidC, "stored")); err != nil {
		t.Fatal(err)
	}

	unread, err := s.Mailbox("sub-1", MailboxOptions{ExcludeRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 2 || unread[0].State != StateQueued || unread[1].State != StateStored {
		t.Fatalf("unread peek = %+v, want queued then stored", unread)
	}
	pending, err := s.Mailbox("sub-1", MailboxOptions{StoredOnly: true, Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Envelope.ID != ulidC || pending[0].State != StateQueued {
		t.Fatalf("pending claim = %+v, want only the stored message queued", pending)
	}
	all, err := s.Mailbox("sub-1", MailboxOptions{Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].State != StateRead || all[1].State != StateQueued || all[2].State != StateQueued {
		t.Fatalf("all = %+v, want read then queued then queued", all)
	}
}
