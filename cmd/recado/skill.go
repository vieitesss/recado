package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/skill"
)

func (a *app) newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print the guide agents follow; manage the installed skill and shims",
		Long: `Print the guide that recado's skill gives agents.

"recado skill setup" installs a small SKILL.md into each agent harness that
tells the agent to run "recado skill", so upgrading recado also updates what
agents are told. It also installs the shim each receiving harness needs, such
as Pi's extension.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), skill.Guide)
			return err
		},
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "setup",
			Short: "Install recado's skill and harness shims",
			Args:  cobra.NoArgs,
			RunE: all(
				eachTarget(func(cmd *cobra.Command, t skill.Target) error {
					if err := skill.Install(t); err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%-20s installed  %s\n", t.Agents, t.Dir)
					return nil
				}),
				a.eachShim(func(cmd *cobra.Command, label string, f skill.File) error {
					if err := f.Install(); err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%-20s installed  %s\n", label, f.Path)
					return nil
				}),
			),
		},
		&cobra.Command{
			Use:   "status",
			Short: "Report where recado's skill and shims are installed",
			Args:  cobra.NoArgs,
			RunE: all(
				eachTarget(func(cmd *cobra.Command, t skill.Target) error {
					state, err := skill.Inspect(t)
					if err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-10s %s\n", t.Agents, state, t.Dir)
					return nil
				}),
				a.eachShim(func(cmd *cobra.Command, label string, f skill.File) error {
					state, err := f.Inspect()
					if err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-10s %s\n", label, state, f.Path)
					return nil
				}),
				a.checkHarnesses,
			),
		},
		&cobra.Command{
			Use:   "remove",
			Short: "Remove recado's skill and harness shims",
			Args:  cobra.NoArgs,
			RunE: all(
				eachTarget(func(cmd *cobra.Command, t skill.Target) error {
					removed, err := skill.Remove(t)
					if err != nil {
						return err
					}
					if removed {
						fmt.Fprintf(cmd.OutOrStdout(), "%-20s removed    %s\n", t.Agents, t.Dir)
					}
					return nil
				}),
				a.eachShim(func(cmd *cobra.Command, label string, f skill.File) error {
					removed, err := f.Remove()
					if err != nil {
						return err
					}
					if removed {
						fmt.Fprintf(cmd.OutOrStdout(), "%-20s removed    %s\n", label, f.Path)
					}
					return nil
				}),
			),
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

// eachShim runs fn for every harness shim this binary installs: recado's
// business is the install-state logic, the harness only names its file.
func (a *app) eachShim(fn func(cmd *cobra.Command, label string, f skill.File) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		for _, h := range a.Harnesses {
			for _, shim := range h.Shims() {
				f := skill.File{Path: shim.Path, Content: shim.Content, Mode: shim.Mode, Marker: skill.MarkerShim}
				if err := fn(cmd, harnessLabel(h, "extension"), f); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// harnessLabel names a harness and one of its parts in status output.
func harnessLabel(h harness.Harness, part string) string {
	return strings.ToUpper(h.Name()[:1]) + h.Name()[1:] + " " + part
}

// checkHarnesses reports a harness that cannot receive messages here, such as
// a Pi older than the one its extension needs.
func (a *app) checkHarnesses(cmd *cobra.Command, _ []string) error {
	for _, h := range a.Harnesses {
		if err := h.Check(); err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s\n", harnessLabel(h, "harness"), err)
		}
	}
	return nil
}

// all runs the steps of one command in order, stopping at the first failure.
func all(steps ...func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		for _, step := range steps {
			if err := step(cmd, args); err != nil {
				return err
			}
		}
		return nil
	}
}
