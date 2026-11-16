package main

import (
	"errors"
	osexec "os/exec"
	"slices"
	"strings"
	"testing"
)

func TestUpdateInstallsOverTheRunningBinaryThenRunsSetup(t *testing.T) {
	t.Setenv("RECADO_INSTALL_CMD", "install-it")
	a, _ := newTestApp(t, false)
	a.executable = func() (string, error) { return "/opt/bin/recado", nil }
	var ran []string
	var installDir string
	a.execCmd = func(c *osexec.Cmd) error {
		ran = append(ran, strings.Join(c.Args, " "))
		if c.Args[0] == "sh" {
			installDir = lastEnv(c.Env, "RECADO_INSTALL_DIR")
		}
		return nil
	}
	out, err := exec(t, a, "", "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh -c install-it", "/opt/bin/recado skill setup"}
	if !slices.Equal(ran, want) {
		t.Fatalf("ran %q, want %q", ran, want)
	}
	if installDir != "/opt/bin" {
		t.Fatalf("RECADO_INSTALL_DIR = %q, want /opt/bin", installDir)
	}
	if !strings.Contains(out, "updated  /opt/bin/recado") {
		t.Fatalf("out = %q", out)
	}
}

func TestUpdateDefaultsToTheInstallScript(t *testing.T) {
	t.Setenv("RECADO_INSTALL_CMD", "")
	a, _ := newTestApp(t, false)
	a.executable = func() (string, error) { return "/opt/bin/recado", nil }
	var install string
	a.execCmd = func(c *osexec.Cmd) error {
		if c.Args[0] == "sh" {
			install = c.Args[2]
		}
		return nil
	}
	if _, err := exec(t, a, "", "update"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(install, installURL) || !strings.HasPrefix(install, "set -e;") {
		t.Fatalf("install = %q, want a set -e download of %s", install, installURL)
	}
}

func TestUpdateStopsWhenInstallFails(t *testing.T) {
	t.Setenv("RECADO_INSTALL_CMD", "boom")
	a, _ := newTestApp(t, false)
	a.executable = func() (string, error) { return "/opt/bin/recado", nil }
	n := 0
	a.execCmd = func(*osexec.Cmd) error { n++; return errors.New("exit 1") }
	if _, err := exec(t, a, "", "update"); err == nil || n != 1 {
		t.Fatalf("err = %v, commands run = %d", err, n)
	}
}

// lastEnv is the value os/exec would use for key: the last entry wins.
func lastEnv(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
}
