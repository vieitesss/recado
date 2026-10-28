package harness

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

// piTimeout bounds how long a push may wait on the Pi process: a wedged agent
// must not hang the sender, and a failed push leaves the message stored.
const piTimeout = 2 * time.Second

// minPiVersion is the first Pi release whose extension API both starts a turn
// when idle and queues a follow-up in the middle of a turn.
const minPiVersion = "1.2.0"

// Pi is the Pi adapter: an extension inside the Pi process listens on a unix
// socket, and turns each line it receives into a user turn.
type Pi struct {
	// Home is where the user's .pi lives; empty means the real home directory.
	Home string
}

func (p *Pi) Name() string { return "pi" }

// Detect reports the agent id when a Pi extension prepared this process.
func (p *Pi) Detect(env []string) (string, bool) {
	if v, ok := lookupEnv(env, HarnessEnv); !ok || v != p.Name() {
		return "", false
	}
	raw, ok := lookupEnv(env, "RECADO_ADDR")
	if !ok {
		return "", false
	}
	self, err := addr.Parse(raw)
	if err != nil || !self.IsLocal() {
		return "", false
	}
	return self.Agent, true
}

// QueuesWhileBusy reports Pi's promise: an extension message sent mid-turn is
// queued and delivered as its own user turn once the running turn ends.
func (p *Pi) QueuesWhileBusy() bool { return true }

// Shims is the extension `recado skill setup` installs for Pi.
func (p *Pi) Shims() []Shim {
	f, err := p.shim()
	if err != nil {
		return nil
	}
	return []Shim{{Path: f.Path, Content: f.Content, Mode: f.Mode}}
}

// Check reports whether this machine can receive messages in Pi: the binary is
// new enough, and the installed extension is the current one.
func (p *Pi) Check() error {
	path, err := exec.LookPath("pi")
	if err != nil {
		return fmt.Errorf("pi is not on PATH: %w", err)
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return fmt.Errorf("pi --version: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if !versionAtLeast(version, minPiVersion) {
		return fmt.Errorf("pi %s is older than %s", version, minPiVersion)
	}
	shim, err := p.shim()
	if err != nil {
		return err
	}
	state, err := shim.Inspect()
	if err != nil {
		return err
	}
	if state != skill.Current {
		return fmt.Errorf("%s is %s; run recado skill setup", shim.Path, state)
	}
	return nil
}

// shim is the extension file recado installs for Pi.
func (p *Pi) shim() (skill.File, error) {
	home := p.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return skill.File{}, err
		}
	}
	return skill.File{
		Path:    filepath.Join(home, ".pi", "agent", "extensions", "recado.ts"),
		Content: Extension(),
		Mode:    0o644,
		Marker:  skill.MarkerShim,
	}, nil
}

//go:embed pi-extension.ts
var piExtension string

// shimVersion changes whenever the extension's text does, so setup can tell an
// outdated shim of ours from a file someone else wrote.
const shimVersion = "3"

// Extension is the file `recado skill setup` installs into Pi, version-stamped
// on its first line.
func Extension() []byte {
	return []byte(`// recado-shim: "` + shimVersion + `"` + "\n" + piExtension)
}

// Push writes one JSON line to the socket the Pi extension listens on, with
// the text the agent should read, and waits for the extension's answer. Pi's
// extension API does not confirm admission: an acceptance means the extension
// attempted a follow-up, not that the session took it or answered it.
func (p *Pi) Push(agent store.Agent, msg Outgoing) error {
	if agent.Endpoint == "" {
		return errors.New("pi: the registry entry has no socket endpoint")
	}
	conn, err := net.DialTimeout("unix", agent.Endpoint, piTimeout)
	if err != nil {
		return fmt.Errorf("pi: dial %s: %w", agent.Endpoint, err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(piTimeout)); err != nil {
		return err
	}
	line, err := json.Marshal(followUp{Text: msg.Text})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("pi: write %s: %w", agent.Endpoint, err)
	}
	return awaitAck(conn, piTimeout, "pi")
}

// followUp is the one frame the Pi extension accepts.
type followUp struct {
	Text string `json:"text"`
}
