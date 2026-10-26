package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func target(t *testing.T) Target {
	t.Helper()
	return Targets(t.TempDir())[0]
}

func write(t *testing.T, tg Target, body string) {
	t.Helper()
	if err := os.MkdirAll(tg.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tg.file(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireState(t *testing.T, tg Target, want State) {
	t.Helper()
	got, err := Inspect(tg)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("state = %v, want %v", got, want)
	}
}

func TestInstallThenRemove(t *testing.T) {
	tg := target(t)
	requireState(t, tg, Missing)

	if err := Install(tg); err != nil {
		t.Fatal(err)
	}
	requireState(t, tg, Current)

	removed, err := Remove(tg)
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v; want true, nil", removed, err)
	}
	if _, err := os.Stat(tg.Dir); !os.IsNotExist(err) {
		t.Fatalf("skill directory still there: %v", err)
	}
}

func TestInstallReplacesOutdatedStub(t *testing.T) {
	tg := target(t)
	write(t, tg, "---\nmetadata:\n  recado-stub: \"0\"\n---\nold\n")
	requireState(t, tg, Outdated)

	if err := Install(tg); err != nil {
		t.Fatal(err)
	}
	requireState(t, tg, Current)
}

func TestForeignSkillIsLeftAlone(t *testing.T) {
	tg := target(t)
	write(t, tg, "---\nname: recado\n---\nsomeone else's\n")
	requireState(t, tg, Foreign)

	if err := Install(tg); err == nil {
		t.Fatal("Install overwrote a foreign skill")
	}
	if _, err := Remove(tg); err == nil {
		t.Fatal("Remove deleted a foreign skill")
	}
	requireState(t, tg, Foreign)
}

func TestRemoveKeepsOtherFiles(t *testing.T) {
	tg := target(t)
	if err := Install(tg); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(tg.Dir, "notes.md")
	if err := os.WriteFile(extra, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(tg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("other file removed: %v", err)
	}
	requireState(t, tg, Missing)
}
