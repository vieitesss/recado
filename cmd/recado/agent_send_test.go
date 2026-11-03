package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/relay"
	"github.com/vieitesss/recado/internal/store"
)

func TestAgentSendAndReplyLocallyDoNotEnroll(t *testing.T) {
	a, _ := newTestApp(t, false)
	svc, err := a.service()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"manager", "worker"} {
		if err := svc.Store.Register(store.Agent{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	sender := relay.Sender{Delivery: svc, Start: func(machine string) error {
		t.Fatalf("started relay for local route: %s", machine)
		return nil
	}}
	question, err := envelope.New(addr.Local("manager"), addr.Local("worker"), envelope.KindQuestion, "question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(question); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Reply(addr.Local("worker"), question.ID, "answer"); err != nil {
		t.Fatal(err)
	}
	messages, err := svc.Store.Messages("manager")
	if err != nil || len(messages) != 1 || messages[0].Envelope.ReplyTo != question.ID {
		t.Fatalf("local answer = %+v, %v", messages, err)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "relay")); !os.IsNotExist(err) {
		t.Fatalf("local send created relay state: %v", err)
	}
}

func TestAgentSendAndReplyUpstreamDoNotEnroll(t *testing.T) {
	a, _ := newTestApp(t, false)
	svc, err := a.service()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.Register(store.Agent{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	upstream := addr.Address{Machine: "host", Agent: "manager", Upstream: true}
	question, err := envelope.New(upstream, addr.Local("worker"), envelope.KindQuestion, "question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Deliver(question); err != nil {
		t.Fatal(err)
	}
	sender := relay.Sender{Delivery: svc, Start: func(machine string) error {
		t.Fatalf("started relay for upstream route: %s", machine)
		return nil
	}}
	message, err := envelope.New(addr.Local("worker"), upstream, envelope.KindMessage, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(message); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Reply(addr.Local("worker"), question.ID, "answer"); err != nil {
		t.Fatal(err)
	}
	out := mustExec(t, a, "", "outbox", "--host", "host", "--json")
	decoder := json.NewDecoder(strings.NewReader(out))
	var first, second struct {
		Envelope envelope.Envelope `json:"envelope"`
	}
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.Envelope.ID != message.ID || second.Envelope.Body != "answer" || second.Envelope.ReplyTo != question.ID {
		t.Fatalf("upstream records = %+v, %+v", first, second)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "relay")); !os.IsNotExist(err) {
		t.Fatalf("upstream send created relay state: %v", err)
	}
}

func TestRelayStartupFailureAfterRemoteDelivery(t *testing.T) {
	for _, command := range []string{"send", "ask", "reply"} {
		t.Run(command, func(t *testing.T) {
			remote, _ := newTestApp(t, false)
			remoteStore, err := remote.store()
			if err != nil {
				t.Fatal(err)
			}
			if err := remoteStore.Register(store.Agent{ID: "worker"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RECADO_TEST_REMOTE", "1")
			t.Setenv("RECADO_STATE_DIR", remote.Root)
			t.Setenv("RECADO_ADDR", "manager")
			local, _ := newTestApp(t, false)
			local.Machines = remoteTestRegistry(t)
			startupErr := errors.New("relay startup failed")
			local.startRelay = func(string) error { return startupErr }
			args := []string{command, "rpi/worker", "hello"}
			if command == "reply" {
				svc, err := local.service()
				if err != nil {
					t.Fatal(err)
				}
				if err := svc.Store.Register(store.Agent{ID: "manager"}); err != nil {
					t.Fatal(err)
				}
				question, err := envelope.New(addr.Address{Machine: "rpi", Agent: "worker"}, addr.Local("manager"), envelope.KindQuestion, "question", nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := svc.Deliver(question); err != nil {
					t.Fatal(err)
				}
				args[1] = question.ID
			}
			out, err := exec(t, local, "", args...)
			if !errors.Is(err, startupErr) || exitCode(err) != 1 || out != "" {
				t.Fatalf("%s output = %q, error = %v, exit = %d", command, out, err, exitCode(err))
			}
			messages, err := remoteStore.Messages("worker")
			if err != nil || len(messages) != 1 || messages[0].Envelope.Body != "hello" {
				t.Fatalf("remote messages = %+v, %v; want delivered message", messages, err)
			}
			data, err := os.ReadFile(filepath.Join(local.Root, "relay", "rpi.senders"))
			if err != nil || string(data) != "manager\n" {
				t.Fatalf("senders = %q, %v; want manager recorded", data, err)
			}
			var restarted []string
			local.startRelay = func(machine string) error {
				restarted = append(restarted, machine)
				return nil
			}
			mustExec(t, local, "", "send", "rpi/worker", "follow up")
			data, err = os.ReadFile(filepath.Join(local.Root, "relay", "rpi.senders"))
			if err != nil || string(data) != "manager\n" || !reflect.DeepEqual(restarted, []string{"rpi"}) {
				t.Fatalf("after retry: senders = %q, restarted = %v, error = %v", data, restarted, err)
			}
		})
	}
}

func TestAgentSendDoesNotEnrollAfterRemoteDeliveryFailure(t *testing.T) {
	a, _ := newTestApp(t, false)
	svc, err := a.service()
	if err != nil {
		t.Fatal(err)
	}
	sender := relay.Sender{Delivery: svc, Start: func(machine string) error {
		t.Fatalf("started relay after delivery failure: %s", machine)
		return nil
	}}
	env, err := envelope.New(addr.Local("manager"), addr.Address{Machine: "missing", Agent: "worker"}, envelope.KindMessage, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(env); err == nil {
		t.Fatal("send to missing machine succeeded")
	}
	if _, err := os.Stat(filepath.Join(a.Root, "relay")); !os.IsNotExist(err) {
		t.Fatalf("failed delivery created relay state: %v", err)
	}
}

func TestGenericDeliverDoesNotEnrollRemoteSender(t *testing.T) {
	remote, _ := newTestApp(t, false)
	remoteStore, err := remote.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStore.Register(store.Agent{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	local, _ := newTestApp(t, false)
	local.Machines = remoteTestRegistry(t)
	local.startRelay = func(machine string) error {
		t.Fatalf("generic deliver started relay for %s", machine)
		return nil
	}
	env, err := envelope.New(addr.Local("manager"), addr.Address{Machine: "rpi", Agent: "worker"}, envelope.KindMessage, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	line, err := env.Line()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, local, string(line), "deliver", "--json")
	messages, err := remoteStore.Messages("worker")
	if err != nil || len(messages) != 1 || messages[0].Envelope.ID != env.ID {
		t.Fatalf("remote messages = %+v, %v; want delivered envelope", messages, err)
	}
	if _, err := os.Stat(filepath.Join(local.Root, "relay")); !os.IsNotExist(err) {
		t.Fatalf("generic deliver created relay state: %v", err)
	}
}

func TestAgentReplyEnrollsRemoteSender(t *testing.T) {
	remote, _ := newTestApp(t, false)
	remoteStore, err := remote.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStore.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	local, _ := newTestApp(t, false)
	local.Machines = remoteTestRegistry(t)
	svc, err := local.service()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.Register(store.Agent{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	question, err := envelope.New(addr.Address{Machine: "rpi", Agent: "manager"}, addr.Local("worker"), envelope.KindQuestion, "question", map[string]string{"item": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Deliver(question); err != nil {
		t.Fatal(err)
	}
	var started []string
	sender := relay.Sender{Delivery: svc, Start: func(machine string) error {
		started = append(started, machine)
		return nil
	}}
	result, err := sender.Reply(addr.Local("worker"), question.ID, "answer")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := remoteStore.Messages("manager")
	if err != nil || len(messages) != 1 {
		t.Fatalf("remote messages = %+v, %v; want one answer", messages, err)
	}
	answer := messages[0].Envelope
	if answer.ID != result.Message.Envelope.ID || answer.Body != "answer" || answer.ReplyTo != question.ID || answer.Meta["item"] != "x" {
		t.Fatalf("answer = %+v", answer)
	}
	data, err := os.ReadFile(filepath.Join(local.Root, "relay", "rpi.senders"))
	if err != nil || string(data) != "worker\n" || !reflect.DeepEqual(started, []string{"rpi"}) {
		t.Fatalf("senders = %q, started = %v, error = %v", data, started, err)
	}
}

func TestAgentSendEnrollsRemoteSendersOncePerRunningRelay(t *testing.T) {
	remote, _ := newTestApp(t, false)
	remoteStore, err := remote.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStore.Register(store.Agent{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	local, _ := newTestApp(t, false)
	local.Machines = remoteTestRegistry(t)
	if err := local.Machines.Add("other", "fake", "other"); err != nil {
		t.Fatal(err)
	}
	svc, err := local.service()
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	var locks []*os.File
	t.Cleanup(func() {
		for _, lock := range locks {
			store.Unlock(lock)
		}
	})
	sender := relay.Sender{Delivery: svc, Start: func(machine string) error {
		started = append(started, machine)
		lock, err := store.LockFile(filepath.Join(local.Root, "relay", machine+".lock"), os.O_RDWR, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			locks = append(locks, lock)
		}
		return err
	}}
	for _, tc := range []struct{ from, machine string }{
		{"manager", "rpi"}, {"manager", "rpi"}, {"other-agent", "rpi"}, {"manager", "other"},
	} {
		env, err := envelope.New(addr.Local(tc.from), addr.Address{Machine: tc.machine, Agent: "worker"}, envelope.KindMessage, "hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sender.Send(env); err != nil {
			t.Fatalf("send from %s to %s: %v", tc.from, tc.machine, err)
		}
	}
	if !reflect.DeepEqual(started, []string{"rpi", "other"}) {
		t.Fatalf("started relays = %v", started)
	}
	for machine, want := range map[string]string{"rpi": "manager\nother-agent\n", "other": "manager\n"} {
		data, err := os.ReadFile(filepath.Join(local.Root, "relay", machine+".senders"))
		if err != nil || string(data) != want {
			t.Errorf("%s senders = %q, %v; want %q", machine, data, err, want)
		}
	}
	messages, err := remoteStore.Messages("worker")
	if err != nil || len(messages) != 4 {
		t.Fatalf("remote messages = %+v, %v; want four deliveries", messages, err)
	}
}
