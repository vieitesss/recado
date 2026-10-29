package store_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/store"
)

func TestConcurrentUpstreamOutboxAppends(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	to, err := addr.Parse("recado+upstream://host/manager")
	if err != nil {
		t.Fatal(err)
	}
	const count = 40
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env, err := envelope.New(addr.Local("worker"), to, "message", "reply", nil)
			if err == nil {
				err = st.AppendOutbox(env)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, end, err := st.ReadOutbox(0, "host")
	if err != nil || len(records) != count {
		t.Fatalf("records=%d end=%d err=%v", len(records), end, err)
	}
	info, err := os.Stat(filepath.Join(root, "outbox.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || info.Size() != end {
		t.Fatalf("outbox mode=%v size=%d end=%d", info.Mode(), info.Size(), end)
	}
}
