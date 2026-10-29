package main

import (
	"context"
	"fmt"
	"os"
	osExec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/store"
)

func TestRemoteSendStartsRelayAndReturnsReply(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "recado")
	build := osExec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	localHome := filepath.Join(dir, "local")
	remoteHome := filepath.Join(dir, "remote")
	config := filepath.Join(dir, "config")
	local, err := store.Open(filepath.Join(localHome, "recado"))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := store.Open(filepath.Join(remoteHome, "recado"))
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Register(store.Agent{ID: "manager", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err = remote.Register(store.Agent{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	// The first follow drops; the detached relay must reconnect over the same transport.
	script := fmt.Sprintf("#!/bin/sh\nshift 3\nif [ \"$2\" = outbox ] && [ ! -e %q ]; then touch %q; exit 1; fi\nunset RECADO_STATE_DIR\nexport XDG_STATE_HOME=%q\nexec \"$@\"\n", filepath.Join(dir, "dropped"), filepath.Join(dir, "dropped"), remoteHome)
	if err = os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+localHome, "XDG_CONFIG_HOME="+config, "RECADO_STATE_DIR=", "RECADO_ADDR=manager")
	run := func(e []string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		c := osExec.CommandContext(ctx, bin, args...)
		c.Env = e
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("recado %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	run(env, "machine", "add", "rpi", "ssh", "rpi")
	run(env, "send", "rpi/worker", "question", "--kind", "question")
	run(env, "send", "rpi/worker", "follow up")
	remoteMsgs, err := remote.Messages("worker")
	if err != nil || len(remoteMsgs) != 2 {
		t.Fatalf("remote inbox = %+v, %v", remoteMsgs, err)
	}
	remoteEnv := append(env, "XDG_STATE_HOME="+remoteHome, "RECADO_ADDR=worker")
	run(remoteEnv, "reply", remoteMsgs[0].Envelope.ID, "answer")
	deadline := time.Now().Add(8 * time.Second)
	for {
		msgs, err := local.Messages("manager")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) > 0 {
			if len(msgs) != 1 || msgs[0].Envelope.Body != "answer" || msgs[0].Envelope.From != "recado://rpi/worker" {
				t.Fatalf("return = %+v", msgs)
			}
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(localHome, "recado", "relay", "rpi.log"))
			t.Fatalf("relay did not return reply; log: %s", log)
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(localHome, "recado", "relay", "rpi.senders"))
	if err != nil || string(data) != "manager\n" {
		t.Fatalf("senders %q, %v", data, err)
	}
	log, err := os.ReadFile(filepath.Join(localHome, "recado", "relay", "rpi.log"))
	if err != nil || !strings.Contains(string(log), "outbox connection") {
		t.Fatalf("reconnect log %q, %v", log, err)
	}
	if err = local.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := os.OpenFile(filepath.Join(localHome, "recado", "relay", "rpi.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		f.Close()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached relay did not exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
