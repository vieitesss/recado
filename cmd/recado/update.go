package main

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// installURL serves install.sh from the latest release.
const installURL = "https://recado.vieitesss.eu/install"

// defaultInstallCmd downloads the install script in full before running it, so
// a failed download fails the command instead of feeding sh an empty script.
const defaultInstallCmd = `set -e; f=$(mktemp); trap 'rm -f "$f"' EXIT; curl -fsSL ` + installURL + ` -o "$f"; sh "$f"`

func (a *app) newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "update",
		Aliases: []string{"upgrade"},
		Short:   "Install the latest recado and refresh its skill and shims",
		Long: `Install the latest recado over the running binary, then run "recado skill setup"
with the new binary so the skill stubs, shims and hooks match it.

The install command is $RECADO_INSTALL_CMD, else the install script at
` + installURL + `. It runs with RECADO_INSTALL_DIR set to the running
binary's directory.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			bin, err := a.runningBinary()
			if err != nil {
				return err
			}
			install := os.Getenv("RECADO_INSTALL_CMD")
			if install == "" {
				install = defaultInstallCmd
			}
			c := osexec.Command("sh", "-c", install)
			c.Env = append(os.Environ(), "RECADO_INSTALL_DIR="+filepath.Dir(bin), "RECADO_UPDATING=1")
			c.Stdout, c.Stderr = out, cmd.ErrOrStderr()
			if err := a.runCmd(c); err != nil {
				return fmt.Errorf("install failed (%s): %w", install, err)
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

// runningBinary is the path of this recado, symlinks resolved, so the update
// replaces the binary that runs instead of installing a second copy.
func (a *app) runningBinary() (string, error) {
	exe := os.Executable
	if a.executable != nil {
		exe = a.executable
	}
	path, err := exe()
	if err != nil {
		return "", fmt.Errorf("find the running recado: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}
