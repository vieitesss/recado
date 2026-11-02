package main

import (
	"context"
	"fmt"
	"os"
	osExec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/machine"
)

func TestMachineCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, _ := newTestApp(t, false)
	a.Machines = nil // exercise the XDG registry
	added := decode[machine.Machine](t, mustExec(t, a, "", "machine", "add", "rpi", "ssh", "pi@rpi", "--json"))
	if added.Name != "rpi" || added.Kind != "ssh" || added.Target != "pi@rpi" {
		t.Fatalf("machine add --json = %+v", added)
	}
	got := decode[struct {
		Machines []machine.Machine `json:"machines"`
	}](t, mustExec(t, a, "", "machine", "ls", "--json"))
	if len(got.Machines) != 1 || got.Machines[0] != (machine.Machine{Name: "rpi", Kind: "ssh", Target: "pi@rpi"}) {
		t.Fatalf("machine ls --json = %+v", got)
	}
	if out := mustExec(t, a, "", "machine", "ls"); !strings.Contains(out, "rpi") || !strings.Contains(out, "pi@rpi") {
		t.Errorf("machine ls = %q", out)
	}
	removed := decode[struct {
		Name string `json:"name"`
	}](t, mustExec(t, a, "", "machine", "rm", "rpi", "--json"))
	if removed.Name != "rpi" {
		t.Fatalf("machine rm --json = %+v", removed)
	}
	if _, err := exec(t, a, "", "machine", "rm", "rpi"); exitCode(err) != 2 {
		t.Errorf("removing unknown machine: %v", err)
	}
}

func TestMachineConcurrentAddsAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "recado")
	if out, err := osExec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	config := filepath.Join(dir, "config")
	const count = 40
	start := make(chan struct{})
	errs := make(chan error, count)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("machine-%03d", i)
		go func() {
			<-start
			cmd := osExec.CommandContext(ctx, bin, "machine", "add", name, "ssh", name)
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+config)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("%s: %w: %s", name, err, out)
				return
			}
			errs <- nil
		}()
	}
	close(start)
	for i := 0; i < count; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	r := &machine.Registry{Path: filepath.Join(config, "recado", "machines.toml"), Kinds: machine.DefaultKinds()}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count {
		t.Fatalf("got %d machines after %d successful CLI adds", len(got), count)
	}
	for i, m := range got {
		name := fmt.Sprintf("machine-%03d", i)
		if m != (machine.Machine{Name: name, Kind: "ssh", Target: name}) {
			t.Errorf("machine %d = %+v, want %s", i, m, name)
		}
	}
}

// The transport invokes the test binary as a separate remote recado process.
func TestRemoteDeliverHelper(t *testing.T) {
	if os.Getenv("RECADO_TEST_REMOTE") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 3 || args[1] != "recado" {
		fmt.Fprintln(os.Stderr, "bad remote command")
		os.Exit(1)
	}
	root := newRootCmd(newApp())
	root.SetArgs(args[2:])
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
	os.Exit(0)
}

// remoteTestRegistry runs remote commands in a separate test process.
func remoteTestRegistry(t *testing.T) *machine.Registry {
	t.Helper()
	r := &machine.Registry{Path: filepath.Join(t.TempDir(), "machines.toml"), Kinds: map[string]machine.Kind{
		"fake": func(string) []string { return []string{os.Args[0], "-test.run=^TestRemoteDeliverHelper$", "--"} },
	}}
	if err := r.Add("rpi", "fake", "rpi"); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRemoteSendDeliversRewrittenEnvelope(t *testing.T) {
	remote, _ := newTestApp(t, false)
	startSession(t, remote, "sub-1")
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	t.Setenv("RECADO_ADDR", "manager")
	local, _ := newTestApp(t, false)
	local.Machines = remoteTestRegistry(t)
	out := mustExec(t, local, "", "send", "recado://rpi/sub-1", "hello", "--kind", "question", "--meta", "item=x", "--json")
	result := decode[sent](t, out)
	if result.To != "recado://rpi/sub-1" || result.Short == "" {
		t.Fatalf("send result = %+v", result)
	}
	t.Setenv("RECADO_ADDR", "sub-1")
	inbox := decode[struct {
		Messages []message `json:"messages"`
	}](t, mustExec(t, remote, "", "inbox", "--peek", "--json"))
	if len(inbox.Messages) != 1 {
		t.Fatalf("inbox = %+v", inbox)
	}
	env := inbox.Messages[0].Envelope
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if env.To.String() != "recado://local/sub-1" || env.From.String() != "recado+upstream://"+host+"/manager" || env.Body != "hello" || env.Kind != "question" || env.Meta["item"] != "x" || env.ID != result.ID || inbox.Messages[0].Short != result.Short {
		t.Errorf("remote envelope = %+v, result = %+v", env, result)
	}
	mustExec(t, remote, "", "reply", result.Short, "answer")
	data, err := os.ReadFile(filepath.Join(remote.Root, "outbox.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	answer := decode[struct {
		From    string `json:"from"`
		To      string `json:"to"`
		ReplyTo string `json:"reply_to"`
		Body    string `json:"body"`
	}](t, string(data))
	if answer.From != "recado://local/sub-1" || answer.To != "recado+upstream://"+host+"/manager" || answer.ReplyTo != result.ID || answer.Body != "answer" {
		t.Fatalf("outbox answer = %+v", answer)
	}
}

func TestSendUnknownMachineListsNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RECADO_ADDR", "manager")
	a, _ := newTestApp(t, false)
	a.Machines = nil // use this test's XDG registry for both machine add and send
	mustExec(t, a, "", "machine", "add", "web", "docker", "web")
	mustExec(t, a, "", "machine", "add", "rpi", "ssh", "rpi")
	_, err := exec(t, a, "", "send", "nas/worker", "hello")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "nas") || !strings.Contains(err.Error(), "rpi, web") {
		t.Errorf("unknown machine: %v", err)
	}
}

func TestMissingRemoteRecadoFailsImmediately(t *testing.T) {
	t.Setenv("RECADO_ADDR", "manager")
	a, _ := newTestApp(t, false)
	r := &machine.Registry{Path: filepath.Join(t.TempDir(), "machines.toml"), Kinds: map[string]machine.Kind{
		"fake": func(string) []string { return []string{"sh", "-c", "echo 'recado: command not found' >&2; exit 127"} },
	}}
	if err := r.Add("rpi", "fake", "rpi"); err != nil {
		t.Fatal(err)
	}
	a.Machines = r
	_, err := exec(t, a, "", "send", "rpi/worker", "hi")
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "recado is not installed") {
		t.Errorf("missing remote recado: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "outbox.jsonl")); !os.IsNotExist(err) {
		t.Errorf("unexpected outbox: %v", err)
	}
}

func TestMachineAddRejectsInvalidArgumentsAsUsage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, _ := newTestApp(t, false)
	a.Machines = nil // exercise the XDG registry
	for _, args := range [][]string{{"local", "ssh", "host"}, {"rpi", "bad", "host"}, {"rpi", "ssh", "-oBad"}} {
		_, err := exec(t, a, "", append([]string{"machine", "add"}, args...)...)
		if exitCode(err) != 2 {
			t.Errorf("machine add %v: exit %d: %v", args, exitCode(err), err)
		}
	}
}

func TestRemoteUnknownAgentIsAnUnknownAddress(t *testing.T) {
	t.Setenv("RECADO_ADDR", "manager")
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", filepath.Join(t.TempDir(), "remote"))
	a, _ := newTestApp(t, false)
	a.Machines = remoteTestRegistry(t)
	_, err := exec(t, a, "", "send", "rpi/nobody", "hi")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("remote unknown agent: exit %d: %v", exitCode(err), err)
	}
}

func TestTransportFailureDoesNotQueue(t *testing.T) {
	t.Setenv("RECADO_ADDR", "manager")
	a, _ := newTestApp(t, false)
	r := &machine.Registry{Path: filepath.Join(t.TempDir(), "machines.toml"), Kinds: map[string]machine.Kind{
		"fake": func(string) []string { return []string{"sh", "-c", "echo connection-refused >&2; exit 255"} },
	}}
	if err := r.Add("rpi", "fake", "rpi"); err != nil {
		t.Fatal(err)
	}
	a.Machines = r
	_, err := exec(t, a, "", "send", "rpi/worker", "hi")
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "connection-refused") {
		t.Errorf("transport error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "outbox.jsonl")); !os.IsNotExist(err) {
		t.Errorf("unexpected outbox: %v", err)
	}
}

func TestDirectSendToUpstream(t *testing.T) {
	t.Setenv("RECADO_ADDR", "worker")
	a, _ := newTestApp(t, false)
	sent := decode[sent](t, mustExec(t, a, "", "send", "recado+upstream://host/manager", "hi", "--json"))
	if sent.Short != strings.ToLower(sent.ID) {
		t.Fatalf("upstream id = %q, want full lowercase %q", sent.Short, strings.ToLower(sent.ID))
	}
	data, err := os.ReadFile(filepath.Join(a.Root, "outbox.jsonl"))
	if err != nil || !strings.Contains(string(data), `"body":"hi"`) {
		t.Fatalf("outbox = %s, %v", data, err)
	}
}
