package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/skill"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print the guide agents follow; manage the installed skill",
		Long: `Print the guide that recado's skill gives agents.

"recado skill setup" installs a small SKILL.md into each agent harness that
tells the agent to run "recado skill", so upgrading recado also updates what
agents are told.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), skill.Guide)
			return err
		},
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "setup",
			Short: "Install recado's skill into agent harnesses",
			Args:  cobra.NoArgs,
			RunE: eachTarget(func(cmd *cobra.Command, t skill.Target) error {
				if err := skill.Install(t); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-20s installed  %s\n", t.Agents, t.Dir)
				return nil
			}),
		},
		&cobra.Command{
			Use:   "status",
			Short: "Report where recado's skill is installed",
			Args:  cobra.NoArgs,
			RunE: eachTarget(func(cmd *cobra.Command, t skill.Target) error {
				state, err := skill.Inspect(t)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-10s %s\n", t.Agents, state, t.Dir)
				return nil
			}),
		},
		&cobra.Command{
			Use:   "remove",
			Short: "Remove recado's skill from agent harnesses",
			Args:  cobra.NoArgs,
			RunE: eachTarget(func(cmd *cobra.Command, t skill.Target) error {
				removed, err := skill.Remove(t)
				if err != nil {
					return err
				}
				if removed {
					fmt.Fprintf(cmd.OutOrStdout(), "%-20s removed    %s\n", t.Agents, t.Dir)
				}
				return nil
			}),
		},
	)
	return cmd
}

// eachTarget runs fn for every skill directory under the user's home.
func eachTarget(fn func(*cobra.Command, skill.Target) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		for _, t := range skill.Targets(home) {
			if err := fn(cmd, t); err != nil {
				return err
			}
		}
		return nil
	}
}
