package main

import "testing"

func TestVersionFlagPrintsTheBuildVersion(t *testing.T) {
	a, _ := newTestApp(t, false)
	out, err := exec(t, a, "", "--version")
	if err != nil {
		t.Fatal(err)
	}
	if out != "recado "+buildVersion()+"\n" {
		t.Fatalf("out = %q", out)
	}
}
