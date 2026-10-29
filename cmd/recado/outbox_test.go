package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/envelope"
)

func TestOutboxAfterAndHost(t *testing.T) {
	t.Setenv("RECADO_ADDR", "worker")
	a, _ := newTestApp(t, false)
	mustExec(t, a, "", "send", "recado+upstream://host/one", "first")
	mustExec(t, a, "", "send", "recado+upstream://other/two", "other")
	mustExec(t, a, "", "send", "recado+upstream://host/one", "last")
	out := mustExec(t, a, "", "outbox", "--host", "host", "--json")
	dec := json.NewDecoder(strings.NewReader(out))
	var first, last struct {
		Offset   int64             `json:"offset"`
		Envelope envelope.Envelope `json:"envelope"`
	}
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&last); err != nil {
		t.Fatal(err)
	}
	if first.Envelope.Body != "first" || last.Envelope.Body != "last" || first.Offset <= 0 || last.Offset <= first.Offset {
		t.Fatalf("records = %+v %+v", first, last)
	}
	after := mustExec(t, a, "", "outbox", "--host", "host", "--after", strconv.FormatInt(first.Offset, 10), "--json")
	if strings.Contains(after, "first") || strings.Contains(after, "other") || !strings.Contains(after, "last") {
		t.Fatalf("after = %s", after)
	}
}

func TestOutboxInvalidOffsetsAndIOFailures(t *testing.T) {
	a, _ := newTestApp(t, false)
	t.Setenv("RECADO_ADDR", "worker")
	if _, err := exec(t, a, "", "outbox", "--host", "host", "--after", "1"); exitCode(err) != 2 {
		t.Errorf("offset beyond missing outbox: exit %d, want 2 (err %v)", exitCode(err), err)
	}
	mustExec(t, a, "", "send", "recado+upstream://host/one", "first")
	for _, offset := range []string{"-1", "1", "999999"} {
		if _, err := exec(t, a, "", "outbox", "--host", "host", "--after", offset); exitCode(err) != 2 {
			t.Errorf("offset %s: exit %d, want 2 (err %v)", offset, exitCode(err), err)
		}
	}
	path := filepath.Join(a.Root, "outbox.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(t, a, "", "outbox", "--host", "host"); exitCode(err) != 1 {
		t.Errorf("outbox read failure: exit %d, want 1 (err %v)", exitCode(err), err)
	}
}

func TestOutboxFollowStreamsNewLines(t *testing.T) {
	t.Setenv("RECADO_ADDR", "worker")
	a, _ := newTestApp(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w := io.Pipe()
	defer r.Close()
	cmd := newRootCmd(a)
	cmd.SetArgs([]string{"outbox", "--follow", "--host", "host", "--json"})
	cmd.SetOut(w)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx); w.Close() }()
	lines := make(chan string, 5)
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			lines <- s.Text()
		}
	}()
	read := func() string {
		t.Helper()
		select {
		case l := <-lines:
			return l
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for outbox")
			return ""
		}
	}
	if line := read(); !strings.Contains(line, `"caught_up":true`) {
		t.Fatalf("watermark = %s", line)
	}
	mustExec(t, a, "", "send", "recado+upstream://host/one", "new")
	for {
		line := read()
		if strings.Contains(line, `"body":"new"`) {
			break
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("follow did not stop")
	}
}
