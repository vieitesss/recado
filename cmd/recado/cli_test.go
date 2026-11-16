package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/machine"
)

func newTestApp(t *testing.T, queues bool) (*app, *harness.Fake) {
	t.Helper()
	fake := &harness.Fake{Dir: t.TempDir(), Queue: queues}
	return &app{
		Root:       filepath.Join(t.TempDir(), "state"),
		Harnesses:  harness.Set{fake},
		Machines:   &machine.Registry{Path: filepath.Join(t.TempDir(), "recado", "machines.toml"), Kinds: machine.DefaultKinds()},
		startRelay: func(string) error { return nil },
	}, fake
}

// exec runs one command line, the way main does, and returns its output.
func exec(t *testing.T, a *app, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd(a)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func mustExec(t *testing.T, a *app, stdin string, args ...string) string {
	t.Helper()
	out, err := exec(t, a, stdin, args...)
	if err != nil {
		t.Fatalf("recado %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func decode[T any](t *testing.T, data string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return v
}

// startSession registers id the way a harness shim does.
func startSession(t *testing.T, a *app, id string) {
	t.Helper()
	t.Setenv("RECADO_ADDR", id)
	mustExec(t, a, "", "hook", "fake", "session-start")
}

func TestWhoami(t *testing.T) {
	a, _ := newTestApp(t, false)
	t.Setenv("FAKE_AGENT_ID", "")

	t.Setenv("RECADO_ADDR", "asker")
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/asker" {
		t.Errorf("whoami = %q, want recado://local/asker", got)
	}

	t.Setenv("RECADO_ADDR", "")
	t.Setenv("FAKE_AGENT_ID", "detected-1")
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/detected-1" {
		t.Errorf("whoami = %q, want the harness-detected address", got)
	}
	if out := strings.TrimSpace(mustExec(t, a, "", "whoami", "--json")); !strings.Contains(out, `"agent": "detected-1"`) {
		t.Errorf("whoami --json = %s", out)
	}
	if out := strings.TrimSpace(mustExec(t, a, "", "--json", "whoami")); !strings.Contains(out, `"agent": "detected-1"`) {
		t.Errorf("recado --json whoami = %s", out)
	}

	t.Setenv("FAKE_AGENT_ID", "")
	if _, err := exec(t, a, "", "whoami"); exitCode(err) != 2 {
		t.Errorf("whoami with no address: exit %d, want 2 (err %v)", exitCode(err), err)
	}
}

func TestSendStoresForBusyAgentAndDrainDelivers(t *testing.T) {
	a, fake := newTestApp(t, false) // this harness cannot queue mid-turn
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "fake", "turn-start")

	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "ask", "worker", "which branch?", "--meta", "item=login"))

	if pushes, err := fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Fatalf("pushed while busy = %d, %v; want none", len(pushes), err)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Errorf("status = %q, want %q", got, short+" stored")
	}

	// Turn end: the shim drains, and the harness takes the message.
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "fake", "turn-end")
	pushes, err := fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 || pushes[0].Body != "which branch?" || pushes[0].Kind != "question" {
		t.Fatalf("pushed = %+v, want the question", pushes)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" queued" {
		t.Errorf("status after drain = %q, want queued", got)
	}

	// A second drain must not deliver it again.
	mustExec(t, a, "", "hook", "fake", "turn-end")
	if pushes, err := fake.Pushes(); err != nil || len(pushes) != 1 {
		t.Fatalf("pushes after second drain = %d, %v; want 1", len(pushes), err)
	}

	// The turn that carries it marks it read.
	mustExec(t, a, "", "hook", "fake", "turn-start")
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" read" {
		t.Errorf("status after turn start = %q, want read", got)
	}
}

func TestSendPushesToLiveQueueingHarness(t *testing.T) {
	a, fake := newTestApp(t, true)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "fake", "turn-start")

	t.Setenv("RECADO_ADDR", "asker")
	out := mustExec(t, a, "", "send", "recado://local/worker", "hi", "--kind", "complete", "--meta", "report=.agents/reports/x.md", "--json")
	got := decode[sent](t, out)
	if got.Short == "" || !got.Pushed || got.To != "recado://local/worker" {
		t.Errorf("send --json = %+v", got)
	}
	pushes, err := fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 || pushes[0].Meta["report"] != ".agents/reports/x.md" || pushes[0].Kind != "complete" {
		t.Fatalf("pushed = %+v, want the complete message", pushes)
	}
}

func TestSendReadsStdin(t *testing.T) {
	a, fake := newTestApp(t, true)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "asker")
	mustExec(t, a, "a body\nfrom stdin\n", "send", "worker", "-")
	pushes, err := fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 || pushes[0].Body != "a body\nfrom stdin" {
		t.Fatalf("pushed = %+v, want the stdin body", pushes)
	}
}

func TestRepliesReachTheRightAsker(t *testing.T) {
	a, fake := newTestApp(t, true)
	startSession(t, a, "worker")
	startSession(t, a, "asker-a")
	startSession(t, a, "asker-b")

	type question struct{ id, short string }
	ask := func(asker, body string) question {
		t.Setenv("RECADO_ADDR", asker)
		out := decode[sent](t, mustExec(t, a, "", "ask", "worker", body, "--meta", "item="+asker, "--json"))
		return question{id: out.ID, short: out.Short}
	}
	qA := ask("asker-a", "a asks")
	qB := ask("asker-b", "b asks")
	if qA.short == qB.short {
		t.Fatalf("both questions got short id %q", qA.short)
	}

	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "reply", qA.short, "answer a")
	mustExec(t, a, "", "reply", qB.short, "answer b")

	// The harness took both answers, so no inbox peek is needed for delivery;
	// the mailboxes still show which answer each asker got.
	type inbox struct {
		Messages []message `json:"messages"`
	}
	answers := map[string]inbox{}
	for _, asker := range []string{"asker-a", "asker-b"} {
		t.Setenv("RECADO_ADDR", asker)
		answers[asker] = decode[inbox](t, mustExec(t, a, "", "inbox", "--peek", "--json"))
	}
	for _, c := range []struct {
		asker, body string
		q           question
	}{{"asker-a", "answer a", qA}, {"asker-b", "answer b", qB}} {
		msgs := answers[c.asker].Messages
		if len(msgs) != 1 {
			t.Fatalf("%s has %d messages, want one answer", c.asker, len(msgs))
		}
		answer := msgs[0].Envelope
		if answer.Body != c.body || answer.Kind != "answer" {
			t.Errorf("%s got %q (%s), want %q", c.asker, answer.Body, answer.Kind, c.body)
		}
		if answer.To.String() != "recado://local/"+c.asker {
			t.Errorf("%s answer went to %s", c.asker, answer.To)
		}
		if answer.ReplyTo != c.q.id {
			t.Errorf("%s answer reply_to = %s, want %s", c.asker, answer.ReplyTo, c.q.id)
		}
		if answer.Meta["item"] != c.asker {
			t.Errorf("%s answer meta = %v, want the question's meta copied", c.asker, answer.Meta)
		}
	}

	pushes, err := fake.Pushes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 4 { // two questions and two answers
		t.Errorf("harness saw %d pushes, want 4", len(pushes))
	}
}

func TestInboxClaimPeekAndFilter(t *testing.T) {
	a, _ := newTestApp(t, false)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "fake", "turn-start")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "ask", "worker", "q", "--meta", "item=login"))
	t.Setenv("RECADO_ADDR", "worker")

	peeked := mustExec(t, a, "", "inbox", "--peek")
	if !strings.Contains(peeked, "[recado] from asker · question · item=login (id "+short+")") {
		t.Errorf("inbox --peek = %q", peeked)
	}
	if !strings.Contains(peeked, `reply with: recado reply `+short) {
		t.Errorf("inbox --peek lost the reply hint: %q", peeked)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); !strings.HasSuffix(got, " stored") {
		t.Errorf("status after peek = %q, want still stored", got)
	}

	if got := strings.TrimSpace(mustExec(t, a, "", "inbox", "--meta", "item=other")); got != "" {
		t.Errorf("inbox --meta item=other = %q, want nothing", got)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "inbox", "--as", "worker")); !strings.Contains(got, "which") && !strings.Contains(got, "q") {
		t.Errorf("inbox --as worker = %q", got)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); !strings.HasSuffix(got, " queued") {
		t.Errorf("status after inbox = %q, want queued: inbox claimed it", got)
	}

	mustExec(t, a, "", "hook", "fake", "turn-start")
	if got := strings.TrimSpace(mustExec(t, a, "", "inbox")); got != "" {
		t.Errorf("inbox lists read messages by default: %q", got)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "inbox", "--all")); !strings.Contains(got, "q") {
		t.Errorf("inbox --all = %q, want the read message", got)
	}
}

func TestDrainPrintsWhatItCouldNotPush(t *testing.T) {
	a, _ := newTestApp(t, false)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "fake", "turn-start")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "ask", "worker", "q"))

	setAgentPID(t, a, "worker", 0) // no push can be attempted
	t.Setenv("RECADO_ADDR", "worker")
	out := mustExec(t, a, "", "hook", "fake", "turn-end")
	if !strings.Contains(out, "[recado] from asker · question (id "+short+")") {
		t.Errorf("hook turn-end = %q, want the message the shim must deliver", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(mustExec(t, a, "", "status", short)), " queued") {
		t.Error("a drained message was not claimed")
	}
	if out := mustExec(t, a, "", "hook", "fake", "turn-end"); out != "" {
		t.Errorf("second turn-end printed %q; it must not deliver the message again", out)
	}
}

func TestFixtureIgnoresXDGMachineRegistry(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	path := filepath.Join(config, "recado", "machines.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[machines.rpi]\nkind = \"docker\"\ntarget = \"rpi\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "transport-called")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ntouch \""+marker+"\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	a, _ := newTestApp(t, false)
	t.Setenv("RECADO_ADDR", "worker")
	out := decode[struct {
		Machines []machine.Machine `json:"machines"`
	}](t, mustExec(t, a, "", "machine", "ls", "--json"))
	if len(out.Machines) != 0 {
		t.Errorf("fixture listed external machines: %+v", out.Machines)
	}
	if _, err := exec(t, a, "", "send", "rpi/worker", "hi"); exitCode(err) != 2 {
		t.Errorf("external machine: exit %d, want 2 (err %v)", exitCode(err), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("external transport was invoked or could not be checked: %v", err)
	}
}

func TestUnknownNamesExitTwo(t *testing.T) {
	a, _ := newTestApp(t, true)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "worker")

	cases := [][]string{
		{"send", "nobody", "hi"},
		{"ask", "nobody", "hi"},
		{"send", "rpi/worker", "hi"},
		{"reply", "abcd", "hi"},
		{"status", "abcd"},
		{"hook", "nope", "session-start"},
		{"hook", "fake", "nope"},
		{"send", "worker"},
		{"inbox", "--as", "rpi/worker"},
	}
	for _, args := range cases {
		if _, err := exec(t, a, "", args...); exitCode(err) != 2 {
			t.Errorf("recado %s: exit %d, want 2 (err %v)", strings.Join(args, " "), exitCode(err), err)
		}
	}
}

func TestNonLocalSenderIsUsageError(t *testing.T) {
	a, _ := newTestApp(t, false)
	t.Setenv("RECADO_ADDR", "rpi/sender")
	for _, args := range [][]string{
		{"reply", "abcd", "hi"},
		{"send", "recado+upstream://host/worker", "hi"},
		{"send", "other/worker", "hi"},
	} {
		if _, err := exec(t, a, "", args...); exitCode(err) != 2 {
			t.Errorf("recado %s: exit %d, want 2 (err %v)", strings.Join(args, " "), exitCode(err), err)
		}
	}
}

func TestDeliverFromStdin(t *testing.T) {
	a, fake := newTestApp(t, true)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "asker")

	// A finished envelope, as a remote send or the relay would hand it over.
	second, err := envelope.New(addr.Local("asker"), addr.Local("worker"), envelope.KindMessage, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	line, err := second.Line()
	if err != nil {
		t.Fatal(err)
	}
	out := mustExec(t, a, string(line), "deliver", "--json")
	got := decode[struct {
		Delivered []sent `json:"delivered"`
	}](t, out)
	if len(got.Delivered) != 1 || got.Delivered[0].ID != second.ID {
		t.Fatalf("deliver --json = %+v", got)
	}
	if pushes, _ := fake.Pushes(); len(pushes) != 1 {
		t.Errorf("pushes = %d, want 1", len(pushes))
	}

	// Delivering the same envelope again is a no-op, not a second message.
	if _, err := exec(t, a, string(line), "deliver"); err != nil {
		t.Errorf("deliver of a known envelope failed: %v", err)
	}
	if pushes, _ := fake.Pushes(); len(pushes) != 1 {
		t.Errorf("pushes after redelivery = %d, want 1", len(pushes))
	}
	if _, err := exec(t, a, `{"v":1}`, "deliver"); exitCode(err) != 2 {
		t.Errorf("deliver of a broken envelope: exit %d, want 2", exitCode(err))
	}
}

func TestHookRegistersAgent(t *testing.T) {
	a, _ := newTestApp(t, false)
	startSession(t, a, "sub-1")

	out := decode[struct {
		Agents []struct {
			ID      string `json:"id"`
			Harness string `json:"harness"`
			Live    bool   `json:"live"`
			State   string `json:"state"`
		} `json:"agents"`
	}](t, mustExec(t, a, "", "agents", "--json"))
	if len(out.Agents) != 1 {
		t.Fatalf("agents = %+v, want one", out.Agents)
	}
	got := out.Agents[0]
	if got.ID != "sub-1" || got.Harness != "fake" || !got.Live {
		t.Errorf("agents = %+v, want sub-1, fake, live", got)
	}
	if got.State != "" {
		t.Errorf("state = %q, want never reported", got.State)
	}

	t.Setenv("RECADO_ADDR", "sub-1")
	mustExec(t, a, "", "hook", "fake", "turn-start")
	if !strings.Contains(mustExec(t, a, "", "agents"), "busy") {
		t.Error("agents does not show the agent busy")
	}
	mustExec(t, a, "", "hook", "fake", "turn-end")
	if !strings.Contains(mustExec(t, a, "", "agents"), "idle") {
		t.Error("agents does not show the agent idle")
	}
}

func TestAgentsJSONShowsCWD(t *testing.T) {
	a, _ := newTestApp(t, false)
	agentsDir := filepath.Join(a.Root, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	records := map[string]string{
		"with-cwd": `{"harness":"fake","endpoint":"/secret/endpoint.sock","token":"secret-token","pid":1,"cwd":"/work/tree","started":"2026-01-01T00:00:00Z"}`,
		"old":      `{"harness":"fake","endpoint":"/secret/endpoint.sock","token":"secret-token","pid":1,"started":"2026-01-01T00:00:00Z"}`,
	}
	for id, record := range records {
		if err := os.WriteFile(filepath.Join(agentsDir, id+".json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	raw := mustExec(t, a, "", "agents", "--json")
	out := decode[struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}](t, raw)
	byID := map[string]map[string]json.RawMessage{}
	for _, ag := range out.Agents {
		byID[strings.Trim(string(ag["id"]), `"`)] = ag
	}
	if got := string(byID["with-cwd"]["cwd"]); got != `"/work/tree"` {
		t.Errorf("with-cwd cwd = %s, want \"/work/tree\"", got)
	}
	if byID["old"] == nil {
		t.Fatalf("agents = %s, want a record for old", raw)
	}
	if got, ok := byID["old"]["cwd"]; ok {
		t.Errorf("old record has cwd %s, want the key absent", got)
	}
	for _, private := range []string{"endpoint", "token", "/secret/endpoint.sock", "secret-token"} {
		if strings.Contains(raw, private) {
			t.Errorf("agents --json exposes %q: %s", private, raw)
		}
	}
}

// writeAgent puts a raw registry record in the state root, the way an older
// recado version or a hand edit would.
func writeAgent(t *testing.T, a *app, id, record string) {
	t.Helper()
	dir := filepath.Join(a.Root, "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
}

// agentsByID lists agents as raw JSON keyed by id.
func agentsByID(t *testing.T, a *app) map[string]map[string]json.RawMessage {
	t.Helper()
	out := decode[struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}](t, mustExec(t, a, "", "agents", "--json"))
	byID := map[string]map[string]json.RawMessage{}
	for _, ag := range out.Agents {
		byID[strings.Trim(string(ag["id"]), `"`)] = ag
	}
	return byID
}

func TestHookRecordsRexSessionAndBlock(t *testing.T) {
	a, _ := newTestApp(t, false)
	t.Setenv("REX_SESSION", "session:abc")
	t.Setenv("REX_BLOCK", "block-1")
	startSession(t, a, "sub-1")

	byID := agentsByID(t, a)
	if got := string(byID["sub-1"]["rex_session"]); got != `"session:abc"` {
		t.Errorf("rex_session = %s, want \"session:abc\"", got)
	}
	if got := string(byID["sub-1"]["rex_block"]); got != `"block-1"` {
		t.Errorf("rex_block = %s, want \"block-1\"", got)
	}

	t.Setenv("REX_SESSION", "")
	t.Setenv("REX_BLOCK", "")
	startSession(t, a, "sub-2")
	for _, key := range []string{"rex_session", "rex_block"} {
		if raw, ok := agentsByID(t, a)["sub-2"][key]; ok {
			t.Errorf("sub-2 records %s=%s with Rex unset", key, raw)
		}
	}
}

func TestAgentsShowRexLabel(t *testing.T) {
	a, _ := newTestApp(t, false)
	writeAgent(t, a, "sub-1", `{"harness":"fake","pid":1,"rex_session":"session:abc"}`)
	writeAgent(t, a, "old", `{"harness":"fake","pid":1}`)
	a.rexList = func() ([]byte, error) {
		return []byte(`{"sessions":[{"session_id":"session:abc","label":"personal_sb"},{"session_id":"session:def","label":"elsewhere"}]}`), nil
	}

	text := mustExec(t, a, "", "agents")
	if !strings.Contains(text, "rex=personal_sb") {
		t.Errorf("agents = %q, want rex=personal_sb", text)
	}
	byID := agentsByID(t, a)
	if got := string(byID["sub-1"]["rex_label"]); got != `"personal_sb"` {
		t.Errorf("sub-1 rex_label = %s, want \"personal_sb\"", got)
	}
	if got, ok := byID["old"]["rex_label"]; ok {
		t.Errorf("an entry with no rex_session got rex_label %s", got)
	}
}

func TestAgentsRexLabelFailureIsSilent(t *testing.T) {
	a, _ := newTestApp(t, false)
	writeAgent(t, a, "sub-1", `{"harness":"fake","pid":1,"rex_session":"session:abc"}`)
	stubs := map[string]func() ([]byte, error){
		"rex failed":       func() ([]byte, error) { return nil, errors.New("exec: rex: not found") },
		"rex printed junk": func() ([]byte, error) { return []byte("not json"), nil },
	}
	for name, stub := range stubs {
		t.Run(name, func(t *testing.T) {
			a.rexList = stub
			if out := mustExec(t, a, "", "agents", "--json"); strings.Contains(out, "rex_label") {
				t.Errorf("agents --json = %s, want no rex_label", out)
			}
			if out := mustExec(t, a, "", "agents"); strings.Contains(out, "rex=") {
				t.Errorf("agents = %q, want no rex", out)
			}
		})
	}
}

func TestAgentsCallRexOnlyWhenAnEntryHasASession(t *testing.T) {
	a, _ := newTestApp(t, false)
	writeAgent(t, a, "old", `{"harness":"fake","pid":1}`)
	called := false
	a.rexList = func() ([]byte, error) { called = true; return nil, nil }
	mustExec(t, a, "", "agents")
	if called {
		t.Error("agents ran rex with no rex_session to resolve")
	}
}

func TestAgentsFilterByRexSession(t *testing.T) {
	a, _ := newTestApp(t, false)
	writeAgent(t, a, "sub-1", `{"harness":"fake","pid":1,"rex_session":"session:abc"}`)
	writeAgent(t, a, "sub-2", `{"harness":"fake","pid":1,"rex_session":"session:def"}`)
	a.rexList = func() ([]byte, error) {
		return []byte(`{"sessions":[{"session_id":"session:abc","label":"personal_sb"},{"session_id":"session:def","label":"other"}]}`), nil
	}

	for _, query := range []string{"personal_sb", "session:abc"} {
		out := mustExec(t, a, "", "agents", "--rex-session", query, "--json")
		if !strings.Contains(out, `"sub-1"`) || strings.Contains(out, `"sub-2"`) {
			t.Errorf("agents --rex-session %s --json = %s, want only sub-1", query, out)
		}
		text := mustExec(t, a, "", "agents", "--rex-session", query)
		if !strings.Contains(text, "sub-1") || strings.Contains(text, "sub-2") {
			t.Errorf("agents --rex-session %s = %q, want only sub-1", query, text)
		}
	}
	if out := mustExec(t, a, "", "agents", "--rex-session", "nobody", "--json"); !strings.Contains(out, `"agents": []`) {
		t.Errorf("agents --rex-session nobody --json = %s, want no agents", out)
	}
}

func TestUsageErrorExitCode(t *testing.T) {
	a, _ := newTestApp(t, false)
	if _, err := exec(t, a, "", "nonsense"); exitCode(err) != 2 {
		t.Errorf("unknown command: exit %d, want 2", exitCode(err))
	}
	if _, err := exec(t, a, "", "send", "--nope"); exitCode(err) != 2 {
		t.Errorf("unknown flag: exit %d, want 2", exitCode(err))
	}
	if _, err := exec(t, a, "", "send", "worker", "hi", "--meta", "nometa"); exitCode(err) != 2 {
		t.Errorf("bad --meta: exit %d, want 2", exitCode(err))
	}
}

func TestClassifyInternalError(t *testing.T) {
	if got := exitCode(classify(errors.New("disk on fire"))); got != 1 {
		t.Errorf("plain error exit = %d, want 1", got)
	}
	if got := exitCode(nil); got != 0 {
		t.Errorf("nil error exit = %d, want 0", got)
	}
}

func TestHookSessionStartRecordsTheEndpoint(t *testing.T) {
	a, _ := newTestApp(t, true)
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "fake", "session-start", "--endpoint", "/tmp/recado-pi.sock")

	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Endpoint != "/tmp/recado-pi.sock" {
		t.Errorf("endpoint = %q, want the socket the shim pushed", agent.Endpoint)
	}
}

// setAgentPID controls whether a test's registered agent is live.
func setAgentPID(t *testing.T, a *app, id string, pid int) int {
	t.Helper()
	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.Lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	old := agent.PID
	agent.PID = pid
	if err := st.Register(agent); err != nil {
		t.Fatal(err)
	}
	return old
}

func TestTurnEndJSONCarriesTheTextTheShimDelivers(t *testing.T) {
	a, _ := newTestApp(t, false)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "send", "worker", "one\ntwo"))

	setAgentPID(t, a, "worker", 0) // no push can be attempted; the shim takes the message
	t.Setenv("RECADO_ADDR", "worker")
	out := mustExec(t, a, "", "hook", "fake", "turn-end", "--json")
	got := decode[struct {
		Pending []message `json:"pending"`
	}](t, out)
	if len(got.Pending) != 1 {
		t.Fatalf("turn-end --json pending = %+v, want one message", got.Pending)
	}
	want := "[recado] from asker · message (id " + short + ")\none\ntwo"
	if got.Pending[0].Text != want {
		t.Errorf("pending text = %q, want %q", got.Pending[0].Text, want)
	}
}

// brokenWriter rejects every write, like a closed stdout.
type brokenWriter struct{ err error }

func (w brokenWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDrainReleasesMessagesWhenItsOutputFails(t *testing.T) {
	a, fake := newTestApp(t, false)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "send", "worker", "one"))
	pid := setAgentPID(t, a, "worker", 0) // leave the message for the shim's output
	t.Setenv("RECADO_ADDR", "worker")

	root := newRootCmd(a)
	root.SetOut(brokenWriter{errors.New("stdout closed")})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"hook", "fake", "turn-end", "--json"})
	if err := root.Execute(); err == nil {
		t.Fatal("hook succeeded though its output failed")
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Fatalf("after the failed output the message is %q, want it stored for a later drain", got)
	}

	setAgentPID(t, a, "worker", pid)
	mustExec(t, a, "", "hook", "fake", "turn-end")
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" queued" {
		t.Fatalf("the next drain left the message %q, want it queued to the agent", got)
	}
	pushes, err := fake.Pushes()
	if err != nil || len(pushes) != 1 {
		t.Fatalf("pushes = %d, %v; want the message handed over exactly once", len(pushes), err)
	}
}

// lineWith returns the output line mentioning part.
func lineWith(out, part string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, part) {
			return line
		}
	}
	return ""
}

func TestSkillManagesThePiExtension(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &app{Root: filepath.Join(t.TempDir(), "state"), Harnesses: harness.Set{&harness.Pi{Home: home}}}
	shimPath := filepath.Join(home, ".pi", "agent", "extensions", "recado.ts")

	if out := mustExec(t, a, "", "skill", "setup"); !strings.Contains(lineWith(out, shimPath), "installed") {
		t.Errorf("skill setup = %q, want the Pi extension installed", out)
	}
	data, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `// recado-shim: "4"`+"\n") {
		t.Errorf("the installed extension is not version-stamped: %q", string(data[:40]))
	}
	if out := mustExec(t, a, "", "skill", "status"); !strings.Contains(lineWith(out, shimPath), "current") {
		t.Errorf("skill status = %q, want the Pi extension current", out)
	}

	if out := mustExec(t, a, "", "skill", "remove"); !strings.Contains(lineWith(out, shimPath), "removed") {
		t.Errorf("skill remove = %q, want the Pi extension removed", out)
	}
	if _, err := os.Stat(shimPath); !os.IsNotExist(err) {
		t.Errorf("the Pi extension is still there: %v", err)
	}
	if out := mustExec(t, a, "", "skill", "status"); !strings.Contains(lineWith(out, shimPath), "missing") {
		t.Errorf("skill status = %q, want the Pi extension missing", out)
	}
}

func TestSkillLeavesAForeignPiExtensionAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &app{Root: filepath.Join(t.TempDir(), "state"), Harnesses: harness.Set{&harness.Pi{Home: home}}}
	shimPath := filepath.Join(home, ".pi", "agent", "extensions", "recado.ts")
	if err := os.MkdirAll(filepath.Dir(shimPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "export default function () { /* mine */ }\n"
	if err := os.WriteFile(shimPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := exec(t, a, "", "skill", "setup"); err == nil {
		t.Fatal("skill setup overwrote an extension recado did not install")
	}
	if _, err := exec(t, a, "", "skill", "remove"); err == nil {
		t.Fatal("skill remove deleted an extension recado did not install")
	}
	if got, err := os.ReadFile(shimPath); err != nil || string(got) != foreign {
		t.Errorf("the foreign extension changed: %q, %v", string(got), err)
	}
}

func TestSkillManagesTheOpenCodePlugin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &app{Root: filepath.Join(t.TempDir(), "state"), Harnesses: harness.Set{&harness.OpenCode{Home: home}}}
	pluginPath := filepath.Join(home, ".config", "opencode", "plugins", "recado.js")

	if out := mustExec(t, a, "", "skill", "setup"); !strings.Contains(lineWith(out, pluginPath), "installed") {
		t.Errorf("skill setup = %q, want the OpenCode plugin installed", out)
	}
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `// recado-shim: "2"`+"\n") {
		t.Errorf("the installed plugin is not version-stamped: %q", string(data[:40]))
	}
	if out := mustExec(t, a, "", "skill", "status"); !strings.Contains(lineWith(out, pluginPath), "current") {
		t.Errorf("skill status = %q, want the OpenCode plugin current", out)
	}

	if out := mustExec(t, a, "", "skill", "remove"); !strings.Contains(lineWith(out, pluginPath), "removed") {
		t.Errorf("skill remove = %q, want the OpenCode plugin removed", out)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Errorf("the OpenCode plugin is still there: %v", err)
	}
	if out := mustExec(t, a, "", "skill", "status"); !strings.Contains(lineWith(out, pluginPath), "missing") {
		t.Errorf("skill status = %q, want the OpenCode plugin missing", out)
	}
}

func TestSkillLeavesAForeignOpenCodePluginAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &app{Root: filepath.Join(t.TempDir(), "state"), Harnesses: harness.Set{&harness.OpenCode{Home: home}}}
	pluginPath := filepath.Join(home, ".config", "opencode", "plugins", "recado.js")
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "export default { id: \"mine\" }\n"
	if err := os.WriteFile(pluginPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := exec(t, a, "", "skill", "setup"); err == nil {
		t.Fatal("skill setup overwrote a plugin recado did not install")
	}
	if _, err := exec(t, a, "", "skill", "remove"); err == nil {
		t.Fatal("skill remove deleted a plugin recado did not install")
	}
	if got, err := os.ReadFile(pluginPath); err != nil || string(got) != foreign {
		t.Errorf("the foreign plugin changed: %q, %v", string(got), err)
	}
}

func TestAgentsShowsARetiredSessionGone(t *testing.T) {
	a, fake := newTestApp(t, false)
	fake.Single = true
	startSession(t, a, "old")
	startSession(t, a, "new")

	text := mustExec(t, a, "", "agents")
	if got := lineWith(text, "old"); !strings.Contains(got, "gone") {
		t.Errorf("agents line for the superseded id = %q, want gone", got)
	}
	if got := lineWith(text, "new"); !strings.Contains(got, "live") {
		t.Errorf("agents line for the current session = %q, want live", got)
	}
}
