package main

import (
	"github.com/spf13/cobra"
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

func (a *app) sender() (relay.Sender, error) {
	svc, err := a.service()
	if err != nil {
		return relay.Sender{}, err
	}
	return relay.Sender{Delivery: svc, Start: a.startRelay}, nil
}
