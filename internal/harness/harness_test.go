package harness

import "testing"

func TestSetLookupAndDetect(t *testing.T) {
	echo := &Fake{Dir: t.TempDir()}
	set := Set{echo}
	if h, ok := set.Lookup("fake"); !ok || h != echo {
		t.Fatalf("Lookup(fake) = %v, %v", h, ok)
	}
	if _, ok := set.Lookup("pi"); ok {
		t.Error("Lookup(pi) found a harness that is not registered")
	}

	if h, id, ok := set.Detect([]string{"HOME=/tmp"}); ok || h != nil || id != "" {
		t.Errorf("Detect(no harness env) = %v, %q, %v; want nil", h, id, ok)
	}
	if h, id, ok := set.Detect([]string{"FAKE_AGENT_ID=sub-1"}); !ok || h != echo || id != "sub-1" {
		t.Errorf("Detect(fake env) = %v, %q, %v; want the fake harness and sub-1", h, id, ok)
	}
}
