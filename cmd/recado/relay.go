package main

import (
	"os"
	osExec "os/exec"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/relay"
)

func (a *app) newRelayCmd() *cobra.Command {
	return &cobra.Command{Use: "relay MACHINE", Short: "Relay replies from a machine", Args: cobra.ExactArgs(1), RunE: run(func(cmd *cobra.Command, args []string) error {
		svc, err := a.service()
		if err != nil {
			return err
		}
		return relay.Run(cmd.Context(), svc, args[0])
	})}
}

func (a *app) startReturnRelay(to addr.Address, sender string) error {
	if to.IsLocal() || to.Upstream {
		return nil
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	return relay.RecordSender(st.Root(), to.Machine, sender, func() error {
		if a.startRelay != nil {
			return a.startRelay(to.Machine)
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := osExec.Command(executable, "relay", to.Machine)
		cmd.Env = append(os.Environ(), "RECADO_STATE_DIR="+st.Root())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	})
}
