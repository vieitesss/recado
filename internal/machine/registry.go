// Package machine manages the configured transports to other machines.
package machine

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/store"
)

var (
	ErrUnknownMachine = errors.New("unknown machine")
	ErrInvalidMachine = errors.New("invalid machine")
)

type Machine struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// Kind turns a target into the command prefix used to run a command there.
type Kind func(target string) []string

func DefaultKinds() map[string]Kind {
	return map[string]Kind{
		"ssh":    func(t string) []string { return []string{"ssh", "-o", "BatchMode=yes", t} },
		"docker": func(t string) []string { return []string{"docker", "exec", "-i", t} },
		"podman": func(t string) []string { return []string{"podman", "exec", "-i", t} },
	}
}

type Registry struct {
	Path  string
	Kinds map[string]Kind
}

func DefaultPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "recado", "machines.toml"), nil
}

func (r *Registry) validate(m Machine) error {
	if m.Name == addr.LocalMachine {
		return fmt.Errorf("machine name %q is reserved", m.Name)
	}
	if _, err := addr.Parse("recado://" + m.Name + "/agent"); err != nil {
		return fmt.Errorf("invalid machine name %q", m.Name)
	}
	if _, ok := r.Kinds[m.Kind]; !ok {
		return fmt.Errorf("unknown machine kind %q", m.Kind)
	}
	if m.Target == "" || strings.HasPrefix(m.Target, "-") || strings.IndexFunc(m.Target, unicode.IsSpace) >= 0 || strings.IndexFunc(m.Target, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid machine target %q", m.Target)
	}
	return nil
}

// List reads a small TOML subset: [machines.NAME] tables with quoted kind and target.
func (r *Registry) List() ([]Machine, error) {
	f, err := os.Open(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return []Machine{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var machines []Machine
	seen := map[string]bool{}
	keys := map[string]bool{}
	line := 0
	fail := func(err error) ([]Machine, error) { return nil, fmt.Errorf("%s:%d: %w", r.Path, line, err) }
	finish := func() error {
		if len(machines) == 0 {
			return nil
		}
		return r.validate(machines[len(machines)-1])
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "[") {
			if err := finish(); err != nil {
				return fail(err)
			}
			end := strings.IndexByte(s, ']')
			if end < 0 || !commentOnly(s[end+1:]) || !strings.HasPrefix(s[:end], "[machines.") {
				return fail(fmt.Errorf("invalid table %q", s))
			}
			name, err := tableName(s[:end])
			if err != nil {
				return fail(err)
			}
			if seen[name] {
				return fail(fmt.Errorf("duplicate machine %q", name))
			}
			seen[name] = true
			machines = append(machines, Machine{Name: name})
			keys = map[string]bool{}
			continue
		}
		key, value, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || len(machines) == 0 || (key != "kind" && key != "target") || keys[key] {
			return fail(fmt.Errorf("invalid or duplicate key %q", key))
		}
		keys[key] = true
		v, err := parseString(strings.TrimSpace(value))
		if err != nil {
			return fail(err)
		}
		m := &machines[len(machines)-1]
		if key == "kind" {
			m.Kind = v
		} else {
			m.Target = v
		}
	}
	if err := scanner.Err(); err != nil {
		return fail(err)
	}
	if err := finish(); err != nil {
		return fail(err)
	}
	sort.Slice(machines, func(i, j int) bool { return machines[i].Name < machines[j].Name })
	if machines == nil {
		machines = []Machine{}
	}
	return machines, nil
}

func tableName(table string) (string, error) {
	key := strings.TrimPrefix(table, "[machines.")
	if strings.HasPrefix(key, "\"") || strings.HasPrefix(key, "'") {
		return parseString(key)
	}
	if strings.Contains(key, ".") {
		return "", fmt.Errorf("machine names containing dots must be quoted")
	}
	return key, nil
}

func commentOnly(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.HasPrefix(s, "#")
}

func parseString(s string) (string, error) {
	if len(s) < 2 || (s[0] != '\'' && s[0] != '"') {
		return "", fmt.Errorf("expected quoted value")
	}
	quote := s[0]
	for i := 1; i < len(s); i++ {
		if quote == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			if !commentOnly(s[i+1:]) {
				return "", fmt.Errorf("trailing text after value")
			}
			if quote == '\'' {
				return s[1:i], nil
			}
			return strconv.Unquote(s[:i+1])
		}
	}
	return "", fmt.Errorf("unterminated value")
}

func (r *Registry) Lookup(name string) (Machine, error) {
	machines, err := r.List()
	if err != nil {
		return Machine{}, err
	}
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		if m.Name == name {
			return m, nil
		}
		names = append(names, m.Name)
	}
	list := strings.Join(names, ", ")
	if list == "" {
		list = "none configured"
	}
	return Machine{}, fmt.Errorf("%w %q (configured: %s)", ErrUnknownMachine, name, list)
}

func (r *Registry) Add(name, kind, target string) error {
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer store.Unlock(lock)
	m := Machine{name, kind, target}
	if err := r.validate(m); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMachine, err)
	}
	machines, err := r.List()
	if err != nil {
		return err
	}
	for _, existing := range machines {
		if existing.Name == name {
			return fmt.Errorf("%w: machine %q already exists", ErrInvalidMachine, name)
		}
	}
	data, err := os.ReadFile(r.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	key := name
	if strings.Contains(name, ".") {
		key = strconv.Quote(name)
	}
	data = append(data, fmt.Appendf(nil, "\n[machines.%s]\nkind = %s\ntarget = %s\n", key, strconv.Quote(kind), strconv.Quote(target))...)
	return r.write(data)
}

func (r *Registry) Remove(name string) error {
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer store.Unlock(lock)
	if _, err := r.Lookup(name); err != nil {
		return err
	}
	data, err := os.ReadFile(r.Path)
	if err != nil {
		return err
	}
	var out strings.Builder
	skip := false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "[") {
			end := strings.IndexByte(s, ']')
			if end >= 0 {
				key, err := tableName(s[:end])
				if err != nil {
					return err
				}
				skip = key == name
			}
		}
		if !skip || s == "" || strings.HasPrefix(s, "#") {
			out.WriteString(line)
		}
	}
	return r.write([]byte(out.String()))
}

// lock uses a stable file because writing the registry replaces the TOML inode.
func (r *Registry) lock() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return nil, err
	}
	return store.LockFile(r.Path+".lock", os.O_RDWR|os.O_CREATE, syscall.LOCK_EX)
}

func (r *Registry) write(data []byte) error {
	dir := filepath.Dir(r.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".machines.*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), r.Path)
}
