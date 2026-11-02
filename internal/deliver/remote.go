package deliver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/machine"
	"github.com/vieitesss/recado/internal/store"
)

// ErrRemoteUsage means remote recado rejected an address or other input.
var ErrRemoteUsage = errors.New("remote deliver rejected input")

// RemoteCommand builds a recado command over the configured machine transport.
func RemoteCommand(registry *machine.Registry, name string, args ...string) (*exec.Cmd, error) {
	if registry == nil {
		return nil, fmt.Errorf("machine registry not configured")
	}
	m, err := registry.Lookup(name)
	if err != nil {
		return nil, err
	}
	prefix := registry.Kinds[m.Kind](m.Target)
	if len(prefix) == 0 {
		return nil, fmt.Errorf("empty command prefix for machine %q", name)
	}
	argv := append(append([]string{}, prefix[1:]...), "recado")
	argv = append(argv, args...)
	return exec.Command(prefix[0], argv...), nil
}

// deliverRemote sends one rewritten envelope over the configured transport.
// A successful remote deliver reports the recipient's short id and push state.
func (s *Service) deliverRemote(env envelope.Envelope, from, to addr.Address) (Result, error) {
	cmd, err := RemoteCommand(s.Machines, to.Machine, "deliver", "--json")
	if err != nil {
		return Result{}, err
	}
	host, err := os.Hostname()
	if err != nil {
		return Result{}, fmt.Errorf("get local hostname: %w", err)
	}
	upstream := addr.Address{Machine: host, Agent: from.Agent, Upstream: true}
	if _, err := addr.Parse(upstream.String()); err != nil {
		return Result{}, fmt.Errorf("local hostname %q cannot be used in an upstream address: %w", host, err)
	}
	wire := env
	wire.From = upstream
	wire.To = addr.Local(to.Agent)
	line, err := wire.Line()
	if err != nil {
		return Result{}, err
	}
	cmd.Stdin = bytes.NewReader(line)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		detail := strings.TrimSpace(stderr.String())
		if (errors.As(err, &exit) && exit.ExitCode() == 127) || (strings.Contains(detail, "recado") && (strings.Contains(detail, "executable file not found") || strings.Contains(detail, "command not found") || strings.Contains(detail, "not found"))) {
			return Result{}, fmt.Errorf("machine %q: recado is not installed or not on PATH: %s", to.Machine, detail)
		}
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return Result{}, fmt.Errorf("%w on machine %q: %s", ErrRemoteUsage, to.Machine, detail)
		}
		if detail != "" {
			return Result{}, fmt.Errorf("machine %q transport failed: %w: %s", to.Machine, err, detail)
		}
		return Result{}, fmt.Errorf("machine %q transport failed: %w", to.Machine, err)
	}
	var response struct {
		Delivered []struct {
			ID     string `json:"id"`
			Short  string `json:"short"`
			To     string `json:"to"`
			Pushed bool   `json:"pushed"`
		} `json:"delivered"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return Result{}, fmt.Errorf("machine %q: invalid deliver response: %w", to.Machine, err)
	}
	if len(response.Delivered) != 1 || response.Delivered[0].ID != env.ID || response.Delivered[0].To != wire.To.String() || response.Delivered[0].Short == "" {
		return Result{}, fmt.Errorf("machine %q: invalid deliver response", to.Machine)
	}
	r := response.Delivered[0]
	state := store.StateStored
	if r.Pushed {
		state = store.StateQueued
	}
	return Result{Message: store.Message{Envelope: env, Short: r.Short, State: state}, Pushed: r.Pushed}, nil
}
