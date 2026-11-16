package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/machine"
)

// agentEntry is one entry of `recado agents --json`.
type agentEntry struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	Name       string `json:"name"`
	Harness    string `json:"harness"`
	PID        int    `json:"pid"`
	Live       bool   `json:"live"`
	State      string `json:"state"`
	CWD        string `json:"cwd"`
	RexSession string `json:"rex_session"`
	RexBlock   string `json:"rex_block"`
	RexLabel   string `json:"rex_label"`
}

type agentList struct {
	Agents []agentEntry `json:"agents"`
}

func listAgents(t *testing.T, out string) agentList {
	t.Helper()
	return decode[agentList](t, out)
}

func TestAgentsAddressIsTheEntrysRecipient(t *testing.T) {
	a, _ := newTestApp(t, false)
	registerNamed(t, a, "sub-4123", "sb", "/work/recado")

	local := listAgents(t, mustExec(t, a, "", "agents", "--json"))
	if len(local.Agents) != 1 || local.Agents[0].Address != "recado://local/sub-4123" {
		t.Fatalf("agents --json = %+v, want one entry addressed recado://local/sub-4123", local.Agents)
	}
	if viaLocal := mustExec(t, a, "", "agents", "--machine", "local", "--json"); viaLocal != mustExec(t, a, "", "agents", "--json") {
		t.Errorf("--machine local = %s, want the local listing", viaLocal)
	}
}

// remotePair is a local app that reaches rpi, and the app standing in for rpi.
func remotePair(t *testing.T) (local, remote *app) {
	t.Helper()
	remote, _ = newTestApp(t, false)
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	t.Setenv("RECADO_ADDR", "manager")
	local, _ = newTestApp(t, false)
	local.Machines = remoteTestRegistry(t)
	return local, remote
}

func TestRemoteAgentsListsTheRemoteRegistry(t *testing.T) {
	local, remote := remotePair(t)
	registerNamed(t, remote, "sub-4123", "sb", "/work/recado")
	registerNamed(t, local, "sub-4123", "sb", "/work/recado")

	remoteJSON := listAgents(t, mustExec(t, local, "", "agents", "--machine", "rpi", "--json"))
	want := listAgents(t, mustExec(t, local, "", "agents", "--json"))
	if len(remoteJSON.Agents) != 1 || len(want.Agents) != 1 {
		t.Fatalf("agents on rpi = %+v, local = %+v", remoteJSON.Agents, want.Agents)
	}
	got := remoteJSON.Agents[0]
	if got.Address != "recado://rpi/sub-4123" || got.ID != "sub-4123" || got.Name != "sb" || got.Harness != "fake" || got.CWD != "/work/recado" || !got.Live {
		t.Errorf("remote entry = %+v, want the remote's registry entry addressed on rpi", got)
	}
	// Everything but the address is what this machine's listing says, because
	// the remote computed its own liveness, name and cwd.
	want.Agents[0].Address = "recado://rpi/sub-4123"
	if got != want.Agents[0] {
		t.Errorf("remote entry = %+v, want %+v", got, want.Agents[0])
	}

	if remoteText, localText := mustExec(t, local, "", "agents", "--machine", "rpi"), mustExec(t, local, "", "agents"); remoteText != localText {
		t.Errorf("agents --machine rpi text = %q, want the local text format %q", remoteText, localText)
	}
}

// cannedRemote is a machine whose remote recado prints payload exactly, the way
// an older or newer recado on the other side would.
func cannedRemote(t *testing.T, payload string) *machine.Registry {
	t.Helper()
	file := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &machine.Registry{Path: filepath.Join(t.TempDir(), "machines.toml"), Kinds: map[string]machine.Kind{
		"fake": func(string) []string { return []string{"sh", "-c", "cat '" + file + "'"} },
	}}
	if err := r.Add("rpi", "fake", "rpi"); err != nil {
		t.Fatal(err)
	}
	return r
}

// An older remote recado has no address, name or Rex keys: the listing still
// works, and every entry can go into send.
func TestRemoteAgentsAcceptsAnOlderPayload(t *testing.T) {
	a, _ := newTestApp(t, false)
	a.Machines = cannedRemote(t, `{"agents":[{"id":"sub-1","harness":"claude","pid":42,"live":true,"state":"idle"}]}`)

	got := listAgents(t, mustExec(t, a, "", "agents", "--machine", "rpi", "--json"))
	if len(got.Agents) != 1 {
		t.Fatalf("agents --machine rpi --json = %+v", got.Agents)
	}
	entry := got.Agents[0]
	if entry.ID != "sub-1" || entry.Address != "recado://rpi/sub-1" || entry.Harness != "claude" || entry.PID != 42 || !entry.Live || entry.State != "idle" {
		t.Errorf("entry = %+v, want the remote's fields with an address on rpi", entry)
	}
	text := mustExec(t, a, "", "agents", "--machine", "rpi")
	if !strings.HasPrefix(text, "sub-1                        live  claude     pid=42     state=idle") {
		t.Errorf("text = %q, want the local text format", text)
	}
}

// Filtering happens on the decoded entries, so the remote needs no flags.
func TestRemoteAgentsFiltersByRexAfterFetch(t *testing.T) {
	a, _ := newTestApp(t, false)
	a.Machines = cannedRemote(t, `{"agents":[
		{"id":"sub-1","address":"recado://local/sub-1","harness":"fake","pid":1,"live":true,"state":"idle","rex_session":"w1","rex_block":"b1","rex_label":"login"},
		{"id":"sub-2","address":"recado://local/sub-2","harness":"fake","pid":2,"live":false,"state":"","rex_session":"w2","rex_block":"b2","rex_label":"billing"}
	]}`)

	byLabel := listAgents(t, mustExec(t, a, "", "agents", "--machine", "rpi", "--rex-session", "billing", "--json"))
	if len(byLabel.Agents) != 1 || byLabel.Agents[0].ID != "sub-2" || byLabel.Agents[0].Address != "recado://rpi/sub-2" {
		t.Errorf("--rex-session billing = %+v, want only sub-2 on rpi", byLabel.Agents)
	}
	byID := listAgents(t, mustExec(t, a, "", "agents", "--machine", "rpi", "--rex-session", "w1", "--json"))
	if len(byID.Agents) != 1 || byID.Agents[0].RexSession != "w1" || byID.Agents[0].RexBlock != "b1" || byID.Agents[0].RexLabel != "login" {
		t.Errorf("--rex-session w1 = %+v, want the login entry with its label kept", byID.Agents)
	}
}

func TestRemoteAgentsUnknownMachineListsNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RECADO_ADDR", "manager")
	a, _ := newTestApp(t, false)
	a.Machines = nil // exercise the XDG registry
	mustExec(t, a, "", "machine", "add", "web", "docker", "web")
	mustExec(t, a, "", "machine", "add", "rpi", "ssh", "rpi")
	_, err := exec(t, a, "", "agents", "--machine", "nas")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "nas") || !strings.Contains(err.Error(), "rpi, web") {
		t.Errorf("unknown machine: exit %d: %v", exitCode(err), err)
	}
}

func TestRemoteAgentsWithoutRecadoFailsImmediately(t *testing.T) {
	a, _ := newTestApp(t, false)
	r := &machine.Registry{Path: filepath.Join(t.TempDir(), "machines.toml"), Kinds: map[string]machine.Kind{
		"fake": func(string) []string { return []string{"sh", "-c", "echo 'recado: command not found' >&2; exit 127"} },
	}}
	if err := r.Add("rpi", "fake", "rpi"); err != nil {
		t.Fatal(err)
	}
	a.Machines = r
	_, err := exec(t, a, "", "agents", "--machine", "rpi")
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "recado is not installed or not on PATH") {
		t.Errorf("missing remote recado: exit %d: %v", exitCode(err), err)
	}
}

func TestRemoteCommandsReportRejectedInput(t *testing.T) {
	t.Setenv("RECADO_ADDR", "manager")
	a, _ := newTestApp(t, false)
	r := &machine.Registry{Path: filepath.Join(t.TempDir(), "machines.toml"), Kinds: map[string]machine.Kind{
		"fake": func(string) []string { return []string{"sh", "-c", "echo 'invalid input' >&2; exit 2"} },
	}}
	if err := r.Add("rpi", "fake", "rpi"); err != nil {
		t.Fatal(err)
	}
	a.Machines = r
	for _, args := range [][]string{{"agents", "--machine", "rpi"}, {"send", "rpi/worker", "hi"}} {
		t.Run(args[0], func(t *testing.T) {
			_, err := exec(t, a, "", args...)
			if exitCode(err) != 2 || !errors.Is(err, deliver.ErrRemoteUsage) {
				t.Fatalf("remote input rejection: exit %d: %v, want ErrRemoteUsage and exit 2", exitCode(err), err)
			}
			if want := `remote recado rejected input on machine "rpi": invalid input`; err.Error() != want {
				t.Errorf("remote input rejection = %q, want %q", err, want)
			}
		})
	}
}

// An id the remote could not have registered is not an address send can use.
func TestRemoteAgentsRejectsAnInvalidID(t *testing.T) {
	a, _ := newTestApp(t, false)
	a.Machines = cannedRemote(t, `{"agents":[{"id":"../etc","harness":"fake","pid":1,"live":true}]}`)

	_, err := exec(t, a, "", "agents", "--machine", "rpi")
	if err == nil || !strings.Contains(err.Error(), "invalid agents response") || !strings.Contains(err.Error(), "../etc") {
		t.Errorf("invalid remote id: %v, want an invalid agents response naming the id", err)
	}
}

// A remote cwd lives under the remote's home, so the listing prints it whole.
func TestRemoteAgentsKeepsTheCWDAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "src", "recado")
	a, _ := newTestApp(t, false)
	a.Machines = cannedRemote(t, `{"agents":[{"id":"sub-1","harness":"fake","pid":1,"live":true,"state":"idle","cwd":"`+cwd+`"}]}`)

	if text := mustExec(t, a, "", "agents", "--machine", "rpi"); !strings.Contains(text, " cwd="+cwd) {
		t.Errorf("remote text = %q, want cwd=%s", text, cwd)
	}
	registerNamed(t, a, "sub-2", "sb", cwd)
	if text := mustExec(t, a, "", "agents"); !strings.Contains(text, " cwd=~/src/recado") {
		t.Errorf("local text = %q, want cwd=~/src/recado", text)
	}
}
