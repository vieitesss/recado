package harness

import (
	_ "embed"
	"encoding/json"
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

// opencodeTimeout bounds how long a push may wait on the OpenCode plugin: a
// wedged server must not hang the sender, and a failed push leaves the message
// stored.
const opencodeTimeout = 2 * time.Second

// OpenCode is the OpenCode adapter: a plugin inside the OpenCode server
// listens on a unix socket and turns each frame into a queued prompt.
type OpenCode struct {
	// Home is where the user's .config lives; empty means the real home.
	Home string
}

func (o *OpenCode) Name() string { return "opencode" }

// Detect reports the agent id when the OpenCode plugin prepared this process.
// The plugin sets both variables for a hook and for the agent's shell tool.
func (o *OpenCode) Detect(env []string) (string, bool) {
	if v, ok := lookupEnv(env, HarnessEnv); !ok || v != o.Name() {
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

// QueuesWhileBusy reports OpenCode's promise: a prompt admitted with delivery
// "queue" mid-turn becomes its own turn once the running one ends.
func (o *OpenCode) QueuesWhileBusy() bool { return true }

// Push writes one JSON frame to the socket the OpenCode plugin listens on. The
// frame names the agent because one plugin process serves every session it
// knows; the text is rendered by the delivery layer.
func (o *OpenCode) Push(agent store.Agent, msg Outgoing) error {
	if agent.Endpoint == "" {
		return fmt.Errorf("opencode: the registry entry has no socket endpoint")
	}
	conn, err := net.DialTimeout("unix", agent.Endpoint, opencodeTimeout)
	if err != nil {
		return fmt.Errorf("opencode: dial %s: %w", agent.Endpoint, err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(opencodeTimeout)); err != nil {
		return err
	}
	line, err := json.Marshal(prompt{Agent: agent.ID, Text: msg.Text})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("opencode: write %s: %w", agent.Endpoint, err)
	}
	return nil
}

// Shims is the plugin `recado skill setup` installs for OpenCode.
func (o *OpenCode) Shims() []Shim {
	f, err := o.shim()
	if err != nil {
		return nil
	}
	return []Shim{{Path: f.Path, Content: f.Content, Mode: f.Mode}}
}

// shim is the plugin file recado installs for OpenCode.
func (o *OpenCode) shim() (skill.File, error) {
	dir, err := o.configDir()
	if err != nil {
		return skill.File{}, err
	}
	return skill.File{
		Path:    filepath.Join(dir, "plugins", "recado.js"),
		Content: Plugin(),
		Mode:    0o644,
		Marker:  skill.MarkerShim,
	}, nil
}

// configDir is where OpenCode reads its configuration: $XDG_CONFIG_HOME/opencode
// when set, else ~/.config/opencode. Home overrides the home directory (tests).
func (o *OpenCode) configDir() (string, error) {
	if o.Home == "" {
		if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
			return filepath.Join(dir, "opencode"), nil
		}
	}
	home := o.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

//go:embed opencode-plugin.js
var opencodePlugin string

// openCodeShimVersion changes whenever the plugin's text does, so setup can
// tell an outdated shim of ours from a file someone else wrote.
const openCodeShimVersion = "1"

// Plugin is the file `recado skill setup` installs into OpenCode, version-stamped
// on its first line.
func Plugin() []byte {
	return []byte(`// recado-shim: "` + openCodeShimVersion + `"` + "\n" + opencodePlugin)
}

// minOpenCodeVersion is the first OpenCode release with the v2 plugin API and
// the session.prompt delivery mode recado needs.
const minOpenCodeVersion = "2.0.0"

// Check reports whether this machine can receive messages in OpenCode: the
// binary is new enough, and the installed plugin is the current one.
func (o *OpenCode) Check() error {
	path, err := exec.LookPath("opencode")
	if err != nil {
		return fmt.Errorf("opencode is not on PATH: %w", err)
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return fmt.Errorf("opencode --version: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return fmt.Errorf("opencode --version printed nothing")
	}
	version := fields[len(fields)-1]
	if !versionAtLeast(version, minOpenCodeVersion) {
		return fmt.Errorf("opencode %s is older than %s", strings.TrimPrefix(version, "v"), minOpenCodeVersion)
	}
	shim, err := o.shim()
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

// prompt is the one frame the OpenCode plugin accepts.
type prompt struct {
	Agent string `json:"agent"`
	Text  string `json:"text"`
}
