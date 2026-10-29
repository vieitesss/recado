package machine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	return &Registry{Path: filepath.Join(t.TempDir(), "recado", "machines.toml"), Kinds: DefaultKinds()}
}

func writeFile(t *testing.T, r *Registry, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.Path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListParsesAHandEditedFile(t *testing.T) {
	r := newRegistry(t)
	writeFile(t, r, `# my machines

[machines.rpi]
kind = "ssh"
target = "pi@rpi.lan"   # the garage one

[machines.web]
kind   = 'docker'
target = "web"
`)
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []Machine{{"rpi", "ssh", "pi@rpi.lan"}, {"web", "docker", "web"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %+v, want %+v", got, want)
	}
}

func TestListWithoutAFileIsEmpty(t *testing.T) {
	got, err := newRegistry(t).List()
	if err != nil || len(got) != 0 {
		t.Errorf("List() = %v, %v; want no machines and no error", got, err)
	}
}

func TestListRejectsBrokenFiles(t *testing.T) {
	cases := map[string]string{
		"unknown section": "[other.rpi]\nkind = \"ssh\"\ntarget = \"rpi\"\n",
		"unknown key":     "[machines.rpi]\nkind = \"ssh\"\ntarget = \"rpi\"\nport = \"22\"\n",
		"key outside":     "kind = \"ssh\"\n",
		"unquoted value":  "[machines.rpi]\nkind = ssh\ntarget = \"rpi\"\n",
		"missing target":  "[machines.rpi]\nkind = \"ssh\"\n",
		"missing kind":    "[machines.rpi]\ntarget = \"rpi\"\n",
		"unknown kind":    "[machines.rpi]\nkind = \"telnet\"\ntarget = \"rpi\"\n",
		"reserved name":   "[machines.local]\nkind = \"ssh\"\ntarget = \"rpi\"\n",
		"duplicate":       "[machines.rpi]\nkind = \"ssh\"\ntarget = \"a\"\n[machines.rpi]\nkind = \"ssh\"\ntarget = \"b\"\n",
		"bad name":        "[machines.a/b]\nkind = \"ssh\"\ntarget = \"rpi\"\n",
		"trailing junk":   "[machines.rpi]\nkind = \"ssh\" nope\ntarget = \"rpi\"\n",
		"option target":   "[machines.rpi]\nkind = \"ssh\"\ntarget = \"-oProxyCommand=x\"\n",
	}
	for name, content := range cases {
		r := newRegistry(t)
		writeFile(t, r, content)
		if _, err := r.List(); err == nil {
			t.Errorf("%s: List() accepted %q", name, content)
		} else if !strings.Contains(err.Error(), r.Path) {
			t.Errorf("%s: error %q does not name the file", name, err)
		}
	}
}

func TestAddAndRemoveRoundTrip(t *testing.T) {
	r := newRegistry(t)
	for _, m := range []Machine{{"web", "docker", "web"}, {"rpi", "ssh", "pi@rpi.lan"}} {
		if err := r.Add(m.Name, m.Kind, m.Target); err != nil {
			t.Fatalf("Add(%v): %v", m, err)
		}
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []Machine{{"rpi", "ssh", "pi@rpi.lan"}, {"web", "docker", "web"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %+v, want %+v (sorted by name)", got, want)
	}
	info, err := os.Stat(r.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("registry file = %v, %v; want mode 0600", info, err)
	}

	if err := r.Remove("rpi"); err != nil {
		t.Fatal(err)
	}
	got, _ = r.List()
	if !reflect.DeepEqual(got, []Machine{{"web", "docker", "web"}}) {
		t.Errorf("List() after rm = %+v", got)
	}
	if err := r.Remove("rpi"); !errors.Is(err, ErrUnknownMachine) {
		t.Errorf("Remove(unknown) error = %v, want ErrUnknownMachine", err)
	}
}

func TestConcurrentAddsKeepEveryMachine(t *testing.T) {
	r := newRegistry(t)
	const count = 80
	start := make(chan struct{})
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("machine-%03d", i)
		go func() {
			<-start
			// Separate Registry values represent callers in different processes.
			caller := &Registry{Path: r.Path, Kinds: DefaultKinds()}
			errs <- caller.Add(name, "ssh", name)
		}()
	}
	close(start)
	for i := 0; i < count; i++ {
		if err := <-errs; err != nil {
			t.Errorf("Add: %v", err)
		}
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count {
		t.Fatalf("got %d machines after %d successful adds", len(got), count)
	}
	for i, m := range got {
		name := fmt.Sprintf("machine-%03d", i)
		if m != (Machine{Name: name, Kind: "ssh", Target: name}) {
			t.Errorf("machine %d = %+v, want %s", i, m, name)
		}
	}
}

func TestConcurrentDuplicateAddsAcceptOnlyOne(t *testing.T) {
	r := newRegistry(t)
	const count = 40
	start := make(chan struct{})
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			<-start
			errs <- r.Add("rpi", "ssh", "rpi")
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < count; i++ {
		err := <-errs
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrInvalidMachine) {
			t.Errorf("Add error = %v, want duplicate error", err)
		}
	}
	if successes != 1 {
		t.Errorf("%d duplicate adds succeeded, want exactly one", successes)
	}
	got, err := r.List()
	if err != nil || !reflect.DeepEqual(got, []Machine{{"rpi", "ssh", "rpi"}}) {
		t.Fatalf("List() = %+v, %v; want one rpi", got, err)
	}
}

func TestConcurrentAddsAndRemovesKeepAllChanges(t *testing.T) {
	r := newRegistry(t)
	const count = 40
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("old-%03d", i)
		if err := r.Add(name, "ssh", name); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, count*2)
	for i := 0; i < count; i++ {
		oldName := fmt.Sprintf("old-%03d", i)
		newName := fmt.Sprintf("new-%03d", i)
		go func() {
			<-start
			errs <- r.Remove(oldName)
		}()
		go func() {
			<-start
			errs <- r.Add(newName, "ssh", newName)
		}()
	}
	close(start)
	for i := 0; i < count*2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("registry change: %v", err)
		}
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count {
		t.Fatalf("got %d machines after concurrent adds and removes, want %d", len(got), count)
	}
	for i, m := range got {
		name := fmt.Sprintf("new-%03d", i)
		if m != (Machine{Name: name, Kind: "ssh", Target: name}) {
			t.Errorf("machine %d = %+v, want %s", i, m, name)
		}
	}
}

func TestAddValidates(t *testing.T) {
	r := newRegistry(t)
	if err := r.Add("rpi", "ssh", "rpi"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, kind, target string }{
		{"local", "ssh", "x"},       // reserved
		{"rpi", "ssh", "other"},     // exists
		{"a/b", "ssh", "x"},         // not an address component
		{"new", "telnet", "x"},      // unknown kind
		{"new", "ssh", ""},          // no target
		{"new", "ssh", "-oFoo=bar"}, // would be read as an option
		{"new", "ssh", "a b"},       // not one argument
	}
	for _, c := range cases {
		if err := r.Add(c.name, c.kind, c.target); err == nil {
			t.Errorf("Add(%q, %q, %q) succeeded", c.name, c.kind, c.target)
		}
	}
	if got, _ := r.List(); len(got) != 1 {
		t.Errorf("List() = %+v; failed adds must not change the registry", got)
	}
}

func TestUnknownMachineListsTheConfiguredNames(t *testing.T) {
	r := newRegistry(t)
	for _, name := range []string{"web", "rpi"} {
		if err := r.Add(name, "ssh", name); err != nil {
			t.Fatal(err)
		}
	}
	_, err := r.Lookup("nas")
	if !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("Lookup(nas) error = %v, want ErrUnknownMachine", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "nas") || !strings.Contains(msg, "rpi, web") {
		t.Errorf("error = %q, want the name and the configured list", msg)
	}

	_, err = newRegistry(t).Lookup("nas")
	if !errors.Is(err, ErrUnknownMachine) || !strings.Contains(err.Error(), "none configured") {
		t.Errorf("empty registry error = %v", err)
	}
}

func TestKindsAreCommandPrefixes(t *testing.T) {
	cases := []struct {
		kind string
		want []string
	}{
		{"ssh", []string{"ssh", "-o", "BatchMode=yes", "rpi"}},
		{"docker", []string{"docker", "exec", "-i", "rpi"}},
		{"podman", []string{"podman", "exec", "-i", "rpi"}},
	}
	for _, c := range cases {
		prefix, ok := DefaultKinds()[c.kind]
		if !ok {
			t.Fatalf("no kind %q", c.kind)
		}
		if got := prefix("rpi"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s prefix = %v, want %v", c.kind, got, c.want)
		}
	}
}

func TestDefaultPathFollowsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, err := DefaultPath(); err != nil || got != "/xdg/recado/machines.toml" {
		t.Errorf("DefaultPath() = %q, %v", got, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, err := DefaultPath(); err != nil || got != "/home/u/.config/recado/machines.toml" {
		t.Errorf("DefaultPath() = %q, %v", got, err)
	}
}

func TestChangesPreserveHandEditedComments(t *testing.T) {
	r := newRegistry(t)
	writeFile(t, r, "# hosts\n[machines.rpi]\nkind = 'ssh'\ntarget = 'pi'\n\n# container notes\n[machines.web]\nkind = 'docker'\ntarget = 'web'\n")
	if err := r.Add("db", "podman", "db"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("rpi"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# hosts", "# container notes", "kind = 'docker'", "[machines.db]"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("registry lost %q: %s", want, data)
		}
	}
}

func TestDottedMachineNameUsesQuotedTOMLKey(t *testing.T) {
	r := newRegistry(t)
	if err := r.Add("rpi.lan", "ssh", "rpi.lan"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `[machines."rpi.lan"]`) {
		t.Errorf("dotted name must be a single TOML key: %s", data)
	}
	if _, err := r.Lookup("rpi.lan"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("rpi.lan"); err != nil {
		t.Fatal(err)
	}
}
