package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/vieitesss/recado/internal/machine"
)

func (a *app) machineRegistry() (*machine.Registry, error) {
	if a.Machines != nil {
		return a.Machines, nil
	}
	path, err := machine.DefaultPath()
	if err != nil {
		return nil, err
	}
	return &machine.Registry{Path: path, Kinds: machine.DefaultKinds()}, nil
}

func (a *app) newMachineCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "machine", Short: "Manage remote machines"}
	cmd.AddCommand(&cobra.Command{
		Use: "add NAME KIND TARGET", Short: "Add a machine", Args: cobra.ExactArgs(3),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			r, err := a.machineRegistry()
			if err != nil {
				return err
			}
			if err := r.Add(args[0], args[1], args[2]); err != nil {
				return err
			}
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), machine.Machine{Name: args[0], Kind: args[1], Target: args[2]})
			}
			return nil
		}),
	}, &cobra.Command{
		Use: "rm NAME", Short: "Remove a machine", Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			r, err := a.machineRegistry()
			if err != nil {
				return err
			}
			if err := r.Remove(args[0]); err != nil {
				return err
			}
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Name string `json:"name"`
				}{args[0]})
			}
			return nil
		}),
	}, &cobra.Command{
		Use: "ls", Short: "List machines", Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			r, err := a.machineRegistry()
			if err != nil {
				return err
			}
			list, err := r.List()
			if err != nil {
				return err
			}
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Machines []machine.Machine `json:"machines"`
				}{list})
			}
			for _, m := range list {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", m.Name, m.Kind, m.Target); err != nil {
					return err
				}
			}
			return nil
		}),
	})
	return cmd
}
