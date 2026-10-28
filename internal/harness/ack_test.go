package harness

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/store"
)

// replyShim listens on sock, reads one pushed line and answers it with reply,
// then closes. An empty reply closes without answering, the way a shim does
// when it dies mid-push.
func replyShim(t *testing.T, sock, reply string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
			return
		}
		if reply != "" {
			_, _ = conn.Write([]byte(reply + "\n"))
		}
	}()
}

// TestPushWaitsForTheShimsAcknowledgement: a push counts as delivered only
// once the shim says its harness took the message, so a refused or unanswering
// shim returns an error and the message goes back to stored instead of being
// recorded queued.
func TestPushWaitsForTheShimsAcknowledgement(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantErr string
	}{
		{"accepted", `{"ok":true}`, ""},
		{"refused", `{"ok":false,"error":"no OpenCode session for agent ses_abc"}`, "no OpenCode session"},
		{"refused without a reason", `{"ok":false}`, "refused"},
		{"not an acknowledgement", `{"text":"hello"}`, "not an acknowledgement"},
		{"the shim died", "", "did not acknowledge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sock := filepath.Join(socketDir(t), "shim.sock")
			replyShim(t, sock, c.reply)
			agent := store.Agent{ID: "worker", Harness: "pi", Endpoint: sock}
			err := (&Pi{}).Push(agent, Outgoing{Text: "hi"})
			checkPush(t, "pi", err, c.wantErr)
		})
	}
}

// TestOpenCodePushWaitsForTheShimsAcknowledgement is the same contract for the
// OpenCode plugin, whose frame names the agent it is for.
func TestOpenCodePushWaitsForTheShimsAcknowledgement(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantErr string
	}{
		{"accepted", `{"ok":true}`, ""},
		{"refused", `{"ok":false,"error":"cannot prompt session ses_abc"}`, "cannot prompt session"},
		{"the shim died", "", "did not acknowledge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sock := filepath.Join(socketDir(t), "shim.sock")
			replyShim(t, sock, c.reply)
			agent := store.Agent{ID: "ses_abc", Harness: "opencode", Endpoint: sock}
			err := (&OpenCode{}).Push(agent, Outgoing{Text: "hi"})
			checkPush(t, "opencode", err, c.wantErr)
		})
	}
}

func checkPush(t *testing.T, name string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("%s: Push = %v, want the acknowledgement accepted", name, err)
	case want != "" && err == nil:
		t.Fatalf("%s: Push succeeded, want an error holding %q", name, want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("%s: Push = %v, want an error holding %q", name, err, want)
	}
}

// TestAwaitAckGivesUpOnASilentShim: a shim that never answers must not hold the
// sender, because the message is stored and a later drain retries it.
func TestAwaitAckGivesUpOnASilentShim(t *testing.T) {
	recado, shim := net.Pipe()
	defer recado.Close()
	defer shim.Close()
	if err := awaitAck(recado, 50*time.Millisecond, "pi"); err == nil {
		t.Fatal("awaitAck on a silent shim succeeded, want a timeout")
	}
}
