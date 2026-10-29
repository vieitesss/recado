package main

import (
	"errors"
	osexec "os/exec"
	"strings"
	"testing"
)

func TestUpdateInstallsThenRunsSetup(t *testing.T) {
	t.Setenv("RECADO_INSTALL_CMD", "install-it")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GOBIN", "")
	a, _ := newTestApp(t, false)
	var ran []string
	a.execCmd = func(c *osexec.Cmd) error {
		ran = append(ran, strings.Join(c.Args, " "))
		if c.Args[0] == "go" {
			c.Stdout.Write([]byte("/gobin\n"))
		}
		return nil
	}
	out, err := exec(t, a, "", "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh -c install-it", "go env GOBIN", "/gobin/recado skill setup"}
	if strings.Join(ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %q, want %q", ran, want)
	}
	if !strings.Contains(out, "updated  /gobin/recado") {
		t.Fatalf("out = %q", out)
	}
}

func TestUpdateStopsWhenInstallFails(t *testing.T) {
	t.Setenv("RECADO_INSTALL_CMD", "boom")
	a, _ := newTestApp(t, false)
	n := 0
	a.execCmd = func(*osexec.Cmd) error { n++; return errors.New("exit 1") }
	if _, err := exec(t, a, "", "update"); err == nil || n != 1 {
		t.Fatalf("err = %v, commands run = %d", err, n)
	}
}
