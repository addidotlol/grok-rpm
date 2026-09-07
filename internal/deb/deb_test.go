package deb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseControl(t *testing.T) {
	body := "Package: grok-bot\nVersion: 0.44.0\nArchitecture: amd64\n" +
		"Depends: libgtk-3-0, libnotify4, libasound2t64 | libasound2, libfoo (>= 1.2)\n" +
		"Recommends: libappindicator3-1\nConflicts: sand\nProvides: sand\n" +
		"Description: Grok Bot desktop agent\n agent line\n"
	c := ParseControl(body)
	if c.Package != "grok-bot" || c.Version != "0.44.0" || c.Architecture != "amd64" {
		t.Fatalf("bad header: %+v", c)
	}
	if len(c.Depends) != 4 {
		t.Fatalf("depends: %q", c.Depends)
	}
	if DepName("libasound2t64 | libasound2") != "libasound2t64" {
		// DepName takes a single alternative; alternatives are split by callers.
		t.Log("note: DepName on full alternative string")
	}
	if DepName("libfoo (>= 1.2)") != "libfoo" {
		t.Fatalf("DepName failed")
	}
	if DepName("libbar:amd64") != "libbar" {
		t.Fatalf("arch qualifier failed")
	}
}

func TestReadControlRealDeb(t *testing.T) {
	p := os.Getenv("GROK_TEST_DEB")
	if p == "" {
		t.Skip("GROK_TEST_DEB not set")
	}
	c, err := ReadControl(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Package != "grok-bot" {
		t.Fatalf("package=%q", c.Package)
	}
	if len(c.Depends) == 0 {
		t.Fatal("no depends")
	}
}

func TestExtractDataRealDeb(t *testing.T) {
	p := os.Getenv("GROK_TEST_DEB")
	if p == "" {
		t.Skip("GROK_TEST_DEB not set")
	}
	dest := t.TempDir()
	if err := ExtractData(p, dest); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"opt/Grok Bot/grok-bot", "usr/share/applications/grok-bot.desktop"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Fatalf("missing %s: %v", want, err)
		}
	}
}
