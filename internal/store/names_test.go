package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeriveNameFromGitRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sb")
	work := filepath.Join(root, "internal", "store")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree marks its root with a .git file, not a directory.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DeriveName(work); got != "sb" {
		t.Errorf("DeriveName(%q) = %q, want the git work-tree root's name", work, got)
	}
}

func TestDeriveNameWithoutGitUsesCWD(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plain", "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DeriveName(dir); got != "sub" {
		t.Errorf("DeriveName(%q) = %q, want the directory's own name", dir, got)
	}
}

func TestDeriveNameSanitises(t *testing.T) {
	cases := []struct{ base, want string }{
		{"sb", "sb"},
		{"SB", "sb"},
		{"my repo", "my-repo"},
		{"repo: v2", "repo-v2"},
		{"sub.1_x", "sub.1_x"},
		{".config", "config"},
		{"...", ""},
	}
	for _, c := range cases {
		if c.base == "" {
			continue // filepath.Join drops it
		}
		if got := DeriveName(filepath.Join("/tmp/x", c.base)); got != c.want {
			t.Errorf("DeriveName(base %q) = %q, want %q", c.base, got, c.want)
		}
	}
	if got := DeriveName(""); got != "" {
		t.Errorf("DeriveName(\"\") = %q, want no name", got)
	}
}

// agent builds a registry entry without writing it.
func agent(id, name string, pid int) Agent {
	return Agent{ID: id, Name: name, Harness: "fake", PID: pid, CWD: "/work/" + id, Started: time.Now().UTC()}
}

func TestNamesSuffixLiveAgentsSharingABase(t *testing.T) {
	agents := []Agent{
		agent("sub-4123", "sb", os.Getpid()),
		agent("sub-4188", "sb", os.Getpid()),
		agent("sub-9999", "sb", deadPID), // gone: shows its base, forces no suffix
		agent("manager-1", "manager", os.Getpid()),
		agent("nameless", "", os.Getpid()),
	}
	nameAgents(agents)
	want := map[string]string{
		"sub-4123":  "sb-23",
		"sub-4188":  "sb-88",
		"sub-9999":  "sb",
		"manager-1": "manager",
		"nameless":  "",
	}
	for _, a := range agents {
		if a.Display != want[a.ID] {
			t.Errorf("%s displays as %q, want %q", a.ID, a.Display, want[a.ID])
		}
	}
}

func TestNamesGrowTheSuffixUntilUnique(t *testing.T) {
	agents := []Agent{
		agent("sub-xa", "sb", os.Getpid()),
		agent("cob-xa", "sb", os.Getpid()),
	}
	nameAgents(agents)
	if agents[0].Display != "sb-ub-xa" || agents[1].Display != "sb-ob-xa" {
		t.Errorf("displays = %q and %q, want the shortest telling suffixes", agents[0].Display, agents[1].Display)
	}
}

func TestDisplayedNameIsNotStored(t *testing.T) {
	s, root := open(t)
	if err := s.Register(agent("sub-4123", "sb", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Display != "sb" {
		t.Fatalf("List = %+v, want one agent displayed as sb", list)
	}
	data, err := os.ReadFile(filepath.Join(root, "agents", "sub-4123.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "display") {
		t.Errorf("registry entry stored the displayed name: %s", data)
	}
}

func TestResolveRecipientExactIDWins(t *testing.T) {
	s, _ := open(t)
	// A gone entry whose id is someone else's name still wins: ids are exact.
	if err := s.Register(agent("sb", "old", deadPID)); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(agent("sub-4123", "sb", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveRecipient("sb")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "sb" {
		t.Errorf("ResolveRecipient(sb) = %q, want the registered id sb", got.ID)
	}
}

func TestResolveRecipientByName(t *testing.T) {
	s, _ := open(t)
	if err := s.Register(agent("sub-4123", "sb", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveRecipient("sb")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "sub-4123" {
		t.Errorf("ResolveRecipient(sb) = %q, want the one live agent's id", got.ID)
	}
	if _, err := s.ResolveRecipient("nobody"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("ResolveRecipient(nobody) = %v, want ErrUnknownAgent", err)
	}
}

func TestResolveRecipientAmbiguousName(t *testing.T) {
	s, _ := open(t)
	if err := s.Register(agent("sub-4123", "sb", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(agent("sub-4188", "sb", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	_, err := s.ResolveRecipient("sb")
	if !errors.Is(err, ErrAmbiguousName) {
		t.Fatalf("ResolveRecipient(sb) = %v, want ErrAmbiguousName", err)
	}
	if !strings.Contains(err.Error(), "sb-23 sub-4123 /work/sub-4123") ||
		!strings.Contains(err.Error(), "sb-88 sub-4188 /work/sub-4188") {
		t.Errorf("ambiguity error = %q, want each match as name id cwd", err)
	}

	// The displayed form names exactly one agent.
	got, err := s.ResolveRecipient("sb-23")
	if err != nil {
		t.Fatalf("ResolveRecipient(sb-23): %v", err)
	}
	if got.ID != "sub-4123" {
		t.Errorf("ResolveRecipient(sb-23) = %q, want sub-4123", got.ID)
	}
}
