package envelope

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/addr"
)

func TestLineRoundTrip(t *testing.T) {
	from, to := addr.Local("manager-1"), addr.Local("sub-1")
	env, err := New(from, to, KindQuestion, "how is it going?", map[string]string{"item": "login"})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.ID) != 26 || env.ID != strings.ToUpper(env.ID) {
		t.Errorf("id = %q, want 26 uppercase Crockford characters", env.ID)
	}
	if got := env.TS.UTC(); !got.Equal(env.TS) || got.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("ts = %v, want whole milliseconds in UTC", env.TS)
	}

	line, err := env.Line()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(line), "\n") != 1 || !strings.HasSuffix(string(line), "\n") {
		t.Fatalf("Line = %q, want one JSON line", line)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v", "id", "ts", "from", "to", "kind", "body", "reply_to", "meta"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("line %s has no %q field", line, key)
		}
	}

	got, err := Decode(line)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.ID != env.ID || !got.TS.Equal(env.TS) || got.From != env.From || got.To != env.To {
		t.Errorf("Decode = %+v, want %+v", got, env)
	}
	if got.Kind != KindQuestion || got.Body != env.Body || got.ReplyTo != env.ReplyTo || got.Meta["item"] != "login" {
		t.Errorf("Decode = %+v, want %+v", got, env)
	}
}

func TestNewDefaults(t *testing.T) {
	env, err := New(addr.Local("a"), addr.Local("b"), "", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind != KindMessage {
		t.Errorf("kind = %q, want %q", env.Kind, KindMessage)
	}
	if env.Meta == nil {
		t.Error("meta is nil, want an empty map so the field marshals as {}")
	}
	line, err := env.Line()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"meta":{}`) || !strings.Contains(string(line), `"reply_to":""`) {
		t.Errorf("line = %s, want the full field set", line)
	}
}

func TestDecodeNormalizes(t *testing.T) {
	raw := `{"v":1,"id":"` + strings.ToLower("01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7") + `",
	 "ts":"2026-10-26T10:00:00.123Z","from":"recado://local/a","to":"b","kind":"",
	 "body":"x","meta":null}`
	env, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7" {
		t.Errorf("id = %q, want canonical uppercase", env.ID)
	}
	if env.To != "recado://local/b" {
		t.Errorf("to = %q, want the canonical address", env.To)
	}
	if env.Kind != KindMessage {
		t.Errorf("kind = %q, want %q", env.Kind, KindMessage)
	}
	if env.Meta == nil {
		t.Error("meta = nil, want empty map")
	}
}

func TestDecodeRejectsBadEnvelopes(t *testing.T) {
	bad := map[string]string{
		"empty":        "",
		"not json":     "{",
		"version":      `{"v":2,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z","from":"a","to":"b"}`,
		"no id":        `{"v":1,"ts":"2026-10-26T10:00:00.123Z","from":"a","to":"b"}`,
		"bad id":       `{"v":1,"id":"nope","ts":"2026-10-26T10:00:00.123Z","from":"a","to":"b"}`,
		"bad ts":       `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"yesterday","from":"a","to":"b"}`,
		"bad from":     `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z","from":"A B","to":"b"}`,
		"bad to":       `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z","from":"a","to":""}`,
		"no recipient": `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z","from":"a"}`,
		"bad reply_to": `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z","from":"a","to":"b","reply_to":"7k2q"}`,
	}
	for name, raw := range bad {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("%s: Decode accepted %q", name, raw)
		}
	}
}

func TestRender(t *testing.T) {
	from, _ := addr.Parse("recado://local/manager-4123")
	to := addr.Local("subagent-build-4188")
	env, err := New(from, to, KindQuestion, "which branch?", map[string]string{"stage": "build", "item": "login"})
	if err != nil {
		t.Fatal(err)
	}
	env.ID = "01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7"

	want := "[recado] from manager-4123 · question · item=login stage=build (id 4y5v6t7)\n" +
		"which branch?\n" +
		"reply with: recado reply 4y5v6t7 \"…\""
	if got := Render(env, "4y5v6t7"); got != want {
		t.Errorf("Render =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderPlain(t *testing.T) {
	env, err := New(addr.Local("a"), addr.Local("b"), KindMessage, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Render(env, "abcd"), "[recado] from a · message (id abcd)\nhi"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}

	env.From = "recado://rpi/subagent-build-4188"
	if got, want := Render(env, "abcd"), "[recado] from rpi/subagent-build-4188 · message (id abcd)\nhi"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}

	env.Body = ""
	if got, want := Render(env, "abcd"), "[recado] from rpi/subagent-build-4188 · message (id abcd)"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestNewRejectsHeaderBreakers(t *testing.T) {
	if _, err := New(addr.Local("a"), addr.Local("b"), "two\nlines", "hi", nil); err == nil {
		t.Error("New accepted a kind with a newline")
	}
	if _, err := New(addr.Local("a"), addr.Local("b"), "", "hi", map[string]string{"item": "a\nb"}); err == nil {
		t.Error("New accepted meta with a newline")
	}
	if _, err := New(addr.Local("a"), addr.Local("b"), "", "hi", map[string]string{"": "x"}); err == nil {
		t.Error("New accepted an empty meta key")
	}
}

func TestDecodeRejectsHeaderBreakers(t *testing.T) {
	raw := `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z",
	 "from":"a","to":"b","kind":"two\nlines"}`
	if _, err := Decode([]byte(raw)); err == nil {
		t.Error("Decode accepted a kind with a newline")
	}
	raw = `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z",
	 "from":"a","to":"b","kind":"message","meta":{"item":"a\nb"}}`
	if _, err := Decode([]byte(raw)); err == nil {
		t.Error("Decode accepted meta with a newline")
	}
}

func TestNewRejectsBadAddresses(t *testing.T) {
	if _, err := New(addr.Address{Machine: ""}, addr.Local("b"), "", "hi", nil); err == nil {
		t.Error("New accepted an address without a machine")
	}
	if _, err := New(addr.Local("NotAnID"), addr.Local("b"), "", "hi", nil); err == nil {
		t.Error("New accepted an invalid agent id")
	}
}

func TestDecodeCanonicalizesReplyTo(t *testing.T) {
	raw := `{"v":1,"id":"01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7","ts":"2026-10-26T10:00:00.123Z",
	 "from":"a","to":"b","reply_to":"01jb7q4w2z6zc9m1z3x4y5w6t7"}`
	env, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if env.ReplyTo != "01JB7Q4W2Z6ZC9M1Z3X4Y5W6T7" {
		t.Errorf("reply_to = %q, want canonical uppercase", env.ReplyTo)
	}
}

func TestFormatTime(t *testing.T) {
	ts := time.Date(2026, 10, 26, 10, 0, 0, 123456789, time.UTC)
	if got, want := FormatTime(ts), "2026-10-26T10:00:00.123Z"; got != want {
		t.Errorf("FormatTime = %q, want %q", got, want)
	}
	back, err := ParseTime("2026-10-26T10:00:00.123Z")
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(time.Date(2026, 10, 26, 10, 0, 0, 123000000, time.UTC)) {
		t.Errorf("ParseTime = %v", back)
	}
}
