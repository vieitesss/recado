package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vieitesss/recado/internal/addr"
)

// ErrAmbiguousName means a recipient name matches more than one live agent.
var ErrAmbiguousName = errors.New("ambiguous agent name")

// DeriveName is the readable name of an agent that registered in cwd: the base
// name of the git work-tree root containing cwd, else the base name of cwd
// itself, lowered and turned into an agent id. "" means no usable name.
func DeriveName(cwd string) string {
	if cwd == "" {
		return ""
	}
	dir := cwd
	if root := workTree(cwd); root != "" {
		dir = root
	}
	return sanitise(filepath.Base(dir))
}

// workTree is the closest directory at or above cwd holding a .git entry. It
// reads the filesystem and runs no git binary: a plain checkout marks its root
// with a directory, a worktree or a submodule with a file.
func workTree(cwd string) string {
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}

// sanitise makes base an agent id: lower case, invalid runs replaced with one
// "-", and trimmed. A base that leaves nothing usable is no name.
func sanitise(base string) string {
	var b strings.Builder
	dash := true // drop whatever cannot start an id
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			dash = r == '-'
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimLeft(b.String(), "-._")
	s = strings.TrimRight(s, "-")
	if !addr.ValidID(s) {
		return ""
	}
	return s
}

// nameAgents fills Display for each agent: its stored base name, except that
// live agents sharing a base each get the shortest suffix of their id (at
// least two characters) that tells them apart. A gone entry shows its base and
// never forces a suffix on a live one.
func nameAgents(agents []Agent) {
	display := make(map[string]string, len(agents))
	groups := map[string][]Agent{}
	for _, a := range agents {
		display[a.ID] = a.Name
		if a.Live() && a.Name != "" {
			groups[a.Name] = append(groups[a.Name], a)
		}
	}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		for _, a := range group {
			display[a.ID] = a.Name + "-" + tellingSuffix(a.ID, group)
		}
	}
	for i := range agents {
		agents[i].Display = display[agents[i].ID]
	}
}

// tellingSuffix is the shortest suffix of id, at least two characters, that no
// other live agent of the same base shares. The whole id always tells them
// apart, because ids are unique.
func tellingSuffix(id string, peers []Agent) string {
	for n := 2; n < len(id); n++ {
		suffix := id[len(id)-n:]
		unique := true
		for _, p := range peers {
			if p.ID != id && strings.HasSuffix(p.ID, suffix) {
				unique = false
				break
			}
		}
		if unique {
			return suffix
		}
	}
	return id
}

// ResolveRecipient maps query to the agent a message goes to. An exact
// registered id wins, live or gone. Otherwise query names a live agent by the
// name it displays or its base name: one match resolves to its id, several are
// ambiguous. If no name matches and query is at least 4 characters long, a
// unique id prefix or suffix resolves to that agent, preferring live over gone.
func (s *Store) ResolveRecipient(query string) (Agent, error) {
	a, err := s.Lookup(query)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, ErrUnknownAgent) {
		return Agent{}, err
	}
	agents, err := s.List()
	if err != nil {
		return Agent{}, err
	}
	var matches []Agent
	for _, a := range agents {
		if a.Live() && a.Name != "" && (a.Name == query || a.Display == query) {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Agent{}, ambiguousError(query, matches)
	}
	if len(query) >= 4 {
		var live, gone []Agent
		for _, a := range agents {
			if strings.HasPrefix(a.ID, query) || strings.HasSuffix(a.ID, query) {
				if a.Live() {
					live = append(live, a)
				} else {
					gone = append(gone, a)
				}
			}
		}
		if len(live) == 1 {
			return live[0], nil
		}
		if len(live) > 1 {
			return Agent{}, ambiguousError(query, live)
		}
		if len(gone) == 1 {
			return gone[0], nil
		}
	}
	return Agent{}, fmt.Errorf("%w: %s", ErrUnknownAgent, query)
}

func ambiguousError(query string, matches []Agent) error {
	lines := make([]string, len(matches))
	for i, a := range matches {
		lines[i] = fmt.Sprintf("  %s %s %s", a.Display, a.ID, a.CWD)
	}
	return fmt.Errorf("%w %q: %d live agents match:\n%s", ErrAmbiguousName, query, len(matches), strings.Join(lines, "\n"))
}
