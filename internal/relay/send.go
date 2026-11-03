package relay

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/envelope"
)

// Sender delivers agent-originated messages and enrolls remote return routes.
// A nil Start launches a detached relay; Start may provide a process adapter.
type Sender struct {
	Delivery *deliver.Service
	Start    func(machine string) error
}

// Send delivers env and, for an outward remote route, enrolls its sender.
// If enrollment fails, delivery has already succeeded and the error is returned.
func (s Sender) Send(env envelope.Envelope) (deliver.Result, error) {
	return s.enroll(s.Delivery.Deliver(env))
}

// Reply answers a message and enrolls the sender for an outward remote route.
func (s Sender) Reply(from addr.Address, query, body string) (deliver.Result, error) {
	return s.enroll(s.Delivery.Reply(from, query, body))
}

func (s Sender) enroll(res deliver.Result, err error) (deliver.Result, error) {
	if err != nil {
		return deliver.Result{}, err
	}
	to := res.Message.Envelope.To
	if !to.IsLocal() && !to.Upstream {
		err = RecordSender(s.Delivery.Store.Root(), to.Machine, res.Message.Envelope.From.Agent, func() error {
			if s.Start != nil {
				return s.Start(to.Machine)
			}
			return start(s.Delivery.Store.Root(), to.Machine)
		})
		if err != nil {
			return deliver.Result{}, err
		}
	}
	return res, nil
}

func start(root, machine string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "relay", machine)
	cmd.Env = append(os.Environ(), "RECADO_STATE_DIR="+root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
