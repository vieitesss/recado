package main

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const defaultInstallCmd = "go install github.com/vieitesss/recado/cmd/recado@latest"

func (a *app) newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "update",
		Aliases: []string{"upgrade"},
		Short:   "Install the latest recado and refresh its skill and shims",
		Long: `Install the latest recado, then run "recado skill setup" with the new binary so
the skill stubs, shims and hooks match it.

The install command is $RECADO_INSTALL_CMD, else
"` + defaultInstallCmd + `", run with
GOPRIVATE=github.com/vieitesss/* unless GOPRIVATE is already set.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			install := os.Getenv("RECADO_INSTALL_CMD")
			if install == "" {
				install = defaultInstallCmd
			}
			c := osexec.Command("sh", "-c", install)
			c.Env = os.Environ()
			if os.Getenv("GOPRIVATE") == "" {
				c.Env = append(c.Env, "GOPRIVATE=github.com/vieitesss/*")
			}
			c.Stdout, c.Stderr = out, cmd.ErrOrStderr()
			if err := a.runCmd(c); err != nil {
				return fmt.Errorf("install failed (%s): %w", install, err)
			}
			bin, err := a.installedBinary()
			if err != nil {
				return err
			}
			setup := osexec.Command(bin, "skill", "setup")
			setup.Stdout, setup.Stderr = out, cmd.ErrOrStderr()
			if err := a.runCmd(setup); err != nil {
				return fmt.Errorf("%s skill setup failed: %w", bin, err)
			}
			fmt.Fprintf(out, "updated  %s\n", bin)
			return nil
		}),
	}
}

func (a *app) runCmd(c *osexec.Cmd) error {
	if a.execCmd != nil {
		return a.execCmd(c)
	}
	return c.Run()
}

// installedBinary is where "go install" puts recado: $GOBIN, else $GOPATH/bin.
func (a *app) installedBinary() (string, error) {
	get := func(name string) (string, error) {
		c := osexec.Command("go", "env", name)
		var b strings.Builder
		c.Stdout = &b
		if err := a.runCmd(c); err != nil {
			return "", fmt.Errorf("go env %s: %w", name, err)
		}
		return strings.TrimSpace(b.String()), nil
	}
	dir, err := get("GOBIN")
	if err != nil {
		return "", err
	}
	if dir == "" {
		gopath, err := get("GOPATH")
		if err != nil {
			return "", err
		}
		dir = filepath.Join(filepath.SplitList(gopath)[0], "bin")
	}
	return filepath.Join(dir, "recado"), nil
}
