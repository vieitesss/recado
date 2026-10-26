// Package addr parses and formats recado agent addresses.
package addr

import (
	"fmt"
	"strings"
)

const (
	// LocalMachine is the machine name messages are delivered on.
	LocalMachine = "local"

	scheme         = "recado://"
	upstreamScheme = "recado+upstream://"
)

// Address names one agent: recado://<machine>/<agent-id>.
type Address struct {
	Machine  string
	Agent    string
	Upstream bool // recado+upstream://: an agent on the machine that reached this one
}

// Local is the address of id on this machine.
func Local(id string) Address { return Address{Machine: LocalMachine, Agent: id} }

// Parse accepts recado://<machine>/<id>, recado+upstream://<host>/<id>,
// <machine>/<id> and a bare <id>, which names an agent on this machine.
func Parse(s string) (Address, error) {
	var a Address
	rest, explicit := s, false
	switch {
	case strings.HasPrefix(s, upstreamScheme):
		a.Upstream, rest, explicit = true, s[len(upstreamScheme):], true
	case strings.HasPrefix(s, scheme):
		rest, explicit = s[len(scheme):], true
	case strings.Contains(s, "://"):
		return Address{}, fmt.Errorf("address %q: want recado://<machine>/<id>", s)
	}

	parts := strings.Split(rest, "/")
	switch {
	case explicit:
		if len(parts) != 2 {
			return Address{}, fmt.Errorf("address %q: want recado://<machine>/<id>", s)
		}
		a.Machine, a.Agent = parts[0], parts[1]
	case len(parts) == 2:
		a.Machine, a.Agent = parts[0], parts[1]
	case len(parts) == 1:
		a.Machine, a.Agent = LocalMachine, parts[0]
	default:
		return Address{}, fmt.Errorf("address %q: want <id>, <machine>/<id> or recado://<machine>/<id>", s)
	}
	if !validMachine(a.Machine) {
		return Address{}, fmt.Errorf("address %q: bad machine name", s)
	}
	if !ValidID(a.Agent) {
		return Address{}, fmt.Errorf("address %q: agent id must match [a-z0-9][a-z0-9._-]*", s)
	}
	return a, nil
}

// IsLocal reports whether a names an agent on this machine.
func (a Address) IsLocal() bool { return !a.Upstream && a.Machine == LocalMachine }

// String is the full address.
func (a Address) String() string {
	if a.Upstream {
		return upstreamScheme + a.Machine + "/" + a.Agent
	}
	return scheme + a.Machine + "/" + a.Agent
}

// Display is how the address shows in a delivered message: the bare agent id
// on this machine, machine/id from elsewhere.
func (a Address) Display() string {
	if a.IsLocal() {
		return a.Agent
	}
	return a.Machine + "/" + a.Agent
}

// ValidID reports whether id is a valid agent id: [a-z0-9][a-z0-9._-]*.
func ValidID(id string) bool {
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return id != ""
}

func validMachine(m string) bool {
	for i := 0; i < len(m); i++ {
		c := m[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return m != ""
}
